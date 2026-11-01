package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Postgres in the browser, through the tunnel.

	browser ──wss──▶ argus-control ──"postgres" stream──▶ agent ──▶ postgres on this server

Opened on a grant for "postgres" that names the database and the access level.
The agent gives the person's own role this session's password (see
postgres_admin.go), connects as that role -- never as a superuser with SET
ROLE, which RESET ROLE would undo -- and runs one statement per request.

Every statement is written to the session's recording and sent to the control
plane before it reaches Postgres; a statement the recording cannot take is not
run. One statement per request is Postgres's own rule here: the extended
protocol refuses "SELECT 1; DROP TABLE x", so what is recorded is what ran.

What Postgres enforces is the boundary. The person's role holds exactly the
grants of their level, so a write on a read session is Postgres's permission
error, whatever the console or this agent thought of the statement.
*/

// PostgresService is the tunnel service name.
const PostgresService = tunnel.PostgresService

// PostgresConfig is the postgres service.
type PostgresConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// RecordDir is where statement recordings are written.
	RecordDir string
	// Admin provisions roles; nil runs the pg-admin helper as AdminUser.
	Admin func(context.Context, PGAdminRequest) PGAdminResult
	// AdminUser is the server account peer authentication maps to a
	// superuser. Empty is "postgres".
	AdminUser string
	// Helper, Spawn and HelperEnv are as for files.
	Helper    string
	Spawn     SpawnMode
	HelperEnv []string
	// MaxStatementsPerMinute bounds one session. Zero is 120.
	MaxStatementsPerMinute int
}

// Defaults when the grant names none.
const (
	defaultStmtTimeout   = 60 * time.Second
	defaultIdleTxTimeout = 2 * time.Minute
	defaultIdleTimeout   = 30 * time.Minute
	defaultMaxSession    = 8 * time.Hour
	lockTimeout          = 10 * time.Second
	maxQueuedRequests    = 8
)

// roleUse counts open sessions per role on this agent, so the last one to
// close is the one that locks the role.
type roleUse struct {
	mu sync.Mutex
	n  map[string]int
}

func (r *roleUse) add(key string, d int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[string]int{}
	}
	r.n[key] += d
	v := r.n[key]
	if v <= 0 {
		delete(r.n, key)
	}
	return v
}

// Postgres is the postgres service.
func Postgres(cfg PostgresConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxStatementsPerMinute <= 0 {
		cfg.MaxStatementsPerMinute = 120
	}
	admin := cfg.Admin
	if admin == nil {
		admin = helperAdmin(cfg)
	}
	uses := &roleUse{}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a database session", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(PostgresService) || g.DB == nil {
			log.Warn("tunnel: refused a database session on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for a database"}.Error())
			return
		}
		db := *g.DB
		switch {
		case !tunnel.ValidPersonalRole(g.Principal):
			call.Refuse("not a role Argus will sign in as: " + g.Principal)
			return
		case tunnel.LevelRank(db.Level) == 0:
			call.Refuse("unknown access level " + db.Level)
			return
		case !tunnel.LocalPGHost(db.Host):
			// The agent reaches its own server's Postgres and nothing else.
			call.Refuse("the database must be on this server; " + db.Host + " is not")
			return
		case db.Name == "" || db.Port <= 0 || db.Port > 65535:
			call.Refuse("the grant does not say which database to open")
			return
		}

		s, err := openPGSession(ctx, cfg, admin, uses, g, log)
		if err != nil {
			log.Warn("tunnel: database session refused", "database", db.Name, "role", g.Principal,
				"for", g.Email, "error", err)
			call.Refuse(err.Error())
			return
		}
		if call.Accept(s.id) != nil {
			s.close(context.WithoutCancel(ctx), "the control plane went away")
			_ = call.Stream.Close()
			return
		}
		log.Info("tunnel: database session opened", "database", db.Name, "role", g.Principal,
			"level", db.Level, "for", g.Email, "session", s.id)
		s.serve(ctx, call.Stream)
		_ = call.Stream.Close()
	}
}

type pgSession struct {
	id    string
	cfg   PostgresConfig
	admin pgAdminRunner
	uses  *roleUse
	g     tunnel.Grant
	db    tunnel.GrantDB
	log   *slog.Logger

	conn    *pgconn.PgConn
	types   map[uint32]string
	version string

	rec    *os.File
	seq    int64
	prev   string
	wmu    sync.Mutex // the stream, between the runner and the reader
	stream tunnel.Stream

	// cancel ends the statement running now, if any.
	cmu    sync.Mutex
	cancel context.CancelFunc
	curID  uint32

	closeOnce sync.Once
}

func openPGSession(ctx context.Context, cfg PostgresConfig, admin pgAdminRunner, uses *roleUse,
	g tunnel.Grant, log *slog.Logger) (*pgSession, error) {

	db := *g.DB
	idb := make([]byte, 16)
	if _, err := rand.Read(idb); err != nil {
		return nil, err
	}
	s := &pgSession{id: sessionUUID(idb), cfg: cfg, admin: admin, uses: uses,
		g: g, db: db, log: log}

	password, err := sessionPassword()
	if err != nil {
		return nil, err
	}
	verifier, err := scramVerifier(password)
	if err != nil {
		return nil, err
	}
	maxSession := secs(db.MaxSessionS, defaultMaxSession)
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	uses.add(s.useKey(), 1)
	res := admin(octx, PGAdminRequest{Op: "open", Database: db.Name, Socket: socketDir(db),
		Port: db.Port, AdminUser: cfg.AdminUser, Role: g.Principal, Owner: g.Email,
		Level: db.Level, Verifier: verifier, ValidUntil: time.Now().Add(maxSession)})
	if !res.OK {
		uses.add(s.useKey(), -1)
		if res.Error == "" {
			res.Error = errPGAdmin.Error()
		}
		return nil, errors.New(res.Error)
	}
	s.version = res.Version

	conn, err := s.connect(octx, password)
	if err != nil {
		s.lockRole(context.WithoutCancel(ctx))
		return nil, err
	}
	s.conn = conn

	if err := s.loadTypes(octx); err != nil {
		s.close(context.WithoutCancel(ctx), "")
		return nil, err
	}
	if err := s.openRecording(); err != nil {
		s.close(context.WithoutCancel(ctx), "")
		return nil, fmt.Errorf("the session could not be recorded, so it was not opened: %w", err)
	}
	return s, nil
}

func (s *pgSession) useKey() string { return s.db.Name + "\x00" + s.g.Principal }

func socketDir(db tunnel.GrantDB) string {
	if strings.HasPrefix(db.Host, "/") {
		return db.Host
	}
	return "/var/run/postgresql"
}

func secs(n int, def time.Duration) time.Duration {
	if n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

func ms(n int, def time.Duration) time.Duration {
	if n <= 0 {
		return def
	}
	return time.Duration(n) * time.Millisecond
}

// connect signs in as the person's role. TCP to localhost first, which every
// stock pg_hba.conf allows with a password; then the socket, which needs the
// "+argus_people scram-sha-256" line.
func (s *pgSession) connect(ctx context.Context, password string) (*pgconn.PgConn, error) {
	params := map[string]string{
		"application_name":                    "argus:" + s.id,
		"statement_timeout":                   strconv.FormatInt(ms(s.db.StatementTimeoutMS, defaultStmtTimeout).Milliseconds(), 10),
		"idle_in_transaction_session_timeout": strconv.FormatInt(ms(s.db.IdleTxTimeoutMS, defaultIdleTxTimeout).Milliseconds(), 10),
		"lock_timeout":                        strconv.FormatInt(lockTimeout.Milliseconds(), 10),
	}
	if s.db.Level == tunnel.LevelRead {
		// A convenience, not the control: the role has no write grants.
		params["default_transaction_read_only"] = "on"
	}
	// Where the database says, then the other local way in. A socket
	// directory is usually under "local all all peer", which refuses a role
	// with no matching account, while 127.0.0.1 is usually password
	// authenticated; trying both means most servers need no pg_hba change.
	hosts := []string{s.db.Host}
	if strings.HasPrefix(s.db.Host, "/") {
		hosts = append(hosts, "127.0.0.1")
	} else {
		hosts = append(hosts, socketDir(s.db))
	}
	var lastErr error
	for _, host := range hosts {
		cfg, err := pgconn.ParseConfig("sslmode=prefer")
		if err != nil {
			return nil, err
		}
		cfg.Host, cfg.Port, cfg.Database = host, uint16(s.db.Port), s.db.Name
		cfg.User, cfg.Password = s.g.Principal, password
		cfg.RuntimeParams = params
		cfg.Fallbacks = nil
		if strings.HasPrefix(host, "/") {
			cfg.TLSConfig = nil
		} else if cfg.TLSConfig != nil {
			// Localhost: TLS when the server offers it, plain when it does
			// not; no certificate to check on a loopback connection.
			cfg.TLSConfig.InsecureSkipVerify = true
			cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: host, Port: uint16(s.db.Port)}}
		}
		cfg.ConnectTimeout = 10 * time.Second
		// A cancelled statement sends Postgres a cancel request and keeps
		// the connection; the default would close it.
		cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
			return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: 10 * time.Second}
		}
		conn, err := pgconn.ConnectConfig(ctx, cfg)
		if err == nil {
			if s.db.Level == tunnel.LevelOwner || s.db.Level == tunnel.LevelDBA {
				// What an owner creates belongs to the level, not to one
				// person whose role is locked when they leave.
				if _, err := conn.Exec(ctx, "SET ROLE "+pgx(levelRole(s.db.Level, s.db.Name))).ReadAll(); err != nil {
					_ = conn.Close(ctx)
					return nil, err
				}
			}
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("could not sign in to Postgres as %s: %w; the server's pg_hba.conf "+
		"must let it in with a password, over localhost or with the line "+
		"\"local all +argus_people scram-sha-256\"", s.g.Principal, lastErr)
}

// loadTypes reads type names once, so a result's columns can be named
// without a round trip while its rows are arriving.
func (s *pgSession) loadTypes(ctx context.Context) error {
	res := s.conn.ExecParams(ctx, `SELECT oid::text, format_type(oid, NULL) FROM pg_type
		WHERE typtype <> 'c' OR typrelid = 0 LIMIT 20000`, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return res.Err
	}
	s.types = make(map[uint32]string, len(res.Rows))
	for _, r := range res.Rows {
		oid, _ := strconv.ParseUint(string(r[0]), 10, 32)
		s.types[uint32(oid)] = string(r[1])
	}
	return nil
}

func (s *pgSession) openRecording() error {
	dir := s.cfg.RecordDir
	if dir == "" {
		return errors.New("no recording directory is configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, s.id+".argussql"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.rec = f
	return nil
}

// record writes one line to the recording and sends it to the control plane.
// Called with wmu free.
func (s *pgSession) record(r tunnel.PGRecord) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.seq++
	r.Seq = s.seq
	r.Time = time.Now().UTC().Format(time.RFC3339Nano)
	r.Prev = s.prev
	r.Hash = ""
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	r.Hash = hex.EncodeToString(sum[:])
	line, _ := json.Marshal(r)
	line = append(line, '\n')
	if _, err := s.rec.Write(line); err != nil {
		return err
	}
	s.prev = r.Hash
	if s.stream != nil {
		return tunnel.WriteFrame(s.stream, tunnel.FramePGRecord, line[:len(line)-1])
	}
	return nil
}

// RecordHash recomputes a record's hash, for verifying a chain.
func RecordHash(r tunnel.PGRecord) string {
	r.Hash = ""
	body, _ := json.Marshal(r)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s *pgSession) send(a tunnel.PGAnswer) error {
	b, _ := json.Marshal(a)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return tunnel.WriteFrame(s.stream, tunnel.FramePGAnswer, b)
}

func (s *pgSession) sendRows(p []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return tunnel.WriteFrame(s.stream, tunnel.FramePGRows, p)
}

// serve runs the session until the stream ends or a limit closes it.
func (s *pgSession) serve(ctx context.Context, st tunnel.Stream) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	s.stream = st

	reason := "closed"
	defer func() { s.close(context.WithoutCancel(ctx), reason) }()

	if s.record(tunnel.PGRecord{Kind: "open", Actor: s.g.Email, Role: s.g.Principal,
		Level: s.db.Level, Grant: s.g.ID}) != nil {
		return
	}
	if s.send(tunnel.PGAnswer{Type: "ready", Role: s.g.Principal, Level: s.db.Level,
		Version: s.version, Session: s.id, ReadOnly: s.db.Level == tunnel.LevelRead}) != nil {
		return
	}

	reqs := make(chan tunnel.PGRequest, maxQueuedRequests)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer stop()
		s.read(ctx, st, reqs)
	}()

	idle := secs(s.db.IdleTimeoutS, defaultIdleTimeout)
	maxSession := secs(s.db.MaxSessionS, defaultMaxSession)
	deadline := time.NewTimer(maxSession)
	defer deadline.Stop()
	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	limiter := newRateLimit(s.cfg.MaxStatementsPerMinute, time.Minute)

	for {
		select {
		case <-ctx.Done():
			<-readerDone
			return
		case <-deadline.C:
			reason = "the session reached its longest allowed length"
			_ = s.send(tunnel.PGAnswer{Type: "closed", Error: reason})
			stop()
			<-readerDone
			return
		case <-idleT.C:
			reason = "the session was idle too long"
			_ = s.send(tunnel.PGAnswer{Type: "closed", Error: reason})
			stop()
			<-readerDone
			return
		case req := <-reqs:
			idleT.Reset(idle)
			switch req.Op {
			case "run":
				if !limiter.allow(time.Now()) {
					_ = s.send(tunnel.PGAnswer{ID: req.ID, Type: "error",
						Error: "too many statements in a minute; wait a moment"})
					continue
				}
				if err := s.run(ctx, req); err != nil {
					reason = err.Error()
					return
				}
			case "schema":
				if err := s.schema(ctx, req); err != nil {
					reason = err.Error()
					return
				}
			default:
				_ = s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "unknown operation " + req.Op})
			}
		}
	}
}

// read takes requests off the stream. A cancel acts at once, on whatever is
// running; everything else waits its turn.
func (s *pgSession) read(ctx context.Context, st tunnel.Stream, reqs chan<- tunnel.PGRequest) {
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			return
		}
		if typ != tunnel.FramePGRequest {
			return
		}
		var req tunnel.PGRequest
		if json.Unmarshal(p, &req) != nil {
			return
		}
		if req.Op == "cancel" {
			s.cmu.Lock()
			running := s.cancel != nil && (req.ID == 0 || req.ID == s.curID)
			if running {
				s.cancel()
			}
			s.cmu.Unlock()
			if running {
				// Cancelling the context tells Postgres eventually; a direct
				// cancel request over a fresh connection tells it now, so a
				// statement does not run to its timeout.
				go func() {
					cctx, c := context.WithTimeout(ctx, 5*time.Second)
					defer c()
					_ = s.conn.CancelRequest(cctx)
				}()
			}
			continue
		}
		select {
		case reqs <- req:
		case <-ctx.Done():
			return
		default:
			_ = s.send(tunnel.PGAnswer{ID: req.ID, Type: "error",
				Error: "the session is busy; wait for the running statement or cancel it"})
		}
	}
}

// run executes one statement. An error return ends the session: the
// recording or the stream failed. A statement's own failure is an answer.
func (s *pgSession) run(ctx context.Context, req tunnel.PGRequest) error {
	sql := strings.TrimSpace(req.SQL)
	switch {
	case sql == "":
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "there is no statement to run"})
	case len(sql) > tunnel.MaxSQL:
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "the statement is too long"})
	case !utf8.ValidString(sql) || strings.IndexByte(sql, 0) >= 0:
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "the statement is not valid text"})
	}
	maxRows := req.MaxRows
	if maxRows <= 0 {
		maxRows = tunnel.DefaultMaxRows
	}
	if s.db.MaxRows > 0 && maxRows > s.db.MaxRows {
		maxRows = s.db.MaxRows
	}
	maxRows = min(maxRows, tunnel.MaxRowsCeiling)

	// Recorded and sent before Postgres sees it. No record, no statement.
	if err := s.record(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: sql}); err != nil {
		return fmt.Errorf("the recording failed: %w", err)
	}

	sctx, cancel := context.WithCancel(ctx)
	s.cmu.Lock()
	s.cancel, s.curID = cancel, req.ID
	s.cmu.Unlock()
	defer func() {
		s.cmu.Lock()
		s.cancel, s.curID = nil, 0
		s.cmu.Unlock()
		cancel()
	}()

	start := time.Now()
	rr := s.conn.ExecParams(sctx, sql, nil, nil, nil, []int16{0})
	h := sha256.New()
	enc := tunnel.NewRowEncoder(req.ID)
	var (
		rows      int64
		truncated bool
		colsSent  bool
		sendErr   error
	)
	flush := func() {
		if enc.Rows() == 0 || sendErr != nil {
			return
		}
		h.Write(enc.Batch()[4:])
		sendErr = s.sendRows(enc.Batch())
		enc.Reset()
	}
	for rr.NextRow() {
		if !colsSent {
			sendErr = s.sendCols(req.ID, rr.FieldDescriptions())
			colsSent = true
		}
		if sendErr != nil {
			break
		}
		if rows >= int64(maxRows) {
			truncated = true
			break
		}
		enc.Add(rr.Values())
		rows++
		if enc.Full() {
			flush()
		}
	}
	flush()
	if truncated && s.conn.TxStatus() == 'I' {
		// Outside a transaction, stop Postgres producing rows nobody will
		// see. Inside one, a cancel would abort the person's transaction,
		// so the rest is read and dropped instead.
		cancel()
	}
	tag, err := rr.Close()
	elapsed := time.Since(start).Milliseconds()
	if sendErr != nil {
		return sendErr
	}
	if !colsSent && err == nil && len(rr.FieldDescriptions()) > 0 {
		// A query that returned no rows still has columns to show.
		if err := s.sendCols(req.ID, rr.FieldDescriptions()); err != nil {
			return err
		}
	}
	var pgErr *pgconn.PgError
	if err != nil && truncated && errors.As(err, &pgErr) && pgErr.Code == "57014" {
		err = nil // the cancel we sent for the row limit
	}
	resultSum := hex.EncodeToString(h.Sum(nil))
	if err != nil {
		a := tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error(), MS: elapsed}
		if errors.As(err, &pgErr) {
			a.Error, a.Code, a.Position, a.Detail, a.Hint =
				pgErr.Message, pgErr.Code, int(pgErr.Position), pgErr.Detail, pgErr.Hint
			if pgErr.Code == "57014" && ctx.Err() == nil {
				a.Error = "cancelled: " + pgErr.Message
			}
		} else if s.conn.IsClosed() {
			_ = s.record(tunnel.PGRecord{Kind: "error", Request: req.ID, Error: err.Error(), MS: elapsed})
			_ = s.send(a)
			return errors.New("the connection to Postgres ended")
		}
		if rerr := s.record(tunnel.PGRecord{Kind: "error", Request: req.ID, Code: a.Code,
			Error: a.Error, MS: elapsed, Rows: rows}); rerr != nil {
			return rerr
		}
		return s.send(a)
	}
	tagText := tag.String()
	if rerr := s.record(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: tagText, Rows: rowsOf(tag, rows),
		MS: elapsed, ResultSHA256: resultSum, Truncated: truncated}); rerr != nil {
		return rerr
	}
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "done", Tag: tagText, Rows: rowsOf(tag, rows),
		MS: elapsed, Truncated: truncated})
}

// rowsOf is how many rows a statement touched: what it returned for a query,
// what the tag says for a write.
func rowsOf(tag pgconn.CommandTag, returned int64) int64 {
	if tag.Select() || returned > 0 {
		return returned
	}
	return tag.RowsAffected()
}

func (s *pgSession) sendCols(id uint32, fds []pgconn.FieldDescription) error {
	cols := make([]tunnel.PGColumn, len(fds))
	for i, fd := range fds {
		t, ok := s.types[fd.DataTypeOID]
		if !ok {
			t = "oid " + strconv.FormatUint(uint64(fd.DataTypeOID), 10)
		}
		cols[i] = tunnel.PGColumn{Name: fd.Name, Type: t}
	}
	return s.send(tunnel.PGAnswer{ID: id, Type: "cols", Cols: cols})
}

// Object tree queries, as the person's role, showing what that role may use.
const (
	pgSchemasSQL = `SELECT n.nspname, pg_get_userbyid(n.nspowner),
	  (SELECT count(*) FROM pg_class c WHERE c.relnamespace = n.oid AND c.relkind IN ('r','p'))
	FROM pg_namespace n
	WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
	  AND has_schema_privilege(n.oid, 'USAGE')
	ORDER BY n.nspname = 'public' DESC, n.nspname LIMIT 2000`
	pgObjectsSQL = `SELECT c.relname,
	  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table' WHEN 'v' THEN 'view'
	    WHEN 'm' THEN 'matview' WHEN 'S' THEN 'sequence' WHEN 'f' THEN 'table' END,
	  '', greatest(c.reltuples, 0)::bigint
	FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','S','f')
	  AND (has_table_privilege(c.oid, 'SELECT') OR has_any_column_privilege(c.oid, 'SELECT')
	       OR (c.relkind = 'S' AND has_sequence_privilege(c.oid, 'USAGE')))
	UNION ALL
	SELECT p.proname, CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END,
	  pg_get_function_identity_arguments(p.oid), 0
	FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	WHERE n.nspname = $1 AND p.prokind IN ('f','p') AND has_function_privilege(p.oid, 'EXECUTE')
	ORDER BY 2, 1 LIMIT 5000`
	pgColumnsSQL = `SELECT a.attname, 'column',
	  format_type(a.atttypid, a.atttypmod)
	    || CASE WHEN a.attnotnull THEN ' not null' ELSE '' END
	    || CASE WHEN EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary
	         AND a.attnum = ANY(i.indkey)) THEN ' · primary key' ELSE '' END
	    || CASE WHEN has_column_privilege(c.oid, a.attnum, 'SELECT') THEN '' ELSE ' · hidden' END,
	  a.attnum
	FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
	ORDER BY a.attnum LIMIT 2000`
)

// schema answers the object tree. Not recorded: it reads the catalogue as the
// person's role and changes nothing, and recording every click on the tree
// would bury the statements people actually ran.
func (s *pgSession) schema(ctx context.Context, req tunnel.PGRequest) error {
	var (
		sql  string
		args [][]byte
	)
	switch req.Kind {
	case "schemas":
		sql = pgSchemasSQL
	case "objects":
		sql, args = pgObjectsSQL, [][]byte{[]byte(req.Schema)}
	case "columns":
		sql, args = pgColumnsSQL, [][]byte{[]byte(req.Schema), []byte(req.Object)}
	default:
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "unknown schema request " + req.Kind})
	}
	if s.conn.TxStatus() == 'E' {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error",
			Error: "the current transaction failed; ROLLBACK to browse again"})
	}
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res := s.conn.ExecParams(qctx, sql, args, nil, nil, nil).Read()
	if res.Err != nil {
		if s.conn.IsClosed() {
			return errors.New("the connection to Postgres ended")
		}
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: res.Err.Error()})
	}
	out := make([]tunnel.PGObject, 0, len(res.Rows))
	for _, r := range res.Rows {
		o := tunnel.PGObject{Name: string(r[0])}
		switch req.Kind {
		case "schemas":
			o.Kind, o.Detail = "schema", string(r[1])
			o.Count, _ = strconv.ParseInt(string(r[2]), 10, 64)
		default:
			o.Kind, o.Detail = string(r[1]), string(r[2])
			if req.Kind != "columns" {
				o.Count, _ = strconv.ParseInt(string(r[3]), 10, 64)
			}
		}
		out = append(out, o)
	}
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "schema", Objects: out})
}

// close ends the session: the connection, the recording, and -- when this
// was the role's last session here -- its ability to log in.
func (s *pgSession) close(ctx context.Context, reason string) {
	s.closeOnce.Do(func() {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if s.conn != nil {
			_ = s.conn.Close(cctx)
		}
		if s.rec != nil {
			_ = s.record(tunnel.PGRecord{Kind: "close", Error: reason})
			_ = s.rec.Sync()
			_ = s.rec.Close()
		}
		s.lockRole(cctx)
		s.log.Info("tunnel: database session closed", "session", s.id, "role", s.g.Principal,
			"reason", reason)
	})
}

func (s *pgSession) lockRole(ctx context.Context) {
	if s.uses.add(s.useKey(), -1) > 0 {
		return
	}
	res := s.admin(ctx, PGAdminRequest{Op: "close", Database: s.db.Name, Socket: socketDir(s.db),
		Port: s.db.Port, AdminUser: s.cfg.AdminUser, Role: s.g.Principal})
	if !res.OK {
		s.log.Error("tunnel: could not lock a database role after its session",
			"role", s.g.Principal, "database", s.db.Name, "error", res.Error,
			"detail", "its password still expires on its own at the session's end")
	}
}

// rateLimit allows n events per window, sliding.
type rateLimit struct {
	n      int
	window time.Duration
	times  []time.Time
}

func newRateLimit(n int, window time.Duration) *rateLimit {
	return &rateLimit{n: n, window: window, times: make([]time.Time, 0, n)}
}

func (r *rateLimit) allow(now time.Time) bool {
	cut := 0
	for cut < len(r.times) && now.Sub(r.times[cut]) >= r.window {
		cut++
	}
	r.times = append(r.times[:0], r.times[cut:]...)
	if len(r.times) >= r.n {
		return false
	}
	r.times = append(r.times, now)
	return true
}

// sessionUUID formats random bytes as a version 4 UUID, which is what the
// control plane's sessions table is keyed by; the recording on this host is
// named by the same id, so one id finds both.
func sessionUUID(b []byte) string {
	var u [16]byte
	copy(u[:], b)
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	h := hex.EncodeToString(u[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

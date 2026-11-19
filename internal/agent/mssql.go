package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-sql/sqlexp"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Microsoft SQL Server in the browser, through the tunnel.

	browser ──wss──▶ argus-control ──"mssql" stream──▶ agent ──▶ sqlservr on this server

The session speaks the Postgres session's frames, so the console's SQL
workspace, the recording and its verification are Postgres's. See
tunnel/mssql.go for the shape.

The agent signs in as SQL Server's administrator on its own server for one
thing: giving the person their level's login (tunnel.MSSQLLogin) with a
one-time password, mapped to a user in the entry's database with the level's
roles there, and disabling it again afterwards. SQL Server has no sign-in by
the operating system's account on Linux, so the administrator's login and
password are read from a file on this server (tunnel.MSSQLAdminFile, or the
entry's), readable by root only, for that one connection. Nothing is stored
and nothing leaves the server.

The person's statements run on a connection signed in as their login, so what
they may do is what SQL Server's roles and permissions say, whatever the
console or this agent thought of a statement.

SQL Server stops a statement when the client sends an attention, and keeps the
connection and any transaction on it. That is what a cancel, the timeout and
the row limit do here: cancel the statement's own context, which the driver
turns into an attention. KILL would end the person's whole connection.
*/

// MSSQLService is the tunnel service name.
const MSSQLService = tunnel.MSSQLService

// MSSQL is the mssql service.
func MSSQL(cfg StoreConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
		cfg.Log = log
	}
	if cfg.MaxRequestsPerMinute <= 0 {
		cfg.MaxRequestsPerMinute = 120
	}
	f := &mssqlFactory{cfg: cfg, uses: &userUse{}}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a sql server session", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(MSSQLService) || g.DB == nil || g.DB.EngineOf() != tunnel.EngineMSSQL {
			log.Warn("tunnel: refused a sql server session on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for sql server"}.Error())
			return
		}
		switch {
		case !tunnel.ValidPersonalRole(g.Principal):
			call.Refuse("not a user Argus will sign in as: " + g.Principal)
			return
		case tunnel.LevelRank(g.DB.Level) == 0:
			call.Refuse("unknown access level " + g.DB.Level)
			return
		case g.DB.Port <= 0 || g.DB.Port > 65535 || !tunnel.ValidMSSQLDB(g.DB.Name):
			call.Refuse("the grant does not say what to open")
			return
		case !tunnel.LocalLoopback(g.DB.Host):
			call.Refuse("SQL Server must be on this server; " + g.DB.Host + " is not")
			return
		case g.DB.ConfigPath != "" && !tunnel.ValidMSSQLAdminFile(g.DB.ConfigPath):
			call.Refuse("the admin file must be an absolute path to a .conf file")
			return
		}
		idb := make([]byte, 16)
		if _, err := rand.Read(idb); err != nil {
			call.Refuse(err.Error())
			return
		}
		id := sessionUUID(idb)
		rec, err := newStmtRecorder(cfg.RecordDir, id)
		if err != nil {
			call.Refuse("the session could not be recorded, so it was not opened: " + err.Error())
			return
		}
		rec.parts = g.DB.AnswerParts
		octx, cancel := context.WithTimeout(ctx, 45*time.Second)
		s, err := f.open(octx, g, id)
		cancel()
		if err != nil {
			rec.discard()
			log.Warn("tunnel: sql server session refused", "database", g.DB.Name, "user", g.Principal,
				"for", g.Email, "error", err)
			call.Refuse(err.Error())
			return
		}
		if call.Accept(id) != nil {
			s.close(context.WithoutCancel(ctx))
			rec.discard()
			_ = call.Stream.Close()
			return
		}
		s.rec = rec
		log.Info("tunnel: sql server session opened", "database", g.DB.Name, "login", s.login,
			"level", g.DB.Level, "for", g.Email, "session", id, "version", s.version)
		s.serve(ctx, call.Stream)
		_ = call.Stream.Close()
	}
}

type mssqlFactory struct {
	cfg  StoreConfig
	uses *userUse
	// mu serializes giving and taking logins, so two sessions of one person
	// opening together cannot interleave an enable with a disable.
	mu sync.Mutex
}

// mssqlDSN is a connection to SQL Server on this server. The connection is
// to loopback only, so the server's certificate (self-signed unless someone
// installed one) is not checked; the sign-in is still encrypted.
func mssqlDSN(host string, port int, user, pw, db string) string {
	q := url.Values{}
	q.Set("database", db)
	q.Set("encrypt", "false")
	q.Set("TrustServerCertificate", "true")
	q.Set("app name", "argus")
	q.Set("dial timeout", "10")
	u := url.URL{Scheme: "sqlserver", User: url.UserPassword(user, pw),
		Host: net.JoinHostPort(host, strconv.Itoa(port)), RawQuery: q.Encode()}
	return u.String()
}

// mssqlAdminCreds is what the admin file holds.
type mssqlAdminCreds struct{ user, pw string }

// readMSSQLAdmin reads user and password from the admin file: lines of
// key = value, # for a comment. The file must be private to its owner, like
// every secret the agent reads.
func readMSSQLAdmin(path string) (mssqlAdminCreds, error) {
	var c mssqlAdminCreds
	if !tunnel.ValidMSSQLAdminFile(path) {
		return c, fmt.Errorf("%s is not an admin file the agent will read", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, fmt.Errorf("SQL Server needs its administrator to give people logins, and %s does not exist: "+
				"put user = and password = for a sysadmin login such as sa in it, readable by root only", path)
		}
		return c, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return c, fmt.Errorf("%s can be read by others; chmod 600 it, since it holds SQL Server's admin password", path)
	}
	b, err := readFileLimited(path, 64<<10)
	if err != nil {
		return c, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "user", "username", "login":
			c.user = v
		case "password", "pass":
			c.pw = v
		}
	}
	if c.user == "" || c.pw == "" {
		return c, fmt.Errorf("%s needs both user = and password =", path)
	}
	return c, sc.Err()
}

// mssqlAdmin is an administrator's connection, for giving and taking logins.
type mssqlAdmin struct {
	db      *sql.DB
	version string
	edition string
}

func (a *mssqlAdmin) close() { _ = a.db.Close() }

func (a *mssqlAdmin) exec(ctx context.Context, q string, args ...any) error {
	_, err := a.db.ExecContext(ctx, q, args...)
	return err
}

// mssqlAdminConn signs in as SQL Server's administrator on this server.
func mssqlAdminConn(ctx context.Context, host string, port int, file string) (*mssqlAdmin, error) {
	if file == "" {
		file = tunnel.MSSQLAdminFile
	}
	c, err := readMSSQLAdmin(file)
	if err != nil {
		return nil, err
	}
	h, err := sql.Open("sqlserver", mssqlDSN(host, port, c.user, c.pw, "master"))
	if err != nil {
		return nil, err
	}
	// One connection: USE moves it between databases, and the next
	// statement has to be on the same one.
	h.SetMaxOpenConns(1)
	h.SetMaxIdleConns(1)
	var version, edition string
	var mayLogins, mayKill int
	err = h.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128)),
	  CAST(SERVERPROPERTY('Edition') AS nvarchar(128)),
	  HAS_PERMS_BY_NAME(NULL, NULL, 'ALTER ANY LOGIN'), HAS_PERMS_BY_NAME(NULL, NULL, 'ALTER ANY CONNECTION')`).
		Scan(&version, &edition, &mayLogins, &mayKill)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("SQL Server on port %d refused the login in %s: %s", port, file, mssqlMessage(err))
	}
	a := &mssqlAdmin{db: h, version: version, edition: edition}
	if err := mssqlVersionOK(version); err != nil {
		a.close()
		return nil, err
	}
	if mayLogins != 1 || mayKill != 1 {
		a.close()
		return nil, fmt.Errorf("the login in %s may not manage logins and connections; give it sysadmin, "+
			"or ALTER ANY LOGIN, ALTER ANY CONNECTION and db_owner on the databases Argus opens", file)
	}
	return a, nil
}

// mssqlVersionOK refuses servers older than SQL Server 2016 (13.x): the
// statements Argus gives logins with need it, and older ones are out of
// support.
func mssqlVersionOK(v string) error {
	maj, _ := versionParts(v)
	if maj < 13 {
		return fmt.Errorf("SQL Server %s is older than Argus supports; it needs SQL Server 2016 or later", v)
	}
	return nil
}

// mssqlSession is one open session.
type mssqlSession struct {
	f   *mssqlFactory
	g   tunnel.Grant
	db  tunnel.GrantDB
	id  string
	rec *stmtRecorder
	log *slog.Logger

	login   string
	pw      string
	key     string
	version string

	pool *sql.DB
	conn *sql.Conn
	spid int64

	timeout time.Duration

	cmu    sync.Mutex
	curID  uint32
	stop   context.CancelFunc
	killed string
}

func (f *mssqlFactory) open(ctx context.Context, g tunnel.Grant, id string) (*mssqlSession, error) {
	db := *g.DB
	admin, err := mssqlAdminConn(ctx, db.Host, db.Port, db.ConfigPath)
	if err != nil {
		return nil, err
	}
	defer admin.close()
	s := &mssqlSession{f: f, g: g, db: db, id: id, log: f.cfg.Log, version: admin.version,
		login:   tunnel.MSSQLLogin(g.Principal, db.Level, db.Name),
		timeout: ms(db.StatementTimeoutMS, defaultStmtTimeout)}
	s.key = strconv.Itoa(db.Port) + "\x00" + s.login

	var state sql.NullString
	if err := admin.db.QueryRowContext(ctx, "SELECT state_desc FROM sys.databases WHERE name = @p1",
		db.Name).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("SQL Server did not answer: %s", mssqlMessage(err))
	}
	switch {
	case !state.Valid:
		return nil, fmt.Errorf("SQL Server on port %d has no database %s", db.Port, db.Name)
	case state.String != "ONLINE":
		return nil, fmt.Errorf("the database %s is %s, not online", db.Name, strings.ToLower(state.String))
	}

	f.mu.Lock()
	err = s.giveLogin(ctx, admin)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.connect(ctx); err != nil {
		s.takeLogin(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

// giveLogin makes the person's login for this level and database if it is
// new, maps it to a user in the database holding exactly the level's roles
// and permissions, gives it this session's password and enables it. A second
// session of the same login shares the password the first one set. Logins
// Argus left enabled -- an agent that stopped mid-session -- are disabled
// first.
func (s *mssqlSession) giveLogin(ctx context.Context, admin *mssqlAdmin) error {
	s.sweep(ctx, admin)
	first := s.f.uses.add(s.key, 1) == 1
	pw, _, err := s.f.uses.password(s.key, sessionPassword)
	if err != nil {
		s.f.uses.add(s.key, -1)
		return err
	}
	s.pw = pw
	if !first {
		return nil
	}
	if err := mssqlSetLogin(ctx, admin, s.login, pw, s.db.Name, s.db.Level); err != nil {
		s.f.uses.add(s.key, -1)
		return fmt.Errorf("could not prepare your SQL Server login %s: %s", s.login, mssqlMessage(err))
	}
	return nil
}

// mssqlSetLogin brings a level's login and its user to exactly the level, and
// enables it with pw. The login is created disabled and enabled last, so a
// failure on the way leaves nothing that can sign in.
func mssqlSetLogin(ctx context.Context, admin *mssqlAdmin, login, pw, dbName, level string) error {
	L, P, N := tunnel.MSSQLIdent(login), tunnel.MSSQLString(pw), tunnel.MSSQLString(login)
	D := tunnel.MSSQLIdent(dbName)
	stmts := []string{
		"IF SUSER_ID(" + N + ") IS NULL BEGIN CREATE LOGIN " + L + " WITH PASSWORD = " + P +
			", CHECK_POLICY = OFF, CHECK_EXPIRATION = OFF, DEFAULT_DATABASE = " + D + "; ALTER LOGIN " + L + " DISABLE END",
		"ALTER LOGIN " + L + " DISABLE",
		"ALTER LOGIN " + L + " WITH PASSWORD = " + P + ", CHECK_POLICY = OFF, CHECK_EXPIRATION = OFF",
		"ALTER LOGIN " + L + " WITH DEFAULT_DATABASE = " + D,
	}
	// The server's permissions: exactly the level's.
	for _, p := range []string{"VIEW SERVER STATE"} {
		stmts = append(stmts, "REVOKE "+p+" FROM "+L)
	}
	for _, p := range tunnel.MSSQLServerPermissions[level] {
		stmts = append(stmts, "GRANT "+p+" TO "+L)
	}
	for _, q := range stmts {
		if err := admin.exec(ctx, q); err != nil {
			return err
		}
	}
	// The database's part on one connection held for it: the driver resets
	// a pooled connection between uses, which would undo the USE.
	c, err := admin.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	exec := func(q string) error {
		_, err := c.ExecContext(ctx, q)
		return err
	}
	if err := exec("USE " + D); err != nil {
		return err
	}
	// The user: made for the login, or mapped back to it when the database
	// came from another server's backup and its user's SID is someone else's.
	if err := exec("IF USER_ID(" + N + ") IS NULL CREATE USER " + L + " FOR LOGIN " + L +
		" WITH DEFAULT_SCHEMA = [dbo] ELSE ALTER USER " + L + " WITH LOGIN = " + L); err != nil {
		return err
	}
	// Take away every role and permission it holds, whoever gave them, then
	// give the level's.
	var undo []string
	rows, err := c.QueryContext(ctx, `SELECT r.name FROM sys.database_role_members m
	  JOIN sys.database_principals r ON r.principal_id = m.role_principal_id
	  WHERE m.member_principal_id = USER_ID(@p1)`, login)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r string
		if rows.Scan(&r) == nil {
			undo = append(undo, "ALTER ROLE "+tunnel.MSSQLIdent(r)+" DROP MEMBER "+L)
		}
	}
	_ = rows.Close()
	rows, err = c.QueryContext(ctx, `SELECT p.class, p.permission_name,
	  ISNULL(QUOTENAME(OBJECT_SCHEMA_NAME(p.major_id)) + '.' + QUOTENAME(OBJECT_NAME(p.major_id)), ''),
	  ISNULL(QUOTENAME(COL_NAME(p.major_id, p.minor_id)), ''), ISNULL(QUOTENAME(SCHEMA_NAME(p.major_id)), '')
	  FROM sys.database_permissions p
	  WHERE p.grantee_principal_id = USER_ID(@p1) AND p.class IN (0, 1, 3)
	    AND NOT (p.class = 0 AND p.permission_name = 'CONNECT')`, login)
	if err != nil {
		return err
	}
	for rows.Next() {
		var class int
		var perm, obj, col, schema string
		if rows.Scan(&class, &perm, &obj, &col, &schema) != nil {
			continue
		}
		switch {
		case class == 0:
			undo = append(undo, "REVOKE "+perm+" FROM "+L+" CASCADE")
		case class == 1 && obj != "":
			on := obj
			if col != "" && col != "[]" {
				on += " (" + col + ")"
			}
			undo = append(undo, "REVOKE "+perm+" ON OBJECT::"+on+" FROM "+L+" CASCADE")
		case class == 3 && schema != "":
			undo = append(undo, "REVOKE "+perm+" ON SCHEMA::"+schema+" FROM "+L+" CASCADE")
		}
	}
	_ = rows.Close()
	for _, q := range undo {
		if err := exec(q); err != nil {
			return err
		}
	}
	give := []string{"GRANT CONNECT TO " + L}
	for _, r := range tunnel.MSSQLRoles[level] {
		give = append(give, "ALTER ROLE "+tunnel.MSSQLIdent(r)+" ADD MEMBER "+L)
	}
	for _, p := range tunnel.MSSQLPermissions[level] {
		give = append(give, "GRANT "+p+" TO "+L)
	}
	for _, q := range give {
		if err := exec(q); err != nil {
			return err
		}
	}
	return exec("ALTER LOGIN " + L + " ENABLE")
}

// sweep disables Argus's logins with no live session on this agent, and
// ends their connections.
func (s *mssqlSession) sweep(ctx context.Context, admin *mssqlAdmin) {
	rows, err := admin.db.QueryContext(ctx,
		"SELECT TOP (2000) name FROM sys.sql_logins WHERE name LIKE 'argus-%' AND is_disabled = 0")
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil && tunnel.IsStoreUser(n) && !s.f.uses.live(strconv.Itoa(s.db.Port)+"\x00"+n) {
			stale = append(stale, n)
		}
	}
	_ = rows.Close()
	for _, n := range stale {
		mssqlDisable(ctx, admin, n)
	}
}

// mssqlDisable gives a login a password nobody knows, disables it and ends
// its connections: disabling alone leaves open ones working.
func mssqlDisable(ctx context.Context, admin *mssqlAdmin, login string) error {
	pw, err := sessionPassword()
	if err != nil {
		return err
	}
	L := tunnel.MSSQLIdent(login)
	err = admin.exec(ctx, "ALTER LOGIN "+L+" DISABLE")
	if e := admin.exec(ctx, "ALTER LOGIN "+L+" WITH PASSWORD = "+tunnel.MSSQLString(pw)+
		", CHECK_POLICY = OFF"); err == nil {
		err = e
	}
	mssqlKillLogin(ctx, admin, login)
	return err
}

// mssqlKillLogin ends every connection of a login.
func mssqlKillLogin(ctx context.Context, admin *mssqlAdmin, login string) {
	rows, err := admin.db.QueryContext(ctx,
		"SELECT session_id FROM sys.dm_exec_sessions WHERE login_name = @p1 AND session_id <> @@SPID", login)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		_ = admin.exec(ctx, "KILL "+strconv.FormatInt(id, 10))
	}
}

// takeLogin ends this session's hold on the login; the last one disables it
// with a password nobody knows and ends its connections.
func (s *mssqlSession) takeLogin(ctx context.Context) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.uses.add(s.key, -1) > 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	admin, err := mssqlAdminConn(cctx, s.db.Host, s.db.Port, s.db.ConfigPath)
	if err != nil {
		s.log.Error("sql server: could not disable a login after its session", "login", s.login, "error", err,
			"detail", "it is disabled at the next session on this server")
		return
	}
	defer admin.close()
	if err := mssqlDisable(cctx, admin, s.login); err != nil {
		s.log.Error("sql server: could not disable a login after its session", "login", s.login, "error", err)
	}
}

// connect signs in as the person's login in the entry's database.
func (s *mssqlSession) connect(ctx context.Context) error {
	pool, err := sql.Open("sqlserver", mssqlDSN(s.db.Host, s.db.Port, s.login, s.pw, s.db.Name))
	if err != nil {
		return err
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxLifetime(0)
	conn, err := pool.Conn(ctx)
	if err != nil {
		_ = pool.Close()
		return fmt.Errorf("could not sign in to SQL Server as %s: %s", s.login, mssqlMessage(err))
	}
	if err := conn.QueryRowContext(ctx, "SELECT @@SPID").Scan(&s.spid); err != nil {
		_ = conn.Close()
		_ = pool.Close()
		return fmt.Errorf("SQL Server did not answer: %s", mssqlMessage(err))
	}
	s.pool, s.conn = pool, conn
	return nil
}

func (s *mssqlSession) close(ctx context.Context) {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.pool != nil {
		_ = s.pool.Close()
	}
	s.takeLogin(ctx)
}

// kill stops the running statement: an attention, which leaves the
// connection and its transaction as they were.
func (s *mssqlSession) kill(why string) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if s.killed == "" {
		s.killed = why
	}
	if s.stop != nil {
		s.stop()
	}
}

func (s *mssqlSession) send(a tunnel.PGAnswer) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.rec.answer(b, func() ([]byte, error) {
		return json.Marshal(tunnel.PGAnswer{ID: a.ID, Type: "error", Error: tunnel.ErrAnswerTooLarge.Error()})
	})
}

func (s *mssqlSession) serve(ctx context.Context, st tunnel.Stream) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	s.rec.attach(st)
	reason := "closed"
	defer func() {
		s.close(context.WithoutCancel(ctx))
		s.rec.close(reason)
		s.log.Info("tunnel: sql server session closed", "session", s.id, "login", s.login, "reason", reason)
	}()
	if s.rec.write(tunnel.PGRecord{Kind: "open", Actor: s.g.Email, Role: s.login,
		Level: s.db.Level, Grant: s.g.ID}) != nil {
		reason = "the recording failed"
		return
	}
	if s.send(tunnel.PGAnswer{Type: "ready", Role: s.login, Level: s.db.Level, Version: s.version,
		Session: s.id, ReadOnly: s.db.Level == tunnel.LevelRead, Via: "tcp"}) != nil {
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
	deadline := time.NewTimer(secs(s.db.MaxSessionS, defaultMaxSession))
	defer deadline.Stop()
	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	limiter := newRateLimit(s.f.cfg.MaxRequestsPerMinute, time.Minute)
	end := func(why string) {
		reason = why
		_ = s.send(tunnel.PGAnswer{Type: "closed", Error: why})
		stop()
		<-readerDone
	}
	for {
		select {
		case <-ctx.Done():
			<-readerDone
			return
		case <-deadline.C:
			end("the session reached its longest allowed length")
			return
		case <-idleT.C:
			end("the session was idle too long")
			return
		case req := <-reqs:
			idleT.Reset(idle)
			var err error
			switch req.Op {
			case "run":
				if !limiter.allow(time.Now()) {
					err = s.send(tunnel.PGAnswer{ID: req.ID, Type: "error",
						Error: "too many statements in a minute; wait a moment"})
					break
				}
				err = s.run(ctx, req)
			case "schema":
				err = s.schema(ctx, req)
			case "admin":
				err = s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "extensions and replication are Postgres's"})
			default:
				// check is Postgres's: the console treats this answer as an
				// agent without it and runs as before.
				err = s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "unknown operation " + req.Op +
					" for SQL Server"})
			}
			if err != nil {
				reason = err.Error()
				return
			}
		}
	}
}

// read takes requests off the stream. A cancel acts at once.
func (s *mssqlSession) read(ctx context.Context, st tunnel.Stream, reqs chan<- tunnel.PGRequest) {
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil || typ != tunnel.FramePGRequest {
			return
		}
		var req tunnel.PGRequest
		if json.Unmarshal(p, &req) != nil {
			return
		}
		if req.Op == "cancel" {
			s.cmu.Lock()
			running := s.curID != 0 && (req.ID == 0 || req.ID == s.curID)
			s.cmu.Unlock()
			if running {
				s.kill("cancelled")
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

// run executes one batch. An error return ends the session: the recording,
// the stream or the connection failed. A statement's own failure is an
// answer.
func (s *mssqlSession) run(ctx context.Context, req tunnel.PGRequest) error {
	text := strings.TrimSpace(req.SQL)
	switch {
	case text == "":
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "there is no statement to run"})
	case !tunnel.RecordedTextFits(text):
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: tunnel.ErrRecordTooLong.Error()})
	case !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0:
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "the statement is not valid text"})
	}
	params, bad := tunnel.PGParamArgs(req.Params)
	if bad != "" {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: bad})
	}
	query, bad := mssqlPlaceholders(text, len(params))
	if bad != "" {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: bad})
	}
	args := make([]any, 0, len(params)+1)
	for _, p := range params {
		if p == nil {
			args = append(args, nil)
		} else {
			args = append(args, string(p))
		}
	}
	class := tunnel.ClassifySQL(text)
	if g := tunnel.GuardSQL(class, s.db.Name, req.Confirm); g != nil {
		if err := s.rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: text, Params: req.Params, Approval: req.Approval}); err != nil {
			return fmt.Errorf("the recording failed: %w", err)
		}
		if err := s.rec.write(tunnel.PGRecord{Kind: "error", Request: req.ID, Code: g.Code, Error: g.Reason}); err != nil {
			return err
		}
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Code: g.Code, Error: g.Reason, Guard: g})
	}
	maxRows := req.MaxRows
	if maxRows <= 0 {
		maxRows = tunnel.DefaultMaxRows
	}
	if s.db.MaxRows > 0 && maxRows > s.db.MaxRows {
		maxRows = s.db.MaxRows
	}
	maxRows = min(maxRows, tunnel.MaxRowsCeiling)

	// Recorded and sent before SQL Server sees it. No record, no statement.
	if err := s.rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: text, Params: req.Params, Approval: req.Approval}); err != nil {
		if errors.Is(err, tunnel.ErrRecordTooLong) {
			return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
		}
		return fmt.Errorf("the recording failed: %w", err)
	}

	qctx, qcancel := context.WithCancel(ctx)
	defer qcancel()
	s.cmu.Lock()
	s.curID, s.killed, s.stop = req.ID, "", qcancel
	s.cmu.Unlock()
	defer func() {
		s.cmu.Lock()
		s.curID, s.stop = 0, nil
		s.cmu.Unlock()
	}()
	timer := time.AfterFunc(s.timeout, func() { s.kill("the statement ran longer than the timeout") })
	defer timer.Stop()

	start := time.Now()
	// An approved change holds to the rows the approver saw counted: it
	// runs in a transaction of its own, or under a savepoint inside the
	// person's, and is rolled back when it changes more.
	var capEnd func(ok bool)
	if req.RowCap != nil && tunnel.MSSQLWrites(text) {
		var err error
		if capEnd, err = s.capOpen(ctx); err != nil {
			return s.failed(ctx, req.ID, err, 0, time.Since(start).Milliseconds(), nil)
		}
	}

	res := s.batch(qctx, req, query, args, maxRows)
	elapsed := time.Since(start).Milliseconds()
	if res.sendErr != nil {
		return res.sendErr
	}
	s.cmu.Lock()
	killed := s.killed
	s.cmu.Unlock()
	if res.err != nil && res.truncated && killed == "the row limit" && !isMSSQLError(res.err) {
		res.err = nil
	}
	if res.err == nil && killed != "" && killed != "the row limit" {
		res.err = context.Canceled
	}
	if res.err != nil {
		if capEnd != nil {
			capEnd(false)
		}
		return s.failed(ctx, req.ID, res.err, res.rows, elapsed, res.notices)
	}
	verb := strings.ToUpper(class.Verb)
	tag := verb
	rowsOut := res.rows
	if res.cols {
		if verb == "" || !class.Writes {
			tag = "SELECT"
		}
		tag += " " + strconv.FormatInt(res.rows, 10)
	} else {
		if res.affectedSeen {
			rowsOut = res.affected
			if class.Writes {
				tag += " " + strconv.FormatInt(res.affected, 10)
			}
		}
		if tag == "" {
			tag = "OK"
		}
	}
	if capEnd != nil {
		if res.affected > *req.RowCap {
			capEnd(false)
			err := fmt.Errorf("it changed %d rows, more than the %d approved, so it was rolled back", res.affected, *req.RowCap)
			return s.failed(ctx, req.ID, err, res.affected, elapsed, res.notices)
		}
		capEnd(true)
	}
	if err := s.rec.write(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: tag, Rows: rowsOut, MS: elapsed,
		ResultSHA256: res.sum, Truncated: res.truncated, Notices: res.notices}); err != nil {
		return err
	}
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "done", Tag: tag, Rows: rowsOut, MS: elapsed,
		Truncated: res.truncated, Notices: res.notices})
}

// mssqlBatch is what running one batch came to.
type mssqlBatch struct {
	// cols says a result set was shown; rows is how many of its rows.
	cols bool
	rows int64
	// affected sums the rows each statement of the batch changed (or
	// returned); affectedSeen says SQL Server reported any.
	affected     int64
	affectedSeen bool
	truncated    bool
	sum          string
	notices      []tunnel.PGNotice
	err          error
	sendErr      error
}

// batch runs a batch and streams its first result set. SQL Server answers a
// batch with a sequence of messages: result sets, row counts, PRINT and
// informational messages, and errors, which do not always stop it. The first
// result set is the answer, as one request is one answer; the rest are read
// and dropped, and every message is kept beside it, as a Postgres notice is.
func (s *mssqlSession) batch(ctx context.Context, req tunnel.PGRequest, query string, args []any, maxRows int) mssqlBatch {
	var out mssqlBatch
	ret := &sqlexp.ReturnMessage{}
	rows, err := s.conn.QueryContext(ctx, query, append(args, ret)...)
	if err != nil {
		out.err = err
		return out
	}
	defer rows.Close()
	h := sha256.New()
	enc := tunnel.NewRowEncoder(req.ID)
	dropped := 0
	note := func(n tunnel.PGNotice) {
		if len(out.notices) >= tunnel.MaxPGNotices {
			dropped++
			return
		}
		out.notices = append(out.notices, n)
	}
	flush := func() {
		if enc.Rows() == 0 || out.sendErr != nil {
			return
		}
		h.Write(enc.Batch()[4:])
		out.sendErr = s.rec.frame(tunnel.FramePGRows, enc.Batch())
		enc.Reset()
	}
	whole := 0
	for active := true; active && out.sendErr == nil; {
		switch m := ret.Message(ctx).(type) {
		case sqlexp.MsgNotice:
			n := tunnel.PGNotice{Severity: "INFO", Message: clipNotice(m.Message.String())}
			var me mssql.Error
			if errors.As(asError(m.Message), &me) {
				n.Message = clipNotice(me.Message)
				if me.Number != 0 {
					n.Code = strconv.Itoa(int(me.Number))
				}
				if me.Class > 10 {
					n.Severity = "WARNING"
				}
			}
			note(n)
		case sqlexp.MsgError:
			if out.err == nil {
				out.err = m.Error
			}
		case sqlexp.MsgRowsAffected:
			out.affected += m.Count
			out.affectedSeen = true
		case sqlexp.MsgNextResultSet:
			active = rows.NextResultSet()
		case sqlexp.MsgNext:
			cts, cerr := rows.ColumnTypes()
			if out.cols || out.truncated || cerr != nil || len(cts) == 0 {
				for rows.Next() {
				}
				continue
			}
			out.cols = true
			out.sendErr = s.sendCols(req.ID, cts)
			types := make([]string, len(cts))
			for i, c := range cts {
				types[i] = c.DatabaseTypeName()
			}
			raw := make([]any, len(cts))
			dest := make([]any, len(cts))
			for i := range raw {
				dest[i] = &raw[i]
			}
			vals := make([][]byte, len(cts))
			for out.sendErr == nil && rows.Next() {
				if out.rows >= int64(maxRows) {
					out.truncated = true
					s.kill("the row limit")
					break
				}
				if err := rows.Scan(dest...); err != nil {
					if out.err == nil {
						out.err = err
					}
					break
				}
				for i, v := range raw {
					vals[i] = mssqlText(v, types[i])
				}
				if !enc.Fits(vals) {
					flush()
				}
				kept := enc.Add(vals)
				if req.Whole && kept != nil && out.sendErr == nil {
					flush()
					for col, k := range kept {
						v := vals[col]
						if len(v) > tunnel.MaxWholeValue || whole+len(v)-k > tunnel.MaxWholeResult {
							continue
						}
						whole += len(v) - k
						h.Write(v[k:])
						for _, b := range tunnel.PartAnswers(req.ID, out.rows, col, v, k) {
							if out.sendErr == nil {
								out.sendErr = s.rec.frame(tunnel.FramePGAnswer, b)
							}
						}
					}
				}
				out.rows++
				if enc.Full() {
					flush()
				}
			}
			flush()
		default:
			// MsgLastInsertID and anything newer: nothing to show.
		}
	}
	if err := rows.Err(); err != nil && out.err == nil {
		out.err = err
	}
	if dropped > 0 && len(out.notices) > 0 {
		last := &out.notices[len(out.notices)-1]
		last.Detail = clipNotice(strings.TrimSpace(last.Detail + " " + strconv.Itoa(dropped) + " more messages were not kept."))
	}
	out.sum = hex.EncodeToString(h.Sum(nil))
	return out
}

func asError(v fmt.Stringer) error {
	if e, ok := v.(error); ok {
		return e
	}
	return nil
}

func isMSSQLError(err error) bool {
	var me mssql.Error
	return errors.As(err, &me)
}

// capOpen starts what holds an approved change to its cap, and returns
// what ends it: commit on true, roll back on false. Inside the person's own
// transaction it is a savepoint, which SQL Server rolls back to but never
// releases.
func (s *mssqlSession) capOpen(ctx context.Context) (func(ok bool), error) {
	var n int
	if err := s.conn.QueryRowContext(ctx, "SELECT @@TRANCOUNT").Scan(&n); err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	if n == 0 {
		if _, err := s.conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
			return nil, err
		}
		return func(ok bool) {
			q := "IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION"
			if ok {
				q = "IF @@TRANCOUNT > 0 COMMIT TRANSACTION"
			}
			_, _ = s.conn.ExecContext(bg, q)
		}, nil
	}
	if _, err := s.conn.ExecContext(ctx, "SAVE TRANSACTION argus_cap"); err != nil {
		return nil, err
	}
	return func(ok bool) {
		if !ok {
			_, _ = s.conn.ExecContext(bg, "IF XACT_STATE() = 1 ROLLBACK TRANSACTION argus_cap")
		}
	}, nil
}

// mssqlMessage is SQL Server's own message for an error, without the
// driver's "mssql: " in front.
func mssqlMessage(err error) string {
	var me mssql.Error
	if errors.As(err, &me) {
		return me.Message
	}
	return err.Error()
}

// failed records and answers a statement's failure. SQL Server's own error
// is passed on as it is; a lost connection ends the session.
func (s *mssqlSession) failed(ctx context.Context, id uint32, err error, rows, elapsed int64, notices []tunnel.PGNotice) error {
	a := tunnel.PGAnswer{ID: id, Type: "error", Error: err.Error(), MS: elapsed, Notices: notices}
	s.cmu.Lock()
	killed := s.killed
	s.cmu.Unlock()
	var me mssql.Error
	switch {
	case errors.As(err, &me):
		a.Error = me.Message
		a.Detail = "SQL Server error " + strconv.Itoa(int(me.Number)) + ", severity " + strconv.Itoa(int(me.Class))
		if me.LineNo > 0 {
			a.Detail += ", line " + strconv.Itoa(int(me.LineNo))
		}
		if me.ProcName != "" {
			a.Detail += ", in " + me.ProcName
		}
		if len(me.All) > 1 {
			// Every error the batch raised, first to last: the last one is
			// often only "the statement has been terminated".
			var msgs []string
			for _, e := range me.All {
				msgs = append(msgs, e.Message)
			}
			a.Error = strings.Join(msgs, " ")
		}
	case killed != "" && ctx.Err() == nil:
		a.Error, a.Code = "stopped: "+killed, "57014"
	case ctx.Err() == nil && s.conn.PingContext(context.WithoutCancel(ctx)) != nil:
		_ = s.rec.write(tunnel.PGRecord{Kind: "error", Request: id, Error: err.Error(), MS: elapsed})
		_ = s.send(a)
		return errors.New("the connection to SQL Server ended")
	}
	if rerr := s.rec.write(tunnel.PGRecord{Kind: "error", Request: id, Code: a.Code,
		Error: a.Error, MS: elapsed, Rows: rows, Notices: notices}); rerr != nil {
		return rerr
	}
	return s.send(a)
}

func (s *mssqlSession) sendCols(id uint32, cts []*sql.ColumnType) error {
	cols := make([]tunnel.PGColumn, len(cts))
	for i, c := range cts {
		cols[i] = tunnel.PGColumn{Name: c.Name(), Type: strings.ToLower(c.DatabaseTypeName())}
	}
	return s.send(tunnel.PGAnswer{ID: id, Type: "cols", Cols: cols})
}

// mssqlText is a value as text, the way SQL Server Management Studio shows
// it: binary as 0x and hex, a uniqueidentifier in its usual form, bit as 1
// or 0, and dates and times at their type's precision. nil is NULL.
func mssqlText(v any, typ string) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		switch typ {
		case "UNIQUEIDENTIFIER":
			var u mssql.UniqueIdentifier
			if u.Scan(x) == nil {
				return []byte(u.String())
			}
		case "BINARY", "VARBINARY", "IMAGE", "TIMESTAMP", "ROWVERSION":
			return []byte("0x" + strings.ToUpper(hex.EncodeToString(x)))
		}
		return append([]byte(nil), x...)
	case string:
		return []byte(x)
	case int64:
		return strconv.AppendInt(nil, x, 10)
	case float64:
		bits := 64
		if typ == "REAL" {
			bits = 32
		}
		return strconv.AppendFloat(nil, x, 'g', -1, bits)
	case bool:
		if x {
			return []byte("1")
		}
		return []byte("0")
	case time.Time:
		switch typ {
		case "DATE":
			return []byte(x.Format("2006-01-02"))
		case "TIME":
			return []byte(x.Format("15:04:05.9999999"))
		case "DATETIME":
			return []byte(x.Format("2006-01-02 15:04:05.000"))
		case "SMALLDATETIME":
			return []byte(x.Format("2006-01-02 15:04:05"))
		case "DATETIMEOFFSET":
			return []byte(x.Format("2006-01-02 15:04:05.9999999 -07:00"))
		}
		return []byte(x.Format("2006-01-02 15:04:05.9999999"))
	}
	return []byte(fmt.Sprint(v))
}

// mssqlPlaceholders turns the console's $1, $2 ... into SQL Server's @p1,
// @p2 ..., which the driver binds in order. Quoted text, quoted names and
// comments are left alone.
func mssqlPlaceholders(s string, n int) (string, string) {
	if n == 0 {
		return s, ""
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := i + 1
			for j < len(s) {
				if s[j] == end {
					if j+1 < len(s) && s[j+1] == end {
						j += 2
						continue
					}
					break
				}
				j++
			}
			j = min(j+1, len(s))
			b.WriteString(s[i:j])
			i = j
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = len(s) - i
			}
			b.WriteString(s[i : i+j])
			i += j
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			// T-SQL block comments nest.
			j, depth := i, 0
			for j < len(s) {
				if s[j] == '/' && j+1 < len(s) && s[j+1] == '*' {
					depth++
					j += 2
					continue
				}
				if s[j] == '*' && j+1 < len(s) && s[j+1] == '/' {
					depth--
					j += 2
					if depth == 0 {
						break
					}
					continue
				}
				j++
			}
			b.WriteString(s[i:j])
			i = j
		case c == '$' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j := i + 1
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			k, _ := strconv.Atoi(s[i+1 : j])
			if k < 1 || k > n {
				return "", fmt.Sprintf("$%d has no value", k)
			}
			b.WriteString("@p" + strconv.Itoa(k))
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), ""
}

// Object tree queries, as the person's user. SQL Server shows a user only the
// objects it holds some permission on; every level holds VIEW DEFINITION, so
// that is all of them.
const (
	msSchemasSQL = `SELECT s.name, ISNULL(USER_NAME(s.principal_id), ''),
	  (SELECT COUNT(*) FROM sys.tables t WHERE t.schema_id = s.schema_id)
	FROM sys.schemas s
	WHERE s.name NOT IN ('sys', 'INFORMATION_SCHEMA', 'guest') AND s.name NOT LIKE 'db[_]%'
	ORDER BY CASE WHEN s.name = SCHEMA_NAME() THEN 0 ELSE 1 END, s.name`
	msObjectsSQL = `SELECT TOP (5000) o.name,
	  CASE WHEN o.type = 'U' THEN 'table' WHEN o.type = 'V' THEN 'view' WHEN o.type IN ('P', 'PC') THEN 'procedure'
	       WHEN o.type = 'SO' THEN 'sequence' WHEN o.type = 'SN' THEN 'synonym' ELSE 'function' END,
	  CASE WHEN o.type = 'U' THEN ISNULL((SELECT CAST(SUM(a.used_pages) AS bigint) FROM sys.partitions p
	        JOIN sys.allocation_units a ON a.container_id = p.partition_id WHERE p.object_id = o.object_id), -1)
	       ELSE -1 END,
	  ISNULL((SELECT SUM(p.rows) FROM sys.partitions p WHERE p.object_id = o.object_id AND p.index_id IN (0, 1)), 0)
	FROM sys.objects o
	WHERE o.schema_id = SCHEMA_ID(@p1) AND o.is_ms_shipped = 0
	  AND o.type IN ('U', 'V', 'P', 'PC', 'FN', 'IF', 'TF', 'FS', 'FT', 'SO', 'SN')
	ORDER BY CASE WHEN o.type IN ('U', 'V') THEN 0 ELSE 1 END, 2, 1`
	msColumnsSQL = `SELECT TOP (2000) c.name, 'column',
	  TYPE_NAME(c.user_type_id) +
	    CASE WHEN TYPE_NAME(c.user_type_id) IN ('varchar', 'char', 'varbinary', 'binary')
	           THEN '(' + CASE WHEN c.max_length = -1 THEN 'max' ELSE CAST(c.max_length AS varchar(10)) END + ')'
	         WHEN TYPE_NAME(c.user_type_id) IN ('nvarchar', 'nchar')
	           THEN '(' + CASE WHEN c.max_length = -1 THEN 'max' ELSE CAST(c.max_length / 2 AS varchar(10)) END + ')'
	         WHEN TYPE_NAME(c.user_type_id) IN ('decimal', 'numeric')
	           THEN '(' + CAST(c.precision AS varchar(10)) + ',' + CAST(c.scale AS varchar(10)) + ')'
	         WHEN TYPE_NAME(c.user_type_id) IN ('datetime2', 'time', 'datetimeoffset')
	           THEN '(' + CAST(c.scale AS varchar(10)) + ')'
	         ELSE '' END +
	    CASE WHEN c.is_identity = 1 THEN ' identity' ELSE '' END +
	    CASE WHEN c.is_nullable = 0 THEN ' not null' ELSE '' END +
	    CASE WHEN EXISTS (SELECT 1 FROM sys.indexes i JOIN sys.index_columns ic
	           ON ic.object_id = i.object_id AND ic.index_id = i.index_id
	           WHERE i.object_id = c.object_id AND i.is_primary_key = 1 AND ic.column_id = c.column_id)
	         THEN ' · primary key' ELSE '' END,
	  c.column_id
	FROM sys.columns c
	WHERE c.object_id = OBJECT_ID(QUOTENAME(@p1) + '.' + QUOTENAME(@p2))
	ORDER BY c.column_id`
	msCompletionSQL = `SELECT TOP (20000) x.s, x.n, x.k FROM (
	  SELECT 1 AS o, s.name AS s, s.name AS n, 'schema' AS k FROM sys.schemas s
	  WHERE s.name NOT IN ('sys', 'INFORMATION_SCHEMA', 'guest') AND s.name NOT LIKE 'db[_]%'
	  UNION ALL
	  SELECT 2, SCHEMA_NAME(o.schema_id), o.name,
	    CASE WHEN o.type = 'U' THEN 'table' WHEN o.type = 'V' THEN 'view' WHEN o.type IN ('P', 'PC') THEN 'procedure'
	         WHEN o.type = 'SO' THEN 'sequence' WHEN o.type = 'SN' THEN 'synonym' ELSE 'function' END
	  FROM sys.objects o
	  WHERE o.is_ms_shipped = 0 AND o.type IN ('U', 'V', 'P', 'PC', 'FN', 'IF', 'TF', 'FS', 'FT', 'SO', 'SN')
	) x ORDER BY x.o, x.s, x.n`
)

// schema answers the object tree and autocomplete. Not recorded: it reads
// the catalogue as the person's user and changes nothing.
func (s *mssqlSession) schema(ctx context.Context, req tunnel.PGRequest) error {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out []tunnel.PGObject
	query := func(q string, args ...any) error {
		rows, err := s.conn.QueryContext(qctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a, b, c string
			var d int64
			var err error
			switch req.Kind {
			case "schemas":
				err = rows.Scan(&a, &b, &d)
				out = append(out, tunnel.PGObject{Name: a, Kind: "schema", Detail: b, Count: d})
			case "completion":
				err = rows.Scan(&a, &b, &c)
				out = append(out, tunnel.PGObject{Schema: a, Name: b, Kind: c})
			case "columns":
				err = rows.Scan(&a, &b, &c, &d)
				out = append(out, tunnel.PGObject{Name: a, Kind: b, Detail: c})
			default:
				var pages int64
				err = rows.Scan(&a, &b, &pages, &d)
				o := tunnel.PGObject{Name: a, Kind: b, Count: d}
				if pages >= 0 && b == "table" {
					o.Detail = prettyBytes(pages * 8192)
				}
				out = append(out, o)
			}
			if err != nil {
				return err
			}
		}
		return rows.Err()
	}
	var err error
	switch req.Kind {
	case "schemas":
		err = query(msSchemasSQL)
	case "objects":
		err = query(msObjectsSQL, req.Schema)
	case "columns":
		err = query(msColumnsSQL, req.Schema, req.Object)
	case "completion":
		var def string
		if err = s.conn.QueryRowContext(qctx, "SELECT ISNULL(SCHEMA_NAME(), 'dbo')").Scan(&def); err == nil {
			out = append(out, tunnel.PGObject{Schema: def, Name: def, Kind: "path", Detail: "1"})
			err = query(msCompletionSQL)
		}
	default:
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "unknown schema request " + req.Kind})
	}
	if err != nil {
		if s.conn.PingContext(context.WithoutCancel(ctx)) != nil {
			return errors.New("the connection to SQL Server ended")
		}
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
	}
	return s.sendObjects(req.ID, out)
}

// sendObjects answers a schema request in parts under one frame each, as
// the Postgres session does.
func (s *mssqlSession) sendObjects(id uint32, objs []tunnel.PGObject) error {
	start, size := 0, 0
	for i, o := range objs {
		b, _ := json.Marshal(o)
		n := len(b) + 1
		if size+n > schemaPartBudget && i > start {
			if err := s.send(tunnel.PGAnswer{ID: id, Type: "schema", Objects: objs[start:i], More: true}); err != nil {
				return err
			}
			start, size = i, 0
		}
		size += n
	}
	return s.send(tunnel.PGAnswer{ID: id, Type: "schema", Objects: objs[start:]})
}

// discoverMSSQL lists the databases of a SQL Server on this server, with
// their size, leaving out SQL Server's own.
func discoverMSSQL(ctx context.Context, d tunnel.GrantDiscover) tunnel.DiscoverResult {
	admin, err := mssqlAdminConn(ctx, d.Host, d.Port, d.ConfigPath)
	if err != nil {
		return tunnel.DiscoverResult{Error: err.Error()}
	}
	defer admin.close()
	version := admin.version
	rows, err := admin.db.QueryContext(ctx, `SELECT d.name, ISNULL(SUM(CAST(f.size AS bigint)), 0) * 8192
	FROM sys.databases d LEFT JOIN sys.master_files f ON f.database_id = d.database_id
	WHERE d.database_id > 4 GROUP BY d.name ORDER BY d.name`)
	if err != nil {
		return tunnel.DiscoverResult{Error: "SQL Server did not answer: " + mssqlMessage(err), Version: version}
	}
	defer rows.Close()
	res := tunnel.DiscoverResult{OK: true, Version: version}
	for rows.Next() {
		var name string
		var size int64
		if rows.Scan(&name, &size) != nil || !tunnel.ValidMSSQLDB(name) || strings.HasPrefix(name, tunnel.TestDBPrefix) {
			continue
		}
		if len(res.Databases) >= tunnel.MaxDiscovered {
			res.Truncated = true
			break
		}
		res.Databases = append(res.Databases, tunnel.Discovered{Name: name, Bytes: size})
	}
	return res
}

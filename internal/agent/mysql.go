package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
MySQL and MariaDB in the browser, through the tunnel.

	browser ──wss──▶ argus-control ──"mysql" stream──▶ agent ──▶ mysqld on this server

The session speaks the Postgres session's frames, so the console's SQL
workspace, the recording and its verification are Postgres's. See
tunnel/mysql.go for the shape.

The agent signs in as MySQL's administrator on its own server for one thing:
giving the person their level's user (tunnel.MySQLUser) with a one-time
password, and locking it again afterwards. That is root over the unix socket,
which MariaDB 10.4+ and Debian's and Ubuntu's MySQL let the operating system's
root do with no password (unix_socket, auth_socket); or, when the entry names
an option file, the [client] user and password in it, read from this server
for that one connection. Nothing is stored and nothing leaves the server.

The person's statements run on a connection signed in as their user, so what
they may do is what MySQL's GRANTs say, whatever the console or this agent
thought of a statement.
*/

// MySQLService is the tunnel service name.
const MySQLService = tunnel.MySQLService

// mysqlSockets are where mysqld usually puts its socket, looked at when the
// entry names a loopback address: root signs in without a password only
// there.
var mysqlSockets = []string{
	"/run/mysqld/mysqld.sock", "/var/run/mysqld/mysqld.sock",
	"/var/lib/mysql/mysql.sock", "/run/mysql/mysql.sock", "/tmp/mysql.sock",
}

// debianCnf is Debian's maintenance option file, used when root has no way
// in over the socket and the entry names no file.
const debianCnf = "/etc/mysql/debian.cnf"

// MySQLConfig is the service's configuration.
type MySQLConfig struct {
	StoreConfig
	// Sockets replaces mysqlSockets; tests use it.
	Sockets []string
}

// MySQL is the mysql service.
func MySQL(cfg MySQLConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
		cfg.Log = log
	}
	if cfg.MaxRequestsPerMinute <= 0 {
		cfg.MaxRequestsPerMinute = 120
	}
	if cfg.Sockets == nil {
		cfg.Sockets = mysqlSockets
	}
	f := &mysqlFactory{cfg: cfg, uses: &userUse{}}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a mysql session", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(MySQLService) || g.DB == nil || g.DB.EngineOf() != tunnel.EngineMySQL {
			log.Warn("tunnel: refused a mysql session on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for mysql"}.Error())
			return
		}
		switch {
		case !tunnel.ValidPersonalRole(g.Principal):
			call.Refuse("not a user Argus will sign in as: " + g.Principal)
			return
		case tunnel.LevelRank(g.DB.Level) == 0:
			call.Refuse("unknown access level " + g.DB.Level)
			return
		case g.DB.Port <= 0 || g.DB.Port > 65535 || !tunnel.ValidMySQLDB(g.DB.Name):
			call.Refuse("the grant does not say what to open")
			return
		case !tunnel.LocalPGHost(g.DB.Host):
			call.Refuse("MySQL must be on this server; " + g.DB.Host + " is not")
			return
		case g.DB.ConfigPath != "" && !tunnel.ValidMySQLOptionFile(g.DB.ConfigPath):
			call.Refuse("the option file must be an absolute path to a .cnf file")
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
		octx, cancel := context.WithTimeout(ctx, 45*time.Second)
		s, err := f.open(octx, g, id)
		cancel()
		if err != nil {
			rec.discard()
			log.Warn("tunnel: mysql session refused", "database", g.DB.Name, "user", g.Principal,
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
		log.Info("tunnel: mysql session opened", "database", g.DB.Name, "user", s.user,
			"level", g.DB.Level, "for", g.Email, "session", id, "version", s.version)
		s.serve(ctx, call.Stream)
		_ = call.Stream.Close()
	}
}

type mysqlFactory struct {
	cfg  MySQLConfig
	uses *userUse
	// mu serializes giving and taking users, so two sessions of one person
	// opening together cannot interleave an unlock with a lock.
	mu sync.Mutex
}

// mysqlWay is how the agent reached mysqld: a socket path, or loopback and
// a port; and the account it signed in with.
type mysqlWay struct {
	socket string
	host   string
	port   int
	user   string
	pw     string
}

func (w mysqlWay) dsn(user, pw, db string) string {
	c := mysql.NewConfig()
	c.User, c.Passwd, c.DBName = user, pw, db
	if w.socket != "" {
		c.Net, c.Addr = "unix", w.socket
	} else {
		c.Net, c.Addr = "tcp", netJoin(w.host, w.port)
	}
	c.InterpolateParams = true
	c.Timeout = 10 * time.Second
	c.AllowNativePasswords = true
	c.Params = map[string]string{"charset": "utf8mb4"}
	return c.FormatDSN()
}

func netJoin(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

func (w mysqlWay) String() string {
	if w.socket != "" {
		return w.socket
	}
	return netJoin(w.host, w.port)
}

// mysqlAdmin is an administrator's connection, for giving and taking users.
type mysqlAdmin struct {
	db      *sql.DB
	way     mysqlWay
	version string
	// host is the client host MySQL sees for connections made this way:
	// "localhost" over the socket, an address or name over TCP. The
	// person's user is made for it.
	host string
}

func (a *mysqlAdmin) close() { _ = a.db.Close() }

func (a *mysqlAdmin) exec(ctx context.Context, q string, args ...any) error {
	_, err := a.db.ExecContext(ctx, q, args...)
	return err
}

func (a *mysqlAdmin) mariaDB() bool { return strings.Contains(strings.ToLower(a.version), "mariadb") }

// adminConn signs in as MySQL's administrator on this server.
func adminConn(ctx context.Context, sockets []string, db tunnel.GrantDB) (*mysqlAdmin, error) {
	var creds []mysqlWay
	if db.ConfigPath != "" {
		w, err := readMySQLOptionFile(db.ConfigPath)
		if err != nil {
			return nil, err
		}
		creds = append(creds, w)
	} else {
		creds = append(creds, mysqlWay{user: "root"})
		if w, err := readMySQLOptionFile(debianCnf); err == nil {
			creds = append(creds, w)
		}
	}
	var ways []mysqlWay
	for _, c := range creds {
		var socks []string
		switch {
		case c.socket != "":
			socks = []string{c.socket}
		case strings.HasPrefix(db.Host, "/"):
			socks = []string{db.Host}
		default:
			socks = sockets
		}
		for _, s := range socks {
			w := c
			w.socket = s
			ways = append(ways, w)
		}
		if c.pw != "" && !strings.HasPrefix(db.Host, "/") {
			// A password works over TCP too; root's socket sign-in does not.
			w := c
			w.socket, w.host, w.port = "", db.Host, db.Port
			ways = append(ways, w)
		}
	}
	var last error
	tried := 0
	for _, w := range ways {
		if w.socket != "" {
			if fi, err := os.Stat(w.socket); err != nil || fi.Mode()&os.ModeSocket == 0 {
				continue
			}
		}
		tried++
		h, err := sql.Open("mysql", w.dsn(w.user, w.pw, ""))
		if err != nil {
			last = err
			continue
		}
		h.SetMaxOpenConns(1)
		var port int
		var version, who string
		err = h.QueryRowContext(ctx, "SELECT @@port, VERSION(), USER()").Scan(&port, &version, &who)
		if err != nil {
			_ = h.Close()
			last = err
			continue
		}
		if port != db.Port {
			// Another mysqld's socket: this server runs more than one.
			_ = h.Close()
			last = fmt.Errorf("the socket %s belongs to the MySQL on port %d", w.socket, port)
			continue
		}
		host := "localhost"
		if i := strings.LastIndexByte(who, '@'); i >= 0 {
			host = who[i+1:]
		}
		a := &mysqlAdmin{db: h, way: w, version: version, host: host}
		if err := mysqlVersionOK(version); err != nil {
			a.close()
			return nil, err
		}
		return a, nil
	}
	if tried == 0 {
		return nil, fmt.Errorf("no MySQL socket was found on this server for port %d; name the socket as the "+
			"entry's address, or an option file with a [client] user and password", db.Port)
	}
	if db.ConfigPath != "" {
		return nil, fmt.Errorf("MySQL refused the account in %s: %w", db.ConfigPath, last)
	}
	return nil, fmt.Errorf("the agent could not sign in to MySQL as root over its socket (%w); give the "+
		"entry an option file with a [client] user and password that may create users and grant", last)
}

// mysqlVersionOK refuses servers without ACCOUNT LOCK, which keeps a
// person's user unusable between sessions.
func mysqlVersionOK(v string) error {
	maj, min := versionParts(v)
	if strings.Contains(strings.ToLower(v), "mariadb") {
		if maj < 10 || (maj == 10 && min < 4) {
			return fmt.Errorf("MariaDB %s cannot lock accounts; Argus needs MariaDB 10.4 or later", v)
		}
		return nil
	}
	if maj < 5 || (maj == 5 && min < 7) {
		return fmt.Errorf("MySQL %s cannot lock accounts; Argus needs MySQL 5.7 or later", v)
	}
	return nil
}

func versionParts(v string) (int, int) {
	f := strings.SplitN(v, ".", 3)
	maj, _ := strconv.Atoi(f[0])
	min := 0
	if len(f) > 1 {
		min, _ = strconv.Atoi(f[1])
	}
	return maj, min
}

// readMySQLOptionFile reads user, password and socket from an option file's
// [client] group (or [mysql], [mysqladmin]), as the mysql client would. The
// file must be root's and private to it, like every secret the agent reads.
func readMySQLOptionFile(path string) (mysqlWay, error) {
	var w mysqlWay
	if !tunnel.ValidMySQLOptionFile(path) {
		return w, errors.New("the option file must be an absolute path to a .cnf file")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return w, fmt.Errorf("could not read %s: %w", path, err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return w, fmt.Errorf("%s can be read by others than its owner; chmod 600 it first", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return w, fmt.Errorf("could not read %s: %w", path, err)
	}
	defer f.Close()
	group := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			group = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		if group != "client" && group != "mysql" && group != "mysqladmin" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), "-", "_")
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		switch k {
		case "user":
			if w.user == "" {
				w.user = v
			}
		case "password":
			if w.pw == "" {
				w.pw = v
			}
		case "socket":
			if w.socket == "" && strings.HasPrefix(v, "/") {
				w.socket = v
			}
		}
	}
	if w.user == "" {
		return w, fmt.Errorf("%s has no user in its [client] group", path)
	}
	return w, sc.Err()
}

// mysqlSession is one open session.
type mysqlSession struct {
	f   *mysqlFactory
	g   tunnel.Grant
	db  tunnel.GrantDB
	id  string
	rec *stmtRecorder
	log *slog.Logger

	way     mysqlWay
	host    string
	user    string
	pw      string
	key     string
	version string
	maria   bool

	pool   *sql.DB
	conn   *sql.Conn
	connID int64

	timeout time.Duration

	cmu   sync.Mutex
	curID uint32
	// killed says the running statement was stopped by the agent: a cancel,
	// the row limit or the timeout.
	killed string
}

func (f *mysqlFactory) open(ctx context.Context, g tunnel.Grant, id string) (*mysqlSession, error) {
	db := *g.DB
	admin, err := adminConn(ctx, f.cfg.Sockets, db)
	if err != nil {
		return nil, err
	}
	defer admin.close()
	s := &mysqlSession{f: f, g: g, db: db, id: id, log: f.cfg.Log, way: admin.way, host: admin.host,
		version: admin.version, maria: admin.mariaDB(),
		user:    tunnel.MySQLUser(g.Principal, db.Level),
		timeout: ms(db.StatementTimeoutMS, defaultStmtTimeout)}
	s.key = strconv.Itoa(db.Port) + "\x00" + s.user + "\x00" + s.host

	var exists int
	if err := admin.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?",
		db.Name).Scan(&exists); err != nil {
		return nil, fmt.Errorf("MySQL did not answer: %w", err)
	}
	if exists == 0 {
		return nil, fmt.Errorf("MySQL on port %d has no database %s", db.Port, db.Name)
	}

	f.mu.Lock()
	err = s.giveUser(ctx, admin)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.connect(ctx); err != nil {
		s.takeUser(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

func (s *mysqlSession) who() string { return tunnel.MySQLString(s.user) + "@" + tunnel.MySQLString(s.host) }

// giveUser makes the person's user for this level if it is new, sets its
// privileges to exactly the level's, gives it this session's password and
// unlocks it. A second session of the same user shares the password the
// first one set: MySQL keeps one. Users Argus left unlocked -- an agent that
// stopped mid-session -- are locked first.
func (s *mysqlSession) giveUser(ctx context.Context, admin *mysqlAdmin) error {
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
	who := s.who()
	stmts := []string{
		"CREATE USER IF NOT EXISTS " + who + " IDENTIFIED BY " + tunnel.MySQLString(pw) + " ACCOUNT LOCK",
		"REVOKE ALL PRIVILEGES, GRANT OPTION FROM " + who,
	}
	stmts = append(stmts, tunnel.MySQLGrantSQL(s.user, s.host, s.db.Name, s.db.Level)...)
	stmts = append(stmts, "ALTER USER "+who+" IDENTIFIED BY "+tunnel.MySQLString(pw)+" ACCOUNT UNLOCK")
	for _, q := range stmts {
		if err := admin.exec(ctx, q); err != nil {
			s.f.uses.add(s.key, -1)
			return fmt.Errorf("could not prepare your MySQL user %s: %s", s.user, mysqlMessage(err))
		}
	}
	return nil
}

// sweep locks Argus's users with no live session on this agent, and ends
// their connections.
func (s *mysqlSession) sweep(ctx context.Context, admin *mysqlAdmin) {
	rows, err := admin.db.QueryContext(ctx,
		"SELECT User, Host FROM mysql.user WHERE User LIKE 'argus-%' LIMIT 2000")
	if err != nil {
		return
	}
	type acct struct{ u, h string }
	var stale []acct
	for rows.Next() {
		var a acct
		if rows.Scan(&a.u, &a.h) == nil && tunnel.IsStoreUser(a.u) &&
			!s.f.uses.live(strconv.Itoa(s.db.Port)+"\x00"+a.u+"\x00"+a.h) {
			stale = append(stale, a)
		}
	}
	_ = rows.Close()
	for _, a := range stale {
		pw, err := sessionPassword()
		if err != nil {
			return
		}
		_ = admin.exec(ctx, "ALTER USER "+tunnel.MySQLString(a.u)+"@"+tunnel.MySQLString(a.h)+
			" IDENTIFIED BY "+tunnel.MySQLString(pw)+" ACCOUNT LOCK")
		killUserConns(ctx, admin, a.u)
	}
}

// killUserConns ends every connection of a user.
func killUserConns(ctx context.Context, admin *mysqlAdmin, user string) {
	rows, err := admin.db.QueryContext(ctx, "SELECT ID FROM information_schema.PROCESSLIST WHERE USER = ?", user)
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
		_ = admin.exec(ctx, "KILL CONNECTION "+strconv.FormatInt(id, 10))
	}
}

// takeUser ends this session's hold on the user; the last one locks it with
// a password nobody knows and ends its connections.
func (s *mysqlSession) takeUser(ctx context.Context) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.uses.add(s.key, -1) > 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	admin, err := adminConn(cctx, s.f.cfg.Sockets, s.db)
	if err != nil {
		s.log.Error("mysql: could not lock a user after its session", "user", s.user, "error", err,
			"detail", "it is locked at the next session on this server")
		return
	}
	defer admin.close()
	pw, err := sessionPassword()
	if err == nil {
		err = admin.exec(cctx, "ALTER USER "+s.who()+" IDENTIFIED BY "+tunnel.MySQLString(pw)+" ACCOUNT LOCK")
	}
	if err != nil {
		s.log.Error("mysql: could not lock a user after its session", "user", s.user, "error", err)
	}
	killUserConns(cctx, admin, s.user)
}

// connect signs in as the person's user in the entry's database.
func (s *mysqlSession) connect(ctx context.Context) error {
	pool, err := sql.Open("mysql", s.way.dsn(s.user, s.pw, s.db.Name))
	if err != nil {
		return err
	}
	// One connection for the session, one more to stop what it runs.
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxLifetime(0)
	conn, err := pool.Conn(ctx)
	if err != nil {
		_ = pool.Close()
		return fmt.Errorf("could not sign in to MySQL as %s: %s", s.user, mysqlMessage(err))
	}
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&s.connID); err != nil {
		_ = conn.Close()
		_ = pool.Close()
		return fmt.Errorf("MySQL did not answer: %w", err)
	}
	// The server stops a long statement itself too, where it can: MySQL
	// only a SELECT, MariaDB any. The agent's own timer covers the rest.
	if s.maria {
		_, _ = conn.ExecContext(ctx, "SET SESSION max_statement_time = "+
			strconv.FormatFloat(s.timeout.Seconds(), 'f', 3, 64))
	} else {
		_, _ = conn.ExecContext(ctx, "SET SESSION max_execution_time = "+strconv.FormatInt(s.timeout.Milliseconds(), 10))
	}
	if s.db.Level == tunnel.LevelRead {
		_, _ = conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY")
	}
	s.pool, s.conn = pool, conn
	return nil
}

func (s *mysqlSession) close(ctx context.Context) {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.pool != nil {
		_ = s.pool.Close()
	}
	s.takeUser(ctx)
}

// kill stops the running statement from a second connection as the same
// user, which MySQL lets a user do to its own connections. The session's
// connection, and any transaction on it, carry on.
func (s *mysqlSession) kill(ctx context.Context, why string) {
	s.cmu.Lock()
	if s.killed == "" {
		s.killed = why
	}
	s.cmu.Unlock()
	kctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := s.pool.Conn(kctx)
	if err == nil {
		defer c.Close()
		_, err = c.ExecContext(kctx, "KILL QUERY "+strconv.FormatInt(s.connID, 10))
	}
	if err != nil {
		s.log.Warn("mysql: could not stop a statement", "session", s.id, "why", why, "error", err)
	}
}

func (s *mysqlSession) send(a tunnel.PGAnswer) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.rec.frame(tunnel.FramePGAnswer, b)
}

func (s *mysqlSession) serve(ctx context.Context, st tunnel.Stream) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	s.rec.attach(st)
	reason := "closed"
	defer func() {
		s.close(context.WithoutCancel(ctx))
		s.rec.close(reason)
		s.log.Info("tunnel: mysql session closed", "session", s.id, "user", s.user, "reason", reason)
	}()
	if s.rec.write(tunnel.PGRecord{Kind: "open", Actor: s.g.Email, Role: s.user,
		Level: s.db.Level, Grant: s.g.ID}) != nil {
		reason = "the recording failed"
		return
	}
	via := "tcp"
	if s.way.socket != "" {
		via = "socket"
	}
	if s.send(tunnel.PGAnswer{Type: "ready", Role: s.user, Level: s.db.Level, Version: s.version,
		Session: s.id, ReadOnly: s.db.Level == tunnel.LevelRead, Via: via}) != nil {
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
			default:
				// check and admin are Postgres's: the console treats this
				// answer as an agent without them and runs as before.
				err = s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "unknown operation " + req.Op +
					" for MySQL"})
			}
			if err != nil {
				reason = err.Error()
				return
			}
		}
	}
}

// read takes requests off the stream. A cancel acts at once.
func (s *mysqlSession) read(ctx context.Context, st tunnel.Stream, reqs chan<- tunnel.PGRequest) {
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
				go s.kill(ctx, "cancelled")
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

// MySQL errors the agent's own stops end with.
const (
	myQueryInterrupted = 1317 // KILL QUERY
	myMaxExecTime      = 3024 // max_execution_time
	myMariaStmtTime    = 1969 // max_statement_time
)

// run executes one statement. An error return ends the session: the
// recording, the stream or the connection failed. A statement's own failure
// is an answer.
func (s *mysqlSession) run(ctx context.Context, req tunnel.PGRequest) error {
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
	query, order, bad := mysqlPlaceholders(text, len(params))
	if bad != "" {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: bad})
	}
	args := make([]any, len(order))
	for i, n := range order {
		if params[n] == nil {
			args[i] = nil
		} else {
			args[i] = string(params[n])
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

	// Recorded and sent before MySQL sees it. No record, no statement.
	if err := s.rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: text, Params: req.Params, Approval: req.Approval}); err != nil {
		if errors.Is(err, tunnel.ErrRecordTooLong) {
			return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
		}
		return fmt.Errorf("the recording failed: %w", err)
	}

	s.cmu.Lock()
	s.curID, s.killed = req.ID, ""
	s.cmu.Unlock()
	defer func() {
		s.cmu.Lock()
		s.curID = 0
		s.cmu.Unlock()
	}()
	// The agent's own timeout: a KILL QUERY, which leaves the connection
	// and its transaction as they were. Cancelling the context instead would
	// close the connection and lose the person's transaction.
	timer := time.AfterFunc(s.timeout, func() { s.kill(ctx, "the statement ran longer than the timeout") })
	defer timer.Stop()

	start := time.Now()
	rows, err := s.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return s.failed(ctx, req.ID, err, 0, time.Since(start).Milliseconds())
	}
	cols, _ := rows.ColumnTypes()
	h := sha256.New()
	enc := tunnel.NewRowEncoder(req.ID)
	var (
		n         int64
		truncated bool
		sendErr   error
		whole     int
	)
	if len(cols) > 0 {
		sendErr = s.sendCols(req.ID, cols)
	}
	flush := func() {
		if enc.Rows() == 0 || sendErr != nil {
			return
		}
		h.Write(enc.Batch()[4:])
		sendErr = s.rec.frame(tunnel.FramePGRows, enc.Batch())
		enc.Reset()
	}
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	vals := make([][]byte, len(cols))
	for sendErr == nil && rows.Next() {
		if n >= int64(maxRows) {
			truncated = true
			s.kill(ctx, "the row limit")
			break
		}
		if err := rows.Scan(dest...); err != nil {
			break
		}
		for i, r := range raw {
			if r == nil {
				vals[i] = nil
			} else {
				vals[i] = r
			}
		}
		if !enc.Fits(vals) {
			flush()
		}
		kept := enc.Add(vals)
		if req.Whole && kept != nil && sendErr == nil {
			flush()
			for col, k := range kept {
				v := vals[col]
				if len(v) > tunnel.MaxWholeValue || whole+len(v)-k > tunnel.MaxWholeResult {
					continue
				}
				whole += len(v) - k
				h.Write(v[k:])
				for _, b := range tunnel.PartAnswers(req.ID, n, col, v, k) {
					if sendErr == nil {
						sendErr = s.rec.frame(tunnel.FramePGAnswer, b)
					}
				}
			}
		}
		n++
		if enc.Full() {
			flush()
		}
	}
	flush()
	err = rows.Err()
	// Further result sets (a CALL returns its own and a status) are read and
	// dropped: one request is one answer.
	for err == nil && rows.NextResultSet() {
		for rows.Next() {
		}
		err = rows.Err()
	}
	_ = rows.Close()
	elapsed := time.Since(start).Milliseconds()
	if sendErr != nil {
		return sendErr
	}
	s.cmu.Lock()
	killed := s.killed
	s.cmu.Unlock()
	if err != nil && truncated && mysqlErrNo(err) == myQueryInterrupted && killed == "the row limit" {
		err = nil
	}
	if err != nil {
		return s.failed(ctx, req.ID, err, n, elapsed)
	}
	verb := strings.ToUpper(class.Verb)
	tag := verb
	rowsOut := n
	if len(cols) > 0 {
		if verb == "" || !class.Writes {
			tag = "SELECT"
		}
		tag += " " + strconv.FormatInt(n, 10)
	} else {
		var affected int64
		if s.conn.QueryRowContext(ctx, "SELECT ROW_COUNT()").Scan(&affected) == nil && affected >= 0 {
			rowsOut = affected
			if class.Writes || verb == "REPLACE" {
				tag += " " + strconv.FormatInt(affected, 10)
			}
		}
		if tag == "" {
			tag = "OK"
		}
	}
	if err := s.rec.write(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: tag, Rows: rowsOut, MS: elapsed,
		ResultSHA256: hex.EncodeToString(h.Sum(nil)), Truncated: truncated}); err != nil {
		return err
	}
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "done", Tag: tag, Rows: rowsOut, MS: elapsed, Truncated: truncated})
}

func mysqlErrNo(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// mysqlMessage is MySQL's own message for an error, without the driver's
// "Error 1045 (28000):" in front.
func mysqlMessage(err error) string {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Message
	}
	return err.Error()
}

// failed records and answers a statement's failure. MySQL's own error is
// passed on as it is; a lost connection ends the session.
func (s *mysqlSession) failed(ctx context.Context, id uint32, err error, rows, elapsed int64) error {
	a := tunnel.PGAnswer{ID: id, Type: "error", Error: err.Error(), MS: elapsed}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		a.Error, a.Code, a.Detail = me.Message, string(me.SQLState[:]), "MySQL error "+strconv.Itoa(int(me.Number))
		s.cmu.Lock()
		killed := s.killed
		s.cmu.Unlock()
		switch me.Number {
		case myQueryInterrupted, myMaxExecTime, myMariaStmtTime:
			if killed == "" {
				killed = "the statement ran longer than the timeout"
			}
			a.Error = "stopped: " + killed
			a.Code = "57014"
		}
	} else if ctx.Err() == nil && s.conn.PingContext(context.WithoutCancel(ctx)) != nil {
		_ = s.rec.write(tunnel.PGRecord{Kind: "error", Request: id, Error: err.Error(), MS: elapsed})
		_ = s.send(a)
		return errors.New("the connection to MySQL ended")
	}
	if rerr := s.rec.write(tunnel.PGRecord{Kind: "error", Request: id, Code: a.Code,
		Error: a.Error, MS: elapsed, Rows: rows}); rerr != nil {
		return rerr
	}
	return s.send(a)
}

func (s *mysqlSession) sendCols(id uint32, cts []*sql.ColumnType) error {
	cols := make([]tunnel.PGColumn, len(cts))
	for i, c := range cts {
		cols[i] = tunnel.PGColumn{Name: c.Name(), Type: strings.ToLower(c.DatabaseTypeName())}
	}
	return s.send(tunnel.PGAnswer{ID: id, Type: "cols", Cols: cols})
}

// mysqlPlaceholders turns the console's $1, $2 ... into MySQL's ?, and says
// which value each ? takes: $2 used twice is two ? taking the same value.
// Quoted text, quoted names and comments are left alone.
func mysqlPlaceholders(s string, n int) (string, []int, string) {
	if n == 0 {
		return s, nil, ""
	}
	var b strings.Builder
	var order []int
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(s) {
				if c != '`' && s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c {
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
		case c == '#' || (c == '-' && i+2 < len(s) && s[i+1] == '-' && (s[i+2] == ' ' || s[i+2] == '\t')):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = len(s) - i
			}
			b.WriteString(s[i : i+j])
			i += j
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				j = len(s) - i - 2
			} else {
				j += 2
			}
			b.WriteString(s[i : i+2+j])
			i += 2 + j
		case c == '$' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j := i + 1
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			k, _ := strconv.Atoi(s[i+1 : j])
			if k < 1 || k > n {
				return "", nil, fmt.Sprintf("$%d has no value", k)
			}
			order = append(order, k-1)
			b.WriteByte('?')
			i = j
		case c == '?':
			return "", nil, "use :name for a value, not ?"
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), order, ""
}

// Object tree queries, as the person's user. information_schema shows a
// user only what it holds some privilege on.
const (
	mySchemasSQL = `SELECT SCHEMA_NAME,
	  (SELECT COUNT(*) FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA = s.SCHEMA_NAME AND t.TABLE_TYPE = 'BASE TABLE')
	FROM information_schema.SCHEMATA s WHERE s.SCHEMA_NAME = ?`
	myObjectsSQL = `SELECT TABLE_NAME, IF(TABLE_TYPE = 'VIEW', 'view', 'table'),
	  IFNULL(DATA_LENGTH + INDEX_LENGTH, 0), IFNULL(TABLE_ROWS, 0)
	FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?
	UNION ALL
	SELECT ROUTINE_NAME, LOWER(ROUTINE_TYPE), -1, 0
	FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?
	ORDER BY 2, 1 LIMIT 5000`
	myColumnsSQL = `SELECT COLUMN_NAME, 'column',
	  CONCAT(COLUMN_TYPE, IF(IS_NULLABLE = 'NO', ' not null', ''), IF(COLUMN_KEY = 'PRI', ' · primary key', '')),
	  ORDINAL_POSITION
	FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
	ORDER BY ORDINAL_POSITION LIMIT 2000`
	myCompletionSQL = `SELECT TABLE_NAME, IF(TABLE_TYPE = 'VIEW', 'view', 'table'), ''
	FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?
	UNION ALL
	SELECT ROUTINE_NAME, LOWER(ROUTINE_TYPE), ''
	FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?
	LIMIT 20000`
)

// schema answers the object tree and autocomplete. Not recorded: it reads
// the catalogue as the person's user and changes nothing. The tree shows the
// entry's database only: it is the one the user holds privileges on.
func (s *mysqlSession) schema(ctx context.Context, req tunnel.PGRequest) error {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	name := s.db.Name
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
				err = rows.Scan(&a, &d)
				out = append(out, tunnel.PGObject{Name: a, Kind: "schema", Count: d})
				continue
			case "completion":
				err = rows.Scan(&a, &b, &c)
				out = append(out, tunnel.PGObject{Name: a, Kind: b, Schema: name})
			case "columns":
				err = rows.Scan(&a, &b, &c, &d)
				out = append(out, tunnel.PGObject{Name: a, Kind: b, Detail: c})
			default:
				var size int64
				err = rows.Scan(&a, &b, &size, &d)
				o := tunnel.PGObject{Name: a, Kind: b, Count: d}
				if size >= 0 && b == "table" {
					o.Detail = prettyBytes(size)
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
		err = query(mySchemasSQL, name)
	case "objects":
		err = query(myObjectsSQL, name, name)
	case "columns":
		err = query(myColumnsSQL, name, req.Object)
	case "completion":
		out = append(out, tunnel.PGObject{Schema: name, Name: name, Kind: "path", Detail: "1"},
			tunnel.PGObject{Schema: name, Name: name, Kind: "schema"})
		err = query(myCompletionSQL, name, name)
	default:
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "unknown schema request " + req.Kind})
	}
	if err != nil {
		if s.conn.PingContext(context.WithoutCancel(ctx)) != nil {
			return errors.New("the connection to MySQL ended")
		}
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mysqlMessage(err)})
	}
	return s.sendObjects(req.ID, out)
}

// sendObjects answers a schema request in parts under one frame each, as
// the Postgres session does.
func (s *mysqlSession) sendObjects(id uint32, objs []tunnel.PGObject) error {
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

// prettyBytes is a size as Postgres's pg_size_pretty writes it.
func prettyBytes(n int64) string {
	units := []string{"bytes", "kB", "MB", "GB", "TB"}
	v, u := n, 0
	for u < len(units)-1 && v >= 10*1024 {
		v = (v + 512) / 1024
		u++
	}
	return strconv.FormatInt(v, 10) + " " + units[u]
}

// discoverMySQL lists the databases of a MySQL on this server, with their
// size, leaving out MySQL's own.
func discoverMySQL(ctx context.Context, sockets []string, d tunnel.GrantDiscover) tunnel.DiscoverResult {
	admin, err := adminConn(ctx, sockets, tunnel.GrantDB{Host: d.Host, Port: d.Port, ConfigPath: d.ConfigPath})
	if err != nil {
		return tunnel.DiscoverResult{Error: err.Error()}
	}
	defer admin.close()
	rows, err := admin.db.QueryContext(ctx, `SELECT s.SCHEMA_NAME,
	  IFNULL((SELECT SUM(t.DATA_LENGTH + t.INDEX_LENGTH) FROM information_schema.TABLES t
	          WHERE t.TABLE_SCHEMA = s.SCHEMA_NAME), 0)
	FROM information_schema.SCHEMATA s ORDER BY s.SCHEMA_NAME`)
	if err != nil {
		return tunnel.DiscoverResult{Error: "MySQL did not answer: " + mysqlMessage(err), Version: admin.version}
	}
	defer rows.Close()
	res := tunnel.DiscoverResult{OK: true, Version: admin.version}
	for rows.Next() {
		var name string
		var size int64
		if rows.Scan(&name, &size) != nil || tunnel.IsMySQLSystemDB(name) {
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

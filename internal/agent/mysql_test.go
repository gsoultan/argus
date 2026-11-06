//go:build !windows

package agent

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Against a real MySQL or MariaDB, when ARGUS_TEST_MYSQL_SOCKET names the
socket of one whose root signs in over it with no password (unix_socket or
auth_socket, as Debian, Ubuntu and MariaDB 10.4+ set root up), for example

	ARGUS_TEST_MYSQL_SOCKET=/run/mysqld/mysqld.sock ARGUS_TEST_MYSQL_PORT=3306

and the test runs as the operating system's root. The test makes its own
databases (argus_t_*) and drops them after; nothing else sets MySQL up.
*/

func myTest(t *testing.T) (sock string, port int) {
	t.Helper()
	sock = os.Getenv("ARGUS_TEST_MYSQL_SOCKET")
	if sock == "" {
		t.Skip("ARGUS_TEST_MYSQL_SOCKET is not set")
	}
	port = 3306
	if p := os.Getenv("ARGUS_TEST_MYSQL_PORT"); p != "" {
		port, _ = strconv.Atoi(p)
	}
	return sock, port
}

// myRoot is the test's own administrator connection, for setting up and
// for checking what the agent left behind.
func myRoot(t *testing.T, sock string) *sql.DB {
	t.Helper()
	w := mysqlWay{socket: sock, user: "root"}
	db, err := sql.Open("mysql", w.dsn("root", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func myExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// myDB makes a scratch database with a table of three rows.
func myDB(t *testing.T, root *sql.DB, name string) {
	t.Helper()
	myExec(t, root, "DROP DATABASE IF EXISTS "+tunnel.MySQLIdent(name), "CREATE DATABASE "+tunnel.MySQLIdent(name),
		"CREATE TABLE "+tunnel.MySQLIdent(name)+".items (id INT PRIMARY KEY, name VARCHAR(40), note TEXT)",
		"INSERT INTO "+tunnel.MySQLIdent(name)+".items VALUES (1,'one',NULL),(2,'two','b'),(3,'three','c')")
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + tunnel.MySQLIdent(name)) })
}

type myService struct {
	svc  TunnelService
	dir  string
	sock string
	port int
}

func newMyService(t *testing.T) myService {
	sock, port := myTest(t)
	dir := t.TempDir()
	svc := MySQL(MySQLConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"}, RecordDir: dir},
		Sockets: []string{sock}})
	return myService{svc: svc, dir: dir, sock: sock, port: port}
}

func (m myService) open(t *testing.T, role, dbname, level string, extra func(*tunnel.GrantDB)) (*pgHarness, error) {
	t.Helper()
	key, _ := tunnel.GrantKeyFromSecret(strings.Repeat("s", 32))
	pub := key.Public().(ed25519.PublicKey)
	g := tunnel.NewGrant("ana.sari@corp.example", "db-01", role, "")
	g.Service = tunnel.MySQLService
	g.DB = &tunnel.GrantDB{ID: "m1", Engine: tunnel.EngineMySQL, Name: dbname, Host: "127.0.0.1", Port: m.port, Level: level}
	if extra != nil {
		extra(g.DB)
	}
	tok, err := tunnel.SignGrant(key, g)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	closeOnce := sync.OnceFunc(func() { cancel(); _ = a.Close(); _ = b.Close(); <-done })
	t.Cleanup(closeOnce)
	go func() {
		defer close(done)
		m.svc(ctx, &Call{Header: tunnel.Header{Service: tunnel.MySQLService, Grant: tok}, Stream: pipeStream{a}, GrantKey: pub})
	}()
	h := &pgHarness{t: t, stream: pipeStream{b}, fr: tunnel.NewFrameReader(b), dir: m.dir, done: done, close: closeOnce}
	var ans tunnel.Answer
	if err := tunnel.ReadJSON(b, &ans); err != nil {
		t.Fatal(err)
	}
	if !ans.OK {
		closeOnce()
		return nil, &refusal{ans.Error}
	}
	if h.ready = h.answer(); h.ready.Type != "ready" || h.ready.Level != level {
		t.Fatalf("expected ready at %s, got %+v", level, h.ready)
	}
	return h, nil
}

func (m myService) mustOpen(t *testing.T, dbname, level string) *pgHarness {
	t.Helper()
	h, err := m.open(t, "ana_sari", dbname, level, nil)
	if err != nil {
		t.Fatalf("mysql session refused: %v", err)
	}
	return h
}

// collect reads the frames of a request already sent, to its final answer.
func (h *pgHarness) collect(id uint32) (cols []tunnel.PGColumn, rows [][]*string, final tunnel.PGAnswer) {
	for {
		typ, p, err := h.fr.Next()
		if err != nil {
			h.t.Fatal(err)
		}
		switch typ {
		case tunnel.FramePGRecord:
			var r tunnel.PGRecord
			_ = json.Unmarshal(p, &r)
			h.rec = append(h.rec, r)
		case tunnel.FramePGRows:
			_, batch, _ := tunnel.DecodeRows(p, len(cols))
			rows = append(rows, batch...)
		case tunnel.FramePGAnswer:
			var a tunnel.PGAnswer
			_ = json.Unmarshal(p, &a)
			if a.ID != id {
				continue
			}
			if a.Type == "cols" {
				cols = a.Cols
				continue
			}
			return cols, rows, a
		}
	}
}

func (h *pgHarness) stmtSeen(id uint32) bool {
	for _, r := range h.rec {
		if r.Kind == "stmt" && r.Request == id {
			return true
		}
	}
	return false
}

func cell(rows [][]*string, r, c int) string {
	if r >= len(rows) || c >= len(rows[r]) || rows[r][c] == nil {
		return "<null>"
	}
	return *rows[r][c]
}

func TestMySQLLevelsAreGrants(t *testing.T) {
	m := newMyService(t)
	root := myRoot(t, m.sock)
	myDB(t, root, "argus_t_levels")

	read := m.mustOpen(t, "argus_t_levels", tunnel.LevelRead)
	if read.ready.Role != "argus-ana_sari-read" || read.ready.Version == "" {
		t.Fatalf("ready: %+v", read.ready)
	}
	cols, rows, a := read.run(1, "SELECT id, name, note FROM items ORDER BY id", 0)
	if a.Type != "done" || a.Tag != "SELECT 3" || len(rows) != 3 || len(cols) != 3 {
		t.Fatalf("select: %+v %v", a, rows)
	}
	if cell(rows, 0, 2) != "<null>" || cell(rows, 2, 1) != "three" {
		t.Fatalf("values: %q %q", cell(rows, 0, 2), cell(rows, 2, 1))
	}
	if _, _, a := read.run(2, "INSERT INTO items VALUES (4,'four',NULL)", 0); a.Type != "error" {
		t.Fatalf("read inserted: %+v", a)
	}
	read.close()

	write := m.mustOpen(t, "argus_t_levels", tunnel.LevelWrite)
	if _, _, a := write.run(1, "UPDATE items SET note = 'x' WHERE id < 3", 0); a.Type != "done" || a.Tag != "UPDATE 2" || a.Rows != 2 {
		t.Fatalf("update: %+v", a)
	}
	if _, _, a := write.run(2, "CREATE TABLE t2 (id INT)", 0); a.Type != "error" || !strings.Contains(a.Error, "denied") {
		t.Fatalf("write created a table: %+v", a)
	}
	// What MySQL refuses, the recording shows as asked and refused.
	if !recordedAs(write.rec, "CREATE TABLE t2 (id INT)", "error") {
		t.Fatalf("the refused statement is not recorded: %+v", write.rec)
	}
	write.close()

	owner := m.mustOpen(t, "argus_t_levels", tunnel.LevelOwner)
	if _, _, a := owner.run(1, "CREATE TABLE t2 (id INT)", 0); a.Type != "done" {
		t.Fatalf("owner create: %+v", a)
	}
	if _, _, a := owner.run(2, "SHOW FULL PROCESSLIST", 0); a.Type != "done" {
		t.Fatalf("processlist: %+v", a)
	}
	owner.close()

	// Nothing on any other database, at any level.
	myDB(t, root, "argus_t_other")
	dba := m.mustOpen(t, "argus_t_levels", tunnel.LevelDBA)
	if _, _, a := dba.run(1, "SELECT * FROM argus_t_other.items", 0); a.Type != "error" {
		t.Fatalf("dba read another database: %+v", a)
	}
	if _, rows, a := dba.run(2, "SELECT COUNT(*) FROM information_schema.PROCESSLIST", 0); a.Type != "done" || cell(rows, 0, 0) == "1" {
		t.Fatalf("dba sees only itself in the process list: %+v %v", a, rows)
	}
	dba.close()

	// Closed: every user is locked again and has no connection left.
	for _, lvl := range []string{"read", "write", "owner", "dba"} {
		u := tunnel.MySQLUser("ana_sari", lvl)
		var n int
		if err := root.QueryRow("SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER = ?", u).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s still has %d connections (%v)", u, n, err)
		}
		if locked := myLocked(t, root, u); !locked {
			t.Fatalf("%s is not locked after its session", u)
		}
	}
}

// myLocked reads whether an account is locked, on MySQL or MariaDB.
func myLocked(t *testing.T, root *sql.DB, user string) bool {
	t.Helper()
	var create string
	var who string
	if err := root.QueryRow("SELECT CONCAT(QUOTE(User), '@', QUOTE(Host)) FROM mysql.user WHERE User = ?", user).Scan(&who); err != nil {
		t.Fatalf("%s: %v", user, err)
	}
	if err := root.QueryRow("SHOW CREATE USER " + who).Scan(&create); err != nil {
		t.Fatal(err)
	}
	return strings.Contains(create, "ACCOUNT LOCK")
}

func recordedAs(rec []tunnel.PGRecord, sqlText, then string) bool {
	for i, r := range rec {
		if r.Kind == "stmt" && r.SQL == sqlText {
			for _, n := range rec[i+1:] {
				if n.Request == r.Request && n.Kind != "stmt" {
					return n.Kind == then
				}
			}
		}
	}
	return false
}

func TestMySQLRecordingAndGuard(t *testing.T) {
	m := newMyService(t)
	root := myRoot(t, m.sock)
	myDB(t, root, "argus_t_rec")
	h := m.mustOpen(t, "argus_t_rec", tunnel.LevelWrite)

	_, _, a := h.run(1, "DELETE FROM items", 0)
	if a.Type != "error" || a.Guard == nil || a.Code != tunnel.GuardNoWhere {
		t.Fatalf("a DELETE of every row was not stopped: %+v", a)
	}
	var n int
	_ = root.QueryRow("SELECT COUNT(*) FROM argus_t_rec.items").Scan(&n)
	if n != 3 {
		t.Fatalf("the guarded DELETE ran: %d rows left", n)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 2, Op: "run", SQL: "DELETE FROM `items`", Confirm: a.Guard.Confirm})
	if a.Type != "done" || a.Tag != "DELETE 3" {
		t.Fatalf("the confirmed DELETE: %+v", a)
	}
	// Values from the console's :name, as $n.
	_, rows, a := h.do(tunnel.PGRequest{ID: 3, Op: "run", SQL: "SELECT $1 AS a, '$1' AS b, $2 AS c, $1 AS d",
		Params: []*string{ptr("it's"), nil}})
	if a.Type != "done" || cell(rows, 0, 0) != "it's" || cell(rows, 0, 1) != "$1" || cell(rows, 0, 2) != "<null>" || cell(rows, 0, 3) != "it's" {
		t.Fatalf("params: %+v %v", a, rows)
	}
	h.close()

	// The recording verifies, and holds every statement before its answer.
	b, err := os.ReadFile(m.dir + "/" + recordingName(m.dir, t))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := tunnel.VerifyPGRecording(strings.NewReader(string(b)), ""); err != nil || !v.OK {
		t.Fatalf("the recording does not verify: %+v %v", v, err)
	}
	if !recordedAs(h.rec, "DELETE FROM items", "error") || !recordedAs(h.rec, "DELETE FROM `items`", "done") {
		t.Fatalf("recording: %+v", h.rec)
	}
}

func ptr(s string) *string { return &s }

func TestMySQLCancelKeepsTheTransaction(t *testing.T) {
	m := newMyService(t)
	root := myRoot(t, m.sock)
	myDB(t, root, "argus_t_cancel")
	h := m.mustOpen(t, "argus_t_cancel", tunnel.LevelWrite)
	if _, _, a := h.run(1, "BEGIN", 0); a.Type != "done" {
		t.Fatalf("begin: %+v", a)
	}
	if _, _, a := h.run(2, "INSERT INTO items VALUES (9,'nine',NULL)", 0); a.Type != "done" || a.Tag != "INSERT 1" {
		t.Fatalf("insert: %+v", a)
	}
	h.send(tunnel.PGRequest{ID: 3, Op: "run", SQL: "SELECT SLEEP(20)"})
	// The pipe holds the agent until its recording line is read.
	for !recordedAs(h.rec, "SELECT SLEEP(20)", "") && !h.stmtSeen(3) {
		typ, p, err := h.fr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if typ == tunnel.FramePGRecord {
			var r tunnel.PGRecord
			_ = json.Unmarshal(p, &r)
			h.rec = append(h.rec, r)
		}
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	h.send(tunnel.PGRequest{ID: 3, Op: "cancel"})
	_, _, a := h.collect(3)
	// MySQL answers a killed SLEEP with 1 rather than an error.
	if a.Type != "done" && a.Type != "error" {
		t.Fatalf("cancel: %+v", a)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the cancel took %s", time.Since(start))
	}
	if _, rows, a := h.run(4, "SELECT COUNT(*) FROM items", 0); a.Type != "done" || cell(rows, 0, 0) != "4" {
		t.Fatalf("the transaction did not survive the cancel: %+v %v", a, rows)
	}
	h.close()
	var n int
	_ = root.QueryRow("SELECT COUNT(*) FROM argus_t_cancel.items").Scan(&n)
	if n != 3 {
		t.Fatalf("an uncommitted insert was kept when the session closed: %d rows", n)
	}
}

func TestMySQLRowLimitAndSchema(t *testing.T) {
	m := newMyService(t)
	root := myRoot(t, m.sock)
	myDB(t, root, "argus_t_rows")
	h := m.mustOpen(t, "argus_t_rows", tunnel.LevelRead)
	_, rows, a := h.run(1, "SELECT a.id FROM items a, items b, items c, items d", 5)
	if a.Type != "done" || !a.Truncated || len(rows) != 5 {
		t.Fatalf("row limit: %+v (%d rows)", a, len(rows))
	}
	// The session goes on after the limit stopped a statement.
	if _, _, a := h.run(2, "SELECT 1", 0); a.Type != "done" {
		t.Fatalf("after the limit: %+v", a)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 3, Op: "schema", Kind: "objects", Schema: "argus_t_rows"})
	if a.Type != "schema" || len(a.Objects) != 1 || a.Objects[0].Name != "items" || a.Objects[0].Kind != "table" {
		t.Fatalf("objects: %+v", a)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 4, Op: "schema", Kind: "columns", Schema: "argus_t_rows", Object: "items"})
	if a.Type != "schema" || len(a.Objects) != 3 || !strings.Contains(a.Objects[0].Detail, "primary key") {
		t.Fatalf("columns: %+v", a)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 5, Op: "check", SQL: "UPDATE items SET name = 'x' WHERE id = 1"})
	if a.Type != "error" || !strings.Contains(a.Error, "unknown operation") {
		t.Fatalf("check, which the console reads as not offered: %+v", a)
	}
}

// A database whose name holds _ grants on that database only: in GRANT, _
// is a wildcard unless escaped, and app_db would also cover appXdb.
func TestMySQLGrantIsNotAWildcard(t *testing.T) {
	m := newMyService(t)
	root := myRoot(t, m.sock)
	myDB(t, root, "argus_t_w_b")
	myDB(t, root, "argus_t_wxb")
	h := m.mustOpen(t, "argus_t_w_b", tunnel.LevelRead)
	if _, _, a := h.run(1, "SELECT * FROM argus_t_wxb.items", 0); a.Type != "error" {
		t.Fatalf("the grant on argus_t_w_b reached argus_t_wxb: %+v", a)
	}
}

// Users that are not Argus's are never touched.
func TestMySQLLeavesOtherUsersAlone(t *testing.T) {
	m := newMyService(t)
	root := myRoot(t, m.sock)
	myDB(t, root, "argus_t_app")
	myExec(t, root, "DROP USER IF EXISTS 'app'@'localhost'",
		"CREATE USER 'app'@'localhost' IDENTIFIED BY 'app-pw-1'",
		"GRANT SELECT, INSERT ON argus_t_app.* TO 'app'@'localhost'")
	t.Cleanup(func() { _, _ = root.Exec("DROP USER IF EXISTS 'app'@'localhost'") })
	before := myGrants(t, root, "'app'@'localhost'")
	for _, lvl := range []string{"read", "dba"} {
		h := m.mustOpen(t, "argus_t_app", lvl)
		h.close()
	}
	if after := myGrants(t, root, "'app'@'localhost'"); after != before {
		t.Fatalf("the application's grants changed:\n%s\n%s", before, after)
	}
	if myLocked(t, root, "app") {
		t.Fatal("the application's user was locked")
	}
}

func myGrants(t *testing.T, root *sql.DB, who string) string {
	t.Helper()
	rows, err := root.Query("SHOW GRANTS FOR " + who)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		_ = rows.Scan(&g)
		out = append(out, g)
	}
	return strings.Join(out, "\n")
}

func TestMySQLRefusals(t *testing.T) {
	m := newMyService(t)
	if _, err := m.open(t, "ana_sari", "argus_t_none", tunnel.LevelRead, nil); err == nil ||
		!strings.Contains(err.Error(), "no database") {
		t.Fatalf("a database that is not there: %v", err)
	}
	if _, err := m.open(t, "ana_sari", "mysql", tunnel.LevelRead, nil); err == nil {
		t.Fatal("MySQL's own schema opened")
	}
	if _, err := m.open(t, "ana_sari", "argus_t_x", tunnel.LevelRead, func(d *tunnel.GrantDB) { d.Host = "10.0.0.5" }); err == nil {
		t.Fatal("a remote host opened")
	}
	if _, err := m.open(t, "pg_root", "argus_t_x", tunnel.LevelRead, nil); err == nil ||
		!strings.Contains(err.Error(), "not a user") {
		t.Fatalf("a session opened as a reserved name: %v", err)
	}
}

func TestMySQLPlaceholders(t *testing.T) {
	for _, c := range []struct {
		in, out string
		n       int
		order   []int
		bad     bool
	}{
		{"SELECT 1", "SELECT 1", 0, nil, false},
		{"SELECT $1, $2, $1", "SELECT ?, ?, ?", 2, []int{0, 1, 0}, false},
		{"SELECT '$1', \"$2\", `$1`, $1 -- $2\n", "SELECT '$1', \"$2\", `$1`, ? -- $2\n", 2, []int{0}, false},
		{"SELECT 'it\\'s $1', $1 # $2", "SELECT 'it\\'s $1', ? # $2", 1, []int{0}, false},
		{"SELECT /* $1 */ $1", "SELECT /* $1 */ ?", 1, []int{0}, false},
		{"SELECT $3", "", 2, nil, true},
		{"SELECT ?", "", 1, nil, true},
	} {
		got, order, bad := mysqlPlaceholders(c.in, c.n)
		if (bad != "") != c.bad || (!c.bad && (got != c.out || !sameInts(order, c.order))) {
			t.Errorf("%q: got %q %v %q", c.in, got, order, bad)
		}
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMySQLOptionFile(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/argus.cnf"
	if err := os.WriteFile(p, []byte("[mysqld]\nuser = mysql\n[client]\nuser = \"argus_admin\"\npassword = s3 cret\nsocket = /run/x.sock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := readMySQLOptionFile(p)
	if err != nil || w.user != "argus_admin" || w.pw != "s3 cret" || w.socket != "/run/x.sock" {
		t.Fatalf("%+v %v", w, err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readMySQLOptionFile(p); err == nil {
		t.Fatal("a file others can read was used")
	}
}

func TestMySQLVersions(t *testing.T) {
	for v, ok := range map[string]bool{"8.0.36": true, "5.7.44-log": true, "5.6.51": false,
		"10.11.14-MariaDB-0ubuntu0.24.04.1": true, "10.3.39-MariaDB": false, "11.4.2-MariaDB": true} {
		if (mysqlVersionOK(v) == nil) != ok {
			t.Errorf("%s: %v", v, mysqlVersionOK(v))
		}
	}
}

// With an option file and no socket, the agent signs in over TCP, and the
// person's user is made for the address MySQL sees, then reached the same way.
func TestMySQLOptionFileOverTCP(t *testing.T) {
	sock, port := myTest(t)
	root := myRoot(t, sock)
	myDB(t, root, "argus_t_tcp")
	for _, host := range []string{"localhost", "127.0.0.1"} {
		myExec(t, root, "DROP USER IF EXISTS 'argus_admin_t'@'"+host+"'",
			"CREATE USER 'argus_admin_t'@'"+host+"' IDENTIFIED BY 'adm-Pw-77'",
			"GRANT ALL PRIVILEGES ON *.* TO 'argus_admin_t'@'"+host+"' WITH GRANT OPTION")
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP USER IF EXISTS 'argus_admin_t'@'localhost'")
		_, _ = root.Exec("DROP USER IF EXISTS 'argus_admin_t'@'127.0.0.1'")
	})
	dir := t.TempDir()
	cnf := dir + "/argus.cnf"
	if err := os.WriteFile(cnf, []byte("[client]\nuser=argus_admin_t\npassword=adm-Pw-77\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := myService{svc: MySQL(MySQLConfig{StoreConfig: StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"},
		RecordDir: dir}, Sockets: []string{}}), dir: dir, sock: sock, port: port}
	h, err := m.open(t, "tcp_person", "argus_t_tcp", tunnel.LevelRead, func(d *tunnel.GrantDB) { d.ConfigPath = cnf })
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if h.ready.Via != "tcp" {
		t.Fatalf("ready: %+v", h.ready)
	}
	if _, rows, a := h.run(1, "SELECT COUNT(*) FROM items", 0); a.Type != "done" || cell(rows, 0, 0) != "3" {
		t.Fatalf("select: %+v %v", a, rows)
	}
	h.close()
}

func TestMySQLDiscover(t *testing.T) {
	sock, port := myTest(t)
	root := myRoot(t, sock)
	myDB(t, root, "argus_t_found")
	res := discoverMySQL(context.Background(), []string{sock}, tunnel.GrantDiscover{Engine: tunnel.EngineMySQL,
		Host: "127.0.0.1", Port: port})
	if !res.OK || res.Version == "" {
		t.Fatalf("%+v", res)
	}
	found := false
	for _, d := range res.Databases {
		if tunnel.IsMySQLSystemDB(d.Name) {
			t.Fatalf("listed MySQL's own %s", d.Name)
		}
		found = found || (d.Name == "argus_t_found" && d.Bytes > 0)
	}
	if !found {
		t.Fatalf("argus_t_found is not listed with its size: %+v", res.Databases)
	}
	// A socket that belongs to another port is not taken for this one.
	res = discoverMySQL(context.Background(), []string{sock}, tunnel.GrantDiscover{Engine: tunnel.EngineMySQL,
		Host: "127.0.0.1", Port: port + 1000})
	if res.OK {
		t.Fatalf("the socket of port %d answered for port %d", port, port+1000)
	}
}

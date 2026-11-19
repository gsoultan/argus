//go:build !windows

package agent

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Against a real SQL Server, when ARGUS_TEST_MSSQL_SA_PASSWORD holds the sa
password of one listening on 127.0.0.1 (ARGUS_TEST_MSSQL_PORT, 1433 by
default), for example the container

	docker run -d -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD='Argus!Test123' -p 1433:1433 \
	  mcr.microsoft.com/mssql/server:2022-latest
	ARGUS_TEST_MSSQL_SA_PASSWORD='Argus!Test123' go test ./internal/agent -run MSSQL

The test makes its own databases (argus_t_*) and drops them after; nothing
else sets SQL Server up.
*/

func msTest(t *testing.T) (pw string, port int) {
	t.Helper()
	pw = os.Getenv("ARGUS_TEST_MSSQL_SA_PASSWORD")
	if pw == "" {
		t.Skip("ARGUS_TEST_MSSQL_SA_PASSWORD is not set")
	}
	port = 1433
	if p := os.Getenv("ARGUS_TEST_MSSQL_PORT"); p != "" {
		port, _ = strconv.Atoi(p)
	}
	return pw, port
}

// msRoot is the test's own administrator connection.
func msRoot(t *testing.T, pw string, port int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlserver", mssqlDSN("127.0.0.1", port, "sa", pw, "master"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func msExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// msDB makes a scratch database with a table of three rows.
func msDB(t *testing.T, root *sql.DB, name string) {
	t.Helper()
	D := tunnel.MSSQLIdent(name)
	drop := "IF DB_ID(" + tunnel.MSSQLString(name) + ") IS NOT NULL BEGIN ALTER DATABASE " + D +
		" SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE " + D + " END"
	msExec(t, root, drop, "CREATE DATABASE "+D,
		"CREATE TABLE "+D+".dbo.items (id INT PRIMARY KEY, name NVARCHAR(40), note NVARCHAR(MAX), "+
			"amount DECIMAL(10,2), seen DATETIME2(3), flag BIT, guid UNIQUEIDENTIFIER, raw VARBINARY(8))",
		"INSERT INTO "+D+".dbo.items VALUES (1, N'one', NULL, 1.50, '2026-01-02 03:04:05.678', 1, "+
			"'6F9619FF-8B86-D011-B42D-00C04FC964FF', 0xCAFE), (2, N'two', N'b', 2, NULL, 0, NULL, NULL), "+
			"(3, N'three', N'c', 3, NULL, NULL, NULL, NULL)")
	t.Cleanup(func() { _, _ = root.Exec(drop) })
}

type msService struct {
	svc  TunnelService
	dir  string
	port int
	file string
}

func newMSService(t *testing.T) msService {
	pw, port := msTest(t)
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "mssql.conf")
	if err := os.WriteFile(file, []byte("# sa for the tests\nuser = sa\npassword = "+pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := MSSQL(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"}, RecordDir: dir})
	return msService{svc: svc, dir: dir, port: port, file: file}
}

func (m msService) open(t *testing.T, role, dbname, level string, extra func(*tunnel.GrantDB)) (*pgHarness, error) {
	t.Helper()
	key, _ := tunnel.GrantKeyFromSecret(strings.Repeat("s", 32))
	pub := key.Public().(ed25519.PublicKey)
	g := tunnel.NewGrant("ana.sari@corp.example", "db-01", role, "")
	g.Service = tunnel.MSSQLService
	g.DB = &tunnel.GrantDB{ID: "s1", Engine: tunnel.EngineMSSQL, Name: dbname, Host: "127.0.0.1", Port: m.port,
		Level: level, ConfigPath: m.file}
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
		m.svc(ctx, &Call{Header: tunnel.Header{Service: tunnel.MSSQLService, Grant: tok}, Stream: pipeStream{a}, GrantKey: pub})
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

func (m msService) mustOpen(t *testing.T, dbname, level string) *pgHarness {
	t.Helper()
	h, err := m.open(t, "ana_sari", dbname, level, nil)
	if err != nil {
		t.Fatalf("sql server session refused: %v", err)
	}
	return h
}

// msLogin reads a login's state: whether it exists and is disabled, and how
// many connections it has.
func msLogin(t *testing.T, root *sql.DB, login string) (exists, disabled bool, conns int) {
	t.Helper()
	var d sql.NullBool
	err := root.QueryRow("SELECT is_disabled FROM sys.sql_logins WHERE name = @p1", login).Scan(&d)
	if err == sql.ErrNoRows {
		return false, false, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := root.QueryRow("SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE login_name = @p1", login).Scan(&conns); err != nil {
		t.Fatal(err)
	}
	return true, d.Bool, conns
}

func TestMSSQLLevelsAreRoles(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_levels")

	read := m.mustOpen(t, "argus_t_levels", tunnel.LevelRead)
	if read.ready.Role != tunnel.MSSQLLogin("ana_sari", "read", "argus_t_levels") || read.ready.Version == "" {
		t.Fatalf("ready: %+v", read.ready)
	}
	cols, rows, a := read.run(1, "SELECT id, name, note, amount, seen, flag, guid, raw FROM items ORDER BY id", 0)
	if a.Type != "done" || a.Tag != "SELECT 3" || len(rows) != 3 || len(cols) != 8 {
		t.Fatalf("select: %+v %v", a, rows)
	}
	want := []string{"1", "one", "<null>", "1.50", "2026-01-02 03:04:05.678", "1", "6F9619FF-8B86-D011-B42D-00C04FC964FF", "0xCAFE"}
	for i, w := range want {
		if got := cell(rows, 0, i); got != w {
			t.Errorf("column %s: %q, want %q", cols[i].Name, got, w)
		}
	}
	if cols[1].Type != "nvarchar" {
		t.Fatalf("types: %+v", cols)
	}
	if _, _, a := read.run(2, "INSERT INTO items (id, name) VALUES (4, N'four')", 0); a.Type != "error" ||
		!strings.Contains(a.Error, "permission was denied") {
		t.Fatalf("read inserted: %+v", a)
	}
	read.close()

	write := m.mustOpen(t, "argus_t_levels", tunnel.LevelWrite)
	if _, _, a := write.run(1, "UPDATE items SET note = N'x' WHERE id < 3", 0); a.Type != "done" || a.Tag != "UPDATE 2" || a.Rows != 2 {
		t.Fatalf("update: %+v", a)
	}
	if _, _, a := write.run(2, "CREATE TABLE t2 (id INT)", 0); a.Type != "error" || !strings.Contains(a.Error, "permission denied") {
		t.Fatalf("write created a table: %+v", a)
	}
	if !recordedAs(write.rec, "CREATE TABLE t2 (id INT)", "error") {
		t.Fatalf("the refused statement is not recorded: %+v", write.rec)
	}
	write.close()

	owner := m.mustOpen(t, "argus_t_levels", tunnel.LevelOwner)
	if _, _, a := owner.run(1, "CREATE TABLE t2 (id INT)", 0); a.Type != "done" {
		t.Fatalf("owner create: %+v", a)
	}
	if _, _, a := owner.run(2, "CREATE USER someone WITHOUT LOGIN", 0); a.Type != "error" {
		t.Fatalf("owner made a user: %+v", a)
	}
	owner.close()

	// Nothing on any other database, at any level.
	msDB(t, root, "argus_t_other")
	dba := m.mustOpen(t, "argus_t_levels", tunnel.LevelDBA)
	if _, _, a := dba.run(1, "SELECT * FROM argus_t_other.dbo.items", 0); a.Type != "error" {
		t.Fatalf("dba read another database: %+v", a)
	}
	if _, rows, a := dba.run(2, "SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE is_user_process = 1", 0); a.Type != "done" || cell(rows, 0, 0) == "1" {
		t.Fatalf("dba sees only itself in the sessions: %+v %v", a, rows)
	}
	if _, rows, a := dba.run(3, "SELECT IS_SRVROLEMEMBER('sysadmin')", 0); a.Type != "done" || cell(rows, 0, 0) != "0" {
		t.Fatalf("dba is sysadmin: %+v %v", a, rows)
	}
	dba.close()

	// Closed: every login is disabled again and has no connection left.
	for _, lvl := range []string{"read", "write", "owner", "dba"} {
		l := tunnel.MSSQLLogin("ana_sari", lvl, "argus_t_levels")
		exists, disabled, conns := msLogin(t, root, l)
		if !exists || !disabled || conns != 0 {
			t.Fatalf("%s: exists %v, disabled %v, %d connections", l, exists, disabled, conns)
		}
	}
}

// A level is exactly the level: a role or permission someone gave the user
// by hand is taken away at the next session.
func TestMSSQLLevelIsExact(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_exact")
	h := m.mustOpen(t, "argus_t_exact", tunnel.LevelRead)
	h.close()
	l := tunnel.MSSQLIdent(tunnel.MSSQLLogin("ana_sari", "read", "argus_t_exact"))
	msExec(t, root, "USE [argus_t_exact]; ALTER ROLE db_datawriter ADD MEMBER "+l+
		"; GRANT ALTER ON SCHEMA::dbo TO "+l+"; GRANT DELETE ON dbo.items TO "+l+"; USE [master]")
	h = m.mustOpen(t, "argus_t_exact", tunnel.LevelRead)
	if _, _, a := h.run(1, "DELETE FROM items WHERE id = 1", 0); a.Type != "error" {
		t.Fatalf("a hand-given grant survived: %+v", a)
	}
}

func TestMSSQLRecordingAndGuard(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_rec")
	h := m.mustOpen(t, "argus_t_rec", tunnel.LevelWrite)

	_, _, a := h.run(1, "DELETE FROM items", 0)
	if a.Type != "error" || a.Guard == nil || a.Code != tunnel.GuardNoWhere {
		t.Fatalf("a DELETE of every row was not stopped: %+v", a)
	}
	_, _, a = h.run(2, "DELETE TOP (2) FROM [dbo].[items]", 0)
	if a.Type != "error" || a.Guard == nil || a.Guard.Targets[0] != "dbo.items" {
		t.Fatalf("a DELETE TOP with no WHERE was not stopped: %+v", a)
	}
	var n int
	_ = root.QueryRow("SELECT COUNT(*) FROM argus_t_rec.dbo.items").Scan(&n)
	if n != 3 {
		t.Fatalf("the guarded DELETE ran: %d rows left", n)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 3, Op: "run", SQL: "DELETE FROM [items]", Confirm: a.Guard.Confirm})
	if a.Type != "error" {
		t.Fatalf("a confirm for another statement let it run: %+v", a)
	}
	_, _, g := h.run(4, "DELETE FROM [items]", 0)
	_, _, a = h.do(tunnel.PGRequest{ID: 5, Op: "run", SQL: "DELETE FROM [items]", Confirm: g.Guard.Confirm})
	if a.Type != "done" || a.Tag != "DELETE 3" {
		t.Fatalf("the confirmed DELETE: %+v", a)
	}
	// Values from the console's :name, as $n.
	_, rows, a := h.do(tunnel.PGRequest{ID: 6, Op: "run", SQL: "SELECT $1 AS a, '$1' AS b, $2 AS c, $1 AS d, [$1] = 1",
		Params: []*string{ptr("it's"), nil}})
	if a.Type != "done" || cell(rows, 0, 0) != "it's" || cell(rows, 0, 1) != "$1" || cell(rows, 0, 2) != "<null>" || cell(rows, 0, 3) != "it's" {
		t.Fatalf("params: %+v %v", a, rows)
	}
	// PRINT and a batch's row counts come back as messages.
	_, _, a = h.run(7, "PRINT 'hello'; INSERT INTO items (id, name) VALUES (8, N'eight'), (9, N'nine')", 0)
	if a.Type != "done" || a.Rows != 2 || len(a.Notices) != 1 || a.Notices[0].Message != "hello" {
		t.Fatalf("print: %+v", a)
	}
	// Every error a batch raised, not only the last.
	_, _, a = h.run(8, "INSERT INTO items (id, name) VALUES (8, N'again')", 0)
	if a.Type != "error" || !strings.Contains(a.Error, "PRIMARY KEY") || !strings.Contains(a.Detail, "SQL Server error 2627") {
		t.Fatalf("duplicate key: %+v", a)
	}
	h.close()

	b, err := os.ReadFile(m.dir + "/" + recordingName(m.dir, t))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := tunnel.VerifyPGRecording(strings.NewReader(string(b)), ""); err != nil || !v.OK {
		t.Fatalf("the recording does not verify: %+v %v", v, err)
	}
	if !recordedAs(h.rec, "DELETE FROM items", "error") || !recordedAs(h.rec, "DELETE FROM [items]", "error") {
		t.Fatalf("recording: %+v", h.rec)
	}
}

func TestMSSQLCancelKeepsTheTransaction(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_cancel")
	h := m.mustOpen(t, "argus_t_cancel", tunnel.LevelWrite)
	if _, _, a := h.run(1, "BEGIN TRANSACTION", 0); a.Type != "done" {
		t.Fatalf("begin: %+v", a)
	}
	if _, _, a := h.run(2, "INSERT INTO items (id, name) VALUES (9, N'nine')", 0); a.Type != "done" || a.Tag != "INSERT 1" {
		t.Fatalf("insert: %+v", a)
	}
	h.send(tunnel.PGRequest{ID: 3, Op: "run", SQL: "WAITFOR DELAY '00:00:20'"})
	for !h.stmtSeen(3) {
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
	if a.Type != "error" || a.Code != "57014" || !strings.Contains(a.Error, "cancelled") {
		t.Fatalf("cancel: %+v", a)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the cancel took %s", time.Since(start))
	}
	if _, rows, a := h.run(4, "SELECT COUNT(*), @@TRANCOUNT FROM items", 0); a.Type != "done" || cell(rows, 0, 0) != "4" || cell(rows, 0, 1) != "1" {
		t.Fatalf("the transaction did not survive the cancel: %+v %v", a, rows)
	}
	h.close()
	var n int
	_ = root.QueryRow("SELECT COUNT(*) FROM argus_t_cancel.dbo.items").Scan(&n)
	if n != 3 {
		t.Fatalf("an uncommitted insert was kept when the session closed: %d rows", n)
	}
}

func TestMSSQLTimeout(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_timeout")
	h, err := m.open(t, "ana_sari", "argus_t_timeout", tunnel.LevelRead, func(d *tunnel.GrantDB) { d.StatementTimeoutMS = 500 })
	if err != nil {
		t.Fatal(err)
	}
	_, _, a := h.run(1, "WAITFOR DELAY '00:00:10'", 0)
	if a.Type != "error" || a.Code != "57014" || !strings.Contains(a.Error, "timeout") {
		t.Fatalf("timeout: %+v", a)
	}
	if _, _, a := h.run(2, "SELECT 1", 0); a.Type != "done" {
		t.Fatalf("after the timeout: %+v", a)
	}
}

func TestMSSQLRowLimitAndSchema(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_rows")
	msExec(t, root, "USE [argus_t_rows]; EXEC('CREATE SCHEMA sales'); EXEC('CREATE VIEW sales.v AS SELECT 1 AS one'); USE [master]")
	h := m.mustOpen(t, "argus_t_rows", tunnel.LevelRead)
	_, rows, a := h.run(1, "SELECT a.id FROM items a, items b, items c, items d", 5)
	if a.Type != "done" || !a.Truncated || len(rows) != 5 {
		t.Fatalf("row limit: %+v (%d rows)", a, len(rows))
	}
	if _, _, a := h.run(2, "SELECT 1", 0); a.Type != "done" {
		t.Fatalf("after the limit: %+v", a)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 3, Op: "schema", Kind: "schemas"})
	if a.Type != "schema" || len(a.Objects) != 2 || a.Objects[0].Name != "dbo" || a.Objects[0].Count != 1 || a.Objects[1].Name != "sales" {
		t.Fatalf("schemas: %+v", a)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 4, Op: "schema", Kind: "objects", Schema: "dbo"})
	if a.Type != "schema" || len(a.Objects) != 1 || a.Objects[0].Name != "items" || a.Objects[0].Kind != "table" || a.Objects[0].Count != 3 {
		t.Fatalf("objects: %+v", a)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 5, Op: "schema", Kind: "columns", Schema: "dbo", Object: "items"})
	if a.Type != "schema" || len(a.Objects) != 8 || !strings.Contains(a.Objects[0].Detail, "primary key") ||
		a.Objects[1].Detail != "nvarchar(40)" || a.Objects[3].Detail != "decimal(10,2)" {
		t.Fatalf("columns: %+v", a)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 6, Op: "schema", Kind: "completion"})
	if a.Type != "schema" || len(a.Objects) < 4 || a.Objects[0].Kind != "path" || a.Objects[0].Name != "dbo" {
		t.Fatalf("completion: %+v", a)
	}
	_, _, a = h.do(tunnel.PGRequest{ID: 7, Op: "check", SQL: "UPDATE items SET name = 'x' WHERE id = 1"})
	if a.Type != "error" || !strings.Contains(a.Error, "unknown operation") {
		t.Fatalf("check, which the console reads as not offered: %+v", a)
	}
}

// Logins that are not Argus's are never touched.
func TestMSSQLLeavesOtherLoginsAlone(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_app")
	msExec(t, root, "IF SUSER_ID('app_t') IS NOT NULL DROP LOGIN app_t",
		"CREATE LOGIN app_t WITH PASSWORD = 'App-pw-1!x', CHECK_POLICY = OFF",
		"USE [argus_t_app]; CREATE USER app_t FOR LOGIN app_t; ALTER ROLE db_datareader ADD MEMBER app_t; USE [master]")
	t.Cleanup(func() { _, _ = root.Exec("IF SUSER_ID('app_t') IS NOT NULL DROP LOGIN app_t") })
	for _, lvl := range []string{"read", "dba"} {
		h := m.mustOpen(t, "argus_t_app", lvl)
		h.close()
	}
	exists, disabled, _ := msLogin(t, root, "app_t")
	if !exists || disabled {
		t.Fatal("the application's login was changed")
	}
	var member int
	_ = root.QueryRow("USE [argus_t_app]; SELECT IS_ROLEMEMBER('db_datareader', 'app_t')").Scan(&member)
	if member != 1 {
		t.Fatal("the application's role was taken away")
	}
}

// A login a stopped agent left enabled is disabled at the next session.
func TestMSSQLSweep(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_sweep")
	h := m.mustOpen(t, "argus_t_sweep", tunnel.LevelRead)
	h.close()
	l := tunnel.MSSQLLogin("ana_sari", "read", "argus_t_sweep")
	msExec(t, root, "ALTER LOGIN "+tunnel.MSSQLIdent(l)+" ENABLE")
	h = m.mustOpen(t, "argus_t_sweep", tunnel.LevelWrite)
	if _, disabled, _ := msLogin(t, root, l); !disabled {
		t.Fatal("the stale login was left enabled")
	}
	h.close()
}

// An approved change that changes more rows than were approved is rolled
// back, alone or inside the person's own transaction.
func TestMSSQLRowCap(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_cap")
	h := m.mustOpen(t, "argus_t_cap", tunnel.LevelWrite)
	one, three := int64(1), int64(3)
	_, _, a := h.do(tunnel.PGRequest{ID: 1, Op: "run", SQL: "UPDATE items SET note = N'x' WHERE id > 0", RowCap: &one})
	if a.Type != "error" || !strings.Contains(a.Error, "rolled back") {
		t.Fatalf("over the cap: %+v", a)
	}
	var n int
	_ = root.QueryRow("SELECT COUNT(*) FROM argus_t_cap.dbo.items WHERE note = N'x'").Scan(&n)
	if n != 0 {
		t.Fatalf("the over-cap update was kept: %d", n)
	}
	// Two statements in one batch count together.
	_, _, a = h.do(tunnel.PGRequest{ID: 2, Op: "run", SQL: "SELECT 1 UPDATE items SET note = N'z' WHERE id = 1 UPDATE items SET note = N'z' WHERE id = 2", RowCap: &one})
	if a.Type != "error" || !strings.Contains(a.Error, "rolled back") {
		t.Fatalf("a batch over the cap: %+v", a)
	}
	h.run(3, "BEGIN TRANSACTION", 0)
	h.run(4, "INSERT INTO items (id, name) VALUES (7, N'seven')", 0)
	if _, _, a := h.do(tunnel.PGRequest{ID: 5, Op: "run", SQL: "DELETE FROM items WHERE id > 0", RowCap: &three}); a.Type != "error" {
		t.Fatalf("over the cap in a transaction: %+v", a)
	}
	if _, rows, _ := h.run(6, "SELECT COUNT(*), @@TRANCOUNT FROM items", 0); cell(rows, 0, 0) != "4" || cell(rows, 0, 1) != "1" {
		t.Fatalf("the savepoint did not keep the person's insert: %v", rows)
	}
	if _, _, a := h.do(tunnel.PGRequest{ID: 7, Op: "run", SQL: "UPDATE items SET note = N'y' WHERE id = 7", RowCap: &one}); a.Type != "done" {
		t.Fatalf("within the cap: %+v", a)
	}
	h.run(8, "COMMIT", 0)
	h.close()
	_ = root.QueryRow("SELECT COUNT(*) FROM argus_t_cap.dbo.items WHERE note = N'y'").Scan(&n)
	if n != 1 {
		t.Fatalf("the change within its cap was not kept: %d", n)
	}
}

func TestMSSQLRefusals(t *testing.T) {
	m := newMSService(t)
	if _, err := m.open(t, "ana_sari", "argus_t_none", tunnel.LevelRead, nil); err == nil ||
		!strings.Contains(err.Error(), "no database") {
		t.Fatalf("a database that is not there: %v", err)
	}
	if _, err := m.open(t, "ana_sari", "master", tunnel.LevelRead, nil); err == nil {
		t.Fatal("SQL Server's own database opened")
	}
	if _, err := m.open(t, "ana_sari", "argus_t_x", tunnel.LevelRead, func(d *tunnel.GrantDB) { d.Host = "10.0.0.5" }); err == nil {
		t.Fatal("a remote host opened")
	}
	if _, err := m.open(t, "pg_root", "argus_t_x", tunnel.LevelRead, nil); err == nil ||
		!strings.Contains(err.Error(), "not a user") {
		t.Fatalf("a session opened as a reserved name: %v", err)
	}
	bad := filepath.Join(m.dir, "open.conf")
	_ = os.WriteFile(bad, []byte("user = sa\npassword = x\n"), 0o644)
	if _, err := m.open(t, "ana_sari", "argus_t_x", tunnel.LevelRead, func(d *tunnel.GrantDB) { d.ConfigPath = bad }); err == nil ||
		!strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("an admin file others can read: %v", err)
	}
}

func TestMSSQLDiscover(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_found")
	res := discoverMSSQL(context.Background(), tunnel.GrantDiscover{Engine: tunnel.EngineMSSQL,
		Host: "127.0.0.1", Port: port, ConfigPath: m.file})
	if !res.OK || res.Version == "" {
		t.Fatalf("%+v", res)
	}
	found := false
	for _, d := range res.Databases {
		if tunnel.IsMSSQLSystemDB(d.Name) {
			t.Fatalf("listed SQL Server's own %s", d.Name)
		}
		found = found || (d.Name == "argus_t_found" && d.Bytes > 0)
	}
	if !found {
		t.Fatalf("argus_t_found is not listed with its size: %+v", res.Databases)
	}
}

func TestMSSQLPlaceholders(t *testing.T) {
	for _, c := range []struct {
		in, out string
		n       int
		bad     bool
	}{
		{"SELECT 1", "SELECT 1", 0, false},
		{"SELECT $1, $2, $1", "SELECT @p1, @p2, @p1", 2, false},
		{"SELECT '$1', \"$2\", [$1], $1 -- $2\n", "SELECT '$1', \"$2\", [$1], @p1 -- $2\n", 2, false},
		{"SELECT 'it''s $1', [a]]$1], $1", "SELECT 'it''s $1', [a]]$1], @p1", 1, false},
		{"SELECT /* $1 /* $1 */ $1 */ $1", "SELECT /* $1 /* $1 */ $1 */ @p1", 1, false},
		{"SELECT $3", "", 2, true},
	} {
		got, bad := mssqlPlaceholders(c.in, c.n)
		if (bad != "") != c.bad || (!c.bad && got != c.out) {
			t.Errorf("%q: got %q %q", c.in, got, bad)
		}
	}
}

func TestMSSQLAdminFile(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/mssql.conf"
	if err := os.WriteFile(p, []byte("# admin\nuser = \"argus_admin\"\npassword = s3=cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := readMSSQLAdmin(p)
	if err != nil || c.user != "argus_admin" || c.pw != "s3=cret" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := readMSSQLAdmin(dir + "/missing.conf"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readMSSQLAdmin(p); err == nil {
		t.Fatal("a file others can read was used")
	}
}

func TestMSSQLVersions(t *testing.T) {
	for v, ok := range map[string]bool{"16.0.4135.4": true, "13.0.5026.0": true, "12.0.6024.0": false, "17.0.1000.7": true} {
		if (mssqlVersionOK(v) == nil) != ok {
			t.Errorf("%s: %v", v, mssqlVersionOK(v))
		}
	}
}

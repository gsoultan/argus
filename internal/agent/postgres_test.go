//go:build !windows

package agent

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Against a real Postgres: ARGUS_TEST_PG_SUPER_DSN is a superuser connection
string to a database the test may create roles and tables in, for example

	ARGUS_TEST_PG_SUPER_DSN='postgres://postgres@/app?host=/var/tmp/pgt&port=5433'

Nothing else sets Postgres up: the test provisions as the agent would.
*/

func pgTest(t *testing.T) (dsn string, host string, port int, dbname string) {
	t.Helper()
	dsn = os.Getenv("ARGUS_TEST_PG_SUPER_DSN")
	if dsn == "" {
		t.Skip("ARGUS_TEST_PG_SUPER_DSN is not set")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return dsn, cfg.Host, int(cfg.Port), cfg.Database
}

// pipeStream is a net.Pipe end as a tunnel stream.
type pipeStream struct{ net.Conn }

func (p pipeStream) SetDeadline(t time.Time) error { return p.Conn.SetDeadline(t) }

type pgHarness struct {
	t      *testing.T
	stream tunnel.Stream
	fr     *tunnel.FrameReader
	rec    []tunnel.PGRecord
	dir    string
	done   chan struct{}
	ready  tunnel.PGAnswer
	// close ends the session as the browser leaving would, and waits for
	// the agent side to finish locking the role.
	close func()
}

// openPG opens a session as the control plane would and returns the browser
// side of it.
func openPG(t *testing.T, level string, extra func(*tunnel.GrantDB)) *pgHarness {
	t.Helper()
	dsn, host, port, dbname := pgTest(t)
	key, _ := tunnel.GrantKeyFromSecret(strings.Repeat("s", 32))
	pub := key.Public().(ed25519.PublicKey)
	dir := t.TempDir()
	svc := Postgres(PostgresConfig{
		Verifier:  &tunnel.GrantVerifier{Host: "db-01"},
		RecordDir: dir,
		Admin:     DSNAdmin(func(string) string { return dsn }),
	})
	g := tunnel.NewGrant("ana.sari@corp.example", "db-01", "ana_sari", "")
	g.Service = tunnel.PostgresService
	g.DB = &tunnel.GrantDB{ID: "db1", Name: dbname, Host: host, Port: port, Level: level}
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
		svc(ctx, &Call{Header: tunnel.Header{Service: tunnel.PostgresService, Grant: tok},
			Stream: pipeStream{a}, GrantKey: pub})
	}()
	h := &pgHarness{t: t, stream: pipeStream{b}, fr: tunnel.NewFrameReader(b), dir: dir, done: done, close: closeOnce}
	var ans tunnel.Answer
	if err := tunnel.ReadJSON(b, &ans); err != nil {
		t.Fatal(err)
	}
	if !ans.OK {
		t.Fatalf("the session was refused: %s", ans.Error)
	}
	if h.ready = h.answer(); h.ready.Type != "ready" || h.ready.Level != level {
		t.Fatalf("expected ready at %s, got %+v", level, h.ready)
	}
	return h
}

func (h *pgHarness) send(req tunnel.PGRequest) {
	b, _ := json.Marshal(req)
	if err := tunnel.WriteFrame(h.stream, tunnel.FramePGRequest, b); err != nil {
		h.t.Fatal(err)
	}
}

// answer reads frames until an answer, keeping records and counting rows.
func (h *pgHarness) answer() tunnel.PGAnswer {
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
		case tunnel.FramePGAnswer:
			var a tunnel.PGAnswer
			_ = json.Unmarshal(p, &a)
			return a
		case tunnel.FramePGRows:
			h.t.Fatalf("rows before the columns")
		}
	}
}

// run sends a statement and collects its columns, rows and final answer.
func (h *pgHarness) run(id uint32, sql string, maxRows int) (cols []tunnel.PGColumn, rows [][]*string, final tunnel.PGAnswer) {
	h.send(tunnel.PGRequest{ID: id, Op: "run", SQL: sql, MaxRows: maxRows})
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
			rid, batch, err := tunnel.DecodeRows(p, len(cols))
			if err != nil || rid != id {
				h.t.Fatalf("bad batch: %v (id %d)", err, rid)
			}
			rows = append(rows, batch...)
		case tunnel.FramePGAnswer:
			var a tunnel.PGAnswer
			_ = json.Unmarshal(p, &a)
			if a.ID != id {
				h.t.Fatalf("answer for %d while running %d", a.ID, id)
			}
			if a.Type == "cols" {
				cols = a.Cols
				continue
			}
			return cols, rows, a
		}
	}
}

func superExec(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	conn, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for _, s := range stmts {
		if _, err := conn.Exec(context.Background(), s).ReadAll(); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func superQuery(t *testing.T, dsn, sql string) [][]string {
	t.Helper()
	conn, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	res, err := conn.Exec(context.Background(), sql).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, r := range res {
		for _, row := range r.Rows {
			var cells []string
			for _, c := range row {
				cells = append(cells, string(c))
			}
			out = append(out, cells)
		}
	}
	return out
}

func TestPostgresReadSession(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	superExec(t, dsn,
		"DROP TABLE IF EXISTS argus_t_orders",
		"CREATE TABLE argus_t_orders (id serial PRIMARY KEY, email text, total bigint, note text)",
		"INSERT INTO argus_t_orders (email, total, note) SELECT 'u'||i||'@x', i*100, NULL FROM generate_series(1, 2500) i",
		"DROP ROLE IF EXISTS ana_sari")
	h := openPG(t, tunnel.LevelRead, nil)

	// A read works, in Postgres's text format, NULL kept apart from ''.
	cols, rows, final := h.run(1, "SELECT id, email, total, note FROM argus_t_orders ORDER BY id LIMIT 3", 0)
	if final.Type != "done" || final.Rows != 3 || len(rows) != 3 || len(cols) != 4 {
		t.Fatalf("final %+v, %d rows, %d cols", final, len(rows), len(cols))
	}
	if cols[2].Type != "bigint" || *rows[0][1] != "u1@x" || rows[0][3] != nil {
		t.Fatalf("cols %+v row %v", cols, rows[0])
	}

	// A write is Postgres's refusal, not ours, and nothing changed.
	_, _, final = h.run(2, "UPDATE argus_t_orders SET total = 0 WHERE id = 1", 0)
	if final.Type != "error" || final.Code != "42501" && final.Code != "25006" {
		t.Fatalf("a write on a read session must be refused by Postgres: %+v", final)
	}
	if got := superQuery(t, dsn, "SELECT total FROM argus_t_orders WHERE id = 1"); got[0][0] != "100" {
		t.Fatalf("the row changed: %v", got)
	}

	// Two statements in one request are one error.
	_, _, final = h.run(3, "SELECT 1; SELECT 2", 0)
	if final.Type != "error" {
		t.Fatalf("multiple statements must be refused: %+v", final)
	}

	// The row limit stops the statement and says so.
	_, rows, final = h.run(4, "SELECT id FROM argus_t_orders ORDER BY id", 1200)
	if final.Type != "done" || !final.Truncated || len(rows) != 1200 {
		t.Fatalf("limit: %+v, %d rows", final, len(rows))
	}
	// And the connection is still usable after it.
	_, rows, final = h.run(5, "SELECT count(*) FROM argus_t_orders", 0)
	if final.Type != "done" || *rows[0][0] != "2500" {
		t.Fatalf("after a truncation: %+v %v", final, rows)
	}

	// A syntax error carries Postgres's position.
	_, _, final = h.run(6, "SELEC 1", 0)
	if final.Type != "error" || final.Code != "42601" || final.Position != 1 {
		t.Fatalf("syntax error: %+v", final)
	}

	// The object tree shows what the role may see.
	h.send(tunnel.PGRequest{ID: 7, Op: "schema", Kind: "columns", Schema: "public", Object: "argus_t_orders"})
	if a := h.answer(); a.Type != "schema" || len(a.Objects) != 4 || a.Objects[0].Name != "id" ||
		!strings.Contains(a.Objects[0].Detail, "primary key") {
		t.Fatalf("columns: %+v", a)
	}

	// The role: created by Argus, a member of read only, login for now.
	got := superQuery(t, dsn, `SELECT rolcanlogin, rolsuper, shobj_description(oid, 'pg_authid'),
		(SELECT string_agg(b.rolname, ',' ORDER BY b.rolname) FROM pg_auth_members m
		   JOIN pg_roles b ON b.oid = m.roleid WHERE m.member = r.oid)
		FROM pg_roles r WHERE rolname = 'ana_sari'`)
	if len(got) != 1 || got[0][0] != "t" || got[0][1] != "f" || !strings.HasPrefix(got[0][2], "argus-managed owner=ana.sari@corp.example") {
		t.Fatalf("role: %v", got)
	}
	if !strings.Contains(got[0][3], "argus_people") || !strings.Contains(got[0][3], "argus_read_") ||
		strings.Contains(got[0][3], "argus_write_") {
		t.Fatalf("memberships: %v", got[0][3])
	}

	// Close: the stream ends, the role is locked, the recording verifies.
	_ = h.stream.Close()
	<-h.done
	got = superQuery(t, dsn, "SELECT rolcanlogin, rolpassword IS NULL FROM pg_authid WHERE rolname = 'ana_sari'")
	if got[0][0] != "f" || got[0][1] != "t" {
		t.Fatalf("after close the role must be NOLOGIN with no password: %v", got)
	}
	f, err := os.Open(filepath.Join(h.dir, recordingName(h.dir, t)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var prev string
	n := 0
	kinds := []string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r tunnel.PGRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Prev != prev || RecordHash(r) != r.Hash {
			t.Fatalf("chain broken at seq %d", r.Seq)
		}
		prev = r.Hash
		n++
		kinds = append(kinds, r.Kind)
	}
	// And the shared verifier, which the control plane and argus-verify use,
	// agrees with the agent's own hashes.
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if v, err := tunnel.VerifyPGRecording(f, prev); err != nil || !v.OK || v.Lines != n {
		t.Fatalf("shared verifier: %+v %v", v, err)
	}
	want := "open stmt done stmt error stmt error stmt done stmt done stmt error close"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("recording kinds: %v", kinds)
	}
	// The statement was recorded before it ran: every stmt was streamed to
	// us before its done/error, and the UPDATE is in the record although it
	// was refused.
	var sawUpdate bool
	for _, r := range h.rec {
		if r.Kind == "stmt" && strings.HasPrefix(r.SQL, "UPDATE") {
			sawUpdate = true
		}
	}
	if !sawUpdate {
		t.Fatal("the refused UPDATE must be in the recording")
	}
}

func recordingName(dir string, t *testing.T) string {
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("recording dir: %v %d", err, len(ents))
	}
	return ents[0].Name()
}

func TestPostgresWriteSessionAndCancel(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	superExec(t, dsn,
		"DROP TABLE IF EXISTS argus_t_w",
		"CREATE TABLE argus_t_w (id int PRIMARY KEY, v text)",
		"DROP ROLE IF EXISTS ana_sari")
	h := openPG(t, tunnel.LevelWrite, func(db *tunnel.GrantDB) { db.StatementTimeoutMS = 20_000 })

	_, _, final := h.run(1, "INSERT INTO argus_t_w VALUES (1, 'a'), (2, 'b')", 0)
	if final.Type != "done" || final.Rows != 2 || final.Tag != "INSERT 0 2" {
		t.Fatalf("insert: %+v", final)
	}
	// A transaction spanning requests, then a rollback: nothing kept.
	for i, s := range []string{"BEGIN", "DELETE FROM argus_t_w", "ROLLBACK"} {
		if _, _, final = h.run(uint32(10+i), s, 0); final.Type != "done" {
			t.Fatalf("%s: %+v", s, final)
		}
	}
	if got := superQuery(t, dsn, "SELECT count(*) FROM argus_t_w"); got[0][0] != "2" {
		t.Fatalf("rollback: %v", got)
	}
	// Owner work is refused at write: no CREATE on the schema.
	_, _, final = h.run(20, "CREATE TABLE argus_t_nope (id int)", 0)
	if final.Type != "error" || final.Code != "42501" {
		t.Fatalf("DDL on a write session must be Postgres's permission error: %+v", final)
	}

	// Cancel a long statement: an error answer, the connection survives. The
	// browser never stops reading, so the cancel is sent from a goroutine
	// while run() is reading this statement's frames.
	go func() {
		time.Sleep(400 * time.Millisecond)
		h.send(tunnel.PGRequest{ID: 30, Op: "cancel"})
	}()
	cancelStart := time.Now()
	_, _, final = h.run(30, "SELECT pg_sleep(30)", 0)
	if final.Type != "error" || final.Code != "57014" {
		t.Fatalf("cancel: %+v", final)
	}
	if took := time.Since(cancelStart); took > 5*time.Second {
		t.Fatalf("cancel took %v; it must not wait out the statement timeout", took)
	}
	_, rows, final := h.run(31, "SELECT 42", 0)
	if final.Type != "done" || *rows[0][0] != "42" {
		t.Fatalf("after cancel: %+v %v", final, rows)
	}
}

func TestPostgresRefusals(t *testing.T) {
	dsn, host, port, dbname := pgTest(t)
	key, _ := tunnel.GrantKeyFromSecret(strings.Repeat("s", 32))
	pub := key.Public().(ed25519.PublicKey)
	svc := Postgres(PostgresConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"},
		RecordDir: t.TempDir(), Admin: DSNAdmin(func(string) string { return dsn })})
	try := func(name string, mutate func(*tunnel.Grant)) string {
		t.Helper()
		g := tunnel.NewGrant("ana.sari@corp.example", "db-01", "ana_sari", "")
		g.Service = tunnel.PostgresService
		g.DB = &tunnel.GrantDB{ID: "db1", Name: dbname, Host: host, Port: port, Level: tunnel.LevelRead}
		mutate(&g)
		tok, _ := tunnel.SignGrant(key, g)
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		go svc(context.Background(), &Call{Header: tunnel.Header{Service: tunnel.PostgresService, Grant: tok},
			Stream: pipeStream{a}, GrantKey: pub})
		var ans tunnel.Answer
		if err := tunnel.ReadJSON(b, &ans); err != nil {
			t.Fatal(err)
		}
		if ans.OK {
			t.Fatalf("%s: must be refused", name)
		}
		return ans.Error
	}
	try("terminal grant", func(g *tunnel.Grant) { g.Service = "" })
	try("files grant", func(g *tunnel.Grant) { g.Service = "files"; g.DB = nil })
	try("remote host", func(g *tunnel.Grant) { g.DB.Host = "10.0.0.9" })
	try("superuser role", func(g *tunnel.Grant) { g.Principal = "postgres" })
	try("pg_ role", func(g *tunnel.Grant) { g.Principal = "pg_read_server_files" })
	try("quoted role", func(g *tunnel.Grant) { g.Principal = `a"; DROP ROLE x; --` })
	try("unknown level", func(g *tunnel.Grant) { g.DB.Level = "superuser" })
	try("taken role", func(g *tunnel.Grant) {
		superExec(t, dsn, "DROP ROLE IF EXISTS deploy_user", "CREATE ROLE deploy_user LOGIN")
		g.Principal = "deploy_user"
	})
	if got := superQuery(t, dsn, "SELECT rolcanlogin FROM pg_roles WHERE rolname = 'deploy_user'"); got[0][0] != "t" {
		t.Fatal("a role Argus did not create must not be touched")
	}
}

func TestLevelRole(t *testing.T) {
	if got := levelRole("read", "orders"); got != "argus_read_orders" {
		t.Fatal(got)
	}
	odd := levelRole("read", `Or"ders;--`)
	if !safeIdent.MatchString(odd) || odd == levelRole("read", "or_ders___") {
		t.Fatalf("an odd database name must give a safe, distinct role: %s", odd)
	}
	long := levelRole("write", strings.Repeat("x", 100))
	if len(long) > 63 || !safeIdent.MatchString(long) {
		t.Fatal(long)
	}
}

func TestRateLimit(t *testing.T) {
	r := newRateLimit(3, time.Minute)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !r.allow(now) {
			t.Fatal(i)
		}
	}
	if r.allow(now) {
		t.Fatal("fourth must be refused")
	}
	if !r.allow(now.Add(time.Minute)) {
		t.Fatal("the window must slide")
	}
}

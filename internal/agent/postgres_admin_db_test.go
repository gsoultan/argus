//go:build !windows

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

// admin sends one administrative operation and returns its answer.
func (h *pgHarness) admin(id uint32, op tunnel.PGAdminOp) tunnel.PGAnswer {
	h.send(tunnel.PGRequest{ID: id, Op: "admin", Admin: &op})
	a := h.answer()
	if a.ID != id {
		h.t.Fatalf("answer for %d while running %d", a.ID, id)
	}
	return a
}

// The helper's extension and replication operations, end to end through a
// session: refused below dba, performed at dba as the server's postgres
// account, recorded before they run, and the publisher's password nowhere
// but inside the statement Postgres ran.
func TestPostgresAdminOps(t *testing.T) {
	dsn, host, port, dbname := pgTest(t)
	const secret = "hunter2-the-publisher-password"
	superExec(t, dsn,
		"DROP TABLE IF EXISTS argus_t_pub",
		"CREATE TABLE argus_t_pub (id int PRIMARY KEY)",
		"DROP PUBLICATION IF EXISTS argus_t_pub_p",
		"DROP EXTENSION IF EXISTS pgcrypto",
		"DROP ROLE IF EXISTS ana_sari")
	// A leftover subscription from an earlier failed run has no slot to
	// drop on any publisher; forget it the way the helper would.
	superTry(t, dsn, "ALTER SUBSCRIPTION argus_t_sub DISABLE",
		"ALTER SUBSCRIPTION argus_t_sub SET (slot_name = NONE)", "DROP SUBSCRIPTION argus_t_sub")

	// Below dba: refused by the agent, before anything is recorded or run.
	r := openPG(t, tunnel.LevelOwner, nil)
	if a := r.admin(1, tunnel.PGAdminOp{Kind: tunnel.AdminExtensionList}); a.Type != "error" || a.Code != "42501" {
		t.Fatalf("an owner session must be refused: %+v", a)
	}
	if a := r.admin(2, tunnel.PGAdminOp{Kind: tunnel.AdminExtensionCreate, Name: "pgcrypto"}); a.Type != "error" || a.Code != "42501" {
		t.Fatalf("an owner session must be refused: %+v", a)
	}
	for _, rec := range r.rec {
		if rec.Kind == "admin" {
			t.Fatal("a refused operation must not be recorded as run")
		}
	}
	if got := superQuery(t, dsn, "SELECT count(*) FROM pg_extension WHERE extname = 'pgcrypto'"); got[0][0] != "0" {
		t.Fatal("the refused install happened")
	}
	r.close()

	h := openPG(t, tunnel.LevelDBA, nil)

	// Extensions.
	a := h.admin(10, tunnel.PGAdminOp{Kind: tunnel.AdminExtensionList})
	if a.Type != "admin" {
		t.Fatalf("extension.list: %+v", a)
	}
	var exts []PGExtension
	if err := json.Unmarshal(a.Admin, &exts); err != nil {
		t.Fatal(err)
	}
	var sawPlpgsql, sawPgcrypto bool
	for _, e := range exts {
		if e.Name == "plpgsql" && e.InstalledVersion != "" {
			sawPlpgsql = true
		}
		if e.Name == "pgcrypto" && e.InstalledVersion == "" && len(e.Versions) > 0 && e.DefaultVersion != "" {
			sawPgcrypto = true
		}
	}
	if !sawPlpgsql || !sawPgcrypto {
		t.Fatalf("extension.list: plpgsql installed %v, pgcrypto available %v", sawPlpgsql, sawPgcrypto)
	}
	if a = h.admin(11, tunnel.PGAdminOp{Kind: tunnel.AdminExtensionCreate, Name: "pgcrypto", Schema: "public"}); a.Type != "admin" || a.Tag != "CREATE EXTENSION" {
		t.Fatalf("extension.create: %+v", a)
	}
	if a = h.admin(12, tunnel.PGAdminOp{Kind: tunnel.AdminExtensionCreate, Name: "no_such_extension_here"}); a.Type != "error" || !strings.Contains(a.Error, "no extension named") {
		t.Fatalf("a missing extension must be a clear refusal: %+v", a)
	}
	if a = h.admin(13, tunnel.PGAdminOp{Kind: tunnel.AdminExtensionUpdate, Name: "pgcrypto"}); a.Type != "admin" {
		t.Fatalf("extension.update: %+v", a)
	}

	// Publications.
	a = h.admin(20, tunnel.PGAdminOp{Kind: tunnel.AdminPublicationCreate, Name: "argus_t_pub_p",
		Tables: []string{"public.argus_t_pub"}, Publish: []string{"insert", "update"}})
	if a.Type != "admin" || a.Tag != "CREATE PUBLICATION" {
		t.Fatalf("publication.create: %+v", a)
	}
	if a = h.admin(21, tunnel.PGAdminOp{Kind: tunnel.AdminPublicationTables, Name: "argus_t_pub_p", Tables: []string{"public.argus_t_pub"}}); a.Type != "admin" {
		t.Fatalf("publication.tables: %+v", a)
	}
	// A publication made outside Argus is found and changed the same way.
	superTry(t, dsn, "DROP PUBLICATION IF EXISTS argus_t_pub_foreign")
	superQuery(t, dsn, "CREATE PUBLICATION argus_t_pub_foreign FOR ALL TABLES")
	if a = h.admin(23, tunnel.PGAdminOp{Kind: tunnel.AdminPublicationOptions, Name: "argus_t_pub_foreign", Publish: []string{"insert"}, ViaRoot: bp(true)}); a.Type != "admin" || a.Tag != "ALTER PUBLICATION" {
		t.Fatalf("publication.options: %+v", a)
	}
	var pubs []PGPublication
	a = h.admin(22, tunnel.PGAdminOp{Kind: tunnel.AdminPublicationList})
	if err := json.Unmarshal(a.Admin, &pubs); err != nil || a.Type != "admin" {
		t.Fatalf("publication.list: %+v %v", a, err)
	}
	var p *PGPublication
	for i := range pubs {
		if pubs[i].Name == "argus_t_pub_p" {
			p = &pubs[i]
		}
	}
	if p == nil || p.AllTables || strings.Join(p.Tables, ",") != "public.argus_t_pub" ||
		strings.Join(p.Publish, ",") != "insert,update" || p.SetUpBy != "ana.sari@corp.example" {
		t.Fatalf("publication.list: %+v", p)
	}
	var foreign *PGPublication
	for i := range pubs {
		if pubs[i].Name == "argus_t_pub_foreign" {
			foreign = &pubs[i]
		}
	}
	if foreign == nil || !foreign.AllTables || strings.Join(foreign.Publish, ",") != "insert" || !foreign.ViaRoot || foreign.SetUpBy != "" {
		t.Fatalf("a publication made outside Argus must be listed as it is, with no set-up-by: %+v", foreign)
	}
	superQuery(t, dsn, "DROP PUBLICATION argus_t_pub_foreign")

	// Subscriptions: created without connecting, so the test needs no second
	// server, and listed with the password gone.
	conn := &tunnel.PGConnInfo{Host: host, Port: port, DBName: dbname, User: "postgres", Password: secret, SSLMode: "disable"}
	if strings.HasPrefix(host, "/") {
		conn.SSLMode = ""
	}
	before := len(h.rec)
	a = h.admin(30, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionCreate, Preview: true, Name: "argus_t_sub",
		Conn: conn, Publications: []string{"argus_t_pub_p"}, Connect: bp(false)})
	var done PGAdminDone
	if err := json.Unmarshal(a.Admin, &done); err != nil || a.Type != "admin" {
		t.Fatalf("preview: %+v %v", a, err)
	}
	if !strings.Contains(done.SQL, "password=[redacted]") || strings.Contains(done.SQL, secret) {
		t.Fatalf("preview leaks or is not redacted: %q", done.SQL)
	}
	if len(h.rec) != before {
		t.Fatal("a preview must not be recorded")
	}
	if got := superQuery(t, dsn, "SELECT count(*) FROM pg_subscription WHERE subname = 'argus_t_sub'"); got[0][0] != "0" {
		t.Fatal("a preview ran")
	}
	a = h.admin(31, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionCreate, Name: "argus_t_sub",
		Conn: conn, Publications: []string{"argus_t_pub_p"}, Connect: bp(false)})
	if a.Type != "admin" || a.Tag != "CREATE SUBSCRIPTION" {
		t.Fatalf("subscription.create: %+v", a)
	}
	var subs []PGSubscription
	a = h.admin(32, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionList})
	if err := json.Unmarshal(a.Admin, &subs); err != nil || a.Type != "admin" || len(subs) == 0 {
		t.Fatalf("subscription.list: %+v %v", a, err)
	}
	var s *PGSubscription
	for i := range subs {
		if subs[i].Name == "argus_t_sub" {
			s = &subs[i]
		}
	}
	if s == nil || s.Enabled || strings.Join(s.Publications, ",") != "argus_t_pub_p" || s.SetUpBy != "ana.sari@corp.example" {
		t.Fatalf("subscription.list: %+v", s)
	}
	if !strings.HasPrefix(s.Publisher, "postgres@") || !strings.HasSuffix(s.Publisher, "/"+dbname) {
		t.Fatalf("publisher summary: %q", s.Publisher)
	}
	if !strings.Contains(s.Conninfo, "password=[redacted]") {
		t.Fatalf("conninfo must say the password was there and is hidden: %q", s.Conninfo)
	}
	if string(a.Admin) == "" || strings.Contains(string(a.Admin), secret) {
		t.Fatal("the list carries the password")
	}
	// Postgres itself holds it, as it always does for a subscription; that
	// is the one place.
	if got := superQuery(t, dsn, "SELECT subconninfo FROM pg_subscription WHERE subname = 'argus_t_sub'"); !strings.Contains(got[0][0], secret) {
		t.Fatalf("Postgres should hold the connection string: %q", got[0][0])
	}
	if a = h.admin(33, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionDisable, Name: "argus_t_sub"}); a.Type != "admin" {
		t.Fatalf("subscription.disable: %+v", a)
	}
	// Re-pointed at other publications without refreshing (the publisher is
	// not reachable here), and its apply options changed; the list shows
	// both.
	if a = h.admin(36, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionPublications, Name: "argus_t_sub", Publications: []string{"argus_t_pub_p", "other_pub"}, Refresh: bp(false)}); a.Type != "admin" || a.Tag != "ALTER SUBSCRIPTION" {
		t.Fatalf("subscription.publications: %+v", a)
	}
	if a = h.admin(37, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionOptions, Name: "argus_t_sub", SyncCommit: "local", Binary: bp(true), DisableOnError: bp(true)}); a.Type != "admin" {
		t.Fatalf("subscription.options: %+v", a)
	}
	a = h.admin(38, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionList})
	subs = nil
	if err := json.Unmarshal(a.Admin, &subs); err != nil || a.Type != "admin" {
		t.Fatalf("subscription.list: %+v %v", a, err)
	}
	s = nil
	for i := range subs {
		if subs[i].Name == "argus_t_sub" {
			s = &subs[i]
		}
	}
	if s == nil || strings.Join(s.Publications, ",") != "argus_t_pub_p,other_pub" || s.SyncCommit != "local" || s.Binary != "on" || s.DisableOnError != "on" || s.Streaming != "off" {
		t.Fatalf("the list must show the changed publications and options: %+v", s)
	}
	if a = h.admin(34, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionConnection, Name: "argus_t_sub",
		Conn: &tunnel.PGConnInfo{Host: host, Port: port, DBName: dbname, User: "postgres", Password: secret + "-rotated"}}); a.Type != "admin" {
		t.Fatalf("subscription.connection: %+v", a)
	}
	if a = h.admin(35, tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionDrop, Name: "argus_t_sub", PublisherGone: true}); a.Type != "admin" || a.Tag != "DROP SUBSCRIPTION" {
		t.Fatalf("subscription.drop: %+v", a)
	}
	if got := superQuery(t, dsn, "SELECT count(*) FROM pg_subscription WHERE subname = 'argus_t_sub'"); got[0][0] != "0" {
		t.Fatal("the subscription is still there")
	}

	// Drops, and a refusal that is Postgres's own.
	if a = h.admin(40, tunnel.PGAdminOp{Kind: tunnel.AdminPublicationDrop, Name: "argus_t_pub_p"}); a.Type != "admin" {
		t.Fatalf("publication.drop: %+v", a)
	}
	if a = h.admin(41, tunnel.PGAdminOp{Kind: tunnel.AdminPublicationDrop, Name: "argus_t_pub_p"}); a.Type != "error" || a.Code != "42704" {
		t.Fatalf("dropping it twice must be Postgres's undefined_object: %+v", a)
	}
	if a = h.admin(42, tunnel.PGAdminOp{Kind: tunnel.AdminExtensionDrop, Name: "pgcrypto"}); a.Type != "admin" || a.Tag != "DROP EXTENSION" {
		t.Fatalf("extension.drop: %+v", a)
	}
	if a = h.admin(43, tunnel.PGAdminOp{Kind: "role.create", Name: "x"}); a.Type != "error" {
		t.Fatalf("an unknown kind must be refused: %+v", a)
	}

	// The recording: every mutation is an admin line before its done/error,
	// lists and previews are absent, the secret is absent, the chain holds.
	_ = h.stream.Close()
	<-h.done
	f, err := os.Open(filepath.Join(h.dir, recordingName(h.dir, t)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var kinds []string
	var prev string
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, secret) {
			t.Fatalf("the recording holds the password: %s", line)
		}
		var rec tunnel.PGRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Prev != prev || RecordHash(rec) != rec.Hash {
			t.Fatalf("chain broken at seq %d", rec.Seq)
		}
		prev = rec.Hash
		n++
		kinds = append(kinds, rec.Kind)
		if rec.Kind == "admin" && rec.SQL == "" {
			t.Fatalf("an admin line without its statement: %+v", rec)
		}
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if v, err := tunnel.VerifyPGRecording(f, prev); err != nil || !v.OK || v.Lines != n {
		t.Fatalf("shared verifier: %+v %v", v, err)
	}
	want := "open " +
		"admin done " + // create pgcrypto
		"admin error " + // no such extension
		"admin done " + // update
		"admin done admin done admin done " + // publication create, tables, foreign options
		"admin done " + // subscription create
		"admin done admin done admin done admin done admin done " + // disable, publications, options, connection, drop
		"admin done admin error admin done " + // publication drop x2, extension drop
		"close"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("recording kinds:\n got %s\nwant %s", got, want)
	}
}

// superTry runs statements and ignores their failures: cleanup of what may
// not be there.
func superTry(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		conn, err := pgconn.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Exec(context.Background(), s).ReadAll()
		_ = conn.Close(context.Background())
	}
}

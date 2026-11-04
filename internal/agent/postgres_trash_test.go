//go:build !windows

package agent

import (
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// What a DELETE, DROP or TRUNCATE removes goes to the trash, and comes back.
func TestPostgresTrashKeepsAndRestores(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	superExec(t, dsn,
		"DROP SCHEMA IF EXISTS argus_t_tr_s CASCADE",
		"DROP TABLE IF EXISTS argus_t_tr, argus_t_tr2, argus_t_tr_dep CASCADE")
	h := openPG(t, tunnel.LevelOwner, nil)
	superExec(t, dsn, "CREATE SCHEMA argus_t_tr_s AUTHORIZATION "+pgx(levelRole(tunnel.LevelOwner, dbname)),
		"CREATE TABLE argus_t_tr_s.x (id int)", "INSERT INTO argus_t_tr_s.x VALUES (1)",
		"GRANT USAGE ON SCHEMA argus_t_tr_s TO PUBLIC")
	count := func(q string) string { return superQuery(t, dsn, q)[0][0] }
	// Made in the session, so the owner level owns them, as it would.
	for i, q := range []string{
		"CREATE TABLE argus_t_tr (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v text, j jsonb, twice int GENERATED ALWAYS AS (id * 2) STORED)",
		"INSERT INTO argus_t_tr (v, j) SELECT 'v' || g, jsonb_build_object('g', g) FROM generate_series(1, 20) g",
		"CREATE TABLE argus_t_tr2 (id int PRIMARY KEY)",
		"INSERT INTO argus_t_tr2 SELECT generate_series(1, 10001)",
	} {
		if _, _, a := h.run(uint32(100+i), q, 0); a.Type != "done" {
			t.Fatalf("%s: %+v", q, a)
		}
	}

	// A DELETE keeps its rows; the answer says what it kept.
	_, _, final := h.run(1, "DELETE FROM argus_t_tr t WHERE t.id <= 3;", 0)
	if final.Type != "done" || final.Tag != "DELETE 3" || final.Rows != 3 || len(final.Trash) != 1 {
		t.Fatalf("delete: %+v", final)
	}
	kept := final.Trash[0]
	if kept.Kind != "rows" || kept.Rows != 3 || kept.Source != "public.argus_t_tr" || kept.Expires == "" {
		t.Fatalf("kept: %+v", kept)
	}
	if count("SELECT count(*) FROM argus_t_tr") != "17" {
		t.Fatal("the rows were not deleted")
	}
	var ran string
	for _, r := range h.rec {
		if r.Request == 1 && r.Kind == "stmt" {
			ran = r.Ran
		}
	}
	if !strings.Contains(ran, "argus_trash_fn.keep_rows") {
		t.Fatalf("the recording does not say what ran: %q", ran)
	}
	// And Undo puts them back, identity and all, generated columns computed.
	_, _, final = h.run(2, "SELECT * FROM argus_trash_fn.restore("+itoa64(kept.ID)+")", 0)
	if final.Type != "done" {
		t.Fatalf("restore rows: %+v", final)
	}
	if got := superQuery(t, dsn, "SELECT id, v, j->>'g', twice FROM argus_t_tr WHERE id = 2"); len(got) != 1 || got[0][1] != "v2" || got[0][2] != "2" || got[0][3] != "4" {
		t.Fatalf("restored row: %v", got)
	}
	_, _, final = h.run(3, "SELECT * FROM argus_trash_fn.restore("+itoa64(kept.ID)+")", 0)
	if final.Type != "error" || !strings.Contains(final.Error, "put back already") {
		t.Fatalf("restoring twice: %+v", final)
	}

	// Past 10,000 rows nothing is deleted.
	_, _, final = h.do(tunnel.PGRequest{ID: 4, Op: "run", SQL: "DELETE FROM argus_t_tr2", Confirm: dbname + "/argus_t_tr2"})
	if final.Type != "error" || final.Code != "AR005" {
		t.Fatalf("a delete past the limit: %+v", final)
	}
	if count("SELECT count(*) FROM argus_t_tr2") != "10001" {
		t.Fatal("the refused delete deleted")
	}
	// A typo is marked where it was typed.
	_, _, final = h.run(5, "DELETE FROM argus_t_tr WHERE nope = 1", 0)
	if final.Type != "error" || final.Position != strings.Index("DELETE FROM argus_t_tr WHERE nope = 1", "nope")+1 {
		t.Fatalf("error position: %+v", final)
	}

	// TRUNCATE keeps a copy, and putting it back fills the table again.
	_, _, final = h.do(tunnel.PGRequest{ID: 6, Op: "run", SQL: "TRUNCATE argus_t_tr", Confirm: dbname + "/argus_t_tr"})
	if final.Type != "done" || final.Tag != "TRUNCATE TABLE" || len(final.Trash) != 1 || final.Trash[0].Rows != 20 {
		t.Fatalf("truncate: %+v", final)
	}
	if count("SELECT count(*) FROM argus_t_tr") != "0" {
		t.Fatal("not emptied")
	}
	_, _, final = h.run(7, "SELECT * FROM argus_trash_fn.restore("+itoa64(final.Trash[0].ID)+")", 0)
	if final.Type != "done" || count("SELECT count(*) FROM argus_t_tr") != "20" {
		t.Fatalf("restore empty: %+v", final)
	}

	// DROP TABLE moves it into the trash, and back.
	_, _, final = h.do(tunnel.PGRequest{ID: 8, Op: "run", SQL: "DROP TABLE argus_t_tr", Confirm: dbname + "/argus_t_tr"})
	if final.Type != "done" || final.Tag != "DROP TABLE" || len(final.Trash) != 1 || final.Trash[0].Kind != "table" {
		t.Fatalf("drop: %+v", final)
	}
	if count("SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'argus_t_tr'") != "0" {
		t.Fatal("still there")
	}
	// No one's grants reach into the trash.
	_, _, final2 := h.run(9, "SELECT count(*) FROM argus_trash.argus_t_tr__"+itoa64(final.Trash[0].ID), 0)
	if final2.Type != "error" || final2.Code != "42501" {
		t.Fatalf("reading the trash directly: %+v", final2)
	}
	_, _, final = h.run(10, "SELECT * FROM argus_trash_fn.restore("+itoa64(final.Trash[0].ID)+")", 0)
	if final.Type != "done" || count("SELECT count(*) FROM argus_t_tr") != "20" {
		t.Fatalf("restore table: %+v", final)
	}

	// A drop that would take something with it is refused whole.
	if _, _, a := h.run(110, "CREATE VIEW argus_t_tr_dep AS SELECT id FROM argus_t_tr", 0); a.Type != "done" {
		t.Fatalf("view: %+v", a)
	}
	_, _, final = h.do(tunnel.PGRequest{ID: 11, Op: "run", SQL: "DROP TABLE argus_t_tr CASCADE", Confirm: dbname + "/argus_t_tr"})
	if final.Type != "error" || final.Code != "2BP01" || !strings.Contains(final.Error, "argus_t_tr_dep") {
		t.Fatalf("drop with a dependent: %+v", final)
	}
	superExec(t, dsn, "DROP VIEW argus_t_tr_dep")

	// DROP SCHEMA renames it out of reach, and back.
	_, _, final = h.do(tunnel.PGRequest{ID: 12, Op: "run", SQL: "DROP SCHEMA argus_t_tr_s", Confirm: dbname + "/argus_t_tr_s"})
	if final.Type != "error" || final.Code != "2BP01" {
		t.Fatalf("drop schema without cascade: %+v", final)
	}
	_, _, final = h.do(tunnel.PGRequest{ID: 13, Op: "run", SQL: "DROP SCHEMA argus_t_tr_s CASCADE", Confirm: dbname + "/argus_t_tr_s"})
	if final.Type != "done" || len(final.Trash) != 1 || final.Trash[0].Kind != "schema" {
		t.Fatalf("drop schema: %+v", final)
	}
	if count("SELECT count(*) FROM pg_namespace WHERE nspname = 'argus_t_tr_s'") != "0" {
		t.Fatal("schema still there")
	}
	_, _, final = h.run(14, "SELECT * FROM argus_trash_fn.restore("+itoa64(final.Trash[0].ID)+")", 0)
	if final.Type != "done" || count("SELECT count(*) FROM argus_t_tr_s.x") != "1" {
		t.Fatalf("restore schema: %+v", final)
	}
	if count("SELECT has_schema_privilege('public', 'argus_t_tr_s', 'USAGE')::text") != "true" {
		t.Fatal("the schema's grants did not come back")
	}

	// The list shows what is there.
	_, rows, final := h.run(15, "SELECT kind, source, restored_by FROM argus_trash_fn.list()", 0)
	if final.Type != "done" || len(rows) < 4 {
		t.Fatalf("list: %+v %v", final, rows)
	}
	h.close()
}

// A write level that does not own a table cannot drop it into the trash.
func TestPostgresTrashChecksTheRealRight(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	superExec(t, dsn, "DROP TABLE IF EXISTS argus_t_tr3", "CREATE TABLE argus_t_tr3 (id int)")
	h := openPG(t, tunnel.LevelWrite, nil)
	_, _, final := h.do(tunnel.PGRequest{ID: 1, Op: "run", SQL: "DROP TABLE argus_t_tr3", Confirm: dbname + "/argus_t_tr3"})
	if final.Type != "error" || final.Code != "42501" {
		t.Fatalf("drop by a non-owner: %+v", final)
	}
	if superQuery(t, dsn, "SELECT count(*) FROM pg_tables WHERE tablename = 'argus_t_tr3'")[0][0] != "1" {
		t.Fatal("dropped anyway")
	}
	h.close()
}

func itoa64(n int64) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

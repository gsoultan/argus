//go:build !windows

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// The bug this exists for: a person given DBA (or owner) on a database could
// not create an index on a table made as the postgres superuser -- "must be
// owner of table". Only an owner, or a member of the owner's role, may index
// or alter a table, and no Argus level is ever made a member of a superuser.
// Now a DBA session is handed what a superuser owns in the database as it
// opens, so a DBA can change or add anything there; owner is unchanged and
// says where to go instead.
func TestPostgresDBAAdoptsSuperuserObjects(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	drop := []string{
		"DROP SCHEMA IF EXISTS argus_t_made_su CASCADE",
		"DROP TABLE IF EXISTS argus_t_orders CASCADE",
		"DROP VIEW IF EXISTS argus_t_big",
		"DROP SEQUENCE IF EXISTS argus_t_tickets",
		"DROP FUNCTION IF EXISTS argus_t_total(numeric)",
		"DROP FUNCTION IF EXISTS argus_t_definer()",
		"DROP TYPE IF EXISTS argus_t_mood",
		"DROP DOMAIN IF EXISTS argus_t_cents",
	}
	superTry(t, dsn, drop...)
	superExec(t, dsn,
		// All made as postgres, the way psql or a migration run as postgres does.
		"CREATE SCHEMA argus_t_made_su",
		"CREATE TABLE argus_t_made_su.items (id int)",
		"CREATE TABLE argus_t_orders (id serial, total numeric)",
		"CREATE VIEW argus_t_big AS SELECT * FROM argus_t_orders WHERE total > 100",
		"CREATE SEQUENCE argus_t_tickets",
		"CREATE FUNCTION argus_t_total(numeric) RETURNS numeric LANGUAGE sql AS 'SELECT $1 * 2'",
		// A superuser's own code: never handed over.
		"CREATE FUNCTION argus_t_definer() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'",
		"CREATE TYPE argus_t_mood AS ENUM ('ok', 'sad')",
		"CREATE DOMAIN argus_t_cents AS int CHECK (VALUE >= 0)")
	t.Cleanup(func() { superTry(t, dsn, drop...) })

	const index = "CREATE INDEX argus_t_orders_total ON argus_t_orders (total)"
	// Owner: still refused, and told where to go.
	ho := openPG(t, tunnel.LevelOwner, nil)
	_, _, final := ho.run(1, index, 0)
	ho.close()
	if final.Type != "error" || final.Code != "42501" || !strings.Contains(final.Hint, "at DBA") {
		t.Fatalf("owner on a superuser's table: %+v", final)
	}

	h := openPG(t, tunnel.LevelDBA, nil)
	for i, s := range []string{
		index,
		"ALTER TABLE argus_t_orders ADD COLUMN note text",
		"ALTER TABLE argus_t_orders RENAME COLUMN note TO remark",
		"CREATE OR REPLACE VIEW argus_t_big AS SELECT * FROM argus_t_orders WHERE total > 1000",
		"ALTER SEQUENCE argus_t_tickets RESTART WITH 10",
		"CREATE OR REPLACE FUNCTION argus_t_total(numeric) RETURNS numeric LANGUAGE sql AS 'SELECT $1 * 3'",
		"ALTER TYPE argus_t_mood ADD VALUE 'glad'",
		"ALTER DOMAIN argus_t_cents SET DEFAULT 0",
		"CREATE TABLE argus_t_made_su.more (id int)",
		"CREATE INDEX argus_t_items_id ON argus_t_made_su.items (id)",
	} {
		if _, _, final := h.run(uint32(10+i), s, 0); final.Type != "done" {
			h.close()
			t.Fatalf("DBA: %s: %+v", s, final)
		}
	}
	// SECURITY DEFINER stays the superuser's: replacing it would be running
	// one's own code as postgres.
	if _, _, final := h.run(40, "CREATE OR REPLACE FUNCTION argus_t_definer() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 2'", 0); final.Type != "error" || final.Code != "42501" {
		h.close()
		t.Fatalf("a superuser's SECURITY DEFINER function was handed over: %+v", final)
	}
	h.close()

	dba := levelRole(tunnel.LevelDBA, dbname)
	got := superQuery(t, dsn, `SELECT x, pg_get_userbyid(o) FROM (
		SELECT 'orders' x, relowner o FROM pg_class WHERE relname = 'argus_t_orders'
		UNION ALL SELECT 'orders_id_seq', relowner FROM pg_class WHERE relname = 'argus_t_orders_id_seq'
		UNION ALL SELECT 'big', relowner FROM pg_class WHERE relname = 'argus_t_big'
		UNION ALL SELECT 'tickets', relowner FROM pg_class WHERE relname = 'argus_t_tickets'
		UNION ALL SELECT 'total', proowner FROM pg_proc WHERE proname = 'argus_t_total'
		UNION ALL SELECT 'definer', proowner FROM pg_proc WHERE proname = 'argus_t_definer'
		UNION ALL SELECT 'mood', typowner FROM pg_type WHERE typname = 'argus_t_mood'
		UNION ALL SELECT 'schema', nspowner FROM pg_namespace WHERE nspname = 'argus_t_made_su'
		UNION ALL SELECT 'catalog', relowner FROM pg_class WHERE relname = 'pg_class') s ORDER BY 1`)
	want := map[string]string{"orders": dba, "orders_id_seq": dba, "big": dba, "tickets": dba, "total": dba,
		"mood": dba, "schema": dba, "definer": "postgres", "catalog": "postgres"}
	for _, r := range got {
		if want[r[0]] != r[1] {
			t.Errorf("%s is owned by %s, want %s", r[0], r[1], want[r[0]])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("owners: %v", got)
	}

	// Recorded, so the control plane audits it as a change by the helper.
	var adopted string
	for _, r := range h.rec {
		if r.Kind == "admin" {
			adopted += r.SQL + "\n"
		}
	}
	for _, w := range []string{"ALTER TABLE argus_t_orders OWNER TO " + dba, "ALTER SCHEMA argus_t_made_su OWNER TO " + dba} {
		if !strings.Contains(adopted, w) {
			t.Fatalf("the recording does not say %q:\n%s", w, adopted)
		}
	}
	if strings.Contains(adopted, "argus_t_definer") {
		t.Fatalf("the definer was handed over:\n%s", adopted)
	}
	b, err := os.ReadFile(filepath.Join(h.dir, recordingName(h.dir, t)))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := tunnel.VerifyPGRecording(strings.NewReader(string(b)), ""); err != nil || !v.OK {
		t.Fatalf("the recording does not verify: %+v %v", v, err)
	}

	// The next DBA open has nothing left to hand over, and says nothing.
	h2 := openPG(t, tunnel.LevelDBA, nil)
	h2.close()
	for _, r := range h2.rec {
		if r.Kind == "admin" {
			t.Fatalf("a second open handed over again: %s", r.SQL)
		}
	}
}

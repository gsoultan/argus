//go:build !windows

package agent

import (
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A GRANT the session's role may not give is not an error to Postgres: it
// ends with the tag "GRANT" and a WARNING (01007) that nothing was granted.
// The agent dropped notices, so the console showed that GRANT as a success
// while the grantee got nothing. The warning now comes with the answer and is
// recorded beside it.
func TestPostgresGrantWithoutRightsCarriesTheWarning(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	superExec(t, dsn,
		"DROP SCHEMA IF EXISTS argus_t_etl CASCADE",
		"DROP ROLE IF EXISTS ana_sari",
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'argus_t_hermod') THEN CREATE ROLE argus_t_hermod; END IF; END $$",
		// Made by the superuser, as a schema made from psql as postgres is:
		// a superuser's objects are never handed to an Argus level.
		"CREATE SCHEMA argus_t_etl",
		"CREATE TABLE argus_t_etl.job (id int)")
	t.Cleanup(func() {
		superExec(t, dsn, "DROP SCHEMA IF EXISTS argus_t_etl CASCADE",
			"DROP OWNED BY argus_t_hermod", "DROP ROLE IF EXISTS argus_t_hermod")
	})

	h := openPG(t, tunnel.LevelOwner, nil)
	defer h.close()
	for i, s := range []string{
		"GRANT USAGE ON SCHEMA argus_t_etl TO argus_t_hermod",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA argus_t_etl TO argus_t_hermod",
		"GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA argus_t_etl TO argus_t_hermod",
	} {
		id := uint32(1 + i)
		_, _, final := h.run(id, s, 0)
		if final.Type != "done" || final.Tag != "GRANT" {
			t.Fatalf("%s: Postgres ends it as done: %+v", s, final)
		}
		if len(final.Notices) == 0 || final.Notices[0].Code != tunnel.NoGrantCode || final.Notices[0].Severity != "WARNING" {
			t.Fatalf("%s: the answer must carry Postgres's warning that nothing was granted: %+v", s, final.Notices)
		}
		var rec *tunnel.PGRecord
		for i := range h.rec {
			if h.rec[i].Kind == "done" && h.rec[i].Request == id {
				rec = &h.rec[i]
			}
		}
		if rec == nil || len(rec.Notices) == 0 || rec.Notices[0].Code != tunnel.NoGrantCode {
			t.Fatalf("%s: the recording must keep the warning: %+v", s, rec)
		}
	}
	// And indeed nothing changed, which is what the warning says.
	got := superQuery(t, dsn, `SELECT has_schema_privilege('argus_t_hermod', 'argus_t_etl', 'USAGE')::text,
		has_table_privilege('argus_t_hermod', 'argus_t_etl.job', 'SELECT')::text`)
	if got[0][0] != "false" || got[0][1] != "false" {
		t.Fatalf("hermod's privileges: %v", got)
	}

	// A statement with nothing to say carries nothing, and a later statement
	// is not told what an earlier one was.
	if _, _, final := h.run(10, "SELECT 1", 0); final.Type != "done" || len(final.Notices) != 0 {
		t.Fatalf("SELECT 1: %+v", final)
	}
}

// A DO block can RAISE NOTICE without end; what one statement keeps is
// bounded, and the last kept notice says how many more there were.
func TestPostgresNoticesAreBounded(t *testing.T) {
	pgTest(t)
	h := openPG(t, tunnel.LevelRead, nil)
	defer h.close()
	_, _, final := h.run(1, "DO $$ BEGIN FOR i IN 1..50 LOOP RAISE NOTICE 'n %', i; END LOOP; END $$", 0)
	if final.Type != "done" || len(final.Notices) != tunnel.MaxPGNotices {
		t.Fatalf("%+v", final)
	}
	last := final.Notices[len(final.Notices)-1]
	if !strings.Contains(last.Detail, "30 more notices") {
		t.Fatalf("the last notice must say what was dropped: %+v", last)
	}
}

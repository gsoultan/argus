//go:build !windows

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A script pasted into the console arrives one statement per request, cut by
// the console's splitSQL (web/src/lib/sqlSplit.ts, whose test reads the same
// files). Sent whole it is refused: the extended protocol takes one
// statement. Sent in turn on one session, its BEGIN, temp tables and
// pg_temp functions last until its ROLLBACK, each statement is recorded,
// and nothing it deleted stays deleted.
func TestPostgresScriptRunsStatementByStatement(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	superExec(t, dsn,
		"DROP TABLE IF EXISTS argus_t_widget",
		"CREATE TABLE argus_t_widget (id int PRIMARY KEY, v text)",
		"INSERT INTO argus_t_widget VALUES (1, 'a'), (2, 'b')",
		"DROP ROLE IF EXISTS ana_sari")
	script, err := os.ReadFile("testdata/script.sql")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/script.statements.json")
	if err != nil {
		t.Fatal(err)
	}
	var stmts []struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal(raw, &stmts); err != nil {
		t.Fatal(err)
	}
	h := openPG(t, tunnel.LevelWrite, nil)

	// The bug: the whole file as one request.
	if _, _, final := h.run(1, string(script), 0); final.Type != "error" || final.Code != "42601" {
		t.Fatalf("a whole script in one request must be Postgres's refusal: %+v", final)
	}

	temp := regexp.MustCompile(`^pg_temp_\d+\.`)
	var preview [][]*string
	for i, st := range stmts {
		id := uint32(100 + i)
		if st.SQL == "INSERT INTO purge_row (tbl, id)\nSELECT 'argus_t_widget'::regclass, w.id FROM argus_t_widget w WHERE w.id IN (SELECT id FROM input_widget)" {
			// The console checks a write before it runs; inside the script's
			// transaction that is a savepoint, and what it touched is only
			// the session's temp table, so the console goes on unasked.
			_, _, c := h.do(tunnel.PGRequest{ID: id + 1000, Op: "check", SQL: st.SQL})
			if c.Type != "check" || c.Check == nil || c.Check.Rows != 1 || len(c.Check.Tables) != 1 || !temp.MatchString(c.Check.Tables[0].Name) {
				t.Fatalf("check of the temp insert: %+v %+v", c, c.Check)
			}
		}
		cols, rows, final := h.run(id, st.SQL, 0)
		if final.Type != "done" {
			t.Fatalf("statement %d (%s): %+v", i+1, st.SQL, final)
		}
		if st.SQL == "SELECT * FROM pg_temp.purge_delete()" {
			if len(cols) != 2 || cols[0].Name != "table_name" {
				t.Fatalf("purge_delete columns: %+v", cols)
			}
			preview = rows
		}
		if i == len(stmts)-2 {
			// Before the ROLLBACK, inside the transaction, the row is gone.
			if _, r, f := h.run(id+500, "SELECT count(*) FROM argus_t_widget", 0); f.Type != "done" || *r[0][0] != "1" {
				t.Fatalf("inside the transaction: %+v %v", f, r)
			}
		}
	}
	if len(preview) != 1 || *preview[0][0] != "argus_t_widget" || *preview[0][1] != "1" {
		t.Fatalf("purge_delete answered %v", preview)
	}
	if got := superQuery(t, dsn, "SELECT count(*) FROM argus_t_widget"); got[0][0] != "2" {
		t.Fatalf("the script ended in ROLLBACK; nothing may be deleted: %v", got)
	}
	// The temp table went with the transaction (ON COMMIT DROP).
	if _, _, f := h.run(900, "SELECT 1 FROM input_widget", 0); f.Type != "error" || f.Code != "42P01" {
		t.Fatalf("temp table after ROLLBACK: %+v", f)
	}

	// Every statement is in the recording, in order, with its own answer.
	h.close()
	var got []string
	for _, r := range h.rec {
		if r.Kind == "stmt" && r.Request >= 100 && r.Request < 100+uint32(len(stmts)) {
			got = append(got, r.SQL)
		}
	}
	if len(got) != len(stmts) {
		t.Fatalf("recorded %d statements of %d", len(got), len(stmts))
	}
	for i, st := range stmts {
		if got[i] != st.SQL {
			t.Fatalf("statement %d recorded as %q", i+1, got[i])
		}
	}
	f, err := os.Open(filepath.Join(h.dir, h.ready.Session+".argussql"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if v, err := tunnel.VerifyPGRecording(f, ""); err != nil || !v.OK {
		t.Fatalf("the recording: %+v %v", v, err)
	}
}

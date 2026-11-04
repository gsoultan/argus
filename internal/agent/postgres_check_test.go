package agent

import (
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A DELETE of every row, a TRUNCATE and a DROP stop before Postgres sees
// them, and are recorded as asked and refused; the typed phrase lets them
// through; DROP DATABASE never runs.
func TestPostgresGuardStopsAccidents(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	superExec(t, dsn,
		"DROP TABLE IF EXISTS argus_t_g",
		"CREATE TABLE argus_t_g (id int PRIMARY KEY, v text)",
		"INSERT INTO argus_t_g SELECT g, 'x' FROM generate_series(1, 5) g")
	h := openPG(t, tunnel.LevelOwner, nil)

	_, _, final := h.run(1, "DELETE FROM argus_t_g", 0)
	if final.Type != "error" || final.Code != tunnel.GuardNoWhere || final.Guard == nil {
		t.Fatalf("delete without where: %+v", final)
	}
	if got := superQuery(t, dsn, "SELECT count(*) FROM argus_t_g"); got[0][0] != "5" {
		t.Fatalf("the refused delete ran: %v rows left", got)
	}
	want := dbname + "/argus_t_g"
	if final.Guard.Confirm != want {
		t.Fatalf("confirm phrase %q, want %q", final.Guard.Confirm, want)
	}
	// Recorded: the statement, then the refusal, under the same request.
	var sawStmt, sawRefusal bool
	for _, r := range h.rec {
		if r.Request == 1 && r.Kind == "stmt" {
			sawStmt = true
		}
		if r.Request == 1 && r.Kind == "error" && r.Code == tunnel.GuardNoWhere {
			sawRefusal = true
		}
	}
	if !sawStmt || !sawRefusal {
		t.Fatalf("the refusal is not in the recording: %+v", h.rec)
	}

	_, _, final = h.do(tunnel.PGRequest{ID: 2, Op: "run", SQL: "DELETE FROM argus_t_g", Confirm: "wrong/argus_t_g"})
	if final.Code != tunnel.GuardNoWhere {
		t.Fatalf("a wrong phrase let it run: %+v", final)
	}
	_, _, final = h.run(3, "TRUNCATE argus_t_g", 0)
	if final.Code != tunnel.GuardDrop {
		t.Fatalf("truncate: %+v", final)
	}
	_, _, final = h.run(4, "DROP TABLE argus_t_g", 0)
	if final.Code != tunnel.GuardDrop {
		t.Fatalf("drop: %+v", final)
	}
	_, _, final = h.do(tunnel.PGRequest{ID: 5, Op: "run", SQL: "DROP DATABASE " + dbname, Confirm: dbname + "/" + dbname})
	if final.Code != tunnel.GuardDropDB {
		t.Fatalf("drop database: %+v", final)
	}
	// With the phrase, Postgres decides, and an owner may.
	_, _, final = h.do(tunnel.PGRequest{ID: 6, Op: "run", SQL: "DELETE FROM argus_t_g", Confirm: want})
	if final.Type != "done" || final.Rows != 5 {
		t.Fatalf("confirmed delete: %+v", final)
	}
	h.close()
}

// A check runs the statement as the person's role, counts it, cascades
// included, and leaves nothing behind, inside their transaction or not.
func TestPostgresCheckCountsAndRollsBack(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	superExec(t, dsn,
		"DROP TABLE IF EXISTS argus_t_ck_items, argus_t_ck_orders",
		"CREATE TABLE argus_t_ck_orders (id int PRIMARY KEY, status text)",
		"CREATE TABLE argus_t_ck_items (id int PRIMARY KEY, order_id int REFERENCES argus_t_ck_orders ON DELETE CASCADE)",
		"INSERT INTO argus_t_ck_orders SELECT g, 'pending' FROM generate_series(1, 10) g",
		"INSERT INTO argus_t_ck_items SELECT g, 1 + g % 2 FROM generate_series(1, 6) g",
		"ANALYZE argus_t_ck_orders")
	h := openPG(t, tunnel.LevelWrite, nil)

	_, _, final := h.do(tunnel.PGRequest{ID: 1, Op: "check", SQL: "DELETE FROM argus_t_ck_orders WHERE id IN (1, 2)"})
	if final.Type != "check" || final.Check == nil || final.Check.Rows != 2 {
		t.Fatalf("check: %+v", final)
	}
	byName := map[string]tunnel.PGCheckTable{}
	for _, tb := range final.Check.Tables {
		byName[tb.Name] = tb
	}
	if byName["public.argus_t_ck_orders"].Deleted != 2 || byName["public.argus_t_ck_items"].Deleted != 6 {
		t.Fatalf("cascade not counted: %+v", final.Check.Tables)
	}
	if final.Check.Guard != nil {
		t.Fatalf("a delete with a where is not guarded: %+v", final.Check.Guard)
	}
	if got := superQuery(t, dsn, "SELECT (SELECT count(*) FROM argus_t_ck_orders), (SELECT count(*) FROM argus_t_ck_items)"); got[0][0] != "10" || got[0][1] != "6" {
		t.Fatalf("the check left changes behind: %v", got)
	}

	// Without a where: counted, and the answer says the run would be stopped.
	_, _, final = h.do(tunnel.PGRequest{ID: 2, Op: "check", SQL: "UPDATE argus_t_ck_orders SET status = 'x'"})
	if final.Check == nil || final.Check.Rows != 10 || final.Check.Guard == nil || final.Check.Guard.Code != tunnel.GuardNoWhere {
		t.Fatalf("check without where: %+v", final)
	}

	// Inside the person's own transaction: a savepoint, and their work stays.
	for i, s := range []string{"BEGIN", "UPDATE argus_t_ck_orders SET status = 'mine' WHERE id = 3"} {
		if _, _, f := h.run(uint32(10+i), s, 0); f.Type != "done" {
			t.Fatalf("%s: %+v", s, f)
		}
	}
	_, _, final = h.do(tunnel.PGRequest{ID: 12, Op: "check", SQL: "DELETE FROM argus_t_ck_orders WHERE id = 4"})
	if final.Check == nil || final.Check.Rows != 1 {
		t.Fatalf("check in a transaction: %+v", final)
	}
	for _, tb := range final.Check.Tables {
		if tb.Name == "public.argus_t_ck_orders" && (tb.Updated != 0 || tb.Deleted != 1) {
			t.Fatalf("their own earlier update was counted as the check's: %+v", tb)
		}
	}
	_, rows, f := h.run(13, "SELECT status FROM argus_t_ck_orders WHERE id = 3", 0)
	if f.Type != "done" || len(rows) != 1 || *rows[0][0] != "mine" {
		t.Fatalf("their transaction did not survive the check: %+v %v", f, rows)
	}
	if _, _, f = h.run(14, "SELECT count(*) FROM argus_t_ck_orders WHERE id = 4", 0); f.Type != "done" {
		t.Fatalf("%+v", f)
	}
	if _, _, f = h.run(15, "ROLLBACK", 0); f.Type != "done" {
		t.Fatalf("rollback: %+v", f)
	}

	// Only writes are checked.
	_, _, final = h.do(tunnel.PGRequest{ID: 20, Op: "check", SQL: "SELECT 1"})
	if final.Code != tunnel.GuardCheckOnly {
		t.Fatalf("check of a select: %+v", final)
	}
	// Recorded as a check, with a rolled-back done line.
	var sawCheck, sawDone bool
	for _, r := range h.rec {
		if r.Request == 1 && r.Kind == "check" {
			sawCheck = true
		}
		if r.Request == 1 && r.Kind == "done" && r.Tag == "ROLLED BACK DELETE 2" {
			sawDone = true
		}
	}
	if !sawCheck || !sawDone {
		t.Fatalf("check not recorded: %+v", h.rec)
	}
	h.close()
}

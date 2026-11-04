package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestChangeOf(t *testing.T) {
	cl := tunnel.ClassifySQL
	for _, tc := range []struct {
		sql, tag, verb string
		rows       int64
		objects    string
	}{
		{"INSERT INTO public.orders (id) VALUES (1)", "INSERT 0 1", "insert", 1, "public.orders"},
		{`insert into "Sales"."Orders" values (1), (2)`, "INSERT 0 2", "insert", 2, `"Sales"."Orders"`},
		{"UPDATE orders SET a = 1 WHERE id = 2", "UPDATE 3", "update", 3, "orders"},
		{"DELETE FROM orders WHERE id = 2", "DELETE 1", "delete", 1, "orders"},
		{"TRUNCATE orders", "TRUNCATE TABLE", "truncate", -1, "orders"},
		{"DROP TABLE orders", "DROP TABLE", "drop", -1, "orders"},
		{"SELECT * FROM argus_trash_fn.restore($1::bigint)", "SELECT 1", "restore", -1, ""},
		{"WITH d AS (DELETE FROM t WHERE x RETURNING *) SELECT count(*) FROM d", "SELECT 1", "write", -1, ""},
	} {
		c := changeOf(tc.sql, tc.tag, cl(tc.sql), nil)
		if c == nil || c.Verb != tc.verb || c.Rows != tc.rows || strings.Join(c.Objects, ",") != tc.objects {
			t.Errorf("%s: %+v", tc.sql, c)
		}
	}
	for sql, tag := range map[string]string{
		"UPDATE orders SET a = 1 WHERE false": "UPDATE 0",
		"SELECT * FROM orders":                "SELECT 2",
		"CREATE INDEX ON orders (a)":          "CREATE INDEX",
	} {
		if c := changeOf(sql, tag, cl(sql), nil); c != nil {
			t.Errorf("%s counted as a change: %+v", sql, c)
		}
	}
}

// Inside a transaction a change waits for COMMIT, and a rollback (or one to
// a savepoint taken before it) forgets it.
func TestChangeTrackerFollowsTransactions(t *testing.T) {
	tr := &changeTracker{sent: map[uint32]sentStmt{}}
	n := uint32(0)
	step := func(sql, tag string) []DataChange {
		n++
		return tr.finished(sentStmt{sql: sql, class: tunnel.ClassifySQL(sql)}, tag, nil)
	}
	if got := step("DELETE FROM t WHERE id = 1", "DELETE 1"); len(got) != 1 {
		t.Fatalf("autocommit: %+v", got)
	}
	step("BEGIN", "BEGIN")
	if got := step("UPDATE t SET a = 1 WHERE id = 1", "UPDATE 1"); got != nil {
		t.Fatalf("written before COMMIT: %+v", got)
	}
	step("SAVEPOINT argus_row_edit", "SAVEPOINT")
	step("UPDATE t SET a = 2 WHERE id = 2", "UPDATE 1")
	step("ROLLBACK TO SAVEPOINT argus_row_edit", "ROLLBACK")
	step("RELEASE SAVEPOINT argus_row_edit", "RELEASE")
	step("INSERT INTO t VALUES (3)", "INSERT 0 1")
	got := step("COMMIT", "COMMIT")
	if len(got) != 2 || got[0].Verb != "update" || got[1].Verb != "insert" {
		t.Fatalf("commit: %+v", got)
	}
	step("BEGIN", "BEGIN")
	step("DELETE FROM t WHERE id = 3", "DELETE 1")
	if got := step("ROLLBACK", "ROLLBACK"); got != nil {
		t.Fatalf("rolled back, still written: %+v", got)
	}
	step("BEGIN", "BEGIN")
	step("DELETE FROM t WHERE id = 3", "DELETE 1")
	// A failed transaction's COMMIT answers ROLLBACK.
	if got := step("COMMIT", "ROLLBACK"); got != nil {
		t.Fatalf("a failed transaction was written: %+v", got)
	}
}

// What is committed goes to the log, and to admins and the database's people
// as one alert, never to the person who did it.
func TestDataChangesAreLoggedAndTold(t *testing.T) {
	a, s, h, db, admin := dbFixture(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	bo, _ := newAccount(t, s, "operator")
	cy, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"], a.UserTokens["bo"], a.UserTokens["cy"] = ana, bo, cy
	team, err := s.CreateTeam(ctx, unique("dba team"), []string{bo}, admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{fmt.Sprintf(`{"userEmail":%q,"level":"write"}`, ana), fmt.Sprintf(`{"teamId":%q,"level":"read"}`, team)} {
		if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token", body); w.Code >= 300 {
			t.Fatalf("assign: %d %s", w.Code, w.Body)
		}
	}

	tr := newChangeTracker(ctx, a, ana, db, "s-1")
	run := func(id uint32, sql, tag string) {
		tr.statement(tunnel.PGRequest{ID: id, Op: "run", SQL: sql})
		tr.mu.Lock()
		st := tr.sent[id]
		delete(tr.sent, id)
		out := tr.finished(st, tag, nil)
		tr.mu.Unlock()
		if len(out) > 0 {
			tr.write(out)
		}
	}
	run(1, "BEGIN", "BEGIN")
	run(2, "UPDATE public.orders SET note = $1 WHERE id = $2", "UPDATE 1")
	run(3, "DELETE FROM public.orders WHERE id = 9", "DELETE 2")
	run(4, "COMMIT", "COMMIT")

	w := do(t, h, http.MethodGet, "/api/v1/databases/"+db.ID+"/changes", "ana", "")
	var out struct{ Changes []DataChange }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || len(out.Changes) != 2 || out.Changes[0].Verb != "delete" || out.Changes[0].Rows != 2 ||
		out.Changes[1].SQL != "UPDATE public.orders SET note = $1 WHERE id = $2" || out.Changes[1].Actor != strings.ToLower(ana) {
		t.Fatalf("log: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/databases/"+db.ID+"/changes", "cy", ""); w.Code != http.StatusForbidden {
		t.Fatalf("someone without the database read its log: %d", w.Code)
	}

	alerts := func(tok string) []ConsoleAlert {
		w := do(t, h, http.MethodGet, "/api/v1/alerts", tok, "")
		var page struct{ Alerts []ConsoleAlert }
		_ = json.Unmarshal(w.Body.Bytes(), &page)
		var mine []ConsoleAlert
		for _, al := range page.Alerts {
			if al.Kind == AlertData && strings.Contains(al.Title, db.Name) {
				mine = append(mine, al)
			}
		}
		return mine
	}
	if got := alerts("bo"); len(got) != 1 || got[0].Detail != "Updated 1 row in public.orders; Deleted 2 rows from public.orders." {
		t.Fatalf("the team member was not told once: %+v", got)
	}
	if got := alerts("admin-token"); len(got) != 1 {
		t.Fatalf("the admin was not told once: %+v", got)
	}
	if got := alerts("ana"); len(got) != 0 {
		t.Fatalf("the person who did it was told: %+v", got)
	}
	if got := alerts("cy"); len(got) != 0 {
		t.Fatalf("someone without the database was told: %+v", got)
	}
}

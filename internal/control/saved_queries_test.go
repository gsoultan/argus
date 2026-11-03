package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// A saved query is private until shared, only its author changes it, and
// it never reaches someone without access to the database.
func TestSavedQueriesStayWithTheirDatabaseAndAuthor(t *testing.T) {
	a, s, h, db, _ := dbFixture(t)
	ana, _ := newAccount(t, s, "operator")
	bo, _ := newAccount(t, s, "operator")
	cy, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"], a.UserTokens["bo"], a.UserTokens["cy"] = ana, bo, cy
	base := "/api/v1/databases/" + db.ID + "/saved-queries"
	for _, who := range []string{ana, bo} {
		if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
			fmt.Sprintf(`{"userEmail":%q,"level":"read"}`, who)); w.Code >= 300 {
			t.Fatalf("assign: %d %s", w.Code, w.Body)
		}
	}
	list := func(tok string) (int, []SavedQuery) {
		w := do(t, h, http.MethodGet, base, tok, "")
		var out struct{ Queries []SavedQuery }
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Queries
	}

	// Without access there is nothing to list or save.
	if code, _ := list("cy"); code != http.StatusForbidden {
		t.Fatalf("an unassigned person listed saved queries: %d", code)
	}
	if w := do(t, h, http.MethodPost, base, "cy", `{"name":"x","sql":"select 1"}`); w.Code != http.StatusForbidden {
		t.Fatalf("an unassigned person saved a query: %d", w.Code)
	}

	w := do(t, h, http.MethodPost, base, "ana", `{"name":"Unshipped","sql":"select * from orders"}`)
	var q SavedQuery
	_ = json.Unmarshal(w.Body.Bytes(), &q)
	if w.Code != http.StatusCreated || q.ID == "" || q.Shared || !q.Mine {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, base, "ana", `{"name":" ","sql":"select 1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a nameless query was saved: %d", w.Code)
	}
	if _, l := list("bo"); len(l) != 0 {
		t.Fatalf("a private query was listed to someone else: %+v", l)
	}

	// Bo can neither change nor remove it.
	if w := do(t, h, http.MethodPut, base+"/"+q.ID, "bo", `{"shared":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("someone else shared it: %d", w.Code)
	}
	if w := do(t, h, http.MethodDelete, base+"/"+q.ID, "bo", ""); w.Code != http.StatusNotFound {
		t.Fatalf("someone else removed it: %d", w.Code)
	}

	// Shared, Bo sees it but still cannot change it.
	if w := do(t, h, http.MethodPut, base+"/"+q.ID, "ana", `{"shared":true}`); w.Code != http.StatusOK {
		t.Fatalf("share: %d %s", w.Code, w.Body)
	}
	if _, l := list("bo"); len(l) != 1 || l[0].Mine || l[0].Owner != ana || l[0].SQL != "select * from orders" {
		t.Fatalf("shared list: %+v", l)
	}
	if w := do(t, h, http.MethodPut, base+"/"+q.ID, "bo", `{"sql":"delete from orders"}`); w.Code != http.StatusNotFound {
		t.Fatalf("someone else rewrote a shared query: %d", w.Code)
	}

	// An admin may take a shared query down; it is in the audit log.
	if w := do(t, h, http.MethodDelete, base+"/"+q.ID, "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("admin remove: %d %s", w.Code, w.Body)
	}
	if _, l := list("ana"); len(l) != 0 {
		t.Fatalf("still listed: %+v", l)
	}
	if !auditHas(t, s, "db.query_removed") || !auditHas(t, s, "db.query_saved") {
		t.Fatal("saving and removing are not in the audit log")
	}
}

func auditHas(t *testing.T, s *Store, action string) bool {
	t.Helper()
	events, err := s.AuditEvents(context.Background(), 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == action {
			return true
		}
	}
	return false
}

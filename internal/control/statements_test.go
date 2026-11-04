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

// On production a statement that changes the database runs only under an
// approved ask for exactly it, once, from the person who asked.
func TestStatementApprovalOnProduction(t *testing.T) {
	a, s, h, db, admin := dbFixture(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	bo, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"], a.UserTokens["bo"] = ana, bo
	for _, who := range []string{ana, bo} {
		if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
			fmt.Sprintf(`{"userEmail":%q,"level":"owner"}`, who)); w.Code >= 300 {
			t.Fatalf("assign: %d %s", w.Code, w.Body)
		}
	}
	del := "DELETE FROM orders WHERE id = $1"
	seven := "7"
	run := func(who, sql, approval string, params ...*string) *tunnel.PGAnswer {
		return a.statementGate(ctx, who, db, tunnel.LevelOwner,
			tunnel.PGRequest{ID: 1, Op: "run", SQL: sql, Params: params, Approval: approval})
	}

	// Reading needs nothing; a change needs an approval, even at owner.
	if no := run(ana, "SELECT * FROM orders", ""); no != nil {
		t.Fatalf("a read was stopped: %+v", no)
	}
	if no := run(ana, del, "", &seven); no == nil || no.Code != tunnel.GuardNeedsApproval {
		t.Fatalf("a delete ran without approval: %+v", no)
	}
	// What the guard stops is left to the agent, so no approval is used on it.
	if no := run(ana, "DELETE FROM orders", ""); no != nil {
		t.Fatalf("a guarded statement was answered here: %+v", no)
	}

	base := "/api/v1/databases/" + db.ID + "/statements"
	if w := do(t, h, http.MethodPost, base, "ana", `{"items":[{"sql":"DELETE FROM orders WHERE id = $1","params":["7"]}],"justification":"short"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an ask without a reason: %d", w.Code)
	}
	w := do(t, h, http.MethodPost, base, "ana",
		`{"items":[{"sql":"DELETE FROM orders WHERE id = $1","params":["7"]}],"summary":{"rows":1},"justification":"order 7 was entered twice by the import"}`)
	var ask StatementAsk
	_ = json.Unmarshal(w.Body.Bytes(), &ask)
	if w.Code != http.StatusCreated || ask.RequestID == "" || ask.State != "pending" || len(ask.Items) != 1 {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	if no := run(ana, del, ask.RequestID, &seven); no == nil || !strings.Contains(no.Error, "waiting") {
		t.Fatalf("ran before the decision: %+v", no)
	}
	// Only the asker reads how it stands.
	if w := do(t, h, http.MethodGet, base+"/"+ask.RequestID, "bo", ""); w.Code != http.StatusNotFound {
		t.Fatalf("someone else read the ask: %d", w.Code)
	}
	// The approver sees the statement on the request.
	reqs, err := s.Requests(ctx, "pending", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range reqs {
		if r.ID == ask.RequestID {
			seen = r.Statement != nil && len(r.Statement.Items) == 1 && r.Statement.Items[0].SQL == del &&
				r.Statement.DatabaseName == "orders"
		}
	}
	if !seen {
		t.Fatalf("the approver cannot see what they approve: %+v", reqs)
	}
	if _, err := s.DecideRequest(ctx, ask.RequestID, "approved", "", ana); err == nil {
		t.Fatal("approved their own statement")
	}
	if _, err := s.DecideRequest(ctx, ask.RequestID, "approved", "", admin); err != nil {
		t.Fatal(err)
	}
	// The approval raises no level.
	if acc, err := s.DatabaseAccess(ctx, ana, db.ID); err != nil || acc.Level != tunnel.LevelOwner {
		t.Fatalf("access after the approval: %+v %v", acc, err)
	}

	// Someone else, another value, another statement: refused.
	if no := run(bo, del, ask.RequestID, &seven); no == nil || !strings.Contains(no.Error, "someone else") {
		t.Fatalf("another person used it: %+v", no)
	}
	eight := "8"
	if no := run(ana, del, ask.RequestID, &eight); no == nil || !strings.Contains(no.Error, "not the one") {
		t.Fatalf("another value ran: %+v", no)
	}
	// Exactly it runs, once.
	if no := run(ana, del, ask.RequestID, &seven); no != nil {
		t.Fatalf("the approved statement was stopped: %+v", no)
	}
	if no := run(ana, del, ask.RequestID, &seven); no == nil || !strings.Contains(no.Error, "already ran") {
		t.Fatalf("ran twice: %+v", no)
	}
	if no := run(ana, del, "not-a-uuid", &seven); no == nil || no.Code != tunnel.GuardNeedsApproval {
		t.Fatalf("a made-up approval: %+v", no)
	}
	w = do(t, h, http.MethodGet, base+"/"+ask.RequestID, "ana", "")
	_ = json.Unmarshal(w.Body.Bytes(), &ask)
	if w.Code != http.StatusOK || ask.State != "approved" || !ask.Items[0].Used {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
	if !auditHas(t, s, "db.statement_requested") {
		t.Fatal("the ask is not in the audit log")
	}

	// Off production nothing is asked.
	db.Label = "staging"
	if no := run(ana, del, "", &seven); no != nil {
		t.Fatalf("staging was stopped: %+v", no)
	}
}

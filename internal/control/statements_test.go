package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
			&tunnel.PGRequest{ID: 1, Op: "run", SQL: sql, Params: params, Approval: approval})
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

// The switch on a database decides whether its changes wait, whatever its
// label: production can turn it off, staging can turn it on.
func TestWriteApprovalFollowsTheSwitch(t *testing.T) {
	a, s, h, db, _ := dbFixture(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"] = ana
	del := "DELETE FROM orders WHERE id = 7"
	gate := func(d Database) *tunnel.PGAnswer {
		return a.statementGate(ctx, ana, d, tunnel.LevelWrite, &tunnel.PGRequest{ID: 1, Op: "run", SQL: del})
	}
	patch := func(body string) Database {
		t.Helper()
		w := do(t, h, http.MethodPatch, "/api/v1/databases/"+db.ID, "admin-token", body)
		if w.Code != http.StatusOK {
			t.Fatalf("patch %s: %d %s", body, w.Code, w.Body)
		}
		var out Database
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if no := gate(db); no == nil || !strings.Contains(no.Error, "is production") {
		t.Fatalf("production's default: %+v", no)
	}
	off := patch(`{"policy":{"writeApproval":false}}`)
	if no := gate(off); no != nil {
		t.Fatalf("production with the switch off: %+v", no)
	}
	if !auditHas(t, s, "db.write_approval_off") {
		t.Fatal("turning approval off is not in the audit log")
	}
	stg := patch(`{"label":"staging","policy":{}}`)
	if no := gate(stg); no != nil {
		t.Fatalf("staging's default: %+v", no)
	}
	on := patch(`{"policy":{"writeApproval":true}}`)
	if no := gate(on); no == nil || !strings.Contains(no.Error, "asks for approval of changes") {
		t.Fatalf("staging with the switch on: %+v", no)
	}
	w := do(t, h, http.MethodPatch, "/api/v1/databases/"+db.ID, "admin-token", `{"policy":{"writeApprovers":"anyone"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown approvers value: %d", w.Code)
	}
}

// Where a database's editors approve each other's changes, one with Write
// may decide another's, never their own, and a reader may not.
func TestEditorsApproveEachOther(t *testing.T) {
	a, s, h, db, _ := dbFixture(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	bo, _ := newAccount(t, s, "operator")
	cy, _ := newAccount(t, s, "operator")
	dee, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"], a.UserTokens["bo"], a.UserTokens["cy"], a.UserTokens["dee"] = ana, bo, cy, dee
	assign := func(who, body string) {
		t.Helper()
		if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
			fmt.Sprintf(`{"userEmail":%q,%s}`, who, body)); w.Code >= 300 {
			t.Fatalf("assign: %d %s", w.Code, w.Body)
		}
	}
	assign(ana, `"level":"write"`)
	assign(bo, `"level":"write"`)
	assign(cy, `"level":"read"`)
	assign(dee, `"level":"owner","needsApproval":true`)

	ask := func() StatementAsk {
		t.Helper()
		w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana",
			`{"items":[{"sql":"DELETE FROM orders WHERE id = 2","rows":1}],"summary":{"rows":1},"justification":"order 2 was entered twice by the import"}`)
		var out StatementAsk
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != http.StatusCreated {
			t.Fatalf("ask: %d %s", w.Code, w.Body)
		}
		return out
	}
	decide := func(who, id string) int {
		return do(t, h, http.MethodPost, "/api/v1/requests/"+id+"/decision", who, `{"decision":"approved"}`).Code
	}

	// By default only the organization's deciders decide.
	first := ask()
	if code := decide("bo", first.RequestID); code != http.StatusForbidden {
		t.Fatalf("an editor decided under the default: %d", code)
	}

	w := do(t, h, http.MethodPatch, "/api/v1/databases/"+db.ID, "admin-token", `{"policy":{"writeApprovers":"editors"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	editors, err := s.DatabaseEditors(ctx, db.ID)
	if err != nil {
		t.Fatal(err)
	}
	// dee's Owner needs approval to open a session, so dee is not an editor
	// until that is approved.
	if strings.Join(editors, ",") != strings.Join(sortedLower(ana, bo), ",") {
		t.Fatalf("editors: %v", editors)
	}
	w = do(t, h, http.MethodGet, "/api/v1/databases/"+db.ID+"/approval", "bo", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"approvers":"editors"`) {
		t.Fatalf("approval: %d %s", w.Code, w.Body)
	}

	// bo sees ana's change on Approvals, marked as theirs to decide.
	w = do(t, h, http.MethodGet, "/api/v1/requests?state=pending", "bo", "")
	var reqs []AccessRequest
	_ = json.Unmarshal(w.Body.Bytes(), &reqs)
	var seen bool
	for _, r := range reqs {
		if r.ID == first.RequestID {
			seen = r.AsEditor && r.Statement != nil
		}
	}
	if !seen {
		t.Fatalf("an editor cannot see the change: %s", w.Body)
	}
	// A reader sees none of it.
	w = do(t, h, http.MethodGet, "/api/v1/requests?state=pending", "cy", "")
	if strings.Contains(w.Body.String(), first.RequestID) {
		t.Fatal("a reader sees an editor's change")
	}
	if code := decide("cy", first.RequestID); code != http.StatusForbidden {
		t.Fatalf("a reader decided: %d", code)
	}
	if code := decide("dee", first.RequestID); code != http.StatusForbidden {
		t.Fatalf("an editor whose access needs approval decided: %d", code)
	}
	if code := decide("ana", first.RequestID); code != http.StatusForbidden {
		t.Fatalf("the asker decided their own: %d", code)
	}
	if code := decide("bo", first.RequestID); code != http.StatusOK {
		t.Fatalf("an editor could not decide: %d", code)
	}
	if !auditHas(t, s, "request.approve") {
		t.Fatal("the decision is not in the audit log")
	}
	// The approval carries the cap.
	rowCap, err := s.UseStatementApproval(ctx, first.RequestID, ana, db.ID, "DELETE FROM orders WHERE id = 2", nil)
	if err != nil || rowCap == nil || *rowCap != 1 {
		t.Fatalf("the cap: %v %v", rowCap, err)
	}
	// An editor decides changes only, not session requests.
	if _, dbID, err := s.requestDatabase(ctx, first.RequestID); err != nil || dbID != db.ID {
		t.Fatalf("request database: %q %v", dbID, err)
	}
}

func sortedLower(xs ...string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strings.ToLower(x)
	}
	sort.Strings(out)
	return out
}

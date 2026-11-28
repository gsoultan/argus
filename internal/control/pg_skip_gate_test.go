package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Skipping a subscription's failed transaction throws that transaction away
// for good, so on production it waits for approval of its own text, runs
// once under it, and never under an approval for another LSN. Reading why
// a subscription stopped never waits.
func TestPostgresSubscriptionSkipWaits(t *testing.T) {
	a, s, h, db, admin := dbFixture(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"] = ana
	if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
		fmt.Sprintf(`{"userEmail":%q,"level":"dba"}`, ana)); w.Code >= 300 {
		t.Fatalf("assign: %d %s", w.Code, w.Body)
	}
	gate := func(op tunnel.PGAdminOp, approval string) *tunnel.PGAnswer {
		return a.statementGate(ctx, ana, db, tunnel.LevelDBA, &tunnel.PGRequest{ID: 1, Op: "admin", Admin: &op, Approval: approval})
	}
	if no := gate(tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionWhy, Name: "athena_sub"}, ""); no != nil {
		t.Fatalf("reading why waited: %+v", no)
	}
	skip := tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionSkip, Name: "athena_sub", LSN: "0/1D5B8F0"}
	if no := gate(tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionSkip, Name: "athena_sub", LSN: "0/1; DROP TABLE x"}, ""); no == nil || no.Code == tunnel.GuardNeedsApproval {
		t.Fatalf("a malformed LSN was not refused outright: %+v", no)
	}
	no := gate(skip, "")
	if no == nil || no.Code != tunnel.GuardNeedsApproval ||
		no.Approve != "ALTER SUBSCRIPTION \"athena_sub\" SKIP (lsn = '0/1D5B8F0');\nALTER SUBSCRIPTION \"athena_sub\" ENABLE" {
		t.Fatalf("a production skip did not wait for its own text: %+v", no)
	}
	body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"sql": no.Approve}},
		"justification": "athena_sub stops on a row that is already here"})
	w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	var ask StatementAsk
	_ = json.Unmarshal(w.Body.Bytes(), &ask)
	if no := gate(skip, ask.RequestID); no == nil {
		t.Fatal("ran before anyone approved it")
	}
	if _, err := s.DecideRequest(ctx, ask.RequestID, "approved", "", admin, false); err != nil {
		t.Fatal(err)
	}
	other := skip
	other.LSN = "0/2000000"
	if no := gate(other, ask.RequestID); no == nil {
		t.Fatal("another transaction was skipped under this approval")
	}
	if no := gate(skip, ask.RequestID); no != nil {
		t.Fatalf("the approved skip was stopped: %+v", no)
	}
	if no := gate(skip, ask.RequestID); no == nil {
		t.Fatal("an approval was used twice")
	}

	// Staging skips without asking.
	if w := do(t, h, http.MethodPatch, "/api/v1/databases/"+db.ID, "admin-token", `{"label":"staging","policy":{}}`); w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	db.Label = "staging"
	if no := gate(skip, ""); no != nil {
		t.Fatalf("a staging skip waited: %+v", no)
	}
}

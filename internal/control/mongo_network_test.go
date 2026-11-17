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

// A MongoDB's change of which machines can connect is DBA work, audited,
// and waits for approval of exactly it on production and, anywhere, always.
func TestMongoNetworkChangeWaits(t *testing.T) {
	a, s, h, pg, admin := dbFixture(t)
	ctx := context.Background()
	db, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMongoDB,
		Name: "app", Host: "127.0.0.1", Port: 27017}, admin)
	if err != nil {
		t.Fatalf("CreateDatabase mongodb: %v", err)
	}
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"] = ana
	if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
		fmt.Sprintf(`{"userEmail":%q,"level":"dba"}`, ana)); w.Code >= 300 {
		t.Fatalf("assign: %d %s", w.Code, w.Body)
	}
	gate := func(level string, req tunnel.StoreRequest) *tunnel.StoreAnswer {
		return a.mongoGate(ctx, ana, db, level, &req)
	}
	// Commands pass, with no approval smuggled along.
	run := tunnel.StoreRequest{Op: "run", Command: "db.orders.find({})", Approval: "x"}
	if no := a.mongoGate(ctx, ana, db, tunnel.LevelWrite, &run); no != nil || run.Approval != "" {
		t.Fatalf("a command: %+v %+v", no, run)
	}
	if no := gate(tunnel.LevelWrite, tunnel.StoreRequest{Op: tunnel.MongoOpNetwork}); no == nil || !strings.Contains(no.Error, "DBA") {
		t.Fatalf("below DBA: %+v", no)
	}
	sel := tunnel.StoreRequest{ID: 1, Op: tunnel.MongoOpNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}, Restart: true}
	no := gate(tunnel.LevelDBA, sel)
	if no == nil || no.NeedsApproval == nil || no.NeedsApproval.Text != tunnel.MongoNetworkText(sel) {
		t.Fatalf("production did not wait: %+v", no)
	}
	body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"sql": no.NeedsApproval.Text}}, "justification": "the app server moves"})
	w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	var ask StatementAsk
	_ = json.Unmarshal(w.Body.Bytes(), &ask)
	if _, err := s.DecideRequest(ctx, ask.RequestID, "approved", "", admin, false); err != nil {
		t.Fatal(err)
	}
	other := sel
	other.Sources = []string{"10.0.0.62"}
	other.Approval = ask.RequestID
	if no := gate(tunnel.LevelDBA, other); no == nil {
		t.Fatal("a different change ran under the approval")
	}
	sel.Approval = ask.RequestID
	if no := gate(tunnel.LevelDBA, sel); no != nil {
		t.Fatalf("the approved change was stopped: %+v", no)
	}
	if !auditHas(t, s, "db.network_change") {
		t.Fatal("the change is not in the audit log")
	}
	// On staging, selected machines run at once; anywhere still waits.
	db.Label = "staging"
	sel.Approval = ""
	if no := gate(tunnel.LevelDBA, sel); no != nil {
		t.Fatalf("staging waited: %+v", no)
	}
	if no := gate(tunnel.LevelDBA, tunnel.StoreRequest{Op: tunnel.MongoOpNetworkSet, Reach: tunnel.ReachAny}); no == nil || !strings.Contains(no.Error, "to anywhere") {
		t.Fatalf("anywhere on staging: %+v", no)
	}
}

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

func redisFixture(t *testing.T) (*API, *Store, http.Handler, Database, string) {
	t.Helper()
	a, s, h, pg, admin := dbFixture(t)
	db, err := s.CreateDatabase(context.Background(), Database{AssetID: pg.AssetID, Engine: tunnel.EngineRedis,
		Name: "0", Host: "127.0.0.1", Port: 6379}, admin)
	if err != nil {
		t.Fatalf("CreateDatabase redis: %v", err)
	}
	return a, s, h, db, admin
}

// A bulk, configuration or user change on a production Redis runs only under
// an approval for exactly it; a bulk change is held to the approved count.
func TestRedisChangesNeedApprovalOnProduction(t *testing.T) {
	a, s, h, db, admin := redisFixture(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"] = ana
	if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
		fmt.Sprintf(`{"userEmail":%q,"level":"dba"}`, ana)); w.Code >= 300 {
		t.Fatalf("assign: %d %s", w.Code, w.Body)
	}
	if !db.WritesNeedApproval() {
		t.Fatal("a production Redis's changes wait by default")
	}
	gate := func(level string, req tunnel.StoreRequest) (*tunnel.StoreAnswer, tunnel.StoreRequest) {
		no := a.redisGate(ctx, ana, db, level, &req)
		return no, req
	}
	// Levels first.
	if no, _ := gate(tunnel.LevelOwner, tunnel.StoreRequest{Op: tunnel.RedisOpConfigSet,
		Config: []tunnel.RedisConfigItem{{Name: "maxmemory", Value: "1gb"}}}); no == nil || !strings.Contains(no.Error, "dba") {
		t.Fatalf("config set at owner: %+v", no)
	}
	if no, _ := gate(tunnel.LevelRead, tunnel.StoreRequest{Op: tunnel.RedisOpEdit}); no == nil {
		t.Fatal("an edit at read passed")
	}
	// Edits and small bulks do not wait; a made-up approval id is dropped.
	if no, r := gate(tunnel.LevelWrite, tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Approval: "x"}); no != nil || r.Approval != "" {
		t.Fatalf("edit: %+v %+v", no, r)
	}
	bulk := tunnel.StoreRequest{Op: tunnel.RedisOpBulk, Action: "delete", Max: 5000,
		Filter: tunnel.RedisFilter{Pattern: "session:2024*"}}
	no, _ := gate(tunnel.LevelWrite, bulk)
	if no == nil || no.NeedsApproval == nil || no.NeedsApproval.Text != "BULK DELETE MATCH session:2024*" || no.NeedsApproval.Cap != 5000 {
		t.Fatalf("bulk without approval: %+v", no)
	}
	// Ask with what the refusal said; an admin approves.
	body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"sql": no.NeedsApproval.Text, "rows": 4000}},
		"justification": "old sessions left behind by the TTL bug"})
	w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana", string(body))
	var ask StatementAsk
	_ = json.Unmarshal(w.Body.Bytes(), &ask)
	if w.Code != http.StatusCreated {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	if _, err := s.DecideRequest(ctx, ask.RequestID, "approved", "", admin); err != nil {
		t.Fatal(err)
	}
	// Another pattern does not fit the approval.
	other := bulk
	other.Filter.Pattern, other.Approval = "user:*", ask.RequestID
	if no, _ := gate(tunnel.LevelWrite, other); no == nil || !strings.Contains(no.Error, "not the one") {
		t.Fatalf("another pattern: %+v", no)
	}
	bulk.Approval = ask.RequestID
	no, r := gate(tunnel.LevelWrite, bulk)
	if no != nil || r.Max != 4000 || r.Approval != ask.RequestID {
		t.Fatalf("approved bulk: %+v max %d", no, r.Max)
	}
	if no, _ := gate(tunnel.LevelWrite, bulk); no == nil || !strings.Contains(no.Error, "already ran") {
		t.Fatalf("ran twice: %+v", no)
	}
	if !auditHas(t, s, "db.redis_change") {
		t.Fatal("the change is not in the audit log")
	}
	// Staging: nothing waits.
	db.Label = "staging"
	if no, _ := gate(tunnel.LevelDBA, tunnel.StoreRequest{Op: tunnel.RedisOpACLDel, User: "app"}); no != nil {
		t.Fatalf("staging waited: %+v", no)
	}
}

// Per-level rules are kept with the entry and checked when saved.
func TestRedisLevelRulesSaved(t *testing.T) {
	_, s, _, db, _ := redisFixture(t)
	ctx := context.Background()
	db.Policy.RedisLevels = map[string][]string{tunnel.LevelWrite: {"%R~*", "~cart:*", "-@all", "+@read", "+@write"}}
	got, err := s.UpdateDatabase(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Policy.RedisLevels[tunnel.LevelWrite]) != 5 {
		t.Fatalf("rules not kept: %+v", got.Policy)
	}
	db.Policy.RedisLevels = map[string][]string{tunnel.LevelOwner: {"~*", "+@admin"}}
	if _, err := s.UpdateDatabase(ctx, db); err == nil {
		t.Fatal("+@admin at owner was saved")
	}
	// The switch is kept for Redis, so a production Redis can opt out.
	off := false
	db.Policy.RedisLevels, db.Policy.WriteApproval = nil, &off
	got, err = s.UpdateDatabase(ctx, db)
	if err != nil || got.WritesNeedApproval() {
		t.Fatalf("approval switch on a Redis: %+v %v", got.Policy, err)
	}
}

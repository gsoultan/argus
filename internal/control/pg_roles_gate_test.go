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

// A role or privilege change waits for approval where the database's changes
// do, for its own text with the password redacted; approved, it runs once and
// only as approved. Making a role a superuser waits on every database.
func TestPostgresRoleChangeWaits(t *testing.T) {
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
	grant := tunnel.PGAdminOp{Kind: tunnel.AdminGrant, GrantSpec: &tunnel.PGGrantSpec{Grantee: "hermod", On: tunnel.OnAllTables,
		Schema: "etl", Privileges: []string{"SELECT"}, SchemaUsage: true}}

	// Lists and previews never wait.
	if no := gate(tunnel.PGAdminOp{Kind: tunnel.AdminRoleList}, ""); no != nil {
		t.Fatalf("a list waited: %+v", no)
	}
	pre := grant
	pre.Preview = true
	if no := gate(pre, ""); no != nil {
		t.Fatalf("a preview waited: %+v", no)
	}

	// Production: the grant waits, naming its own text.
	no := gate(grant, "")
	if no == nil || no.Code != tunnel.GuardNeedsApproval || !strings.Contains(no.Approve, `GRANT SELECT ON ALL TABLES IN SCHEMA "etl" TO "hermod"`) {
		t.Fatalf("a production grant did not wait: %+v", no)
	}
	body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"sql": no.Approve}},
		"justification": "hermod's ETL job fails with permission denied on etl"})
	w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	var ask StatementAsk
	_ = json.Unmarshal(w.Body.Bytes(), &ask)
	if no := gate(grant, ask.RequestID); no == nil {
		t.Fatal("ran before anyone approved it")
	}
	if _, err := s.DecideRequest(ctx, ask.RequestID, "approved", "", admin, false); err != nil {
		t.Fatal(err)
	}
	more := grant
	more.GrantSpec = &tunnel.PGGrantSpec{Grantee: "hermod", On: tunnel.OnAllTables, Schema: "etl", Privileges: []string{"ALL"}, SchemaUsage: true}
	if no := gate(more, ask.RequestID); no == nil {
		t.Fatal("a wider grant ran under a narrower approval")
	}
	if no := gate(grant, ask.RequestID); no != nil {
		t.Fatalf("the approved grant was stopped: %+v", no)
	}
	if no := gate(grant, ask.RequestID); no == nil {
		t.Fatal("an approval was used twice")
	}

	// A password is approved as [redacted]; the approver never sees it.
	pw := tunnel.PGAdminOp{Kind: tunnel.AdminRoleAlter, RoleSpec: &tunnel.PGRoleSpec{Name: "hermod", Password: "s3cret-for-hermod"}}
	no = gate(pw, "")
	if no == nil || strings.Contains(no.Approve, "s3cret") || !strings.Contains(no.Approve, "PASSWORD [redacted]") {
		t.Fatalf("password change: %+v", no)
	}

	// Staging runs without asking, except making a superuser.
	if w := do(t, h, http.MethodPatch, "/api/v1/databases/"+db.ID, "admin-token", `{"label":"staging","policy":{}}`); w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	db.Label = "staging"
	if no := gate(grant, ""); no != nil {
		t.Fatalf("a staging grant waited: %+v", no)
	}
	yes := true
	super := tunnel.PGAdminOp{Kind: tunnel.AdminRoleAlter, RoleSpec: &tunnel.PGRoleSpec{Name: "hermod", Superuser: &yes}}
	no = gate(super, "")
	if no == nil || !strings.Contains(no.Error, "superuser") {
		t.Fatalf("making a superuser on staging did not wait: %+v", no)
	}
	body, _ = json.Marshal(map[string]any{"items": []map[string]any{{"sql": no.Approve}}, "justification": "migration tool needs it for one night"})
	if w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana", string(body)); w.Code != http.StatusCreated {
		t.Fatalf("asking for a superuser on staging: %d %s", w.Code, w.Body)
	}
	plain, _ := json.Marshal(map[string]any{"items": []map[string]any{{"sql": `GRANT SELECT ON ALL TABLES IN SCHEMA "etl" TO "hermod"`}}, "justification": "no need to ask for this one"})
	if w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana", string(plain)); w.Code != http.StatusBadRequest {
		t.Fatalf("an ordinary grant ask on staging: %d %s", w.Code, w.Body)
	}

	// Not Postgres: refused rather than passed on.
	db.Engine = tunnel.EngineMySQL
	if no := gate(grant, ""); no == nil || !strings.Contains(no.Error, "Postgres only") {
		t.Fatalf("a MySQL role change: %+v", no)
	}
}

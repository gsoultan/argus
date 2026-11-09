package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// bulkFixture is a server with two databases and a RabbitMQ vhost, a team
// with ana in it, and bob on his own.
type bulkFix struct {
	a        *API
	s        *Store
	h        http.Handler
	admin    string
	assetID  string
	host     string
	orders   Database
	billing  Database
	vhost    Database
	team     string
	ana, bob string
}

func bulkFixture(t *testing.T) bulkFix {
	t.Helper()
	a, s, h, db, admin := dbFixture(t)
	ctx := context.Background()
	billing, err := s.CreateDatabase(ctx, Database{AssetID: db.AssetID, Name: "billing"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	vhost, err := s.CreateDatabase(ctx, Database{AssetID: db.AssetID, Engine: tunnel.EngineRabbitMQ, Name: "/orders"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, unique("payments"), nil, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteTeam(context.Background(), team) })
	ana, _ := newAccount(t, s, "operator")
	bob, _ := newAccount(t, s, "operator")
	if err := s.AddTeamMember(ctx, team, ana, admin); err != nil {
		t.Fatal(err)
	}
	return bulkFix{a: a, s: s, h: h, admin: admin, assetID: db.AssetID, host: db.Hostname, orders: db,
		billing: billing, vhost: vhost, team: team, ana: ana, bob: bob}
}

func (f bulkFix) post(t *testing.T, path string, body any) (int, map[string]json.RawMessage) {
	t.Helper()
	b, _ := json.Marshal(body)
	w := do(t, f.h, http.MethodPost, path, "admin-token", string(b))
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func linesOf(t *testing.T, raw json.RawMessage) []BulkAccessLine {
	t.Helper()
	var lines []BulkAccessLine
	if err := json.Unmarshal(raw, &lines); err != nil {
		t.Fatalf("lines: %v %s", err, raw)
	}
	return lines
}

func (f bulkFix) level(t *testing.T, email, dbID string) DBAccess {
	t.Helper()
	acc, err := f.s.DatabaseAccess(context.Background(), email, dbID)
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// One grant gives a team and a person a level on several databases and a
// broker, with each row exactly as a single grant would write it, and an
// audit entry per row. Someone who has more keeps it unless asked.
func TestBulkAccessGivesEveryRowAndNeverLowersUnlessAsked(t *testing.T) {
	f := bulkFixture(t)
	ctx := context.Background()
	// bob already has DBA on billing.
	if err := f.s.SetDBAssignment(ctx, DBAssignment{DatabaseID: f.billing.ID, UserEmail: &f.bob, Level: tunnel.LevelDBA}, f.admin); err != nil {
		t.Fatal(err)
	}
	in := BulkAccessInput{DatabaseIDs: []string{f.orders.ID, f.billing.ID, f.vhost.ID}, Teams: []string{f.team},
		People: []string{f.bob}, Level: tunnel.LevelWrite, BrokerLevel: tunnel.LevelRead}

	// The plan writes nothing.
	code, out := f.post(t, "/api/v1/database-access/plan", in)
	if code != http.StatusOK {
		t.Fatalf("plan: %d %s", code, out)
	}
	plan := linesOf(t, out["lines"])
	if len(plan) != 6 {
		t.Fatalf("2 who x 3 databases is 6 lines, got %d", len(plan))
	}
	if acc := f.level(t, f.ana, f.orders.ID); acc.Level != "" || acc.Approvable != "" {
		t.Fatalf("the plan gave access: %+v", acc)
	}

	code, out = f.post(t, "/api/v1/database-access", in)
	if code != http.StatusOK {
		t.Fatalf("give: %d %s", code, out)
	}
	changes := map[string]string{}
	for _, l := range linesOf(t, out["lines"]) {
		changes[lineWho(l)+"@"+l.DatabaseName] = l.Change
	}
	if changes["person "+f.bob+"@billing"] != "kept" || changes["person "+f.bob+"@orders"] != "new" {
		t.Fatalf("changes: %v", changes)
	}
	// Production, Write: sessions wait by label. The broker at Read does not.
	if acc := f.level(t, f.ana, f.orders.ID); acc.Approvable != tunnel.LevelWrite || acc.Level != "" {
		t.Fatalf("ana on orders through the team: %+v", acc)
	}
	if acc := f.level(t, f.ana, f.vhost.ID); acc.Level != tunnel.LevelRead {
		t.Fatalf("ana on the vhost through the team: %+v", acc)
	}
	if acc := f.level(t, f.bob, f.billing.ID); acc.Level != tunnel.LevelDBA {
		t.Fatalf("bob was lowered without being asked: %+v", acc)
	}
	var audits int
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'db.assigned' AND target = $1
		AND actor_email = $2`, f.host, f.admin).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 5 { // six lines, bob's DBA on billing kept and not written
		t.Fatalf("one audit entry per written row: %d", audits)
	}

	// Asked to lower, it does.
	in.Lower = true
	if code, out := f.post(t, "/api/v1/database-access", in); code != http.StatusOK {
		t.Fatalf("give with lower: %d %s", code, out)
	}
	if acc := f.level(t, f.bob, f.billing.ID); acc.Approvable != tunnel.LevelWrite || acc.Level != "" {
		t.Fatalf("bob after lowering: %+v", acc)
	}

	// And taken away from all of them at once.
	code, out = f.post(t, "/api/v1/database-access/remove", BulkRemoveInput{
		DatabaseIDs: []string{f.orders.ID, f.billing.ID, f.vhost.ID}, Teams: []string{f.team}})
	if code != http.StatusOK || len(linesOf(t, out["lines"])) != 3 {
		t.Fatalf("remove: %d %s", code, out)
	}
	if acc := f.level(t, f.ana, f.vhost.ID); acc.Level != "" || acc.Approvable != "" {
		t.Fatalf("ana kept the vhost: %+v", acc)
	}
}

// Only an admin may give or plan access to many databases, the same as one.
func TestBulkAccessIsAdminOnly(t *testing.T) {
	f := bulkFixture(t)
	f.a.UserTokens["op-token"] = f.ana
	body := fmt.Sprintf(`{"databaseIds":[%q],"people":[%q],"level":"dba"}`, f.orders.ID, f.ana)
	for _, path := range []string{"/api/v1/database-access", "/api/v1/database-access/plan", "/api/v1/database-access/remove"} {
		if w := do(t, f.h, http.MethodPost, path, "op-token", body); w.Code != http.StatusForbidden {
			t.Fatalf("an operator called %s: %d %s", path, w.Code, w.Body)
		}
	}
	if w := do(t, f.h, http.MethodGet, "/api/v1/assets/"+f.assetID+"/database-access", "op-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator read who has what: %d", w.Code)
	}
	if acc := f.level(t, f.ana, f.orders.ID); acc.Level != "" {
		t.Fatalf("the refusal still gave access: %+v", acc)
	}
}

// A grant is all or nothing: one database that is not there refuses the
// whole request and writes no row.
func TestBulkAccessIsAllOrNothing(t *testing.T) {
	f := bulkFixture(t)
	code, _ := f.post(t, "/api/v1/database-access", BulkAccessInput{
		DatabaseIDs: []string{f.orders.ID, "00000000-0000-0000-0000-000000000000"}, People: []string{f.bob}, Level: "read"})
	if code != http.StatusBadRequest {
		t.Fatalf("a missing database: %d", code)
	}
	if acc := f.level(t, f.bob, f.orders.ID); acc.Level != "" {
		t.Fatalf("part of a refused grant was written: %+v", acc)
	}
}

// With later, a database added to the server afterwards is given at once, a
// broker is not (the rule was for databases), a hand edit makes the row the
// admin's own, and taking the rule away takes back only what is still its.
func TestDatabaseRuleGivesDatabasesAddedLater(t *testing.T) {
	f := bulkFixture(t)
	ctx := context.Background()
	code, out := f.post(t, "/api/v1/database-access", BulkAccessInput{DatabaseIDs: []string{f.orders.ID, f.billing.ID},
		Teams: []string{f.team}, Level: tunnel.LevelRead, Later: true})
	if code != http.StatusOK {
		t.Fatalf("give with later: %d %s", code, out)
	}
	var rules []DatabaseRule
	_ = json.Unmarshal(out["rules"], &rules)
	if len(rules) != 1 || rules[0].Kind != RuleKindDatabase {
		t.Fatalf("one rule, for databases: %s", out["rules"])
	}

	w := do(t, f.h, http.MethodPost, "/api/v1/databases", "admin-token", fmt.Sprintf(`{"assetId":%q,"dbname":"reporting"}`, f.assetID))
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var reporting Database
	_ = json.Unmarshal(w.Body.Bytes(), &reporting)
	if acc := f.level(t, f.ana, reporting.ID); acc.Level != tunnel.LevelRead {
		t.Fatalf("a database added later was not given: %+v", acc)
	}
	var audited int
	_ = f.s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'db.assigned' AND target = $1
		AND detail LIKE '%reporting%rule for databases added later%'`, f.host).Scan(&audited)
	if audited != 1 {
		t.Fatalf("the rule's grant was not audited: %d", audited)
	}

	w = do(t, f.h, http.MethodPost, "/api/v1/databases", "admin-token",
		fmt.Sprintf(`{"assetId":%q,"engine":"rabbitmq","dbname":"/events"}`, f.assetID))
	var events Database
	_ = json.Unmarshal(w.Body.Bytes(), &events)
	if acc := f.level(t, f.ana, events.ID); acc.Level != "" {
		t.Fatalf("a database rule gave a broker: %+v", acc)
	}

	// billing set by hand stops being the rule's.
	if err := f.s.SetDBAssignment(ctx, DBAssignment{DatabaseID: f.billing.ID, TeamID: &f.team, Level: tunnel.LevelRead}, f.admin); err != nil {
		t.Fatal(err)
	}
	w = do(t, f.h, http.MethodDelete, "/api/v1/assets/"+f.assetID+"/database-rules/"+rules[0].ID+"?takeAway=1", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("remove rule: %d %s", w.Code, w.Body)
	}
	if acc := f.level(t, f.ana, reporting.ID); acc.Level != "" {
		t.Fatalf("the rule's own grant stayed: %+v", acc)
	}
	if acc := f.level(t, f.ana, f.billing.ID); acc.Level != tunnel.LevelRead {
		t.Fatalf("a grant set by hand was taken with the rule: %+v", acc)
	}
	// No rule, so the next database is not given.
	w = do(t, f.h, http.MethodPost, "/api/v1/databases", "admin-token", fmt.Sprintf(`{"assetId":%q,"dbname":"later2"}`, f.assetID))
	var later2 Database
	_ = json.Unmarshal(w.Body.Bytes(), &later2)
	if acc := f.level(t, f.ana, later2.ID); acc.Level != "" {
		t.Fatalf("a removed rule still gave: %+v", acc)
	}
}

// A rule is for one server: later with databases on two is refused.
func TestDatabaseRuleIsForOneServer(t *testing.T) {
	f := bulkFixture(t)
	g := bulkFixture(t)
	code, _ := f.post(t, "/api/v1/database-access", BulkAccessInput{DatabaseIDs: []string{f.orders.ID, g.orders.ID},
		People: []string{f.bob}, Level: "read", Later: true})
	if code != http.StatusBadRequest {
		t.Fatalf("later across two servers: %d", code)
	}
	if acc := f.level(t, f.bob, f.orders.ID); acc.Level != "" {
		t.Fatalf("the refused grant wrote a row: %+v", acc)
	}
}

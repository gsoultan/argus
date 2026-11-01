package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Who may open a database, and at what level. Against a real Postgres, like
the rest of this package: the rules are joins over assignments, teams and
access requests, and a fake store would assert that the fake works.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable go test ./internal/control/ -run Database
*/

func dbFixture(t *testing.T) (*API, *Store, http.Handler, Database, string) {
	t.Helper()
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admin, _ := newAccount(t, s, "admin")
	a.UserTokens = map[string]string{"admin-token": admin}
	signer, err := auth.NewSigner("a-signing-secret-with-plenty-of-variety-0193")
	if err != nil {
		t.Fatal(err)
	}
	a.signer = signer
	ctx := context.Background()
	host := unique("db-host")
	asset, err := s.CreateAsset(ctx, AssetInput{Hostname: host, Address: host, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	t.Cleanup(func() { _ = s.ArchiveAsset(context.Background(), asset.ID) })
	db, err := s.CreateDatabase(ctx, Database{AssetID: asset.ID, Name: "orders"}, admin)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	return a, s, a.Handler(), db, admin
}

func TestDatabaseDefaultsAreSafe(t *testing.T) {
	_, _, _, db, _ := dbFixture(t)
	if db.Label != "production" || db.Host != "/var/run/postgresql" || db.Port != 5432 ||
		db.AdminUser != "postgres" || db.Engine != "postgres" {
		t.Fatalf("defaults: %+v", db)
	}
}

func TestDatabaseMustBeLocalToItsServer(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	for _, host := range []string{"10.0.0.5", "db.internal", "/var/run/../etc"} {
		_, err := s.CreateDatabase(context.Background(), Database{AssetID: db.AssetID, Name: "x", Host: host}, admin)
		var invalid ErrInvalidDatabase
		if err == nil || !errorsAs(err, &invalid) {
			t.Fatalf("host %q must be refused, got %v", host, err)
		}
	}
}

func errorsAs(err error, target *ErrInvalidDatabase) bool {
	e, ok := err.(ErrInvalidDatabase)
	if ok {
		*target = e
	}
	return ok
}

// The level a person holds is the highest of their rows that need no
// approval; a row that needs approval only counts while a request for it is
// approved and unexpired.
func TestDatabaseAccessLevels(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	teamID, err := s.CreateTeam(ctx, unique("ops"), nil, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteTeam(context.Background(), teamID) })
	if err := s.AddTeamMember(ctx, teamID, ana, admin); err != nil {
		t.Fatal(err)
	}

	acc, err := s.DatabaseAccess(ctx, ana, db.ID)
	if err != nil || acc.Level != "" || acc.Approvable != "" {
		t.Fatalf("nothing assigned yet: %+v %v", acc, err)
	}

	// Team: read, no approval. Person: write, with approval.
	if err := s.SetDBAssignment(ctx, DBAssignment{DatabaseID: db.ID, TeamID: &teamID, Level: tunnel.LevelRead}, admin); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDBAssignment(ctx, DBAssignment{DatabaseID: db.ID, UserEmail: &ana, Level: tunnel.LevelWrite,
		NeedsApproval: true}, admin); err != nil {
		t.Fatal(err)
	}
	acc, err = s.DatabaseAccess(ctx, ana, db.ID)
	if err != nil || acc.Level != tunnel.LevelRead || acc.Approvable != tunnel.LevelWrite {
		t.Fatalf("read now, write on request: %+v %v", acc, err)
	}

	// Asking for more than assigned is refused; asking for write is filed.
	if _, err := s.CreateDatabaseRequest(ctx, ana, db.ID, tunnel.LevelDBA, "need to manage roles for the release", 60); err == nil {
		t.Fatal("dba was not assigned, so it cannot be asked for")
	}
	req, err := s.CreateDatabaseRequest(ctx, ana, db.ID, tunnel.LevelWrite, "cancel order 48163, ticket SUP-482", 60)
	if err != nil {
		t.Fatal(err)
	}
	// Pending: still read.
	if acc, _ = s.DatabaseAccess(ctx, ana, db.ID); acc.Level != tunnel.LevelRead {
		t.Fatalf("a pending request gives nothing: %+v", acc)
	}
	if _, err := s.DecideRequest(ctx, req.ID, "approved", "", admin); err != nil {
		t.Fatal(err)
	}
	acc, err = s.DatabaseAccess(ctx, ana, db.ID)
	if err != nil || acc.Level != tunnel.LevelWrite || acc.GrantExpires == nil || acc.Approvable != "" {
		t.Fatalf("approved: write until it expires: %+v %v", acc, err)
	}

	// Expired: back to read, by the clock alone.
	if _, err := s.pool.Exec(ctx, `UPDATE access_requests SET expires_at = now() - interval '1 minute' WHERE id = $1::uuid`, req.ID); err != nil {
		t.Fatal(err)
	}
	if acc, _ = s.DatabaseAccess(ctx, ana, db.ID); acc.Level != tunnel.LevelRead || acc.Approvable != tunnel.LevelWrite {
		t.Fatalf("expired: %+v", acc)
	}

	// Leaving the team takes read away.
	if err := s.RemoveTeamMember(ctx, teamID, ana); err != nil {
		t.Fatal(err)
	}
	if acc, _ = s.DatabaseAccess(ctx, ana, db.ID); acc.Level != "" {
		t.Fatalf("out of the team: %+v", acc)
	}
}

func TestDatabaseTicketRoute(t *testing.T) {
	a, s, h, db, _ := dbFixture(t)
	ctx := context.Background()
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana-token"] = ana
	auditor, _ := newAccount(t, s, "auditor")
	a.UserTokens["auditor-token"] = auditor
	path := "/api/v1/databases/" + db.ID + "/ticket"

	// Not assigned: refused, and the refusal is a 403, not a ticket at nothing.
	if w := do(t, h, http.MethodPost, path, "ana-token", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("unassigned: %d %s", w.Code, w.Body)
	}
	if err := s.SetDBAssignment(ctx, DBAssignment{DatabaseID: db.ID, UserEmail: &ana, Level: tunnel.LevelRead}, "t"); err != nil {
		t.Fatal(err)
	}
	// Assigned read: a ticket at read, as a role made from the login.
	w := do(t, h, http.MethodPost, path, "ana-token", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("read ticket: %d %s", w.Code, w.Body)
	}
	var out struct{ Ticket, Level, Role string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Level != tunnel.LevelRead || !tunnel.ValidPersonalRole(out.Role) || out.Ticket == "" {
		t.Fatalf("ticket body: %+v", out)
	}
	// The ticket is bound to this host and role@level: redeeming it for a
	// higher level fails, so the relay cannot be talked into one.
	if _, err := a.signer.RedeemTicket(ctx, out.Ticket, db.Hostname, out.Role+"@write"); err == nil {
		t.Fatal("a read ticket redeemed at write")
	}
	if _, err := a.signer.RedeemTicket(ctx, out.Ticket, db.Hostname, out.Role+"@read"); err != nil {
		t.Fatalf("the ticket must redeem for what it was issued for: %v", err)
	}
	// Asking for a level above what is held: refused.
	if w := do(t, h, http.MethodPost, path, "ana-token", `{"level":"owner"}`); w.Code != http.StatusForbidden {
		t.Fatalf("owner on a read assignment: %d %s", w.Code, w.Body)
	}
	// An auditor reads sessions and opens nothing.
	if w := do(t, h, http.MethodPost, path, "auditor-token", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("auditor: %d %s", w.Code, w.Body)
	}
	// Admins open at dba, and it is audited.
	w = do(t, h, http.MethodPost, path, "admin-token", `{}`)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || out.Level != tunnel.LevelDBA {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
	events, err := s.AuditEvents(ctx, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, e := range events {
		if e.Action == "db.authorized" && e.Target == db.Hostname && strings.Contains(e.Detail, "at dba") {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the authorisation must be in the audit log with the level")
	}
}

func TestDatabaseWritesAreAdminOnly(t *testing.T) {
	a, s, h, db, _ := dbFixture(t)
	op, _ := newAccount(t, s, "operator")
	a.UserTokens["op-token"] = op
	body := fmt.Sprintf(`{"assetId":%q,"dbname":"other"}`, db.AssetID)
	if w := do(t, h, http.MethodPost, "/api/v1/databases", "op-token", body); w.Code != http.StatusForbidden {
		t.Fatalf("an operator added a database: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "op-token",
		fmt.Sprintf(`{"userEmail":%q,"level":"dba"}`, op)); w.Code != http.StatusForbidden {
		t.Fatalf("an operator assigned themselves dba: %d %s", w.Code, w.Body)
	}
	// And the list shows an operator only what they may open.
	w := do(t, h, http.MethodGet, "/api/v1/databases", "op-token", "")
	var list struct{ Databases []Database }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != http.StatusOK || len(list.Databases) != 0 {
		t.Fatalf("an unassigned operator sees databases: %d %s", w.Code, w.Body)
	}
	_ = time.Now
}

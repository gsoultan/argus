package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
)

/*
Teams.

A team is a second way to hold an assignment, so it has to obey every rule
the first way does: bounded by the asset's own accounts, gone the moment the
membership or the team is, and refused rather than allowed when in doubt.
These tests exercise the refusals, since a team that grants too little is a
support ticket and one that grants too much is the product failing.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Team
*/

// testTeam makes a team holding the given people and removes it afterwards.
func testTeam(t *testing.T, s *Store, members ...string) string {
	t.Helper()
	id, err := s.CreateTeam(context.Background(), unique("team"), members, "admin@corp.example")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteTeam(context.Background(), id) })
	return id
}

func giveTeam(t *testing.T, s *Store, assetID, teamID string, principals ...string) {
	t.Helper()
	if err := s.SetTeamAssignment(context.Background(), assetID, teamID, principals,
		"admin@corp.example"); err != nil {
		t.Fatalf("SetTeamAssignment: %v", err)
	}
}

func TestATeamMemberReachesTheTeamsServer(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("member") + "@corp.example"
	asset := testAsset(t, s, "ops")
	giveTeam(t, s, asset.ID, testTeam(t, s, email), "ops")

	if got := askAuthorize(t, a, email, asset.Hostname, "ops"); !got.Allowed {
		t.Fatalf("a team member was refused the team's server: %s", got.Reason)
	}
}

// Being in one team says nothing about another team's servers.
func TestAnotherTeamsServerIsRefused(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("member") + "@corp.example"
	testTeam(t, s, email)
	other := testAsset(t, s, "ops")
	giveTeam(t, s, other.ID, testTeam(t, s, unique("else")+"@corp.example"), "ops")

	if got := askAuthorize(t, a, email, other.Hostname, "ops"); got.Allowed {
		t.Fatal("a member of one team reached a server given only to another")
	}
}

// Leaving the team is the whole revocation: nothing else has to be found and
// undone, which is what makes it safe to rely on.
func TestLeavingATeamRevokesItsServers(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("member") + "@corp.example"
	asset := testAsset(t, s, "ops")
	team := testTeam(t, s, email)
	giveTeam(t, s, asset.ID, team, "ops")

	if err := s.RemoveTeamMember(context.Background(), team, email); err != nil {
		t.Fatalf("RemoveTeamMember: %v", err)
	}
	if got := askAuthorize(t, a, email, asset.Hostname, "ops"); got.Allowed {
		t.Fatal("someone who left the team still reached its server")
	}
}

func TestDeletingATeamRevokesItsServers(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("member") + "@corp.example"
	asset := testAsset(t, s, "ops")
	team := testTeam(t, s, email)
	giveTeam(t, s, asset.ID, team, "ops")

	if err := s.DeleteTeam(context.Background(), team); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	if got := askAuthorize(t, a, email, asset.Hostname, "ops"); got.Allowed {
		t.Fatal("a deleted team's member still reached its server")
	}
}

// A team is given accounts, not the host: deploy was never given.
func TestTeamAccessIsPerPrincipal(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("member") + "@corp.example"
	asset := testAsset(t, s, "ops", "deploy")
	giveTeam(t, s, asset.ID, testTeam(t, s, email), "ops")

	if got := askAuthorize(t, a, email, asset.Hostname, "deploy"); got.Allowed {
		t.Fatal("a team member assumed an account the team was not given")
	}
}

// The asset's own list bounds the team's, at read time, the same as for a
// person: removing an account from the server removes it from the team.
func TestRemovingAnAccountFromTheServerRevokesItFromTheTeam(t *testing.T) {
	a, s := authorizeAPI(t)
	ctx := context.Background()
	email := unique("member") + "@corp.example"
	asset := testAsset(t, s, "ops", "deploy")
	giveTeam(t, s, asset.ID, testTeam(t, s, email), "ops", "deploy")

	if _, err := s.UpdateAsset(ctx, asset.ID, AssetInput{
		Hostname: asset.Hostname, Address: asset.Address, Principals: []string{"deploy"},
		CredentialRef: "injected",
	}); err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}
	if got := askAuthorize(t, a, email, asset.Hostname, "ops"); got.Allowed {
		t.Fatal("an account removed from the server was still reachable through a team")
	}
}

func TestATeamCannotBeGivenAnAccountTheServerDoesNotHave(t *testing.T) {
	s := testStore(t)
	asset := testAsset(t, s, "ops")
	err := s.SetTeamAssignment(context.Background(), asset.ID,
		testTeam(t, s), []string{"root"}, "admin@corp.example")
	var invalid ErrInvalidAsset
	if !errors.As(err, &invalid) {
		t.Fatalf("assigning an account the server does not permit: err = %v, want ErrInvalidAsset", err)
	}
}

// The console's list and the session check read the same union, so a member
// sees exactly what they can open, with the accounts from every way they hold it.
func TestAssetsForUserIncludesTeamServers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email := unique("member") + "@corp.example"
	asset := testAsset(t, s, "ops", "deploy", "backup")
	if err := s.SetAssignment(ctx, asset.ID, email, []string{"ops"}, "admin@corp.example"); err != nil {
		t.Fatal(err)
	}
	giveTeam(t, s, asset.ID, testTeam(t, s, email), "deploy")
	notMine := testAsset(t, s, "ops")

	got, err := s.AssetsForUser(ctx, email)
	if err != nil {
		t.Fatalf("AssetsForUser: %v", err)
	}
	if len(got) != 1 || got[0].ID != asset.ID {
		t.Fatalf("AssetsForUser = %v, want only %s (and never %s)", got, asset.Hostname, notMine.Hostname)
	}
	slices.Sort(got[0].Principals)
	if !slices.Equal(got[0].Principals, []string{"deploy", "ops"}) {
		t.Errorf("principals = %v, want [deploy ops]: one from the person, one from the team", got[0].Principals)
	}
}

func TestOnlyAdminsManageTeams(t *testing.T) {
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	op, _ := newAccount(t, s, "operator")
	a.UserTokens = map[string]string{"operator-token": op}
	h := a.Handler()

	if w := do(t, h, http.MethodPost, "/api/v1/teams", "operator-token", `{"name":"Mine"}`); w.Code != http.StatusForbidden {
		t.Fatalf("an operator made a team: status %d", w.Code)
	}
	team := testTeam(t, s)
	if w := do(t, h, http.MethodPut, "/api/v1/teams/"+team+"/members", "operator-token",
		`{"email":"`+op+`"}`); w.Code != http.StatusForbidden {
		t.Fatalf("an operator added themselves to a team: status %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/teams", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator read every team: status %d", w.Code)
	}
}

// Joining a team is a change in who can reach which server, so the entry says
// which servers, with the actor a reconstruction needs.
func TestJoiningATeamIsAudited(t *testing.T) {
	_, s, h, admin := enrollAPI(t)
	asset := testAsset(t, s, "ops")
	team := testTeam(t, s)
	giveTeam(t, s, asset.ID, team, "ops")
	who := unique("member") + "@corp.example"

	w := do(t, h, http.MethodPut, "/api/v1/teams/"+team+"/members", "admin-token", `{"email":"`+who+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", w.Code, w.Body)
	}
	var out Team
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if !slices.Contains(out.Members, who) {
		t.Errorf("members = %v, want %s among them", out.Members, who)
	}

	var actor, detail string
	err := s.pool.QueryRow(context.Background(), `
		SELECT actor_email, detail FROM audit_events
		 WHERE action = 'team.member_added' ORDER BY seq DESC LIMIT 1`).Scan(&actor, &detail)
	if err != nil {
		t.Fatalf("reading the audit chain: %v", err)
	}
	if actor != admin {
		t.Errorf("actor = %q, want %q", actor, admin)
	}
	for _, want := range []string{who, asset.Hostname} {
		if !strings.Contains(detail, want) {
			t.Errorf("audit detail %q does not name %s", detail, want)
		}
	}
}

// Approving a server can give it to teams in the same transaction, so a
// half-given server never exists.
func TestEnrolGivesTheServerToTeams(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cleanHost(t, s, "disc-08")
	seedDiscovered(t, s, "disc-08")
	who := unique("member") + "@corp.example"
	team := testTeam(t, s, who)

	id, err := s.EnrolHost(ctx, "disc-08", "admin@northwind.id", []string{"ops"}, nil, []string{team})
	if err != nil {
		t.Fatalf("EnrolHost: %v", err)
	}
	asset, err := s.AssetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AssignmentAllows(ctx, who, asset.Hostname, "ops"); !ok {
		t.Error("a member of the team picked at approval cannot reach the server")
	}
}

func TestEnrolWithAMissingTeamApprovesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cleanHost(t, s, "disc-09")
	seedDiscovered(t, s, "disc-09")

	_, err := s.EnrolHost(ctx, "disc-09", "admin@northwind.id", []string{"ops"}, nil,
		[]string{"00000000-0000-0000-0000-000000000000"})
	if !errors.Is(err, ErrTeamNotFound) {
		t.Fatalf("err = %v, want ErrTeamNotFound", err)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT enrollment_state FROM agents WHERE hostname = 'disc-09'`).
		Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unreviewed" {
		t.Errorf("enrollment_state = %q, want unreviewed: the refusal must roll the approval back", state)
	}
}

package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

/*
Who may reach which host.

The principal list on an asset says which accounts Argus can broker there. It
never said for whom, so every signed-in account could open a session on every
host in the inventory by naming it -- on a product whose entire claim is knowing
who reached what.

Assignment is that missing half. These tests hold the rules that make it worth
having: it is bounded by the asset's own principal list, it is not the only way
in (an approved request still is), and losing it must refuse rather than allow.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/
*/

// testAsset creates a console-owned asset with the given principals.
func testAsset(t *testing.T, s *Store, principals ...string) Asset {
	t.Helper()
	asset, err := s.CreateAsset(context.Background(), AssetInput{
		Hostname:      unique("host") + ".example",
		Address:       "192.0.2.10",
		Principals:    principals,
		CredentialRef: "injected",
	})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM assets WHERE id = $1::uuid`, asset.ID)
	})
	return asset
}

// assignedAsset creates an asset and assigns it to one person.
func assignedAsset(t *testing.T, s *Store, email string, principals ...string) Asset {
	t.Helper()
	asset := testAsset(t, s, principals...)
	if err := s.SetAssignment(context.Background(), asset.ID, email, principals,
		"admin@corp.example"); err != nil {
		t.Fatalf("SetAssignment: %v", err)
	}
	return asset
}

/* ── The rule itself ─────────────────────────────────────────────────────── */

// The case the whole feature exists for.
func TestAnUnassignedHostIsRefused(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("stranger") + "@corp.example"
	asset := testAsset(t, s, "ops")

	got := askAuthorize(t, a, email, asset.Hostname, "ops")
	if got.Allowed {
		t.Fatal("a host nobody assigned was opened by someone who asked for it")
	}
	if !strings.Contains(got.Reason, "not assigned") {
		t.Errorf("the refusal should say why: %q", got.Reason)
	}
}

// And the refusal is written down. "Who tried to reach what and was told no" is
// the other half of the question an audit opens with.
func TestARefusedSessionIsAudited(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("stranger") + "@corp.example"
	asset := testAsset(t, s, "ops")

	askAuthorize(t, a, email, asset.Hostname, "ops")

	var action string
	err := s.pool.QueryRow(context.Background(), `
		SELECT action FROM audit_events
		 WHERE actor_email = $1 ORDER BY seq DESC LIMIT 1`, email).Scan(&action)
	if err != nil {
		t.Fatalf("reading the audit chain: %v", err)
	}
	if action != "session.refused_unassigned" {
		t.Errorf("last audit entry = %q, want session.refused_unassigned", action)
	}
}

func TestAnAssignedHostIsAllowed(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("lin") + "@corp.example"
	asset := assignedAsset(t, s, email, "ops")

	if got := askAuthorize(t, a, email, asset.Hostname, "ops"); !got.Allowed {
		t.Fatalf("an assigned host was refused: %s", got.Reason)
	}
}

// Assigned one account does not mean assigned every account the host has.
func TestAssignmentIsPerPrincipal(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("lin") + "@corp.example"
	asset := testAsset(t, s, "ops", "deploy")
	if err := s.SetAssignment(context.Background(), asset.ID, email,
		[]string{"ops"}, "admin@corp.example"); err != nil {
		t.Fatal(err)
	}

	if got := askAuthorize(t, a, email, asset.Hostname, "deploy"); got.Allowed {
		t.Error("a principal nobody assigned was opened")
	}
}

// The short name works at an ssh(1) command line, so it has to resolve to the
// same assignment. Otherwise `ssh ops:pay-01@gw` is refused while
// `ssh ops:pay-01.corp@gw` is allowed, for the same person and the same host.
func TestAShortNameMatchesTheAssignment(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("lin") + "@corp.example"
	asset := assignedAsset(t, s, email, "ops")
	short, _, _ := strings.Cut(asset.Hostname, ".")

	if got := askAuthorize(t, a, email, short, "ops"); !got.Allowed {
		t.Fatalf("the short name was refused where the full one is allowed: %s", got.Reason)
	}
}

// An approved access request is the other way in, and has to be: a product
// whose answer to "I need this host for an hour" is "ask for it permanently"
// has no just-in-time access at all.
func TestAnApprovedRequestOpensAnUnassignedHost(t *testing.T) {
	a, s := authorizeAPI(t)
	ctx := context.Background()
	email := unique("oncall") + "@corp.example"
	asset := testAsset(t, s, "ops")

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO access_requests (requester_email, asset_hostnames, principal,
		                             justification, duration_minutes, state, expires_at)
		VALUES ($1, ARRAY[$2], 'ops', 'incident', 30, 'approved', now() + interval '30 minutes')`,
		email, asset.Hostname); err != nil {
		t.Fatal(err)
	}

	if got := askAuthorize(t, a, email, asset.Hostname, "ops"); !got.Allowed {
		t.Fatalf("an approved request did not open the host: %s", got.Reason)
	}
}

/* ── The asset bounds the assignment ─────────────────────────────────────── */

// Assigning an account the host does not permit would produce access that looks
// granted in the console and is refused at the gateway.
func TestAssigningAPrincipalTheAssetDoesNotPermitIsRefused(t *testing.T) {
	s := testStore(t)
	asset := testAsset(t, s, "ops")

	err := s.SetAssignment(context.Background(), asset.ID,
		unique("lin")+"@corp.example", []string{"root"}, "admin@corp.example")
	var invalid ErrInvalidAsset
	if !errors.As(err, &invalid) {
		t.Fatalf("assigning an unpermitted principal returned %v", err)
	}
	if !strings.Contains(invalid.Reason, "root") {
		t.Errorf("the refusal should name the principal: %q", invalid.Reason)
	}
}

// Taking a principal off the asset takes it away from everyone assigned it,
// without having to revisit every assignment. The intersection is computed, not
// stored, and this is the test that keeps it that way.
func TestRemovingAPrincipalFromTheAssetRevokesIt(t *testing.T) {
	a, s := authorizeAPI(t)
	ctx := context.Background()
	email := unique("lin") + "@corp.example"
	asset := assignedAsset(t, s, email, "ops", "deploy")

	if _, err := s.UpdateAsset(ctx, asset.ID, AssetInput{
		Hostname:      asset.Hostname,
		Address:       asset.Address,
		Principals:    []string{"ops"},
		CredentialRef: "injected",
	}); err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}

	if got := askAuthorize(t, a, email, asset.Hostname, "deploy"); got.Allowed {
		t.Error("a principal removed from the asset survived in the assignment")
	}
	if got := askAuthorize(t, a, email, asset.Hostname, "ops"); !got.Allowed {
		t.Errorf("the principal that was kept was revoked too: %s", got.Reason)
	}

	// And the console shows the same thing the gateway enforces.
	assets, err := s.AssetsForUser(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range assets {
		if got.ID != asset.ID {
			continue
		}
		if len(got.Principals) != 1 || got.Principals[0] != "ops" {
			t.Errorf("the console still offers %v", got.Principals)
		}
	}
}

/* ── What each person sees ───────────────────────────────────────────────── */

func TestAssetsForUserShowsOnlyWhatWasAssigned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email := unique("lin") + "@corp.example"

	mine := assignedAsset(t, s, email, "ops")
	theirs := testAsset(t, s, "ops")

	got, err := s.AssetsForUser(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	var sawMine bool
	for _, a := range got {
		if a.ID == theirs.ID {
			t.Error("an unassigned host appeared in someone's inventory")
		}
		if a.ID == mine.ID {
			sawMine = true
		}
	}
	if !sawMine {
		t.Error("an assigned host did not appear in the assignee's inventory")
	}
}

// An address is matched case-insensitively everywhere else in this product, and
// an assignment that differs only in capitalisation is an assignment nobody
// holds.
func TestAssignmentIsCaseInsensitiveOnTheAddress(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("Lin") + "@Corp.Example"
	asset := assignedAsset(t, s, email, "ops")

	if got := askAuthorize(t, a, strings.ToLower(email), asset.Hostname, "ops"); !got.Allowed {
		t.Fatalf("the same address in another case was refused: %s", got.Reason)
	}
}

/* ── Who may change it ───────────────────────────────────────────────────── */

func TestOnlyAdminsChangeTheInventory(t *testing.T) {
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.UserTokens = map[string]string{"operator-token": "operator@corp.example"}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/assets",
		strings.NewReader(`{"hostname":"new.example","address":"192.0.2.1","principals":["ops"],"credentialRef":"injected"}`))
	r.Header.Set("Authorization", "Bearer operator-token")
	w := httptest.NewRecorder()
	a.postAssetCreate(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("an operator created an asset: status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "admin") {
		t.Errorf("the refusal should say which role is needed: %s", w.Body.String())
	}
}

/* ── The credential is a name, not a path ────────────────────────────────── */

// An administrator who could type a path here could point the gateway at any
// file it can read and have it used as a private key. The vault directory is
// the boundary, and this is the check that keeps input inside it.
func TestACredentialCannotNameAPath(t *testing.T) {
	s := testStore(t)
	for _, ref := range []string{
		"../../etc/shadow", "/etc/shadow", "keys/injected", "..", ".",
		`..\windows\system32`,
	} {
		_, err := s.CreateAsset(context.Background(), AssetInput{
			Hostname:      unique("host") + ".example",
			Address:       "192.0.2.10",
			Principals:    []string{"ops"},
			CredentialRef: ref,
		})
		var invalid ErrInvalidAsset
		if !errors.As(err, &invalid) {
			t.Errorf("credential %q was accepted (%v)", ref, err)
		}
	}
}

// Certificate mode has no standing credential, so a name left over from an
// earlier mode must not survive: it would say the host has a secret to rotate
// when the entire point of the mode is that it does not.
func TestCertificateModeDropsTheCredentialName(t *testing.T) {
	s := testStore(t)
	asset, err := s.CreateAsset(context.Background(), AssetInput{
		Hostname:       unique("host") + ".example",
		Address:        "192.0.2.10",
		Principals:     []string{"ops"},
		CredentialMode: "ca-certificate",
		CredentialRef:  "injected",
	})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1::uuid`, asset.ID)
	})
	if asset.CredentialRef != "" {
		t.Errorf("a certificate asset kept a credential name: %q", asset.CredentialRef)
	}
}

/* ── The console owns what it edits ──────────────────────────────────────── */

// A gateway publishing its file inventory must not overwrite what an
// administrator entered in the console — otherwise every edit reverts at the
// next gateway restart, which is the kind of bug people blame on themselves.
func TestAGatewayPublishDoesNotOverwriteAConsoleAsset(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	asset := testAsset(t, s, "ops")

	err := s.UpsertAsset(ctx, Asset{
		Hostname:   asset.Hostname,
		Address:    "10.0.0.9",
		Port:       2222,
		Principals: []string{"ops", "root"},
		Health:     "reachable",
	})
	if !errors.Is(err, ErrConsoleOwned) {
		t.Fatalf("a gateway publish over a console asset returned %v", err)
	}

	after, err := s.AssetByID(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Address != asset.Address || len(after.Principals) != 1 {
		t.Errorf("the console's values were overwritten: %s %v",
			after.Address, after.Principals)
	}
}

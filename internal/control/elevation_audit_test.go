package control

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

/*
The record of who was given root.

Every refusal was in the audit chain and no grant was. "Who held root on this
host, when, and under which approval" is the question an audit opens with, and
it was the one decision this path did not write down. The browser terminal has
recorded its own authorisations all along; this is the same fact arriving by
the other door.
*/

func authorizeAPI(t *testing.T) (*API, *Store) {
	t.Helper()
	s := testStore(t)
	return NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

func askAuthorize(t *testing.T, a *API, email, target, principal string) Authorization {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet,
		"/api/v1/report/authorize?email="+email+"&target="+target+"&principal="+principal, nil)
	got, err := a.authorizePrincipal(r, email, target, principal)
	if err != nil {
		t.Fatalf("authorizePrincipal: %v", err)
	}
	return got
}

// lastElevationAudit returns the most recent elevation decision for an actor.
func lastElevationAudit(t *testing.T, s *Store, email string) (action, detail string) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(), `
		SELECT action, detail FROM audit_events
		 WHERE actor_email = $1 AND action LIKE 'session.elevation_%'
		 ORDER BY seq DESC LIMIT 1`, email).Scan(&action, &detail)
	if err != nil {
		return "", ""
	}
	return action, detail
}

// A grant that authorises a root session has to be written down.
func TestAnAllowedElevationIsAudited(t *testing.T) {
	a, s := authorizeAPI(t)
	ctx := context.Background()
	email := unique("granted") + "@corp.example"
	host := unique("host") + ".example"

	// Satisfy the kernel-evidence check with an agent rather than by turning
	// the policy off. gateway_policy is a single shared row, and a test that
	// flips it is changing live configuration for the duration of its run --
	// on a shared development database that is somebody else's session being
	// refused, or worse, allowed.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO agents (hostname, version, last_seen_at, exec_tracing, updated_at)
		VALUES ($1, 'test', now(), TRUE, now())
		ON CONFLICT (hostname) DO UPDATE SET exec_tracing = TRUE`, host); err != nil {
		t.Fatal(err)
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO access_requests (requester_email, asset_hostnames, principal,
		                             justification, duration_minutes, state, expires_at)
		VALUES ($1, ARRAY[$2], 'root', 'incident', 30, 'approved', now() + interval '30 minutes')`,
		email, host); err != nil {
		t.Fatal(err)
	}

	got := askAuthorize(t, a, email, host, "root")
	if !got.Allowed {
		t.Fatalf("an approved request did not authorise: %s", got.Reason)
	}

	action, detail := lastElevationAudit(t, s, email)
	if action != "session.elevation_allowed" {
		t.Fatalf("last elevation audit = %q, want session.elevation_allowed -- "+
			"a grant of root left no record", action)
	}
	if !strings.Contains(detail, "root") {
		t.Errorf("the entry should name the principal: %q", detail)
	}
	if !strings.Contains(detail, time.Now().UTC().Format("2006")) {
		t.Errorf("the entry should say how long it lasts: %q", detail)
	}
}

// Refusals stay audited too; adding the positive case must not lose it.
func TestARefusedElevationIsStillAudited(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("norequest") + "@corp.example"

	got := askAuthorize(t, a, email, unique("host")+".example", "root")
	if got.Allowed {
		t.Fatal("root was allowed with no approval on file")
	}
	if action, _ := lastElevationAudit(t, s, email); action != "session.elevation_refused" {
		t.Errorf("last elevation audit = %q, want session.elevation_refused", action)
	}
}

// An ordinary principal needs no approval and writes no elevation entry, or
// the chain fills with noise and the interesting lines are lost in it.
func TestAnOrdinaryPrincipalIsNotAudited(t *testing.T) {
	a, s := authorizeAPI(t)
	email := unique("ordinary") + "@corp.example"

	if got := askAuthorize(t, a, email, "db-01.example", "ops"); !got.Allowed {
		t.Fatalf("an ordinary principal was refused: %s", got.Reason)
	}
	if action, _ := lastElevationAudit(t, s, email); action != "" {
		t.Errorf("an ordinary session wrote an elevation entry: %q", action)
	}
}

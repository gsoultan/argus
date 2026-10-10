package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

/*
What each role is told about the fleet.

The Overview described the whole fleet to everyone: an operator assigned two
hosts was shown thirteen, with a headline about hosts they cannot reach. The
Sessions list was worse -- who else was on which host and when is the same
reconnaissance the asset list is filtered to prevent, and a recording is the
most sensitive artefact this product holds.

Scoped rather than gated, so the pages still work and every figure on them is
about something the person can act on.
*/

func visibilityAPI(t *testing.T, email string) (*API, *Store) {
	t.Helper()
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.UserTokens = map[string]string{"tok": email}
	return a, s
}

// insertSession writes a session straight to the table. The reporter path needs
// a gateway; this test only needs a row that belongs to somebody.
func insertSession(t *testing.T, s *Store, id, email, hostname string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_email, asset_hostname, principal, state,
		                      started_at, last_reported_at)
		VALUES ($1::uuid, $2, $3, 'ops', 'active', now(), now())`,
		id, email, hostname); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id = $1::uuid`, id)
	})
}

func newID(t *testing.T) string {
	t.Helper()
	var id string
	if err := testStore(t).pool.QueryRow(context.Background(),
		`SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

/* ── The Overview ────────────────────────────────────────────────────────── */

func TestStatsCountOnlyTheHostsSomeoneWasAssigned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email := unique("lin") + "@corp.example"

	assignedAsset(t, s, email, "ops")
	testAsset(t, s, "ops") // somebody else's

	mine, err := s.StatsFor(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if mine.AssetsTotal != 1 {
		t.Errorf("an operator assigned one host was shown %d", mine.AssetsTotal)
	}
	if fleet.AssetsTotal < 2 {
		t.Fatalf("the fleet count should include both test hosts, got %d", fleet.AssetsTotal)
	}
}

// A retired asset leaves the inventory, so it must leave the counters with it —
// otherwise the Overview reports hosts that no gateway will broker.
func TestArchivedAssetsLeaveTheCounters(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	before, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	asset := testAsset(t, s, "ops")
	if err := s.ArchiveAsset(ctx, asset.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.AssetsTotal != before.AssetsTotal {
		t.Errorf("a retired host is still counted: %d before, %d after",
			before.AssetsTotal, after.AssetsTotal)
	}
}

func TestStatsCountOnlyYourOwnSessions(t *testing.T) {
	email := unique("lin") + "@corp.example"
	a, s := visibilityAPI(t, email)
	asset := assignedAsset(t, s, email, "ops")

	insertSession(t, s, newID(t), email, asset.Hostname)
	insertSession(t, s, newID(t), unique("other")+"@corp.example", asset.Hostname)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	a.getStats(w, r)

	var got FleetStats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding stats: %v (%s)", err, w.Body.String())
	}
	if got.SessionsActive != 1 {
		t.Errorf("an operator with one live session was shown %d", got.SessionsActive)
	}
}

/* ── The Sessions list ───────────────────────────────────────────────────── */

func TestAnOperatorSeesOnlyTheirOwnSessions(t *testing.T) {
	email := unique("lin") + "@corp.example"
	a, s := visibilityAPI(t, email)
	asset := assignedAsset(t, s, email, "ops")
	theirs := newID(t)

	insertSession(t, s, newID(t), email, asset.Hostname)
	insertSession(t, s, theirs, unique("other")+"@corp.example", asset.Hostname)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	a.getSessions(w, r)

	var got []Session
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding sessions: %v", err)
	}
	for _, sess := range got {
		if sess.ID == theirs {
			t.Fatal("an operator was shown someone else's session")
		}
		if sess.UserEmail != email {
			t.Errorf("session for %s appeared in %s's list", sess.UserEmail, email)
		}
	}
}

// Not found rather than forbidden: a 403 confirms the id names a real session
// and whose it is, which is most of what the recording would have said.
func TestSomeoneElsesSessionIsNotFound(t *testing.T) {
	email := unique("lin") + "@corp.example"
	a, s := visibilityAPI(t, email)
	asset := assignedAsset(t, s, email, "ops")
	theirs := newID(t)
	insertSession(t, s, theirs, unique("other")+"@corp.example", asset.Hostname)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+theirs, nil)
	r.SetPathValue("id", theirs)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	a.getSession(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("reading someone else's session returned %d", w.Code)
	}
}

// And the recording behind it, which is the artefact that actually matters.
func TestSomeoneElsesRecordingIsNotFound(t *testing.T) {
	email := unique("lin") + "@corp.example"
	a, s := visibilityAPI(t, email)
	asset := assignedAsset(t, s, email, "ops")
	theirs := newID(t)
	insertSession(t, s, theirs, unique("other")+"@corp.example", asset.Hostname)

	// Give it a recording, so a 404 cannot be explained by there being nothing
	// to serve.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET recording_path = 'recordings/x.cast' WHERE id = $1::uuid`,
		theirs); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+theirs+"/recording", nil)
	r.SetPathValue("id", theirs)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	a.getRecording(w, r, email)

	if w.Code != http.StatusNotFound {
		t.Errorf("reading someone else's recording returned %d", w.Code)
	}
}

/* ── An auditor still sees everything ────────────────────────────────────── */

// Reviewing sessions is what the role is for. Scoping it to their own would
// make an auditor unable to audit.
func TestAnAuditorStillSeesTheFleet(t *testing.T) {
	s := testStore(t)
	email, _ := newAccount(t, s, "auditor")
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.UserTokens = map[string]string{"tok": email}

	asset := testAsset(t, s, "ops")
	other := unique("other") + "@corp.example"
	id := newID(t)
	insertSession(t, s, id, other, asset.Hostname)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	a.getSessions(w, r)

	var got []Session
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding sessions: %v", err)
	}
	var found bool
	for _, sess := range got {
		if sess.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("an auditor could not see a session they are meant to review")
	}
}

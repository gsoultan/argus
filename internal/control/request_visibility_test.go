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
Who may read the access-request queue.

The queue carries every requester's address, the hosts they asked for and the
reason they gave -- a break-glass reason names the incident. It was sent whole
to any signed-in account, so an operator who opened the page to ask for a
server read everyone else's requests, with Approve buttons the API would then
refuse. People who decide or audit requests see the queue; everyone else sees
their own.
*/

func listRequestsAs(t *testing.T, a *API, token string) []AccessRequest {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/requests?limit=500", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list as %s: status %d: %s", token, w.Code, w.Body.String())
	}
	var out []AccessRequest
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnOperatorSeesOnlyTheirOwnRequests(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	mine := unique("op") + "@corp.example"
	theirs := unique("other") + "@corp.example"
	approver := unique("appr") + "@corp.example"
	for email, role := range map[string]string{mine: "operator", theirs: "operator", approver: "approver"} {
		dropAccount(t, s, email)
		if _, err := s.CreateAccount(ctx, email, email, role, "a-long-enough-password"); err != nil {
			t.Fatalf("create %s: %v", email, err)
		}
	}
	for _, email := range []string{mine, theirs} {
		if _, err := s.CreateRequest(ctx, AccessRequest{
			RequesterEmail: email, AssetHostnames: []string{"db-01.example"}, Principal: "ops",
			Justification: "rotating the replica after the failover", DurationMinutes: 60,
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM access_requests WHERE requester_email IN ($1, $2)`, mine, theirs)
	})

	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.UserTokens = map[string]string{"op": mine, "appr": approver}

	for _, r := range listRequestsAs(t, a, "op") {
		if r.RequesterEmail != mine {
			t.Fatalf("an operator was sent %s's request", r.RequesterEmail)
		}
	}
	if got := listRequestsAs(t, a, "op"); len(got) != 1 {
		t.Fatalf("an operator was sent %d of their own requests, want 1", len(got))
	}

	seen := map[string]bool{}
	for _, r := range listRequestsAs(t, a, "appr") {
		seen[r.RequesterEmail] = true
	}
	if !seen[mine] || !seen[theirs] {
		t.Fatalf("an approver was not sent the whole queue: %v", seen)
	}
}

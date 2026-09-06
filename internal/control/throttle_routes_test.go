package control

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/auth"
)

// The limits are configured, the limiter is unit-tested, and nothing drove
// repeated requests at the real handlers to see them bite. Brute-force
// protection nobody has watched work is brute-force protection nobody has.

// throttledAPI is the control plane with the given limits.
//
// withOIDC wires the fake issuer from oidc_flow_test. Without it the callback
// returns 501 before it ever looks at the error parameter, so no failure is
// recorded and the failure budget cannot be exercised -- which is correct for
// static-token mode, where there is no login flow to brute-force.
func throttledAPI(t *testing.T, cfg ThrottleConfig, withOIDC bool) *httptest.Server {
	t.Helper()
	api := NewAPI(testStore(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	api.UserTokens = map[string]string{"dev-token": "dewi.p@northwind.id"}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	// Tickets are signed; without a signer the handler has nothing to sign with.
	signer, err := auth.NewSigner("a-development-secret-of-sufficient-length")
	if err != nil {
		t.Fatal(err)
	}
	var o *auth.OIDC
	if withOIDC {
		issuer := newFakeIssuer(t)
		o, err = auth.NewOIDC(context.Background(), auth.OIDCConfig{
			Issuer: issuer.srv.URL, ClientID: "argus-console", ClientSecret: "s",
			RedirectURL: srv.URL + "/auth/callback", DefaultRole: "auditor",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	api.SetAuth(o, signer, "", false)

	th, err := NewThrottles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(th.Close)
	api.SetThrottles(th)
	return srv
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func TestLoginIsThrottledPerClient(t *testing.T) {
	// Burst is half the per-minute rate: a browser fires a few requests at
	// once, a script fires them forever. So 6/minute admits 3 immediately.
	srv := throttledAPI(t, ThrottleConfig{LoginPerMinute: 6}, false)
	for i := 1; i <= 3; i++ {
		if res := get(t, srv.URL+"/auth/login"); res.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d of 3 should be within the burst", i)
		}
	}
	res := get(t, srv.URL+"/auth/login")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th immediate login = %d, want 429", res.StatusCode)
	}
	// A client that is told to wait must be told how long.
	if res.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After leaves the client guessing")
	}
}

// The failure budget is separate and tighter. Someone who fumbles a login
// once must not be locked out; a script offering credentials must be.
func TestFailedAuthenticationsAreBudgetedSeparately(t *testing.T) {
	srv := throttledAPI(t, ThrottleConfig{LoginPerMinute: 100, FailuresPerHour: 3}, true)

	// The IdP refusing is a failure the callback records.
	for i := 1; i <= 3; i++ {
		res := get(t, srv.URL+"/auth/callback?error=access_denied")
		if res.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("failure %d of 3 should still be processed", i)
		}
	}
	// Over budget: refused before any work is done, with no hint about
	// whether anything supplied was valid.
	res := get(t, srv.URL+"/auth/login")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("login after 3 failures = %d, want 429", res.StatusCode)
	}
	// And this is the budget that used to refill itself on every request.
	res = get(t, srv.URL+"/auth/callback?error=access_denied")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a 4th failure attempt = %d, want 429 -- the failure budget is not holding", res.StatusCode)
	}
}

func TestTicketMintingIsThrottled(t *testing.T) {
	srv := throttledAPI(t, ThrottleConfig{TicketsPerMinute: 2}, false)
	post := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/terminal/ticket",
			strings.NewReader(`{"target":"pay-01","principal":"ops"}`))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for i := 1; i <= 2; i++ {
		if post() == http.StatusTooManyRequests {
			t.Fatalf("ticket %d of 2 should be within the budget", i)
		}
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Fatalf("3rd ticket in a minute = %d, want 429", code)
	}
}

// A throttle response must not become an oracle for the thing it protects.
func TestThrottleResponseSaysNothingAboutTheCredential(t *testing.T) {
	srv := throttledAPI(t, ThrottleConfig{LoginPerMinute: 1}, false)
	get(t, srv.URL+"/auth/login")
	res, _ := http.Get(srv.URL + "/auth/login")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, leak := range []string{"valid", "invalid", "password", "user", "unknown"} {
		if strings.Contains(strings.ToLower(string(body)), leak) {
			t.Errorf("throttle body leaks %q: %s", leak, body)
		}
	}
}

package control

import (
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
func throttledAPI(t *testing.T, cfg ThrottleConfig) *httptest.Server {
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
	api.SetAuth(signer, "", false)

	th, err := NewThrottles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(th.Close)
	api.SetThrottles(th)
	return srv
}

// postLogin drives the password route. An empty body is refused as malformed
// before any credential is checked, so it exercises the rate limiter without
// spending the separate failure budget.
func postLogin(t *testing.T, url, body string) *http.Response {
	t.Helper()
	res, err := http.Post(url+"/auth/password", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
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
	srv := throttledAPI(t, ThrottleConfig{LoginPerMinute: 6})
	for i := 1; i <= 3; i++ {
		if res := postLogin(t, srv.URL, `{}`); res.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d of 3 should be within the burst", i)
		}
	}
	res := postLogin(t, srv.URL, `{}`)
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
	srv := throttledAPI(t, ThrottleConfig{LoginPerMinute: 100, FailuresPerHour: 3})

	// A refused password is a failure the login route records.
	const wrong = `{"email":"nobody@northwind.id","password":"wrong"}`
	for i := 1; i <= 3; i++ {
		res := postLogin(t, srv.URL, wrong)
		if res.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("failure %d of 3 should still be processed", i)
		}
	}
	// Over budget: refused before any work is done, with no hint about
	// whether anything supplied was valid.
	res := postLogin(t, srv.URL, `{}`)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("login after 3 failures = %d, want 429", res.StatusCode)
	}
	// And this is the budget that used to refill itself on every request.
	res = postLogin(t, srv.URL, wrong)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a 4th failure attempt = %d, want 429 -- the failure budget is not holding", res.StatusCode)
	}
}

func TestTicketMintingIsThrottled(t *testing.T) {
	srv := throttledAPI(t, ThrottleConfig{TicketsPerMinute: 2})
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
	srv := throttledAPI(t, ThrottleConfig{LoginPerMinute: 1})
	postLogin(t, srv.URL, `{}`)
	res, err := http.Post(srv.URL+"/auth/password", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, leak := range []string{"valid", "invalid", "password", "user", "unknown"} {
		if strings.Contains(strings.ToLower(string(body)), leak) {
			t.Errorf("throttle body leaks %q: %s", leak, body)
		}
	}
}

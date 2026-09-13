package control

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

/*
Static console tokens.

They exist so a brand-new deployment has some way in before anyone has an
account. Every other property this product claims about access -- attribution,
a password, a second factor, a role -- is bypassed by one, so the moment a real
way in exists they must stop working.
*/

func apiWithTokens(t *testing.T, disabled bool) *API {
	t.Helper()
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(discard{}, nil)))
	a.UserTokens = map[string]string{"dev-token": "someone@corp.example"}
	a.StaticTokensDisabled = disabled
	return a
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func withToken(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

// The bootstrap case: nothing else configured, so the token works.
func TestAStaticTokenWorksWhenItIsTheOnlyWayIn(t *testing.T) {
	a := apiWithTokens(t, false)
	sess, ok := a.authenticate(withToken("dev-token"))
	if !ok {
		t.Fatal("with no provider and no accounts, the token is the only way in")
	}
	if sess.Email != "someone@corp.example" {
		t.Errorf("email = %q", sess.Email)
	}
}

// Once the deployment has a real sign-in, the token is dead.
func TestAStaticTokenIsRefusedOnceThereIsARealWayIn(t *testing.T) {
	a := apiWithTokens(t, true)
	if _, ok := a.authenticate(withToken("dev-token")); ok {
		t.Error("a bearer token in a config file walked past password and MFA")
	}
}

// A token must not out-rank the account it names.
func TestAStaticTokenDoesNotMintAdmin(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email := unique("viewer") + "@corp.example"
	if _, err := s.CreateAccount(ctx, email, "A Viewer", "viewer", "a-long-enough-password"); err != nil {
		t.Fatalf("create: %v", err)
	}
	a := NewAPI(s, slog.New(slog.NewTextHandler(discard{}, nil)))
	a.UserTokens = map[string]string{"tok": email}

	sess, ok := a.authenticate(withToken("tok"))
	if !ok {
		t.Fatal("precondition: the token should resolve")
	}
	if sess.Role == "admin" {
		t.Errorf("a token naming a viewer resolved to admin; role must come "+
			"from the account, not from holding the string (got %q)", sess.Role)
	}
	if sess.Role != "viewer" {
		t.Errorf("role = %q, want viewer", sess.Role)
	}
}

// An unknown token is nobody.
func TestAnUnknownTokenIsRefused(t *testing.T) {
	a := apiWithTokens(t, false)
	if _, ok := a.authenticate(withToken("not-a-real-token")); ok {
		t.Error("an unknown bearer token authenticated")
	}
	if _, ok := a.authenticate(httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)); ok {
		t.Error("a request with no credential authenticated")
	}
}

// A valid session cookie still wins, and is unaffected by any of this.
func TestASessionCookieStillAuthenticates(t *testing.T) {
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(discard{}, nil)))
	signer, err := auth.NewSigner("a-signing-secret-with-plenty-of-variety-0193")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	t.Cleanup(signer.Close)
	a.SetAuth(signer, "https://console.example", true)
	a.StaticTokensDisabled = true

	tok, err := signer.IssueSession(auth.Session{
		Email: "real@corp.example", Name: "Real Person", Role: "admin"}, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: tok})

	sess, ok := a.authenticate(r)
	if !ok {
		t.Fatal("a valid session cookie was refused")
	}
	if sess.Email != "real@corp.example" || sess.Role != "admin" {
		t.Errorf("session = %+v", sess)
	}
}

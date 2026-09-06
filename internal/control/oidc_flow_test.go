package control

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

/*
The sign-in path, end to end, against the real handlers.

SSO was silently broken in production for every returning visitor by a
service-worker rule, and nothing on the server side would have noticed either:
there was no test that started at /auth/login and ended with an authenticated
request. This one does, against a fake identity provider that speaks enough
OIDC for go-oidc to trust it -- discovery, JWKS, an authorization endpoint that
sends the browser back with a code, and a token endpoint that signs an ID token
carrying the nonce the flow began with.

Every check the real handler makes is exercised by a real request: the CSRF
state, the PKCE verifier, the nonce, the issuer and audience on the ID token,
the email claim, and the group-to-role mapping. Then the cookie it issued is
used to read and write policy, which is the reason anyone signs in.
*/

// testLogWriter forwards the control plane's log to the test, so a failed
// sign-in reports the handler's reason rather than just "login_failed".
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// fakeIssuer is the smallest identity provider go-oidc will accept.
type fakeIssuer struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	groups []string
	email  string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, email: "dewi.p@northwind.id"}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	iss := f.srv.URL

	asJSON := func(w http.ResponseWriter) { w.Header().Set("Content-Type", "application/json") }
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		asJSON(w)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss,
			"authorization_endpoint":                iss + "/auth",
			"token_endpoint":                        iss + "/token",
			"jwks_uri":                              iss + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		asJSON(w)
		pub := &key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	// The user "logs in" instantly. The code carries the nonce so the token
	// endpoint can put it in the ID token without the fake keeping state.
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := back.Query()
		bq.Set("code", "nonce:"+q.Get("nonce"))
		bq.Set("state", q.Get("state"))
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.Form.Get("code")
		if !strings.HasPrefix(code, "nonce:") {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		groups, email := f.groups, f.email
		f.mu.Unlock()
		// oauth2 parses the token response by Content-Type: without this it
		// treats the JSON as a form body and the id_token silently vanishes.
		asJSON(w)
		now := time.Now()
		claims := map[string]any{
			"iss": iss, "aud": "argus-console", "sub": "08a8684b",
			"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
			"nonce": strings.TrimPrefix(code, "nonce:"),
			"email": email, "name": "Dewi P.", "groups": groups,
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "opaque", "token_type": "Bearer", "expires_in": 3600,
			"id_token": f.sign(claims),
		})
	})
	return f
}

func (f *fakeIssuer) sign(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]string{"alg": "RS256", "kid": "test", "typ": "JWT"})
	body := enc(claims)
	sum := sha256.Sum256([]byte(head + "." + body))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// consoleUnderTest is the control plane with OIDC pointed at the fake.
type consoleUnderTest struct {
	api    *API
	srv    *httptest.Server
	issuer *fakeIssuer
	client *http.Client
}

func newConsoleUnderTest(t *testing.T) *consoleUnderTest {
	t.Helper()
	store := testStore(t)
	issuer := newFakeIssuer(t)

	api := NewAPI(store, slog.New(slog.NewTextHandler(testLogWriter{t}, nil)))
	// A leftover dev token must be dead once OIDC is on; asserted below.
	api.UserTokens = map[string]string{"dev-token": "someone@northwind.id"}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	signer, err := auth.NewSigner("test-signing-secret-that-is-long-enough-for-hmac")
	if err != nil {
		t.Fatal(err)
	}
	o, err := auth.NewOIDC(context.Background(), auth.OIDCConfig{
		Issuer:       issuer.srv.URL,
		ClientID:     "argus-console",
		ClientSecret: "argus-console-secret",
		RedirectURL:  srv.URL + "/auth/callback",
		RoleClaim:    "groups",
		RoleMap:      map[string]string{"argus-admins": "admin", "engineering": "operator"},
		DefaultRole:  "auditor",
	})
	if err != nil {
		t.Fatalf("NewOIDC against the fake issuer: %v", err)
	}
	api.SetAuth(o, signer, "", false)

	jar, _ := cookiejar.New(nil)
	return &consoleUnderTest{api: api, srv: srv, issuer: issuer, client: &http.Client{
		Jar: jar,
		// Each hop is inspected, not followed blindly.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// signIn walks login -> issuer -> callback and returns the final redirect.
func (c *consoleUnderTest) signIn(t *testing.T, returnTo string) *http.Response {
	t.Helper()
	r1, err := c.client.Get(c.srv.URL + "/auth/login?return_to=" + url.QueryEscape(returnTo))
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusFound {
		t.Fatalf("/auth/login = %d, want 302", r1.StatusCode)
	}
	toIssuer := r1.Header.Get("Location")
	if !strings.HasPrefix(toIssuer, c.issuer.srv.URL+"/auth?") {
		t.Fatalf("login should redirect to the issuer, got %s", toIssuer)
	}
	for _, p := range []string{"state=", "nonce=", "code_challenge="} {
		if !strings.Contains(toIssuer, p) {
			t.Errorf("authorization request is missing %s", p)
		}
	}

	r2, err := c.client.Get(toIssuer)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	toCallback := r2.Header.Get("Location")

	r3, err := c.client.Get(toCallback)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	return r3
}

func (c *consoleUnderTest) me(t *testing.T) map[string]any {
	t.Helper()
	res, err := c.client.Get(c.srv.URL + "/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out
}

/* ── Tests ───────────────────────────────────────────────────────────────── */

func TestSSOSignInIssuesASessionAndMapsTheRole(t *testing.T) {
	c := newConsoleUnderTest(t)
	c.issuer.groups = []string{"engineering", "argus-admins"}

	final := c.signIn(t, "/settings")
	if final.StatusCode != http.StatusFound || final.Header.Get("Location") != "/settings" {
		t.Fatalf("callback should send the user to where they were going: %d %s",
			final.StatusCode, final.Header.Get("Location"))
	}

	me := c.me(t)
	if me["authenticated"] != true {
		t.Fatalf("not authenticated after sign-in: %v", me)
	}
	if me["email"] != "dewi.p@northwind.id" {
		t.Errorf("email = %v", me["email"])
	}
	// Most privileged group wins, so someone in both engineering and
	// argus-admins is admin, not whichever the map iterated first.
	if me["role"] != "admin" {
		t.Errorf("role = %v, want admin", me["role"])
	}
}

func TestSSOUnmappedUserGetsTheLeastPrivilegedRole(t *testing.T) {
	c := newConsoleUnderTest(t)
	c.issuer.groups = []string{"sales"}
	c.signIn(t, "/")
	if me := c.me(t); me["role"] != "auditor" {
		t.Errorf("unmapped user should be auditor, got %v", me["role"])
	}
}

// The reason anyone signs in: the cookie has to work against the API.
func TestSSOSessionCanReadAndWritePolicy(t *testing.T) {
	c := newConsoleUnderTest(t)
	c.issuer.groups = []string{"argus-admins"}
	c.signIn(t, "/")

	res, err := c.client.Get(c.srv.URL + "/api/v1/policy")
	if err != nil {
		t.Fatal(err)
	}
	var before GatewayPolicy
	_ = json.NewDecoder(res.Body).Decode(&before)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/policy with a session cookie = %d", res.StatusCode)
	}
	t.Cleanup(func() {
		_, _ = c.api.store.SaveGatewayPolicy(context.Background(), before, "test-cleanup")
	})

	next := before
	next.AllowX11Forward = !before.AllowX11Forward
	body, _ := json.Marshal(next)
	res, err = c.client.Post(c.srv.URL+"/api/v1/policy", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var saved GatewayPolicy
	_ = json.NewDecoder(res.Body).Decode(&saved)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/policy as admin = %d", res.StatusCode)
	}
	if saved.AllowX11Forward != next.AllowX11Forward {
		t.Error("policy write did not take effect")
	}
	if saved.UpdatedBy != "dewi.p@northwind.id" {
		t.Errorf("updatedBy = %q, want the signed-in user", saved.UpdatedBy)
	}
}

func TestSSOAuditorCanReadButNotWritePolicy(t *testing.T) {
	c := newConsoleUnderTest(t)
	c.issuer.groups = nil // -> auditor
	c.signIn(t, "/")

	res, _ := c.client.Get(c.srv.URL + "/api/v1/policy")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("an auditor must be able to read policy: %d", res.StatusCode)
	}
	res, _ = c.client.Post(c.srv.URL+"/api/v1/policy", "application/json", strings.NewReader("{}"))
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("an auditor must not be able to write policy: %d", res.StatusCode)
	}
}

// A dev token left in a config file must not become a backdoor once real
// identity is configured.
func TestStaticTokenIsDeadOnceOIDCIsConfigured(t *testing.T) {
	c := newConsoleUnderTest(t)
	req, _ := http.NewRequest(http.MethodGet, c.srv.URL+"/api/v1/policy", nil)
	req.Header.Set("Authorization", "Bearer dev-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("static token accepted with OIDC configured: %d", res.StatusCode)
	}
}

// CSRF: a callback whose state does not match the signed cookie is a forgery
// or a broken client, and must not produce a session.
func TestSSORejectsATamperedState(t *testing.T) {
	c := newConsoleUnderTest(t)
	r1, _ := c.client.Get(c.srv.URL + "/auth/login?return_to=/")
	r1.Body.Close()
	toIssuer := r1.Header.Get("Location")
	r2, _ := c.client.Get(toIssuer)
	r2.Body.Close()

	cb, _ := url.Parse(r2.Header.Get("Location"))
	q := cb.Query()
	q.Set("state", "forged")
	cb.RawQuery = q.Encode()
	r3, _ := c.client.Get(cb.String())
	r3.Body.Close()

	if !strings.Contains(r3.Header.Get("Location"), "login_failed") {
		t.Errorf("tampered state should redirect with login_failed, got %s", r3.Header.Get("Location"))
	}
	if me := c.me(t); me["authenticated"] == true {
		t.Error("a tampered callback must not leave the browser signed in")
	}
}

// The open-redirect guard on return_to.
func TestSSOReturnToMustBeSameOrigin(t *testing.T) {
	c := newConsoleUnderTest(t)
	for _, bad := range []string{"https://evil.example/", "//evil.example/", "javascript:alert(1)"} {
		final := c.signIn(t, bad)
		if loc := final.Header.Get("Location"); loc != "/" {
			t.Errorf("return_to=%q should be replaced with /, got %q", bad, loc)
		}
	}
}

func TestSSOSignOutClearsTheSession(t *testing.T) {
	c := newConsoleUnderTest(t)
	c.signIn(t, "/")
	if me := c.me(t); me["authenticated"] != true {
		t.Fatal("precondition: signed in")
	}
	res, _ := c.client.Post(c.srv.URL+"/auth/logout", "application/json", nil)
	res.Body.Close()
	if me := c.me(t); me["authenticated"] == true {
		t.Error("still authenticated after sign-out")
	}
}

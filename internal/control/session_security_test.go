package control

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

// The console session after the signature: what has to stop working when an
// account is disabled, demoted, re-passworded, signed out or guessed at.
//
// Each of these failed before the change that added it: a signed cookie was
// trusted for its whole lifetime with the role it was issued with, a second
// factor could be switched off from a stolen session, a code could be spent
// twice, and an account could be guessed at from as many addresses as the
// attacker had.

// browser is one cookie jar against a harness, with its own client address.
type browser struct {
	h      *secHarness
	client *http.Client
	ip     string
}

type secHarness struct {
	api   *API
	srv   *httptest.Server
	store *Store
}

func newSecHarness(t *testing.T) *secHarness {
	t.Helper()
	store := testStore(t)
	api := NewAPI(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	signer, err := auth.NewSigner("a-development-secret-of-sufficient-length")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Close)
	api.SetAuth(signer, "", false)
	// Loopback is the trusted proxy, so each browser below can be a different
	// client address -- the shape of a guess spread over many machines.
	th, err := NewThrottles(ThrottleConfig{TrustedProxies: []string{"127.0.0.1/32", "::1/128"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(th.Close)
	api.SetThrottles(th)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &secHarness{api: api, srv: srv, store: store}
}

func (h *secHarness) browser(ip string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{h: h, client: &http.Client{Jar: jar}, ip: ip}
}

func (b *browser) do(t *testing.T, method, path string, body any, hdr ...string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, b.h.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if b.ip != "" {
		req.Header.Set("X-Forwarded-For", b.ip)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (b *browser) signIn(t *testing.T, email, password string) {
	t.Helper()
	if c, body := b.do(t, "POST", "/auth/password", loginRequest{email, password}); c != 200 || body["mfaRequired"] == true {
		t.Fatalf("sign-in = %d %v", c, body)
	}
}

func (b *browser) authed(t *testing.T) bool {
	t.Helper()
	_, me := b.do(t, "GET", "/auth/me", nil)
	return me["authenticated"] == true
}

// sessionCookie is the browser's current cookie value.
func (b *browser) sessionCookie(t *testing.T) string {
	t.Helper()
	req, _ := http.NewRequest("GET", b.h.srv.URL, nil)
	for _, c := range b.client.Jar.Cookies(req.URL) {
		if c.Name == auth.SessionCookieName {
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

// withCookie is a second browser holding a copy of another's cookie: what an
// attacker has after stealing one.
func (h *secHarness) withCookie(t *testing.T, value string) *browser {
	b := h.browser("")
	u, _ := http.NewRequest("GET", h.srv.URL, nil)
	b.client.Jar.SetCookies(u.URL, []*http.Cookie{{Name: auth.SessionCookieName, Value: value}})
	return b
}

func withMFA(t *testing.T, s *Store, email string) string {
	t.Helper()
	secret, err := s.BeginMFAEnrolment(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	// The previous step, so the current one is still unspent for sign-in.
	code, _ := auth.TOTPCode(secret, time.Now().Add(-auth.TOTPPeriod))
	if _, err := s.ConfirmMFAEnrolment(t.Context(), email, code); err != nil {
		t.Fatal(err)
	}
	return secret
}

/* ── Revocation ──────────────────────────────────────────────────────────── */

func TestDisablingAnAccountEndsItsSessionsAtOnce(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "operator")
	b := h.browser("")
	b.signIn(t, email, password)

	if err := h.store.SetAccountDisabled(t.Context(), email, true); err != nil {
		t.Fatal(err)
	}
	if b.authed(t) {
		t.Fatal("a disabled account's cookie still authenticated")
	}
	// Enabling it again does not bring the old cookie back.
	if err := h.store.SetAccountDisabled(t.Context(), email, false); err != nil {
		t.Fatal(err)
	}
	if b.authed(t) {
		t.Error("re-enabling the account revived a cookie issued before it was disabled")
	}
}

func TestADemotionAppliesToTheNextRequest(t *testing.T) {
	h := newSecHarness(t)
	h.api.RequireMFA = MFAOff
	email, password := newAccount(t, h.store, "admin")
	newAccount(t, h.store, "owner") // so the admin is not the last one
	b := h.browser("")
	b.signIn(t, email, password)

	if _, err := h.store.SetAccountRole(t.Context(), email, "auditor"); err != nil {
		t.Fatal(err)
	}
	_, me := b.do(t, "GET", "/auth/me", nil)
	if me["role"] != "auditor" {
		t.Fatalf("role after demotion = %v, want auditor (the cookie's role was trusted)", me["role"])
	}
	if c, _ := b.do(t, "POST", "/api/v1/users", accountInput{
		Email: unique("x") + "@northwind.id", Role: "admin", Password: "long enough passphrase"}); c != http.StatusForbidden {
		t.Errorf("a demoted admin could still add accounts: %d", c)
	}
}

func TestSigningOutRevokesACopiedCookie(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "operator")
	b := h.browser("")
	b.signIn(t, email, password)
	stolen := h.withCookie(t, b.sessionCookie(t))
	if !stolen.authed(t) {
		t.Fatal("the copied cookie should work before sign-out")
	}

	if c, _ := b.do(t, "POST", "/auth/logout", nil); c != 200 {
		t.Fatalf("logout = %d", c)
	}
	if stolen.authed(t) {
		t.Error("a copy of a signed-out cookie still authenticated")
	}
}

func TestChangingThePasswordSignsOutEveryOtherSession(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "operator")
	mine, theirs := h.browser(""), h.browser("")
	mine.signIn(t, email, password)
	theirs.signIn(t, email, password)

	if c, b := mine.do(t, "POST", "/auth/password/change",
		map[string]string{"current": password, "new": "an entirely new passphrase"}); c != 200 {
		t.Fatalf("change = %d %v", c, b)
	}
	if !mine.authed(t) {
		t.Error("the browser that changed the password was signed out of it")
	}
	if theirs.authed(t) {
		t.Error("another session survived a password change")
	}
}

func TestSignOutEverywhere(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "operator")
	a, b := h.browser(""), h.browser("")
	a.signIn(t, email, password)
	b.signIn(t, email, password)
	if c, _ := a.do(t, "POST", "/auth/sessions/revoke", nil); c != 200 {
		t.Fatalf("revoke = %d", c)
	}
	if a.authed(t) || b.authed(t) {
		t.Error("a session survived signing out everywhere")
	}
	// A fresh sign-in afterwards works.
	a.signIn(t, email, password)
	if !a.authed(t) {
		t.Error("could not sign in again after signing out everywhere")
	}
}

func TestACookieForAnAccountThatDoesNotExistIsRefused(t *testing.T) {
	h := newSecHarness(t)
	tok, _ := h.api.signer.IssueSession(auth.Session{
		Email: unique("ghost") + "@northwind.id", Role: "owner"}, time.Hour)
	if h.withCookie(t, tok).authed(t) {
		t.Error("a signed cookie for no account authenticated")
	}
}

/* ── Second factor ───────────────────────────────────────────────────────── */

// The bug this replaces: POST /auth/mfa/enrol with nothing but a session
// cleared mfa_enrolled_at, so the next sign-in needed only the password.
func TestAStolenSessionCannotSwitchTheSecondFactorOff(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "admin")
	secret := withMFA(t, h.store, email)
	b := h.browser("")
	_, body := b.do(t, "POST", "/auth/password", loginRequest{email, password})
	code, _ := auth.TOTPCode(secret, time.Now())
	if c, _ := b.do(t, "POST", "/auth/mfa", mfaRequest{Challenge: body["challenge"].(string), Code: code}); c != 200 {
		t.Fatalf("sign-in with a code = %d", c)
	}
	thief := h.withCookie(t, b.sessionCookie(t))

	if c, _ := thief.do(t, "POST", "/auth/mfa/enrol", map[string]string{}); c != http.StatusUnauthorized {
		t.Errorf("enrolment began without the password: %d", c)
	}
	if c, _ := thief.do(t, "POST", "/auth/mfa/enrol",
		map[string]string{"password": password, "code": "123456"}); c != http.StatusUnauthorized {
		t.Errorf("an enrolled factor was replaced without a current code: %d", c)
	}
	acct, _ := h.store.Account(t.Context(), email)
	if !acct.MFAEnrolled || acct.TOTPSecret != secret {
		t.Fatal("the account's second factor changed")
	}
	// And a password alone still does not sign in.
	_, body = h.browser("").do(t, "POST", "/auth/password", loginRequest{email, password})
	if body["mfaRequired"] != true {
		t.Error("the password alone signed in an enrolled account")
	}
}

// Beginning a replacement keeps the old factor in force until the new one is
// confirmed.
func TestReEnrolmentKeepsTheOldFactorUntilConfirmed(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "operator")
	secret := withMFA(t, h.store, email)
	if _, err := h.store.BeginMFAEnrolment(t.Context(), email); err != nil {
		t.Fatal(err)
	}
	acct, _ := h.store.Account(t.Context(), email)
	if !acct.MFAEnrolled || acct.TOTPSecret != secret {
		t.Fatal("starting a re-enrolment changed the factor in use")
	}
	_ = password
}

func TestAnAuthenticatorCodeSignsInOnce(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "operator")
	secret := withMFA(t, h.store, email)
	code, _ := auth.TOTPCode(secret, time.Now())

	first := h.browser("")
	_, body := first.do(t, "POST", "/auth/password", loginRequest{email, password})
	if c, _ := first.do(t, "POST", "/auth/mfa", mfaRequest{Challenge: body["challenge"].(string), Code: code}); c != 200 {
		t.Fatalf("first use = %d", c)
	}
	replay := h.browser("")
	_, body = replay.do(t, "POST", "/auth/password", loginRequest{email, password})
	if c, _ := replay.do(t, "POST", "/auth/mfa", mfaRequest{Challenge: body["challenge"].(string), Code: code}); c != http.StatusUnauthorized {
		t.Errorf("the same code signed in twice: %d", c)
	}
}

func TestPrivilegedRolesMustEnrolBeforeAnythingElse(t *testing.T) {
	h := newSecHarness(t)
	admin, apw := newAccount(t, h.store, "admin")
	op, opw := newAccount(t, h.store, "operator")

	a := h.browser("")
	a.signIn(t, admin, apw)
	_, me := a.do(t, "GET", "/auth/me", nil)
	if me["mfaSetupRequired"] != true {
		t.Errorf("an admin without a second factor was not told to enrol: %v", me)
	}
	if c, body := a.do(t, "GET", "/api/v1/users", nil); c != http.StatusForbidden || body["mfaSetupRequired"] != true {
		t.Errorf("an admin without a second factor reached the API: %d %v", c, body)
	}
	// Enrolment itself is open.
	c, body := a.do(t, "POST", "/auth/mfa/enrol", map[string]string{"password": apw})
	if c != 200 {
		t.Fatalf("enrol = %d %v", c, body)
	}
	code, _ := auth.TOTPCode(body["secret"].(string), time.Now())
	if c, b := a.do(t, "POST", "/auth/mfa/confirm", map[string]string{"code": code}); c != 200 {
		t.Fatalf("confirm = %d %v", c, b)
	}
	if c, _ := a.do(t, "GET", "/api/v1/users", nil); c != 200 {
		t.Errorf("after enrolling, the admin is still held: %d", c)
	}

	o := h.browser("")
	o.signIn(t, op, opw)
	if c, _ := o.do(t, "GET", "/api/v1/assets", nil); c != 200 {
		t.Errorf("an operator was held for enrolment under the privileged policy: %d", c)
	}
}

/* ── Brute force ─────────────────────────────────────────────────────────── */

// Wrong codes from many addresses run out the account's budget, and then even
// the right code is refused for a while.
func TestCodesAreLimitedPerAccountAcrossAddresses(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "operator")
	secret := withMFA(t, h.store, email)

	for i := range 5 {
		b := h.browser("198.51.100." + itoa(i+1))
		_, body := b.do(t, "POST", "/auth/password", loginRequest{email, password})
		if c, _ := b.do(t, "POST", "/auth/mfa", mfaRequest{Challenge: body["challenge"].(string), Code: "000000"}); c != http.StatusUnauthorized {
			t.Fatalf("wrong code %d = %d", i, c)
		}
	}
	b := h.browser("198.51.100.99")
	_, body := b.do(t, "POST", "/auth/password", loginRequest{email, password})
	code, _ := auth.TOTPCode(secret, time.Now())
	if c, _ := b.do(t, "POST", "/auth/mfa", mfaRequest{Challenge: body["challenge"].(string), Code: code}); c != http.StatusTooManyRequests {
		t.Errorf("codes were not limited per account: %d", c)
	}
	var n int
	if err := h.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events
		WHERE action = 'auth.account_throttled' AND actor_email = $1`, email).Scan(&n); err != nil || n != 1 {
		t.Errorf("throttling the account was audited %d times (%v), want once", n, err)
	}
}

// A botnet guessing one account's password is refused once the account's
// budget is spent -- but the account holder's own browser is not, so the
// limit cannot be used to lock an administrator out.
func TestPasswordsAreLimitedPerAccountButNotForAKnownBrowser(t *testing.T) {
	h := newSecHarness(t)
	h.api.RequireMFA = MFAOff
	email, password := newAccount(t, h.store, "admin")
	newAccount(t, h.store, "owner")

	mine := h.browser("203.0.113.7")
	mine.signIn(t, email, password)

	for i := range 10 {
		b := h.browser("198.51.100." + itoa(i+1))
		if c, _ := b.do(t, "POST", "/auth/password", loginRequest{email, "guess " + itoa(i)}); c != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d", i, c)
		}
	}
	stranger := h.browser("198.51.100.200")
	if c, _ := stranger.do(t, "POST", "/auth/password", loginRequest{email, password}); c != http.StatusTooManyRequests {
		t.Errorf("an unknown browser was let through a throttled account: %d", c)
	}
	// The account holder signs in from their own browser regardless.
	if c, body := mine.do(t, "POST", "/auth/password", loginRequest{email, password}); c != 200 {
		t.Errorf("the account holder was locked out by someone else's guesses: %d %v", c, body)
	}
}

/* ── Cross-site requests ─────────────────────────────────────────────────── */

func TestCrossSiteWritesAreRefused(t *testing.T) {
	h := newSecHarness(t)
	email, password := newAccount(t, h.store, "operator")
	b := h.browser("")
	b.signIn(t, email, password)

	if c, _ := b.do(t, "POST", "/auth/logout", nil, "Origin", "https://evil.example"); c != http.StatusForbidden {
		t.Errorf("a write from another origin = %d, want 403", c)
	}
	if c, _ := b.do(t, "POST", "/auth/logout", nil, "Sec-Fetch-Site", "same-site"); c != http.StatusForbidden {
		t.Errorf("a write from a sibling site = %d, want 403", c)
	}
	if !b.authed(t) {
		t.Fatal("a refused cross-site sign-out signed the browser out")
	}
	if c, _ := b.do(t, "POST", "/auth/logout", nil, "Origin", h.srv.URL); c != 200 {
		t.Errorf("a write from the console's own origin = %d", c)
	}
}

/* ── Owners ──────────────────────────────────────────────────────────────── */

func TestAnAdminCannotDisableOrResetAnOwner(t *testing.T) {
	h := newSecHarness(t)
	h.api.RequireMFA = MFAOff
	admin, pw := newAccount(t, h.store, "admin")
	owner, _ := newAccount(t, h.store, "owner")
	b := h.browser("")
	b.signIn(t, admin, pw)

	if c, _ := b.do(t, "POST", "/api/v1/users/"+owner+"/disable", nil); c != http.StatusForbidden {
		t.Errorf("an admin disabled an owner: %d", c)
	}
	if c, _ := b.do(t, "POST", "/api/v1/users/"+owner+"/reset-mfa", nil); c != http.StatusForbidden {
		t.Errorf("an admin reset an owner's second factor: %d", c)
	}
	if c, _ := b.do(t, "POST", "/api/v1/users/"+admin+"/reset-mfa", nil); c != http.StatusBadRequest {
		t.Errorf("an admin reset their own second factor from a session: %d", c)
	}
}

func TestAnAdminResetsSomeonesLostSecondFactor(t *testing.T) {
	h := newSecHarness(t)
	h.api.RequireMFA = MFAOff
	admin, pw := newAccount(t, h.store, "admin")
	op, opw := newAccount(t, h.store, "operator")
	withMFA(t, h.store, op)
	b := h.browser("")
	b.signIn(t, admin, pw)
	if c, body := b.do(t, "POST", "/api/v1/users/"+op+"/reset-mfa", nil); c != 200 {
		t.Fatalf("reset = %d %v", c, body)
	}
	acct, _ := h.store.Account(t.Context(), op)
	if acct.MFAEnrolled || acct.TOTPSecret != "" {
		t.Error("the second factor is still there")
	}
	if n, _ := h.store.RemainingRecoveryCodes(t.Context(), acct.ID); n != 0 {
		t.Errorf("%d recovery codes survived the reset", n)
	}
	// They sign in with their password and enrol again.
	h.browser("").signIn(t, op, opw)
	var n int
	if err := h.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events
		WHERE action = 'auth.mfa_reset' AND target = $1 AND actor_email = $2`, op, admin).Scan(&n); err != nil || n != 1 {
		t.Errorf("the reset was audited %d times (%v), want once under the admin who did it", n, err)
	}
}

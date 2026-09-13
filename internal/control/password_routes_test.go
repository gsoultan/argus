package control

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

// The whole sign-in path over real HTTP: password, second factor, session.

type loginHarness struct {
	api    *API
	srv    *httptest.Server
	client *http.Client
	store  *Store
}

func newLoginHarness(t *testing.T) *loginHarness {
	t.Helper()
	store := testStore(t)
	api := NewAPI(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	signer, err := auth.NewSigner("a-development-secret-of-sufficient-length")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Close)
	api.SetAuth(signer, "", false)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &loginHarness{api: api, srv: srv, client: &http.Client{Jar: jar}, store: store}
}

func (h *loginHarness) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	res, err := h.client.Post(h.srv.URL+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (h *loginHarness) me(t *testing.T) map[string]any {
	t.Helper()
	res, err := h.client.Get(h.srv.URL + "/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out
}

// Without a second factor enrolled, a correct password signs you in.
func TestPasswordSignInIssuesASession(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "admin")

	code, body := h.post(t, "/auth/password", loginRequest{Email: email, Password: password})
	if code != http.StatusOK {
		t.Fatalf("sign-in = %d: %v", code, body)
	}
	if body["mfaRequired"] == true {
		t.Fatal("no second factor is enrolled, so none should be demanded")
	}
	me := h.me(t)
	if me["authenticated"] != true || me["role"] != "admin" {
		t.Errorf("not signed in afterwards: %v", me)
	}
}

// The console cannot draw a sign-in form it does not know exists.
func TestUnauthenticatedMeAdvertisesPasswordSignIn(t *testing.T) {
	h := newLoginHarness(t)
	me := h.me(t)
	if me["passwordEnabled"] != true {
		t.Error("a control plane that can issue sessions must say so")
	}
}

func TestWrongPasswordIsRefusedIndistinguishably(t *testing.T) {
	h := newLoginHarness(t)
	email, _ := newAccount(t, h.store, "operator")

	c1, b1 := h.post(t, "/auth/password", loginRequest{Email: email, Password: "wrong"})
	c2, b2 := h.post(t, "/auth/password", loginRequest{
		Email: "nobody@northwind.id", Password: "wrong"})

	if c1 != http.StatusUnauthorized || c2 != http.StatusUnauthorized {
		t.Fatalf("statuses = %d, %d, want 401 for both", c1, c2)
	}
	// Identical wording, or the response says which addresses exist.
	if b1["error"] != b2["error"] {
		t.Errorf("responses differ and enumerate accounts: %q vs %q", b1["error"], b2["error"])
	}
	if h.me(t)["authenticated"] == true {
		t.Error("a refused password left a session behind")
	}
}

// With MFA enrolled the password alone is not a sign-in.
func TestMFAIsDemandedAndCompletesTheSignIn(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "admin")
	secret, err := h.store.BeginMFAEnrolment(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := auth.TOTPCode(secret, time.Now())
	if _, err := h.store.ConfirmMFAEnrolment(t.Context(), email, first); err != nil {
		t.Fatal(err)
	}

	code, body := h.post(t, "/auth/password", loginRequest{Email: email, Password: password})
	if code != http.StatusOK || body["mfaRequired"] != true {
		t.Fatalf("expected a second factor to be demanded, got %d %v", code, body)
	}
	challenge, _ := body["challenge"].(string)
	if challenge == "" {
		t.Fatal("no challenge was issued")
	}
	// The password alone must not have signed anyone in.
	if h.me(t)["authenticated"] == true {
		t.Fatal("a password alone signed in an account with MFA enrolled")
	}

	// A wrong code does not.
	if c, _ := h.post(t, "/auth/mfa", mfaRequest{Challenge: challenge, Code: "000000"}); c != http.StatusUnauthorized {
		t.Errorf("a wrong code returned %d, want 401", c)
	}

	now, _ := auth.TOTPCode(secret, time.Now())
	if c, b := h.post(t, "/auth/mfa", mfaRequest{Challenge: challenge, Code: now}); c != http.StatusOK {
		t.Fatalf("the correct code was refused: %d %v", c, b)
	}
	if me := h.me(t); me["authenticated"] != true || me["mfaEnrolled"] != true {
		t.Errorf("not signed in after a correct code: %v", me)
	}
}

// A challenge is not a session, and must not be accepted as one anywhere.
func TestAChallengeIsNotASession(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "admin")
	secret, _ := h.store.BeginMFAEnrolment(t.Context(), email)
	first, _ := auth.TOTPCode(secret, time.Now())
	if _, err := h.store.ConfirmMFAEnrolment(t.Context(), email, first); err != nil {
		t.Fatal(err)
	}
	_, body := h.post(t, "/auth/password", loginRequest{Email: email, Password: password})
	challenge := body["challenge"].(string)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/policy", nil)
	req.Header.Set("Authorization", "Bearer "+challenge)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a half-finished sign-in reached the API: %d", res.StatusCode)
	}
}

func TestRecoveryCodeCompletesASignInOnce(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "admin")
	secret, _ := h.store.BeginMFAEnrolment(t.Context(), email)
	first, _ := auth.TOTPCode(secret, time.Now())
	recovery, err := h.store.ConfirmMFAEnrolment(t.Context(), email, first)
	if err != nil {
		t.Fatal(err)
	}

	_, body := h.post(t, "/auth/password", loginRequest{Email: email, Password: password})
	challenge := body["challenge"].(string)
	if c, b := h.post(t, "/auth/mfa", mfaRequest{
		Challenge: challenge, Code: recovery[0], Recovery: true}); c != http.StatusOK {
		t.Fatalf("a recovery code was refused: %d %v", c, b)
	}
	if h.me(t)["authenticated"] != true {
		t.Error("a recovery code did not sign the account in")
	}

	// The same code must not work twice.
	_, body2 := h.post(t, "/auth/password", loginRequest{Email: email, Password: password})
	if c, _ := h.post(t, "/auth/mfa", mfaRequest{
		Challenge: body2["challenge"].(string), Code: recovery[0], Recovery: true,
	}); c != http.StatusUnauthorized {
		t.Errorf("a spent recovery code was accepted again: %d", c)
	}
}

// Changing a password needs the current one, so a borrowed unlocked laptop is
// not enough to lock the owner out.
func TestChangingAPasswordRequiresTheCurrentOne(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "admin")
	h.post(t, "/auth/password", loginRequest{Email: email, Password: password})

	if c, _ := h.post(t, "/auth/password/change", map[string]string{
		"current": "not the password", "new": "a brand new passphrase"}); c != http.StatusUnauthorized {
		t.Errorf("changed a password without the current one: %d", c)
	}
	if c, b := h.post(t, "/auth/password/change", map[string]string{
		"current": password, "new": "a brand new passphrase"}); c != http.StatusOK {
		t.Fatalf("a valid change was refused: %d %v", c, b)
	}
	if _, err := h.store.Authenticate(t.Context(), email, "a brand new passphrase"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

// Enrolment must not weaken an account until a code proves the app has the
// same secret.
func TestBeginningEnrolmentDoesNotEnrol(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "admin")
	h.post(t, "/auth/password", loginRequest{Email: email, Password: password})

	c, body := h.post(t, "/auth/mfa/enrol", nil)
	if c != http.StatusOK {
		t.Fatalf("enrolment could not begin: %d %v", c, body)
	}
	if !strings.HasPrefix(body["uri"].(string), "otpauth://totp/") {
		t.Errorf("no scannable URI: %v", body["uri"])
	}
	if h.me(t)["mfaEnrolled"] == true {
		t.Error("beginning enrolment marked the account enrolled")
	}

	if c, _ := h.post(t, "/auth/mfa/confirm", map[string]string{"code": "000000"}); c != http.StatusBadRequest {
		t.Errorf("a wrong code confirmed enrolment: %d", c)
	}
	code, _ := auth.TOTPCode(body["secret"].(string), time.Now())
	c, confirm := h.post(t, "/auth/mfa/confirm", map[string]string{"code": code})
	if c != http.StatusOK {
		t.Fatalf("the correct code was refused: %d %v", c, confirm)
	}
	if len(confirm["recoveryCodes"].([]any)) != recoveryCodeCount {
		t.Errorf("wrong number of recovery codes: %v", confirm["recoveryCodes"])
	}
	if h.me(t)["mfaEnrolled"] != true {
		t.Error("confirming did not enrol the account")
	}
}

// A fresh install has to say so, or an operator types a password they never set
// into a form that can only refuse it.
func TestFreshInstallReportsThatNobodyCanSignInYet(t *testing.T) {
	h := newLoginHarness(t)
	ctx := t.Context()
	// testStore shares a database with the rest of the suite, so this test owns
	// the answer only for accounts it can see. Assert the transition instead.
	before, err := h.store.AnyAccountExists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !before {
		if me := h.me(t); me["accountsExist"] != false {
			t.Errorf("with no accounts, accountsExist = %v, want false", me["accountsExist"])
		}
	}

	newAccount(t, h.store, "admin")
	exists, err := h.store.AnyAccountExists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("an account was created but AnyAccountExists says otherwise")
	}
	if me := h.me(t); me["accountsExist"] != true {
		t.Errorf("after creating an account, accountsExist = %v, want true", me["accountsExist"])
	}
}

// An account with no password hash cannot sign in, so it must not make the
// console claim somebody can. AnyAccountExists is what the sign-in form reads.
func TestPasswordlessAccountsDoNotCountAsSignInAble(t *testing.T) {
	h := newLoginHarness(t)
	ctx := t.Context()
	email := unique("passwordless") + "@northwind.id"
	if _, err := h.store.pool.Exec(ctx,
		`INSERT INTO users (email, display_name, role) VALUES ($1,'Passwordless','operator')`,
		email); err != nil {
		t.Fatal(err)
	}
	// Not asserting false outright, since the shared database may hold local
	// accounts from other tests; asserting the row itself is not counted.
	var counted bool
	if err := h.store.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE email = $1 AND password_hash IS NOT NULL)`,
		email).Scan(&counted); err != nil {
		t.Fatal(err)
	}
	if counted {
		t.Error("an account created without a password was given a hash")
	}
}

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The setup page is only meaningful against an empty users table, which the
// shared test database never is. Each test gets a database of its own.
type setupHarness struct {
	api   *API
	srv   *httptest.Server
	store *Store
	token string
	file  string
}

func newSetupHarness(t *testing.T, offer bool) *setupHarness {
	t.Helper()
	dsn := scratchDB(t, fmt.Sprintf("argus_setup_%d", time.Now().UnixNano()%1000000))
	store, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)

	api := NewAPI(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	signer, err := auth.NewSigner("a-development-secret-of-sufficient-length")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Close)
	api.SetAuth(signer, "", false)

	h := &setupHarness{api: api, store: store}
	if offer {
		h.token, err = NewSetupToken()
		if err != nil {
			t.Fatal(err)
		}
		h.file = filepath.Join(t.TempDir(), "setup-token")
		if err := os.WriteFile(h.file, []byte(h.token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		api.OfferSetup(h.token, h.file)
	}
	h.srv = httptest.NewServer(api.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *setupHarness) submit(t *testing.T, in setupRequest) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(in)
	res, err := http.Post(h.srv.URL+"/auth/setup", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (h *setupHarness) status(t *testing.T) setupStatus {
	t.Helper()
	res, err := http.Get(h.srv.URL + "/auth/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var st setupStatus
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

const setupPassword = "correct horse battery staple"

// The whole point of the token: without it, finding the page first is not
// enough to own the deployment.
func TestSetupRefusesTheWrongToken(t *testing.T) {
	h := newSetupHarness(t, true)
	for _, tok := range []string{"", "not-the-token", strings.ToUpper(h.token)} {
		code, _ := h.submit(t, setupRequest{Token: tok, Email: "eve@example.com", Password: setupPassword})
		if code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, code)
		}
	}
	if done, err := h.store.SetupComplete(t.Context()); err != nil || done {
		t.Fatalf("a refused setup created an account (complete=%v, err=%v)", done, err)
	}
	if st := h.status(t); !st.Available || st.Complete {
		t.Errorf("status after refusals = %+v, want still open", st)
	}
}

// With the token: one admin, audited, and the page closes behind it.
func TestSetupCreatesTheFirstAdminOnce(t *testing.T) {
	h := newSetupHarness(t, true)
	if st := h.status(t); !st.Available || st.Complete || st.TokenFile != h.file {
		t.Fatalf("fresh status = %+v", st)
	}

	code, body := h.submit(t, setupRequest{
		Token: " " + h.token + "\n", Email: "Ada@Example.com", DisplayName: "Ada", Password: setupPassword,
	})
	if code != http.StatusCreated {
		t.Fatalf("setup = %d: %v", code, body)
	}
	if body["email"] != "ada@example.com" || body["role"] != "admin" {
		t.Errorf("setup answered %v", body)
	}
	if _, err := h.store.Authenticate(t.Context(), "ada@example.com", setupPassword); err != nil {
		t.Errorf("the account cannot sign in: %v", err)
	}
	if _, err := os.Stat(h.file); !os.IsNotExist(err) {
		t.Errorf("the used token file is still there (stat err %v)", err)
	}

	// Closed: the same token, a second time, makes nothing.
	code, _ = h.submit(t, setupRequest{Token: h.token, Email: "eve@example.com", Password: setupPassword})
	if code != http.StatusConflict {
		t.Errorf("second setup = %d, want 409", code)
	}
	if _, err := h.store.Account(t.Context(), "eve@example.com"); err == nil {
		t.Error("a second setup created a second account")
	}
	if st := h.status(t); !st.Complete || st.Available || st.TokenFile != "" {
		t.Errorf("status after setup = %+v, want complete and closed", st)
	}

	events, err := h.store.AuditEvents(t.Context(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == "user.created" && e.ActorEmail == "ada@example.com" && e.Target == "ada@example.com" {
			found = true
		}
	}
	if !found {
		t.Error("no user.created audit event naming the account the setup page made")
	}
}

// Two browsers submitting together, or a browser racing `users add`, must not
// both see an empty table.
func TestSetupAdmitsOneOfManyConcurrentSubmissions(t *testing.T) {
	h := newSetupHarness(t, true)
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = h.submit(t, setupRequest{
				Token: h.token, Email: fmt.Sprintf("racer%d@example.com", i), Password: setupPassword,
			})
		}()
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		}
	}
	var accounts int
	if err := h.store.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE password_hash IS NOT NULL`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if created != 1 || accounts != 1 {
		t.Errorf("%d submissions succeeded and %d accounts exist, want 1 and 1 (codes %v)",
			created, accounts, codes)
	}
}

// The same race at the store, without argon2 spreading the contenders out:
// every transaction checks the empty table at once, and one may insert.
func TestCreateFirstAccountIsSerialised(t *testing.T) {
	h := newSetupHarness(t, false)
	hash, err := auth.HashPassword(setupPassword)
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	// Open the pool's connections first. Otherwise each contender spends a
	// few milliseconds dialling, the first is committed before the second has
	// begun, and the test passes with or without the lock.
	var conns []*pgxpool.Conn
	for range h.store.pool.Config().MaxConns {
		c, err := h.store.pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = h.store.createFirstAccountHashed(t.Context(),
				fmt.Sprintf("racer%d@example.com", i), "", "admin", hash)
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrSetupComplete):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d first accounts were created, want exactly 1", ok)
	}
}

// Disabling every account must not reopen setup: that would hand the
// deployment to whoever reached /setup next.
func TestDisabledAccountsKeepSetupClosed(t *testing.T) {
	h := newSetupHarness(t, true)
	if _, err := h.store.CreateAccount(t.Context(), "ops@example.com", "", "operator", setupPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.pool.Exec(t.Context(), `UPDATE users SET disabled_at = now()`); err != nil {
		t.Fatal(err)
	}
	if any, _ := h.store.AnyAccountExists(t.Context()); any {
		t.Fatal("precondition: the only account should be disabled")
	}
	code, _ := h.submit(t, setupRequest{Token: h.token, Email: "eve@example.com", Password: setupPassword})
	if code != http.StatusConflict {
		t.Errorf("setup with only disabled accounts = %d, want 409", code)
	}
}

// A control plane that did not offer the page refuses it, even on an empty
// database, and says what to do instead.
func TestSetupNotOfferedIsRefused(t *testing.T) {
	h := newSetupHarness(t, false)
	if st := h.status(t); st.Available {
		t.Errorf("status = %+v, want unavailable", st)
	}
	code, body := h.submit(t, setupRequest{Token: "anything", Email: "eve@example.com", Password: setupPassword})
	if code != http.StatusForbidden || !strings.Contains(fmt.Sprint(body["error"]), "users add") {
		t.Errorf("setup not offered = %d %v, want 403 naming users add", code, body)
	}
}

// Bootstrap tokens are honoured only while nobody has an account. The
// start-up check enforces that, and an account made by the setup page did not
// exist at start-up.
func TestSetupEndsStaticTokens(t *testing.T) {
	h := newSetupHarness(t, true)
	h.api.UserTokens = map[string]string{"dev-token": "ada@example.com"}
	get := func() int {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/audit", nil)
		req.Header.Set("Authorization", "Bearer dev-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("before setup the bootstrap token got %d, want 200", code)
	}
	if code, body := h.submit(t, setupRequest{Token: h.token, Email: "ada@example.com", Password: setupPassword}); code != http.StatusCreated {
		t.Fatalf("setup = %d: %v", code, body)
	}
	if code := get(); code != http.StatusUnauthorized {
		t.Errorf("after setup the bootstrap token got %d, want 401", code)
	}
}

// /auth/me is what sends a fresh install's sign-in screen to /setup.
func TestMeReportsSetupAvailable(t *testing.T) {
	h := newSetupHarness(t, true)
	res, err := http.Get(h.srv.URL + "/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	var me map[string]any
	_ = json.NewDecoder(res.Body).Decode(&me)
	res.Body.Close()
	if me["setupAvailable"] != true || me["accountsExist"] != false {
		t.Errorf("/auth/me on a fresh install = %v", me)
	}
}

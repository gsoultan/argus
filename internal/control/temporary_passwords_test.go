package control

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/notify"
)

func auditCount(t *testing.T, s *Store, action, target string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`, action, target).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// withMail gives a security harness an email channel.
func withMail(t *testing.T, h *secHarness) (*mailbox, func()) {
	t.Helper()
	email, mb := newMailbox(t)
	n := notify.NewSignInNotifier(email, nil, h.srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.api.SignInAlerts = n
	h.api.SignInPolicy = SignInAlertPolicy{}
	var once sync.Once
	drain := func() { once.Do(func() { n.Close(context.Background()) }) }
	t.Cleanup(drain)
	return mb, drain
}

var mailedPassword = regexp.MustCompile(`Temporary password: (\S+)`)

// adminBrowser is a signed-in administrator, with a second factor so the
// default policy does not hold them.
func adminBrowser(t *testing.T, h *secHarness) (*browser, string) {
	t.Helper()
	admin, pw := newAccount(t, h.store, "admin")
	secret := withMFA(t, h.store, admin)
	b := h.browser("")
	c, body := b.do(t, "POST", "/auth/password", loginRequest{admin, pw})
	if c != 200 || body["mfaRequired"] != true {
		t.Fatalf("admin sign-in = %d %v", c, body)
	}
	code, _ := auth.TOTPCode(secret, time.Now())
	if c, body := b.do(t, "POST", "/auth/mfa", mfaRequest{Challenge: body["challenge"].(string), Code: code}); c != 200 {
		t.Fatalf("admin code = %d %v", c, body)
	}
	return b, admin
}

// A person added from the console is emailed a temporary password, which
// the administrator never sees; it signs them in to nothing but "choose your
// own password", that password must be strong, and after it the second
// factor is owed before anything else.
func TestAPersonAddedIsEmailedATemporaryPasswordTheyMustReplace(t *testing.T) {
	h := newSecHarness(t)
	mb, _ := withMail(t, h)
	admin, _ := adminBrowser(t, h)

	person := unique("new") + "@northwind.id"
	dropAccount(t, h.store, person)
	c, out := admin.do(t, "POST", "/api/v1/users", accountInput{Email: person, DisplayName: "Rani Putri", Role: "operator"})
	if c != http.StatusCreated || out["emailed"] != true {
		t.Fatalf("add = %d %v", c, out)
	}
	if _, leaked := out["temporaryPassword"]; leaked {
		t.Fatalf("the administrator was shown a password that was emailed: %v", out)
	}
	mail := mb.for_(person)
	if len(mail) != 1 {
		t.Fatalf("%d emails to the new person", len(mail))
	}
	m := mailedPassword.FindStringSubmatch(mail[0])
	if m == nil {
		t.Fatalf("no password in the email:\n%s", mail[0])
	}
	temp := m[1]
	if auditCount(t, h.store, "user.created", person) != 1 {
		t.Fatal("adding the person was not audited")
	}
	var detail string
	_ = h.store.pool.QueryRow(context.Background(),
		`SELECT detail FROM audit_events WHERE action = 'user.created' AND target = $1`, person).Scan(&detail)
	if strings.Contains(detail, temp) {
		t.Fatal("the temporary password is in the audit log")
	}

	p := h.browser("")
	p.signIn(t, person, temp)
	_, me := p.do(t, "GET", "/auth/me", nil)
	if me["passwordChangeRequired"] != true {
		t.Fatalf("a temporary password did not ask for a new one: %v", me)
	}
	if c, body := p.do(t, "GET", "/api/v1/assets", nil); c != http.StatusForbidden || body["passwordChangeRequired"] != true {
		t.Fatalf("a temporary password reached the API: %d %v", c, body)
	}
	// Nor can it set up the second factor: whoever reads the temporary
	// password must not be the one who enrols.
	if c, _ := p.do(t, "POST", "/auth/mfa/enrol", map[string]string{"password": temp}); c != http.StatusForbidden {
		t.Fatalf("enrolment with a temporary password = %d", c)
	}
	for _, weak := range []string{"Password2026!", "rani.putri.2026X", "aaaaaaaaaaaa1A"} {
		if c, body := p.do(t, "POST", "/auth/password/change", map[string]string{"current": temp, "new": weak}); c != http.StatusBadRequest {
			t.Errorf("weak %q accepted: %d %v", weak, c, body)
		}
	}
	mine := "Kopi-Tubruk-Senja-7"
	if c, body := p.do(t, "POST", "/auth/password/change", map[string]string{"current": temp, "new": mine}); c != 200 {
		t.Fatalf("change = %d %v", c, body)
	}
	_, me = p.do(t, "GET", "/auth/me", nil)
	if me["passwordChangeRequired"] == true || me["mfaSetupRequired"] != true {
		t.Fatalf("after choosing a password, want only the second factor owed: %v", me)
	}
	if c, _ := p.do(t, "GET", "/api/v1/assets", nil); c != http.StatusForbidden {
		t.Fatalf("an operator without a second factor reached the API under the default policy: %d", c)
	}
	// The temporary password is spent.
	if c, _ := h.browser("").do(t, "POST", "/auth/password", loginRequest{person, temp}); c != http.StatusUnauthorized {
		t.Fatalf("the temporary password still signs in: %d", c)
	}
}

// Unused in time, a temporary password stops working, and says why.
func TestAnExpiredTemporaryPasswordIsRefused(t *testing.T) {
	h := newSecHarness(t)
	person := unique("late") + "@northwind.id"
	dropAccount(t, h.store, person)
	past := time.Now().Add(-time.Minute)
	temp, _ := auth.NewTemporaryPassword()
	if _, err := h.store.CreateAccountWith(t.Context(), NewAccount{Email: person, Role: "operator",
		Password: temp, TemporaryUntil: &past}); err != nil {
		t.Fatal(err)
	}
	c, body := h.browser("").do(t, "POST", "/auth/password", loginRequest{person, temp})
	if c != http.StatusUnauthorized || !strings.Contains(body["error"].(string), "expired") {
		t.Fatalf("expired temporary password = %d %v", c, body)
	}
}

// Without email, the first password is shown to the administrator once; a
// new one is shown only to someone who never chose their own, because for
// anyone else it would let the administrator sign in as them.
func TestWithoutEmailANewPasswordIsShownOnlyForAnInvitation(t *testing.T) {
	h := newSecHarness(t)
	admin, _ := adminBrowser(t, h)

	invited := unique("inv") + "@northwind.id"
	dropAccount(t, h.store, invited)
	c, out := admin.do(t, "POST", "/api/v1/users", accountInput{Email: invited, Role: "operator"})
	if c != http.StatusCreated || out["emailed"] == true || out["temporaryPassword"] == "" || out["temporaryPassword"] == nil {
		t.Fatalf("add without email = %d %v", c, out)
	}
	if c, out := admin.do(t, "POST", "/api/v1/users/"+invited+"/temporary-password", nil); c != 200 || out["temporaryPassword"] == nil {
		t.Fatalf("a fresh invitation = %d %v", c, out)
	}

	settled, _ := newAccount(t, h.store, "operator")
	if c, out := admin.do(t, "POST", "/api/v1/users/"+settled+"/temporary-password", nil); c != http.StatusConflict {
		t.Fatalf("a new password for someone with their own was shown to the admin: %d %v", c, out)
	}
	if auditCount(t, h.store, "user.temporary_password", settled) != 0 {
		t.Fatal("a refused reset was audited as done")
	}
}

// With email, a new temporary password signs the person out, keeps their
// second factor, and goes only to them.
func TestANewTemporaryPasswordGoesOnlyToThePerson(t *testing.T) {
	h := newSecHarness(t)
	mb, _ := withMail(t, h)
	admin, adminEmail := adminBrowser(t, h)

	person, pw := newAccount(t, h.store, "operator")
	withMFA(t, h.store, person)
	c, out := admin.do(t, "POST", "/api/v1/users/"+person+"/temporary-password", nil)
	if c != 200 || out["emailed"] != true || out["temporaryPassword"] != nil {
		t.Fatalf("reset = %d %v", c, out)
	}
	if len(mb.for_(person)) != 1 || len(mb.for_(adminEmail)) != 0 {
		t.Fatalf("mail: person %d, admin %d", len(mb.for_(person)), len(mb.for_(adminEmail)))
	}
	if c, _ := h.browser("").do(t, "POST", "/auth/password", loginRequest{person, pw}); c != http.StatusUnauthorized {
		t.Fatal("the old password still works")
	}
	acct, _ := h.store.Account(t.Context(), person)
	if !acct.MFAEnrolled || !acct.PasswordChangeRequired {
		t.Fatalf("after a reset: mfa %v, change required %v", acct.MFAEnrolled, acct.PasswordChangeRequired)
	}
	if auditCount(t, h.store, "user.temporary_password", person) != 1 {
		t.Fatal("the reset was not audited")
	}
	// Nobody resets their own this way.
	if c, _ := admin.do(t, "POST", "/api/v1/users/"+adminEmail+"/temporary-password", nil); c != http.StatusBadRequest {
		t.Fatalf("an admin reset their own password: %d", c)
	}
}

/* ── Session binding ───────────────────────────────────────────────────── */

const (
	uaChrome  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	uaChrome2 = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36"
	uaCurl    = "curl/8.5.0"
)

// A cookie copied out of the browser it was issued to is refused from
// anything else, and the copy's use signs that session out for the browser
// too -- the cookie is known to be loose. A browser updating itself is not a
// change.
func TestACookieCopiedIntoAnotherBrowserIsRefusedAndRevoked(t *testing.T) {
	h := newSecHarness(t)
	h.api.RequireMFA = MFAOff
	email, pw := newAccount(t, h.store, "operator")
	b := h.browser("")
	if c, _ := b.do(t, "POST", "/auth/password", loginRequest{email, pw}, "User-Agent", uaChrome); c != 200 {
		t.Fatalf("sign-in = %d", c)
	}
	if c, _ := b.do(t, "GET", "/api/v1/assets", nil, "User-Agent", uaChrome2); c != 200 {
		t.Fatalf("the same browser, one version on, was refused: %d", c)
	}
	thief := h.withCookie(t, b.sessionCookie(t))
	if c, _ := thief.do(t, "GET", "/api/v1/assets", nil, "User-Agent", uaCurl); c != http.StatusUnauthorized {
		t.Fatalf("a copied cookie worked from curl: %d", c)
	}
	if c, _ := b.do(t, "GET", "/api/v1/assets", nil, "User-Agent", uaChrome); c != http.StatusUnauthorized {
		t.Fatalf("a cookie known to be copied still works in the browser: %d", c)
	}
	if auditCount(t, h.store, "auth.session_hijack_refused", "console") < 1 {
		t.Fatal("the refused cookie was not audited")
	}

	// Turned off, a cookie works from anywhere, as it did before.
	h.api.AllowBrowserChange = true
	b2 := h.browser("")
	if c, _ := b2.do(t, "POST", "/auth/password", loginRequest{email, pw}, "User-Agent", uaChrome); c != 200 {
		t.Fatal("sign-in")
	}
	if c, _ := h.withCookie(t, b2.sessionCookie(t)).do(t, "GET", "/api/v1/assets", nil, "User-Agent", uaCurl); c != 200 {
		t.Fatalf("with the check off, a cookie elsewhere = %d", c)
	}
}

/* ── Server connected ──────────────────────────────────────────────────── */

// A server's first connection tells administrators; a reconnection does not
// unless asked for, and then at most once per quiet window.
func TestAServerConnectingTellsAdministratorsOnce(t *testing.T) {
	h := newSecHarness(t)
	mb, drain := withMail(t, h)
	admin, _ := newAccount(t, h.store, "admin")
	ctx := WithTenant(context.Background(), defaultTenant())
	host := unique("srv") + ".northwind.id"

	h.api.ServerConnected(ctx, ServerConnection{Host: host, First: true, Transport: "quic", RemoteAddr: "203.0.113.9"})
	h.api.ServerConnected(ctx, ServerConnection{Host: host})
	h.api.ServerAlerts = ServerAlertEvery
	h.api.ServerConnected(ctx, ServerConnection{Host: host})
	h.api.ServerConnected(ctx, ServerConnection{Host: host})
	h.api.ServerAlerts = ServerAlertOff
	h.api.ServerConnected(ctx, ServerConnection{Host: host, First: true})
	drain()

	got := mb.for_(admin)
	if len(got) != 2 {
		t.Fatalf("%d emails, want first connection and one reconnection", len(got))
	}
	if !strings.Contains(got[0], host+" connected to Argus") || !strings.Contains(got[0], "203.0.113.9") {
		t.Fatalf("first email:\n%s", got[0])
	}
	if !strings.Contains(got[1], "connected again") {
		t.Fatalf("second email:\n%s", got[1])
	}
}

// The first tunnel a credential opens is the first; later ones are not.
func TestOnlyTheFirstTunnelIsTheFirst(t *testing.T) {
	e := newTunnelEnv(t)
	_, id := enrolled(t, e.store, e.h, unique("tun-first"))
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Microsecond)
	first, err := e.store.tunnelConnected(ctx, id, "quic", "r1", at)
	if err != nil || !first {
		t.Fatalf("first connection: %v %v", first, err)
	}
	again, err := e.store.tunnelConnected(ctx, id, "quic", "r1", at.Add(time.Second))
	if err != nil || again {
		t.Fatalf("second connection called first: %v %v", again, err)
	}
}

/* ── Location ──────────────────────────────────────────────────────────── */

// The place is read from a trusted proxy's headers and nobody else's, and
// private addresses are named as such.
func TestLocationComesOnlyFromATrustedProxy(t *testing.T) {
	trusted := false
	l := NewLocator(LocationProxy, func(*http.Request) bool { return trusted }, nil)
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("CF-IPCity", "Jakarta")
	r.Header.Set("CF-Region", "Jakarta")
	r.Header.Set("CF-IPCountry", "ID")
	if got := l.Locate(t.Context(), r, "203.0.113.7"); got != "" {
		t.Fatalf("believed headers from an untrusted client: %q", got)
	}
	trusted = true
	if got := l.Locate(t.Context(), r, "203.0.113.7"); got != "Jakarta, ID" {
		t.Fatalf("got %q", got)
	}
	if got := l.Locate(t.Context(), r, "10.1.2.3"); got != "a private network" {
		t.Fatalf("private address: %q", got)
	}
	r.Header.Set("CF-IPCity", "<script>")
	if got := l.Locate(t.Context(), r, "203.0.113.7"); strings.Contains(got, "<") {
		t.Fatalf("markup passed through: %q", got)
	}
	if NewLocator(LocationOff, nil, nil) != nil {
		t.Fatal("off is not nil")
	}
}

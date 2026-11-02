package control

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/notify"
)

// mailbox is a fake SMTP relay that records every message by recipient.
type mailbox struct {
	mu  sync.Mutex
	got map[string][]string
}

func (m *mailbox) for_(addr string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.got[strings.ToLower(addr)]...)
}

func (m *mailbox) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.got {
		n += len(v)
	}
	return n
}

func newMailbox(t *testing.T) (*notify.Email, *mailbox) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	mb := &mailbox{got: map[string][]string{}}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
				say("220 fake")
				var rcpt []string
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.TrimSpace(line)
					switch up := strings.ToUpper(cmd); {
					case strings.HasPrefix(up, "RCPT TO:"):
						rcpt = append(rcpt, strings.ToLower(strings.Trim(cmd[len("RCPT TO:"):], "<> ")))
						say("250 ok")
					case strings.HasPrefix(up, "DATA"):
						say("354 go on")
						var b strings.Builder
						for {
							l, err := r.ReadString('\n')
							if err != nil || l == ".\r\n" {
								break
							}
							b.WriteString(l)
						}
						mb.mu.Lock()
						for _, to := range rcpt {
							mb.got[to] = append(mb.got[to], b.String())
						}
						mb.mu.Unlock()
						say("250 queued")
					case strings.HasPrefix(up, "QUIT"):
						say("221 bye")
						return
					default:
						say("250 ok")
					}
				}
			}(c)
		}
	}()
	host, portStr, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portStr)
	e, err := notify.NewEmail(notify.EmailConfig{Host: host, Port: port, From: "argus@northwind.id",
		To: []string{"soc@northwind.id"}})
	if err != nil {
		t.Fatal(err)
	}
	return e, mb
}

// withSignInAlerts gives a harness an email channel. drain waits for the
// queued alerts to go out; the harness cannot be used to send more after.
func withSignInAlerts(t *testing.T, h *loginHarness, p SignInAlertPolicy) (mb *mailbox, drain func()) {
	t.Helper()
	email, mb := newMailbox(t)
	n := notify.NewSignInNotifier(email, nil, h.srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.api.SignInAlerts = n
	h.api.SignInPolicy = p
	var once sync.Once
	drain = func() { once.Do(func() { n.Close(context.Background()) }) }
	t.Cleanup(drain)
	return mb, drain
}

func signIn(t *testing.T, h *loginHarness, email, password, ua string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/auth/password",
		strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("sign-in = %d", res.StatusCode)
	}
}

const (
	chromeMac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	firefoxLx = "Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0"
)

// A new device tells the organization's administrators -- at their own
// address and the configured one -- and the person, who gets a report link.
func TestASignInFromANewDeviceTellsAdminsAndThePerson(t *testing.T) {
	h := newLoginHarness(t)
	admin, _ := newAccount(t, h.store, "admin")
	email, password := newAccount(t, h.store, "operator")
	mb, drain := withSignInAlerts(t, h, DefaultSignInAlertPolicy)

	signIn(t, h, email, password, chromeMac)
	drain()

	if got := mb.for_(admin); len(got) != 1 || !strings.Contains(got[0], "from a new device") {
		t.Fatalf("administrator got %d alerts: %v", len(got), got)
	}
	if got := mb.for_("soc@northwind.id"); len(got) != 1 {
		t.Fatalf("the configured address got %d alerts", len(got))
	}
	got := mb.for_(email)
	if len(got) != 1 || !strings.Contains(got[0], "New sign-in to your Argus account") ||
		!strings.Contains(got[0], "/auth/sign-in-report?ticket=") || !strings.Contains(got[0], "Chrome 131 on macOS") {
		t.Fatalf("the person got %d emails: %v", len(got), got)
	}
}

// The same browser at the same address is not news; by default nothing is sent.
func TestAKnownDeviceIsNotAnnouncedByDefault(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "operator")
	if _, err := h.store.RememberSignInDevice(t.Context(), email, signInFingerprint("127.0.0.1", "Firefox on Linux")); err != nil {
		t.Fatal(err)
	}
	mb, drain := withSignInAlerts(t, h, DefaultSignInAlertPolicy)

	signIn(t, h, email, password, firefoxLx)
	drain()
	if n := mb.total(); n != 0 {
		t.Fatalf("%d alerts for a device the account has used before", n)
	}
}

// Guessing at a password must never make Argus send mail: only a sign-in that
// succeeded is announced.
func TestRefusedSignInsSendNothing(t *testing.T) {
	h := newLoginHarness(t)
	email, _ := newAccount(t, h.store, "operator")
	mb, drain := withSignInAlerts(t, h, SignInAlertPolicy{Admins: true, Person: true, Every: true})

	for i := 0; i < 5; i++ {
		if c, _ := h.post(t, "/auth/password", loginRequest{Email: email, Password: "wrong"}); c != http.StatusUnauthorized {
			t.Fatalf("wrong password = %d", c)
		}
	}
	drain()
	if n := mb.total(); n != 0 {
		t.Fatalf("%d alerts for refused sign-ins", n)
	}
}

// "Every sign-in" still holds an account's routine alerts to one per quiet
// window, so signing in over and over cannot flood anyone's inbox.
func TestEverySignInIsBoundedPerAccount(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "operator")
	mb, drain := withSignInAlerts(t, h, SignInAlertPolicy{Person: true, Every: true})

	for i := 0; i < 4; i++ {
		signIn(t, h, email, password, firefoxLx)
	}
	drain()
	if got := mb.for_(email); len(got) != 1 {
		t.Fatalf("four sign-ins in a row sent %d emails; want 1", len(got))
	}
}

func TestTheLimiterHoldsRoutineAlertsButLetsNewDevicesThrough(t *testing.T) {
	var l signInAlertLimiter
	now := time.Now()
	if !l.allow("a", false, now) {
		t.Fatal("first alert held")
	}
	if l.allow("a", false, now.Add(time.Minute)) {
		t.Fatal("a routine alert inside the quiet window went out")
	}
	if !l.allow("a", true, now.Add(2*time.Minute)) {
		t.Fatal("a new device inside the quiet window was held")
	}
	for i := 3; i <= signInAlertsPerHour; i++ {
		l.allow("a", true, now.Add(time.Duration(i)*time.Minute))
	}
	if l.allow("a", true, now.Add(30*time.Minute)) {
		t.Fatal("more than the hourly bound went out")
	}
	if !l.allow("a", false, now.Add(2*time.Hour)) {
		t.Fatal("the bound did not lift after an hour")
	}
	if !l.allow("b", false, now) {
		t.Fatal("one account's alerts held another's")
	}
}

// With no channel set up nothing is sent, and the sign-in is unaffected.
func TestNoChannelMeansNoAlertAndTheSignInStillWorks(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "operator")
	signIn(t, h, email, password, chromeMac)
	if h.me(t)["authenticated"] != true {
		t.Fatal("signed out by an absent alert channel")
	}
}

// Policy switches: administrators only, or the person only.
func TestPolicyDecidesWhoIsTold(t *testing.T) {
	h := newLoginHarness(t)
	admin, _ := newAccount(t, h.store, "admin")
	email, password := newAccount(t, h.store, "operator")
	mb, drain := withSignInAlerts(t, h, SignInAlertPolicy{Admins: false, Person: true})
	signIn(t, h, email, password, chromeMac)
	drain()
	if len(mb.for_(admin)) != 0 || len(mb.for_("soc@northwind.id")) != 0 {
		t.Fatal("administrators were told with the switch off")
	}
	if len(mb.for_(email)) != 1 {
		t.Fatal("the person was not told")
	}
}

/* ── "This wasn't me" ───────────────────────────────────────────────────── */

func reportLink(t *testing.T, h *loginHarness, email string) string {
	t.Helper()
	tok, err := h.api.signer.IssueSignInReport(auth.SignInReport{
		Account: email, At: time.Now().UTC(), ClientIP: "203.0.113.9", Device: "Chrome 131 on macOS",
		Method: notify.MethodAuthenticator,
	}, signInReportTTL)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func getPage(t *testing.T, h *loginHarness, tok string) (int, string) {
	t.Helper()
	res, err := http.Get(h.srv.URL + "/auth/sign-in-report?ticket=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func postReport(t *testing.T, h *loginHarness, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/auth/sign-in-report",
		strings.NewReader(url.Values{"ticket": {tok}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// Opening the link only shows the sign-in: mail scanners open every link, and
// one that acted on GET would report every scanned account.
func TestOpeningTheReportLinkDoesNothing(t *testing.T) {
	h := newLoginHarness(t)
	email, _ := newAccount(t, h.store, "operator")
	tok := reportLink(t, h, email)
	for i := 0; i < 2; i++ {
		code, body := getPage(t, h, tok)
		if code != http.StatusOK || !strings.Contains(body, "This wasn't me, report it") ||
			!strings.Contains(body, "203.0.113.9") {
			t.Fatalf("page = %d:\n%s", code, body)
		}
	}
	if c, body := postReport(t, h, tok); c != http.StatusOK || !strings.Contains(body, "Reported") {
		t.Fatalf("the report after two views was refused: %d\n%s", c, body)
	}
}

// Reporting is audited, tells administrators even with sign-in alerts
// switched off, and works once.
func TestAReportIsAuditedAlertedAndSingleUse(t *testing.T) {
	h := newLoginHarness(t)
	admin, _ := newAccount(t, h.store, "admin")
	email, _ := newAccount(t, h.store, "operator")
	mb, drain := withSignInAlerts(t, h, SignInAlertPolicy{})
	tok := reportLink(t, h, email)

	if c, body := postReport(t, h, tok); c != http.StatusOK || !strings.Contains(body, "told your administrators") {
		t.Fatalf("report = %d:\n%s", c, body)
	}
	if c, body := postReport(t, h, tok); c != http.StatusOK || !strings.Contains(body, "Already reported") {
		t.Fatalf("a second report = %d:\n%s", c, body)
	}
	drain()

	got := mb.for_(admin)
	if len(got) != 1 || !strings.Contains(got[0], "reported a sign-in that wasn't them") {
		t.Fatalf("administrator got %v", got)
	}
	if len(mb.for_(email)) != 0 {
		t.Fatal("the reporter was alerted about their own report")
	}

	events, err := h.store.AuditEvents(t.Context(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, e := range events {
		if e.Action == "auth.sign_in_reported" && strings.EqualFold(e.ActorEmail, email) {
			found++
			if e.Severity != "warning" || !strings.Contains(e.Detail, "203.0.113.9") {
				t.Fatalf("audit row: %+v", e)
			}
		}
	}
	if found != 1 {
		t.Fatalf("%d audit rows for one report", found)
	}
}

func TestATamperedOrExpiredReportLinkIsRefused(t *testing.T) {
	h := newLoginHarness(t)
	email, _ := newAccount(t, h.store, "operator")
	tok := reportLink(t, h, email)
	if c, _ := postReport(t, h, tok[:len(tok)-2]+"xx"); c != http.StatusBadRequest {
		t.Fatalf("tampered = %d", c)
	}
	old, _ := h.api.signer.IssueSignInReport(auth.SignInReport{Account: email}, -time.Minute)
	if c, body := getPage(t, h, old); c != http.StatusGone || !strings.Contains(body, "expired") {
		t.Fatalf("expired = %d", c)
	}
}

// The link is mailed, so it is public. It must not work as a session, a
// ticket or a second-factor challenge.
func TestAReportLinkAuthenticatesNothing(t *testing.T) {
	h := newLoginHarness(t)
	email, _ := newAccount(t, h.store, "admin")
	tok := reportLink(t, h, email)
	if _, err := h.api.signer.VerifySession(tok); err == nil {
		t.Fatal("a report link verified as a session")
	}
	if _, err := h.api.signer.RedeemMFAChallenge(tok); err == nil {
		t.Fatal("a report link verified as a challenge")
	}
	if _, err := h.api.signer.RedeemTicket(t.Context(), tok, "", ""); err == nil {
		t.Fatal("a report link redeemed as a ticket")
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: tok})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if strings.Contains(string(b), `"authenticated":true`) {
		t.Fatal("a report link signed a browser in")
	}
}

func TestDevicesAreRememberedPerAccount(t *testing.T) {
	s := testStore(t)
	a, _ := newAccount(t, s, "operator")
	b, _ := newAccount(t, s, "operator")
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sign_in_devices WHERE email = ANY($1)`,
			[]string{strings.ToLower(a), strings.ToLower(b)})
	})
	fp := signInFingerprint("2001:db8::1", "Chrome on macOS")
	for i, c := range []struct {
		email string
		fp    string
		isNew bool
	}{
		{a, fp, true},
		{a, fp, false},
		{strings.ToUpper(a), fp, false},
		{a, signInFingerprint("2001:db8::ffff", "Chrome on macOS"), false}, // same /64
		{a, signInFingerprint("2001:db9::1", "Chrome on macOS"), true},
		{b, fp, true},
	} {
		got, err := s.RememberSignInDevice(t.Context(), c.email, c.fp)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.isNew {
			t.Fatalf("step %d: new = %v, want %v", i, got, c.isNew)
		}
	}
}

// Reporting a sign-in ends every session on the account, the reporter's own
// and the one reported, so whoever signed in is out at once.
func TestAReportSignsTheAccountOutEverywhere(t *testing.T) {
	h := newLoginHarness(t)
	email, password := newAccount(t, h.store, "operator")
	signIn(t, h, email, password, chromeMac)
	if h.me(t)["authenticated"] != true {
		t.Fatal("not signed in to begin with")
	}
	if c, body := postReport(t, h, reportLink(t, h, email)); c != http.StatusOK ||
		!strings.Contains(body, "Every session on your account is signed out") {
		t.Fatalf("report = %d:\n%s", c, body)
	}
	if h.me(t)["authenticated"] == true {
		t.Fatal("the reported account's session survived the report")
	}
}

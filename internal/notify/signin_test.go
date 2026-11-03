package notify

import (
	"bufio"
	"context"
	"github.com/gsoultan/gsmail"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var dewi = SignIn{
	Email: "dewi@acme.co", Name: "Dewi Pratama", Role: "operator", Org: "Acme Production",
	ClientIP: "182.253.14.7", Device: "Chrome 131 on macOS", Method: MethodAuthenticator,
	NewDevice: true, At: time.Date(2026, 11, 2, 6, 42, 0, 0, time.UTC),
}

// The facts an administrator needs to decide whether a sign-in was expected:
// who, when, from where, on what, and with which second step.
func TestAdminAlertSaysWhoWhenWhereAndHow(t *testing.T) {
	m := SignInAdminMessage(dewi, "https://argus.acme.co")
	if m.Subject != "Argus: Dewi Pratama signed in from a new device" {
		t.Fatalf("subject = %q", m.Subject)
	}
	for _, want := range []string{"dewi@acme.co", "Mon 2 Nov 2026, 06:42 UTC", "182.253.14.7",
		"Chrome 131 on macOS", "Authenticator app", "Acme Production", "https://argus.acme.co/audit"} {
		for name, body := range map[string]string{"text": m.Text, "html": m.HTML, "telegram": m.Telegram + m.Link} {
			if !strings.Contains(body, want) {
				t.Errorf("%s part is missing %q", name, want)
			}
		}
	}
}

func TestARecoveryCodeSignInSaysSo(t *testing.T) {
	e := dewi
	e.Method, e.NewDevice = MethodRecoveryCode, false
	m := SignInAdminMessage(e, "")
	if !strings.Contains(m.Subject, "with a recovery code") || !strings.Contains(m.Telegram, "Recovery code used") {
		t.Fatalf("subject %q / telegram %q", m.Subject, m.Telegram)
	}
}

// A display name is chosen by whoever made the account; it must not become
// markup in an administrator's mail client or in Telegram.
func TestNamesAreEscapedInHTMLAndTelegram(t *testing.T) {
	e := dewi
	e.Name = `<img src=x onerror=alert(1)> & co`
	e.Device = `<b>evil</b>`
	m := SignInAdminMessage(e, "https://argus.acme.co")
	for name, body := range map[string]string{"html": m.HTML, "telegram": m.Telegram} {
		if strings.Contains(body, "<img") || strings.Contains(body, "<b>evil") {
			t.Fatalf("%s carries the name as markup:\n%s", name, body)
		}
	}
	p := SignInPersonMessage(e, "", "https://argus.acme.co/auth/sign-in-report?ticket=a\"b")
	if strings.Contains(p.HTML, `ticket=a"b`) {
		t.Fatal("the report link was not escaped in its attribute")
	}
}

func TestPersonEmailCarriesTheReportLinkOnlyWhenThereIsOne(t *testing.T) {
	with := SignInPersonMessage(dewi, "https://argus.acme.co", "https://argus.acme.co/auth/sign-in-report?ticket=T")
	if !strings.Contains(with.Text, "This wasn't me: https://argus.acme.co/auth/sign-in-report?ticket=T") ||
		!strings.Contains(with.HTML, "This wasn&#39;t me") {
		t.Fatalf("no report link:\n%s", with.Text)
	}
	if !strings.HasPrefix(with.Text, "Hi Dewi, your account dewi@acme.co") {
		t.Fatalf("greeting: %q", with.Text[:60])
	}
	without := SignInPersonMessage(dewi, "", "")
	if strings.Contains(without.Text, "This wasn't me:") || strings.Contains(without.HTML, "<a ") {
		t.Fatal("a button with nowhere to go")
	}
}

func TestReportedAlertSaysWhetherSessionsWereEnded(t *testing.T) {
	r := SignInReport{SignIn: dewi, ReportedAt: dewi.At.Add(9 * time.Minute)}
	if m := SignInReportedMessage(r, ""); !strings.Contains(m.Text, "still signed in") {
		t.Fatalf("claims sessions were ended:\n%s", m.Text)
	}
	r.SignedOut = true
	if m := SignInReportedMessage(r, ""); !strings.Contains(m.Text, "already signed out every session") {
		t.Fatalf("does not say sessions were ended:\n%s", m.Text)
	}
}

func TestDescribeUserAgent(t *testing.T) {
	cases := []struct{ ua, label, family string }{
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			"Chrome 131 on macOS", "Chrome on macOS"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.2903.70",
			"Edge 131 on Windows", "Edge on Windows"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0", "Firefox 132 on Linux", "Firefox on Linux"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1",
			"Safari 18 on iOS", "Safari on iOS"},
		{"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36",
			"Chrome 130 on Android", "Chrome on Android"},
		{"curl/8.5.0", "curl 8", "curl"},
		{"", "An unknown browser", "An unknown browser"},
	}
	for _, c := range cases {
		label, family := DescribeUserAgent(c.ua)
		if label != c.label || family != c.family {
			t.Errorf("%q\n got %q / %q\nwant %q / %q", c.ua, label, family, c.label, c.family)
		}
	}
}

func TestSignInEmailIsPlainTextAndHTML(t *testing.T) {
	e, _ := NewEmail(EmailConfig{Host: "smtp", From: "argus@x.io", To: []string{"sec@x.io"}})
	raw, err := gsmail.RenderMessage(e.message([]string{"dewi@acme.co"}, "subject", "text body", "<p>html body</p>"))
	if err != nil {
		t.Fatal(err)
	}
	head, parts := mimeParts(t, raw)
	if !strings.HasPrefix(head.Get("Content-Type"), "multipart/alternative") ||
		parts["text/plain"] != "text body" || parts["text/html"] != "<p>html body</p>" {
		t.Fatalf("message:\n%s", raw)
	}
	if head.Get("To") != "dewi@acme.co" || strings.Contains(string(raw), "sec@x.io") {
		t.Fatal("addressed to the configured list instead of the account")
	}
}

func TestSendToRefusesAnAddressThatIsNotOne(t *testing.T) {
	e, _ := NewEmail(EmailConfig{Host: "127.0.0.1", Port: 1, From: "argus@x.io", To: []string{"sec@x.io"}})
	err := e.SendTo(context.Background(), []string{"dewi@acme.co\r\nBcc: x@evil.io"}, "s", "t", "")
	if err == nil || !strings.Contains(err.Error(), "not a single email address") {
		t.Fatalf("err = %v", err)
	}
}

/* ── Routing ────────────────────────────────────────────────────────────── */

type smtpLog struct {
	mu   sync.Mutex
	rcpt []string
}

// multiSMTP accepts any number of messages and records who each was for.
func multiSMTP(t *testing.T) (*Email, *smtpLog) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := &smtpLog{}
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
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.TrimSpace(line)
					switch up := strings.ToUpper(cmd); {
					case strings.HasPrefix(up, "RCPT TO:"):
						got.mu.Lock()
						got.rcpt = append(got.rcpt, strings.Trim(cmd[len("RCPT TO:"):], "<> "))
						got.mu.Unlock()
						say("250 ok")
					case strings.HasPrefix(up, "DATA"):
						say("354 go on")
						for {
							l, err := r.ReadString('\n')
							if err != nil || l == ".\r\n" {
								break
							}
						}
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
	e, err := NewEmail(EmailConfig{Host: host, Port: port, From: "argus@x.io", To: []string{"sec@acme.co"}})
	if err != nil {
		t.Fatal(err)
	}
	return e, got
}

func fakeTelegram(t *testing.T) (*Telegram, *[]string, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		texts = append(texts, r.Form.Get("text"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	tg, err := NewTelegram(TelegramConfig{BotToken: "T", ChatIDs: []string{"-100"}}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	tg.api = srv.URL
	return tg, &texts, &mu
}

// Administrators of the default organization: their own addresses, the
// configured list, and Telegram. Each address in a message of its own.
func TestAdminsOfTheDefaultOrganizationHearOnEveryChannel(t *testing.T) {
	e, got := multiSMTP(t)
	tg, texts, mu := fakeTelegram(t)
	n := NewSignInNotifier(e, tg, "https://argus.acme.co", nil)
	n.Admins(dewi, []string{"rizal@acme.co", "SEC@acme.co"}, true)
	n.Close(context.Background())

	if strings.Join(got.rcpt, ",") != "rizal@acme.co,SEC@acme.co" {
		t.Fatalf("mailed %v; want each admin once, the configured address folded in", got.rcpt)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*texts) != 1 || !strings.Contains((*texts)[0], "Dewi Pratama signed in") {
		t.Fatalf("telegram = %v", *texts)
	}
}

// Another organization's sign-ins reach its own administrators only: the
// Telegram chats and the configured addresses are the platform's.
func TestAnotherOrganizationNeverReachesThePlatformChannels(t *testing.T) {
	e, got := multiSMTP(t)
	tg, texts, mu := fakeTelegram(t)
	n := NewSignInNotifier(e, tg, "", nil)
	n.Admins(dewi, []string{"admin@other.co"}, false)
	n.Reported(SignInReport{SignIn: dewi}, []string{"admin@other.co"}, false)
	n.Close(context.Background())

	for _, r := range got.rcpt {
		if r != "admin@other.co" {
			t.Fatalf("mailed %v", got.rcpt)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*texts) != 0 {
		t.Fatalf("telegram got %v", *texts)
	}
}

// The person is told by email only, at the account's address.
func TestThePersonIsToldByEmailOnly(t *testing.T) {
	e, got := multiSMTP(t)
	tg, texts, mu := fakeTelegram(t)
	n := NewSignInNotifier(e, tg, "", nil)
	n.Person(dewi, "")
	n.Close(context.Background())
	if strings.Join(got.rcpt, ",") != "dewi@acme.co" {
		t.Fatalf("mailed %v", got.rcpt)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*texts) != 0 {
		t.Fatalf("telegram got %v", *texts)
	}
}

// Telegram only: the person cannot be told, and nothing fails trying.
func TestWithoutEmailThePersonIsNotTold(t *testing.T) {
	tg, texts, mu := fakeTelegram(t)
	n := NewSignInNotifier(nil, tg, "", nil)
	if n.EmailOn() || !n.TelegramOn() {
		t.Fatal("channels misreported")
	}
	n.Person(dewi, "")
	n.Admins(dewi, []string{"rizal@acme.co"}, true)
	n.Close(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(*texts) != 1 {
		t.Fatalf("telegram got %d messages", len(*texts))
	}
}

func TestNoChannelMeansNoNotifier(t *testing.T) {
	var n *SignInNotifier = NewSignInNotifier(nil, nil, "", nil)
	if n != nil {
		t.Fatal("a notifier with nothing to send through")
	}
	n.Admins(dewi, []string{"x@y.z"}, true)
	n.Person(dewi, "")
	n.Close(context.Background())
}

// Telegram refuses a button pointing at an address it does not consider
// public. The alert must still arrive.
func TestARefusedButtonFallsBackToALink(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		calls = append(calls, r.Form.Get("reply_markup"))
		mu.Unlock()
		if r.Form.Get("reply_markup") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"description":"Bad Request: inline keyboard button URL 'http://argus.lan/audit' is invalid: Wrong HTTP URL"}`)
			return
		}
		if !strings.Contains(r.Form.Get("text"), "http://argus.lan/audit") {
			t.Errorf("fallback lost the link: %q", r.Form.Get("text"))
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	tg, _ := NewTelegram(TelegramConfig{BotToken: "T", ChatIDs: []string{"1"}}, srv.Client())
	tg.api = srv.URL
	if err := tg.SendHTML(context.Background(), "<b>hi</b>", "Review", "http://argus.lan/audit"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
}

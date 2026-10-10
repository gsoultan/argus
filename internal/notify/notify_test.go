package notify

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestMessageSaysWhoWhereAndAsWhom(t *testing.T) {
	subject, body := Message(SessionStarted{
		ID: "s-1", UserEmail: "ana@example.com", Host: "db-01", Principal: "postgres",
		Protocol: "ssh", Origin: "brokered", ClientIP: "10.0.0.7",
		StartedAt: time.Date(2026, 10, 10, 3, 4, 5, 0, time.UTC),
	}, "https://argus.example.com/")

	if subject != "Argus: ana@example.com opened SSH on db-01 as postgres" {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{
		"Server:  db-01", "Account: postgres", "From:    10.0.0.7",
		"Started: 2026-10-10 03:04:05 UTC", "https://argus.example.com/sessions/s-1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
}

// A login that went around the gateway is the one an administrator most needs
// to hear about, so it must not read like an ordinary session.
func TestMessageCallsOutADirectLogin(t *testing.T) {
	subject, body := Message(SessionStarted{
		Host: "web-02", Principal: "root", Protocol: "ssh", Origin: "direct",
	}, "")
	if !strings.Contains(subject, "without going through Argus") {
		t.Fatalf("subject = %q", subject)
	}
	if !strings.Contains(body, "did not go through Argus") {
		t.Fatalf("body = %q", body)
	}
	if strings.Contains(body, "/sessions/") {
		t.Fatalf("no console URL configured, but the body links to one: %q", body)
	}
}

type recorder struct {
	mu   sync.Mutex
	got  []string
	fail error
	gate chan struct{}
}

func (r *recorder) Name() string { return "test" }
func (r *recorder) Send(_ context.Context, subject, _ string) error {
	if r.gate != nil {
		<-r.gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, subject)
	return r.fail
}

func TestDispatcherSendsToEveryChannelAndDrainsOnClose(t *testing.T) {
	a, b := &recorder{fail: errors.New("down")}, &recorder{}
	d := NewDispatcher([]Sender{a, b}, "", quiet)
	d.SessionStarted(SessionStarted{Host: "h1"})
	d.SessionStarted(SessionStarted{Host: "h2"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Close(ctx)

	// A failing channel must not stop the next one from hearing about it.
	if len(a.got) != 2 || len(b.got) != 2 {
		t.Fatalf("a got %d, b got %d; want 2 each", len(a.got), len(b.got))
	}
}

// The report handler calls this inline. If a dead mail server could make it
// block, it would hold up every gateway reporting a session.
func TestDispatcherNeverBlocksTheCaller(t *testing.T) {
	stuck := &recorder{gate: make(chan struct{})}
	d := NewDispatcher([]Sender{stuck}, "", quiet)

	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize*3; i++ {
			d.SessionStarted(SessionStarted{Host: "h"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SessionStarted blocked behind a stuck channel")
	}
	close(stuck.gate)
}

func TestNoChannelsMeansNilAndNilIsSafe(t *testing.T) {
	d := NewDispatcher(nil, "", quiet)
	if d != nil {
		t.Fatal("want nil with no senders")
	}
	d.SessionStarted(SessionStarted{})
	d.Close(context.Background())
}

func TestTelegramPostsToEveryChat(t *testing.T) {
	var mu sync.Mutex
	var chats []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN123/sendMessage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = r.ParseForm()
		mu.Lock()
		chats = append(chats, r.Form.Get("chat_id"))
		mu.Unlock()
		if r.Form.Get("text") != "hello" {
			t.Errorf("text = %q", r.Form.Get("text"))
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tg, err := NewTelegram(TelegramConfig{BotToken: "TOKEN123", ChatIDs: []string{"1", "-100"}}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	tg.api = srv.URL
	if err := tg.Send(context.Background(), "subject", "hello"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(chats, ",") != "1,-100" {
		t.Fatalf("chats = %v", chats)
	}
}

func TestTelegramReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Bad Request: chat not found"}`)
	}))
	defer srv.Close()
	tg, _ := NewTelegram(TelegramConfig{BotToken: "T", ChatIDs: []string{"9"}}, srv.Client())
	tg.api = srv.URL
	err := tg.Send(context.Background(), "", "x")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("err = %v", err)
	}
}

// net/http puts the request URL in its errors, and the bot token is in the URL.
// Every failed delivery is logged, so an unredacted error is a leaked token.
func TestTelegramErrorsNeverCarryTheToken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listening: the request fails at connect

	const token = "123456:SECRET-bot-token"
	tg, _ := NewTelegram(TelegramConfig{BotToken: token, ChatIDs: []string{"1"}}, nil)
	tg.api = (&url.URL{Scheme: "http", Host: addr}).String()
	err = tg.Send(context.Background(), "", "x")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestEmailConfigIsCheckedAtStartUp(t *testing.T) {
	for name, cfg := range map[string]EmailConfig{
		"no host":          {From: "a@x", To: []string{"b@x"}},
		"no recipients":    {Host: "smtp", From: "a@x"},
		"half a login":     {Host: "smtp", From: "a@x", To: []string{"b@x"}, Username: "u"},
		"header injection": {Host: "smtp", From: "a@x", To: []string{"b@x\r\nBcc: c@x"}},
		"two in one":       {Host: "smtp", From: "a@x", To: []string{"b@x, c@x"}},
	} {
		if _, err := NewEmail(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The subject carries a hostname and an email address taken from a session
// report. A line break in either must not become a new header.
func TestEmailSubjectCannotAddHeaders(t *testing.T) {
	e, err := NewEmail(EmailConfig{Host: "smtp", From: "argus@x.io", To: []string{"sec@x.io"}})
	if err != nil {
		t.Fatal(err)
	}
	msg := string(e.compose("Argus: evil\r\nBcc: attacker@x.io", "body"))
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("injected header made it through:\n%s", head)
		}
	}
}

// fakeSMTP is just enough of a server to accept one message, with no STARTTLS.
func fakeSMTP(t *testing.T) (addr string, received <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 1)
	go func() {
		defer l.Close()
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
		say("220 fake")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "DATA"):
				say("354 go on")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				out <- data.String()
				say("250 queued")
			case strings.HasPrefix(cmd, "QUIT"):
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return l.Addr().String(), out
}

func TestEmailDeliversToALocalRelayWithoutTLS(t *testing.T) {
	addr, received := fakeSMTP(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	e, err := NewEmail(EmailConfig{Host: host, Port: port, From: "argus@x.io", To: []string{"sec@x.io"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Send(ctx, "Argus: ana opened SSH on db-01 as root", "line one\nline two\n"); err != nil {
		t.Fatal(err)
	}
	msg := <-received
	if !strings.Contains(msg, "Subject: Argus: ana opened SSH on db-01 as root") ||
		!strings.Contains(msg, "line one\r\nline two") {
		t.Fatalf("message:\n%s", msg)
	}
}

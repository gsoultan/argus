package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/hostkey"
	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/recorder"
)

// A shadower must not be shown what the user typed. The target echoes input
// back as output, so relaying input frames would double every keystroke on the
// viewer's screen — and would put text typed at a non-echoing prompt onto a
// second person's display, which live viewing has no reason to disclose.
func TestDecodeFrameDropsInput(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantOK   bool
		wantType string
		wantData string
	}{
		{"output", `[1.5,"o","hello\r\n"]`, true, "output", "hello\r\n"},
		{"resize", `[2.0,"r","120x40"]`, true, "resize", "120x40"},
		{"input is dropped", `[1.6,"i","sudo -i\r"]`, false, "", ""},
		{"marker is dropped", `[3.0,"m","checkpoint"]`, false, "", ""},
		{"malformed json", `not json at all`, false, "", ""},
		{"wrong arity", `[1.0,"o"]`, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := decodeFrame([]byte(tc.line))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if m.Type != tc.wantType || m.Data != tc.wantData {
				t.Errorf("got %s/%q, want %s/%q", m.Type, m.Data, tc.wantType, tc.wantData)
			}
		})
	}
}

/* ── End to end over a real WebSocket ────────────────────────────────────── */

// newTestServer builds a gateway with one session already in flight, recording
// to a real file with the shadow tap attached — the same wiring openRecording
// produces, so the test exercises the path the product uses.
func newTestServer(t *testing.T) (*Server, *Session, WebConfig) {
	t.Helper()

	// The Server struct directly rather than NewServer: this exercises the
	// shadow HTTP surface, and NewServer would additionally demand a host key
	// and an authorized_keys file for an SSH listener the test never starts.
	srv := &Server{
		cfg: Config{
			RecordingDir: t.TempDir(),
			HostKeys:     mustHostKeys(t),
			Inventory: &Inventory{assets: map[string]Asset{
				"pay-01.payments.northwind.id": {
					Hostname:   "pay-01.payments.northwind.id",
					Address:    "127.0.0.1",
					Principals: []string{"ops"},
				},
			}},
		},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: make(map[string]*Session),
	}

	sess := &Session{
		ID:        "sess-under-test",
		User:      "dewi.p@northwind.id",
		Principal: "ops",
		Target:    Asset{Hostname: "pay-01.payments.northwind.id"},
		RemoteIP:  "10.0.0.9:51234",
		StartedAt: time.Now().UTC(),
		srv:       srv,
		hub:       live.NewHub(),
		log:       srv.log,
	}
	if err := sess.openRecording(srv.cfg.RecordingDir); err != nil {
		t.Fatalf("openRecording: %v", err)
	}
	srv.trackSession(sess)
	t.Cleanup(func() { _, _ = sess.Close() })

	cfg := WebConfig{
		Signer:         newTestSigner(t),
		AllowedOrigins: []string{"*"},
	}
	return srv, sess, cfg
}

func mustHostKeys(t *testing.T) *hostkey.Store {
	t.Helper()
	// Trust on first use: this test is about shadowing, not pinning, and an
	// empty strict store would refuse before the session under test exists.
	st, err := hostkey.Open(filepath.Join(t.TempDir(), "known_hosts"), true)
	if err != nil {
		t.Fatalf("hostkey.Open: %v", err)
	}
	return st
}

func newTestSigner(t *testing.T) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner("a-development-secret-of-sufficient-length")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// A Signer runs a sweeper for its lifetime. Closing it is what the
	// production callers do, and without it every test that builds one leaks
	// a goroutine -- which is precisely what the package's leak check reports.
	t.Cleanup(s.Close)
	return s
}

func shadowHTTP(t *testing.T, srv *Server, cfg WebConfig) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/shadow", srv.handleShadow(cfg))
	mux.HandleFunc("POST /api/v1/sessions/{id}/terminate", srv.handleTerminate(cfg))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func readMsg(t *testing.T, ctx context.Context, c *websocket.Conn) serverMessage {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m serverMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	return m
}

// The whole point of the feature: output written to a live session reaches a
// viewer that was not there when it started, and the keystrokes that produced
// it do not.
func TestShadowStreamsOutputAndWithholdsInput(t *testing.T) {
	srv, sess, cfg := newTestServer(t)
	ts := shadowHTTP(t, srv, cfg)

	// Written before anyone attaches, so this can only arrive via the backlog.
	if err := sess.rec.Write(recorder.Output, []byte("before-attach\r\n")); err != nil {
		t.Fatal(err)
	}

	ticket, err := cfg.Signer.IssueSessionScopedTicket(
		"auditor@northwind.id", auth.ScopeShadow, sess.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/shadow?session=" + sess.ID + "&ticket=" + ticket
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if m := readMsg(t, ctx, c); m.Type != "ready" {
		t.Fatalf("first message = %s, want ready", m.Type)
	}
	if m := readMsg(t, ctx, c); m.Type != "output" || m.Data != "before-attach\r\n" {
		t.Fatalf("backlog = %s/%q, want output/before-attach", m.Type, m.Data)
	}

	// Now the live path, with an input frame between two output frames. The
	// input must be skipped rather than delivered.
	_ = sess.rec.Write(recorder.Input, []byte("sudo -i\r"))
	_ = sess.rec.Write(recorder.Output, []byte("after-attach\r\n"))

	if m := readMsg(t, ctx, c); m.Type != "output" || m.Data != "after-attach\r\n" {
		t.Fatalf("live frame = %s/%q; an input frame reached the viewer", m.Type, m.Data)
	}
}

func TestShadowRequiresAShadowScopedTicket(t *testing.T) {
	srv, sess, cfg := newTestServer(t)
	ts := shadowHTTP(t, srv, cfg)

	terminal, _ := cfg.Signer.IssueTicket(
		"dewi.p@northwind.id", "pay-01.payments.northwind.id", "ops", time.Minute)
	other, _ := cfg.Signer.IssueSessionScopedTicket(
		"auditor@northwind.id", auth.ScopeShadow, "a-different-session", time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for name, ticket := range map[string]string{
		"a terminal ticket":              terminal,
		"a shadow ticket for another id": other,
		"no ticket":                      "",
	} {
		t.Run(name, func(t *testing.T) {
			url := "ws" + strings.TrimPrefix(ts.URL, "http") +
				"/ws/shadow?session=" + sess.ID + "&ticket=" + ticket
			c, _, err := websocket.Dial(ctx, url, nil)
			if err == nil {
				c.CloseNow()
				t.Fatalf("%s was accepted for shadowing", name)
			}
		})
	}
}

// Terminating must end the session, say so inside the recording, and refuse to
// report success a second time.
func TestTerminateEndsTheSessionAndRecordsWhy(t *testing.T) {
	srv, sess, cfg := newTestServer(t)
	ts := shadowHTTP(t, srv, cfg)

	post := func(t *testing.T, ticket, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost,
			ts.URL+"/api/v1/sessions/"+sess.ID+"/terminate", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+ticket)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	mint := func(t *testing.T) string {
		t.Helper()
		tok, err := cfg.Signer.IssueSessionScopedTicket(
			"admin@northwind.id", auth.ScopeTerminate, sess.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	// A termination with no reason leaves an unexplained gap; refuse it.
	if res := post(t, mint(t), `{"reason":"  "}`); res.StatusCode != http.StatusBadRequest {
		t.Errorf("empty reason: status = %d, want 400", res.StatusCode)
	}

	res := post(t, mint(t), `{"reason":"credential use outside the approved window"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("terminate: status = %d, want 200", res.StatusCode)
	}
	by, reason, killed := sess.Killed()
	if !killed || by != "admin@northwind.id" {
		t.Fatalf("Killed() = %q/%q/%v", by, reason, killed)
	}

	// The notice belongs in the chain, so a replay explains the ending rather
	// than simply stopping.
	head, err := sess.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if head == "" {
		t.Error("no chain head after a terminated session")
	}
	body, err := os.ReadFile(filepath.Join(srv.cfg.RecordingDir, sess.ID+".cast"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "terminated by admin@northwind.id") {
		t.Error("the recording does not say who ended the session")
	}
	if !strings.Contains(string(body), "outside the approved window") {
		t.Error("the recording does not say why the session was ended")
	}

	// Second attempt: the session is already gone.
	if res := post(t, mint(t), `{"reason":"again"}`); res.StatusCode == http.StatusOK {
		t.Error("terminating a finished session reported success")
	}
}

// A viewer attached to a session that gets killed must be told what happened,
// not merely disconnected.
func TestShadowIsToldWhenTheSessionIsTerminated(t *testing.T) {
	srv, sess, cfg := newTestServer(t)
	ts := shadowHTTP(t, srv, cfg)

	ticket, _ := cfg.Signer.IssueSessionScopedTicket(
		"auditor@northwind.id", auth.ScopeShadow, sess.ID, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/ws/shadow?session=" + sess.ID + "&ticket=" + ticket
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if m := readMsg(t, ctx, c); m.Type != "ready" {
		t.Fatalf("first message = %s, want ready", m.Type)
	}

	sess.Terminate("admin@northwind.id", "suspected credential misuse")
	_, _ = sess.Close()

	// The kill notice, then the close.
	var sawNotice, sawClosed bool
	for range 5 {
		m := readMsg(t, ctx, c)
		if m.Type == "output" && strings.Contains(m.Data, "terminated by admin@northwind.id") {
			sawNotice = true
		}
		if m.Type == "closed" {
			sawClosed = true
			if !strings.Contains(m.Data, "terminated by admin@northwind.id") {
				t.Errorf("close reason = %q, does not name who ended it", m.Data)
			}
			break
		}
	}
	if !sawNotice {
		t.Error("the viewer never saw the termination notice")
	}
	if !sawClosed {
		t.Error("the viewer was disconnected without being told why")
	}
}

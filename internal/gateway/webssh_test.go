package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

/*
The browser terminal's handshake.

A browser cannot set headers on a WebSocket upgrade, so the credential travels
in the query string. That is acceptable only because a ticket is single-use,
bound to one target and principal, and expires in about a minute -- the three
properties that stop a URL in a proxy log or a browser history from being a
credential. None of them had a test on this endpoint.
*/

// webGateway is a real Server in front of a real SSH target, with the browser
// endpoint mounted. The session goes through Dial, so it is subject to the
// same principal check, host-key pin and recording as one opened with ssh(1).
func webGateway(t *testing.T) (*Server, *httptest.Server, WebConfig) {
	t.Helper()
	dir := t.TempDir()
	injectedPath, injectedPub := writeKeyPair(t, dir, "injected", "")
	targetAddr := targetSSHServer(t, ssh.FingerprintSHA256(injectedPub))
	host, portStr, _ := net.SplitHostPort(targetAddr)
	port, _ := strconv.Atoi(portStr)

	srv := &Server{
		cfg: Config{
			RecordingDir: dir,
			HostKeys:     mustHostKeys(t),
			Inventory: &Inventory{assets: map[string]Asset{
				"pay-01": {
					Hostname: "pay-01", Address: host, Port: port,
					Principals: []string{"ops"}, KeyPath: injectedPath,
				},
			}},
		},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: map[string]*Session{},
	}
	cfg := WebConfig{Signer: newTestSigner(t), AllowedOrigins: []string{"*"}}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/session", srv.handleWebSession(cfg))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts, cfg
}

func wsURL(ts *httptest.Server, query string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/session?" + query
}

// dialTerminal opens the socket and returns the HTTP status the upgrade got,
// so a refusal can be asserted on rather than surfacing as a generic dial error.
func dialTerminal(t *testing.T, ctx context.Context, url string) (*websocket.Conn, int) {
	t.Helper()
	c, res, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		if res != nil {
			return nil, res.StatusCode
		}
		t.Fatalf("dial: %v", err)
	}
	return c, res.StatusCode
}

func TestBrowserTerminalOpensWithAValidTicket(t *testing.T) {
	srv, ts, cfg := webGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ticket, err := cfg.Signer.IssueTicket("dewi.p@northwind.id", "pay-01", "ops", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, code := dialTerminal(t, ctx, wsURL(ts, "target=pay-01&principal=ops&ticket="+ticket))
	if c == nil {
		t.Fatalf("upgrade refused with %d", code)
	}
	defer c.CloseNow()

	// The gateway reports the session id once the target shell exists, which
	// is what proves the whole path: ticket -> Dial -> target -> channel.
	for {
		m := readMsg(t, ctx, c)
		if m.Type == "error" {
			t.Fatalf("gateway reported an error: %s", m.Data)
		}
		if m.Type == "ready" {
			if m.Session == "" {
				t.Error("ready message carries no session id")
			}
			if _, tracked := srv.Session(m.Session); !tracked {
				t.Error("a ready session must be visible to the control plane's session list")
			}
			// It is being recorded, in the same artefact format as any other.
			if _, err := os.Stat(filepath.Join(srv.cfg.RecordingDir, m.Session+".cast")); err != nil {
				t.Errorf("no recording was opened for the browser session: %v", err)
			}
			return
		}
	}
}

func TestBrowserTerminalRefusesWithoutATicket(t *testing.T) {
	_, ts, _ := webGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c, code := dialTerminal(t, ctx, wsURL(ts, "target=pay-01&principal=ops")); c != nil {
		c.CloseNow()
		t.Fatal("a connection with no ticket was accepted")
	} else if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// Bound to one target and principal: a ticket minted for ops on staging must
// not open root on production, and must not open ops on production either.
func TestBrowserTerminalTicketIsBoundToTargetAndPrincipal(t *testing.T) {
	_, ts, cfg := webGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticket, _ := cfg.Signer.IssueTicket("dewi.p@northwind.id", "pay-01", "ops", time.Minute)

	for _, q := range []string{
		"target=pay-01&principal=root&ticket=" + ticket,
		"target=db-99&principal=ops&ticket=" + ticket,
	} {
		if c, code := dialTerminal(t, ctx, wsURL(ts, q)); c != nil {
			c.CloseNow()
			t.Errorf("%s: accepted a ticket for a different target or principal", q)
		} else if code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", q, code)
		}
	}
}

// Single-use is what makes a credential in a query string tolerable.
func TestBrowserTerminalTicketCannotBeReplayed(t *testing.T) {
	_, ts, cfg := webGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ticket, _ := cfg.Signer.IssueTicket("dewi.p@northwind.id", "pay-01", "ops", time.Minute)
	q := "target=pay-01&principal=ops&ticket=" + ticket

	first, code := dialTerminal(t, ctx, wsURL(ts, q))
	if first == nil {
		t.Fatalf("first use refused with %d", code)
	}
	defer first.CloseNow()

	if second, code := dialTerminal(t, ctx, wsURL(ts, q)); second != nil {
		second.CloseNow()
		t.Fatal("the same ticket opened a second session")
	} else if code != http.StatusUnauthorized {
		t.Errorf("replay status = %d, want 401", code)
	}
}

func TestBrowserTerminalTicketExpires(t *testing.T) {
	_, ts, cfg := webGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticket, _ := cfg.Signer.IssueTicket("dewi.p@northwind.id", "pay-01", "ops", 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	if c, code := dialTerminal(t, ctx, wsURL(ts, "target=pay-01&principal=ops&ticket="+ticket)); c != nil {
		c.CloseNow()
		t.Fatal("an expired ticket was accepted")
	} else if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// A ticket scoped to watching a session must not open a new one. The scopes
// exist so that an auditor's shadow ticket is not also a terminal.
func TestBrowserTerminalRefusesAShadowScopedTicket(t *testing.T) {
	_, ts, cfg := webGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticket, _ := cfg.Signer.IssueSessionScopedTicket("audit@northwind.id", "shadow", "some-session", time.Minute)
	if c, code := dialTerminal(t, ctx, wsURL(ts, "target=pay-01&principal=ops&ticket="+ticket)); c != nil {
		c.CloseNow()
		t.Fatal("a shadow-scoped ticket opened a terminal")
	} else if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

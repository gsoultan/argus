package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The browser terminal through an agent's tunnel.

The refusals first: a ticket opens only the host and account it was issued
for, the agent refuses a grant the control plane did not sign or has already
spent, and a session that cannot be recorded never starts. Then that it works:
the shell runs as the account, the person it was for is who the recording and
the audit chain name, closing the terminal on purpose hangs the shell up, and\na dropped connection does not: the same person can come back to it.

The shell runs as whoever runs the test, so this needs no root.
*/

const terminalSecret = "terminal-test-signing-secret-0123456789abcdef"

type terminalHost struct {
	host     string
	dir      string
	sessions chan agent.SessionRecord
}

// withTerminal gives the hub a grant key and the API a ticket signer.
func (e *tunnelEnv) withTerminal(t *testing.T) *auth.Signer {
	t.Helper()
	signer, err := auth.NewSigner(terminalSecret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Close)
	e.api.SetAuth(signer, "", false)
	if e.api.Tunnels.GrantKey, err = tunnel.GrantKeyFromSecret(terminalSecret); err != nil {
		t.Fatal(err)
	}
	return signer
}

// runTerminalAgent runs an agent offering the terminal service, recording
// into a directory of its own.
func (e *tunnelEnv) runTerminalAgent(t *testing.T, cred, host, dir string) *terminalHost {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	col := agent.NewCollector(dir, log)
	th := &terminalHost{host: host, dir: dir, sessions: make(chan agent.SessionRecord, 4)}
	col.OnSession = func(r agent.SessionRecord) { th.sessions <- r }

	services := agent.DefaultTunnelServices()
	services[agent.TerminalService] = agent.Terminal(agent.TerminalConfig{
		Collector: col, Verifier: &tunnel.GrantVerifier{Host: host}, Hostname: host, Log: log,
		Spawn: agent.SpawnDirect,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agent.RunTunnel(ctx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")
	return th
}

func me(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

func (e *tunnelEnv) terminalURL(target, principal, ticket string) string {
	u, _ := url.Parse(e.srv.URL)
	u.Scheme = "wss"
	u.Path = "/api/v1/terminal"
	q := url.Values{"target": {target}, "principal": {principal}}
	if ticket != "" {
		q.Set("ticket", ticket)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (e *tunnelEnv) openTerminal(t *testing.T, target, principal, ticket string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return websocket.Dial(ctx, e.terminalURL(target, principal, ticket), &websocket.DialOptions{
		HTTPClient: e.srv.Client(),
		HTTPHeader: http.Header{"Origin": {e.srv.URL}},
	})
}

// terminalRun reads a session to its end.
type terminalRun struct {
	ready, closed terminalMessage
	errors        []string
	output        strings.Builder
}

func readTerminal(t *testing.T, c *websocket.Conn, until time.Duration, input ...string) *terminalRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), until)
	defer cancel()
	run := &terminalRun{}
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return run
		}
		if typ == websocket.MessageBinary {
			run.output.Write(data)
			continue
		}
		var m terminalMessage
		_ = json.Unmarshal(data, &m)
		switch m.Type {
		case "ready":
			run.ready = m
			for _, in := range input {
				b, _ := json.Marshal(terminalMessage{Type: "input", Data: in})
				if err := c.Write(ctx, websocket.MessageText, b); err != nil {
					t.Fatal(err)
				}
			}
		case "error":
			run.errors = append(run.errors, m.Data)
		case "closed":
			run.closed = m
			return run
		}
	}
}

/* ── Refusals ────────────────────────────────────────────────────────────── */

func TestATerminalTicketOpensOnlyItsOwnHostAndAccount(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-scope")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runTerminalAgent(t, cred, host, t.TempDir())

	if _, resp, err := e.openTerminal(t, host, me(t), ""); err == nil ||
		resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no ticket: %v", err)
	}
	ticket, _ := signer.IssueTicket("ana@example.com", host, "nobody-else", time.Minute)
	if _, resp, err := e.openTerminal(t, host, me(t), ticket); err == nil ||
		resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a ticket for another account opened %s: %v", me(t), err)
	}
	ticket, _ = signer.IssueTicket("ana@example.com", "some-other-host", me(t), time.Minute)
	if _, resp, err := e.openTerminal(t, host, me(t), ticket); err == nil ||
		resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a ticket for another host opened %s: %v", host, err)
	}
}

// Whoever can put a stream on the tunnel -- not only the control plane --
// still needs the control plane's signature, for this host, once.
func TestAnAgentRefusesAGrantTheControlPlaneDidNotSignOrAlreadySpent(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	host := unique("term-grant")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	open := func(grant string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st, _, err := e.api.Tunnels.OpenWith(ctx, host,
			tunnel.Header{Service: agent.TerminalService, Grant: grant})
		if st != nil {
			_ = st.Close()
		}
		return err
	}
	_, forger, _ := ed25519.GenerateKey(rand.Reader)
	forged, _ := tunnel.SignGrant(forger, tunnel.NewGrant("mallory@x", host, me(t), ""))
	if err := open(forged); !tunnel.IsRefused(err) {
		t.Errorf("a grant signed by another key: %v", err)
	}
	other, _ := tunnel.SignGrant(e.api.Tunnels.GrantKey, tunnel.NewGrant("ana@x", "db-01", me(t), ""))
	if err := open(other); !tunnel.IsRefused(err) {
		t.Errorf("a grant for another host: %v", err)
	}
	if err := open(""); !tunnel.IsRefused(err) {
		t.Errorf("no grant at all: %v", err)
	}
	g, _ := tunnel.SignGrant(e.api.Tunnels.GrantKey, tunnel.NewGrant("ana@x", host, me(t), ""))
	if err := open(g); err != nil {
		t.Fatalf("the genuine grant was refused: %v", err)
	}
	<-th.sessions // the stream closed at once; the session ends
	if err := open(g); !tunnel.IsRefused(err) {
		t.Errorf("a spent grant opened a second session: %v", err)
	}
	unknown, _ := tunnel.SignGrant(e.api.Tunnels.GrantKey, tunnel.NewGrant("ana@x", host, "no-such-account-x", ""))
	if err := open(unknown); !tunnel.IsRefused(err) {
		t.Errorf("an account the host does not have: %v", err)
	}
}

func TestASessionThatCannotBeRecordedDoesNotOpen(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-norec")
	cred, _ := enrolled(t, e.store, e.h, host)
	// The recordings directory is a file: nothing can be written there.
	blocked := filepath.Join(t.TempDir(), "recordings")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	e.runTerminalAgent(t, cred, host, blocked)

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	run := readTerminal(t, c, 10*time.Second, "touch "+marker+"\n")
	if run.ready.Type != "" {
		t.Fatal("an unrecordable session reported ready")
	}
	if len(run.errors) == 0 || !strings.Contains(run.errors[0], "could not be recorded") {
		t.Fatalf("refused with %q", run.errors)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command ran in a session that was refused for not being recordable")
	}
}

/* ── It works, and says who it was for ──────────────────────────────────── */

func TestABrowserTerminalOpensThroughTheTunnelAndIsRecorded(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-ok")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	run := readTerminal(t, c, 15*time.Second, "echo argus-$((6*7))\n", "exit 3\n")

	if run.ready.Session == "" {
		t.Fatalf("no ready; errors %q", run.errors)
	}
	if !strings.Contains(run.output.String(), "argus-42") {
		t.Errorf("the shell's output did not arrive: %q", run.output.String())
	}
	if run.closed.ExitCode != 3 || run.closed.ChainHead == "" || run.closed.Session != run.ready.Session {
		t.Errorf("closed as %+v", run.closed)
	}

	var rec agent.SessionRecord
	select {
	case rec = <-th.sessions:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent sealed no recording")
	}
	if rec.ID != run.ready.Session || rec.Actor != "ana@example.com" ||
		rec.Principal != me(t) || rec.Origin != agent.Brokered || rec.ChainHead != run.closed.ChainHead {
		t.Errorf("recorded as %+v", rec)
	}
	cast, err := os.ReadFile(rec.Path)
	if err != nil || !strings.Contains(string(cast), "argus-42") {
		t.Errorf("the recording does not hold what was shown: %v", err)
	}

	// The audit chain says who, where, as whom, and which recording.
	events, err := e.store.AuditEvents(context.Background(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == "terminal.opened" && ev.Target == host {
			found = ev.ActorEmail == "ana@example.com" &&
				strings.Contains(ev.Detail, me(t)) && strings.Contains(ev.Detail, rec.ID)
			break
		}
	}
	if !found {
		t.Error("no terminal.opened entry naming the person, the account and the recording")
	}
}

// untilReady reads c to its ready message, then types input.
func untilReady(t *testing.T, c *websocket.Conn, input ...string) terminalMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no ready: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m terminalMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "ready":
			for _, in := range input {
				typeInto(t, c, in)
			}
			return m
		case "error", "gone":
			t.Fatalf("%s before ready: %s", m.Type, m.Data)
		}
	}
}

func typeInto(t *testing.T, c *websocket.Conn, in string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _ := json.Marshal(terminalMessage{Type: "input", Data: in})
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// readOutput reads c until its output holds want, and returns all it read.
func readOutput(t *testing.T, c *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out strings.Builder
	for !strings.Contains(out.String(), want) {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v; had %q", want, err, out.String())
		}
		if typ == websocket.MessageBinary {
			out.Write(data)
		}
	}
	return out.String()
}

func (e *tunnelEnv) resumeTerminal(t *testing.T, signer *auth.Signer, email, host, principal, session string) *websocket.Conn {
	t.Helper()
	ticket, _ := signer.IssueTicket(email, host, principal, time.Minute)
	u, _ := url.Parse(e.terminalURL(host, principal, ticket))
	q := u.Query()
	q.Set("resume", session)
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: e.srv.Client(),
		HTTPHeader: http.Header{"Origin": {e.srv.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func sealed(t *testing.T, th *terminalHost, within time.Duration) bool {
	t.Helper()
	select {
	case rec := <-th.sessions:
		if rec.ChainHead == "" {
			t.Error("the recording was not sealed")
		}
		return true
	case <-time.After(within):
		return false
	}
}

func TestClosingTheTerminalOnPurposeHangsTheShellUp(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-hangup")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	// Ready, then a shell that would otherwise run for five minutes.
	untilReady(t, c, "sleep 300\n")
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _ := json.Marshal(terminalMessage{Type: "hangup"})
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	if !sealed(t, th, 10*time.Second) {
		t.Fatal("the shell outlived a terminal closed on purpose")
	}
}

func TestADroppedBrowserIsHungUpOnlyAfterTheReattachWindow(t *testing.T) {
	e := newTunnelEnv(t)
	e.api.TerminalReattachWindow = 2 * time.Second
	signer := e.withTerminal(t)
	host := unique("term-window")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	untilReady(t, c, "sleep 300\n")
	time.Sleep(300 * time.Millisecond)
	_ = c.CloseNow() // the network went, not the person

	if sealed(t, th, time.Second) {
		t.Fatal("a dropped connection hung the shell up at once")
	}
	if !sealed(t, th, 10*time.Second) {
		t.Fatal("nobody came back, and the shell was never hung up")
	}
}

func TestADroppedBrowserComesBackToTheSameShell(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-resume")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	ready := untilReady(t, c, "X=kept-$((6*7)); echo before-$X\n")
	readOutput(t, c, "before-kept-42")
	_ = c.CloseNow()
	// Output while nobody is looking still reaches the backlog.
	time.Sleep(200 * time.Millisecond)

	// Someone else, with a ticket of their own for the same host and
	// account, cannot take it by knowing its id.
	other := e.resumeTerminal(t, signer, "eve@example.com", host, me(t), ready.Session)
	if m := firstMessage(t, other); m.Type != "gone" {
		t.Fatalf("another person's reattach got %+v", m)
	}

	back := e.resumeTerminal(t, signer, "ana@example.com", host, me(t), ready.Session)
	again := untilReady(t, back)
	if again.Session != ready.Session || !again.Resumed {
		t.Fatalf("reattached as %+v, want session %s resumed", again, ready.Session)
	}
	// The screen it missed, then the same shell, with its variable.
	readOutput(t, back, "before-kept-42")
	typeInto(t, back, "echo after-$X\n")
	readOutput(t, back, "after-kept-42")
	typeInto(t, back, "exit 4\n")
	run := readTerminal(t, back, 10*time.Second)
	if run.closed.ExitCode != 4 || run.closed.Session != ready.Session {
		t.Errorf("closed as %+v", run.closed)
	}
	if !sealed(t, th, 5*time.Second) {
		t.Fatal("the agent sealed no recording")
	}

	events, err := e.store.AuditEvents(context.Background(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == "terminal.reattached" && ev.Target == host &&
			ev.ActorEmail == "ana@example.com" && strings.Contains(ev.Detail, ready.Session) {
			found = true
		}
	}
	if !found {
		t.Error("no terminal.reattached entry naming the person and the session")
	}
}

func TestASecondWindowTakesTheSessionOver(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-moved")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	first, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer first.CloseNow()
	ready := untilReady(t, first)

	second := e.resumeTerminal(t, signer, "ana@example.com", host, me(t), ready.Session)
	untilReady(t, second)

	// The first is told why, so it does not try to come back.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	moved := false
	for !moved {
		typ, data, err := first.Read(ctx)
		if err != nil {
			t.Fatalf("the first window was not told it was replaced: %v", err)
		}
		var m terminalMessage
		if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.Type == "moved" {
			moved = true
		}
	}
	typeInto(t, second, "exit 0\n")
	if run := readTerminal(t, second, 10*time.Second); run.closed.Session != ready.Session {
		t.Errorf("the second window did not hold the session: %+v", run)
	}
}

func TestReattachingAnEndedSessionSaysSo(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-gone")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runTerminalAgent(t, cred, host, t.TempDir())

	c := e.resumeTerminal(t, signer, "ana@example.com", host, me(t), "no-such-session")
	if m := firstMessage(t, c); m.Type != "gone" {
		t.Fatalf("got %+v", m)
	}
}

func firstMessage(t *testing.T, c *websocket.Conn) terminalMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no message: %v", err)
		}
		var m terminalMessage
		if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil {
			return m
		}
	}
}

// The console decides how to open a session from this flag, so it must
// follow the tunnel, by the same freshness rule the agent list uses.
func TestAnAssetSaysWhetherItsAgentIsConnected(t *testing.T) {
	if tunnelFreshFor != 90*time.Second {
		t.Fatalf("tunnelFreshFor is %s; assetSelect hard-codes 90 seconds", tunnelFreshFor)
	}
	e := newTunnelEnv(t)
	asset, cred := enrolledAsset(t, e.store, e.h)
	read := func() bool {
		got, err := e.store.AssetByID(context.Background(), asset.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.TunnelConnected
	}
	if read() {
		t.Fatal("an asset whose agent never connected is listed as reachable")
	}
	e.runAgent(t, cred, asset.Hostname)
	e.waitTunnel(t, asset.Hostname, "")
	if !read() {
		t.Fatal("an asset whose agent is connected is not listed as reachable")
	}
	// A replica that stopped pinging is not believed.
	if _, err := e.store.pool.Exec(context.Background(), `
		UPDATE agent_credentials SET tunnel_seen_at = now() - interval '91 seconds'
		 WHERE lower(hostname) = lower($1)`, asset.Hostname); err != nil {
		t.Fatal(err)
	}
	if read() {
		t.Fatal("a stale tunnel is listed as reachable")
	}
}

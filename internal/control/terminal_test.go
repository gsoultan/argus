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
	"sync"
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
the audit chain name, and closing the browser hangs the shell up.

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

func TestClosingTheBrowserHangsTheShellUp(t *testing.T) {
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
	// Ready, then a shell that would otherwise run for five minutes.
	var once sync.Once
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m terminalMessage
		if json.Unmarshal(data, &m) == nil && m.Type == "ready" {
			once.Do(func() {
				b, _ := json.Marshal(terminalMessage{Type: "input", Data: "sleep 300\n"})
				_ = c.Write(ctx, websocket.MessageText, b)
			})
			break
		}
	}
	time.Sleep(300 * time.Millisecond)
	_ = c.CloseNow()

	select {
	case rec := <-th.sessions:
		if rec.ChainHead == "" {
			t.Error("the recording was not sealed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shell outlived the browser")
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

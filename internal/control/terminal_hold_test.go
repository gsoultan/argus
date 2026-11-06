package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

/*
A terminal outlives the link between argus-control and the agent.

The stream to the agent goes whenever the tunnel reconnects or argus-control
restarts. An agent that holds shells keeps this one running and recording,
argus-control gets back to it -- by itself while the browser waits, or when
the browser comes back to a control plane that has forgotten it -- and the
person carries on in the same shell. Then the edges: a shell running a
command outlives the hold until the command is done, an agent too old to hold
says so, and the person at the terminal hears when the agent is stopping.
*/

// cutTunnel ends host's tunnel as a network blip or a proxy would. The agent
// reconnects on its own about a second later.
func (e *tunnelEnv) cutTunnel(t *testing.T, host string) {
	t.Helper()
	h := e.api.Tunnels
	h.mu.Lock()
	lt := h.byHost[tunnelKey(DefaultTenantID, host)]
	h.mu.Unlock()
	if lt == nil {
		t.Fatal("no tunnel to cut")
	}
	_ = lt.sess.Close()
}

// forget makes this control plane lose the session as a restart would: no
// record of it, nothing sent to the agent.
func (e *tunnelEnv) forget(t *testing.T, id string) {
	t.Helper()
	ls, ok := e.api.live.get(context.Background(), id)
	if !ok || ls.term == nil {
		t.Fatal("no such session relayed here")
	}
	ts := ls.term
	ts.hungUp.Store(true) // its relay must not reconnect, as a dead process would not
	ts.end(websocket.StatusGoingAway, "restart")
	_ = ts.stream().Close()
}

// messagesUntil reads text messages and output until a message of type want
// arrives, returning the output seen on the way.
func messagesUntil(t *testing.T, c *websocket.Conn, want string, within time.Duration) (terminalMessage, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	var out strings.Builder
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no %q message (output so far %q): %v", want, out.String(), err)
		}
		if typ == websocket.MessageBinary {
			out.Write(data)
			continue
		}
		var m terminalMessage
		if json.Unmarshal(data, &m) == nil && (m.Type == want || m.Kind == want) {
			return m, out.String()
		}
	}
}

func TestATunnelDropDoesNotEndTheSessionOrTheBrowser(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-blip")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	untilReady(t, c, "X=kept-$((6*7)); echo before-$X\n")
	readOutput(t, c, "before-kept-42")

	e.cutTunnel(t, host)
	messagesUntil(t, c, "agent-reconnecting", 10*time.Second)
	if _, out := messagesUntil(t, c, "agent-back", 20*time.Second); strings.Contains(out, "before-kept-42") {
		t.Errorf("the reconnect repeated output the browser already had: %q", out)
	}
	if sealed(t, th, 0) {
		t.Fatal("the tunnel dropping ended the session")
	}
	// The same shell, with its variable, in the same browser.
	typeInto(t, c, "echo after-$X\n")
	readOutput(t, c, "after-kept-42")
	typeInto(t, c, "exit 5\n")
	if run := readTerminal(t, c, 10*time.Second); run.closed.ExitCode != 5 {
		t.Errorf("closed as %+v", run.closed)
	}
}

func TestAControlPlaneThatForgotTheSessionGetsItBackFromTheAgent(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-restart")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	ready := untilReady(t, c, "X=kept-$((6*7)); echo before-$X\n")
	readOutput(t, c, "before-kept-42")

	// argus-control restarts: its record of the session goes, the browser's
	// connection goes, and so does the agent's tunnel.
	_ = c.CloseNow()
	e.forget(t, ready.Session)
	e.cutTunnel(t, host)
	if sealed(t, th, 500*time.Millisecond) {
		t.Fatal("the agent hung the shell up when its stream dropped")
	}

	// A ticket for the same host and account does not make it someone
	// else's.
	other := e.resumeTerminal(t, signer, "eve@example.com", host, me(t), ready.Session)
	for {
		m := firstMessage(t, other)
		if m.Type == "retry" {
			other = e.resumeTerminal(t, signer, "eve@example.com", host, me(t), ready.Session)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if m.Type != "gone" {
			t.Fatalf("another person's resume got %+v", m)
		}
		break
	}

	// The browser comes back, as the console does, until the agent is there.
	var back *websocket.Conn
	var again terminalMessage
	deadline := time.Now().Add(20 * time.Second)
	for {
		back = e.resumeTerminal(t, signer, "ana@example.com", host, me(t), ready.Session)
		again = firstMessage(t, back)
		if again.Type != "retry" || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if again.Type != "ready" || again.Session != ready.Session || !again.Resumed {
		t.Fatalf("came back to %+v, want session %s resumed", again, ready.Session)
	}
	readOutput(t, back, "before-kept-42")
	typeInto(t, back, "echo after-$X\n")
	readOutput(t, back, "after-kept-42")
	typeInto(t, back, "exit 6\n")
	if run := readTerminal(t, back, 10*time.Second); run.closed.ExitCode != 6 {
		t.Errorf("closed as %+v", run.closed)
	}
	if !sealed(t, th, 5*time.Second) {
		t.Fatal("no sealed recording")
	}
	events, _ := e.store.AuditEvents(context.Background(), 50, 0)
	found := false
	for _, ev := range events {
		if ev.Action == "terminal.reattached" && ev.ActorEmail == "ana@example.com" &&
			strings.Contains(ev.Detail, "new connection to the agent") {
			found = true
		}
	}
	if !found {
		t.Error("no terminal.reattached entry for the resume through the agent")
	}
}

func TestASessionThatEndedWhileUnreachableSaysHowItEnded(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-ended-away")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	untilReady(t, c, "sleep 1; exit 3\n")
	time.Sleep(100 * time.Millisecond)
	e.cutTunnel(t, host)
	// The shell exits while nobody can hear it; the reconnect learns how.
	if run := readTerminal(t, c, 20*time.Second); run.closed.ExitCode != 3 {
		t.Errorf("closed as %+v, errors %v", run.closed, run.errors)
	}
}

func TestARunningCommandOutlivesTheHoldAndAnIdleShellDoesNot(t *testing.T) {
	e := newTunnelEnv(t)
	e.api.TerminalReattachWindow = time.Second
	e.api.TerminalKeepRunning = time.Minute
	signer := e.withTerminal(t)
	host := unique("term-keep")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	// An upgrade, as far as the shell can tell: something in the
	// foreground that takes longer than the hold.
	untilReady(t, c, "sleep 4\n")
	time.Sleep(300 * time.Millisecond)
	_ = c.CloseNow()
	if sealed(t, th, 3*time.Second) {
		t.Fatal("the shell was hung up while its command was still running")
	}
	// Back at its prompt with nobody there, it goes at the next look.
	if !sealed(t, th, 20*time.Second) {
		t.Fatal("the shell outlived its command with nobody attached")
	}
}

func TestAnAgentTooOldToHoldSaysSoInsteadOfOpeningAFreshShell(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-old")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgentHolding(t, cred, host, t.TempDir(), false)

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	ready := untilReady(t, c)
	_ = c.CloseNow()
	e.forget(t, ready.Session)
	if !sealed(t, th, 10*time.Second) {
		t.Fatal("an agent that cannot hold kept the shell")
	}
	var m terminalMessage
	for i := 0; i < 50; i++ {
		m = firstMessage(t, e.resumeTerminal(t, signer, "ana@example.com", host, me(t), ready.Session))
		if m.Type != "retry" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if m.Type != "gone" || !strings.Contains(m.Data, "too old") {
		t.Fatalf("got %+v, want gone naming the agent as too old", m)
	}
}

func TestThePersonAtTheTerminalHearsTheAgentIsStopping(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("term-drain")
	cred, _ := enrolled(t, e.store, e.h, host)
	th := e.runTerminalAgent(t, cred, host, t.TempDir())

	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c, _, err := e.openTerminal(t, host, me(t), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	untilReady(t, c)
	go func() { _ = th.col.CloseWithin(3 * time.Second) }()
	m, _ := messagesUntil(t, c, "restarting", 10*time.Second)
	if m.EndsInS != 3 || !strings.Contains(m.Data, "stopping") {
		t.Errorf("notice %+v", m)
	}
}

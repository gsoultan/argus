package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/argus/internal/wskeep"
)

/*
The browser terminal, through an agent's tunnel.

	browser ──wss──▶ argus-control ──"terminal" stream──▶ agent ──▶ login shell

The control plane redeems the console's single-use ticket -- the same ticket,
minted by the same checks, that a gateway redeems -- and turns it into a grant
the agent checks for itself: signed, for one host, one account and one person,
valid for a minute and spendable once. It then relays bytes and nothing else.
It does not speak SSH, holds no key for the host, and keeps no copy of the
session: the agent records it, on the host, before it shows it.

To the browser this speaks the gateway's protocol, so the console has one
terminal component for both: JSON for control messages, with output as binary
messages instead of JSON strings. Binary is cheaper, and output that is not
valid UTF-8 -- or a multi-byte character split across two reads -- arrives
intact rather than replaced.
*/

// terminalOpenWait bounds how long the agent has to verify the grant, open
// the recording and start the shell. A Windows account's first session
// creates its profile, which took 17 seconds on a CI runner.
const terminalOpenWait = 60 * time.Second

// handleTerminal serves GET /api/v1/terminal.
func (a *API) handleTerminal(w http.ResponseWriter, r *http.Request) {
	target, principal := r.URL.Query().Get("target"), r.URL.Query().Get("principal")
	if target == "" || principal == "" {
		writeErr(w, http.StatusBadRequest, "target and principal are required")
		return
	}
	if a.signer == nil || a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this control plane cannot open sessions through agents: set signing_secret")
		return
	}
	token := r.URL.Query().Get("ticket")
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "no ticket supplied")
		return
	}
	// Verify and burn in one step, bound to this target and principal: a
	// ticket for one host cannot open another.
	t, err := a.signerFor(r).RedeemTicket(r.Context(), token, target, principal)
	if err != nil {
		a.log.Warn("terminal refused", "target", target, "principal", principal, "error", err)
		writeErr(w, http.StatusUnauthorized, "unauthorized: "+err.Error())
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns(a.AllowedOrigins),
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	if resume := r.URL.Query().Get("resume"); resume != "" {
		a.reattachTerminal(r.Context(), conn, t.Email, target, principal, resume, a.clientAddr(r))
		return
	}
	a.relayTerminal(r.Context(), conn, t.Email, target, principal, a.clientAddr(r))
}

// originPatterns turns the CORS allow-list into websocket host patterns.
// Same-origin is always allowed by the library.
func originPatterns(allowed []string) []string {
	out := make([]string, 0, len(allowed))
	for _, o := range allowed {
		if i := strings.Index(o, "://"); i >= 0 {
			o = o[i+3:]
		}
		if o != "" {
			out = append(out, o)
		}
	}
	return out
}

type terminalMessage struct {
	Type      string `json:"type"`
	Data      string `json:"data,omitempty"`
	Session   string `json:"session,omitempty"`
	ChainHead string `json:"chain_head,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
	// Resumed is set on the ready that answers a reattach: the shell is the
	// one that was already running, and the output that follows starts with
	// the recent screen rather than a fresh prompt.
	Resumed bool `json:"resumed,omitempty"`
	// Clipboard is a desktop's clipboard rule, on its ready: which way text
	// may cross, so the console offers only what will work.
	Clipboard string `json:"clipboard,omitempty"`
	// Kind says what a "notice" is about: "agent-reconnecting",
	// "agent-back", "restarting" (the agent is stopping, EndsInS from now),
	// "control-restarting", "output-missed".
	Kind    string `json:"kind,omitempty"`
	EndsInS int    `json:"ends_in_s,omitempty"`
}

/*
A dropped connection is not the end of the session.

	browser ─╳─ argus-control ══"terminal" stream══ agent ══ shell
	   ⋮            │ the stream stays open; output still reaches the
	browser ────────┘ recording and the session's backlog
	(again, within terminalReattachWindow)

Wi-Fi changing, a laptop lid closing, a phone putting the tab in the
background, a proxy restarting: each closes the browser's WebSocket, and
until now each one hung the shell up -- the person came back to a fresh
prompt and whatever they had running was gone. Nobody had ended anything.

So the control plane holds the stream to the agent for a while after the
browser goes, and the same person, with a fresh ticket for the same host and
account, can pick it back up: they get the recent screen from the session's
backlog and then the live output. Only one browser holds a session at a
time; a second one takes it over and the first is told so, rather than two
windows typing into one shell.

Closing the terminal on purpose says so ("hangup") and hangs up at once, as
before. A session nobody comes back to is hung up when the window passes,
and the recording then reads as it always did for a closed browser. The
recording is the agent's and is unaffected either way: it never knew the
browser had gone.
*/

// defaultTerminalReattachWindow is how long a shell outlives its browser.
//
// Long enough for a laptop to wake or a train to leave a tunnel; short enough
// that a shell walked away from is not left logged in for long.
const defaultTerminalReattachWindow = 5 * time.Minute

func (a *API) terminalReattachWindow() time.Duration {
	if a.TerminalReattachWindow > 0 {
		return a.TerminalReattachWindow
	}
	return defaultTerminalReattachWindow
}

// defaultTerminalKeepRunning is how long a shell nobody came back to is still
// kept while a command runs in its foreground. Long enough for a
// distribution upgrade; bounded, because it is still a logged-in shell.
const defaultTerminalKeepRunning = 4 * time.Hour

func (a *API) terminalKeepRunning() time.Duration {
	switch {
	case a.TerminalKeepRunning < 0:
		return 0
	case a.TerminalKeepRunning > 0:
		return a.TerminalKeepRunning
	}
	return defaultTerminalKeepRunning
}

// terminalHold is what the agent is asked to keep when its stream drops.
func (a *API) terminalHold() *tunnel.TermHold {
	return &tunnel.TermHold{ForS: int(a.terminalReattachWindow().Seconds()),
		RunningS: int(a.terminalKeepRunning().Seconds())}
}

// attachmentQueue is how far a browser may fall behind before output waits
// for it. Output waits rather than being dropped, as it always has: the shell
// is held up by a slow browser, never shown to it with holes.
const attachmentQueue = 64

// termSession is one browser terminal, independent of whichever browser
// connection is showing it.
type termSession struct {
	a         *API
	ls        *liveSession
	email     string
	host      string
	principal string
	client    string
	// tctx carries the organization the session is in, for signing the
	// grant a reconnect to the agent needs; it is never cancelled.
	tctx context.Context
	st   tunnel.Stream
	stmu sync.Mutex // frames to the agent are written whole; guards st
	// agentHolds is set when the agent keeps the shell if the stream drops,
	// so a lost stream is reconnected rather than the end of the session.
	agentHolds bool
	// received is how much output has come from the agent, which is where a
	// reconnect asks it to carry on from. Only run touches it.
	received int64
	// hungUp is set once the shell was hung up on purpose: no reconnect.
	hungUp atomic.Bool
	// moved is set when the agent says another stream has the session now.
	moved atomic.Bool
	// released is set once the agent was told nobody is watching; see
	// tunnel.FrameRelease. Guarded by mu.
	released bool

	mu       sync.Mutex
	att      *termAttachment // the browser showing it, or nil
	timer    *time.Timer     // hangs up a session nobody came back to
	over     bool            // the shell has ended
	ended    chan struct{}
	endOnce  sync.Once
	endedBy  string
	endWhy   string
	termOnce sync.Once
}

func (ts *termSession) writeStream(typ byte, p []byte) error {
	ts.stmu.Lock()
	defer ts.stmu.Unlock()
	return tunnel.WriteFrame(ts.st, typ, p)
}

func (ts *termSession) stream() tunnel.Stream {
	ts.stmu.Lock()
	defer ts.stmu.Unlock()
	return ts.st
}

// termOut is one message for a browser, in order.
type termOut struct {
	typ   websocket.MessageType
	b     []byte
	close bool
	code  websocket.StatusCode
	why   string
}

// termAttachment is one browser connection showing a session.
type termAttachment struct {
	conn *websocket.Conn
	// first is sent before anything queued on out: ready and, on a
	// reattach, the screen the browser missed.
	first    []termOut
	out      chan termOut
	gone     chan struct{}
	goneOnce sync.Once
}

func newAttachment(conn *websocket.Conn) *termAttachment {
	return &termAttachment{conn: conn, out: make(chan termOut, attachmentQueue), gone: make(chan struct{})}
}

func (t *termAttachment) leave() { t.goneOnce.Do(func() { close(t.gone) }) }

// put queues m, or drops it once the browser has gone.
func (t *termAttachment) put(m termOut) {
	select {
	case t.out <- m:
	case <-t.gone:
	}
}

func (t *termAttachment) json(m terminalMessage) {
	b, _ := json.Marshal(m)
	t.put(termOut{typ: websocket.MessageText, b: b})
}

func (t *termAttachment) closeWith(code websocket.StatusCode, why string) {
	t.put(termOut{close: true, code: code, why: why})
}

// write sends everything queued for this browser until it goes or is closed.
func (t *termAttachment) write(ctx context.Context) {
	defer t.leave()
	for _, m := range t.first {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := t.conn.Write(wctx, m.typ, m.b)
		cancel()
		if err != nil {
			return
		}
	}
	t.first = nil
	for {
		select {
		case <-t.gone:
			return
		case m := <-t.out:
			if m.close {
				_ = t.conn.Close(m.code, m.why)
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := t.conn.Write(wctx, m.typ, m.b)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (a *API) relayTerminal(ctx context.Context, conn *websocket.Conn,
	email, host, principal, client string) {

	fail := func(why string) {
		b, _ := json.Marshal(terminalMessage{Type: "error", Data: why})
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = conn.Write(wctx, websocket.MessageText, b)
		cancel()
		_ = conn.Close(websocket.StatusNormalClosure, "refused")
	}

	agentHost, err := a.agentOf(ctx, host)
	if err != nil {
		fail("could not find the agent on " + host + ": " + err.Error())
		return
	}
	g := tunnel.NewGrant(email, agentHost, principal, client)
	// On a workstation whoever is at its screen is told who opened a
	// terminal, by its type's rule or its own; a terminal takes nothing
	// from them, so it never waits on an answer.
	ask, err := a.askFor(ctx, host, email, false)
	if err != nil {
		fail("could not read who is told before connecting: " + err.Error())
		return
	}
	g.Ask = ask
	grant, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		fail("could not sign the session grant")
		return
	}
	octx, cancel := context.WithTimeout(ctx, terminalOpenWait)
	st, answer, err := a.Tunnels.OpenWith(octx, agentHost,
		tunnel.Header{Service: agentTerminalService, Grant: grant, Hold: a.terminalHold()})
	cancel()
	switch {
	case errors.Is(err, ErrNoTunnel):
		fail("the agent on " + host + " is not connected to this control plane, so a " +
			"session cannot open through it; check the agent is running and shows " +
			"as connected under Servers")
		return
	case tunnel.IsRefused(err):
		a.log.Warn("terminal: the agent refused the session", "host", host,
			"principal", principal, "email", email, "error", err)
		fail("the agent on " + host + " refused the session: " + answer.Error)
		return
	case err != nil:
		fail("could not reach the agent on " + host + ": " + err.Error())
		return
	}

	if _, aerr := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "terminal.opened",
		Severity:   "notice",
		ActorEmail: email,
		Target:     host,
		Detail: "Browser terminal opened as " + principal + " through the agent's tunnel; " +
			"session " + answer.Session + ", recorded on the host.",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	ts := a.newTermSession(ctx, email, host, principal, client, st, answer)
	// The browser that opened it is attached before the agent's first
	// output is read, so it misses none of it.
	att := newAttachment(conn)
	ts.attach(att, false)
	go ts.run()
	ts.serve(ctx, att)
}

func (a *API) newTermSession(ctx context.Context, email, host, principal, client string,
	st tunnel.Stream, answer tunnel.Answer) *termSession {
	ts := &termSession{a: a, email: email, host: host, principal: principal, client: client,
		tctx: context.WithoutCancel(ctx), st: st, agentHolds: answer.Holds,
		received: answer.From, ended: make(chan struct{})}
	ts.ls = &liveSession{tenant: tenantIDOf(ctx), id: answer.Session, protocol: "ssh", hub: live.NewHub(),
		width: 80, height: 24, term: ts}
	ts.ls.terminate = ts.terminate
	a.live.add(ts.ls)
	return ts
}

// reattachTerminal puts a browser back on a session whose connection dropped.
//
// The ticket was minted by the same checks as one for a new session and is
// bound to this host and account, so access withdrawn in the meantime is not
// kept by coming back. The session must also be this person's, on this host,
// as this account: a session id is not a credential.
func (a *API) reattachTerminal(ctx context.Context, conn *websocket.Conn,
	email, host, principal, id, client string) {

	gone := func(why string) {
		b, _ := json.Marshal(terminalMessage{Type: "gone", Data: why})
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = conn.Write(wctx, websocket.MessageText, b)
		cancel()
		_ = conn.Close(websocket.StatusNormalClosure, "gone")
	}
	ls, ok := a.live.get(ctx, id)
	if !ok || ls.term == nil {
		// Not relayed here: this control plane restarted since, or another
		// replica had it. The agent may still be holding the shell.
		a.resumeFromAgent(ctx, conn, email, host, principal, id, client)
		return
	}
	ts := ls.term
	if ts.email != email || ts.host != host || ts.principal != principal {
		a.log.Warn("terminal reattach refused: not this person's session",
			"session", id, "email", email, "host", host, "principal", principal)
		gone("the session has ended on the server, or another replica is relaying it")
		return
	}
	att := newAttachment(conn)
	if !ts.attach(att, true) {
		gone("the session has ended on the server")
		return
	}
	a.log.Info("terminal reattached", "session", id, "email", email, "host", host)
	if _, aerr := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "terminal.reattached",
		Severity:   "notice",
		ActorEmail: email,
		Target:     host,
		Detail: "Browser terminal reconnected to session " + id + " as " + principal +
			" after its connection dropped; the shell kept running and recording meanwhile.",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	ts.serve(ctx, att)
}

// resumeFromAgent picks a terminal this control plane is not relaying back up
// from the agent holding it, and puts the browser on it.
//
// The agent checks the fresh grant is for the person and account the shell
// was opened for, so a session id here is no more a credential than it is
// anywhere else.
func (a *API) resumeFromAgent(ctx context.Context, conn *websocket.Conn,
	email, host, principal, id, client string) {

	say := func(typ, why string) {
		b, _ := json.Marshal(terminalMessage{Type: typ, Data: why})
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = conn.Write(wctx, websocket.MessageText, b)
		cancel()
		_ = conn.Close(websocket.StatusNormalClosure, typ)
	}
	st, answer, err := a.openResume(ctx, email, host, principal, client, id, 0)
	switch {
	case errors.Is(err, ErrNoTunnel):
		// The agent has not reconnected yet -- this control plane has just
		// started, or the network is still out. Not an answer about the
		// session, so the browser tries again.
		say("retry", "waiting for the agent on "+host+" to reconnect")
		return
	case tunnel.IsRefused(err) && answer.Error == "unknown service":
		say("gone", "the agent on "+host+" is too old to keep a session through a "+
			"reconnect; update it from the server's page so the next one is kept")
		return
	case tunnel.IsRefused(err):
		say("gone", answer.Error)
		return
	case err != nil:
		say("retry", "could not reach the agent on "+host+": "+err.Error())
		return
	}
	ts := a.newTermSession(ctx, email, host, principal, client, st, answer)
	a.log.Info("terminal resumed from the agent", "session", id, "email", email, "host", host)
	if _, aerr := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "terminal.reattached",
		Severity:   "notice",
		ActorEmail: email,
		Target:     host,
		Detail: "Browser terminal reconnected to session " + id + " as " + principal +
			" through a new connection to the agent, which kept the shell running and " +
			"recording while argus-control could not reach it.",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	att := newAttachment(conn)
	ts.attach(att, true)
	go ts.run()
	ts.serve(ctx, att)
}

// openResume asks host's agent for the held terminal id, from output byte from.
func (a *API) openResume(ctx context.Context, email, host, principal, client, id string,
	from int64) (tunnel.Stream, tunnel.Answer, error) {
	agentHost, err := a.agentOf(ctx, host)
	if err != nil {
		return nil, tunnel.Answer{}, err
	}
	grant, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), tunnel.NewGrant(email, agentHost, principal, client))
	if err != nil {
		return nil, tunnel.Answer{}, err
	}
	octx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return a.Tunnels.OpenWith(octx, agentHost, tunnel.Header{Service: tunnel.TerminalResumeService,
		Grant: grant, Resume: &tunnel.TermResume{Session: id, From: from}})
}

// attach makes att the browser showing the session, replacing any other.
// It reports false once the shell has ended.
func (ts *termSession) attach(att *termAttachment, resumed bool) bool {
	ts.mu.Lock()
	if ts.over {
		ts.mu.Unlock()
		return false
	}
	if ts.timer != nil {
		ts.timer.Stop()
		ts.timer = nil
	}
	old := ts.att
	ts.att = att
	unrelease := ts.released
	ts.released = false
	// Taken under the same lock run delivers under: every frame is either in
	// this backlog or delivered to att afterwards, never both or neither.
	ready, _ := json.Marshal(terminalMessage{Type: "ready", Session: ts.ls.id, Resumed: resumed})
	att.first = []termOut{{typ: websocket.MessageText, b: ready}}
	if resumed {
		for _, f := range ts.ls.hub.Backlog() {
			if len(f) > 0 && f[0] == hubOutput {
				att.first = append(att.first, termOut{typ: websocket.MessageBinary, b: f[1:]})
			}
		}
	}
	ts.mu.Unlock()
	if unrelease {
		_ = ts.writeStream(tunnel.FrameRelease, []byte("0"))
	}

	if old != nil {
		// Two windows typing into one shell is never what anyone meant. The
		// one left behind is told why, so it does not try to come back.
		go func() {
			old.json(terminalMessage{Type: "moved",
				Data: "this session was opened in another window"})
			old.closeWith(websocket.StatusNormalClosure, "moved")
		}()
	}
	return true
}

// serve runs one browser connection until it goes, then detaches it.
func (ts *termSession) serve(ctx context.Context, att *termAttachment) {
	// Idle shells look dead to the proxy in front; see wskeep.
	go wskeep.Run(ctx, att.conn)
	go att.write(ctx)

	defer ts.detach(att)
	for {
		typ, data, err := att.conn.Read(ctx)
		if err != nil {
			att.leave()
			return
		}
		ts.mu.Lock()
		current := ts.att == att
		ts.mu.Unlock()
		if !current {
			// Replaced by another window, or the session ended; anything
			// this one still sends is not for the shell. Its writer is
			// telling it why and closing it.
			continue
		}
		if typ != websocket.MessageText {
			continue
		}
		var m terminalMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "input":
			// A paste can be larger than a frame; split it rather than
			// end the session over it.
			for in := []byte(m.Data); len(in) > 0; {
				n := min(len(in), 32<<10)
				if ts.writeStream(tunnel.FrameInput, in[:n]) != nil {
					if ts.agentHolds {
						// The link to the agent is being restored; the
						// browser has been told, and typing is not sent
						// until it is back.
						break
					}
					return
				}
				in = in[n:]
			}
		case "resize":
			if m.Cols > 0 && m.Rows > 0 {
				if ts.writeStream(tunnel.FrameResize, tunnel.ResizePayload(m.Cols, m.Rows)) != nil &&
					!ts.agentHolds {
					return
				}
				ts.ls.setSize(m.Cols, m.Rows)
				ts.ls.hub.Broadcast(hubResizeFrame(m.Cols, m.Rows))
			}
		case "hangup":
			// Closed on purpose: hang the shell up now, as closing the
			// browser always did, rather than wait for a return that is
			// not coming.
			ts.hangup()
			return
		}
	}
}

// detach notes that att has gone. If it was the one showing the session, the
// shell keeps running for the reattach window and is hung up after it.
func (ts *termSession) detach(att *termAttachment) {
	att.leave()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.att != att || ts.over {
		return
	}
	ts.att = nil
	window := ts.a.terminalReattachWindow()
	ts.a.log.Info("terminal connection dropped; keeping the shell for a reconnect",
		"session", ts.ls.id, "email", ts.email, "host", ts.host, "window", window)
	ts.timer = time.AfterFunc(window, func() {
		ts.mu.Lock()
		stale := ts.att != nil || ts.over
		release := !stale && ts.agentHolds && ts.a.terminalKeepRunning() > 0
		if release {
			ts.released = true
		}
		ts.mu.Unlock()
		if stale {
			return
		}
		if release {
			// The agent hangs it up now if it is at its prompt, and once the
			// command running in it finishes otherwise. The stream stays,
			// so the session can still be watched and ended from here.
			ts.a.log.Info("terminal not reconnected in time; the shell goes once "+
				"its running command, if any, finishes",
				"session", ts.ls.id, "email", ts.email, "host", ts.host)
			if ts.writeStream(tunnel.FrameRelease, []byte("1")) == nil {
				return
			}
		}
		ts.a.log.Info("terminal not reconnected in time; hanging up",
			"session", ts.ls.id, "email", ts.email, "host", ts.host)
		ts.hangup()
	})
}

// hangup ends the shell on the host. An agent that holds shells is told so
// first -- a stream that merely closes is a dropped one, which it keeps. The
// recording then reads as a closed browser, as it always has.
func (ts *termSession) hangup() {
	ts.hungUp.Store(true)
	if ts.agentHolds {
		_ = ts.writeStream(tunnel.FrameHangup, nil)
	}
	_ = ts.stream().Close()
}

// terminate asks the agent to end the session for by, for reason.
func (ts *termSession) terminate(by, reason string) bool {
	select {
	case <-ts.ended:
		return false
	default:
	}
	first := false
	ts.termOnce.Do(func() {
		first = true
		ts.stmu.Lock()
		ts.endedBy, ts.endWhy = by, reason
		ts.stmu.Unlock()
	})
	if !first {
		return true
	}
	if err := ts.writeStream(tunnel.FrameTerminate, tunnel.TerminatePayload(by, reason)); err != nil &&
		!ts.agentHolds {
		// The agent is unreachable on this stream. Closing it hangs the
		// shell up all the same; the recording then reads as dropped.
		_ = ts.stream().Close()
	}
	// An agent that holds shells and is being reconnected to is sent the
	// termination again once it is back; see reconnectAgent.
	return true
}

// run relays the agent's output until the shell ends, whoever is attached.
func (ts *termSession) run() {
	defer func() { _ = ts.stream().Close() }()
	for {
		if ts.relay(ts.stream()) {
			return
		}
		// The agent went away without saying how the session ended.
		switch {
		case ts.moved.Load():
			ts.end(websocket.StatusNormalClosure, "moved", terminalMessage{Type: "moved",
				Data: "this session was opened in another window"})
			return
		case ts.agentHolds && !ts.hungUp.Load() && ts.reconnectAgent():
			continue
		case ts.agentHolds && !ts.hungUp.Load() && stopping(ts.a.Tunnels):
			// This control plane is going away, not the session: the browser
			// is left to reconnect, to the next one, which picks it up.
			ts.end(websocket.StatusGoingAway, "restarting")
			return
		}
		// Said as such: the recording is on the host, sealed or not, and the
		// console must not invent an exit status for it.
		ts.end(websocket.StatusGoingAway, "agent gone", terminalMessage{Type: "error",
			Data: "the connection to the agent ended before the session did"})
		return
	}
}

// relay reads st until it ends. It reports true when the session is over.
func (ts *termSession) relay(st tunnel.Stream) bool {
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			return false
		}
		switch typ {
		case tunnel.FrameOutput:
			ts.received += int64(len(p))
			// The same bytes to whoever is watching; the hub never waits.
			// Into the backlog and to the browser under one lock; see attach.
			// The reader reuses p; the hub frame is a copy, and the browser
			// is handed that copy's payload, since its writer runs later.
			frame := append([]byte{hubOutput}, p...)
			ts.mu.Lock()
			ts.ls.hub.Broadcast(frame)
			att := ts.att
			ts.mu.Unlock()
			if att != nil {
				att.put(termOut{typ: websocket.MessageBinary, b: frame[1:]})
			}
		case tunnel.FrameNotice:
			n, ok := tunnel.ParseNotice(p)
			if !ok {
				continue
			}
			if n.Kind == "moved" {
				ts.moved.Store(true)
				continue
			}
			ts.tell(terminalMessage{Type: "notice", Kind: n.Kind, Data: n.Text, EndsInS: n.EndsInS})
		case tunnel.FrameExit:
			code, head, _ := tunnel.ParseExit(p)
			ts.stmu.Lock()
			by, why := ts.endedBy, ts.endWhy
			ts.stmu.Unlock()
			msgs := []terminalMessage{}
			if by != "" {
				msgs = append(msgs, terminalMessage{Type: "error",
					Data: "this session was ended by " + by + ": " + why})
			}
			msgs = append(msgs, terminalMessage{Type: "closed", ExitCode: code,
				Session: ts.ls.id, ChainHead: head})
			ts.end(websocket.StatusNormalClosure, "exit", msgs...)
			return true
		}
	}
}

func stopping(h *TunnelHub) bool {
	select {
	case <-h.Done():
		return true
	default:
		return false
	}
}

// tell sends m to whichever browser is showing the session, if any.
func (ts *termSession) tell(m terminalMessage) {
	ts.mu.Lock()
	att := ts.att
	ts.mu.Unlock()
	if att != nil {
		att.json(m)
	}
}

// agentReconnectDelays is how soon each try to get back to the agent comes.
var agentReconnectDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second}

/*
reconnectAgent gets back to the agent holding the shell after the stream to it
dropped -- the tunnel reconnecting after a network blip, a proxy recycling the
agent's WebSocket -- while the browser stays where it is. The agent carries on
from the last byte this side had, so the screen neither repeats nor skips;
typing in the meantime is not sent, and the browser is told why.

It gives up when the agent says the session is not there, or when the agent's
hold has run out, after which there is nothing left to reconnect to.
*/
func (ts *termSession) reconnectAgent() bool {
	ts.tell(terminalMessage{Type: "notice", Kind: "agent-reconnecting",
		Data: "lost the connection to the agent on " + ts.host + "; the shell is still " +
			"running there, reconnecting…"})
	ts.a.log.Info("terminal: the stream to the agent dropped; reconnecting",
		"session", ts.ls.id, "host", ts.host)
	deadline := time.Now().Add(ts.a.terminalReattachWindow())
	for i := 0; time.Now().Before(deadline); i++ {
		select {
		case <-time.After(agentReconnectDelays[min(i, len(agentReconnectDelays)-1)]):
		case <-ts.a.Tunnels.Done():
			// This control plane is stopping; the browser reconnects to
			// whichever answers next, and that one picks the shell up.
			return false
		}
		if ts.hungUp.Load() {
			return false
		}
		st, answer, err := ts.a.openResume(ts.tctx, ts.email, ts.host, ts.principal, ts.client,
			ts.ls.id, ts.received)
		if tunnel.IsRefused(err) {
			ts.a.log.Info("terminal: the agent no longer has the session",
				"session", ts.ls.id, "host", ts.host, "reason", answer.Error)
			return false
		}
		if err != nil {
			continue
		}
		ts.stmu.Lock()
		ts.st = st
		by, why := ts.endedBy, ts.endWhy
		ts.stmu.Unlock()
		ts.a.log.Info("terminal: reconnected to the agent", "session", ts.ls.id,
			"host", ts.host, "after_tries", i+1)
		if answer.Gap {
			ts.tell(terminalMessage{Type: "notice", Kind: "output-missed",
				Data: "some output from while the connection was down is no longer held; " +
					"the session recording has all of it"})
		}
		ts.received = answer.From
		if by != "" {
			_ = ts.writeStream(tunnel.FrameTerminate, tunnel.TerminatePayload(by, why))
		}
		ts.mu.Lock()
		released := ts.released
		ts.mu.Unlock()
		if released {
			_ = ts.writeStream(tunnel.FrameRelease, []byte("1"))
		}
		ts.tell(terminalMessage{Type: "notice", Kind: "agent-back",
			Data: "reconnected to the agent on " + ts.host})
		return true
	}
	return false
}

// end marks the shell over, takes it off the registry before the browser
// hears, so nothing can be watched, ended or reattached once it has, and
// tells whichever browser is showing it.
func (ts *termSession) end(code websocket.StatusCode, why string, msgs ...terminalMessage) {
	ts.endOnce.Do(func() {
		ts.mu.Lock()
		ts.over = true
		att := ts.att
		ts.att = nil
		if ts.timer != nil {
			ts.timer.Stop()
			ts.timer = nil
		}
		ts.mu.Unlock()
		close(ts.ended)
		ts.a.live.remove(ts.ls)
		if att != nil {
			for _, m := range msgs {
				att.json(m)
			}
			att.closeWith(code, why)
		}
	})
}

// agentOf is the name the agent on host goes by, which a grant has to name
// and its tunnel is found under. host is what the console calls the server,
// and stays what the session, its recording and the audit chain are about.
func (a *API) agentOf(ctx context.Context, host string) (string, error) {
	if a.store == nil {
		return host, nil
	}
	return a.store.AgentNameFor(ctx, host)
}

// agentTerminalService is the agent's name for it; see agent.TerminalService.
const agentTerminalService = "terminal"

// OpenWith opens a stream on host's agent with a full header.
func (h *TunnelHub) OpenWith(ctx context.Context, host string, hd tunnel.Header) (tunnel.Stream, tunnel.Answer, error) {
	h.mu.Lock()
	t := h.byHost[tunnelKey(tenantIDOf(ctx), host)]
	h.mu.Unlock()
	if t == nil {
		return nil, tunnel.Answer{}, ErrNoTunnel
	}
	return tunnel.OpenWith(ctx, t.sess, hd)
}

// grantKeyPublic is what agents are sent; empty when sessions are off.
func (h *TunnelHub) grantKeyPublic(ctx context.Context) string {
	k := h.grantKeyFor(ctx)
	if k == nil {
		return ""
	}
	return tunnel.EncodeGrantKey(k.Public().(ed25519.PublicKey))
}

// grantKeyFor is the key that signs session grants for ctx's organization.
//
// Derived from GrantKey's seed and the organization id, so a grant signed for
// one organization's servers does not verify on another's: each agent pins
// the key of the organization it enrolled into. The default organization
// keeps GrantKey itself, so agents enrolled before there were several go on
// verifying against the key they pinned.
func (h *TunnelHub) grantKeyFor(ctx context.Context) ed25519.PrivateKey {
	if h.GrantKey == nil {
		return nil
	}
	id := tenantIDOf(ctx)
	if id == DefaultTenantID {
		return h.GrantKey
	}
	h.gmu.Lock()
	defer h.gmu.Unlock()
	if k, ok := h.grantKeys[id]; ok {
		return k
	}
	if h.grantKeys == nil {
		h.grantKeys = map[string]ed25519.PrivateKey{}
	}
	k := tunnel.DeriveGrantKey(h.GrantKey, id)
	h.grantKeys[id] = k
	return k
}

// WarnTerminalsRestarting tells every browser terminal this control plane is
// relaying that it is about to restart. Their shells are held by agents that
// can, and the browsers reconnect on their own; this is so the pause reads as
// one. Safe to call more than once.
func (a *API) WarnTerminalsRestarting() {
	a.live.mu.Lock()
	terms := make([]*termSession, 0, len(a.live.m))
	for _, ls := range a.live.m {
		if ls.term != nil {
			terms = append(terms, ls.term)
		}
	}
	a.live.mu.Unlock()
	for _, ts := range terms {
		msg := "argus-control is restarting; this session reconnects when it is back"
		if !ts.agentHolds {
			msg = "argus-control is restarting, and this server's agent is too old to keep " +
				"the session through it: this session will end"
		}
		ts.tell(terminalMessage{Type: "notice", Kind: "control-restarting", Data: msg})
	}
}

package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
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
		a.reattachTerminal(r.Context(), conn, t.Email, target, principal, resume)
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
	st        tunnel.Stream
	stmu      sync.Mutex // frames to the agent are written whole

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

	grant, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), tunnel.NewGrant(email, host, principal, client))
	if err != nil {
		fail("could not sign the session grant")
		return
	}
	octx, cancel := context.WithTimeout(ctx, terminalOpenWait)
	st, answer, err := a.Tunnels.OpenWith(octx, host,
		tunnel.Header{Service: agentTerminalService, Grant: grant})
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

	ts := &termSession{a: a, email: email, host: host,
		principal: principal, st: st, ended: make(chan struct{})}
	ts.ls = &liveSession{tenant: tenantIDOf(ctx), id: answer.Session, protocol: "ssh", hub: live.NewHub(),
		width: 80, height: 24, term: ts}
	ts.ls.terminate = ts.terminate
	a.live.add(ts.ls)

	// The browser that opened it is attached before the agent's first
	// output is read, so it misses none of it.
	att := newAttachment(conn)
	ts.attach(att, false)
	go ts.run()
	ts.serve(ctx, att)
}

// reattachTerminal puts a browser back on a session whose connection dropped.
//
// The ticket was minted by the same checks as one for a new session and is
// bound to this host and account, so access withdrawn in the meantime is not
// kept by coming back. The session must also be this person's, on this host,
// as this account: a session id is not a credential.
func (a *API) reattachTerminal(ctx context.Context, conn *websocket.Conn,
	email, host, principal, id string) {

	gone := func(why string) {
		b, _ := json.Marshal(terminalMessage{Type: "gone", Data: why})
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = conn.Write(wctx, websocket.MessageText, b)
		cancel()
		_ = conn.Close(websocket.StatusNormalClosure, "gone")
	}
	ls, ok := a.live.get(ctx, id)
	if !ok || ls.term == nil {
		gone("the session has ended on the server, or another replica is relaying it")
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
					return
				}
				in = in[n:]
			}
		case "resize":
			if m.Cols > 0 && m.Rows > 0 {
				if ts.writeStream(tunnel.FrameResize, tunnel.ResizePayload(m.Cols, m.Rows)) != nil {
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
		ts.mu.Unlock()
		if stale {
			return
		}
		ts.a.log.Info("terminal not reconnected in time; hanging up",
			"session", ts.ls.id, "email", ts.email, "host", ts.host)
		ts.hangup()
	})
}

// hangup closes the stream, which hangs the shell up on the host. The
// recording then reads as a closed browser, as it always has.
func (ts *termSession) hangup() { _ = ts.st.Close() }

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
	if err := ts.writeStream(tunnel.FrameTerminate, tunnel.TerminatePayload(by, reason)); err != nil {
		// The agent is unreachable on this stream. Closing it hangs the
		// shell up all the same; the recording then reads as dropped.
		_ = ts.st.Close()
	}
	return true
}

// run relays the agent's output until the shell ends, whoever is attached.
func (ts *termSession) run() {
	defer ts.st.Close()
	fr := tunnel.NewFrameReader(ts.st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			// The agent went away without saying how the session ended. Said
			// as such: the recording is on the host, sealed or not, and the
			// console must not invent an exit status for it.
			ts.end(websocket.StatusGoingAway, "agent gone", terminalMessage{Type: "error",
				Data: "the connection to the agent ended before the session did"})
			return
		}
		switch typ {
		case tunnel.FrameOutput:
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
			return
		}
	}
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

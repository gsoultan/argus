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
	t, err := a.signer.RedeemTicket(r.Context(), token, target, principal)
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
}

func (a *API) relayTerminal(ctx context.Context, conn *websocket.Conn,
	email, host, principal, client string) {

	var wmu sync.Mutex
	send := func(typ websocket.MessageType, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return conn.Write(wctx, typ, b)
	}
	sendJSON := func(m terminalMessage) error {
		b, _ := json.Marshal(m)
		return send(websocket.MessageText, b)
	}
	fail := func(why string) {
		_ = sendJSON(terminalMessage{Type: "error", Data: why})
		_ = conn.Close(websocket.StatusNormalClosure, "refused")
	}

	grant, err := tunnel.SignGrant(a.Tunnels.GrantKey, tunnel.NewGrant(email, host, principal, client))
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
	defer st.Close()

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
	if sendJSON(terminalMessage{Type: "ready", Session: answer.Session}) != nil {
		return
	}
	// Idle shells look dead to the proxy in front; see wskeep.
	go wskeep.Run(ctx, conn)

	// Writes to the stream come from the browser loop only, so frames
	// cannot interleave. When the browser goes, the stream is closed, which
	// is what hangs the shell up on the host.
	go func() {
		defer st.Close()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
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
					if tunnel.WriteFrame(st, tunnel.FrameInput, in[:n]) != nil {
						return
					}
					in = in[n:]
				}
			case "resize":
				if m.Cols > 0 && m.Rows > 0 &&
					tunnel.WriteFrame(st, tunnel.FrameResize, tunnel.ResizePayload(m.Cols, m.Rows)) != nil {
					return
				}
			}
		}
	}()

	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			// The agent went away without saying how the session ended. Said
			// as such: the recording is on the host, sealed or not, and the
			// console must not invent an exit status for it.
			_ = sendJSON(terminalMessage{Type: "error",
				Data: "the connection to the agent ended before the session did"})
			_ = conn.Close(websocket.StatusGoingAway, "agent gone")
			return
		}
		switch typ {
		case tunnel.FrameOutput:
			if send(websocket.MessageBinary, p) != nil {
				// The browser is gone: closing the stream hangs the shell up.
				return
			}
		case tunnel.FrameExit:
			code, head, _ := tunnel.ParseExit(p)
			_ = sendJSON(terminalMessage{Type: "closed", ExitCode: code,
				Session: answer.Session, ChainHead: head})
			_ = conn.Close(websocket.StatusNormalClosure, "exit")
			return
		}
	}
}

// agentTerminalService is the agent's name for it; see agent.TerminalService.
const agentTerminalService = "terminal"

// OpenWith opens a stream on host's agent with a full header.
func (h *TunnelHub) OpenWith(ctx context.Context, host string, hd tunnel.Header) (tunnel.Stream, tunnel.Answer, error) {
	h.mu.Lock()
	t := h.byHost[strings.ToLower(host)]
	h.mu.Unlock()
	if t == nil {
		return nil, tunnel.Answer{}, ErrNoTunnel
	}
	return tunnel.OpenWith(ctx, t.sess, hd)
}

// grantKeyPublic is what agents are sent; empty when sessions are off.
func (h *TunnelHub) grantKeyPublic() string {
	if h.GrantKey == nil {
		return ""
	}
	return tunnel.EncodeGrantKey(h.GrantKey.Public().(ed25519.PublicKey))
}

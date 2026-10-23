package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/argus/internal/wskeep"
)

/*
Remote Desktop in the browser, through a Windows agent's tunnel.

	browser ──wss──▶ argus-control ──"desktop" stream──▶ agent ──▶ the host's own RDP

Ticket and grant as for a terminal; the grant is for "desktop", so neither can
be spent on the other. The agent is the RDP client and records the session;
this side relays display frames one way and input messages the other, and
keeps a hub so the session can be watched.

To the browser this speaks the gateway's display protocol (rdp/display.go),
encoded frames only, with two additions: the browser's first message is its
logon -- the account's password and the size it wants -- and a refusal arrives
as a JSON {"type":"error"} message before the socket closes, so the console
can say why.
*/

// agentDesktopService is the agent's name for it; see agent.DesktopService.
const agentDesktopService = "desktop"

// desktopLogonWait bounds how long the browser has to send its logon.
const desktopLogonWait = 2 * time.Minute

type desktopLogon struct {
	Type     string `json:"type"`
	Password string `json:"password"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// handleDesktop serves GET /api/v1/desktop.
func (a *API) handleDesktop(w http.ResponseWriter, r *http.Request) {
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
	t, err := a.signer.RedeemTicket(r.Context(), token, target, principal)
	if err != nil {
		a.log.Warn("desktop refused", "target", target, "principal", principal, "error", err)
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
	// Input messages are small; the bound stops a browser making this side
	// buffer on its say-so.
	conn.SetReadLimit(64 << 10)
	a.relayDesktop(r.Context(), conn, t.Email, target, principal, a.clientAddr(r))
}

func (a *API) relayDesktop(ctx context.Context, conn *websocket.Conn,
	email, host, principal, client string) {

	var wmu sync.Mutex
	send := func(typ websocket.MessageType, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return conn.Write(wctx, typ, b)
	}
	sendText := func(m terminalMessage) error {
		b, _ := json.Marshal(m)
		return send(websocket.MessageText, b)
	}
	fail := func(why string) {
		_ = sendText(terminalMessage{Type: "error", Data: why})
		_ = send(websocket.MessageBinary, rdp.EncodeControl(rdp.FrameClosed, 0, 0))
		_ = conn.Close(websocket.StatusNormalClosure, "refused")
	}

	lctx, cancel := context.WithTimeout(ctx, desktopLogonWait)
	typ, data, err := conn.Read(lctx)
	cancel()
	var logon desktopLogon
	if err != nil || typ != websocket.MessageText || json.Unmarshal(data, &logon) != nil ||
		logon.Type != "logon" {
		fail("the console did not send its logon")
		return
	}
	// The password is in memory here for the length of this call, and goes
	// nowhere but the agent's stream header.
	defer func() { logon.Password = "" }()

	g := tunnel.NewGrant(email, host, principal, client)
	g.Service = agentDesktopService
	grant, err := tunnel.SignGrant(a.Tunnels.GrantKey, g)
	if err != nil {
		fail("could not sign the session grant")
		return
	}
	octx, cancel := context.WithTimeout(ctx, terminalOpenWait)
	st, answer, err := a.Tunnels.OpenWith(octx, host, tunnel.Header{
		Service: agentDesktopService, Grant: grant,
		Desktop: &tunnel.DesktopOpen{Password: logon.Password, Width: logon.Width, Height: logon.Height},
	})
	cancel()
	logon.Password = ""
	switch {
	case errors.Is(err, ErrNoTunnel):
		fail("the agent on " + host + " is not connected to this control plane, so a " +
			"desktop cannot open through it; check the agent is running and shows as " +
			"connected under Servers")
		return
	case tunnel.IsRefused(err):
		a.log.Warn("desktop: the agent refused the session", "host", host,
			"principal", principal, "email", email, "error", err)
		fail("the agent on " + host + " refused the session: " + answer.Error)
		return
	case err != nil:
		fail("could not reach the agent on " + host + ": " + err.Error())
		return
	}
	defer st.Close()

	if _, aerr := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "desktop.opened",
		Severity:   "notice",
		ActorEmail: email,
		Target:     host,
		Detail: "Remote Desktop opened in the browser as " + principal + " through the " +
			"agent's tunnel; session " + answer.Session + ", recorded on the host.",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	var stmu sync.Mutex
	writeStream := func(typ byte, p []byte) error {
		stmu.Lock()
		defer stmu.Unlock()
		return tunnel.WriteFrame(st, typ, p)
	}
	ended := make(chan struct{})
	var endOnce sync.Once
	var endedBy, endReason string
	ls := &liveSession{id: answer.Session, protocol: "rdp", hub: live.NewHub()}
	ls.refresh = func() { _ = writeStream(tunnel.FrameRefresh, nil) }
	ls.terminate = func(by, reason string) bool {
		select {
		case <-ended:
			return false
		default:
		}
		first := false
		endOnce.Do(func() {
			first = true
			stmu.Lock()
			endedBy, endReason = by, reason
			stmu.Unlock()
		})
		if first && writeStream(tunnel.FrameTerminate, tunnel.TerminatePayload(by, reason)) != nil {
			_ = st.Close()
		}
		return true
	}
	a.live.add(ls)
	finish := sync.OnceFunc(func() {
		close(ended)
		a.live.remove(ls)
	})
	defer finish()
	go wskeep.Run(ctx, conn)

	// Input, forwarded as the browser sent it; the agent maps it to RDP.
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
			if writeStream(tunnel.FrameInput, data) != nil {
				return
			}
		}
	}()

	ready := false
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			_ = sendText(terminalMessage{Type: "error",
				Data: "the connection to the agent ended before the session did"})
			_ = send(websocket.MessageBinary, rdp.EncodeControl(rdp.FrameClosed, 0, 0))
			_ = conn.Close(websocket.StatusGoingAway, "agent gone")
			return
		}
		switch typ {
		case tunnel.FrameResize:
			wd, ht, ok := tunnel.ParseResize(p)
			if !ok {
				continue
			}
			ls.setSize(wd, ht)
			kind := rdp.FrameResize
			if !ready {
				kind, ready = rdp.FrameReady, true
			}
			ls.hub.Broadcast(rdp.EncodeControl(rdp.FrameResize, wd, ht))
			if send(websocket.MessageBinary, rdp.EncodeControl(kind, wd, ht)) != nil {
				return
			}
		case tunnel.FrameOutput:
			// The same frames to whoever is watching; the hub never waits.
			ls.hub.Broadcast(append([]byte(nil), p...))
			if send(websocket.MessageBinary, p) != nil {
				return
			}
		case tunnel.FrameExit:
			finish()
			stmu.Lock()
			by, why := endedBy, endReason
			stmu.Unlock()
			if by != "" {
				_ = sendText(terminalMessage{Type: "error",
					Data: "this session was ended by " + by + ": " + why})
			}
			_ = send(websocket.MessageBinary, rdp.EncodeControl(rdp.FrameClosed, 0, 0))
			_ = conn.Close(websocket.StatusNormalClosure, "ended")
			return
		}
	}
}

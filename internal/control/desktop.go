package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// agentScreenService is screen sharing, for a server whose desktop mode is
// "screen": the agent shares the console itself, without Remote Desktop.
// See agent.ScreenService. A grant for one cannot be spent on the other.
const agentScreenService = "screen"

// agentAndroidService is a device's screen through the Argus app on it;
// see agent.AndroidService. A grant for it opens neither of the others.
const agentAndroidService = "android"

// desktopLogonWait bounds how long the browser has to send its logon.
const desktopLogonWait = 2 * time.Minute

type desktopLogon struct {
	Type     string `json:"type"`
	Password string `json:"password"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	// Graphics asks Remote Desktop for its graphics pipeline, which the
	// browser decodes.
	Graphics bool `json:"graphics"`
	// Video says the browser decodes H.264, so screen sharing may send
	// motion as it.
	Video bool `json:"video"`
	// SlowLink is the browser saying its link is slow, where it can tell.
	SlowLink bool `json:"slowLink"`
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
	t, err := a.signerFor(r).RedeemTicket(r.Context(), token, target, principal)
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
	// buffer on its say-so. A paste is the exception, up to the clipboard's
	// own limit, and is refused past it in relayDesktop.
	conn.SetReadLimit(tunnel.MaxFrame)
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

	// The server's setting decides, not the browser: a console cannot ask
	// for the console's screen on a server set to Remote Desktop.
	kind, mode, err := a.store.AssetDesktopOpen(ctx, host)
	if err != nil {
		fail("could not read how this server's desktop opens: " + err.Error())
		return
	}
	service, what := agentDesktopService, "Remote Desktop"
	switch {
	case kind == "android":
		// A device's screen, through the Argus app on it: nobody is signed
		// in by Argus, so there is no password, and none is passed on.
		service, what = agentAndroidService, "The device's screen"
		logon.Password = ""
		logon.Graphics = logon.Video
	case mode == "screen":
		// Sharing the console signs nobody in, so there is no password to
		// pass on, and none is.
		service, what = agentScreenService, "Screen sharing of the console"
		logon.Password = ""
		// Pictures for motion, then exact tiles once still; a browser
		// that cannot decode them gets exact tiles only.
		logon.Graphics = logon.Video
	}

	agentHost, err := a.agentOf(ctx, host)
	if err != nil {
		fail("could not find the agent on " + host + ": " + err.Error())
		return
	}
	// The asset type's rule says which way text may cross, and is held to
	// here as well as by the agent. Remote Desktop has a clipboard channel
	// (synced); screen sharing has none yet, and the console uses the rule
	// there only to offer paste as typing. A device has neither.
	clip, synced := tunnel.ClipboardOff, service == agentDesktopService
	if service != agentAndroidService {
		if clip, err = a.clipboardFor(ctx, host); err != nil {
			fail("could not read this server's clipboard rule: " + err.Error())
			return
		}
	}
	openClip := tunnel.ClipboardOff
	if synced {
		openClip = clip
	}
	g := tunnel.NewGrant(email, agentHost, principal, client)
	g.Service = service
	// A workstation is someone's: by its type's rule, or its own, whoever
	// is using it is asked first, and the open waits for their answer as
	// well as for the agent. A device's app asks on its own.
	openWait := terminalOpenWait
	if kind != "android" {
		ask, err := a.askFor(ctx, host, email, true)
		if err != nil {
			fail("could not read who is asked before connecting: " + err.Error())
			return
		}
		g.Ask = ask
		if ask != nil && ask.Mode == tunnel.AskFirst {
			openWait += time.Duration(ask.Seconds) * time.Second
		}
	}
	grant, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		fail("could not sign the session grant")
		return
	}
	octx, cancel := context.WithTimeout(ctx, openWait)
	st, answer, err := a.Tunnels.OpenWith(octx, agentHost, tunnel.Header{
		Service: service, Grant: grant,
		Desktop: &tunnel.DesktopOpen{Password: logon.Password, Width: logon.Width, Height: logon.Height,
			Graphics: logon.Graphics, Clipboard: openClip, SlowLink: logon.SlowLink},
	})
	cancel()
	logon.Password = ""
	switch {
	case errors.Is(err, ErrNoTunnel):
		fail("the agent on " + host + " is not connected to this control plane, so a " +
			"desktop cannot open through it; check the agent is running and shows as " +
			"connected under Servers")
		return
	case tunnel.IsRefused(err) && service == agentScreenService && answer.Error == "unknown service":
		fail("the agent on " + host + " is too old to share its screen; update it, or set " +
			"this server back to Remote Desktop")
		return
	case tunnel.IsRefused(err) && service == agentAndroidService && answer.Error == "unknown service":
		fail("what is connected as " + host + " is not the Argus app for Android, so it has " +
			"no screen to share; check the device and its app")
		return
	case tunnel.IsRefused(err) && strings.HasPrefix(answer.Error, tunnel.AskRefusedPrefix):
		a.auditAskRefused(ctx, email, host, what+" as "+principal, answer.Error)
		fail(strings.TrimPrefix(answer.Error, tunnel.AskRefusedPrefix) + ", so the session did not open. " +
			"Ask them, or try again later.")
		return
	case tunnel.IsRefused(err):
		a.log.Warn("desktop: the agent refused the session", "host", host,
			"principal", principal, "email", email, "service", service, "error", err)
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
		Detail:     desktopAuditDetail(what, service, principal, answer.Session),
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
	ls := &liveSession{tenant: tenantIDOf(ctx), id: answer.Session, protocol: "rdp", hub: live.NewHub()}
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
	// The session's id, for the console's link to it; a console that does
	// not know this message ignores it.
	if sendText(terminalMessage{Type: "ready", Session: answer.Session, Clipboard: clip}) != nil {
		return
	}

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
			if bytes.Contains(data, []byte(`"clipboard"`)) {
				var m struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(data, &m) == nil && m.Type == "clipboard" {
					if !synced || !tunnel.ClipboardAllows(clip, true) || len(m.Text) > rdp.ClipboardMax ||
						len(data) > tunnel.MaxFrame {
						continue
					}
					a.auditClipboard(ctx, email, host, answer.Session, true, m.Text)
				}
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
		case tunnel.FrameClipboard:
			// Copied on the desktop. Not to watchers: it is the person's.
			if !synced || !tunnel.ClipboardAllows(clip, false) || len(p) > rdp.ClipboardMax {
				continue
			}
			a.auditClipboard(ctx, email, host, answer.Session, false, string(p))
			if sendText(terminalMessage{Type: "clipboard", Data: string(p)}) != nil {
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

// auditClipboard records one crossing of a desktop's clipboard: who, which
// way and how much. Not what: that stays between the person and the desktop,
// as it would at the machine.
func (a *API) auditClipboard(ctx context.Context, email, host, session string, toAsset bool, text string) {
	detail := fmt.Sprintf("copied %d characters out of the desktop", len([]rune(text)))
	if toAsset {
		detail = fmt.Sprintf("pasted %d characters into the desktop", len([]rune(text)))
	}
	if session != "" {
		detail += " (session " + session + ")"
	}
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: "desktop.clipboard", Severity: "info", ActorEmail: email, Target: host, Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
}

// desktopAuditDetail says what opened. Screen sharing acts as whoever is
// signed in at the console rather than as the principal the grant names, and
// the audit says so instead of naming an account that may not be the one.
func desktopAuditDetail(what, service, principal, session string) string {
	if service == agentAndroidService {
		verb := "to drive it"
		if principal == DeviceView {
			verb = "to watch it, with no input"
		}
		return what + " opened in the browser through the Argus app's tunnel as " + principal +
			", " + verb + "; it acts as whoever holds the device; session " + session +
			", recorded on the device."
	}
	if service == agentScreenService {
		return what + " opened in the browser through the agent's tunnel, acting as " +
			"whoever is signed in at the console (requested as " + principal + "; the " +
			"recording names each account); session " + session + ", recorded on the host."
	}
	return what + " opened in the browser as " + principal + " through the agent's " +
		"tunnel; session " + session + ", recorded on the host."
}

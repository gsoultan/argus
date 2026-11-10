package control

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/argus/internal/wskeep"
)

/*
Sessions this control plane is relaying, so they can be watched and ended.

	browser ──▶ argus-control ──tunnel──▶ agent
	                │ every byte of output also goes to the session's hub
	viewer  ◀───────┘ read only: nothing a viewer sends goes anywhere

The bytes a viewer sees are the ones the agent sent for the person working,
after it recorded them, so the live view and the replay cannot disagree.
Ending a session asks the agent to end it, naming who and why, so the
recording says so rather than reading as a dropped connection.

Only on the replica holding the relay: a viewer who reaches another replica is
told the session is not running here, the same limit the tunnel itself has.
*/

// liveSession is one session being relayed.
type liveSession struct {
	// tenant is the organization the session is in; see liveRegistry.
	tenant   string
	id       string
	protocol string // "ssh", "rdp", or a database engine: see statements
	hub      *live.Hub

	mu sync.Mutex
	// width and height are the terminal's columns and rows, or the
	// desktop's pixels: what a viewer arriving now must be drawn at.
	width, height int
	// terminate asks the agent to end the session for by, for reason. It
	// reports false when the session had already ended.
	terminate func(by, reason string) bool
	// refresh asks a desktop to repaint for a viewer arriving late.
	refresh func()
	// term is set on a browser terminal, which its owner can come back to
	// after their connection drops; see termSession.
	term *termSession
}

// statements reports whether this is a database session, whose viewers are
// sent its recording lines -- statements and what they answered -- rather
// than terminal output.
func (s *liveSession) statements() bool {
	return s.protocol != "ssh" && s.protocol != "rdp"
}

func (s *liveSession) size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.width, s.height
}

func (s *liveSession) setSize(w, h int) {
	s.mu.Lock()
	s.width, s.height = w, h
	s.mu.Unlock()
}

// liveRegistry is every session this replica is relaying. The zero value is
// ready to use.
type liveRegistry struct {
	mu sync.Mutex
	m  map[string]*liveSession
}

// Keyed by (organization, session id). Session ids are minted here and do
// not collide, but a watcher in one organization asking for an id must find
// nothing unless the session is in theirs, whatever the id is.
func (l *liveRegistry) add(s *liveSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*liveSession{}
	}
	l.m[tunnelKey(s.tenant, s.id)] = s
}

func (l *liveRegistry) remove(s *liveSession) {
	l.mu.Lock()
	if l.m[tunnelKey(s.tenant, s.id)] == s {
		delete(l.m, tunnelKey(s.tenant, s.id))
	}
	l.mu.Unlock()
	s.hub.Close()
}

func (l *liveRegistry) get(ctx context.Context, id string) (*liveSession, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.m[tunnelKey(tenantIDOf(ctx), id)]
	return s, ok
}

// Hub frames for a terminal carry a one-byte kind; a desktop's are display
// frames as the browser draws them.
const (
	hubOutput byte = 'o'
	hubResize byte = 'r'
)

func hubResizeFrame(cols, rows int) []byte {
	b := make([]byte, 5)
	b[0] = hubResize
	binary.BigEndian.PutUint16(b[1:3], uint16(cols))
	binary.BigEndian.PutUint16(b[3:5], uint16(rows))
	return b
}

// notRelayedHere is the answer for a session this replica is not relaying.
const notRelayedHere = "this session is not running through this control plane: it has " +
	"ended, it is a login that did not come through Argus, or another replica is relaying it"

// handleWatch serves GET /api/v1/sessions/{id}/watch: a read-only view of a
// session in flight, on a shadow ticket from handleShadowTicket, which
// decided who may watch whom and audited it.
//
// The socket is opened before the ticket is checked, so that a refusal
// reaches the person as a sentence. A browser is told nothing about an HTTP
// answer to a WebSocket handshake but that it failed, and every refusal --
// a spent ticket, a session that just ended, an agent that is not connected
// -- read in the console as the same bare "connection failed". Nothing but
// the refusal is sent until the ticket has been redeemed.
func (a *API) handleWatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token := r.URL.Query().Get("ticket")
	if token == "" {
		token = bearer(r)
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns(a.AllowedOrigins),
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()

	if token == "" || a.signer == nil {
		refuseWatch(ctx, conn, "no ticket was supplied to watch this session")
		return
	}
	t, err := a.signerFor(r).RedeemSessionScoped(ctx, token, auth.ScopeShadow, id)
	if err != nil {
		a.log.Warn("watch refused", "session", id, "error", err)
		refuseWatch(ctx, conn, "not authorised to watch this session: "+err.Error())
		return
	}
	go wskeep.Run(ctx, conn)

	if sess, ok := a.live.get(ctx, id); ok {
		a.log.Info("watching a session", "session", id, "viewer", t.Email, "protocol", sess.protocol)
		a.runWatch(ctx, conn, sess)
		a.log.Info("stopped watching a session", "session", id, "viewer", t.Email)
		return
	}
	// Not relayed here: a login through sshd, which never passes through
	// the control plane, or a browser session another replica relays. The
	// server's agent records both, and shows them from its recording.
	a.watchThroughAgent(ctx, conn, id, t.Email, a.clientAddr(r))
}

// refuseWatch tells the viewer why the view cannot open, and closes it.
func refuseWatch(ctx context.Context, conn *websocket.Conn, why string) {
	b, _ := json.Marshal(terminalMessage{Type: "error", Data: why})
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = conn.Write(wctx, websocket.MessageText, b)
	cancel()
	_ = conn.Close(websocket.StatusNormalClosure, "refused")
}

// watchOpenWait bounds how long the agent has to answer a watch.
const watchOpenWait = 15 * time.Second

// agentWatchService is the agent's name for it; see agent.WatchService.
const agentWatchService = "watch"

// watchThroughAgent shows a session from its server's own recording.
//
// The grant names the session, and the person, host and account it was
// recorded for, signed here after the shadow ticket was redeemed; the agent
// checks it as it checks every grant and does not take the session id on
// trust.
func (a *API) watchThroughAgent(ctx context.Context, conn *websocket.Conn,
	id, email, client string) {

	subject, err := a.store.Session(ctx, id)
	switch {
	case err != nil:
		a.log.Error("watch: load session", "session", id, "error", err)
		refuseWatch(ctx, conn, "the session could not be loaded; try again")
		return
	case subject == nil:
		refuseWatch(ctx, conn, "no such session")
		return
	case subject.State != "active":
		closedWatch(ctx, conn, "the session ended")
		return
	case subject.Protocol == "rdp" || subject.Kind == "database":
		// A desktop's frames are relayed, not recorded as a screen, and a
		// database session's statements reach viewers from the relay: the
		// agent's watch service knows terminals only. Only the replica
		// relaying either can show it.
		refuseWatch(ctx, conn, notRelayedHere)
		return
	case a.Tunnels == nil || a.Tunnels.GrantKey == nil:
		refuseWatch(ctx, conn, "this control plane cannot reach agents to watch a session: "+
			"set signing_secret")
		return
	}
	host := subject.AssetHostname
	agentHost, err := a.agentOf(ctx, host)
	if err != nil {
		refuseWatch(ctx, conn, "could not find the agent on "+host+": "+err.Error())
		return
	}

	g := tunnel.NewGrant(email, agentHost, subject.Principal, client)
	g.Service, g.Session = agentWatchService, id
	grant, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		refuseWatch(ctx, conn, "could not sign the watch grant")
		return
	}
	octx, cancel := context.WithTimeout(ctx, watchOpenWait)
	st, answer, err := a.Tunnels.OpenWith(octx, agentHost,
		tunnel.Header{Service: agentWatchService, Grant: grant})
	cancel()
	switch {
	case errors.Is(err, ErrNoTunnel):
		refuseWatch(ctx, conn, "the agent on "+host+" is not connected to this control plane, "+
			"so the session cannot be watched through it; check the agent is running and "+
			"shows as connected under Servers")
		return
	case tunnel.IsRefused(err):
		why := answer.Error
		if strings.Contains(why, "unknown service") {
			why = "its agent is older than this control plane and cannot show a session " +
				"live; update the agent from the server's page"
		}
		a.log.Warn("watch: the agent refused", "session", id, "host", host, "error", err)
		refuseWatch(ctx, conn, "the agent on "+host+" refused: "+why)
		return
	case err != nil:
		refuseWatch(ctx, conn, "could not reach the agent on "+host+": "+err.Error())
		return
	}
	defer st.Close()

	a.log.Info("watching a session through its agent", "session", id, "viewer", email, "host", host)
	defer a.log.Info("stopped watching a session", "session", id, "viewer", email)

	// Nothing a viewer sends goes anywhere; reading notices that they left,
	// and closing the stream then tells the agent.
	ctx, viewerLeft := context.WithCancel(ctx)
	defer viewerLeft()
	go func() {
		defer viewerLeft()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		_ = st.Close()
	}()

	write := func(typ websocket.MessageType, b []byte) error {
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return conn.Write(wctx, typ, b)
	}
	writeJSON := func(m terminalMessage) error {
		b, _ := json.Marshal(m)
		return write(websocket.MessageText, b)
	}
	if writeJSON(terminalMessage{Type: "ready", Session: id}) != nil {
		return
	}
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// The agent let this view go -- it fell behind -- or the
			// connection to it dropped. Either way the session may well be
			// running still.
			_ = writeJSON(terminalMessage{Type: "error",
				Data: "this view lost the session's stream and was closed; open it again to keep watching"})
			_ = conn.Close(websocket.StatusTryAgainLater, "behind")
			return
		}
		switch typ {
		case tunnel.FrameOutput:
			if write(websocket.MessageBinary, p) != nil {
				return
			}
		case tunnel.FrameResize:
			if cols, rows, ok := tunnel.ParseResize(p); ok {
				if writeJSON(terminalMessage{Type: "resize", Data: sizeString(cols, rows)}) != nil {
					return
				}
			}
		case tunnel.FrameExit:
			closedWatch(ctx, conn, "the session ended")
			return
		}
	}
}

// closedWatch tells the viewer the session is over.
func closedWatch(ctx context.Context, conn *websocket.Conn, why string) {
	b, _ := json.Marshal(terminalMessage{Type: "closed", Data: why})
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = conn.Write(wctx, websocket.MessageText, b)
	cancel()
	_ = conn.Close(websocket.StatusNormalClosure, "ended")
}

func (a *API) runWatch(ctx context.Context, conn *websocket.Conn, sess *liveSession) {
	// Read, and discard, whatever the viewer sends: pings and closes are
	// handled inside Read, and nothing a viewer types reaches the session.
	readCtx := ctx
	ctx, viewerLeft := context.WithCancel(ctx)
	defer viewerLeft()
	go func() {
		defer viewerLeft()
		for {
			if _, _, err := conn.Read(readCtx); err != nil {
				return
			}
		}
	}()

	write := func(typ websocket.MessageType, b []byte) error {
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return conn.Write(wctx, typ, b)
	}
	writeJSON := func(m terminalMessage) error {
		b, _ := json.Marshal(m)
		return write(websocket.MessageText, b)
	}

	if sess.protocol == "rdp" {
		if a.watchDesktop(ctx, sess, write) {
			_ = write(websocket.MessageBinary, rdp.EncodeControl(rdp.FrameClosed, 0, 0))
			_ = conn.Close(websocket.StatusNormalClosure, "ended")
		}
		return
	}

	backlog, frames, cancel := sess.hub.Subscribe()
	defer cancel()
	width, height := sess.size()

	if writeJSON(terminalMessage{Type: "ready", Session: sess.id}) != nil {
		return
	}
	if !sess.statements() && writeJSON(terminalMessage{Type: "resize", Data: sizeString(width, height)}) != nil {
		return
	}
	send := func(f []byte) error {
		if len(f) == 0 {
			return nil
		}
		switch f[0] {
		case hubOutput:
			return write(websocket.MessageBinary, f[1:])
		case hubRecord:
			// One line of a database session's recording, as stored.
			return writeJSON(terminalMessage{Type: "record", Data: string(f[1:])})
		case hubResize:
			if len(f) == 5 {
				return writeJSON(terminalMessage{Type: "resize", Data: sizeString(
					int(binary.BigEndian.Uint16(f[1:3])), int(binary.BigEndian.Uint16(f[3:5])))})
			}
		}
		return nil
	}
	// A screenful or two of what came before, so an idle shell does not look
	// like a broken view. For a database session, its recent statements.
	for _, f := range backlog {
		if send(f) != nil {
			return
		}
	}
	for f := range frames {
		if send(f) != nil {
			return
		}
	}
	if cur, ok := a.live.get(WithTenant(context.Background(), &Tenant{ID: sess.tenant}), sess.id); ok && cur == sess {
		// Still running: this viewer fell too far behind and was let go, so
		// the person working was never held up by it.
		_ = writeJSON(terminalMessage{Type: "error",
			Data: "this view could not keep up and was closed; open it again to keep watching"})
		_ = conn.Close(websocket.StatusTryAgainLater, "behind")
		return
	}
	_ = writeJSON(terminalMessage{Type: "closed", Data: "the session ended"})
	_ = conn.Close(websocket.StatusNormalClosure, "ended")
}

// watchDesktop streams a desktop to a viewer.
//
// Updates are deltas: a backlog would paint fragments of the recent past, so
// the desktop is asked to repaint instead. A viewer whose link cannot keep up
// is let go by the hub so the person working is never held up; rather than
// ending the view, it is caught up the same way it started -- subscribed
// afresh and repainted -- so it skips what it missed and carries on live.
// That keeps the hub's rule: the viewer is never shown a picture with pieces
// missing, because a repaint is the whole screen as it stands.
//
// It reports whether the session ended, as opposed to the viewer leaving.
func (a *API) watchDesktop(ctx context.Context, sess *liveSession,
	write func(websocket.MessageType, []byte) error) bool {
	width, height := sess.size()
	if write(websocket.MessageBinary, rdp.EncodeControl(rdp.FrameReady, width, height)) != nil {
		return false
	}
	var last time.Time
	for first := true; ; first = false {
		// At most one repaint a second for a viewer that keeps falling
		// behind: each is a whole screen, which is what it could not take.
		if wait := time.Second - time.Since(last); !first && wait > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(wait):
			}
		}
		last = time.Now()
		_, frames, cancel := sess.hub.Subscribe()
		if !first {
			// The desktop may have changed size while this viewer was away.
			w, h := sess.size()
			if write(websocket.MessageBinary, rdp.EncodeControl(rdp.FrameResize, w, h)) != nil {
				cancel()
				return false
			}
		}
		if sess.refresh != nil {
			sess.refresh()
		}
		for f := range frames {
			if write(websocket.MessageBinary, f) != nil {
				cancel()
				return false
			}
		}
		cancel()
		if ctx.Err() != nil {
			return false
		}
		cur, ok := a.live.get(WithTenant(context.Background(), &Tenant{ID: sess.tenant}), sess.id)
		if !ok || cur != sess {
			return true
		}
	}
}

func sizeString(w, h int) string { return strconv.Itoa(w) + "x" + strconv.Itoa(h) }

// handleTerminate serves POST /api/v1/sessions/{id}/terminate, on a
// terminate ticket from handleTerminateTicket, which decided who may end
// whose session and put the attempt in the audit chain.
func (a *API) handleTerminate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token := bearer(r)
	if token == "" || a.signer == nil {
		writeErr(w, http.StatusUnauthorized, "no ticket supplied")
		return
	}
	t, err := a.signerFor(r).RedeemSessionScoped(r.Context(), token, auth.ScopeTerminate, id)
	if err != nil {
		a.log.Warn("terminate refused", "session", id, "error", err)
		writeErr(w, http.StatusUnauthorized, "unauthorized: "+err.Error())
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in)
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		writeErr(w, http.StatusBadRequest, "a reason is required")
		return
	}
	sess, ok := a.live.get(r.Context(), id)
	if !ok {
		why := notRelayedHere
		if a.store == nil {
			writeErr(w, http.StatusNotFound, why)
			return
		}
		if subject, err := a.store.Session(r.Context(), id); err == nil && subject != nil &&
			subject.Origin == "direct" && subject.State == "active" {
			// Shown running, and watchable, since its agent reports it while
			// it runs; ending it is still the server's to do.
			why = "this is a direct SSH login, which did not come through Argus, so it " +
				"cannot be ended from the console; end it on the server"
		}
		writeErr(w, http.StatusNotFound, why)
		return
	}
	if !sess.terminate(t.Email, reason) {
		// Reporting success would tell an operator they stopped something
		// that had in fact run to completion.
		writeErr(w, http.StatusConflict, "the session had already ended")
		return
	}
	a.log.Warn("session ended from the console", "session", id, "by", t.Email, "reason", reason)
	writeJSON(w, http.StatusOK, map[string]any{
		"session": id, "terminated": true, "by": t.Email, "reason": reason,
	})
}

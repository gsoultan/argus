package control

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/rdp"
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
	id       string
	protocol string // "ssh" or "rdp"
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

func (l *liveRegistry) add(s *liveSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*liveSession{}
	}
	l.m[s.id] = s
}

func (l *liveRegistry) remove(s *liveSession) {
	l.mu.Lock()
	if l.m[s.id] == s {
		delete(l.m, s.id)
	}
	l.mu.Unlock()
	s.hub.Close()
}

func (l *liveRegistry) get(id string) (*liveSession, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.m[id]
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
func (a *API) handleWatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token := r.URL.Query().Get("ticket")
	if token == "" {
		token = bearer(r)
	}
	if token == "" || a.signer == nil {
		writeErr(w, http.StatusUnauthorized, "no ticket supplied")
		return
	}
	t, err := a.signer.RedeemSessionScoped(r.Context(), token, auth.ScopeShadow, id)
	if err != nil {
		a.log.Warn("watch refused", "session", id, "error", err)
		writeErr(w, http.StatusUnauthorized, "unauthorized: "+err.Error())
		return
	}
	sess, ok := a.live.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, notRelayedHere)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns(a.AllowedOrigins),
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	go wskeep.Run(r.Context(), conn)

	a.log.Info("watching a session", "session", id, "viewer", t.Email, "protocol", sess.protocol)
	a.runWatch(r.Context(), conn, sess)
	a.log.Info("stopped watching a session", "session", id, "viewer", t.Email)
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

	backlog, frames, cancel := sess.hub.Subscribe()
	defer cancel()
	width, height := sess.size()

	if sess.protocol == "rdp" {
		// Updates are deltas: a backlog would paint fragments of the recent
		// past. The desktop is asked to repaint instead.
		if write(websocket.MessageBinary, rdp.EncodeControl(rdp.FrameReady, width, height)) != nil {
			return
		}
		if sess.refresh != nil {
			sess.refresh()
		}
		for f := range frames {
			if write(websocket.MessageBinary, f) != nil {
				return
			}
		}
		_ = write(websocket.MessageBinary, rdp.EncodeControl(rdp.FrameClosed, 0, 0))
		_ = conn.Close(websocket.StatusNormalClosure, "ended")
		return
	}

	if writeJSON(terminalMessage{Type: "ready", Session: sess.id}) != nil ||
		writeJSON(terminalMessage{Type: "resize", Data: sizeString(width, height)}) != nil {
		return
	}
	send := func(f []byte) error {
		if len(f) == 0 {
			return nil
		}
		switch f[0] {
		case hubOutput:
			return write(websocket.MessageBinary, f[1:])
		case hubResize:
			if len(f) == 5 {
				return writeJSON(terminalMessage{Type: "resize", Data: sizeString(
					int(binary.BigEndian.Uint16(f[1:3])), int(binary.BigEndian.Uint16(f[3:5])))})
			}
		}
		return nil
	}
	// A screenful or two of what came before, so an idle shell does not look
	// like a broken view.
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
	if cur, ok := a.live.get(sess.id); ok && cur == sess {
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
	t, err := a.signer.RedeemSessionScoped(r.Context(), token, auth.ScopeTerminate, id)
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
	sess, ok := a.live.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, notRelayedHere)
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

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/rdp"
)

// handleRDPShadow attaches a read-only viewer to a Remote Desktop session.
//
// Read-only is a property of this handler never reading from the socket, not a
// flag the client is trusted to honour: a shadower has no path to the target's
// input at all.
func (s *Server) handleRDPShadow(cfg WebConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session")
		if id == "" {
			http.Error(w, "session is required", http.StatusBadRequest)
			return
		}

		viewer, err := cfg.authorizeSession(r, auth.ScopeShadow, id)
		if err != nil {
			s.log.Warn("rdp shadow refused", "session", id, "error", err)
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}

		sess, ok := s.RDPSession(id)
		if !ok {
			// The same answer whether the session never existed or has already
			// finished, so a ticket cannot be used to probe which ids are live.
			http.Error(w, "no such live session", http.StatusNotFound)
			return
		}

		conn, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: cfg.AllowedOrigins,
		})
		if wsErr != nil {
			return
		}
		defer conn.CloseNow()

		s.log.Info("rdp shadow attached",
			"session", id, "viewer", viewer, "subject", sess.User,
			"target", sess.Target, "principal", sess.Principal)
		s.runRDPShadow(r.Context(), conn, sess)
		s.log.Info("rdp shadow detached", "session", id, "viewer", viewer)
	}
}

func (s *Server) runRDPShadow(ctx context.Context, conn *websocket.Conn, sess *rdp.Session) {
	// Drain the viewer's side of the socket.
	//
	// A shadow is read-only and nothing a viewer sends ever reached the session
	// -- verified by typing into one and watching the target execute nothing.
	// But the socket was not read at all, and that costs two things:
	// coder/websocket handles ping and close frames inside Read, so a viewer
	// that went away was noticed only when a write to it eventually failed, and
	// whatever a viewer sent sat in a buffer nobody would drain.
	//
	// Done with a goroutine this function owns rather than conn.CloseRead: the
	// library's version is waited on by CloseNow, which the handler defers, and
	// the two deadlock on each other.
	// readCtx is captured before ctx is reassigned below. Closing over ctx
	// itself would have the goroutine reading the variable while this function
	// writes it -- a race the detector catches and a bug that would outlive it.
	readCtx := ctx
	viewerGone, viewerLeft := context.WithCancel(ctx)
	defer viewerLeft()
	go func() {
		defer viewerLeft()
		for {
			if _, _, err := conn.Read(readCtx); err != nil {
				return // the viewer closed, or the connection did
			}
			// Discarded on purpose. A viewer that can type is not a viewer.
		}
	}()
	ctx = viewerGone

	_, frames, cancel := sess.Hub().Subscribe()
	defer cancel()

	// The backlog is deliberately discarded. Screen updates are deltas, so
	// replaying whatever happened to be buffered would paint fragments of the
	// recent past over a blank canvas. Asking the target to redraw gives the
	// viewer what is actually on screen now.
	if err := writeBinary(ctx, conn, rdp.EncodeControl(
		rdp.FrameReady, sess.Width, sess.Height)); err != nil {
		return
	}
	// Ask the target to redraw so the viewer sees the screen as it stands.
	// Which mechanism depends on whether Argus is the client or a proxy; both
	// end in the same place.
	if client := s.rdpClientFor(sess.ID); client != nil {
		if err := client.Refresh(); err != nil {
			s.log.Warn("could not request a redraw for a shadow viewer",
				"session", sess.ID, "error", err)
		}
	} else if proxy := s.rdpProxyServer(); proxy != nil {
		if err := proxy.refresh(sess.ID); err != nil {
			// Not fatal: the viewer still sees everything from now on, which is
			// worse but not wrong, and saying so beats a blank canvas with no
			// explanation.
			s.log.Warn("could not request a redraw on a proxied session",
				"session", sess.ID, "error", err,
				"detail", "the viewer will see updates from this point rather than the current screen")
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case frame, open := <-frames:
			if !open {
				reason := "session ended"
				if by, why, killed := sess.Killed(); killed {
					reason = "session terminated by " + by + " (" + why + ")"
				} else if sess.Live() {
					reason = "the shadow stream fell behind and was dropped; reconnect to resume"
				}
				s.log.Info("rdp shadow ended", "session", sess.ID, "reason", reason)
				_ = writeBinary(ctx, conn, rdp.EncodeControl(rdp.FrameClosed, 0, 0))
				return
			}
			if err := writeBinary(ctx, conn, frame); err != nil {
				return
			}
		}
	}
}

// handleRDPTerminate ends a Remote Desktop session.
func (s *Server) handleRDPTerminate(cfg WebConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "session id is required", http.StatusBadRequest)
			return
		}

		actor, err := cfg.authorizeSession(r, auth.ScopeTerminate, id)
		if err != nil {
			s.log.Warn("rdp terminate refused", "session", id, "error", err)
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}

		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
		reason := strings.TrimSpace(body.Reason)
		if reason == "" {
			// A session cut short with no explanation is a gap in the record
			// rather than an entry in it.
			http.Error(w, "a reason is required", http.StatusBadRequest)
			return
		}

		sess, ok := s.RDPSession(id)
		if !ok {
			http.Error(w, "no such live session", http.StatusNotFound)
			return
		}
		if !sess.Terminate(actor, reason) {
			// Reporting success would tell an operator they stopped something
			// that had in fact run to completion.
			http.Error(w, "session had already ended", http.StatusConflict)
			return
		}
		// Closing the connection to the target is what actually ends it; the
		// relay or frame pump then unwinds and seals the recording.
		if client := s.rdpClientFor(id); client != nil {
			_ = client.Close()
		} else if proxy := s.rdpProxyServer(); proxy != nil {
			proxy.disconnect(id)
		}

		s.log.Warn("rdp session terminated", "session", id, "by", actor, "reason", reason)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session": id, "terminated": true, "by": actor, "reason": reason,
		})
	}
}

// RDPSession looks up one live Remote Desktop session.
//
// Searches both kinds: sessions Argus drives for a browser, and sessions it
// proxies for a native client. An operator watching or stopping a session
// should not have to know which door the user came in by.
func (s *Server) RDPSession(id string) (*rdp.Session, bool) {
	s.mu.Lock()
	sess, ok := s.rdpWeb[id]
	proxy := s.rdpProxy
	s.mu.Unlock()
	if ok {
		return sess, true
	}
	if proxy == nil {
		return nil, false
	}
	return proxy.session(id)
}

// rdpClientFor returns the RDP client driving a session, or nil.
func (s *Server) rdpClientFor(id string) *rdp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rdpClients[id]
}

func (s *Server) setRDPClient(id string, c *rdp.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rdpClients == nil {
		s.rdpClients = map[string]*rdp.Client{}
	}
	s.rdpClients[id] = c
}

func (s *Server) clearRDPClient(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rdpClients, id)
}

// ActiveRDPSessions returns a snapshot for the console.
func (s *Server) ActiveRDPSessions() []*rdp.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*rdp.Session, 0, len(s.rdpWeb))
	for _, sess := range s.rdpWeb {
		out = append(out, sess)
	}
	return out
}

func (s *Server) rdpProxyServer() *RDPServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rdpProxy
}

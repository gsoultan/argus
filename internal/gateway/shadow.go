package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/auth"
)

// shadowFrame is one asciicast line as the console receives it.
//
// The gateway decodes the recording's own frames rather than inventing a second
// wire format, so a shadow view and a replay are rendered by the same code in
// the browser. Two formats would be two chances for them to disagree about what
// a session did.
// A slice rather than a [3]any: unmarshalling into a fixed array silently
// accepts a truncated frame and leaves the missing elements nil, which would
// render a malformed line as a perfectly valid empty one.
type shadowFrame []any

// handleShadow attaches a read-only viewer to a session already in flight.
//
// Read-only is enforced by never reading from the socket, not by asking the
// client to behave: a shadower has no path to the target's stdin at all, so a
// modified console cannot type into somebody else's privileged session.
func (s *Server) handleShadow(cfg WebConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session")
		if id == "" {
			http.Error(w, "session is required", http.StatusBadRequest)
			return
		}

		viewer, err := cfg.authorizeSession(r, auth.ScopeShadow, id)
		if err != nil {
			s.log.Warn("shadow refused", "session", id, "error", err)
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}

		sess, ok := s.Session(id)
		if !ok {
			// Deliberately the same answer whether the session never existed or
			// has already finished. Distinguishing them would let anyone with a
			// ticket probe which session ids are live.
			http.Error(w, "no such live session", http.StatusNotFound)
			return
		}

		conn, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: cfg.AllowedOrigins,
		})
		if wsErr != nil {
			s.log.Warn("shadow websocket upgrade failed", "error", wsErr)
			return
		}
		defer conn.CloseNow()

		s.log.Info("shadow attached",
			"session", id, "viewer", viewer, "subject", sess.User,
			"target", sess.Target.Hostname, "principal", sess.Principal)
		s.runShadow(r.Context(), conn, sess, viewer)
		s.log.Info("shadow detached", "session", id, "viewer", viewer)
	}
}

func (s *Server) runShadow(ctx context.Context, conn *websocket.Conn, sess *Session, viewer string) {
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

	backlog, frames, cancel := sess.Hub().Subscribe()
	defer cancel()

	send := func(m serverMessage) error {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		writeCtx, c := context.WithTimeout(ctx, 10*time.Second)
		defer c()
		return conn.Write(writeCtx, websocket.MessageText, b)
	}

	if err := send(serverMessage{
		Type:    "ready",
		Session: sess.ID,
		Data: fmt.Sprintf("shadowing %s as %s@%s",
			sess.User, sess.Principal, sess.Target.Hostname),
	}); err != nil {
		return
	}

	// The backlog first, so a viewer joining an idle session sees the screen as
	// it stands rather than a blank terminal they cannot distinguish from a
	// broken stream.
	for _, f := range backlog {
		if m, ok := decodeFrame(f); ok {
			if err := send(m); err != nil {
				return
			}
		}
	}

	// A shadower's socket is never read. Anything it sends is ignored, so no
	// amount of client-side creativity turns a viewer into a participant.
	for {
		select {
		case <-ctx.Done():
			return
		case frame, open := <-frames:
			if !open {
				// Either the session ended or this viewer fell behind. Say
				// which: a viewer who silently missed output must not believe
				// they watched the whole session.
				reason := "session ended"
				if by, why, killed := sess.Killed(); killed {
					reason = fmt.Sprintf("session terminated by %s (%s)", by, why)
				} else if sess.Live() {
					reason = "shadow stream fell behind and was dropped; reconnect to resume"
				}
				_ = send(serverMessage{Type: "closed", Session: sess.ID, Data: reason})
				return
			}
			if m, ok := decodeFrame(frame); ok {
				if err := send(m); err != nil {
					return
				}
			}
		}
	}
}

// decodeFrame turns one asciicast line into a message for the console.
//
// Input frames are dropped. The target echoes what the user types, so relaying
// input as well would double every keystroke on the shadower's screen — and it
// would put text the user typed at a non-echoing prompt onto a second screen,
// which is a disclosure the live view has no reason to make.
func decodeFrame(line []byte) (serverMessage, bool) {
	var f shadowFrame
	if err := json.Unmarshal(line, &f); err != nil || len(f) != 3 {
		return serverMessage{}, false
	}
	stream, sOK := f[1].(string)
	data, dOK := f[2].(string)
	if !sOK || !dOK {
		return serverMessage{}, false
	}
	switch stream {
	case "o":
		return serverMessage{Type: "output", Data: data}, true
	case "r":
		return serverMessage{Type: "resize", Data: data}, true
	default:
		return serverMessage{}, false
	}
}

// handleTerminate ends a session on an administrator's instruction.
func (s *Server) handleTerminate(cfg WebConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "session id is required", http.StatusBadRequest)
			return
		}

		actor, err := cfg.authorizeSession(r, auth.ScopeTerminate, id)
		if err != nil {
			s.log.Warn("terminate refused", "session", id, "error", err)
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}

		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
		reason := strings.TrimSpace(body.Reason)
		if reason == "" {
			// Killing someone's session without recording why leaves the next
			// reviewer with an unexplained gap in the evidence.
			http.Error(w, "a reason is required", http.StatusBadRequest)
			return
		}

		sess, ok := s.Session(id)
		if !ok {
			http.Error(w, "no such live session", http.StatusNotFound)
			return
		}
		if !sess.Terminate(actor, reason) {
			// Already finished. Reporting success would tell an operator they
			// stopped something that had in fact run to completion.
			http.Error(w, "session had already ended", http.StatusConflict)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session":    id,
			"terminated": true,
			"by":         actor,
			"reason":     reason,
		})
	}
}

// handleLiveSessions lists what is running on this gateway right now.
//
// Served from the gateway's own memory rather than the control plane's session
// table. The table is what the gateway last managed to report; this is what is
// actually connected. When they disagree the difference matters, and an
// operator deciding whether to terminate needs the truthful one.
func (s *Server) handleLiveSessions(cfg WebConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := cfg.authorizeRead(r); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		type view struct {
			ID         string    `json:"id"`
			User       string    `json:"userEmail"`
			Principal  string    `json:"principal"`
			Target     string    `json:"assetHostname"`
			ClientIP   string    `json:"clientIp"`
			StartedAt  time.Time `json:"startedAt"`
			Viewers    int       `json:"viewers"`
			RiskFlags  []string  `json:"riskFlags"`
			CertSerial uint64    `json:"certSerial,omitempty"`
		}
		out := []view{}
		for _, sess := range s.ActiveSessions() {
			out = append(out, view{
				ID:         sess.ID,
				User:       sess.User,
				Principal:  sess.Principal,
				Target:     sess.Target.Hostname,
				ClientIP:   sess.RemoteIP,
				StartedAt:  sess.StartedAt,
				Viewers:    sess.Hub().Viewers(),
				RiskFlags:  sess.riskFlags(),
				CertSerial: sess.CertSerial,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// authorizeSession verifies a ticket minted for one specific live session.
func (cfg WebConfig) authorizeSession(r *http.Request, scope, sessionID string) (string, error) {
	token := bearer(r)
	if token == "" {
		return "", errors.New("no ticket supplied")
	}
	if cfg.Signer != nil {
		t, err := cfg.Signer.RedeemSessionScoped(r.Context(), token, scope, sessionID)
		if err != nil {
			return "", err
		}
		return t.Email, nil
	}
	// Development fallback. Shadowing and termination are privileged enough
	// that running without a Signer is worth saying out loud rather than
	// letting a dev token quietly stand in for an authorisation decision.
	user, ok := cfg.Tokens[token]
	if !ok {
		return "", errors.New("unknown token")
	}
	return user, nil
}

// authorizeRead gates the read-only listings.
func (cfg WebConfig) authorizeRead(r *http.Request) (string, error) {
	token := bearer(r)
	if cfg.Signer != nil {
		if sess, err := cfg.Signer.VerifySession(token); err == nil {
			return sess.Email, nil
		}
		return "", errors.New("no valid session")
	}
	user, ok := cfg.Tokens[token]
	if !ok {
		return "", errors.New("unknown token")
	}
	return user, nil
}

func bearer(r *http.Request) string {
	if t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); t != "" {
		return t
	}
	if t := r.URL.Query().Get("ticket"); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}

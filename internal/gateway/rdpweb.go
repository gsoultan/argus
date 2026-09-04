package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/credssp"
	"github.com/gsoultan/argus/internal/hostkey"
	"github.com/gsoultan/argus/internal/rdp"
)

// maxBatchBytes bounds one WebSocket message of screen rectangles.
//
// Rectangles are batched because a desktop update is often a dozen of them and
// one message each costs more in framing and event dispatch than the pixels do.
// The bound stops a full-screen repaint becoming a single message large enough
// to stall the socket, which would show as the whole session freezing rather
// than one frame arriving late.
const maxBatchBytes = 1 << 20

// handleRDPWeb serves a Remote Desktop session to the browser.
//
// Argus is the RDP client here, not a proxy: the browser speaks the small
// display protocol in internal/rdp/display.go, and everything RDP happens on
// this side. That is what keeps the browser to a few hundred bytes of script
// instead of a codec, and it is the only arrangement in which the gateway can
// record what it is showing.
func (s *Server) handleRDPWeb(cfg WebConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		principal := r.URL.Query().Get("principal")
		if target == "" || principal == "" {
			http.Error(w, "target and principal are required", http.StatusBadRequest)
			return
		}

		user, err := cfg.authorize(r, target, principal)
		if err != nil {
			s.log.Warn("rdp terminal refused",
				"target", target, "principal", principal, "error", err)
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}

		asset, err := s.cfg.Inventory.Resolve(target)
		if err != nil {
			http.Error(w, "unknown target", http.StatusNotFound)
			return
		}
		if asset.Proto() != ProtocolRDP {
			http.Error(w, "this asset is not reached over RDP", http.StatusBadRequest)
			return
		}
		if !asset.AllowsPrincipal(principal) {
			// The inventory is the authority, not the target's account database.
			s.log.Warn("rdp principal refused",
				"principal", principal, "target", asset.Hostname, "user", user)
			http.Error(w, "principal not permitted on this host", http.StatusForbidden)
			return
		}

		conn, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: cfg.AllowedOrigins,
		})
		if wsErr != nil {
			s.log.Warn("rdp websocket upgrade failed", "error", wsErr)
			return
		}
		defer conn.CloseNow()
		// Input messages are small; the limit exists so a client cannot make the
		// gateway buffer on its say-so.
		conn.SetReadLimit(64 << 10)

		s.runRDPWeb(r.Context(), conn, user, principal, asset)
	}
}

func (s *Server) runRDPWeb(ctx context.Context, conn *websocket.Conn,
	user, principal string, asset Asset) {

	sess := &rdp.Session{
		ID:        newRDPSessionID(),
		User:      user,
		Principal: principal,
		Target:    asset.Hostname,
		Address:   asset.Addr(),
		StartedAt: time.Now().UTC(),
	}
	log := s.log.With("session", sess.ID, "user", user,
		"principal", principal, "target", asset.Hostname)

	client, protocol, err := s.dialRDP(asset, principal, log)
	if err != nil {
		log.Error("rdp target unreachable", "error", err)
		writeControl(ctx, conn, rdp.FrameClosed, 0, 0)
		return
	}
	defer client.Close()
	sess.Protocol = protocol

	// Recording is opened before a single frame is shown. A session Argus
	// cannot record is one it must not display, or "every privileged session is
	// recorded" stops being true for exactly the sessions opened from a
	// browser.
	if err := s.openRDPWebRecording(sess, asset, client); err != nil {
		log.Error("rdp recording could not be opened, refusing the session", "error", err)
		writeControl(ctx, conn, rdp.FrameClosed, 0, 0)
		return
	}

	s.trackRDPWeb(sess)
	s.reportRDPWeb(sess, "", "active")
	width, height := client.Size()
	log.Info("rdp browser session opened",
		"protocol", rdp.ProtocolName(protocol), "size", [2]int{width, height})

	if err := writeControl(ctx, conn, rdp.FrameReady, width, height); err != nil {
		return
	}

	// Input runs in its own goroutine so a user typing never waits on a screen
	// update, which is the difference between a session that feels attached and
	// one that feels remote.
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		s.pumpRDPInput(ctx, conn, client, log)
	}()

	err = s.pumpRDPFrames(ctx, conn, client)

	head, closeErr := sess.Close()
	s.untrackRDPWeb(sess)
	s.reportRDPWeb(sess, head, "closed")
	_ = client.Close()
	<-inputDone

	if err != nil {
		log.Info("rdp browser session ended", "reason", err)
	}
	if closeErr != nil {
		log.Error("sealing the rdp recording failed", "error", closeErr)
	}
	log.Info("rdp browser session closed", "chain_head", head)
	writeControl(ctx, conn, rdp.FrameClosed, 0, 0)
}

// dialRDP opens and authenticates a connection to the target.
func (s *Server) dialRDP(asset Asset, principal string, log *slog.Logger) (
	*rdp.Client, uint32, error) {

	// CredSSP where the target supports it, so no password reaches a logon
	// screen; otherwise the credential rides in the client info packet.
	protocol := rdp.ProtocolHybrid
	var auth *credssp.Authenticator
	var logon rdp.LogonInfo

	password, cerr := credentialFor(asset, principal)
	switch {
	case cerr == nil:
		auth = &credssp.Authenticator{Credentials: credssp.Credentials{
			Domain: asset.Domain, User: principal, Password: password,
			Workstation: "ARGUS",
		}}
		logon = rdp.LogonInfo{Domain: asset.Domain, User: principal, Password: password}
	case errors.Is(cerr, ErrNoCredential):
		log.Warn("no vaulted credential; the user will meet a logon screen",
			"principal", principal)
		protocol = rdp.ProtocolSSL
	default:
		return nil, 0, cerr
	}

	conn, result, err := rdp.DialTargetWithAuth(asset.Addr(), asset.Hostname,
		protocol, s.cfg.HostKeys, 15*time.Second, auth)
	if err != nil {
		// A target that will not do CredSSP is common; falling back to TLS with
		// the credential in the client info packet keeps it reachable, and the
		// session record says which happened.
		if auth != nil && !errors.Is(err, hostkey.ErrMismatch) && !errors.Is(err, hostkey.ErrUnpinned) {
			log.Warn("credssp failed, retrying over tls", "error", err)
			protocol = rdp.ProtocolSSL
			conn, result, err = rdp.DialTargetWithAuth(asset.Addr(), asset.Hostname,
				protocol, s.cfg.HostKeys, 15*time.Second, nil)
		}
		if err != nil {
			return nil, 0, err
		}
	}
	if result != nil && result.LegacyBinding {
		log.Warn("target used the pre-version-5 credssp binding",
			"detail", "not bound to a nonce; a captured exchange can be replayed")
	}

	client, err := rdp.Connect(conn, rdp.ClientConfig{
		Width: 1280, Height: 800, Depth: 16,
		SelectedProtocol: protocol,
		Logon:            logon,
		Log:              log,
	})
	if err != nil {
		conn.Close()
		return nil, 0, err
	}
	return client, protocol, nil
}

func (s *Server) openRDPWebRecording(sess *rdp.Session, asset Asset, client *rdp.Client) error {
	dir := s.cfg.RecordingDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, sess.ID+rdp.Extension),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	width, height := client.Size()
	rec, err := rdp.NewRecorder(f, rdp.Header{
		Width: width, Height: height,
		Timestamp: sess.StartedAt.Unix(),
		Title:     sess.Principal + "@" + asset.Hostname,
		Protocol:  rdp.ProtocolName(sess.Protocol),
		Env: map[string]string{
			"ARGUS_SESSION": sess.ID,
			"ARGUS_USER":    sess.User,
			"ARGUS_ORIGIN":  "browser",
		},
	})
	if err != nil {
		_ = f.Close()
		return err
	}
	sess.SetRecorder(rec)

	// The raw PDUs, not the decoded rectangles: a later build that understands
	// more of the protocol can then replay this recording in more detail than
	// this one could show.
	client.SetTap(func(pdu []byte) {
		if werr := rec.Write(rdp.ServerOutput, pdu); werr != nil {
			s.log.Error("recording an rdp frame failed",
				"session", sess.ID, "error", werr)
		}
	})
	return nil
}

// pumpRDPFrames reads screen updates and writes them to the browser.
func (s *Server) pumpRDPFrames(ctx context.Context, conn *websocket.Conn, client *rdp.Client) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rects, err := client.Next()
		if err != nil {
			return err
		}
		if len(rects) == 0 {
			continue
		}

		batch := make([]byte, 0, 4096)
		for _, r := range rects {
			frame := rdp.EncodeRect(r)
			// Flushed before it grows large enough to stall the socket; a
			// full-screen repaint otherwise arrives as one message and the
			// whole session appears to freeze rather than one frame lagging.
			if len(batch)+len(frame) > maxBatchBytes && len(batch) > 0 {
				if werr := writeBinary(ctx, conn, batch); werr != nil {
					return werr
				}
				batch = batch[:0]
			}
			batch = append(batch, frame...)
		}
		if len(batch) > 0 {
			if werr := writeBinary(ctx, conn, batch); werr != nil {
				return werr
			}
		}
	}
}

// pumpRDPInput forwards browser input to the target.
func (s *Server) pumpRDPInput(ctx context.Context, conn *websocket.Conn,
	client *rdp.Client, log *slog.Logger) {

	type message struct {
		Type   string `json:"type"`
		Code   string `json:"code"`
		Down   bool   `json:"down"`
		X      int    `json:"x"`
		Y      int    `json:"y"`
		Button int    `json:"button"`
		Delta  int    `json:"delta"`
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}

		e := rdp.InputEvent{
			Kind: m.Type, Down: m.Down, X: m.X, Y: m.Y,
			Button: m.Button, WheelDelta: m.Delta,
		}
		if m.Type == "key" {
			code, extended, ok := rdp.ScancodeFor(m.Code)
			if !ok {
				// An unmapped key is dropped rather than sent as scancode zero,
				// which is a real key and would type something the user did not
				// press.
				continue
			}
			e.Scancode, e.Extended = code, extended
		}
		if err := client.Send(e); err != nil {
			log.Warn("sending input failed", "error", err)
			return
		}
	}
}

func writeBinary(ctx context.Context, conn *websocket.Conn, b []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageBinary, b)
}

func writeControl(ctx context.Context, conn *websocket.Conn, kind uint8, w, h int) error {
	return writeBinary(ctx, conn, rdp.EncodeControl(kind, w, h))
}

/* ── Session tracking ────────────────────────────────────────────────────── */

func (s *Server) trackRDPWeb(sess *rdp.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rdpWeb == nil {
		s.rdpWeb = map[string]*rdp.Session{}
	}
	s.rdpWeb[sess.ID] = sess
}

func (s *Server) untrackRDPWeb(sess *rdp.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rdpWeb, sess.ID)
}

func (s *Server) reportRDPWeb(sess *rdp.Session, chainHead, state string) {
	if s.cfg.Reporter == nil || !s.cfg.Reporter.Enabled() {
		return
	}
	rec := map[string]any{
		"id":            sess.ID,
		"userEmail":     sess.User,
		"assetHostname": sess.Target,
		"principal":     sess.Principal,
		"protocol":      "rdp",
		"origin":        "brokered",
		"state":         state,
		"startedAt":     sess.StartedAt,
		"clientIp":      sess.RemoteIP,
		"fidelity":      "rdp",
		"reportedBy":    "gateway",
		"riskFlags":     []string{},
	}
	if chainHead != "" {
		rec["chainHead"] = chainHead
		rec["endedAt"] = time.Now().UTC()
		if r := sess.Recorder(); r != nil {
			_, bytes, _ := r.Stats()
			rec["recordingBytes"] = bytes
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() {
		defer cancel()
		s.cfg.Reporter.Session(ctx, rec)
	}()
}

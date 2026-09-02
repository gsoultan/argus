package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/recorder"
)

// WebConfig configures the browser-terminal endpoint.
type WebConfig struct {
	// Listen is the HTTP address for the terminal endpoint.
	Listen string
	// Signer verifies terminal tickets issued by the control plane.
	//
	// The gateway deliberately holds no policy of its own for browser access:
	// the control plane decides who may open what, and mints a ticket saying
	// so. The gateway only checks the signature. One place for the rules means
	// no second place for them to drift.
	Signer *auth.Signer

	// Tokens is a development fallback used only when no Signer is configured.
	// Disabled entirely once ticket verification is available, so a leftover
	// dev token cannot become a way in.
	Tokens map[string]string
	// AllowedOrigins are the browser origins permitted to open a terminal.
	//
	// Empty means same-origin only. This is a real control, not boilerplate: a
	// WebSocket is not subject to the same-origin policy the way XHR is, so
	// without an origin check any page the user visits could open a privileged
	// session using their cookies.
	AllowedOrigins []string
}

// clientMessage is what the browser terminal sends.
//
// Keystrokes and control events share one channel, so a resize can never be
// mistaken for input the user typed — which would otherwise end up in the
// recording as commands nobody ran.
type clientMessage struct {
	Type string `json:"type"` // "input" | "resize"
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// serverMessage is what the gateway sends back.
type serverMessage struct {
	Type string `json:"type"` // "output" | "ready" | "error" | "closed"
	Data string `json:"data,omitempty"`
	// Session and ChainHead let the console link a live terminal to its
	// recording without a second round trip.
	Session   string `json:"session,omitempty"`
	ChainHead string `json:"chain_head,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
}

// ServeWeb runs the browser-terminal HTTP endpoint until ctx is cancelled.
func (s *Server) ServeWeb(ctx context.Context, cfg WebConfig) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/v1/assets", s.handleAssets(cfg))
	mux.HandleFunc("/ws/session", s.handleWebSession(cfg))

	srv := &http.Server{
		Addr:              querySafe(cfg.Listen),
		Handler:           withCORS(mux, cfg.AllowedOrigins),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	s.log.Info("browser terminal listening", "addr", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func querySafe(addr string) string {
	if addr == "" {
		return "127.0.0.1:8081"
	}
	return addr
}

// authorize validates the request to open a terminal.
//
// A browser cannot set headers on a WebSocket handshake, so the credential has
// to travel in the query string. That is acceptable only because a ticket is
// single-use and expires in about a minute: leaking it into a proxy log buys an
// attacker nothing. A session cookie in the same position would be a serious
// leak, which is precisely why tickets exist.
func (cfg WebConfig) authorize(r *http.Request, target, principal string) (string, error) {
	ctx := r.Context()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.URL.Query().Get("ticket")
	}
	if token == "" {
		token = r.URL.Query().Get("token") // legacy dev path
	}
	if token == "" {
		return "", fmt.Errorf("no ticket supplied")
	}

	if cfg.Signer != nil {
		// Verify and burn in one step. Checking first and redeeming later
		// would let two concurrent connections both pass.
		t, err := cfg.Signer.RedeemTicket(ctx, token, target, principal)
		if err != nil {
			return "", err
		}
		return t.Email, nil
	}

	user, ok := cfg.Tokens[token]
	if !ok {
		return "", fmt.Errorf("unknown token")
	}
	return user, nil
}

func (s *Server) handleAssets(cfg WebConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The inventory lives in the control plane; the gateway exposes it only
		// as a convenience and does not authorise reads of it.
		if cfg.Signer == nil {
			if _, ok := cfg.Tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		type assetView struct {
			Hostname   string   `json:"hostname"`
			Address    string   `json:"address"`
			Principals []string `json:"principals"`
		}
		var out []assetView
		for _, a := range s.cfg.Inventory.Unique() {
			out = append(out, assetView{
				Hostname:   a.Hostname,
				Address:    a.Address,
				Principals: a.Principals,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// handleWebSession upgrades to a WebSocket and proxies a live shell.
func (s *Server) handleWebSession(cfg WebConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		principal := r.URL.Query().Get("principal")
		if target == "" || principal == "" {
			http.Error(w, "target and principal are required", http.StatusBadRequest)
			return
		}

		// Authorisation is bound to this exact target and principal, so a
		// ticket for a staging host cannot be replayed against production.
		user, err := cfg.authorize(r, target, principal)
		if err != nil {
			s.log.Warn("terminal request refused",
				"target", target, "principal", principal, "error", err)
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}

		conn, wsErr := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: cfg.AllowedOrigins,
		})
		if wsErr != nil {
			s.log.Warn("websocket upgrade failed", "error", wsErr)
			return
		}
		// Terminal output is bursty; the default read limit is too small for a
		// screenful of a large paste.
		conn.SetReadLimit(1 << 20)
		defer conn.CloseNow()

		s.runWebTerminal(r.Context(), conn, user, principal, target, r.RemoteAddr)
	}
}

func (s *Server) runWebTerminal(ctx context.Context, conn *websocket.Conn,
	user, principal, target, remoteAddr string) {

	send := func(m serverMessage) error {
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return conn.Write(ctx, websocket.MessageText, data)
	}

	// Same Dial as the ssh(1) path: same principal check, same host-key pin,
	// same credential injection, same recording.
	sess, err := s.Dial(user, principal, target, remoteAddr)
	if err != nil {
		_ = send(serverMessage{Type: "error", Data: err.Error()})
		_ = conn.Close(websocket.StatusNormalClosure, "refused")
		return
	}

	s.trackSession(sess)
	defer func() {
		s.untrackSession(sess)
		head, cerr := sess.Close()
		if cerr != nil {
			sess.log.Error("closing recording failed", "error", cerr)
		}
		sess.log.Info("recording sealed", "chain_head", head, "transport", "websocket")
		sess.report(head, "closed")
	}()

	targetSess, err := sess.client.NewSession()
	if err != nil {
		_ = send(serverMessage{Type: "error", Data: "cannot open session: " + err.Error()})
		return
	}
	defer targetSess.Close()

	stdin, err := targetSess.StdinPipe()
	if err != nil {
		_ = send(serverMessage{Type: "error", Data: err.Error()})
		return
	}
	stdout, err := targetSess.StdoutPipe()
	if err != nil {
		_ = send(serverMessage{Type: "error", Data: err.Error()})
		return
	}
	stderr, err := targetSess.StderrPipe()
	if err != nil {
		_ = send(serverMessage{Type: "error", Data: err.Error()})
		return
	}

	// xterm.js speaks xterm-256color; anything else and colours and line
	// editing misbehave in ways users report as "the terminal is broken".
	if err := targetSess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}); err != nil {
		_ = send(serverMessage{Type: "error", Data: "pty: " + err.Error()})
		return
	}
	if err := targetSess.Shell(); err != nil {
		_ = send(serverMessage{Type: "error", Data: "shell: " + err.Error()})
		return
	}

	_ = send(serverMessage{Type: "ready", Session: sess.ID})

	var writeMu sync.Mutex
	safeSend := func(m serverMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return send(m)
	}

	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }

	// target -> browser
	pump := func(r interface{ Read([]byte) (int, error) }) {
		defer finish()
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if rerr := sess.record(recorder.Output, chunk); rerr != nil {
					sess.log.Error("recording failed, terminating session", "error", rerr)
					return
				}
				if serr := safeSend(serverMessage{Type: "output", Data: string(chunk)}); serr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go pump(stdout)
	go pump(stderr)

	// browser -> target
	go func() {
		defer finish()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg clientMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "input":
				if err := sess.record(recorder.Input, []byte(msg.Data)); err != nil {
					sess.log.Error("recording failed, terminating session", "error", err)
					return
				}
				if _, err := stdin.Write([]byte(msg.Data)); err != nil {
					return
				}
			case "resize":
				if msg.Cols > 0 && msg.Rows > 0 {
					_ = targetSess.WindowChange(msg.Rows, msg.Cols)
					sess.mu.Lock()
					if sess.rec != nil {
						_ = sess.rec.Resize(msg.Cols, msg.Rows)
					}
					sess.mu.Unlock()
				}
			}
		}
	}()

	<-done

	// Closing the WebSocket does not kill the remote shell — the browser going
	// away is invisible to sshd. Without closing the channel here, Wait blocks
	// forever, the recording is never sealed, and the shell stays alive on the
	// target. Two leaks from one missing call.
	_ = targetSess.Close()

	exitCode := 0
	waited := make(chan error, 1)
	go func() { waited <- targetSess.Wait() }()

	select {
	case err := <-waited:
		if err != nil {
			var ee *ssh.ExitError
			if asExitError(err, &ee) {
				exitCode = ee.ExitStatus()
			} else {
				exitCode = 255
			}
		}
	case <-time.After(5 * time.Second):
		// A shell ignoring the channel close must not hold the recording open.
		// Seal what we have; an unsealed recording is worse than one with an
		// unknown exit status.
		sess.log.Warn("target session did not exit after close, sealing anyway")
		exitCode = 255
	}
	_ = safeSend(serverMessage{
		Type:      "closed",
		ExitCode:  exitCode,
		Session:   sess.ID,
		ChainHead: sess.rec.Head(),
	})
	_ = conn.Close(websocket.StatusNormalClosure, fmt.Sprintf("exit %d", exitCode))
}

// withCORS allows the console's origin to call the JSON endpoints.
//
// WebSocket origin checking is handled separately by websocket.Accept; this
// only covers the plain HTTP routes.
func withCORS(next http.Handler, allowed []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, a := range allowed {
			if a == origin && origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Vary", "Origin")
				break
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

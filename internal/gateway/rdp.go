package gateway

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/credssp"
	"github.com/gsoultan/argus/internal/hostkey"
	"github.com/gsoultan/argus/internal/rdp"
)

// RDPConfig configures the Remote Desktop listener.
type RDPConfig struct {
	// Listen is the address to bind. Empty disables RDP entirely, which is the
	// default: a port that speaks a privileged protocol should be opened
	// deliberately rather than by upgrading.
	Listen string
	// TLS is the certificate Argus presents to RDP clients.
	//
	// Required. Argus terminates TLS to record the session, so it needs an
	// identity of its own — and a client that cannot verify the gateway is
	// exactly the situation a privileged access product exists to remove.
	TLS *tls.Config
	// RecordingDir is where session recordings land.
	RecordingDir string
	// DialTimeout bounds the connection to a target.
	DialTimeout time.Duration
}

// RDPServer brokers Remote Desktop sessions.
//
// Deliberately a separate listener sharing the SSH gateway's Server rather than
// a second service. The inventory, the host pins, the reporter and the audit
// trail are the same for both protocols; running them apart would mean two
// places to add a host and two answers to "who reached this machine".
type RDPServer struct {
	srv *Server
	cfg RDPConfig
	log *slog.Logger

	mu       sync.Mutex
	listener net.Listener
	sessions map[string]*rdp.Session
	closing  bool
	wg       sync.WaitGroup
}

// NewRDPServer builds the RDP listener from an existing gateway.
func NewRDPServer(srv *Server, cfg RDPConfig) (*RDPServer, error) {
	if cfg.Listen == "" {
		return nil, fmt.Errorf("no listen address")
	}
	if cfg.TLS == nil {
		return nil, fmt.Errorf("rdp requires a TLS certificate: Argus terminates " +
			"TLS to record the session, so it must present an identity of its own")
	}
	if cfg.RecordingDir == "" {
		return nil, fmt.Errorf("no recording directory")
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 15 * time.Second
	}
	return &RDPServer{
		srv:      srv,
		cfg:      cfg,
		log:      srv.log.With("protocol", "rdp"),
		sessions: map[string]*rdp.Session{},
	}, nil
}

// Listen accepts RDP connections until Close.
func (s *RDPServer) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	s.log.Info("rdp gateway listening", "addr", ln.Addr().String())

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return nil
			}
			// One bad accept must not kill the listener; every session in
			// flight would die with it.
			s.log.Error("accept failed", "error", err)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *RDPServer) handleConn(conn net.Conn) {
	defer conn.Close()

	remote := conn.RemoteAddr().String()
	if s.srv.cfg.AuthLimiter != nil {
		host, _, _ := net.SplitHostPort(remote)
		if ok, retry := s.srv.cfg.AuthLimiter.Allow(host); !ok {
			// The RDP port is scanned continuously. Dropping without a reply is
			// correct here: a refusal PDU would be a free oracle telling a
			// scanner it found a live gateway.
			s.log.Warn("rdp connection refused by rate limit",
				"remote", remote, "retry_after", retry)
			return
		}
	}

	// A client that opens a socket and says nothing holds a goroutine and a file
	// descriptor for as long as it likes.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	req, protocol, err := rdp.Handshake(conn, s.log)
	if err != nil {
		s.log.Warn("rdp handshake refused", "remote", remote, "error", err)
		return
	}

	asset, err := s.srv.cfg.Inventory.Resolve(req.Target)
	if err != nil {
		s.log.Warn("rdp target refused", "remote", remote, "target", req.Target, "error", err)
		rdp.Refuse(conn, rdp.FailInconsistentFlags)
		return
	}
	if asset.Proto() != ProtocolRDP {
		// Reaching an SSH host over RDP would fail confusingly at the target.
		s.log.Warn("rdp request for a non-rdp asset",
			"target", asset.Hostname, "protocol", asset.Proto())
		rdp.Refuse(conn, rdp.FailInconsistentFlags)
		return
	}
	if !asset.AllowsPrincipal(req.Principal) {
		// The inventory is the authority, not the target's account database.
		s.log.Warn("rdp principal refused",
			"principal", req.Principal, "target", asset.Hostname)
		rdp.Refuse(conn, rdp.FailInconsistentFlags)
		return
	}

	sess := &rdp.Session{
		ID:        newRDPSessionID(),
		User:      req.Principal + "@" + asset.Hostname,
		Principal: req.Principal,
		Target:    asset.Hostname,
		Address:   asset.Addr(),
		RemoteIP:  remote,
		StartedAt: time.Now().UTC(),
		Protocol:  protocol,
	}
	log := s.log.With("session", sess.ID,
		"principal", sess.Principal, "target", sess.Target)

	// The target is opened before the client is told the negotiation succeeded.
	// A client that receives a confirm starts a TLS handshake immediately, so
	// confirming first would leave the user waiting on a connection that may
	// never come.
	// A credential is looked up only for protocols that can carry one. Failing
	// to find one is not fatal: the session still opens, the user meets the
	// host's own logon screen, and the session record says the credential was
	// not injected — which is a coverage gap an operator can see and close,
	// rather than a connection that mysteriously does not work.
	var auth *credssp.Authenticator
	if protocol == rdp.ProtocolHybrid || protocol == rdp.ProtocolHybridEx {
		password, cerr := credentialFor(asset, req.Principal)
		switch {
		case cerr == nil:
			auth = &credssp.Authenticator{Credentials: credssp.Credentials{
				Domain:      asset.Domain,
				User:        req.Principal,
				Password:    password,
				Workstation: "ARGUS",
			}}
		case errors.Is(cerr, ErrNoCredential):
			log.Warn("no vaulted credential; the user will be asked to authenticate",
				"principal", req.Principal, "target", asset.Hostname)
		default:
			// A credential that exists but cannot be used safely — the wrong
			// file mode, say — must not be silently skipped.
			log.Error("credential unusable", "error", cerr)
			rdp.Refuse(conn, rdp.FailInconsistentFlags)
			return
		}
	}

	target, authResult, err := rdp.DialTargetWithAuth(asset.Addr(), asset.Hostname,
		protocol, s.srv.cfg.HostKeys, s.cfg.DialTimeout, auth)
	if err != nil {
		log.Error("rdp target unreachable or unverified", "error", err)
		// The protocol has no code for "the gateway could not reach the
		// target", so only a genuine certificate problem is reported as one.
		// Sending SSL_CERT_NOT_ON_SERVER for an unreachable host would put a
		// confident, wrong explanation in front of the user; a closed
		// connection at least matches what happened, and the reason is in the
		// gateway log where an operator can act on it.
		if errors.Is(err, hostkey.ErrMismatch) || errors.Is(err, hostkey.ErrUnpinned) {
			rdp.Refuse(conn, rdp.FailSSLCertNotOnServer)
		}
		return
	}
	defer target.Close()

	if err := rdp.ConfirmProtocol(conn, protocol); err != nil {
		log.Error("rdp confirm failed", "error", err)
		return
	}

	// Argus presents its own certificate to the client and its own trust
	// decision to the target, which is what puts the plaintext in reach to be
	// recorded.
	client := tls.Server(conn, s.cfg.TLS)
	if err := client.Handshake(); err != nil {
		log.Warn("rdp client tls handshake failed", "error", err)
		return
	}

	if err := s.openRecording(sess, asset); err != nil {
		// Recording is not optional. A session Argus cannot record is a session
		// it must not broker, or "every privileged session is recorded" stops
		// being true without anyone noticing.
		log.Error("rdp recording could not be opened, refusing the session", "error", err)
		return
	}

	sess.Injected = auth != nil
	if authResult != nil {
		sess.LegacyBinding = authResult.LegacyBinding
	}

	s.track(sess)
	s.report(sess, "", "active")
	log.Info("rdp session opened",
		"protocol", rdp.ProtocolName(protocol),
		"credential_injected", sess.Injected,
		"legacy_binding", sess.LegacyBinding)

	// The handshake deadline must not survive into the session itself.
	_ = conn.SetDeadline(time.Time{})

	relayErr := sess.Relay(client, target, log)

	head, closeErr := sess.Close()
	s.untrack(sess)
	s.report(sess, head, "closed")

	if relayErr != nil {
		log.Error("rdp session ended with an error", "error", relayErr)
	}
	if closeErr != nil {
		log.Error("sealing the rdp recording failed", "error", closeErr)
	}
	log.Info("rdp session closed", "chain_head", head)
}

// openRecording creates the session's recording file.
func (s *RDPServer) openRecording(sess *rdp.Session, asset Asset) error {
	if err := os.MkdirAll(s.cfg.RecordingDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.cfg.RecordingDir, sess.ID+rdp.Extension)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	rec, err := rdp.NewRecorder(f, rdp.Header{
		Timestamp: sess.StartedAt.Unix(),
		Title:     sess.Principal + "@" + asset.Hostname,
		Protocol:  rdp.ProtocolName(sess.Protocol),
		Env: map[string]string{
			"ARGUS_SESSION": sess.ID,
			"ARGUS_USER":    sess.User,
			"ARGUS_CLIENT":  sess.RemoteIP,
		},
	})
	if err != nil {
		_ = f.Close()
		return err
	}
	sess.SetRecorder(rec)
	return nil
}

// report publishes the session to the control plane.
func (s *RDPServer) report(sess *rdp.Session, chainHead, state string) {
	if s.srv.cfg.Reporter == nil || !s.srv.cfg.Reporter.Enabled() {
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
		// RDP recordings are the protocol stream, not a terminal capture, and
		// not kernel-observed execution either. Naming it honestly keeps the
		// console from implying a fidelity this session does not have.
		"fidelity":   "rdp",
		"reportedBy": "gateway",
		"riskFlags":  s.riskFlags(sess),
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
		s.srv.cfg.Reporter.Session(ctx, rec)
	}()
}

func (s *RDPServer) riskFlags(sess *rdp.Session) []string {
	flags := []string{}
	// Administrator is to Windows what root is to Linux.
	switch sess.Principal {
	case "Administrator", "administrator", "admin":
		flags = append(flags, "root-principal")
	}
	if pin, ok := s.srv.cfg.HostKeys.Lookup(sess.Target); !ok || pin.Fingerprint == "" {
		flags = append(flags, "unpinned-host-key")
	}
	if h := sess.StartedAt.Hour(); h < 7 || h > 20 {
		flags = append(flags, "off-hours")
	}
	// A session where the user supplied their own password is one where a
	// standing credential still exists on the target.
	if !sess.Injected {
		flags = append(flags, "no-credential-injection")
	}
	// The pre-version-5 binding is not nonce-bound, so a captured exchange can
	// be replayed against another channel. Worth telling an auditor apart.
	if sess.LegacyBinding {
		flags = append(flags, "legacy-credssp-binding")
	}
	// TLS without CredSSP means the user meets a Windows logon screen through
	// the tunnel, unauthenticated until they type something.
	if sess.Protocol == rdp.ProtocolSSL {
		flags = append(flags, "no-network-level-auth")
	}
	return flags
}

func (s *RDPServer) track(sess *rdp.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
}

func (s *RDPServer) untrack(sess *rdp.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sess.ID)
}

// ActiveSessions returns a snapshot.
func (s *RDPServer) ActiveSessions() []*rdp.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*rdp.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

// Close stops accepting and waits for sessions in flight.
func (s *RDPServer) Close() error {
	s.mu.Lock()
	s.closing = true
	ln := s.listener
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
	return nil
}

// newRDPSessionID matches the SSH side's identifier shape, so one column in the
// control plane holds both and a session id looks the same wherever it appears.
func newRDPSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A predictable session id would make recordings guessable by anyone
		// who can list them.
		panic("rdp: no entropy for a session id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

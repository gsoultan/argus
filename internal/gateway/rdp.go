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
	"github.com/gsoultan/argus/internal/storage"
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
	// targets holds each session's connection to the host, so an administrator
	// can end a session that Argus is only relaying.
	targets map[string]net.Conn
	// clients holds the operator's own connection. Closing only the target
	// leaves them attached to a gateway with a dead desktop and leaves
	// handleConn blocked on a socket nobody is going to close -- the same
	// asymmetry the SSH side had, with the same consequence at shutdown.
	clients map[string]net.Conn
	closing bool
	wg      sync.WaitGroup
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
	rs := &RDPServer{
		srv:      srv,
		cfg:      cfg,
		log:      srv.log.With("protocol", "rdp"),
		sessions: map[string]*rdp.Session{},
	}
	srv.mu.Lock()
	srv.rdpProxy = rs
	srv.mu.Unlock()
	return rs, nil
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

	// Being listed is permission to ask, not permission to have -- the same
	// rule the SSH path and the browser path both enforce.
	//
	// This path cannot enforce it the same way, because it has no idea who is
	// asking. The request is an mstshash cookie: an unauthenticated string
	// carrying a principal and a target and no person at all. `sess.User` is
	// synthesised from the principal, and the CredSSP identity is the *target*
	// account Argus injects from the vault, not the operator. So there is
	// nobody to hold an approval and nobody to name in the audit chain, and
	// asking the control plane about "administrator@win-01" would look up a
	// grant for an account that does not exist and dress a denial up as an
	// authorisation.
	//
	// Refusing is the only honest answer available here. Until this path
	// authenticates a person, an elevated Windows account is not reachable
	// through it -- which is what the README already claims of every elevated
	// session, and what the risk flags already assume by treating
	// administrator as root's equivalent.
	if s.srv.policy().IsElevated(req.Principal) {
		s.log.Warn("rdp elevated principal refused: this path cannot identify the requester",
			"principal", req.Principal, "target", asset.Hostname, "remote", remote,
			"detail", "an elevated account needs an approved access request, and the "+
				"mstshash cookie names no person to hold one; use the browser "+
				"console, which authenticates before it brokers")
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
		// A proxied session's desktop size is negotiated between the real
		// client and the target, and Argus does not parse the capability sets
		// that carry it. A refresh request larger than the screen is clipped by
		// the server, so asking generously is correct rather than a guess that
		// could be wrong.
		Width:  4096,
		Height: 4096,
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
	s.setTarget(sess.ID, target)
	defer s.clearTarget(sess.ID)

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
	s.setClient(sess.ID, conn)
	defer s.clearClient(sess.ID)
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
	// A desktop someone stopped is not one that finished. The console has a
	// distinct state for it; reporting both as "closed" made an administrative
	// terminate indistinguishable from a logout.
	state := "closed"
	if _, _, killed := sess.Killed(); killed {
		state = "terminated"
	}
	s.report(sess, head, state)

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
	// Who ended it and why, when someone did. Never sent before, so an
	// administrator terminating a desktop left no record of having done it --
	// the row read the same as a user who closed their own session.
	if by, reason, killed := sess.Killed(); killed {
		rec["terminatedBy"] = by
		rec["terminationReason"] = reason
	}
	if chainHead != "" {
		rec["chainHead"] = chainHead
		rec["endedAt"] = time.Now().UTC()
		if key := s.uploadRDPRecording(sess, chainHead); key != "" {
			rec["recordingPath"] = key
		}
		if r := sess.Recorder(); r != nil {
			_, bytes, _ := r.Stats()
			rec["recordingBytes"] = bytes
		}
	}

	// On the server's books, so shutdown waits for it. Detached and untracked,
	// the last thing a session said about itself was lost to the exit.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	s.srv.reports.Add(1)
	go func() {
		defer s.srv.reports.Done()
		defer cancel()
		s.srv.cfg.Reporter.Session(ctx, rec)
	}()
}

func (s *RDPServer) riskFlags(sess *rdp.Session) []string {
	return s.srv.rdpRiskFlags(sess)
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
func (s *RDPServer) Close() error { return s.CloseWithin(DefaultDrain) }

// CloseWithin stops accepting, waits up to d for desktops to close on their
// own, and then ends the rest deliberately.
//
// Unbounded before, exactly as the SSH listener was, and worse in one respect:
// main closes this one first, so a single open desktop held the whole shutdown
// -- including the SSH drain that seals SSH recordings -- until systemd lost
// patience and sent SIGKILL.
func (s *RDPServer) CloseWithin(d time.Duration) error {
	s.mu.Lock()
	s.closing = true
	ln := s.listener
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}

	if !waitGroup(&s.wg, d) {
		live := s.ActiveSessions()
		s.log.Warn("rdp sessions still open at the end of the drain window; "+
			"ending them so their recordings are sealed",
			"sessions", len(live), "drain", d)
		for _, sess := range live {
			sess.Terminate("argus", "the gateway is shutting down")
			// Terminate only marks it; the connections are what actually end
			// it, and both have to go.
			s.disconnect(sess.ID)
		}
		if !waitGroup(&s.wg, sealGrace) {
			s.log.Error("rdp sessions did not finish sealing within the grace period",
				"remaining", len(s.ActiveSessions()), "grace", sealGrace,
				"detail", "their recordings may have no chain head and cannot be verified")
		}
	}
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

// uploadRDPRecording moves a sealed recording off the gateway.
//
// Without this the evidence lives only where it was produced, which means
// whoever compromises the gateway can delete the record of having done so. It
// is also what lets the control plane serve a replay at all.
func (s *RDPServer) uploadRDPRecording(sess *rdp.Session, chainHead string) string {
	if chainHead == "" || s.srv.cfg.Storage == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key, err := s.srv.cfg.Storage.Upload(ctx,
		storage.LocalPathExt(s.cfg.RecordingDir, sess.ID, rdp.Extension),
		sess.ID, chainHead, sess.StartedAt)
	if err != nil {
		if errors.Is(err, storage.ErrNotConfigured) {
			return ""
		}
		// Queued like a terminal recording. Without this an RDP artefact that
		// missed one upload stayed on the host forever: the retry pass only
		// ever carried .cast files, so a desktop session was stranded by the
		// same outage a shell session recovered from.
		s.log.Error("rdp recording upload failed, artefact remains local only",
			"session", sess.ID, "error", err,
			"detail", "queued for retry; until it lands, this evidence is "+
				"stored only on the host that produced it")
		s.srv.queueUpload(pendingUpload{
			SessionID: sess.ID, ChainHead: chainHead, Ext: rdp.Extension,
			StartedAt: sess.StartedAt, FailedAt: time.Now().UTC(),
		})
		return ""
	}
	s.log.Info("rdp recording uploaded", "session", sess.ID, "key", key)
	return key
}

// session looks up one proxied session.
func (s *RDPServer) session(id string) (*rdp.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// refresh asks a proxied session's target to redraw.
func (s *RDPServer) refresh(id string) error {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	target := s.targets[id]
	s.mu.Unlock()
	if !ok || target == nil {
		return fmt.Errorf("no such proxied session")
	}
	return sess.RefreshFor(target)
}

// disconnect closes a proxied session's connection to the target, which ends
// the relay and seals the recording.
func (s *RDPServer) disconnect(id string) {
	s.mu.Lock()
	target, client := s.targets[id], s.clients[id]
	s.mu.Unlock()
	// Both, because they are two different connections. Closing the target
	// ends the desktop; the operator's own socket stays open until it is
	// closed too, and until then handleConn cannot return and the recording
	// cannot be sealed.
	if target != nil {
		_ = target.Close()
	}
	if client != nil {
		_ = client.Close()
	}
}

func (s *RDPServer) setClient(id string, conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients == nil {
		s.clients = map[string]net.Conn{}
	}
	s.clients[id] = conn
}

func (s *RDPServer) clearClient(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, id)
}

func (s *RDPServer) setTarget(id string, conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.targets == nil {
		s.targets = map[string]net.Conn{}
	}
	s.targets[id] = conn
}

func (s *RDPServer) clearTarget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.targets, id)
}

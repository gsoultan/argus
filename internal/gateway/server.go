package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gsoultan/argus/internal/hostkey"
	"github.com/gsoultan/argus/internal/reporter"
	"github.com/gsoultan/argus/internal/sshca"
	"github.com/gsoultan/argus/internal/storage"
)

// Config is everything the gateway needs to run.
type Config struct {
	// Listen is the address the gateway's SSH server binds.
	Listen string
	// HostKeyPath is the gateway's own host key — what clients verify.
	HostKeyPath string
	// AuthorizedKeysPath lists the users allowed to reach the gateway.
	AuthorizedKeysPath string
	// RecordingDir is where session recordings land. Object storage in
	// production; the filesystem here.
	RecordingDir string

	Inventory *Inventory
	HostKeys  *hostkey.Store
	Log       *slog.Logger

	// CA mints short-lived certificates for assets on certificate auth. Nil
	// means every asset falls back to injected keys.
	CA *sshca.CA

	// Reporter publishes sessions to the control plane. Optional: with none,
	// the gateway still brokers and records, it is simply not visible in the
	// console until one is configured.
	Reporter *reporter.Client

	// Storage moves sealed recordings off this host. Optional, but without it
	// the evidence lives only where it was produced — which means whoever
	// compromises the gateway can delete the record of having done so.
	Storage *storage.Client
}

// Server is the Argus SSH gateway.
type Server struct {
	cfg      Config
	sshCfg   *ssh.ServerConfig
	log      *slog.Logger
	listener net.Listener

	mu       sync.Mutex
	sessions map[string]*Session
	wg       sync.WaitGroup
	closing  bool
}

// NewServer builds a gateway from cfg.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Inventory == nil {
		return nil, errors.New("inventory is required")
	}
	if cfg.HostKeys == nil {
		return nil, errors.New("host key store is required")
	}

	signer, err := loadHostKey(cfg.HostKeyPath)
	if err != nil {
		return nil, err
	}
	authorized, err := loadAuthorizedKeys(cfg.AuthorizedKeysPath)
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, log: cfg.Log, sessions: map[string]*Session{}}

	s.sshCfg = &ssh.ServerConfig{
		// Public key only. Passwords on a bastion are a credential to phish,
		// and the whole point of Argus is that credentials stop being the
		// user's problem.
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			fp := ssh.FingerprintSHA256(key)
			user, ok := authorized[fp]
			if !ok {
				s.log.Warn("rejected unknown key",
					"fingerprint", fp,
					"username", c.User(),
					"remote", c.RemoteAddr().String())
				return nil, fmt.Errorf("unknown key")
			}
			// The username carries the target, so it is parsed here rather than
			// trusted later. Failing at auth time gives the user an immediate
			// error instead of a confusing dropped session.
			req, err := ParseUsername(c.User())
			if err != nil {
				return nil, err
			}
			return &ssh.Permissions{
				Extensions: map[string]string{
					"argus-user":      user,
					"argus-principal": req.Principal,
					"argus-target":    req.Target,
					"argus-key":       fp,
				},
			}, nil
		},

		ServerVersion: "SSH-2.0-Argus",
	}
	s.sshCfg.AddHostKey(signer)

	return s, nil
}

// Listen starts accepting connections and blocks until Close.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	s.listener = ln
	s.log.Info("gateway listening",
		"addr", ln.Addr().String(),
		"assets", s.cfg.Inventory.Count())

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return nil
			}
			// A single bad accept must not kill the gateway; every other
			// session in flight would die with it.
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

// Close stops accepting and waits for in-flight sessions to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()

	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.wg.Wait()
	return nil
}

// ActiveSessions returns a snapshot for the control plane.
func (s *Server) ActiveSessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

func (s *Server) handleConn(nConn net.Conn) {
	defer nConn.Close()

	// A handshake that never completes must not hold a slot forever.
	_ = nConn.SetDeadline(time.Now().Add(30 * time.Second))

	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, s.sshCfg)
	if err != nil {
		s.log.Debug("handshake failed", "remote", nConn.RemoteAddr().String(), "error", err)
		return
	}
	defer sshConn.Close()

	// Handshake done; the session itself may legitimately idle for hours.
	_ = nConn.SetDeadline(time.Time{})

	ext := sshConn.Permissions.Extensions
	sess, err := s.newSession(sshConn, ext)
	if err != nil {
		s.log.Warn("session refused",
			"user", ext["argus-user"],
			"target", ext["argus-target"],
			"error", err)
		// Tell the user why. A silent disconnect sends them to the wrong
		// place — usually their own key setup — when the real reason is
		// policy or a host key mismatch.
		s.rejectAll(chans, err.Error())
		return
	}

	s.trackSession(sess)
	defer func() {
		s.untrackSession(sess)
		// Finalise the recording and publish the chain head. Without this the
		// artefact is never closed and there is nothing to verify it against.
		head, err := sess.Close()
		if err != nil {
			sess.log.Error("closing recording failed", "error", err)
			return
		}
		sess.log.Info("recording sealed",
			"chain_head", head,
			"file", sess.ID+".cast",
			"duration", time.Since(sess.StartedAt).String())
		sess.report(head, "closed")
	}()

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			go sess.handleSessionChannel(newChan)
		default:
			// direct-tcpip is port forwarding. Denied by default: an
			// unrestricted bastion is an open tunnel into the private network,
			// which is precisely what it exists to prevent.
			s.log.Info("channel refused",
				"type", newChan.ChannelType(),
				"user", sess.User,
				"target", sess.Target.Hostname)
			_ = newChan.Reject(ssh.Prohibited,
				fmt.Sprintf("%s is not permitted by policy", newChan.ChannelType()))
		}
	}
}

// rejectAll tells the user why their session was refused.
//
// A channel rejection carries a reason string, but OpenSSH does not display it
// for session channels — the user just gets exit 255 and no explanation, and
// then goes debugging their own key setup instead of reading the policy that
// actually stopped them. So the channel is accepted, the reason written to
// stderr where ssh(1) does show it, and a non-zero status returned.
func (s *Server) rejectAll(chans <-chan ssh.NewChannel, reason string) {
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.Prohibited, reason)
			continue
		}
		ch, reqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		// Requests must be drained or the client blocks waiting on its
		// pty-req/exec reply instead of seeing the message.
		go func() {
			for req := range reqs {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}()
		fmt.Fprintf(ch.Stderr(), "argus: %s\r\n", reason)
		sendExitStatus(ch, 1)
		_ = ch.Close()
	}
}

func (s *Server) trackSession(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
}

func (s *Server) untrackSession(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sess.ID)
}

// loadHostKey reads the gateway's own identity.
func loadHostKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gateway host key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse gateway host key: %w", err)
	}
	return signer, nil
}

// loadAuthorizedKeys maps fingerprint -> user identity.
func loadAuthorizedKeys(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authorized keys %s: %w", path, err)
	}

	keys := map[string]string{}
	for len(data) > 0 {
		pub, comment, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			break
		}
		// The comment is the user identity. Every audit event is attributed to
		// it, so an unlabelled key is refused rather than logged as "unknown"
		// — ISO 27001 wants every admin action traceable to an individual.
		if comment == "" {
			return nil, fmt.Errorf("authorized key %s has no comment; it must identify a user",
				ssh.FingerprintSHA256(pub))
		}
		keys[ssh.FingerprintSHA256(pub)] = comment
		data = rest
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no usable keys in %s", path)
	}
	return keys, nil
}

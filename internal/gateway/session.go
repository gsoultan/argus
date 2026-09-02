package gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/sftp"
	"github.com/gsoultan/argus/internal/sshca"
	"github.com/gsoultan/argus/internal/storage"
)

// Session is one brokered connection: client -> gateway -> target.
//
// The two SSH connections are entirely separate. That separation is what makes
// recording possible without an agent, and it is also why the gateway holds
// session plaintext in memory — the deliberate cost of working against hosts
// where nothing is installed.
type Session struct {
	ID        string
	User      string
	Principal string
	Target    Asset
	RemoteIP  string
	StartedAt time.Time
	// CertSerial is non-zero when the session authenticated with a minted
	// certificate, so a recording can be tied back to the exact credential.
	CertSerial uint64

	srv    *Server
	client *ssh.Client
	log    *slog.Logger

	mu sync.Mutex
	// sftpMon decodes the SFTP subsystem when one is requested, so a transfer
	// becomes "downloaded /path, 4.2 GB" instead of a stream of opaque bytes.
	sftpMon  *sftp.Monitor
	rec      *recorder.Recorder
	recFile  *os.File
	chainEnd string
	closed   bool
}

// newSession authorises an SSH-transport request and dials the target.
func (s *Server) newSession(conn ssh.Conn, ext map[string]string) (*Session, error) {
	return s.Dial(ext["argus-user"], ext["argus-principal"], ext["argus-target"],
		conn.RemoteAddr().String())
}

// Dial authorises a connection request and opens the session to the target.
//
// Transport-agnostic on purpose: the SSH listener and the browser terminal both
// come through here, so a session opened from the web console is subject to the
// same principal check, the same host-key pin and the same recording as one
// opened with ssh(1). A second code path would be a second place for a control
// to be missing.
//
// Everything that can be refused is refused here, before any channel exists, so
// a rejected session never reaches the point of producing output.
func (s *Server) Dial(user, principal, targetName, remoteAddr string) (*Session, error) {

	asset, err := s.cfg.Inventory.Resolve(targetName)
	if err != nil {
		return nil, err
	}

	// The inventory is the authority on who may be assumed, not the target's
	// own account list. A credential that happens to work is not authorisation.
	if !asset.AllowsPrincipal(principal) {
		return nil, fmt.Errorf("principal %q is not permitted on %s", principal, asset.Hostname)
	}

	sess := &Session{
		ID:        newSessionID(),
		User:      user,
		Principal: principal,
		Target:    asset,
		RemoteIP:  remoteAddr,
		StartedAt: time.Now().UTC(),
		srv:       s,
	}
	sess.log = s.log.With("session", sess.ID, "user", user,
		"target", asset.Hostname, "principal", principal)

	// How Argus authenticates to the target.
	//
	// Certificate mode is the goal state: a keypair generated for this session
	// alone, signed for this principal alone, valid for minutes. Nothing is
	// left on the host and there is no secret anywhere to steal afterwards.
	// Injected keys remain for hosts not yet migrated.
	var signer ssh.Signer
	if asset.UsesCertificate() {
		if s.cfg.CA == nil {
			return nil, fmt.Errorf("%s is configured for certificate auth but the gateway has no CA", asset.Hostname)
		}
		certSigner, cert, err := s.cfg.CA.Mint(sshca.Identity{
			SessionID: sess.ID,
			User:      user,
			Principal: principal,
			Target:    asset.Hostname,
		})
		if err != nil {
			return nil, fmt.Errorf("mint certificate for %s: %w", asset.Hostname, err)
		}
		signer = certSigner
		sess.CertSerial = cert.Serial
		sess.log = sess.log.With("cert_serial", cert.Serial)
		sess.log.Info("minted session certificate",
			"key_id", cert.KeyId,
			"valid_for", time.Until(time.Unix(int64(cert.ValidBefore), 0)).Round(time.Second).String())
	} else {
		var err error
		signer, err = loadInjectedKey(asset.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("credential for %s: %w", asset.Hostname, err)
		}
	}

	// Dial the target, verifying its host key against the pin. This callback is
	// the reason the gateway is not itself a man-in-the-middle opportunity.
	clientCfg := &ssh.ClientConfig{
		User:            principal,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: s.cfg.HostKeys.Callback(asset.Hostname),
		Timeout:         15 * time.Second,
	}

	client, err := ssh.Dial("tcp", asset.Addr(), clientCfg)
	if err != nil {
		// Surfaced verbatim to the user: a host key mismatch must be loud and
		// unambiguous, not folded into a generic "connection failed".
		return nil, fmt.Errorf("connect to %s: %w", asset.Hostname, err)
	}
	sess.client = client

	if err := sess.openRecording(s.cfg.RecordingDir); err != nil {
		_ = client.Close()
		// Fail closed. A session that cannot be recorded must not proceed, or
		// "all privileged sessions are recorded" stops being true.
		return nil, fmt.Errorf("start recording: %w", err)
	}

	sess.log.Info("session opened", "remote", sess.RemoteIP, "addr", asset.Addr())
	sess.report("", "active")
	return sess, nil
}

// report publishes the session to the control plane.
//
// Fire and forget with a short timeout: the console being out of date is a
// nuisance, whereas blocking a user's shell on an HTTP call is an outage.
func (s *Session) report(chainHead, state string) {
	if s.srv.cfg.Reporter == nil || !s.srv.cfg.Reporter.Enabled() {
		return
	}

	key := s.uploadRecording(chainHead)

	rec := map[string]any{
		"id":            s.ID,
		"userEmail":     s.User,
		"assetHostname": s.Target.Hostname,
		"principal":     s.Principal,
		"protocol":      "ssh",
		"origin":        "brokered",
		"state":         state,
		"startedAt":     s.StartedAt,
		"clientIp":      s.RemoteIP,
		"fidelity":      "pty",
		"reportedBy":    "gateway",
		"riskFlags":     s.riskFlags(),
	}
	if key != "" {
		rec["recordingPath"] = key
	}
	if chainHead != "" {
		rec["chainHead"] = chainHead
		now := time.Now().UTC()
		rec["endedAt"] = now
		if s.rec != nil {
			_, bytes, _ := s.rec.Stats()
			rec["recordingBytes"] = bytes
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() {
		defer cancel()
		s.srv.cfg.Reporter.Session(ctx, rec)
	}()
}

// uploadRecording pushes the sealed artefact to object storage.
//
// Runs only once the recording is closed, so the uploaded bytes are exactly
// what the chain head covers. Failure is logged, not fatal: the local copy is
// still the record, and the spool will not help here because the file, not the
// report, is what needs moving.
func (s *Session) uploadRecording(chainHead string) string {
	if chainHead == "" || s.srv.cfg.Storage == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key, err := s.srv.cfg.Storage.Upload(ctx,
		storage.LocalPath(s.srv.cfg.RecordingDir, s.ID), s.ID, chainHead, s.StartedAt)
	if err != nil {
		s.log.Error("recording upload failed, artefact remains local only",
			"error", err, "detail", "evidence is stored only on the host that produced it")
		return ""
	}
	s.log.Info("recording uploaded", "key", key)
	return key
}

// riskFlags derives the signals the console highlights.
func (s *Session) riskFlags() []string {
	flags := []string{}
	if s.Principal == "root" {
		flags = append(flags, "root-principal")
	}
	// An unpinned target means nobody verified the host this session reached.
	if pin, ok := s.srv.cfg.HostKeys.Lookup(s.Target.Hostname); !ok || pin.Fingerprint == "" {
		flags = append(flags, "unpinned-host-key")
	}
	if h := s.StartedAt.Hour(); h < 7 || h > 20 {
		flags = append(flags, "off-hours")
	}
	return flags
}

func (s *Session) openRecording(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, s.ID+".cast")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	rec, err := recorder.New(f, recorder.Header{
		Width:     80,
		Height:    24,
		Timestamp: s.StartedAt.Unix(),
		Title:     fmt.Sprintf("%s@%s", s.Principal, s.Target.Hostname),
		Env: map[string]string{
			"ARGUS_SESSION": s.ID,
			"ARGUS_USER":    s.User,
		},
	})
	if err != nil {
		_ = f.Close()
		return err
	}

	s.recFile = f
	s.rec = rec
	return nil
}

// handleSessionChannel proxies one "session" channel end to end.
func (s *Session) handleSessionChannel(newChan ssh.NewChannel) {
	clientCh, clientReqs, err := newChan.Accept()
	if err != nil {
		s.log.Error("accept client channel", "error", err)
		return
	}
	defer clientCh.Close()

	targetSess, err := s.client.NewSession()
	if err != nil {
		s.log.Error("open target session", "error", err)
		fmt.Fprintf(clientCh, "argus: cannot open session on %s: %v\r\n", s.Target.Hostname, err)
		_ = clientCh.CloseWrite()
		return
	}
	defer targetSess.Close()

	targetStdin, err := targetSess.StdinPipe()
	if err != nil {
		s.log.Error("target stdin", "error", err)
		return
	}
	targetStdout, err := targetSess.StdoutPipe()
	if err != nil {
		s.log.Error("target stdout", "error", err)
		return
	}
	targetStderr, err := targetSess.StderrPipe()
	if err != nil {
		s.log.Error("target stderr", "error", err)
		return
	}

	// Requests (pty-req, shell, exec, window-change, signal) are translated
	// rather than blindly forwarded, so policy can inspect each one.
	go s.pumpRequests(clientReqs, targetSess, clientCh)

	var wg sync.WaitGroup
	wg.Add(3)

	// Client -> target. Keystrokes are recorded as "i" events, which is what
	// lets the console reconstruct a command timeline exactly rather than
	// scraping it back out of echoed output.
	go func() {
		defer wg.Done()
		defer targetStdin.Close()
		s.relay(clientCh, targetStdin, recorder.Input)
	}()

	// Target -> client. This is the replay stream.
	go func() {
		defer wg.Done()
		s.relay(targetStdout, clientCh, recorder.Output)
	}()

	go func() {
		defer wg.Done()
		s.relay(targetStderr, clientCh.Stderr(), recorder.Output)
	}()

	wg.Wait()

	// Propagate the real exit status, or scripts calling through the gateway
	// see success for a command that failed.
	exitCode := 0
	if err := targetSess.Wait(); err != nil {
		var ee *ssh.ExitError
		if ok := asExitError(err, &ee); ok {
			exitCode = ee.ExitStatus()
		} else {
			exitCode = 255
		}
	}
	sendExitStatus(clientCh, exitCode)
	s.log.Info("session closed", "exit", exitCode, "duration", time.Since(s.StartedAt).String())
}

// startSFTPMonitor begins decoding an SFTP session.
func (s *Session) startSFTPMonitor() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sftpMon != nil {
		return
	}
	s.sftpMon = sftp.New(func(e sftp.Event) {
		s.log.Info("file transfer",
			"op", string(e.Op), "path", e.Path, "new_path", e.NewPath,
			"bytes", e.Bytes, "failed", e.Failed)
		s.reportFileEvent(e)
	})
	s.log.Info("sftp subsystem requested, decoding file operations")
}

// observeSFTP feeds a copy of the stream to the decoder.
//
// Deliberately after the bytes have already been forwarded and recorded, so
// nothing here can stall or corrupt the session it is observing.
func (s *Session) observeSFTP(stream recorder.Stream, p []byte) {
	s.mu.Lock()
	mon := s.sftpMon
	s.mu.Unlock()
	if mon == nil {
		return
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	if stream == recorder.Input {
		mon.ClientToServer(buf)
	} else {
		mon.ServerToClient(buf)
	}
}

// reportFileEvent publishes a file operation to the control plane.
//
// File exfiltration is the question a PAM buyer asks, so these go to the audit
// chain rather than only the session recording — the audit log is what an
// auditor reads and the one that is tamper-evident.
func (s *Session) reportFileEvent(e sftp.Event) {
	if s.srv.cfg.Reporter == nil || !s.srv.cfg.Reporter.Enabled() {
		return
	}
	action := "file.download"
	severity := "notice"
	switch e.Op {
	case sftp.OpUpload:
		action = "file.upload"
	case sftp.OpDelete, sftp.OpRmdir:
		action = "file.delete"
		// Deleting on a host reached through a bastion is worth a closer look
		// than reading.
		severity = "warning"
	case sftp.OpRename:
		action = "file.rename"
	case sftp.OpChmod:
		action = "file.chmod"
	case sftp.OpMkdir, sftp.OpSymlink, sftp.OpList:
		action = "file." + string(e.Op)
		severity = "info"
	}
	if e.Failed {
		severity = "warning"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() {
		defer cancel()
		s.srv.cfg.Reporter.Audit(ctx, map[string]any{
			"action":     action,
			"severity":   severity,
			"actorEmail": s.User,
			"target":     s.Target.Hostname,
			"detail":     e.Describe(),
		})
	}()
}

// relay copies src to dst while teeing to the recording.
//
// A recording write failure kills the session on purpose: continuing would
// produce an unrecorded privileged session, which is the one outcome the whole
// product exists to prevent.
func (s *Session) relay(src io.Reader, dst io.Writer, stream recorder.Stream) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if err := s.record(stream, chunk); err != nil {
				s.log.Error("recording failed, terminating session", "error", err)
				return
			}
			if _, writeErr := dst.Write(chunk); writeErr != nil {
				return
			}
			s.observeSFTP(stream, chunk)
		}
		if readErr != nil {
			return
		}
	}
}

func (s *Session) record(stream recorder.Stream, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rec == nil {
		return nil
	}
	return s.rec.Write(stream, p)
}

// pumpRequests translates channel requests onto the target session.
func (s *Session) pumpRequests(reqs <-chan *ssh.Request, target *ssh.Session, clientCh ssh.Channel) {
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			term, w, h, modes, err := parsePTYRequest(req.Payload)
			if err != nil {
				s.replyReq(req, false)
				continue
			}
			if err := target.RequestPty(term, h, w, modes); err != nil {
				s.replyReq(req, false)
				continue
			}
			s.mu.Lock()
			if s.rec != nil {
				_ = s.rec.Resize(w, h)
			}
			s.mu.Unlock()
			s.replyReq(req, true)

		case "window-change":
			w, h, err := parseWindowChange(req.Payload)
			if err != nil {
				s.replyReq(req, false)
				continue
			}
			_ = target.WindowChange(h, w)
			s.mu.Lock()
			if s.rec != nil {
				_ = s.rec.Resize(w, h)
			}
			s.mu.Unlock()
			s.replyReq(req, true)

		case "shell":
			if err := target.Shell(); err != nil {
				s.log.Error("start shell", "error", err)
				s.replyReq(req, false)
				continue
			}
			s.replyReq(req, true)

		case "exec":
			cmd, err := parseStringPayload(req.Payload)
			if err != nil {
				s.replyReq(req, false)
				continue
			}
			// Non-interactive commands are audited too. `ssh host cmd` has no
			// PTY, so without this the most scriptable path would be the least
			// visible one.
			s.log.Info("exec", "command", cmd)
			s.mu.Lock()
			if s.rec != nil {
				_ = s.rec.Write(recorder.Input, []byte(cmd+"\r\n"))
			}
			s.mu.Unlock()
			if err := target.Start(cmd); err != nil {
				s.log.Error("start exec", "error", err)
				s.replyReq(req, false)
				continue
			}
			s.replyReq(req, true)

		case "subsystem":
			name, err := parseStringPayload(req.Payload)
			if err != nil {
				s.replyReq(req, false)
				continue
			}
			if name == "sftp" {
				// Decode the subsystem so transfers become per-file audit
				// events. The monitor only ever observes a copy: a fault in it
				// must degrade the audit trail, never the session.
				s.startSFTPMonitor()
			}
			s.log.Info("subsystem", "name", name)
			if err := target.RequestSubsystem(name); err != nil {
				s.replyReq(req, false)
				continue
			}
			s.replyReq(req, true)

		case "signal":
			s.replyReq(req, true)

		case "env":
			// Dropped deliberately: forwarded environment is an easy way to
			// smuggle configuration past policy (LD_PRELOAD being the classic).
			s.replyReq(req, false)

		default:
			s.log.Debug("unhandled request", "type", req.Type)
			s.replyReq(req, false)
		}
	}
	_ = clientCh.CloseWrite()
}

func (s *Session) replyReq(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// Close finalises the recording and returns the chain head.
func (s *Session) Close() (chainHead string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.chainEnd, nil
	}
	s.closed = true

	// Flush any transfer still open: a session dropped mid-download is exactly
	// the one worth recording.
	if s.sftpMon != nil {
		s.sftpMon.Close()
	}
	if s.rec != nil {
		s.chainEnd, err = s.rec.Close()
	}
	if s.recFile != nil {
		_ = s.recFile.Close()
	}
	if s.client != nil {
		_ = s.client.Close()
	}
	return s.chainEnd, err
}

// loadInjectedKey reads the credential the gateway presents to the target.
func loadInjectedKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", path, err)
	}
	return signer, nil
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sendExitStatus tells the client what the remote command returned.
func sendExitStatus(ch ssh.Channel, code int) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(code))
	_, _ = ch.SendRequest("exit-status", false, payload)
}

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
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/secrets"
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

	// userConn is the connection the operator is on, needed to open channels
	// back towards them — forwarded-tcpip, agent and X11 all originate at the
	// target and have to reach the client. Nil for a browser session, which has
	// no SSH connection and therefore no forwarding.
	userConn ssh.Conn
	// remote holds listeners opened on the target by -R, so they close with the
	// session rather than outliving the authorisation that created them.
	remote remoteForwards
	// localForwards counts open -L channels, bounded by MaxLocalForwards.
	localForwards atomic.Int32

	// recordingBroken marks a session whose recorder stopped working, so the
	// two relay directions do not both report it and so the final report can
	// say the artefact is incomplete.
	recordingBroken bool

	// pol is the policy in force for this session, captured once when it opened.
	//
	// Captured rather than read live, and captured *here* rather than at each
	// decision point, because the two were inconsistent: channel opens used a
	// connection-scoped copy while agent and X11 requests re-read the current
	// policy. A session could therefore be told it could open a channel and
	// then refused a related request a minute later, for no reason visible to
	// the person using it.
	pol Policy

	mu sync.Mutex
	// sftpMon decodes the SFTP subsystem when one is requested, so a transfer
	// becomes "downloaded /path, 4.2 GB" instead of a stream of opaque bytes.
	sftpMon  *sftp.Monitor
	rec      *recorder.Recorder
	recFile  *os.File
	chainEnd string
	closed   bool

	// hub fans this session's frames out to shadow viewers. Always present, so
	// any session can be watched without having been opened in a special way —
	// the session an operator most wants to watch is not one that announced
	// itself in advance.
	hub *live.Hub
	// killedBy and killReason record an administrative termination, so the
	// session's final report says who ended it rather than leaving it
	// indistinguishable from a dropped connection.
	killedBy     string
	killReason   string
	terminatedAt time.Time
}

// newSession authorises an SSH-transport request and dials the target.
func (s *Server) newSession(conn ssh.Conn, ext map[string]string) (*Session, error) {
	sess, err := s.Dial(ext["argus-user"], ext["argus-principal"], ext["argus-target"],
		conn.RemoteAddr().String())
	if err != nil {
		return nil, err
	}
	// Only the SSH transport has one. The browser terminal reaches Dial with no
	// connection to forward to, which is why forwarding is unavailable there
	// rather than failing halfway through opening a channel.
	sess.userConn = conn
	sess.watchTargetChannels()
	return sess, nil
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
		hub:       live.NewHub(),
		pol:       s.policy(),
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
		// Fail closed by default: a session that cannot be recorded must not
		// proceed, or "all privileged sessions are recorded" stops being true.
		//
		// Policy can choose otherwise -- some fleets would rather keep access
		// working than cut it -- but that is a decision with a name against it,
		// and the session is then reported as carrying no usable recording
		// rather than as an ordinary one.
		if sess.policy().FailClosedOnRecordingLoss {
			_ = client.Close()
			return nil, fmt.Errorf("start recording: %w", err)
		}
		sess.log.Error("starting the recording failed; policy allows the session to proceed unrecorded",
			"session", sess.ID, "user", user, "target", asset.Hostname, "error", err)
		sess.mu.Lock()
		sess.recordingBroken = true
		sess.mu.Unlock()
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
		// A session whose recorder stopped has no usable replay, and calling
		// that "pty" would put an artefact in the console that cannot show
		// what happened while claiming it can.
		"fidelity":   s.reportedFidelity(),
		"reportedBy": "gateway",
		"riskFlags":  s.riskFlags(),
	}
	if by, reason, killed := s.Killed(); killed {
		rec["terminatedBy"] = by
		rec["terminationReason"] = reason
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
	// A session someone had to cut short is the first thing an auditor should
	// be able to filter for.
	if _, _, killed := s.Killed(); killed {
		flags = append(flags, "terminated")
	}
	if s.recordingIsBroken() {
		flags = append(flags, "recording-incomplete")
	}
	return flags
}

// reportedFidelity is what this session's artefact can actually evidence.
func (s *Session) reportedFidelity() string {
	if s.recordingIsBroken() {
		return "none"
	}
	return "pty"
}

func (s *Session) recordingIsBroken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordingBroken
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

	// The header is not broadcast: a viewer attaching mid-session gets the
	// backlog of frames, and xterm needs no asciicast header to render them.
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
	rec.SetTap(s.hub.Broadcast)
	return nil
}

// Terminate ends the session on an administrator's instruction.
//
// The reason is written into the recording before the connection drops. Without
// it a replay simply stops, which is indistinguishable from a network failure —
// and "the session ended" is a much weaker finding for an investigator than
// "an administrator ended it, at this point, for this reason".
//
// Returns false if the session had already finished, so the caller can say so
// rather than report a kill that did not happen.
func (s *Session) Terminate(by, reason string) bool {
	s.mu.Lock()
	if s.closed || s.killedBy != "" {
		s.mu.Unlock()
		return false
	}
	if reason == "" {
		reason = "no reason given"
	}
	s.killedBy, s.killReason = by, reason
	s.terminatedAt = time.Now().UTC()
	rec, client := s.rec, s.client
	s.mu.Unlock()

	notice := fmt.Sprintf("\r\n\x1b[1;31margus: session terminated by %s (%s)\x1b[0m\r\n", by, reason)
	if rec != nil {
		// Into the chain, so the notice is as tamper-evident as the session it
		// ends, and visible to anyone shadowing at the moment it happens.
		_ = rec.Write(recorder.Output, []byte(notice))
	}

	// Closing the client connection is what actually ends it. The user's own
	// connection collapses with it, since every channel is multiplexed over
	// this one transport.
	if client != nil {
		_ = client.Close()
	}
	s.log.Warn("session terminated", "session", s.ID, "by", by, "reason", reason)
	return true
}

// Hub exposes the session's broadcast hub to shadow viewers.
func (s *Session) Hub() *live.Hub { return s.hub }

// Killed reports the administrative termination, if there was one.
func (s *Session) Killed() (by, reason string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killedBy, s.killReason, s.killedBy != ""
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
				// Returning here would only stop this one direction. The
				// session would carry on, unrecorded, with the user none the
				// wiser -- which is the exact outcome "all privileged sessions
				// are recorded" is supposed to rule out.
				s.recordingLost(err)
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

// recordingLost handles a recording that stopped working mid-session.
//
// The recorder failing is not a detail of one copy loop: the disk filled, the
// volume went read-only, or something removed the file. Whatever the cause,
// every byte from here on is unrecorded, so the only question is whether the
// session is allowed to continue producing them.
//
// Policy decides, and defaults to no. An operator can deliberately choose
// availability over evidence -- some fleets would rather keep a session alive
// during an incident than cut it -- but that is a decision someone makes with
// their name on it, and the session is marked so nobody later mistakes its
// recording for a complete one.
func (s *Session) recordingLost(err error) {
	s.mu.Lock()
	if s.recordingBroken {
		s.mu.Unlock()
		return // already handled; both relay directions can arrive here
	}
	s.recordingBroken = true
	s.mu.Unlock()

	failClosed := s.policy().FailClosedOnRecordingLoss
	s.log.Error("recording failed mid-session",
		"session", s.ID, "user", s.User, "target", s.Target.Hostname,
		"fail_closed", failClosed, "error", err)

	// Into the audit chain either way. A session whose recording stopped is a
	// gap in the evidence, and the gap itself has to be evidence.
	s.auditForward("session.recording_lost", "critical",
		fmt.Sprintf("Recording stopped part-way through this session: %v. "+
			"Everything after that point is unrecorded. %s", err,
			map[bool]string{
				true:  "The session was terminated because policy fails closed on recording loss.",
				false: "The session was allowed to continue: policy does not fail closed on recording loss.",
			}[failClosed]))

	if failClosed {
		s.Terminate("argus", "recording failed and policy requires every session to be recorded")
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
			if name == "sftp" && s.policy().ProxySftpSubsystem {
				// Decode the subsystem so transfers become per-file audit
				// events. The monitor only ever observes a copy: a fault in it
				// must degrade the audit trail, never the session.
				s.startSFTPMonitor()
			} else if name == "sftp" {
				// Policy allows raw SFTP. The transfer still happens and is
				// still recorded as bytes; what is lost is the per-file detail,
				// so the log says which of the two an auditor is looking at.
				s.log.Warn("sftp proxied without decoding — no per-file audit events",
					"session", s.ID, "user", s.User)
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
			// Agent and X11 forwarding arrive here. Both are governed by
			// policy, and both are refused unless an owner has deliberately
			// opened them — agent forwarding in particular hands anyone with
			// root on this gateway the ability to sign challenges with the
			// user's keys for as long as the session lasts.
			if governed, allowed := s.policy().channelRequestAllowed(req.Type); governed {
				if !allowed {
					s.log.Info("forwarding request refused by policy",
						"type", req.Type, "session", s.ID, "user", s.User)
					s.replyReq(req, false)
					continue
				}
				if s.userConn == nil {
					// A browser session has nowhere to forward to. Saying so is
					// better than accepting and then failing to open the
					// channel the target will go on to request.
					s.replyReq(req, false)
					continue
				}
				// Passed through verbatim. The target answers, and its answer is
				// the one the client gets — the gateway does not invent a
				// success the far end did not give.
				ok, err := target.SendRequest(req.Type, true, req.Payload)
				if err != nil {
					s.log.Warn("forwarding request failed on the target",
						"type", req.Type, "error", err)
					s.replyReq(req, false)
					continue
				}
				s.log.Info("forwarding request permitted", "type", req.Type, "accepted", ok)
				s.replyReq(req, ok)
				continue
			}
			s.log.Debug("unhandled request", "type", req.Type)
			s.replyReq(req, false)
		}
	}
	_ = clientCh.CloseWrite()
}

// policy is the configuration this session runs under.
//
// The value captured when the session opened, not the current one. Tightening
// policy must not revoke a channel a user was already told they could have —
// terminating the session is how you stop one in progress, and that is a
// deliberate act with its own audit entry. Loosening mid-session is equally
// not applied, so the policy a session ran under is the one recorded against it.
func (s *Session) policy() Policy {
	return s.pol
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
	// Ends every shadow subscription. A viewer left hanging on a finished
	// session cannot tell it from one that has gone quiet.
	if s.hub != nil {
		s.hub.Close()
	}
	if s.recFile != nil {
		_ = s.recFile.Close()
	}
	// Ports opened on the target by -R must not outlive the session that
	// authorised them.
	s.remote.closeAll()
	if s.client != nil {
		_ = s.client.Close()
	}
	return s.chainEnd, err
}

// loadInjectedKey reads the credential the gateway presents to the target.
func loadInjectedKey(path string) (ssh.Signer, error) {
	// Mode-checked: an injected key that every local account can read is not
	// a vaulted credential, whatever the inventory calls it.
	data, err := secrets.ReadPrivate(path)
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

// Live reports whether the session is still connected.
func (s *Session) Live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// watchTargetChannels routes channels the target opens back to the client.
//
// Registered once, at session start, because x/crypto/ssh requires a handler to
// exist before the peer opens the channel — a target that requests agent
// forwarding does so immediately, and a handler installed lazily would miss it.
//
// Only agent and X11 are claimed. Anything else the target tries to open falls
// through to x/crypto/ssh's own refusal, which is the correct answer for a
// bastion: a target is not supposed to be initiating connections into the
// operator's machine.
func (s *Session) watchTargetChannels() {
	client := s.targetClient()
	if client == nil || s.userConn == nil {
		return
	}
	for _, kind := range []string{"auth-agent@openssh.com", "x11"} {
		chans := client.HandleChannelOpen(kind)
		if chans == nil {
			continue // already claimed
		}
		go func() {
			for newChan := range chans {
				go s.handleTargetChannel(newChan)
			}
		}()
	}
}

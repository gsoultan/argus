package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gsoultan/argus/internal/execlog"
	"github.com/gsoultan/argus/internal/recorder"
)

// Collector receives session streams from shims and owns the recordings.
//
// Running as root and holding the only writable handle to the recording is the
// point: the user whose session is being recorded can send bytes but can never
// reach the artefact. Compare with the naive design where the shim writes the
// file directly, which lets anyone edit the evidence of what they just did.
type Collector struct {
	dir string
	log *slog.Logger

	mu       sync.Mutex
	active   map[string]*activeSession
	listener net.Listener
	wg       sync.WaitGroup
	closing  bool
	// conns holds each shim's connection. A shim that is still attached keeps
	// handle() blocked on a read, and without a way to close it a shutdown
	// waits for a shell nobody is going to exit.
	conns map[net.Conn]struct{}
	// hangups ends each session the agent opened itself through the tunnel,
	// for the same reason: those have no shim connection to close.
	hangups map[string]func()
	// tunnels is every session through the tunnel still open, terminal or
	// desktop, for the reports that say they are running.
	tunnels map[string]liveRecording

	// OnSession is called when a recording is sealed. The daemon uses it to
	// report to the control plane; nil in standalone mode.
	OnSession func(SessionRecord)

	// OnActive is called when a session through the tunnel starts, so the
	// console can show it running -- and watch or end it -- rather than
	// learning of it only once it is over. The daemon also repeats it for
	// every such session still open, see ActiveTunnelRecords. Not called for
	// logins through sshd: a direct login's report is an alarm, and one per
	// minute would be noise.
	OnActive func(SessionRecord)

	// OnRefused is called for a direct login turned away because the
	// deployment is console-only. The daemon reports it; nil in standalone
	// mode, where nothing can switch console-only on.
	OnRefused func(SessionStart)

	// OnAllowed is called for a direct login let in because the server is
	// set to allow and record them, so the console hears of it when it starts
	// rather than when it ends.
	OnAllowed func(SessionStart)

	// AskApproval asks the control plane whether a direct login may go ahead
	// and waits for the answer, when the server is set to "approve". Nil,
	// or a control plane that cannot be reached, is a refusal.
	AskApproval func(ctx context.Context, m SessionStart) Approval

	// directSSH is the console's direct SSH mode as of the last heartbeat:
	// DirectSSHRefuse, DirectSSHApprove or DirectSSHAllow. Unset until the
	// control plane says, so an agent installed before it can reach the
	// control plane does not lock its host's administrators out; it records
	// every login meanwhile.
	directSSH atomic.Pointer[string]

	// personal is the personal accounts this server holds, by name, as of
	// the last heartbeat. Nil until the control plane says: no account is
	// treated as personal before then.
	personal atomic.Pointer[map[string]PersonalAccount]

	// stop is closed when the collector starts shutting down, so a login
	// waiting for approval is refused rather than holding the shutdown.
	stop     chan struct{}
	stopOnce sync.Once

	// Exec attributes kernel-observed executions to sessions. Nil, or holding
	// no probe, on hosts without kernel support — the collector behaves
	// identically apart from the fidelity it reports, so there is one path
	// rather than a condition at every call site.
	Exec *execlog.Context
}

// SessionRecord is what the control plane is told about a finished session.
type SessionRecord struct {
	ID         string    `json:"id"`
	Principal  string    `json:"principal"`
	ClientAddr string    `json:"client_addr"`
	Origin     Origin    `json:"origin"`
	Command    string    `json:"command,omitempty"`
	Hostname   string    `json:"hostname"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	ExitCode   int       `json:"exit_code"`
	ChainHead  string    `json:"chain_head"`
	Bytes      int64     `json:"bytes"`
	Path       string    `json:"path"`
	// Fidelity is what this recording can actually evidence, decided per
	// session rather than per host: a session recorded while the probe was
	// unavailable must not claim kernel evidence it does not contain.
	Fidelity string `json:"fidelity"`
	// Commands counts kernel-observed executions, and is zero for a PTY-only
	// recording rather than a guess derived from terminal output.
	Commands int `json:"commands"`
	// Actor is who a tunnel session was opened for, from the signed grant.
	// Empty for a login through sshd, where only the local account is known.
	Actor string `json:"actor,omitempty"`
	// Protocol is "ssh" for a terminal, "rdp" for a desktop. Empty is ssh.
	Protocol string `json:"protocol,omitempty"`
	// State is "active" while it runs, then "closed", or "terminated" when
	// someone ended it from the console.
	State string `json:"state,omitempty"`
	// TerminatedBy and TerminationReason say who ended it and why.
	TerminatedBy      string `json:"terminated_by,omitempty"`
	TerminationReason string `json:"termination_reason,omitempty"`
	// Flags are risk flags the service that opened the session knows of,
	// on top of what the daemon derives itself.
	Flags []string `json:"flags,omitempty"`
	// DirectApproval says how a direct login was let in: "approved" by an
	// admin, under the approval DirectLoginID, or "allowed" by the server's
	// setting. Empty for a login let in before the agent heard any setting.
	DirectApproval string `json:"direct_approval,omitempty"`
	DirectLoginID  string `json:"direct_login_id,omitempty"`
}

type activeSession struct {
	id    string
	start SessionStart
	rec   *recorder.Recorder
	file  *os.File
	path  string

	mu    sync.Mutex
	execs int
	// ptyOnly records that kernel tracing was not attached to this session in
	// particular, so its fidelity is reported honestly even when the host as a
	// whole supports it.
	ptyOnly bool
	// execLost records that a kernel execution was observed but could not be
	// written. The claim eBPF fidelity makes is "every execve is here", and one
	// dropped event makes that false for the whole recording.
	execLost bool
	// execDropped counts executions that never arrived at all, as opposed to
	// execLost which is one that arrived and could not be written. Different
	// causes, same consequence for what the artefact can claim.
	execDropped uint64
	// terminatedBy and terminationReason are set when the console ended it.
	terminatedBy, terminationReason string
	// directApproval and directLoginID say how a direct login was let in.
	directApproval, directLoginID string
}

// Direct SSH modes, as the control plane names them.
const (
	DirectSSHRefuse  = "refuse"
	DirectSSHApprove = "approve"
	DirectSSHAllow   = "allow"
)

// DirectApprovalWait bounds how long a direct login is held for a decision.
// The control plane refuses one nobody decided after two minutes; this is
// the agent's own limit in case that answer never arrives.
const DirectApprovalWait = 2*time.Minute + 15*time.Second

// Approval is the control plane's answer about one direct login.
type Approval struct {
	// Approved means the login may go ahead. Allowed means it went ahead
	// because the server is set to allow, with nobody approving it.
	Approved, Allowed bool
	// Owner is the person whose personal account the login used, when the
	// control plane has said it is one they may reach directly. Personal
	// means it was let in on that alone, with nobody approving it.
	Owner    string
	Personal bool
	// ID is the approval the login was let in on, or refused under.
	ID string
	// Reason is said to the person when the login is refused.
	Reason string
}

// SetDirectSSH applies the control plane's direct SSH mode. Anything other
// than the three modes is a refusal.
func (c *Collector) SetDirectSSH(mode string) {
	switch mode {
	case DirectSSHRefuse, DirectSSHApprove, DirectSSHAllow:
	default:
		mode = DirectSSHRefuse
	}
	c.directSSH.Store(&mode)
}

// DirectSSH is the mode being applied, or "" before the control plane has
// said anything.
func (c *Collector) DirectSSH() string {
	if m := c.directSSH.Load(); m != nil {
		return *m
	}
	return ""
}

// SetConsoleOnly applies a control plane older than the direct SSH setting,
// which says only whether to refuse.
func (c *Collector) SetConsoleOnly(on bool) {
	if on {
		c.SetDirectSSH(DirectSSHRefuse)
		return
	}
	c.SetDirectSSH(DirectSSHAllow)
}

// ConsoleOnly reports whether a direct login is kept out unless someone
// approves it: refused outright, or held for approval.
func (c *Collector) ConsoleOnly() bool {
	m := c.DirectSSH()
	return m == DirectSSHRefuse || m == DirectSSHApprove
}

// stopping is closed once the collector starts shutting down.
func (c *Collector) stopping() chan struct{} {
	c.stopOnce.Do(func() { c.stop = make(chan struct{}) })
	return c.stop
}

// ConsoleOnlyReason is what a refused direct login is told.
const ConsoleOnlyReason = "this server accepts sessions from the Argus console only; " +
	"open it from the console in your browser. ssh, scp and sftp straight to this " +
	"server are refused, and this attempt has been reported"

// NewCollector builds a collector writing recordings into dir.
func NewCollector(dir string, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	return &Collector{dir: dir, log: log, active: map[string]*activeSession{}}
}

// Listen serves the unix socket at path until Close.
func (c *Collector) Listen(path string) error {
	if err := prepareSocketDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	// A stale socket from an unclean shutdown would otherwise make every
	// subsequent start fail with "address already in use".
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", path, err)
	}
	// Published under the mutex, because Close reads it from another goroutine.
	// The unsynchronised write was invisible until the shutdown path started
	// reading the field properly, which is the usual way these surface.
	//
	// A shutdown that arrives before this point has already set closing, and
	// leaving the listener unpublished would let it accept after Close ran.
	c.mu.Lock()
	closed := c.closing
	c.listener = ln
	c.mu.Unlock()
	if closed {
		_ = ln.Close()
		return nil
	}

	// Any local user must be able to connect — their session is what we are
	// recording. See openSocket.
	if err := openSocket(path); err != nil {
		return err
	}
	if err := ProtectStateDir(c.dir); err != nil {
		return fmt.Errorf("create recording dir: %w", err)
	}

	c.log.Info("collector listening", "socket", path, "recordings", c.dir)

	for {
		conn, err := ln.Accept()
		if err != nil {
			c.mu.Lock()
			closing := c.closing
			c.mu.Unlock()
			if closing {
				return nil
			}
			c.log.Error("accept failed", "error", err)
			continue
		}
		c.wg.Add(1)
		c.trackConn(conn)
		go func() {
			defer c.wg.Done()
			defer c.untrackConn(conn)
			c.handle(conn)
		}()
	}
}

// DefaultDrain bounds how long a shutdown waits for shims to finish.
//
// The agent runs on every managed host, so this is the shutdown that happens
// most often -- once per host per upgrade.
const DefaultDrain = 30 * time.Second

// sealGrace is how long the shims get to finish writing once disconnected.
const sealGrace = 10 * time.Second

// Close stops accepting and drains in-flight recordings.
func (c *Collector) Close() error { return c.CloseWithin(DefaultDrain) }

// CloseWithin stops accepting, waits up to d for shims to finish, then
// disconnects the rest so their recordings are sealed.
//
// Unbounded before. A shim stays attached for the life of the shell it is
// recording, so any host with someone logged in held the agent's shutdown open
// until systemd sent SIGKILL -- and SIGKILL means the recording is never
// sealed, has no chain head, and cannot be verified. On a fleet, an upgrade
// did that to every host with an active session at once.
func (c *Collector) CloseWithin(d time.Duration) error {
	stop := c.stopping()
	c.mu.Lock()
	if !c.closing {
		close(stop)
	}
	c.closing = true
	ln := c.listener
	c.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}

	if !waitGroup(&c.wg, d) {
		c.mu.Lock()
		open := make([]net.Conn, 0, len(c.conns))
		for conn := range c.conns {
			open = append(open, conn)
		}
		hangups := make([]func(), 0, len(c.hangups))
		for _, h := range c.hangups {
			hangups = append(hangups, h)
		}
		active := len(c.active)
		c.mu.Unlock()

		c.log.Warn("sessions still open at the end of the drain window; "+
			"disconnecting them so their recordings are sealed",
			"shims", len(open), "tunnel_sessions", len(hangups), "sessions", active, "drain", d)
		for _, conn := range open {
			_ = conn.Close()
		}
		for _, h := range hangups {
			h()
		}
		if !waitGroup(&c.wg, sealGrace) {
			c.log.Error("recordings did not finish sealing within the grace period",
				"grace", sealGrace,
				"detail", "they may have no chain head and cannot be verified")
		}
	}
	return nil
}

func (c *Collector) trackConn(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conns == nil {
		c.conns = map[net.Conn]struct{}{}
	}
	c.conns[conn] = struct{}{}
}

func (c *Collector) untrackConn(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.conns, conn)
}

// waitGroup waits on wg for at most d, reporting whether it drained.
func waitGroup(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// ActiveCount reports sessions currently being recorded.
func (c *Collector) ActiveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.active)
}

func (c *Collector) handle(conn net.Conn) {
	defer conn.Close()

	sc := bufio.NewScanner(conn)
	// A single burst of terminal output can be large; the 64 KB default would
	// truncate a session mid-stream and silently lose evidence.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var sess *activeSession
	exitCode := 0

	defer func() {
		if sess == nil {
			return
		}
		// Seal on any exit path, including a shim that died mid-session. A
		// half-recorded session is still evidence; losing it is not acceptable.
		c.seal(sess, exitCode, conn)
	}()

	for sc.Scan() {
		msg, err := DecodeMessage(sc.Bytes())
		if err != nil {
			c.log.Warn("bad message from shim", "error", err)
			return
		}

		switch m := msg.(type) {
		case SessionStart:
			if sess != nil {
				c.log.Warn("duplicate start on one connection")
				return
			}
			// Everything on this socket came in through sshd, and nothing
			// Argus opens does: the console's sessions arrive over the tunnel.
			// So a login here is direct whatever its start message claims --
			// the socket is open to every local user, and a claim of having
			// come through Argus is not something to take from one of them.
			m.Origin = Direct
			// A login that did not come through Argus happens only if the
			// server's setting lets it: held for an admin, or allowed and
			// recorded. Every login that is admitted is recorded.
			approval, ok := c.admitDirect(conn, m)
			if !ok {
				return
			}
			// A personal account names its owner, so the session is theirs
			// and not only the account's.
			m.Actor = approval.Owner
			sess, err = c.open(m)
			if err != nil {
				c.log.Error("cannot open recording", "error", err)
				_ = json.NewEncoder(conn).Encode(Ack{Error: err.Error()})
				return
			}
			switch {
			case approval.Personal:
				sess.directApproval = "personal"
				if c.OnAllowed != nil {
					c.OnAllowed(m)
				}
			case approval.Allowed:
				sess.directApproval = "allowed"
				if c.OnAllowed != nil {
					c.OnAllowed(m)
				}
			case approval.Approved:
				sess.directApproval, sess.directLoginID = "approved", approval.ID
			}
			_ = json.NewEncoder(conn).Encode(Decision{
				Type: "decision", Record: true, SessionID: sess.id,
			})
			// A bypass is the event the agent exists to catch. Log it loudly at
			// the moment it happens, not only when the session ends — a long
			// session should not delay the alarm.
			if m.Origin == Direct {
				c.log.Warn("DIRECT SESSION — Argus was bypassed",
					"session", sess.id,
					"principal", m.Principal,
					"client", m.ClientAddr,
					"command", m.Command,
					"detail", "no approval, time window or principal check was applied to this session")
			} else {
				c.log.Info("brokered session",
					"session", sess.id, "principal", m.Principal, "client", m.ClientAddr)
			}

		case Frame:
			if sess == nil {
				c.log.Warn("frame before start")
				return
			}
			if err := c.writeFrame(sess, m); err != nil {
				c.log.Error("recording write failed", "session", sess.id, "error", err)
				return
			}

		case SessionEnd:
			exitCode = m.ExitCode
			return
		}
	}
	if err := sc.Err(); err != nil {
		c.log.Warn("shim connection error", "error", err)
	}
}

func (c *Collector) open(m SessionStart) (*activeSession, error) {
	id := newID()
	path := filepath.Join(c.dir, id+".cast")

	// 0600 as root: the recorded user cannot read their own session back, let
	// alone modify it.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}

	cols, rows := m.Cols, m.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	rec, err := recorder.New(f, recorder.Header{
		Width:     cols,
		Height:    rows,
		Timestamp: m.StartedAt.Unix(),
		Title:     fmt.Sprintf("%s@%s", m.Principal, m.Hostname),
		Env: map[string]string{
			"ARGUS_SESSION": id,
			"ARGUS_ORIGIN":  string(m.Origin),
			"ARGUS_CLIENT":  m.ClientAddr,
		},
	})
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	sess := &activeSession{id: id, start: m, rec: rec, file: f, path: path}

	// The shim's PID is the session leader; everything the user runs descends
	// from it. Attached before the shell is exec'd, so the first command is
	// caught rather than being the one that gets away.
	if m.PID > 0 {
		if err := c.Exec.Attach(id, m.PID, func(e execlog.Exec) {
			sess.recordExec(e, c.log)
		}); err != nil {
			// Loudly: a session that believes it has kernel evidence and does
			// not is worse than one that never claimed to.
			c.log.Error("kernel execution tracing unavailable for this session",
				"session", id, "pid", m.PID, "error", err,
				"detail", "the recording will be terminal output only")
			sess.ptyOnly = true
		}
	} else {
		sess.ptyOnly = true
	}

	c.mu.Lock()
	c.active[id] = sess
	c.mu.Unlock()
	return sess, nil
}

func (c *Collector) writeFrame(sess *activeSession, f Frame) error {
	switch f.Type {
	case "o":
		return sess.rec.Write(recorder.Output, []byte(f.Data))
	case "i":
		return sess.rec.Write(recorder.Input, []byte(f.Data))
	case "r":
		var cols, rows int
		if _, err := fmt.Sscanf(f.Data, "%dx%d", &cols, &rows); err != nil {
			return nil // a malformed resize is not worth killing a session over
		}
		return sess.rec.Resize(cols, rows)
	}
	return nil
}

func (c *Collector) seal(sess *activeSession, exitCode int, conn net.Conn) {
	c.mu.Lock()
	delete(c.active, sess.id)
	c.mu.Unlock()

	c.Exec.Detach(sess.id, sess.start.PID)

	head, err := sess.rec.Close()
	if err != nil {
		c.log.Error("sealing recording failed", "session", sess.id, "error", err)
	}
	_ = sess.file.Close()

	// After the close, before the claim. An execution that arrives between
	// Detach and here finds no sink, and counting it anywhere the fidelity
	// decision does not read is the same as discarding it. Taken once: this
	// releases the counters.
	switch lost, known := c.Exec.TakeLost(sess.id); {
	case !known:
		// The probe could not read its own counter, so whether anything was
		// lost is unknown -- and an unknown is not a zero. eBPF fidelity
		// asserts every execve is in the file, which is not a claim to make
		// from a counter that could not be read.
		sess.noteExecDropped(1)
		c.log.Error("cannot tell whether kernel executions were lost; reporting reduced fidelity",
			"session", sess.id,
			"detail", "the recording is complete as terminal output; nothing "+
				"can vouch for it as a list of everything that ran")
	case lost > 0:
		sess.noteExecDropped(lost)
		c.log.Warn("kernel executions were lost; reporting reduced fidelity",
			"session", sess.id, "lost", lost,
			"detail", "the recording is complete as terminal output but cannot "+
				"stand as a list of everything that ran")
	}

	_, bytes, _ := sess.rec.Stats()

	state := "closed"
	sess.mu.Lock()
	by, why := sess.terminatedBy, sess.terminationReason
	sess.mu.Unlock()
	if by != "" {
		state = "terminated"
	}
	rec := SessionRecord{
		ID:         sess.id,
		Principal:  sess.start.Principal,
		ClientAddr: sess.start.ClientAddr,
		Origin:     sess.start.Origin,
		Command:    sess.start.Command,
		Hostname:   sess.start.Hostname,
		StartedAt:  sess.start.StartedAt,
		EndedAt:    time.Now().UTC(),
		ExitCode:   exitCode,
		ChainHead:  head,
		Bytes:      bytes,
		Path:       sess.path,
		Fidelity:   sess.fidelity(c.Exec),
		Commands:   sess.commandCount(),
		Actor:      sess.start.Actor,
		Protocol:   "ssh",
		State:      state,

		TerminatedBy:      by,
		TerminationReason: why,

		DirectApproval: sess.directApproval,
		DirectLoginID:  sess.directLoginID,
	}

	c.log.Info("recording sealed",
		"session", sess.id,
		"origin", rec.Origin,
		"principal", rec.Principal,
		"exit", exitCode,
		"chain_head", head,
		"bytes", bytes)

	if conn != nil {
		_ = json.NewEncoder(conn).Encode(Ack{SessionID: sess.id, ChainHead: head})
	}
	if c.OnSession != nil {
		c.OnSession(rec)
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// recordExec writes one kernel-observed execution into the recording.
//
// A failure here is not logged and dropped. eBPF fidelity claims the recording
// contains every execution; a command the kernel saw and the file did not makes
// that a false statement about the artefact, and the artefact is the evidence.
// The session keeps recording -- the terminal output is still complete and
// still chained -- but it stops claiming to be a full list of what ran.
func (s *activeSession) recordExec(e execlog.Exec, log *slog.Logger) {
	// At the moment the kernel saw it, not the moment this got round to
	// writing it. See Recorder.ExecAt.
	if err := s.rec.ExecAt(e.At, e); err != nil {
		s.mu.Lock()
		first := !s.execLost
		s.execLost = true
		s.mu.Unlock()
		// Once per session. A full disk fails every subsequent execution too,
		// and a line per command buries the one that matters.
		if first && log != nil {
			// The program, not the command line: argv carries credentials,
			// and this log is not the access-controlled artefact they were
			// captured into.
			log.Error("recording a kernel execution failed",
				"session", s.id, "program", e.Filename, "argc", len(e.Args), "error", err,
				"detail", "this recording no longer evidences every execution "+
					"and will be reported at reduced fidelity")
		}
		return
	}
	s.mu.Lock()
	s.execs++
	s.mu.Unlock()
}

// noteExecDropped records executions lost before they reached this session.
func (s *activeSession) noteExecDropped(n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execDropped += n
}

// fidelity reports what this recording can evidence.
func (s *activeSession) fidelity(c *execlog.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptyOnly || s.execLost || s.execDropped > 0 {
		// Degraded rather than absent: the terminal output in this recording is
		// still complete and still chained. What it cannot do is stand as a
		// list of everything that ran.
		return execlog.FidelityPTY
	}
	return c.Fidelity()
}

func (s *activeSession) commandCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execs
}

// admitDirect decides whether a direct login may go ahead, telling the shim
// when it may not. ok is false when the login is refused and the connection
// is done with.
func (c *Collector) admitDirect(conn net.Conn, m SessionStart) (Approval, bool) {
	refuse := func(reason string) (Approval, bool) {
		_ = json.NewEncoder(conn).Encode(Decision{
			Type: "decision", Record: false, Refused: true, Reason: reason, Error: reason,
		})
		return Approval{}, false
	}
	// A personal account a team may reach directly decides for itself,
	// whatever the server's setting for every other account.
	if p, ok := c.PersonalAccount(m.Principal); ok {
		switch p.Direct {
		case DirectSSHAllow:
			c.log.Warn("DIRECT LOGIN ALLOWED — a personal account its owner may reach directly",
				"principal", m.Principal, "owner", p.Owner, "client", m.ClientAddr, "command", m.Command)
			return Approval{Approved: true, Allowed: true, Personal: true, Owner: p.Owner}, true
		case DirectSSHApprove:
			a, ok := c.awaitApproval(conn, m, refuse)
			a.Owner = p.Owner
			return a, ok
		}
	}
	switch c.DirectSSH() {
	case "":
		// Never heard from the control plane: record, do not refuse.
		return Approval{}, true
	case DirectSSHAllow:
		c.log.Warn("DIRECT LOGIN ALLOWED — this server is set to allow and record them",
			"principal", m.Principal, "client", m.ClientAddr, "command", m.Command)
		return Approval{Approved: true, Allowed: true}, true
	case DirectSSHApprove:
		return c.awaitApproval(conn, m, refuse)
	}

	c.log.Warn("DIRECT LOGIN REFUSED — this host is console-only",
		"principal", m.Principal,
		"client", m.ClientAddr,
		"command", m.Command,
		"detail", "the login reached sshd without Argus; whoever holds its "+
			"key or password can still reach sshd directly")
	a, ok := refuse(ConsoleOnlyReason)
	if c.OnRefused != nil {
		c.OnRefused(m)
	}
	return a, ok
}

// ApprovalPendingReason is what a person holding a direct login is told while
// it waits.
const ApprovalPendingReason = "this login is waiting for approval in the Argus console. " +
	"An admin has been asked; it will be refused if nobody approves it in two minutes"

// awaitApproval holds a direct login while the control plane asks an admin.
func (c *Collector) awaitApproval(
	conn net.Conn, m SessionStart, refuse func(string) (Approval, bool),
) (Approval, bool) {
	if c.AskApproval == nil {
		c.log.Warn("DIRECT LOGIN REFUSED — approval is required and there is no control plane to ask",
			"principal", m.Principal, "client", m.ClientAddr)
		if c.OnRefused != nil {
			c.OnRefused(m)
		}
		return refuse("this server needs an admin to approve direct logins, and Argus cannot be reached")
	}
	// Refused and Error as well as Pending: a shim older than Pending reads
	// it as a refusal and stops there, rather than as a login to let through.
	if err := json.NewEncoder(conn).Encode(Decision{
		Type: "decision", Pending: true, Refused: true,
		Reason: ApprovalPendingReason, Error: ApprovalPendingReason,
	}); err != nil {
		return Approval{}, false
	}
	c.log.Warn("direct login waiting for approval",
		"principal", m.Principal, "client", m.ClientAddr, "command", m.Command)

	ctx, cancel := context.WithTimeout(context.Background(), DirectApprovalWait)
	defer cancel()
	go func() {
		select {
		case <-c.stopping():
			cancel()
		case <-ctx.Done():
		}
	}()
	a := c.AskApproval(ctx, m)
	if !a.Approved {
		reason := a.Reason
		if reason == "" {
			reason = "this login was not approved"
		}
		c.log.Warn("DIRECT LOGIN REFUSED — not approved",
			"principal", m.Principal, "client", m.ClientAddr, "approval", a.ID, "reason", reason)
		// A request the control plane never received is reported as a
		// refusal; one it holds is already in the audit log with its outcome.
		if a.ID == "" && c.OnRefused != nil {
			c.OnRefused(m)
		}
		return refuse(reason)
	}
	c.log.Warn("DIRECT LOGIN APPROVED", "principal", m.Principal, "client", m.ClientAddr,
		"approval", a.ID)
	return a, true
}

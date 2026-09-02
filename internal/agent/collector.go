package agent

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
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

	// OnSession is called when a recording is sealed. The daemon uses it to
	// report to the control plane; nil in standalone mode.
	OnSession func(SessionRecord)

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
}

// NewCollector builds a collector writing recordings into dir.
func NewCollector(dir string, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	return &Collector{dir: dir, log: log, active: map[string]*activeSession{}}
}

// Listen serves the unix socket at path until Close.
func (c *Collector) Listen(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	// A stale socket from an unclean shutdown would otherwise make every
	// subsequent start fail with "address already in use".
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", path, err)
	}
	c.listener = ln

	// Any local user must be able to connect — their session is what we are
	// recording. Confidentiality comes from the daemon owning the output, not
	// from restricting who may speak to it.
	if err := os.Chmod(path, 0o666); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
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
		go func() {
			defer c.wg.Done()
			c.handle(conn)
		}()
	}
}

// Close stops accepting and waits for in-flight recordings to seal.
func (c *Collector) Close() error {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	if c.listener != nil {
		_ = c.listener.Close()
	}
	c.wg.Wait()
	return nil
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
			sess, err = c.open(m)
			if err != nil {
				c.log.Error("cannot open recording", "error", err)
				_ = json.NewEncoder(conn).Encode(Ack{Error: err.Error()})
				return
			}
			// A bypass is the event the agent exists to catch. Log it loudly at
			// the moment it happens, not only when the session ends — a long
			// session should not delay the alarm.
			if m.Origin == Direct {
				c.log.Warn("DIRECT SESSION — gateway was bypassed",
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
			if err := sess.rec.Exec(e); err != nil {
				c.log.Error("recording a kernel execution failed",
					"session", id, "command", e.CommandLine(), "error", err)
				return
			}
			sess.mu.Lock()
			sess.execs++
			sess.mu.Unlock()
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

	_, bytes, _ := sess.rec.Stats()

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

// fidelity reports what this recording can evidence.
func (s *activeSession) fidelity(c *execlog.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptyOnly {
		return execlog.FidelityPTY
	}
	return c.Fidelity()
}

func (s *activeSession) commandCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execs
}

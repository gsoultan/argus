package agent

import (
	"errors"

	"github.com/gsoultan/argus/internal/recorder"
)

// TunnelRecording is the recording of a session the agent opened itself,
// through the tunnel. Same file, same chain, same kernel tracing and same
// report as a login through sshd; only the bytes arrive in-process instead
// of over the shim's socket.
type TunnelRecording struct {
	c    *Collector
	sess *activeSession
}

// ErrShuttingDown refuses a session that would start during a drain.
var ErrShuttingDown = errors.New("the agent is shutting down")

// StartRecording opens a recording for m. m.PID must be the process that
// will become the session, not yet running anything, so kernel tracing is
// attached before the first execution. hangup ends the session; a shutdown
// calls it when the drain window runs out.
func (c *Collector) StartRecording(m SessionStart, hangup func()) (*TunnelRecording, error) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, ErrShuttingDown
	}
	// Counted with the shims, so a shutdown drains these too.
	c.wg.Add(1)
	c.mu.Unlock()

	// Listen makes the directory for sshd logins; a host can take a tunnel
	// session before its first one.
	if err := ProtectStateDir(c.dir); err != nil {
		c.wg.Done()
		return nil, err
	}
	sess, err := c.open(m)
	if err != nil {
		c.wg.Done()
		return nil, err
	}
	c.mu.Lock()
	if c.hangups == nil {
		c.hangups = map[string]func(){}
	}
	c.hangups[sess.id] = hangup
	r := &TunnelRecording{c: c, sess: sess}
	c.trackTunnel(sess.id, r)
	c.mu.Unlock()
	if c.OnActive != nil {
		c.OnActive(r.activeRecord())
	}
	return r, nil
}

// liveRecording is a session the agent opened through the tunnel, as the
// console should see it while it runs.
type liveRecording interface {
	activeRecord() SessionRecord
}

// trackTunnel registers a tunnel session for ActiveTunnelRecords. Called
// with c.mu held.
func (c *Collector) trackTunnel(id string, r liveRecording) {
	if c.tunnels == nil {
		c.tunnels = map[string]liveRecording{}
	}
	c.tunnels[id] = r
}

// ActiveTunnelRecords is every session through the tunnel still open, as
// an "active" report. The daemon repeats these so a long session is never
// mistaken for one nothing is speaking for.
func (c *Collector) ActiveTunnelRecords() []SessionRecord {
	c.mu.Lock()
	open := make([]liveRecording, 0, len(c.tunnels))
	for _, r := range c.tunnels {
		open = append(open, r)
	}
	c.mu.Unlock()
	out := make([]SessionRecord, 0, len(open))
	for _, r := range open {
		out = append(out, r.activeRecord())
	}
	return out
}

func (r *TunnelRecording) activeRecord() SessionRecord {
	s := r.sess
	_, bytes, _ := s.rec.Stats()
	return SessionRecord{
		ID:         s.id,
		Principal:  s.start.Principal,
		ClientAddr: s.start.ClientAddr,
		Origin:     s.start.Origin,
		Hostname:   s.start.Hostname,
		StartedAt:  s.start.StartedAt,
		Bytes:      bytes,
		Fidelity:   s.fidelity(r.c.Exec),
		Commands:   s.commandCount(),
		Actor:      s.start.Actor,
		Protocol:   "ssh",
		State:      "active",
	}
}

// Terminate records that by ended the session from the console, for reason.
// The caller hangs the session up; the seal then reports it as terminated.
func (r *TunnelRecording) Terminate(by, reason string) {
	r.sess.mu.Lock()
	defer r.sess.mu.Unlock()
	if r.sess.terminatedBy == "" {
		r.sess.terminatedBy, r.sess.terminationReason = by, reason
	}
}

// ID is the recording's session id.
func (r *TunnelRecording) ID() string { return r.sess.id }

// Output, Input and Resize append to the recording.
func (r *TunnelRecording) Output(p []byte) error { return r.sess.rec.Write(recorder.Output, p) }
func (r *TunnelRecording) Input(p []byte) error  { return r.sess.rec.Write(recorder.Input, p) }
func (r *TunnelRecording) Resize(cols, rows int) error {
	return r.sess.rec.Resize(cols, rows)
}

// Seal closes the recording, reports it, and returns its chain head. Call
// once, after the session's process has exited.
func (r *TunnelRecording) Seal(exitCode int) string {
	r.c.mu.Lock()
	delete(r.c.hangups, r.sess.id)
	delete(r.c.tunnels, r.sess.id)
	r.c.mu.Unlock()
	defer r.c.wg.Done()
	r.c.seal(r.sess, exitCode, nil)
	return r.sess.rec.Head()
}

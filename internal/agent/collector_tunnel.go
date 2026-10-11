package agent

import (
	"errors"
	"os"

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
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
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
	c.mu.Unlock()
	return &TunnelRecording{c: c, sess: sess}, nil
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
	r.c.mu.Unlock()
	defer r.c.wg.Done()
	r.c.seal(r.sess, exitCode, nil)
	return r.sess.rec.Head()
}

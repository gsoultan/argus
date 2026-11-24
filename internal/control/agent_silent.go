package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
When an agent does not answer.

The console never dials a server: the agent connects out and holds a tunnel,
and each session, file transfer or question is a stream the console opens on
it. An open that times out was reported as the transport's own words --
"could not reach the agent on ip-10-0-2-147...: i/o deadline reached" --
which reads as a network address Argus cannot route to, and says nothing of
what to do.

A silent open means one of two things. The tunnel has stalled: the agent's
side is gone or its path is wedged, and nothing on it will be answered until
the agent reconnects, which the regular ping would only notice up to 40
seconds later. Or the agent is up and this one step is slow. The hub tells
them apart with a ping at once: a stalled tunnel is closed there and then,
so the agent reconnects now rather than after the next ping, and the person
is told which of the two it was and what to do.
*/

// agentCheckWait bounds the ping that follows a silent open. Short: the
// person has already waited out the open.
const agentCheckWait = 5 * time.Second

// AgentSilentError is an open on an agent's tunnel that got no answer in
// time. Alive says whether the agent answered a ping straight after.
type AgentSilentError struct {
	Host   string
	Waited time.Duration
	Alive  bool
	Err    error
}

func (e *AgentSilentError) Error() string {
	waited := e.Waited.Round(time.Second)
	if waited <= 0 {
		waited = time.Second
	}
	if e.Alive {
		return fmt.Sprintf("the agent on %s is connected but did not answer within %s. "+
			"It answers a ping, so it is running but was slow to start this; try again, and "+
			"if it keeps happening look at `sudo journalctl -u argus-agent` on that server",
			e.Host, waited)
	}
	return fmt.Sprintf("the agent on %s stopped answering: nothing came back within %s and "+
		"a ping went unanswered too, so its connection to Argus had stalled. Argus has "+
		"closed that connection so the agent reconnects, which usually takes a few seconds; "+
		"try again shortly. If the server stays offline, check it is running and that "+
		"`sudo systemctl status argus-agent` shows the agent active",
		e.Host, waited)
}

func (e *AgentSilentError) Unwrap() error { return e.Err }

// isTimeout reports whether err is a deadline running out, from the
// caller's context or the stream's own deadline on either transport.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// silent turns an open on t that timed out into an AgentSilentError, after
// pinging the agent; a tunnel that does not answer the ping either is
// closed, so the agent reconnects. Any other error is returned as it is.
func (h *TunnelHub) silent(t *liveTunnel, host string, started time.Time, err error) error {
	if err == nil || tunnel.IsRefused(err) || !isTimeout(err) {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentCheckWait)
	defer cancel()
	_, perr := tunnel.Ping(ctx, t.sess)
	// A refusal is an answer: the agent is there, only too busy to take
	// another stream.
	alive := perr == nil || tunnel.IsRefused(perr)
	if !alive {
		h.log.Warn("tunnel: the agent did not answer an open or a ping; closing so it reconnects",
			"host", t.host, "error", err, "ping_error", perr)
		_ = t.sess.Close()
	} else {
		h.log.Warn("tunnel: the agent answered a ping but not an open in time",
			"host", t.host, "error", err)
	}
	return &AgentSilentError{Host: host, Waited: time.Since(started), Alive: alive, Err: err}
}

// agentUnreachable is what the person is told when opening a stream on
// host's agent failed for a reason other than a refusal or no tunnel.
func agentUnreachable(host string, err error) string {
	return agentUnreachableErr(host, err).Error()
}

// agentUnreachableErr is agentUnreachable as an error that still wraps err.
func agentUnreachableErr(host string, err error) error {
	var s *AgentSilentError
	if errors.As(err, &s) {
		// Named as the console calls the server, which can differ from the
		// agent's own name the tunnel is found under.
		c := *s
		c.Host = host
		return &c
	}
	return fmt.Errorf("could not reach the agent on %s: %w", host, err)
}

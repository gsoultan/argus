package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Watching a session from the server it runs on.

	viewer ◀── argus-control ◀──"watch" stream── agent ◀── the recording's tap

The control plane can only show a session it relays itself, and it relays
only the browser terminals it opened -- and only on the replica that opened
them. A login through sshd never passes through it at all. So the agent,
which records every session on its server whichever way it came in, offers
its own read-only view of each one: the lines its recorder has committed,
from the recorder's tap, so what a viewer sees is what the recording holds.

Opened on a grant for "watch" naming the session, like every other service
the control plane asks for: the agent does not take a session id from the
tunnel on trust.
*/

// WatchService is the name the control plane asks for.
const WatchService = "watch"

// WatchConfig is what the watch service needs.
type WatchConfig struct {
	Collector *Collector
	Verifier  *tunnel.GrantVerifier
	Log       *slog.Logger
}

// Frames in a session's hub: a one-byte kind, then the output's bytes or a
// size as cols and rows (uint16 each).
const (
	watchOutput byte = 'o'
	watchResize byte = 'r'
)

func watchResizeFrame(cols, rows int) []byte {
	b := make([]byte, 5)
	b[0] = watchResize
	binary.BigEndian.PutUint16(b[1:3], uint16(min(max(cols, 1), 1<<16-1)))
	binary.BigEndian.PutUint16(b[3:5], uint16(min(max(rows, 1), 1<<16-1)))
	return b
}

// watchTap is the recorder tap for one session: it turns each committed line
// into a frame for the session's viewers. Keystrokes and kernel executions
// are not shown; the host echoes what is typed, so the output carries it.
func (s *activeSession) watchTap(line []byte) {
	stream, data, ok := parseEvent(line)
	if !ok {
		return
	}
	switch stream {
	case 'o':
		s.hub.Broadcast(append([]byte{watchOutput}, data...))
	case 'r':
		var cols, rows int
		if n, _ := parseSize(data, &cols, &rows); n != 2 || cols <= 0 || rows <= 0 {
			return
		}
		s.mu.Lock()
		s.cols, s.rows = cols, rows
		s.mu.Unlock()
		s.hub.Broadcast(watchResizeFrame(cols, rows))
	}
}

// size is the terminal's size now: what a viewer arriving must draw at.
func (s *activeSession) size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// parseEvent reads an asciicast event line, [time,"s","data"], as the
// recorder writes it. Only output and resize lines have their data decoded;
// the rest are reported by stream alone, so a busy session's keystrokes and
// executions cost a scan, not a decode.
func parseEvent(line []byte) (stream byte, data []byte, ok bool) {
	if len(line) == 0 || line[0] != '[' {
		return 0, nil, false
	}
	i := bytes.IndexByte(line, ',')
	if i < 0 || len(line) < i+6 || line[i+1] != '"' || line[i+3] != '"' || line[i+4] != ',' {
		return 0, nil, false
	}
	stream = line[i+2]
	if stream != 'o' && stream != 'r' {
		return stream, nil, true
	}
	rest := line[i+5:]
	if len(rest) < 3 || rest[len(rest)-1] != ']' {
		return 0, nil, false
	}
	var s string
	if json.Unmarshal(rest[:len(rest)-1], &s) != nil {
		return 0, nil, false
	}
	return stream, []byte(s), true
}

// parseSize parses "COLSxROWS" without fmt's reflection, reporting how
// many of the two it read.
func parseSize(b []byte, cols, rows *int) (int, error) {
	c, r, found := bytes.Cut(b, []byte{'x'})
	if !found {
		return 0, nil
	}
	n := 0
	if v, ok := atoi(c); ok {
		*cols = v
		n++
	}
	if v, ok := atoi(r); ok {
		*rows = v
		n++
	}
	return n, nil
}

func atoi(b []byte) (int, bool) {
	if len(b) == 0 || len(b) > 5 {
		return 0, false
	}
	v := 0
	for _, ch := range b {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		v = v*10 + int(ch-'0')
	}
	return v, true
}

// watching looks up a session being recorded now.
func (c *Collector) watching(id string) (*activeSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.active[id]
	return s, ok && s.hub != nil
}

// endedKept bounds how long, and how many, ended sessions are remembered
// for telling a late viewer that one ended.
const (
	endedKept    = 15 * time.Minute
	endedKeptMax = 512
)

// noteEnded remembers that id ended at t. Called with c.mu held.
func (c *Collector) noteEnded(id string, t time.Time) {
	if c.ended == nil {
		c.ended = map[string]time.Time{}
	}
	if len(c.ended) >= endedKeptMax {
		for k, at := range c.ended {
			if t.Sub(at) > endedKept {
				delete(c.ended, k)
			}
		}
		// Still full: forget an arbitrary one rather than grow.
		for k := range c.ended {
			if len(c.ended) < endedKeptMax {
				break
			}
			delete(c.ended, k)
		}
	}
	c.ended[id] = t
}

// continuesFrom is the ids m carries on from. A shim from before Continues
// said so only in its origin reason, and a login that outlived an agent
// update is running exactly that older shim.
func continuesFrom(m SessionStart) []string {
	if len(m.Continues) > 0 {
		return m.Continues
	}
	const prefix = "continues session "
	if rest, ok := strings.CutPrefix(m.OriginReason, prefix); ok {
		if id, _, _ := strings.Cut(rest, " "); id != "" {
			return []string{id}
		}
	}
	return nil
}

// Why a watch finds nothing to show, as the viewer is told.
const (
	watchEnded = "the session has ended on this server"
	// watchUnknown is a session id this agent has no record of. The one
	// way that happens to a login that is still running is the agent
	// restarting under it, which records the rest as a new session; a
	// shim that could not reach the new agent in time is not recorded
	// at all, and says so on the person's terminal.
	watchUnknown = "the session is not running on this server and this agent has no " +
		"record of it ending. The agent has been running since %s; if the session " +
		"began before that, the agent restarted during it, and the rest of the " +
		"login is recorded as a new session or, if it could not reconnect, not at all"
	watchOtherAccount = "the session with this id on this server is not running as the " +
		"account the console has on record for it"
)

// watchable finds what a viewer asking for id, as principal, is shown: the
// session itself while it runs, or the session that carries it on after an
// agent restart, which is the same login. why says what was found instead
// when there is nothing to show.
func (c *Collector) watchable(id, principal string) (s *activeSession, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.active[id]; ok && s.hub != nil {
		if s.start.Principal != principal {
			return nil, watchOtherAccount
		}
		return s, ""
	}
	for _, s := range c.active {
		if s.hub == nil || s.start.Principal != principal {
			continue
		}
		if slices.Contains(continuesFrom(s.start), id) {
			return s, ""
		}
	}
	if at, ok := c.ended[id]; ok {
		return nil, watchEnded + " (" + time.Since(at).Round(time.Second).String() + " ago)"
	}
	return nil, fmt.Sprintf(watchUnknown, c.startedAt.UTC().Format("2006-01-02 15:04 MST"))
}

// Watch returns the watch service.
func Watch(cfg WatchConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a watch", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(WatchService) || g.Session == "" {
			log.Warn("tunnel: refused a watch on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for watching a session"}.Error())
			return
		}
		sess, why := cfg.Collector.watchable(g.Session, g.Principal)
		if sess == nil {
			log.Info("tunnel: nothing to watch", "session", g.Session, "viewer", g.Email, "why", why)
			call.Refuse(why)
			return
		}
		if call.Accept(g.Session) != nil {
			_ = call.Stream.Close()
			return
		}
		if sess.id != g.Session {
			log.Info("tunnel: session watched through the session that continues it",
				"session", g.Session, "continued_as", sess.id, "viewer", g.Email)
		}
		log.Info("tunnel: session watched", "session", sess.id, "viewer", g.Email)
		serveWatch(ctx, call.Stream, cfg.Collector, sess)
		_ = call.Stream.Close()
		log.Info("tunnel: stopped watching", "session", g.Session, "viewer", g.Email)
	}
}

// serveWatch sends a session's frames until it ends, the viewer goes, or the
// viewer falls too far behind. The session's own output is never held up:
// the hub drops a slow viewer instead.
func serveWatch(ctx context.Context, st tunnel.Stream, c *Collector, sess *activeSession) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Nothing the control plane sends is for the session; reading only
	// notices that the viewer has gone.
	go func() {
		defer cancel()
		fr := tunnel.NewFrameReader(st)
		for {
			if _, _, err := fr.Next(); err != nil {
				return
			}
		}
	}()

	backlog, frames, unsubscribe := sess.hub.Subscribe()
	defer unsubscribe()

	write := func(f []byte) error {
		if len(f) == 0 {
			return nil
		}
		_ = st.SetDeadline(time.Now().Add(30 * time.Second))
		switch f[0] {
		case watchOutput:
			// The hub holds whatever one write produced; a frame is bounded.
			for p := f[1:]; len(p) > 0; {
				n := min(len(p), 32<<10)
				if err := tunnel.WriteFrame(st, tunnel.FrameOutput, p[:n]); err != nil {
					return err
				}
				p = p[n:]
			}
		case watchResize:
			if len(f) == 5 {
				return tunnel.WriteFrame(st, tunnel.FrameResize, f[1:])
			}
		}
		return nil
	}

	cols, rows := sess.size()
	if write(watchResizeFrame(cols, rows)) != nil {
		return
	}
	for _, f := range backlog {
		if write(f) != nil {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-frames:
			if !ok {
				if _, still := c.watching(sess.id); still {
					// Let go for falling behind. Closing without an exit
					// tells the control plane it was this view, not the
					// session, that stopped.
					return
				}
				_ = st.SetDeadline(time.Now().Add(10 * time.Second))
				_ = tunnel.WriteFrame(st, tunnel.FrameExit, tunnel.ExitPayload(0, sess.rec.Head()))
				return
			}
			if write(f) != nil {
				return
			}
		}
	}
}

package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

// LivenessInterval is how often a gateway re-reports the sessions it is holding.
//
// A minute, matching PolicySyncInterval. At the published ceiling of 100
// concurrent sessions per gateway that is under two requests a second, and the
// report is the same one the session already sends when it opens.
//
// control.SessionSilenceThreshold is three of these. The two are a protocol
// between separate binaries rather than a shared symbol, so shortening one
// without the other is what a reviewer has to catch.
const LivenessInterval = time.Minute

// ReportLiveSessions keeps the control plane's picture of this gateway current.
//
// Blocks until ctx is done, so callers run it in a goroutine.
//
// Fifteen sessions in the dev control plane have been `active` since early
// September, the oldest for nineteen days, because a gateway that is killed
// rather than drained never reports the end. Nothing could reap them either: a
// gateway carries no identity the control plane could attribute orphans to, and
// with no maximum session duration, age alone cannot tell a dead session from a
// long-running one.
//
// So this does not reap. It gives the control plane the one fact it was
// missing — when each live session was last heard about — and lets the reader
// draw the line. That is already how agents work: `stale` means "stopped
// reporting", the console says how long ago, and silence is treated as hostile
// until proven otherwise.
//
// Re-reporting the whole record rather than a bare ping is deliberate. The
// upsert takes the latest risk flags and the greatest byte count, so a session
// that has been running for an hour stops being described by whatever was true
// in its first second.
//
// rdpSrv may be nil; the native RDP proxy is only started when it is
// configured. Every registry that can report a session as `active` has to be
// covered here or its sessions are the ones that go quiet: a desktop session
// left out of the keepalive would be marked unknown three minutes in while
// somebody was still looking at it.
func ReportLiveSessions(ctx context.Context, srv *Server, rdpSrv *RDPServer, log *slog.Logger) {
	if srv == nil || srv.cfg.Reporter == nil || !srv.cfg.Reporter.Enabled() {
		return
	}
	if log == nil {
		log = slog.Default()
	}

	t := time.NewTicker(LivenessInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			log.Debug("reported live sessions", "count", reportLiveOnce(srv, rdpSrv))
		}
	}
}

// reportLiveOnce re-reports every session this gateway is holding, and returns
// how many. Separated from the ticker so a test can drive one round.
//
// Deregistration happens before the closing report on all three paths, so a
// session this walks is one no close has begun for. Without that ordering a
// keepalive could land after a close and take the row back to `active` while
// its end time stood -- the exact contradiction the previous change outlawed.
func reportLiveOnce(srv *Server, rdpSrv *RDPServer) int {
	n := 0

	// "active" and no chain head throughout: this says the session is still
	// here, not that it ended. The report helpers only write an end time for a
	// terminal state, so a keepalive cannot close one.
	for _, sess := range srv.ActiveSessions() {
		sess.report("", "active")
		n++
	}
	// Browser RDP is tracked on the Server rather than the RDP proxy.
	for _, sess := range srv.activeRDPWeb() {
		srv.reportRDPWeb(sess, "", "active")
		n++
	}
	if rdpSrv != nil {
		for _, sess := range rdpSrv.ActiveSessions() {
			rdpSrv.report(sess, "", "active")
			n++
		}
	}
	return n
}

// activeRDPWeb snapshots the browser RDP sessions this gateway is holding.
func (s *Server) activeRDPWeb() []*rdp.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*rdp.Session, 0, len(s.rdpWeb))
	for _, sess := range s.rdpWeb {
		out = append(out, sess)
	}
	return out
}

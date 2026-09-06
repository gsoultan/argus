package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/gsoultan/argus/internal/reporter"
)

// PolicySyncInterval is how often the gateway re-reads policy.
//
// A minute, not a second: policy changes are rare and deliberate, and a
// tighter loop would put a request on the control plane per gateway per second
// to observe nothing. A change therefore takes up to a minute to reach every
// gateway, which is stated in the console rather than hidden.
const PolicySyncInterval = time.Minute

// SyncPolicy keeps a holder current with the control plane.
//
// Blocks until ctx is done, so callers run it in a goroutine. On any failure
// the holder keeps the policy it already had: a control plane that is
// unreachable must not be able to change what this gateway permits, in either
// direction. Losing contact should never open a channel, and it should never
// close one an owner deliberately opened either — that would make an outage
// look like a policy change to whoever is on the far end.
func SyncPolicy(
	ctx context.Context, h *PolicyHolder, rep *reporter.Client, log *slog.Logger,
) {
	if h == nil || rep == nil || !rep.Enabled() {
		return
	}
	if log == nil {
		log = slog.Default()
	}

	fetch := func() {
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		got, err := rep.GatewayPolicy(reqCtx)
		if err != nil {
			log.Warn("gateway policy fetch failed — keeping the policy in force",
				"error", err)
			return
		}
		next := Policy(*got)
		if next == h.Get() {
			return
		}
		// Logged at warn because a policy change is exactly the event someone
		// reconstructing an incident needs to find, and it happens on the
		// gateway rather than where the operator pressed the button.
		log.Warn("gateway policy changed",
			"localForward", next.AllowLocalForward,
			"remoteForward", next.AllowRemoteForward,
			"agentForward", next.AllowAgentForward,
			"x11Forward", next.AllowX11Forward,
			"sftpProxy", next.ProxySftpSubsystem,
			"failClosedOnRecordingLoss", next.FailClosedOnRecordingLoss)
		h.Set(next)
	}

	fetch() // once at startup, so the first session does not wait a minute
	t := time.NewTicker(PolicySyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fetch()
		}
	}
}

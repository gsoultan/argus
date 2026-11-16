package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Live usage for the console: while a person has a server's page open, the
control plane holds one "usage.live" stream here and this samples on it
every couple of seconds (tunnel/usage.go).

Its own sampler, so the heartbeat's once-a-minute rates are untouched: a
rate is "since the previous sample", and sharing one sampler would make the
heartbeat's processor and traffic figures cover the last two seconds instead
of the last minute. Sampling stops the moment the stream closes; nothing is
read or kept while nobody is looking.
*/

// UsageLiveService is the name the control plane asks for.
const UsageLiveService = tunnel.UsageLiveService

// maxUsageLive bounds the live streams one agent serves at once. The
// control plane opens one per server per replica, so a handful is plenty;
// more is something asking for samples it does not need.
const maxUsageLive = 4

// usageLiveStall is how long a sample may wait to be taken by the control
// plane before the stream is given up.
const usageLiveStall = 15 * time.Second

// UsageLiveConfig is what the service needs.
type UsageLiveConfig struct {
	Log *slog.Logger
	// Sample takes one sample with the sampler it is given; nil is the
	// platform's. Tests replace it.
	Sample func(*UsageSampler) (Usage, bool)
}

// UsageLive returns the usage.live service.
func UsageLive(cfg UsageLiveConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	sample := cfg.Sample
	if sample == nil {
		sample = (*UsageSampler).Sample
	}
	var open atomic.Int32
	return func(ctx context.Context, call *Call) {
		if open.Add(1) > maxUsageLive {
			open.Add(-1)
			call.Refuse("too many live usage streams are open on this agent")
			return
		}
		defer open.Add(-1)
		if call.Accept("") != nil {
			_ = call.Stream.Close()
			return
		}
		serveUsageLive(ctx, call.Stream, tunnel.UsagePace(call.Header.UsageEveryMS), sample, log)
	}
}

func serveUsageLive(ctx context.Context, st tunnel.Stream, every time.Duration,
	sample func(*UsageSampler) (Usage, bool), log *slog.Logger) {
	defer st.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The control plane sends nothing after the header: a read returning
	// is the stream closing, which is how sampling stops.
	go func() {
		_, _ = io.Copy(io.Discard, st)
		cancel()
	}()

	var s UsageSampler
	// The first sample has nothing to compare against, so its rates are
	// empty; take it now so the first one sent a moment later has them.
	sample(&s)
	_ = st.SetDeadline(time.Now().Add(every + usageLiveStall))
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		u, ok := sample(&s)
		if !ok {
			continue
		}
		b, err := json.Marshal(u)
		if err != nil || len(b) > tunnel.MaxFrame {
			continue
		}
		// One deadline for both directions, pushed out every tick: the
		// reader above never trips it while samples flow, and a control
		// plane that stopped reading ends the stream rather than holding
		// a write here for ever.
		_ = st.SetDeadline(time.Now().Add(every + usageLiveStall))
		if err := tunnel.WriteFrame(st, tunnel.FrameUsage, b); err != nil {
			log.Debug("tunnel: live usage stream ended", "error", err)
			return
		}
	}
}

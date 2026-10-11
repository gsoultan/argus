package agent

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// TunnelService handles one stream the control plane opened. It owns the
// stream and closes it.
type TunnelService func(ctx context.Context, st tunnel.Stream)

// TunnelConfig is the agent's side of the tunnel.
type TunnelConfig struct {
	Dial tunnel.DialOptions
	// Services maps the names the control plane may ask for to what they
	// do on this host. A name not in this map is refused: the control plane
	// asks for a service, it never names an address or a command.
	Services map[string]TunnelService
	Log      *slog.Logger
	// MaxStreams bounds concurrent streams. Zero means 64.
	MaxStreams int
}

// quicRetryAfter is how long the agent stays on the HTTPS fallback after
// QUIC failed, before trying QUIC again. UDP that was blocked a minute ago is
// almost always still blocked; checking every reconnect would put a
// handshake timeout in front of every one of them.
const quicRetryAfter = 10 * time.Minute

// RunTunnel holds the agent's connection to the control plane until ctx ends,
// reconnecting with backoff.
func RunTunnel(ctx context.Context, cfg TunnelConfig) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 64
	}

	var (
		quicPort    int
		quicFailed  time.Time
		backoff     = time.Second
		lastRefusal string
	)
	for ctx.Err() == nil {
		var (
			sess tunnel.Session
			err  error
		)
		if quicPort > 0 && time.Since(quicFailed) > quicRetryAfter {
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			sess, err = tunnel.DialQUIC(dctx, cfg.Dial, quicPort)
			cancel()
			if err != nil && !tunnel.IsRefused(err) {
				log.Info("tunnel: QUIC unreachable, using HTTPS",
					"error", err,
					"detail", "UDP to the control plane looks blocked; sessions work over "+
						"HTTPS but are less smooth on a lossy network")
				quicFailed = time.Now()
			}
		}
		if sess == nil && (err == nil || !tunnel.IsRefused(err)) {
			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			var port int
			sess, port, err = tunnel.DialUpgrade(dctx, cfg.Dial)
			cancel()
			if err == nil {
				if port != quicPort && port > 0 {
					// Offered for the first time, or moved: try it next.
					quicFailed = time.Time{}
				}
				quicPort = port
			}
		}

		if err != nil {
			if tunnel.IsRefused(err) {
				// Said once per distinct reason, not every retry: a revoked
				// host would otherwise log the same line forever.
				if err.Error() != lastRefusal {
					log.Error("tunnel: the control plane refused this agent",
						"error", err,
						"detail", "check the agent is enrolled and not revoked")
					lastRefusal = err.Error()
				}
				backoff = 5 * time.Minute
			} else {
				log.Warn("tunnel: cannot reach the control plane", "error", err,
					"retry_in", backoff.Round(time.Second))
			}
			if !sleep(ctx, jitter(backoff)) {
				return
			}
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}

		lastRefusal = ""
		backoff = time.Second
		for sess != nil {
			sess = hold(ctx, sess, cfg, log, quicPort, quicFailed)
		}
		// A session that lasted is not a failure to back off from.
		if !sleep(ctx, jitter(time.Second)) {
			return
		}
	}
}

// hold serves one session until it ends. On the HTTPS fallback it also tries
// QUIC in the background; when that works it returns the QUIC session for the
// caller to serve next, without a reconnect in between. The control plane
// replaces the old session with the new one as soon as the new one says hello.
func hold(ctx context.Context, sess tunnel.Session, cfg TunnelConfig, log *slog.Logger,
	quicPort int, quicFailed time.Time) tunnel.Session {

	started := time.Now()
	log.Info("tunnel: connected", "transport", sess.Transport(), "remote", sess.RemoteAddr())

	moved := make(chan tunnel.Session, 1)
	tryCtx, stopTrying := context.WithCancel(ctx)
	defer stopTrying()
	if sess.Transport() == tunnel.TransportUpgrade && quicPort > 0 &&
		time.Since(quicFailed) > quicRetryAfter {
		go func() {
			dctx, cancel := context.WithTimeout(tryCtx, 5*time.Second)
			defer cancel()
			q, err := tunnel.DialQUIC(dctx, cfg.Dial, quicPort)
			if err != nil {
				return
			}
			if tryCtx.Err() != nil {
				_ = q.Close()
				return
			}
			moved <- q
			_ = sess.Close()
		}()
	}

	serveTunnel(ctx, sess, cfg, log)
	_ = sess.Close()
	stopTrying()

	select {
	case q := <-moved:
		log.Info("tunnel: moved to QUIC")
		return q
	default:
	}
	log.Info("tunnel: disconnected", "transport", sess.Transport(),
		"after", time.Since(started).Round(time.Second))
	return nil
}

// serveTunnel accepts streams until the session ends.
func serveTunnel(ctx context.Context, sess tunnel.Session, cfg TunnelConfig, log *slog.Logger) {
	sem := make(chan struct{}, cfg.MaxStreams)
	for {
		st, err := sess.AcceptStream(ctx)
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			// Over the bound: refuse rather than queue, so a flood of opens
			// cannot pin memory or goroutines on the host.
			_ = st.SetDeadline(time.Now().Add(tunnel.HeaderTimeout))
			_ = tunnel.WriteJSON(st, tunnel.Answer{Error: "too many streams"})
			_ = st.Close()
			continue
		}
		go func() {
			defer func() { <-sem }()
			handleStream(ctx, st, cfg.Services, log)
		}()
	}
}

func handleStream(ctx context.Context, st tunnel.Stream, services map[string]TunnelService, log *slog.Logger) {
	_ = st.SetDeadline(time.Now().Add(tunnel.HeaderTimeout))
	var h tunnel.Header
	if err := tunnel.ReadJSON(st, &h); err != nil {
		_ = st.Close()
		return
	}
	svc, ok := services[h.Service]
	if !ok {
		log.Warn("tunnel: refused a stream for a service this agent does not offer",
			"service", h.Service)
		_ = tunnel.WriteJSON(st, tunnel.Answer{Error: "unknown service"})
		_ = st.Close()
		return
	}
	if err := tunnel.WriteJSON(st, tunnel.Answer{OK: true}); err != nil {
		_ = st.Close()
		return
	}
	_ = st.SetDeadline(time.Time{})
	svc(ctx, st)
}

// DefaultTunnelServices is what every agent offers: a ping, so the control
// plane can measure the path. Sessions arrive as further services.
func DefaultTunnelServices() map[string]TunnelService {
	return map[string]TunnelService{
		"ping": func(_ context.Context, st tunnel.Stream) { tunnel.ServePing(st) },
	}
}

func jitter(d time.Duration) time.Duration {
	// ±20%, so a fleet that lost the control plane together does not come
	// back in one burst.
	return d + time.Duration((rand.Float64()*0.4-0.2)*float64(d))
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// TunnelService handles one stream the control plane opened. It answers
// the call -- Accept or Refuse -- before using the stream, and owns the
// stream from then on.
type TunnelService func(ctx context.Context, call *Call)

// Call is one stream the control plane opened, with its header.
type Call struct {
	Header tunnel.Header
	Stream tunnel.Stream
	// GrantKey is the control plane's grant key for this connection, after
	// the pin was checked; nil when it could not be trusted, with why.
	GrantKey    ed25519.PublicKey
	GrantKeyErr error

	answered bool
}

// Accept tells the control plane the service started; session names its
// recording, when it has one.
func (c *Call) Accept(session string) error {
	c.answered = true
	if err := tunnel.WriteJSON(c.Stream, tunnel.Answer{OK: true, Session: session}); err != nil {
		return err
	}
	return c.Stream.SetDeadline(time.Time{})
}

// Refuse tells the control plane why not, and closes the stream.
func (c *Call) Refuse(reason string) {
	c.answered = true
	_ = tunnel.WriteJSON(c.Stream, tunnel.Answer{Error: reason})
	_ = c.Stream.Close()
}

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
	// GrantPin holds the control plane's grant key across connections.
	// Nil trusts whatever key each connection brings, which only a test
	// should do.
	GrantPin *GrantPin
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

	key, keyErr := grantKeyFor(sess, cfg.GrantPin)
	if keyErr != nil {
		log.Error("tunnel: sessions through Argus are refused on this host",
			"error", keyErr)
	}
	serveTunnel(ctx, sess, cfg, log, key, keyErr)
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
func serveTunnel(ctx context.Context, sess tunnel.Session, cfg TunnelConfig, log *slog.Logger,
	key ed25519.PublicKey, keyErr error) {
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
			_ = tunnel.WriteJSON(st, tunnel.Answer{Error: fmt.Sprintf(
				"this server already has %d sessions and file transfers open through "+
					"Argus, the most its agent takes at once; close one and try again",
				cfg.MaxStreams)})
			_ = st.Close()
			continue
		}
		go func() {
			defer func() { <-sem }()
			handleStream(ctx, &Call{Stream: st, GrantKey: key, GrantKeyErr: keyErr},
				cfg.Services, log)
		}()
	}
}

func handleStream(ctx context.Context, call *Call, services map[string]TunnelService, log *slog.Logger) {
	st := call.Stream
	_ = st.SetDeadline(time.Now().Add(tunnel.HeaderTimeout))
	if err := tunnel.ReadJSON(st, &call.Header); err != nil {
		_ = st.Close()
		return
	}
	svc, ok := services[call.Header.Service]
	if !ok {
		log.Warn("tunnel: refused a stream for a service this agent does not offer",
			"service", call.Header.Service)
		call.Refuse("unknown service")
		return
	}
	svc(ctx, call)
	if !call.answered {
		// A service that returned without answering is a refusal it did
		// not explain; the control plane must not be left waiting.
		call.Refuse("the service did not start")
	}
}

// DefaultTunnelServices is what every agent offers: a ping, so the control
// plane can measure the path. Sessions arrive as further services.
func DefaultTunnelServices() map[string]TunnelService {
	return map[string]TunnelService{
		"ping": func(_ context.Context, call *Call) {
			if call.Accept("") != nil {
				_ = call.Stream.Close()
				return
			}
			tunnel.ServePing(call.Stream)
		},
	}
}

// grantKeyFor checks the key a connection brought against the pin.
func grantKeyFor(sess tunnel.Session, pin *GrantPin) (ed25519.PublicKey, error) {
	sent := sess.GrantKey()
	if sent == "" {
		return nil, errors.New("the control plane sent no grant key; it is older than " +
			"this agent or has no signing_secret, so it cannot authorise sessions here")
	}
	key, err := tunnel.DecodeGrantKey(sent)
	if err != nil {
		return nil, err
	}
	if pin == nil {
		return key, nil
	}
	return pin.Check(key)
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

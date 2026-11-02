// Package tlsconfig builds the TLS configurations both ends of Argus share.
//
// This is the client half: what an agent needs to reach the control plane and
// nothing more. The inbound listener, the control plane's own CA and the
// certificate renewal that goes with them live in internal/control/pki, so
// the agent binary carries none of it.
//
// Two things it does that a bare tls.LoadX509KeyPair does not:
//
//   - Certificates are re-read from disk when they change, so renewal does not
//     require a restart. Restarting drops every live session, which turns a
//     routine 90-day renewal into a scheduled outage — and an outage people
//     schedule around is one they eventually skip.
//   - Machine-to-machine links can present a client certificate, so an agent
//     proves who it is rather than presenting a shared bearer token that is
//     identical on every host and never expires.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/secrets"
)

// ClientOptions configures an outbound connection.
type ClientOptions struct {
	// CAFile is the CA that signed the server's certificate. Empty uses the
	// system roots, which is right for a public CA and wrong for an internal one.
	CAFile string

	// CertFile and KeyFile present a client certificate for mTLS.
	CertFile string
	KeyFile  string

	// ServerName overrides the name verified against the certificate. Needed
	// only when connecting by IP to a host whose certificate names it
	// differently.
	ServerName string
}

// Client builds a *tls.Config for an outbound connection.
//
// There is deliberately no option to skip verification. A flag like that gets
// set once during a rushed deployment and never unset, and it silently removes
// the only thing preventing an interception of the link carrying session
// recordings and audit records.
func Client(opts ClientOptions) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: opts.ServerName,
	}

	if opts.CAFile != "" {
		pool, err := LoadCAPool(opts.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}

	if opts.CertFile != "" || opts.KeyFile != "" {
		if opts.CertFile == "" || opts.KeyFile == "" {
			return nil, fmt.Errorf("client cert and key must be given together")
		}
		r := NewReloader(opts.CertFile, opts.KeyFile, nil)
		if _, err := r.Get(); err != nil {
			return nil, err
		}
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return r.Get()
		}
	}

	return cfg, nil
}

// Reloader re-reads a certificate pair when the files change.
//
// Keyed on modification time rather than a timer: a renewal should take effect
// on the next connection, and an unchanged certificate should not be parsed
// on every handshake.
type Reloader struct {
	certFile string
	keyFile  string

	log *slog.Logger

	mu       sync.RWMutex
	cert     *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	loadedAt time.Time
	// refusedKeyMod is the modification time of the key this reloader has
	// already complained about, so a rotation that stays broken produces one
	// line rather than one per connection.
	refusedKeyMod time.Time
}

// NewReloader watches a certificate and key pair. log receives refused
// rotations; nil means the default logger.
func NewReloader(certFile, keyFile string, log *slog.Logger) *Reloader {
	if log == nil {
		log = slog.Default()
	}
	return &Reloader{certFile: certFile, keyFile: keyFile, log: log}
}

// Get returns the current certificate, re-reading the files when they have
// changed and keeping the loaded one when the new files cannot be used.
func (r *Reloader) Get() (*tls.Certificate, error) {
	certMod, keyMod, err := modTimes(r.certFile, r.keyFile)
	if err != nil {
		// Fall back to what is already loaded: a certificate momentarily
		// missing during an atomic replace must not break live connections.
		r.mu.RLock()
		defer r.mu.RUnlock()
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, err
	}

	r.mu.RLock()
	if r.cert != nil && certMod.Equal(r.certMod) && keyMod.Equal(r.keyMod) {
		defer r.mu.RUnlock()
		return r.cert, nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check: another goroutine may have reloaded while we waited.
	if r.cert != nil && certMod.Equal(r.certMod) && keyMod.Equal(r.keyMod) {
		return r.cert, nil
	}

	// The TLS key is checked on every reload, not just at startup: a rotation
	// that drops a world-readable key into place is exactly the case a
	// startup-only check would miss.
	if err := secrets.CheckPrivate(r.keyFile); err != nil {
		// Keep serving the certificate already loaded. The key in memory is
		// the previous one, and nothing about the new file's permissions makes
		// it any less safe -- whereas refusing to serve turns a renewal script
		// that forgot chmod into a total outage of the thing operators use to
		// reach production. The other two failure paths above already work
		// this way; this one taking the listener down was the odd one out, and
		// it is the failure most likely to actually happen.
		if r.cert != nil {
			if !keyMod.Equal(r.refusedKeyMod) {
				r.refusedKeyMod = keyMod
				r.log.Error("refusing to load a rotated TLS key; still serving the previous certificate",
					"key", r.keyFile, "error", err,
					"detail", "fix the permissions and the next connection picks up the new "+
						"certificate; until then this listener is serving one that may expire")
			}
			return r.cert, nil
		}
		// Nothing loaded yet, so there is nothing to fall back to. Startup
		// must fail here rather than serve with an exposed key.
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			// A half-written certificate during renewal must not take the
			// listener down; keep serving the previous one.
			return r.cert, nil
		}
		return nil, fmt.Errorf("load certificate %s: %w", r.certFile, err)
	}

	r.cert = &cert
	r.certMod, r.keyMod, r.loadedAt = certMod, keyMod, time.Now()
	return r.cert, nil
}

func modTimes(certFile, keyFile string) (time.Time, time.Time, error) {
	cs, err := os.Stat(certFile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	ks, err := os.Stat(keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return cs.ModTime(), ks.ModTime(), nil
}

// LoadCAPool reads a PEM bundle into a pool.
func LoadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s contains no usable certificates", path)
	}
	return pool, nil
}

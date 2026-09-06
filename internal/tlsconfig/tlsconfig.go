// Package tlsconfig builds the TLS configurations Argus uses.
//
// Two things it does that a bare tls.LoadX509KeyPair does not:
//
//   - Certificates are re-read from disk when they change, so renewal does not
//     require a restart. Restarting a gateway drops every live session, which
//     turns a routine 90-day renewal into a scheduled outage — and an outage
//     people schedule around is one they eventually skip.
//   - Machine-to-machine links can require a client certificate, so a gateway
//     or agent proves who it is rather than presenting a shared bearer token
//     that is identical on every host and never expires.
package tlsconfig

import (
	"github.com/gsoultan/argus/internal/secrets"

	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"
)

// ServerOptions configures an inbound listener.
type ServerOptions struct {
	CertFile string
	KeyFile  string

	// ClientCAFile enables mutual TLS. When set, a client must present a
	// certificate signed by this CA. Used for the reporter endpoints, where the
	// callers are gateways and agents rather than people.
	ClientCAFile string

	// RequireClientCert makes mTLS mandatory rather than optional. Leave false
	// when the same listener also serves browsers, which have no client cert.
	RequireClientCert bool
}

// Server builds a *tls.Config for an inbound listener.
func Server(opts ServerOptions) (*tls.Config, error) {
	if opts.CertFile == "" || opts.KeyFile == "" {
		return nil, fmt.Errorf("cert_file and key_file are both required")
	}

	r := &reloader{certFile: opts.CertFile, keyFile: opts.KeyFile}
	// Load once up front so a bad path or an unreadable key fails at startup
	// rather than on the first connection, when nobody is watching.
	if _, err := r.get(); err != nil {
		return nil, err
	}

	cfg := &tls.Config{
		// GetCertificate rather than Certificates: this is what allows a
		// renewed certificate to be picked up without a restart.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return r.get()
		},
		// 1.2 floor. 1.0 and 1.1 have no place in a product whose entire
		// purpose is protecting privileged access.
		MinVersion: tls.VersionTLS12,
		// For 1.2 only; 1.3 negotiates its own and ignores this. AEAD suites
		// with forward secrecy, nothing with CBC or RSA key exchange.
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	}

	if opts.ClientCAFile != "" {
		pool, err := loadCAPool(opts.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("client CA: %w", err)
		}
		cfg.ClientCAs = pool
		if opts.RequireClientCert {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			// Verify it when offered, but do not demand one — the same listener
			// serves browsers, which have none.
			cfg.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}

	return cfg, nil
}

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
		pool, err := loadCAPool(opts.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}

	if opts.CertFile != "" || opts.KeyFile != "" {
		if opts.CertFile == "" || opts.KeyFile == "" {
			return nil, fmt.Errorf("client cert and key must be given together")
		}
		r := &reloader{certFile: opts.CertFile, keyFile: opts.KeyFile}
		if _, err := r.get(); err != nil {
			return nil, err
		}
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return r.get()
		}
	}

	return cfg, nil
}

// reloader re-reads a certificate when the files change.
//
// Keyed on modification time rather than a timer: a renewal should take effect
// on the next connection, and an unchanged certificate should not be parsed
// on every handshake.
type reloader struct {
	certFile string
	keyFile  string

	mu       sync.RWMutex
	cert     *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	loadedAt time.Time
}

func (r *reloader) get() (*tls.Certificate, error) {
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

func loadCAPool(path string) (*x509.CertPool, error) {
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

// PeerIdentity returns the common name of a verified client certificate.
//
// Used to attribute reporter calls to a specific gateway or agent, so an audit
// record can say which host reported a session rather than only that something
// holding the shared token did.
func PeerIdentity(state *tls.ConnectionState) string {
	if state == nil || len(state.VerifiedChains) == 0 {
		return ""
	}
	leaf := state.VerifiedChains[0][0]
	if leaf.Subject.CommonName != "" {
		return leaf.Subject.CommonName
	}
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames[0]
	}
	return ""
}

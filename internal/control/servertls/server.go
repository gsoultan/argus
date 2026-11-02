// Package servertls holds the control plane's side of TLS: the inbound listener's
// configuration, the private CA it keeps for itself, and the throwaway
// certificates a laptop runs on.
//
// Only the control plane listens, so only the control plane needs any of this.
// The agent dials out, and the client half it shares with the control plane is
// internal/tlsconfig.
package servertls

import (
	"crypto/tls"
	"fmt"
	"log/slog"

	"github.com/gsoultan/argus/internal/tlsconfig"
)

// ServerOptions configures an inbound listener.
type ServerOptions struct {
	CertFile string
	KeyFile  string

	// ClientCAFile enables mutual TLS. When set, a client must present a
	// certificate signed by this CA. Used for the reporter endpoints, where the
	// callers are agents rather than people.
	ClientCAFile string

	// RequireClientCert makes mTLS mandatory rather than optional. Leave false
	// when the same listener also serves browsers, which have no client cert.
	RequireClientCert bool

	// Log receives rotation failures. Without it a refused rotation is visible
	// only as a per-connection handshake error, which is the quietest possible
	// way to report that the certificate on disk is being ignored.
	Log *slog.Logger
}

// Server builds a *tls.Config for an inbound listener.
func Server(opts ServerOptions) (*tls.Config, error) {
	if opts.CertFile == "" || opts.KeyFile == "" {
		return nil, fmt.Errorf("cert_file and key_file are both required")
	}

	r := tlsconfig.NewReloader(opts.CertFile, opts.KeyFile, opts.Log)
	// Load once up front so a bad path or an unreadable key fails at startup
	// rather than on the first connection, when nobody is watching.
	if _, err := r.Get(); err != nil {
		return nil, err
	}

	cfg := &tls.Config{
		// GetCertificate rather than Certificates: this is what allows a
		// renewed certificate to be picked up without a restart.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return r.Get()
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
		pool, err := tlsconfig.LoadCAPool(opts.ClientCAFile)
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

// PeerIdentity returns the common name of a verified client certificate.
//
// Used to attribute reporter calls to a specific agent, so an audit record can
// say which host reported a session rather than only that something holding
// the shared token did.
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

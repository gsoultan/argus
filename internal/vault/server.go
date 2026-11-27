package vault

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/secrets"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

// Config is argus-secret's configuration.
type Config struct {
	// Listen is where agents and argus-control reach it. Default ":8460".
	Listen  string `yaml:"listen"`
	DataDir string `yaml:"data_dir"`
	// RootKeyFile holds the 32-byte root key. Made on first start when
	// missing; argus-secret refuses to start when others can read it.
	RootKeyFile  string `yaml:"root_key_file"`
	Organization string `yaml:"organization"`
	TLS          struct {
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
	} `yaml:"tls"`
	// BehindTLSProxy is for a cloudflared, nginx or other proxy on this host
	// that ends TLS: argus-secret then listens without TLS, on loopback only.
	BehindTLSProxy bool `yaml:"behind_tls_proxy"`
	// TrustedProxies are the addresses whose forwarding headers are believed.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// ControlPublicKey is argus-control's Ed25519 public key, base64.
	ControlPublicKey string `yaml:"control_public_key"`
	// RulesMaxAge is how long rules are trusted without hearing from
	// argus-control. Default 15 minutes.
	RulesMaxAge time.Duration `yaml:"rules_max_age"`
	// PostQuantumOnly refuses reads and writes sealed in format v1 (X25519
	// only), so every value in transit is sealed with ML-KEM-768 + X25519.
	// Turn it on once every agent and console that calls this server is
	// updated.
	PostQuantumOnly bool `yaml:"post_quantum_only"`
}

// Defaults fills what was left out.
func (c *Config) Defaults() {
	if c.Listen == "" {
		c.Listen = ":8460"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/argus-secret"
	}
	if c.RootKeyFile == "" {
		c.RootKeyFile = filepath.Join(c.DataDir, "root.key")
	}
	if c.RulesMaxAge == 0 {
		c.RulesMaxAge = 15 * time.Minute
	}
}

// Validate refuses a configuration that would start in an unsafe way.
func (c *Config) Validate() error {
	if c.Organization == "" {
		return errors.New("organization is required")
	}
	if _, err := ParseControlKey(c.ControlPublicKey); err != nil {
		return err
	}
	if c.RulesMaxAge < time.Minute || c.RulesMaxAge > 24*time.Hour {
		return errors.New("rules_max_age is between 1m and 24h")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return errors.New("tls.cert_file and tls.key_file are given together")
	}
	if c.BehindTLSProxy && c.Listen != ListenOff {
		if c.TLS.CertFile != "" {
			return errors.New("behind_tls_proxy and tls are exclusive: the proxy ends TLS")
		}
		if !loopbackListen(c.Listen) {
			return fmt.Errorf("behind_tls_proxy listens without TLS, so listen must be a loopback address such as 127.0.0.1:8460, not %q", c.Listen)
		}
	}
	if _, err := ParsePrefixes(c.TrustedProxies); err != nil {
		return err
	}
	return nil
}

// ListenOff, as listen, serves over the tunnel only: no port at all.
const ListenOff = "off"

// ApplyEnrollment fills the organization and argus-control's key from the
// enrollment, when there is one, and refuses a configuration that says
// otherwise. It returns the enrollment, or nil.
func (c *Config) ApplyEnrollment() (*Enrollment, error) {
	e, err := LoadEnrollment(c.DataDir)
	if err != nil || e == nil {
		return nil, err
	}
	if c.Organization == "" {
		c.Organization = e.Org
	} else if c.Organization != e.Org {
		return nil, fmt.Errorf("organization is %q, but this server enrolled into %q", c.Organization, e.Org)
	}
	if c.ControlPublicKey == "" {
		c.ControlPublicKey = e.ControlKey
	} else if c.ControlPublicKey != e.ControlKey {
		return nil, errors.New("control_public_key differs from the key argus-control gave when this server enrolled; remove one")
	}
	return e, nil
}

func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// ParseControlKey reads argus-control's Ed25519 public key: base64 (standard
// or URL, with or without padding), optionally prefixed "ed25519:".
func ParseControlKey(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "ed25519:")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("control_public_key is argus-control's Ed25519 public key in base64; copy it from Settings › Secrets servers")
}

// LoadRootKey reads the root key, making one when the file does not exist.
// created says a new key was made: it must be backed up, because nothing in
// the store can be opened without it.
func LoadRootKey(path string) (key []byte, created bool, err error) {
	raw, err := secrets.ReadPrivate(path)
	if err == nil {
		if len(raw) != KeySize {
			return nil, false, fmt.Errorf("%s is %d bytes; a root key is %d", path, len(raw), KeySize)
		}
		return raw, false, nil
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		return nil, false, err
	}
	key, err = randomKey()
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, false, err
	}
	if err := f.Close(); err != nil {
		return nil, false, err
	}
	return key, true, secrets.MakePrivate(path)
}

// Run serves until ctx ends.
func Run(ctx context.Context, cfg Config, version string, log *slog.Logger) error {
	cfg.Defaults()
	enr, err := cfg.ApplyEnrollment()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Listen == ListenOff && enr == nil {
		return errors.New("listen: off leaves only the tunnel, and this server has not enrolled: run argus-secret enroll first")
	}
	root, created, err := LoadRootKey(cfg.RootKeyFile)
	if err != nil {
		return fmt.Errorf("root key: %w", err)
	}
	if created {
		log.Warn("made a new root key; back it up now, nothing in this store opens without it", "file", cfg.RootKeyFile)
	}
	store, err := Open(cfg.DataDir, root, cfg.Organization)
	clear(root)
	if err != nil {
		return err
	}
	defer store.Close()
	store.RequirePostQuantum(cfg.PostQuantumOnly)
	if err := store.VerifyLog(); err != nil {
		return err
	}
	rules := NewRules(cfg.RulesMaxAge)
	if err := store.LoadRules(rules); err != nil {
		return fmt.Errorf("saved rules: %w", err)
	}
	controlKey, _ := ParseControlKey(cfg.ControlPublicKey)
	trusted, _ := ParsePrefixes(cfg.TrustedProxies)

	var tlsCfg *tls.Config
	pin := ""
	if !cfg.BehindTLSProxy && cfg.Listen != ListenOff {
		tlsCfg, pin, err = serverTLS(cfg)
		if err != nil {
			return err
		}
	}
	api, err := NewAPI(APIOptions{Store: store, Rules: rules, ControlKey: controlKey,
		ClientIP: ClientIP{Trusted: trusted}, Log: log, Version: version, TLSPin: pin})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		housekeeping(ctx, store, log)
	}()
	tunnelDone := make(chan struct{})
	if enr != nil {
		go func() {
			defer close(tunnelDone)
			RunTunnel(ctx, enr, api.Handler(), log)
		}()
	} else {
		close(tunnelDone)
	}
	if cfg.Listen == ListenOff {
		log.Info("argus-secret serving over its tunnel only", "control", enr.ControlURL, "org", cfg.Organization)
		<-ctx.Done()
		<-tunnelDone
		<-done
		return nil
	}
	defer func() { cancel(); <-tunnelDone }()
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		cancel()
		<-done
		return err
	}
	errc := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	log.Info("argus-secret listening", "addr", ln.Addr().String(), "tls", tlsCfg != nil, "org", cfg.Organization, "pin", pin)
	select {
	case <-ctx.Done():
	case err := <-errc:
		cancel()
		<-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
	<-done
	return err
}

// housekeeping purges secrets past their recovery window and prunes log
// entries argus-control already has, once an hour.
func housekeeping(ctx context.Context, store *Store, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if names, err := store.PurgeDue(); err != nil {
			log.Error("purging deleted secrets", "err", err)
		} else if len(names) > 0 {
			log.Info("purged deleted secrets", "count", len(names))
		}
		if _, err := store.PruneLog(time.Now().Add(-30 * 24 * time.Hour)); err != nil {
			log.Error("pruning the read log", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// serverTLS uses the configured certificate, or one argus-secret makes and
// keeps in its data directory. Its key is kept, so its pin survives restarts
// and renewals.
func serverTLS(cfg Config) (*tls.Config, string, error) {
	certFile, keyFile := cfg.TLS.CertFile, cfg.TLS.KeyFile
	if certFile == "" {
		var err error
		certFile, keyFile, err = selfSigned(filepath.Join(cfg.DataDir, "tls"), time.Now())
		if err != nil {
			return nil, "", fmt.Errorf("self-signed certificate: %w", err)
		}
	}
	pin, err := tlsconfig.PinFile(certFile)
	if err != nil {
		return nil, "", err
	}
	if err := secrets.CheckPrivate(keyFile); err != nil {
		return nil, "", err
	}
	r := tlsconfig.NewReloader(certFile, keyFile, nil)
	if _, err := r.Get(); err != nil {
		return nil, "", err
	}
	return &tls.Config{
		MinVersion:     tlsconfig.MinVersion,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return r.Get() },
	}, pin, nil
}

// selfSigned keeps a key in dir and a certificate for it, renewed 30 days
// before it ends.
func selfSigned(dir string, now time.Time) (certFile, keyFile string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	certFile, keyFile = filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	var key *ecdsa.PrivateKey
	if raw, err := secrets.ReadPrivate(keyFile); err == nil {
		block, _ := pem.Decode(raw)
		if block == nil {
			return "", "", fmt.Errorf("%s is not a PEM key", keyFile)
		}
		key, err = x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return "", "", err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", "", err
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return "", "", err
		}
		if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return "", "", err
		}
	} else {
		return "", "", err
	}
	if certGood(certFile, key, now) {
		return certFile, keyFile, nil
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: "argus-secret"},
		DNSNames:     []string{"localhost", host},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(2 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	if err := writeFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

func certGood(path string, key *ecdsa.PrivateKey, now time.Time) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	return ok && pub.Equal(&key.PublicKey) && now.Add(30*24*time.Hour).Before(cert.NotAfter)
}

// writeFile writes by rename, so a reader never sees half a file.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

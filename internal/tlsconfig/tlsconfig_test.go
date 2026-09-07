package tlsconfig

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func devPKI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := GenerateDevPKI(dir, []string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatalf("GenerateDevPKI: %v", err)
	}
	return dir
}

func p(dir, name string) string { return filepath.Join(dir, name) }

// startTLSServer runs a real http.Server over ServeTLS.
//
// Not httptest.StartTLS: it injects its own certificate when Certificates is
// empty, and Go then prefers that over GetCertificate whenever the client
// connects without SNI — which is every connection to an IP address. The test
// would be exercising httptest's certificate rather than ours.
func startTLSServer(t *testing.T, cfg *tls.Config, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, TLSConfig: cfg}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return "https://" + ln.Addr().String()
}

// The whole point: a client that trusts the CA can talk to the server, and one
// that does not cannot.
func TestServerAndClientInteroperate(t *testing.T) {
	dir := devPKI(t)

	serverTLS, err := Server(ServerOptions{
		CertFile: p(dir, "server.crt"), KeyFile: p(dir, "server.key"),
	})
	if err != nil {
		t.Fatalf("Server: %v", err)
	}

	url := startTLSServer(t, serverTLS, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))

	t.Run("trusting the CA succeeds", func(t *testing.T) {
		clientTLS, err := Client(ClientOptions{CAFile: p(dir, "ca.crt")})
		if err != nil {
			t.Fatalf("Client: %v", err)
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
		resp, err := c.Get(url)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "ok" {
			t.Errorf("body = %q, want ok", body)
		}
	})

	// An unknown CA must fail. If this ever passes, verification is off
	// somewhere and every guarantee above it is void.
	t.Run("an untrusted CA is rejected", func(t *testing.T) {
		other := devPKI(t)
		clientTLS, err := Client(ClientOptions{CAFile: p(other, "ca.crt")})
		if err != nil {
			t.Fatalf("Client: %v", err)
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
		if _, err := c.Get(url); err == nil {
			t.Fatal("connected to a server signed by an untrusted CA")
		}
	})
}

func TestClientNeverSkipsVerification(t *testing.T) {
	cfg, err := Client(ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	// There is no option to set this, and there should never be: it gets
	// enabled once during a rushed deployment and silently never unset.
	if cfg.InsecureSkipVerify {
		t.Error("client config skips certificate verification")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want at least TLS 1.2", cfg.MinVersion)
	}
}

func TestServerRefusesObsoleteTLS(t *testing.T) {
	dir := devPKI(t)
	cfg, err := Server(ServerOptions{
		CertFile: p(dir, "server.crt"), KeyFile: p(dir, "server.key"),
	})
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want at least TLS 1.2", cfg.MinVersion)
	}
	for _, suite := range cfg.CipherSuites {
		for _, weak := range tls.InsecureCipherSuites() {
			if suite == weak.ID {
				t.Errorf("cipher suite %s is known-weak", weak.Name)
			}
		}
	}
}

// Certificates are renewed on a schedule. If picking one up needs a restart,
// renewal becomes an outage and eventually gets skipped.
func TestCertificateIsReloadedAfterRenewal(t *testing.T) {
	dir := devPKI(t)
	certFile, keyFile := p(dir, "server.crt"), p(dir, "server.key")

	cfg, err := Server(ServerOptions{CertFile: certFile, KeyFile: keyFile})
	if err != nil {
		t.Fatalf("Server: %v", err)
	}

	first, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	// Renew: a fresh PKI written over the same paths.
	renewed := devPKI(t)
	time.Sleep(10 * time.Millisecond) // ensure a distinct mtime
	for _, name := range []string{"server.crt", "server.key"} {
		data, err := os.ReadFile(p(renewed, name))
		if err != nil {
			t.Fatalf("read renewed %s: %v", name, err)
		}
		if err := os.WriteFile(p(dir, name), data, 0o600); err != nil {
			t.Fatalf("write renewed %s: %v", name, err)
		}
	}

	second, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate after renewal: %v", err)
	}
	if string(first.Certificate[0]) == string(second.Certificate[0]) {
		t.Error("certificate was not reloaded after the files changed")
	}
}

// A renewal that briefly leaves a truncated file must not take the listener
// down; serving the previous certificate is strictly better than refusing.
func TestUnreadableCertificateKeepsServingThePrevious(t *testing.T) {
	dir := devPKI(t)
	certFile, keyFile := p(dir, "server.crt"), p(dir, "server.key")

	cfg, err := Server(ServerOptions{CertFile: certFile, KeyFile: keyFile})
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(certFile, []byte("half-written garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Errorf("a corrupt certificate file took the listener down: %v", err)
	}
}

func TestMissingCertificateFailsAtStartup(t *testing.T) {
	// Better to refuse to start than to fail on the first connection, when
	// nobody is watching the logs.
	if _, err := Server(ServerOptions{
		CertFile: "/nonexistent.crt", KeyFile: "/nonexistent.key",
	}); err == nil {
		t.Error("Server accepted a nonexistent certificate path")
	}
	if _, err := Server(ServerOptions{}); err == nil {
		t.Error("Server accepted empty cert and key paths")
	}
}

// Machine-to-machine links present a client certificate, so a call is
// attributable to a host rather than to whoever holds a shared token.
func TestMutualTLS(t *testing.T) {
	dir := devPKI(t)

	serverTLS, err := Server(ServerOptions{
		CertFile:          p(dir, "server.crt"),
		KeyFile:           p(dir, "server.key"),
		ClientCAFile:      p(dir, "ca.crt"),
		RequireClientCert: true,
	})
	if err != nil {
		t.Fatalf("Server: %v", err)
	}

	var sawIdentity string
	url := startTLSServer(t, serverTLS, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			sawIdentity = PeerIdentity(r.TLS)
			_, _ = w.Write([]byte("ok"))
		}))

	t.Run("with a client certificate", func(t *testing.T) {
		clientTLS, err := Client(ClientOptions{
			CAFile:   p(dir, "ca.crt"),
			CertFile: p(dir, "client.crt"),
			KeyFile:  p(dir, "client.key"),
		})
		if err != nil {
			t.Fatalf("Client: %v", err)
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
		resp, err := c.Get(url)
		if err != nil {
			t.Fatalf("mTLS request failed: %v", err)
		}
		defer resp.Body.Close()
		if sawIdentity != "argus-gateway" {
			t.Errorf("peer identity = %q, want argus-gateway", sawIdentity)
		}
	})

	t.Run("without one it is refused", func(t *testing.T) {
		clientTLS, _ := Client(ClientOptions{CAFile: p(dir, "ca.crt")})
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
		if _, err := c.Get(url); err == nil {
			t.Error("server accepted a client with no certificate despite requiring one")
		}
	})
}

func TestClientCertAndKeyMustBeGivenTogether(t *testing.T) {
	dir := devPKI(t)
	if _, err := Client(ClientOptions{CertFile: p(dir, "client.crt")}); err == nil {
		t.Error("accepted a client certificate with no key")
	}
}

/*
A refused rotation must not become an outage.

Three things can go wrong while a certificate is being replaced: the files
vanish mid-rename, the certificate is half-written, or the new key lands with
the wrong permissions. The first two already fell back to the certificate
already in memory. The third -- the one a renewal script actually causes, by
forgetting chmod -- took the listener down instead, so every operator lost the
browser terminal because a file was 0644.
*/

// writePair puts a self-signed cert and key at the given paths.
func writeTestPair(t *testing.T, certFile, keyFile string, mode os.FileMode) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "argus-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	if err := os.WriteFile(keyFile, keyPEM, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyFile, mode); err != nil {
		t.Fatal(err)
	}
}

func serialOf(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber.String()
}

// A good rotation is picked up without a restart.
func TestRotationIsPickedUpWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "s.crt"), filepath.Join(dir, "s.key")
	writeTestPair(t, certFile, keyFile, 0o600)

	cfg, err := Server(ServerOptions{CertFile: certFile, KeyFile: keyFile,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	first, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// A different modification time is what triggers a reload.
	time.Sleep(10 * time.Millisecond)
	writeTestPair(t, certFile, keyFile, 0o600)

	second, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if serialOf(t, first) == serialOf(t, second) {
		t.Error("the rotated certificate was not picked up")
	}
}

// A rotation whose key is world-readable is refused, and the previous
// certificate keeps serving.
func TestARefusedRotationKeepsServingThePreviousCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "s.crt"), filepath.Join(dir, "s.key")
	writeTestPair(t, certFile, keyFile, 0o600)

	cfg, err := Server(ServerOptions{CertFile: certFile, KeyFile: keyFile,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	good, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	writeTestPair(t, certFile, keyFile, 0o644) // the renewal that forgot chmod

	after, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("a bad rotation took the listener down: %v", err)
	}
	if serialOf(t, after) != serialOf(t, good) {
		t.Error("a world-readable key was loaded; the exposed key must be refused")
	}
}

// With nothing loaded yet there is nothing to fall back to, so start-up must
// still refuse rather than serve with an exposed key.
func TestStartupStillRefusesAnExposedKey(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "s.crt"), filepath.Join(dir, "s.key")
	writeTestPair(t, certFile, keyFile, 0o644)

	if _, err := Server(ServerOptions{CertFile: certFile, KeyFile: keyFile,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
		t.Error("start-up accepted a world-readable private key")
	}
}

// A certificate that vanishes mid-rename must not break live connections.
func TestAMissingFileFallsBackToTheLoadedCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "s.crt"), filepath.Join(dir, "s.key")
	writeTestPair(t, certFile, keyFile, 0o600)

	cfg, err := Server(ServerOptions{CertFile: certFile, KeyFile: keyFile,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	good, _ := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})

	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	after, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("a missing file took the listener down: %v", err)
	}
	if serialOf(t, after) != serialOf(t, good) {
		t.Error("expected the previously loaded certificate")
	}
}

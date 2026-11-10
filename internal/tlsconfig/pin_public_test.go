package tlsconfig

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// server serves TLS on a certificate of its own for argus.test, and returns a
// pool that trusts it, standing in for a public CA.
func server(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "edge"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"argus.test"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return srv, pool
}

// get fetches https://argus.test from srv: by name, as an agent dials the
// control plane, since a name is what a public certificate is checked for.
func get(t *testing.T, cfg *tls.Config, srv *httptest.Server) error {
	t.Helper()
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{TLSClientConfig: cfg,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://argus.test")
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// The control plane's pin, while a server outside the control plane's network
// meets Cloudflare's publicly trusted certificate for the same name. Pinned
// alone refused it, which is why an agent could only be installed from inside.
func TestPinnedOrPublicAcceptsAPublicCertificateForTheName(t *testing.T) {
	edge, public := server(t)
	control, _ := server(t)
	pin := Pin(control.Certificate())

	strict, _ := Pinned(pin)
	if err := get(t, strict, edge); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("Pinned accepted another key: %v", err)
	}
	cfg, err := pinnedOrRoots(pin, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := get(t, cfg, edge); err != nil {
		t.Fatalf("a publicly trusted certificate was refused: %v", err)
	}
}

func TestPinnedOrPublicStillTrustsThePinnedKey(t *testing.T) {
	control, _ := server(t)
	cfg, _ := pinnedOrRoots(Pin(control.Certificate()), x509.NewCertPool())
	if err := get(t, cfg, control); err != nil {
		t.Fatalf("the pinned key was refused: %v", err)
	}
}

// Neither the pinned key nor a certificate anything trusts: an interception.
func TestPinnedOrPublicRefusesAnUntrustedOtherKey(t *testing.T) {
	impostor, _ := server(t)
	control, _ := server(t)
	cfg, _ := pinnedOrRoots(Pin(control.Certificate()), x509.NewCertPool())
	if err := get(t, cfg, impostor); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("an untrusted key was accepted: %v", err)
	}
}

// A certificate trusted for some other name does not stand in for the pin.
func TestPinnedOrPublicChecksTheName(t *testing.T) {
	edge, public := server(t)
	control, _ := server(t)
	cfg, _ := pinnedOrRoots(Pin(control.Certificate()), public)
	cfg.ServerName = "argus.example.com" // what the agent dials, not what edge is for
	if err := get(t, cfg, edge); err == nil {
		t.Fatal("a certificate for another name was accepted")
	}
}

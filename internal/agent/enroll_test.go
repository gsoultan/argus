package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tlsconfig"
)

// fakeControl answers the enrollment route the way argus-control does, handing
// back caPEM as the CA to keep.
func fakeControl(t *testing.T, caPEM func(*httptest.Server) string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/agents/enroll" {
			http.NotFound(w, r)
			return
		}
		var in struct{ Token, Hostname string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Token != "argus-enrol-good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"enrollment token not recognised"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"credential": "argus-agent-secret", "hostname": in.Hostname, "caPem": caPEM(srv),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// otherServer has a key of its own. Every httptest server shares one built-in
// certificate, so two of them are indistinguishable to a pin -- which is the
// thing these tests need to tell apart.
func otherServer(t *testing.T) *httptest.Server {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "impostor"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// The test server's certificate is self-signed, so it is its own CA.
func selfCA(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

func TestEnrollTrustsOnlyThePinnedKey(t *testing.T) {
	srv, _ := fakeControl(t, selfCA)
	other := otherServer(t)
	wrongPin := tlsconfig.Pin(other.Certificate())

	_, err := Enroll(context.Background(), EnrollOptions{
		ControlURL: srv.URL, Token: "argus-enrol-good", Pin: wrongPin,
		Hostname: "web-01", StateDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "not the control plane") {
		t.Fatalf("a server with another key was trusted: %v", err)
	}
}

func TestEnrollKeepsTheCAThatVerifiesTheServer(t *testing.T) {
	srv, _ := fakeControl(t, selfCA)
	dir := t.TempDir()
	enr, err := Enroll(context.Background(), EnrollOptions{
		ControlURL: srv.URL, Token: "argus-enrol-good", Pin: tlsconfig.Pin(srv.Certificate()),
		Hostname: "web-01", StateDir: dir,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if enr.CAFile == "" || enr.Pin != "" {
		t.Fatalf("expected the CA to be kept, got CAFile=%q Pin=%q", enr.CAFile, enr.Pin)
	}
	for _, f := range []string{EnrollmentFile, enrolledCAFile} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v; the credential must be root-only", f, info.Mode().Perm())
		}
	}
	loaded, err := LoadEnrollment(dir)
	if err != nil || loaded == nil || loaded.Credential != "argus-agent-secret" {
		t.Fatalf("LoadEnrollment = %+v, %v", loaded, err)
	}
	// And the kept trust really does reach the server.
	cfg, err := loaded.TLS()
	if err != nil {
		t.Fatalf("TLS: %v", err)
	}
	client := srv.Client()
	client.Transport.(*http.Transport).TLSClientConfig = cfg
	if _, err := client.Get(srv.URL + "/healthz"); err != nil {
		t.Fatalf("the kept CA does not verify the control plane: %v", err)
	}
}

// A CA that does not verify the server would turn into a failed first
// heartbeat with the credential already spent. The pin is kept instead.
func TestEnrollFallsBackToThePinWhenTheCADoesNotVerify(t *testing.T) {
	other := otherServer(t)
	srv, _ := fakeControl(t, func(*httptest.Server) string { return selfCA(other) })
	enr, err := Enroll(context.Background(), EnrollOptions{
		ControlURL: srv.URL, Token: "argus-enrol-good", Pin: tlsconfig.Pin(srv.Certificate()),
		Hostname: "web-01", StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if enr.Pin == "" || enr.CAFile != "" {
		t.Fatalf("kept CAFile=%q Pin=%q; a CA that cannot verify the server was kept", enr.CAFile, enr.Pin)
	}
}

func TestEnrollRefusesPlainHTTP(t *testing.T) {
	_, err := Enroll(context.Background(), EnrollOptions{
		ControlURL: "http://control.example", Token: "argus-enrol-good", StateDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("enrolled over plaintext: %v", err)
	}
}

func TestEnrollingTwiceNeedsForce(t *testing.T) {
	srv, calls := fakeControl(t, selfCA)
	dir := t.TempDir()
	opts := EnrollOptions{
		ControlURL: srv.URL, Token: "argus-enrol-good", Pin: tlsconfig.Pin(srv.Certificate()),
		Hostname: "web-01", StateDir: dir,
	}
	if _, err := Enroll(context.Background(), opts); err != nil {
		t.Fatalf("first: %v", err)
	}
	before := *calls
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("a second enrollment silently replaced the first")
	}
	if *calls != before {
		t.Error("the refusal spent a token: it should be decided before asking")
	}
	opts.Force = true
	if _, err := Enroll(context.Background(), opts); err != nil {
		t.Fatalf("with --force: %v", err)
	}
}

func TestARefusalSaysWhy(t *testing.T) {
	srv, _ := fakeControl(t, selfCA)
	_, err := Enroll(context.Background(), EnrollOptions{
		ControlURL: srv.URL, Token: "argus-enrol-bad", Pin: tlsconfig.Pin(srv.Certificate()),
		Hostname: "web-01", StateDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "not recognised") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotEnrolledIsNotAnError(t *testing.T) {
	e, err := LoadEnrollment(t.TempDir())
	if e != nil || err != nil {
		t.Fatalf("LoadEnrollment on an empty dir = %v, %v", e, err)
	}
}

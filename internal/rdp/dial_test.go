package rdp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func fakeTarget(t *testing.T, protocol uint32) (addr string, fingerprint string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "win-01"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"win-01"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := ReadPDU(raw); err != nil {
					return
				}
				if _, err := raw.Write(BuildConnectionConfirm(protocol)); err != nil {
					return
				}
				srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := srv.Handshake(); err != nil {
					return
				}
				// Echo whatever arrives, so a relay test has something to see.
				_, _ = io.Copy(srv, srv)
			}()
		}
	}()

	return ln.Addr().String(), CertFingerprint(der)
}

// stubPinner records what it was asked and answers with a fixed verdict.
type stubPinner struct {
	expect string
	err    error
	seen   string
}

func (p *stubPinner) VerifyFingerprint(host, presented, keyType string) error {
	p.seen = presented
	if p.err != nil {
		return p.err
	}
	if p.expect != "" && presented != p.expect {
		return errors.New("fingerprint mismatch")
	}
	return nil
}

func TestDialTargetPinsTheCertificate(t *testing.T) {
	addr, fingerprint := fakeTarget(t, ProtocolSSL)

	pins := &stubPinner{expect: fingerprint}
	conn, err := DialTarget(addr, "win-01", ProtocolSSL, pins, 5*time.Second)
	if err != nil {
		t.Fatalf("DialTarget: %v", err)
	}
	defer conn.Close()

	if pins.seen != fingerprint {
		t.Errorf("pinned %q, target presented %q", pins.seen, fingerprint)
	}
	if !strings.HasPrefix(pins.seen, "SHA256:") {
		t.Errorf("fingerprint %q is not in the form the trust store uses", pins.seen)
	}
}

// A changed certificate is the RDP equivalent of a changed host key: it is the
// signature of an interception, and the connection must not proceed.
func TestDialTargetRefusesAnUnpinnedCertificate(t *testing.T) {
	addr, _ := fakeTarget(t, ProtocolSSL)

	pins := &stubPinner{err: errors.New("host key mismatch")}
	conn, err := DialTarget(addr, "win-01", ProtocolSSL, pins, 5*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("DialTarget proceeded despite a refused pin")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error does not name the reason: %v", err)
	}
}

// A target that answers with standard RDP security must be refused even though
// the client asked for something stronger — otherwise the far half of the
// session is unauthenticated while the near half looks fine.
func TestDialTargetRefusesAWeakTarget(t *testing.T) {
	addr, _ := fakeTarget(t, ProtocolRDP)

	conn, err := DialTarget(addr, "win-01", ProtocolSSL, &stubPinner{}, 5*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("DialTarget accepted a target that selected standard RDP security")
	}
	if !strings.Contains(err.Error(), "standard RDP security") {
		t.Errorf("error does not name the reason: %v", err)
	}
}

// `auth == nil` short-circuited before the check on what was actually
// negotiated, so this returned a TLS connection that had negotiated NLA and
// then skipped it -- reported as success, with a nil CredSSP result the caller
// had no reason to inspect. Connect's contract is that everything establishing
// trust has already run; here nothing had.
//
// Found by pointing the opt-in integration test at a real Windows host and
// getting a usable connection back.
func TestDialTargetRefusesNLAWithNoAuthenticator(t *testing.T) {
	addr, _ := fakeTarget(t, ProtocolHybrid)

	conn, result, err := DialTargetWithAuth(
		addr, "win-01", ProtocolHybrid, &stubPinner{}, 5*time.Second, nil)
	if err == nil {
		conn.Close()
		t.Fatalf("accepted a target that selected NLA with no authenticator "+
			"(credssp result %+v); nothing authenticated", result)
	}
	if !strings.Contains(err.Error(), "no authenticator") {
		t.Errorf("error does not name the reason: %v", err)
	}
	// And the socket is not left behind for a caller that ignored the error.
	if conn != nil {
		t.Error("returned a connection alongside the error")
	}
}

// The short-circuit is still right when the target did not choose NLA: a
// TLS-only session has nothing for an authenticator to do.
func TestDialTargetAllowsNoAuthenticatorWithoutNLA(t *testing.T) {
	addr, _ := fakeTarget(t, ProtocolSSL)

	conn, _, err := DialTargetWithAuth(
		addr, "win-01", ProtocolSSL, &stubPinner{}, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("a TLS-only target needs no authenticator: %v", err)
	}
	conn.Close()
}

func TestTheBindingCoversTheKeyNotItsAlgorithm(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := subjectPublicKey(spki)
	if err != nil {
		t.Fatal(err)
	}
	if want := x509.MarshalPKCS1PublicKey(&key.PublicKey); !bytes.Equal(got, want) {
		t.Fatal("the binding is not over the RSA public key itself")
	}
	if _, err := subjectPublicKey(append(spki, 0)); err == nil {
		t.Fatal("trailing bytes after the key were accepted")
	}
}

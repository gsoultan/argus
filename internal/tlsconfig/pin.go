package tlsconfig

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// pinPrefix names the digest, so a pin can change algorithm without a pin of
// one kind ever being compared against a digest of another.
const pinPrefix = "sha256:"

// Pin is the SHA-256 of a certificate's public key, as "sha256:<hex>".
//
// The public key rather than the whole certificate: a renewal that keeps the
// key keeps the pin, and the pin only has to survive the hour an enrollment
// token lives.
func Pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return pinPrefix + hex.EncodeToString(sum[:])
}

// PinFile pins the first certificate in a PEM file: the leaf, in a chain file.
func PinFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s: no certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return Pin(cert), nil
}

// Pinned builds a client configuration that trusts exactly one public key.
//
// For one request: an agent enrolling, which has a pin from the token it was
// handed and nothing else yet. Chain verification is replaced, not skipped --
// the connection is refused unless the server's own key is the pinned one,
// which is a narrower trust than any CA gives. There is no way to build this
// with an empty pin.
func Pinned(pin string) (*tls.Config, error) {
	if !strings.HasPrefix(pin, pinPrefix) || len(pin) != len(pinPrefix)+64 {
		return nil, fmt.Errorf("pin %q is not sha256:<64 hex digits>", pin)
	}
	want := strings.ToLower(pin)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The standard verification is what the pin replaces. VerifyConnection
		// below runs on every handshake and is the check.
		InsecureSkipVerify: true, //nolint:gosec // replaced by the pin check
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server presented no certificate")
			}
			got := Pin(cs.PeerCertificates[0])
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("server key is %s, not the pinned %s: "+
					"this is not the control plane the token was made on", got, want)
			}
			return nil
		},
	}, nil
}

// VerifiesServer reports whether caPEM is enough to verify the server
// certificate in cs for serverName. An agent checks this before keeping a CA it
// was handed, so it never stores trust that would fail on its next request.
func VerifiesServer(caPEM []byte, cs tls.ConnectionState, serverName string) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("no certificates in the CA the control plane sent")
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("server presented no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots: pool, Intermediates: inter, DNSName: serverName,
	})
	return err
}

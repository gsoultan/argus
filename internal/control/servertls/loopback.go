package servertls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// loopbackLife is how long a loopback certificate is issued for. It is
// reissued on the same key well before then, so the pin an agent holds
// outlives every certificate.
const loopbackLife = 10 * 365 * 24 * time.Hour

// loopbackRenewBefore is how close to expiry a loopback certificate is
// reissued at start.
const loopbackRenewBefore = 365 * 24 * time.Hour

// LoopbackCert is a self-signed certificate for 127.0.0.1, ::1 and localhost,
// kept in dir as loopback.crt and loopback.key, for a listener that only a
// process on this machine reaches and that pins it.
//
// The key is made once and kept: what a client pins is the public key, so a
// reissued certificate keeps working for a client that pinned the old one.
func LoopbackCert(dir string) (certFile, keyFile string, err error) {
	certFile = filepath.Join(dir, "loopback.crt")
	keyFile = filepath.Join(dir, "loopback.key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}

	key, err := loadLoopbackKey(keyFile)
	if errors.Is(err, os.ErrNotExist) {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err == nil {
			err = writeKey(keyFile, key)
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("loopback key %s: %w", keyFile, err)
	}

	if loopbackCertGood(certFile, key, time.Now()) {
		return certFile, keyFile, nil
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "argus-control (this server only)", Organization: []string{"Argus"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(loopbackLife),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	tmp := certFile + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, certFile); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

func loadLoopbackKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, errors.New("not an EC private key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	// 0600: a readable private key makes the certificate meaningless.
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

// loopbackCertGood is whether the certificate at path is for key and not
// near its end.
func loopbackCertGood(path string, key *ecdsa.PrivateKey, now time.Time) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	return ok && pub.Equal(&key.PublicKey) && now.Add(loopbackRenewBefore).Before(cert.NotAfter)
}

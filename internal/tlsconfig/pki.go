package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/secrets"
)

/*
A private CA the control plane keeps for itself.

First-run setup has to serve the setup page over TLS before anyone has given it
a certificate -- the page carries a database password and the first
administrator's -- and a fresh install should not end with an operator running
openssl. So the control plane makes a small CA of its own in its state
directory, issues its server certificate from it, and later issues gateways'
client certificates from it too. The CA key never leaves that directory.

Distinct from GenerateDevPKI: that writes throwaway certificates for a laptop;
these are what a production install runs on until someone uploads a
certificate from their own CA.
*/

// ManagedPKI names the files of the control plane's own CA in one directory.
type ManagedPKI struct{ Dir string }

func (p ManagedPKI) CACert() string     { return filepath.Join(p.Dir, "ca.crt") }
func (p ManagedPKI) CAKey() string      { return filepath.Join(p.Dir, "ca.key") }
func (p ManagedPKI) ServerCert() string { return filepath.Join(p.Dir, "server.crt") }
func (p ManagedPKI) ServerKey() string  { return filepath.Join(p.Dir, "server.key") }

// serverValidity is short enough that a stolen key ages out, long enough that
// renewal is rare. Renewal is automatic; see RenewServerIfDue.
const serverValidity = 397 * 24 * time.Hour

// renewBefore is how close to expiry a managed certificate is reissued.
const renewBefore = 30 * 24 * time.Hour

// EnsureCA creates the CA if the directory holds none, and returns whether it
// made one.
func (p ManagedPKI) EnsureCA() (bool, error) {
	if _, err := os.Stat(p.CACert()); err == nil {
		if _, err := os.Stat(p.CAKey()); err == nil {
			return false, nil
		}
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return false, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Argus internal CA " + host, Organization: []string{"Argus"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return false, err
	}
	return true, writePair(p.Dir, "ca", der, key)
}

// IssueServer writes a server certificate for hosts, signed by the CA.
func (p ManagedPKI) IssueServer(hosts []string) error {
	der, key, err := p.issueServer(hosts)
	if err != nil {
		return err
	}
	return writePairAtomic(p.ServerCert(), p.ServerKey(), der, key)
}

func (p ManagedPKI) issueServer(hosts []string) ([]byte, *ecdsa.PrivateKey, error) {
	caCert, caKey, err := p.loadCA()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: firstOr(hosts, "argus-control")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range dedupe(hosts) {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	return der, key, err
}

// IssuesServer reports whether the server certificate in the directory was
// issued by this CA. False for one an operator uploaded, which is never
// replaced behind their back.
func (p ManagedPKI) IssuesServer() bool {
	ca, err := readCert(p.CACert())
	if err != nil {
		return false
	}
	leaf, err := readCert(p.ServerCert())
	if err != nil {
		return false
	}
	return leaf.CheckSignatureFrom(ca) == nil
}

// RenewServerIfDue reissues a CA-issued server certificate that is close to
// expiry or does not cover every name in hosts. It returns whether it did.
func (p ManagedPKI) RenewServerIfDue(hosts []string) (bool, error) {
	if !p.IssuesServer() {
		return false, nil
	}
	leaf, err := readCert(p.ServerCert())
	if err != nil {
		return false, err
	}
	due := time.Until(leaf.NotAfter) < renewBefore
	for _, h := range hosts {
		if leaf.VerifyHostname(h) != nil {
			due = true
		}
	}
	if !due {
		return false, nil
	}
	// The names it already covers are kept: an address an agent enrolled
	// against must go on verifying after a renewal.
	keep := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		keep = append(keep, ip.String())
	}
	return true, p.IssueServer(append(hosts, keep...))
}

// HasCAKey reports whether this directory can issue certificates.
func (p ManagedPKI) HasCAKey() bool {
	_, err := os.Stat(p.CAKey())
	return err == nil
}

func (p ManagedPKI) loadCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := readCert(p.CACert())
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(p.CAKey())
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, nil, errors.New("CA key: no PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("CA key: %w", err)
	}
	return cert, key, nil
}

// WriteServerPair checks an uploaded certificate and key belong together and
// installs them in place of the server certificate.
func (p ManagedPKI) WriteServerPair(certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("the certificate and key do not form a pair: %w", err)
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(p.ServerKey(), keyPEM, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(p.ServerCert(), certPEM, 0o644)
}

// CertSummary describes a certificate for a person deciding whether it is
// the right one.
type CertSummary struct {
	Subject  string    `json:"subject"`
	Issuer   string    `json:"issuer"`
	Names    []string  `json:"names"`
	NotAfter time.Time `json:"notAfter"`
	Pin      string    `json:"pin"`
}

// Summarize reads the first certificate in a PEM file.
func Summarize(path string) (CertSummary, error) {
	c, err := readCert(path)
	if err != nil {
		return CertSummary{}, err
	}
	names := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	return CertSummary{
		Subject: c.Subject.CommonName, Issuer: c.Issuer.CommonName,
		Names: names, NotAfter: c.NotAfter, Pin: Pin(c),
	}, nil
}

func readCert(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no certificate", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

// writePairAtomic replaces a certificate and its key so a reader never sees a
// new certificate with the old key: the key goes first, and the reloader
// keeps serving the loaded pair until both files parse together.
func writePairAtomic(certPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	// A key is made private before anything is written to it, and on Windows
	// that takes an access list: the mode above is ignored there.
	if mode&0o077 == 0 {
		if err := secrets.MakePrivate(tmp.Name()); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func dedupe(in []string) []string {
	var out []string
	for _, h := range in {
		h = strings.TrimSpace(h)
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func firstOr(in []string, fallback string) string {
	for _, h := range in {
		if h != "" && net.ParseIP(h) == nil {
			return h
		}
	}
	return fallback
}

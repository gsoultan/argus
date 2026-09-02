package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// GenerateDevPKI writes a local CA plus server and client certificates.
//
// For development and tests only. Production uses a real CA — Let's Encrypt at
// the edge, an internal PKI for machine-to-machine — and the point of this
// helper is that "TLS is hard to set up locally" never becomes the reason a
// deployment runs without it.
//
// Files written into dir:
//
//	ca.crt / ca.key            the local authority
//	server.crt / server.key    for control plane and gateway listeners
//	client.crt / client.key    for gateways and agents calling the control plane
func GenerateDevPKI(dir string, hosts []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Argus Development CA", Organization: []string{"Argus"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	if err := writePair(dir, "ca", caDER, caKey); err != nil {
		return err
	}

	// Server certificate, valid for every name and address the listeners are
	// reached by. A certificate that does not cover 127.0.0.1 is the usual
	// reason a local TLS setup gets abandoned.
	serverTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "argus-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			serverTmpl.IPAddresses = append(serverTmpl.IPAddresses, ip)
		} else {
			serverTmpl.DNSNames = append(serverTmpl.DNSNames, h)
		}
	}
	if err := issue(dir, "server", serverTmpl, caCert, caKey); err != nil {
		return err
	}

	// Client certificate. The common name identifies which gateway or agent
	// is calling, so reporter traffic is attributable to a host rather than to
	// "whoever holds the token".
	clientTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "argus-gateway"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return issue(dir, "client", clientTmpl, caCert, caKey)
}

func issue(dir, name string, tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		return err
	}
	return writePair(dir, name, der, key)
}

func writePair(dir, name string, der []byte, key *ecdsa.PrivateKey) error {
	certPath := filepath.Join(dir, name+".crt")
	if err := os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(dir, name+".key")
	// 0600: a readable private key makes the certificate meaningless.
	return os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
}

func serial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

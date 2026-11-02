package tlsconfig

import (
	"crypto/tls"
	"testing"
)

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

func TestClientCertAndKeyMustBeGivenTogether(t *testing.T) {
	if _, err := Client(ClientOptions{CertFile: "client.crt"}); err == nil {
		t.Error("accepted a client certificate with no key")
	}
}

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
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3", cfg.MinVersion)
	}
}

// A control plane, or a proxy in front of it, that speaks nothing newer than
// TLS 1.2 is refused by every way an agent trusts one: the CA, the pin, and
// the pin-or-public fallback.
func TestClientRefusesATLS12Server(t *testing.T) {
	srv, pool := serverUpTo(t, tls.VersionTLS12)
	pin := Pin(srv.Certificate())

	pinned, err := Pinned(pin)
	if err != nil {
		t.Fatal(err)
	}
	orPublic, err := pinnedOrRoots(pin, pool)
	if err != nil {
		t.Fatal(err)
	}
	byCA := &tls.Config{RootCAs: pool, MinVersion: MinVersion}
	for name, cfg := range map[string]*tls.Config{"pinned": pinned, "pin or public": orPublic, "CA": byCA} {
		if err := get(t, cfg, srv); err == nil {
			t.Errorf("%s: connected to a TLS 1.2-only server", name)
		}
	}
}

func TestClientCertAndKeyMustBeGivenTogether(t *testing.T) {
	if _, err := Client(ClientOptions{CertFile: "client.crt"}); err == nil {
		t.Error("accepted a client certificate with no key")
	}
}

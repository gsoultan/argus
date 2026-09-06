package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePEMKey(t *testing.T, mode os.FileMode) string {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// The host key is what every operator's client pins. A gateway that starts
// with it readable by every local account has already been impersonated by
// anyone who wanted to.
func TestLoadHostKeyRefusesWorldReadable(t *testing.T) {
	if _, err := loadHostKey(writePEMKey(t, 0o600)); err != nil {
		t.Fatalf("0600 host key should load: %v", err)
	}
	_, err := loadHostKey(writePEMKey(t, 0o644))
	if err == nil {
		t.Fatal("a world-readable host key must refuse to load")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("refusal should say how to fix it: %v", err)
	}
}

// An injected key every local account can read is not a vaulted credential,
// whatever the inventory calls it.
func TestLoadInjectedKeyRefusesGroupReadable(t *testing.T) {
	if _, err := loadInjectedKey(writePEMKey(t, 0o400)); err != nil {
		t.Fatalf("0400 injected key should load: %v", err)
	}
	if _, err := loadInjectedKey(writePEMKey(t, 0o640)); err == nil {
		t.Fatal("a group-readable injected key must refuse to load")
	}
}

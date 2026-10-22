package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func authorizedLine(t *testing.T, comment string) string {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := string(ssh.MarshalAuthorizedKey(sp))
	if comment != "" {
		line = line[:len(line)-1] + " " + comment + "\n"
	}
	return line
}

// A fresh install has nobody to list: the browser terminal does not use this
// file, and the gateway must start without it.
func TestAuthorizedKeysMayBeEmpty(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]*string{
		"missing":       nil,
		"empty":         new(""),
		"comments only": new("# native ssh(1) users go here\n\n   \n# one per line\n"),
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name)
			if body != nil {
				if err := os.WriteFile(p, []byte(*body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			keys, err := loadAuthorizedKeys(p)
			if err != nil {
				t.Fatalf("loadAuthorizedKeys: %v", err)
			}
			if len(keys) != 0 {
				t.Fatalf("got %d keys from a file with none", len(keys))
			}
		})
	}
}

func TestAuthorizedKeysRefusesADamagedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(p, []byte("# header\nssh-ed25519 not-base64 alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAuthorizedKeys(p); err == nil {
		t.Fatal("a file with content and no usable key was accepted")
	}
}

func TestAuthorizedKeysReadsEveryKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "authorized_keys")
	body := "# ops\n" + authorizedLine(t, "alice@example.com") + "\n" + authorizedLine(t, "bob@example.com")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := loadAuthorizedKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}
}

func TestAuthorizedKeysRefusesAnUnlabelledKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(p, []byte(authorizedLine(t, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAuthorizedKeys(p); err == nil {
		t.Fatal("a key with no comment was accepted")
	}
}

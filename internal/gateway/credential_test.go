package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vault(t *testing.T, files map[string]os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	for name, mode := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCredentialForReadsThePrincipalsPassword(t *testing.T) {
	dir := vault(t, map[string]os.FileMode{"ops": 0o600})
	got, err := credentialFor(Asset{CredentialDir: dir}, "ops")
	if err != nil {
		t.Fatalf("credentialFor: %v", err)
	}
	// The trailing newline every editor leaves behind is not the password.
	if got != "hunter2" {
		t.Errorf("password = %q", got)
	}
}

// A password every local account can read is not a vaulted credential. This is
// exactly the kind of thing set up correctly and then loosened by a deployment
// script nobody re-reads.
func TestCredentialForRefusesLoosePermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		dir := vault(t, map[string]os.FileMode{"ops": mode})
		_, err := credentialFor(Asset{CredentialDir: dir}, "ops")
		if err == nil {
			t.Errorf("mode %04o was accepted", mode)
			continue
		}
		if !strings.Contains(err.Error(), "readable") {
			t.Errorf("mode %04o: error does not name the reason: %v", mode, err)
		}
	}
}

// The principal arrives in a client-supplied cookie. Anything that could climb
// out of the vault directory must be refused rather than cleaned up: a
// sanitised path that still resolves somewhere unexpected is worse than a
// rejection, because it looks like it worked.
func TestCredentialForRefusesPathTraversal(t *testing.T) {
	dir := vault(t, map[string]os.FileMode{"ops": 0o600})
	// A file the traversal would reach if it were allowed.
	outside := filepath.Join(filepath.Dir(dir), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, principal := range []string{
		"../outside", "..", ".", "", "ops/../../outside",
		`..\outside`, "sub/ops",
	} {
		if _, err := credentialFor(Asset{CredentialDir: dir}, principal); err == nil {
			t.Errorf("principal %q was accepted", principal)
		}
	}
}

func TestCredentialForReportsAMissingVault(t *testing.T) {
	if _, err := credentialFor(Asset{Hostname: "win-01"}, "ops"); !errors.Is(err, ErrNoCredential) {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}
	dir := vault(t, map[string]os.FileMode{"ops": 0o600})
	if _, err := credentialFor(Asset{CredentialDir: dir}, "administrator"); !errors.Is(err, ErrNoCredential) {
		t.Errorf("a principal with no vaulted credential gave %v", err)
	}
}

package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMode(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by umask; Chmod is not.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckPrivateAcceptsOwnerOnly(t *testing.T) {
	for _, m := range []os.FileMode{0o600, 0o400, 0o700} {
		if err := CheckPrivate(writeMode(t, m)); err != nil {
			t.Errorf("mode %04o should be accepted: %v", m, err)
		}
	}
}

// Every one of these is a real deployment mistake: a key copied with cp,
// unpacked from a tarball, or created by a script that never set umask.
func TestCheckPrivateRefusesGroupOrWorldReadable(t *testing.T) {
	for _, m := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666, 0o755} {
		err := CheckPrivate(writeMode(t, m))
		if err == nil {
			t.Errorf("mode %04o must be refused", m)
			continue
		}
		// The message has to say what to do. An operator reading this at
		// startup should not need to look anything up.
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: error should say how to fix it: %v", m, err)
		}
	}
}

func TestCheckPrivateRefusesDirectoryAndMissing(t *testing.T) {
	if err := CheckPrivate(t.TempDir()); err == nil {
		t.Error("a directory must be refused")
	}
	if err := CheckPrivate(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing file must be refused")
	}
}

func TestReadPrivateReturnsContentOnlyWhenSafe(t *testing.T) {
	data, err := ReadPrivate(writeMode(t, 0o600))
	if err != nil || string(data) != "secret" {
		t.Fatalf("ReadPrivate = %q, %v", data, err)
	}
	if _, err := ReadPrivate(writeMode(t, 0o644)); err == nil {
		t.Error("ReadPrivate must not return a world-readable secret")
	}
}

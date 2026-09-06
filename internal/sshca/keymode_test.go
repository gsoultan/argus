package sshca

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The CA key is the single most sensitive file in a deployment: whoever reads
// it mints a certificate for any principal on any target, and no host-key pin
// or policy stops them. It must not load if anyone but its owner can read it.
func TestLoadRefusesReadableCAKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca")
	if _, err := Generate(path); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := Load(path, time.Minute); err != nil {
		t.Fatalf("a freshly generated 0600 key should load: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, time.Minute); err == nil {
		t.Fatal("a world-readable CA key must refuse to load")
	}
}

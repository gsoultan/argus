package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// The first grant key is kept; a different one afterwards is refused, even by
// a fresh process reading the pin back from disk.
func TestTheGrantKeyIsPinnedAndAChangeIsRefused(t *testing.T) {
	dir := t.TempDir()
	first, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)

	if _, err := NewGrantPin(dir).Check(first); err != nil {
		t.Fatalf("the first key was not pinned: %v", err)
	}
	restarted := NewGrantPin(dir)
	if _, err := restarted.Check(other); err == nil {
		t.Fatal("a different grant key was trusted after one was pinned")
	}
	if k, err := restarted.Check(first); err != nil || !k.Equal(first) {
		t.Fatalf("the pinned key was refused: %v", err)
	}
}

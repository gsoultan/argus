package agent

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

func recoveryAgent(t *testing.T) (*Recovery, *GrantPin, ed25519.PrivateKey) {
	t.Helper()
	dir := t.TempDir()
	pin := NewGrantPin(dir)
	oldGrant, _, _ := ed25519.GenerateKey(nil)
	if _, err := pin.Check(oldGrant); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := tunnel.NewRecoveryKey()
	r := &Recovery{StateDir: dir, Pin: pin}
	if learned, err := r.Learn(tunnel.EncodeGrantKey(pub)); err != nil || !learned {
		t.Fatalf("learn: %v %v", learned, err)
	}
	return r, pin, priv
}

func order(t *testing.T, k ed25519.PrivateKey, kind string, key ed25519.PublicKey, at time.Time) string {
	t.Helper()
	tok, err := tunnel.SignRecovery(k, tunnel.RecoveryOrder{Kind: kind, Key: tunnel.EncodeGrantKey(key), IssuedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// The key is learned once; a control plane cannot swap it afterwards.
func TestTheRecoveryKeyIsLearnedOnce(t *testing.T) {
	r, _, _ := recoveryAgent(t)
	before := r.Fingerprint()
	other, _, _ := tunnel.NewRecoveryKey()
	if learned, err := r.Learn(tunnel.EncodeGrantKey(other)); learned || err == nil {
		t.Fatalf("a second key was taken: %v %v", learned, err)
	}
	again := &Recovery{StateDir: r.StateDir}
	if again.Fingerprint() != before {
		t.Error("the pinned key did not survive a restart")
	}
}

// A rekey signed with the recovery key moves the grant pin; one signed with
// anything else, or older than the last, does not.
func TestARekeyMovesTheGrantPin(t *testing.T) {
	r, pin, priv := recoveryAgent(t)
	newGrant, _, _ := ed25519.GenerateKey(nil)
	_, forger, _ := tunnel.NewRecoveryKey()

	if _, err := r.Apply(order(t, forger, tunnel.RecoveryRekey, newGrant, time.Now())); err == nil {
		t.Fatal("a rekey signed with another key was acted on")
	}
	now := time.Now()
	if what, err := r.Apply(order(t, priv, tunnel.RecoveryRekey, newGrant, now)); err != nil || what == "" {
		t.Fatalf("rekey: %q %v", what, err)
	}
	if !pin.Pinned().Equal(newGrant) {
		t.Fatal("the grant key was not re-pinned")
	}
	if _, err := pin.Check(newGrant); err != nil {
		t.Errorf("the tunnel refuses the new key: %v", err)
	}
	// An older one cannot move it back.
	oldAgain, _, _ := ed25519.GenerateKey(nil)
	if what, _ := r.Apply(order(t, priv, tunnel.RecoveryRekey, oldAgain, now.Add(-time.Hour))); what != "" ||
		!pin.Pinned().Equal(newGrant) {
		t.Error("an older rekey was acted on")
	}
	b, _ := os.ReadFile(filepath.Join(r.StateDir, GrantKeyFile))
	if k, _ := tunnel.DecodeGrantKey(string(b)); !k.Equal(newGrant) {
		t.Error("the new key is not on disk")
	}
}

// A replacement signed with the old key moves to the new one, and a rekey
// signed with the new one is then taken.
func TestAReplacementThenARekeySignedWithIt(t *testing.T) {
	r, pin, oldPriv := recoveryAgent(t)
	newPub, newPriv, _ := tunnel.NewRecoveryKey()
	at := time.Now()
	if what, err := r.Apply(order(t, oldPriv, tunnel.RecoveryReplace, newPub, at)); err != nil || what == "" {
		t.Fatalf("replace: %q %v", what, err)
	}
	if r.Fingerprint() != tunnel.RecoveryFingerprint(newPub) {
		t.Fatal("the recovery key was not replaced")
	}
	grant, _, _ := ed25519.GenerateKey(nil)
	// Issued before the replacement: kinds are counted apart.
	if _, err := r.Apply(order(t, newPriv, tunnel.RecoveryRekey, grant, at.Add(-time.Minute))); err != nil ||
		!pin.Pinned().Equal(grant) {
		t.Fatalf("a rekey signed with the new key: %v", err)
	}
}

func TestNoRecoveryKeyNoOrders(t *testing.T) {
	dir := t.TempDir()
	r := &Recovery{StateDir: dir, Pin: NewGrantPin(dir)}
	_, priv, _ := tunnel.NewRecoveryKey()
	k, _, _ := ed25519.GenerateKey(nil)
	if _, err := r.Apply(order(t, priv, tunnel.RecoveryRekey, k, time.Now())); err == nil {
		t.Fatal("acted on an order with no key to check it")
	}
}

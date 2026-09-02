package hostkey

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newStore(t *testing.T, tofu bool) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pins.json"), tofu)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestTrustOnFirstUsePinsThenEnforces(t *testing.T) {
	s := newStore(t, true)
	first := testKey(t)

	if err := s.Verify("pay-01", first); err != nil {
		t.Fatalf("first contact should pin: %v", err)
	}

	// Same key again: fine.
	if err := s.Verify("pay-01", first); err != nil {
		t.Errorf("re-presenting the pinned key failed: %v", err)
	}

	// A different key on a pinned host is either a rebuild or an interception,
	// and the gateway cannot tell which. It must refuse.
	if err := s.Verify("pay-01", testKey(t)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch", err)
	}
}

// Strict mode is what a production fleet runs: pins are placed out of band, so
// even the first connection is protected.
func TestStrictModeRefusesUnpinnedHosts(t *testing.T) {
	s := newStore(t, false)
	if err := s.Verify("never-seen", testKey(t)); !errors.Is(err, ErrUnpinned) {
		t.Errorf("err = %v, want ErrUnpinned", err)
	}
}

func TestMismatchErrorNamesBothFingerprints(t *testing.T) {
	s := newStore(t, true)
	pinned := testKey(t)
	_ = s.Verify("pay-01", pinned)

	presented := testKey(t)
	err := s.Verify("pay-01", presented)
	if err == nil {
		t.Fatal("expected a mismatch")
	}
	// An operator has to compare these by eye against console access, so both
	// must appear in the message.
	msg := err.Error()
	for _, want := range []string{ssh.FingerprintSHA256(pinned), ssh.FingerprintSHA256(presented)} {
		if !contains(msg, want) {
			t.Errorf("message does not contain %s: %s", want, msg)
		}
	}
}

func TestPinsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")

	first, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	if err := first.Verify("pay-01", key); err != nil {
		t.Fatal(err)
	}

	// A gateway restart must not forget what it verified, or every restart
	// re-opens the trust-on-first-use window.
	second, err := Open(path, true)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := second.Verify("pay-01", testKey(t)); !errors.Is(err, ErrMismatch) {
		t.Error("pins did not survive a restart")
	}
	if err := second.Verify("pay-01", key); err != nil {
		t.Errorf("the original key was rejected after restart: %v", err)
	}
}

// An unreadable store must stop the gateway, not degrade into trusting
// everything.
func TestCorruptStoreRefusesToOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pins.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, true); err == nil {
		t.Error("opened a corrupt store instead of refusing to start")
	}
}

func TestRepinIsExplicit(t *testing.T) {
	s := newStore(t, true)
	old := testKey(t)
	_ = s.Verify("pay-01", old)

	replacement := testKey(t)
	if err := s.Verify("pay-01", replacement); err == nil {
		t.Fatal("a new key was accepted without an explicit repin")
	}

	// Repinning is a deliberate act after out-of-band verification, never a
	// side effect of a gateway happening to connect during a rebuild.
	if err := s.Repin("pay-01", replacement, "dewi.p@northwind.id"); err != nil {
		t.Fatalf("Repin: %v", err)
	}
	if err := s.Verify("pay-01", replacement); err != nil {
		t.Errorf("the repinned key was rejected: %v", err)
	}

	pin, ok := s.Lookup("pay-01")
	if !ok || pin.PinnedBy != "dewi.p@northwind.id" {
		t.Errorf("repin did not record who vouched for it: %+v", pin)
	}
}

func TestPinsAreScopedPerHost(t *testing.T) {
	s := newStore(t, true)
	a, b := testKey(t), testKey(t)
	_ = s.Verify("pay-01", a)

	// Pinning one host must not affect another.
	if err := s.Verify("pay-02", b); err != nil {
		t.Errorf("pinning pay-01 leaked onto pay-02: %v", err)
	}
	if err := s.Verify("pay-01", b); err == nil {
		t.Error("pay-02's key was accepted for pay-01")
	}
}

/* ── Shared pins ─────────────────────────────────────────────────────────── */

type fakeRemote struct {
	pins   map[string]Pin
	err    error
	writes int
}

func (f *fakeRemote) Pin(_ context.Context, host string) (*Pin, error) {
	if f.err != nil {
		return nil, f.err
	}
	if p, ok := f.pins[host]; ok {
		return &p, nil
	}
	return nil, nil
}

func (f *fakeRemote) Record(_ context.Context, p Pin) error {
	if f.err != nil {
		return f.err
	}
	f.writes++
	if f.pins == nil {
		f.pins = map[string]Pin{}
	}
	if existing, ok := f.pins[p.Host]; ok && existing.Fingerprint != p.Fingerprint {
		return errors.New("conflicts with recorded state")
	}
	f.pins[p.Host] = p
	return nil
}

// The shared store is authoritative: a gateway that never pinned a host itself
// must still enforce what another gateway recorded.
func TestRemotePinsAreAuthoritative(t *testing.T) {
	key := testKey(t)
	remote := &fakeRemote{pins: map[string]Pin{
		"pay-01": {Host: "pay-01", Fingerprint: ssh.FingerprintSHA256(key),
			KeyType: key.Type(), PinnedAt: time.Now(), PinnedBy: "gateway-a"},
	}}

	s := newStore(t, true)
	s.UseRemote(remote)

	if err := s.Verify("pay-01", key); err != nil {
		t.Errorf("a pin recorded elsewhere was not honoured: %v", err)
	}
	if err := s.Verify("pay-01", testKey(t)); !errors.Is(err, ErrMismatch) {
		t.Error("a different key was accepted despite the shared pin")
	}
}

// The local file must not be consulted once shared storage is in use, or a
// stale local pin silently overrides the real one.
func TestRemoteOverridesLocalState(t *testing.T) {
	s := newStore(t, true)
	local := testKey(t)
	_ = s.Verify("pay-01", local) // pinned locally

	// Shared storage says something different.
	other := testKey(t)
	s.UseRemote(&fakeRemote{pins: map[string]Pin{
		"pay-01": {Host: "pay-01", Fingerprint: ssh.FingerprintSHA256(other)},
	}})

	if err := s.Verify("pay-01", local); err == nil {
		t.Error("the stale local pin was used instead of the shared one")
	}
	if err := s.Verify("pay-01", other); err != nil {
		t.Errorf("the shared pin was not honoured: %v", err)
	}
}

// A network blip must not downgrade the check to whatever this gateway
// remembers; that is how a mismatch goes unnoticed.
func TestUnreachableRemoteRefuses(t *testing.T) {
	s := newStore(t, true)
	key := testKey(t)
	_ = s.Verify("pay-01", key) // a local pin exists

	s.UseRemote(&fakeRemote{err: errors.New("connection refused")})

	if err := s.Verify("pay-01", key); err == nil {
		t.Error("fell back to the local pin when shared storage was unreachable")
	}
}

func TestRemoteTOFURecordsThePin(t *testing.T) {
	remote := &fakeRemote{}
	s := newStore(t, true)
	s.UseRemote(remote)

	if err := s.Verify("new-host", testKey(t)); err != nil {
		t.Fatalf("first contact failed: %v", err)
	}
	if remote.writes != 1 {
		t.Errorf("recorded %d pins, want 1", remote.writes)
	}
}

func TestRemoteRecordConflictIsAMismatch(t *testing.T) {
	remote := &fakeRemote{pins: map[string]Pin{}}
	// Another gateway got there first with a different key.
	_ = remote.Record(context.Background(), Pin{Host: "pay-01", Fingerprint: "SHA256:someone-else"})

	s := newStore(t, true)
	s.UseRemote(remote)

	// Pin() returns the existing entry, so this is a straight mismatch.
	if err := s.Verify("pay-01", testKey(t)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch", err)
	}
}

func TestSharedIsReported(t *testing.T) {
	s := newStore(t, true)
	if s.Shared() {
		t.Error("reports shared pins with no remote configured")
	}
	s.UseRemote(&fakeRemote{})
	if !s.Shared() {
		t.Error("does not report shared pins after a remote is set")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newEdKeyLine(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ssh.NewPublicKey(pub)
	return string(ssh.MarshalAuthorizedKey(k))
}

// The heartbeat must report a key added between two beats on the next beat,
// as it did when every beat scanned the host. A cache that only expired on a
// timer would hide a planted key for as long as its timer.
func TestPostureCacheSeesAChangeOnTheNextCall(t *testing.T) {
	cfg := postureHost(t, 3)
	c := NewPostureCache(cfg, time.Hour)

	first, err := c.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Scan(); err != nil {
		t.Fatal(err)
	}
	if c.Scans() != 1 {
		t.Fatalf("an unchanged host was scanned %d times, want 1", c.Scans())
	}

	// A key appended to an existing file.
	home := strings.TrimSuffix(filepath.Dir(cfg.SSHDConfig), "/") + "/home/user001"
	f, err := os.OpenFile(filepath.Join(home, ".ssh", "authorized_keys"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(newEdKeyLine(t))
	_ = f.Close()
	p, _ := c.Scan()
	if len(p.UnmanagedKeys) != len(first.UnmanagedKeys)+1 {
		t.Fatalf("appended key not reported: %d keys, was %d", len(p.UnmanagedKeys), len(first.UnmanagedKeys))
	}

	// A file that did not exist: authorized_keys2.
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys2"), []byte(newEdKeyLine(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, _ := c.Scan()
	if len(p2.UnmanagedKeys) != len(p.UnmanagedKeys)+1 {
		t.Fatal("a new authorized_keys2 was not reported")
	}

	// A new account with a key.
	newHome := filepath.Join(filepath.Dir(cfg.SSHDConfig), "home", "intruder")
	_ = os.MkdirAll(filepath.Join(newHome, ".ssh"), 0o700)
	_ = os.WriteFile(filepath.Join(newHome, ".ssh", "authorized_keys"), []byte(newEdKeyLine(t)), 0o600)
	pw, _ := os.ReadFile(cfg.PasswdFile)
	pw = append(pw, []byte("intruder:x:4000:4000::"+newHome+":/bin/sh\n")...)
	if err := os.WriteFile(cfg.PasswdFile, pw, 0o600); err != nil {
		t.Fatal(err)
	}
	p3, _ := c.Scan()
	found := false
	for _, k := range p3.UnmanagedKeys {
		found = found || k.User == "intruder"
	}
	if !found {
		t.Fatal("a new account's key was not reported")
	}

	// sshd_config changed.
	if err := os.WriteFile(cfg.SSHDConfig, []byte("PasswordAuthentication yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p4, _ := c.Scan()
	if p4.ShimInstalled || !p4.PasswordAuthEnabled {
		t.Fatal("an sshd_config change was not reported")
	}
}

// What a stat cannot see is bounded by maxAge.
func TestPostureCacheRescansAfterMaxAge(t *testing.T) {
	cfg := postureHost(t, 1)
	c := NewPostureCache(cfg, time.Minute)
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	_, _ = c.Scan()
	now = now.Add(30 * time.Second)
	_, _ = c.Scan()
	if c.Scans() != 1 {
		t.Fatalf("scanned %d times inside maxAge", c.Scans())
	}
	now = now.Add(31 * time.Second)
	_, _ = c.Scan()
	if c.Scans() != 2 {
		t.Fatalf("scanned %d times after maxAge, want 2", c.Scans())
	}
}

// A cached answer must be the same answer a scan gives.
func TestPostureCacheMatchesAScan(t *testing.T) {
	cfg := postureHost(t, 5)
	c := NewPostureCache(cfg, time.Hour)
	_, _ = c.Scan()
	cached, _ := c.Scan()
	fresh, _ := Scan(cfg)
	cached.CheckedAt, fresh.CheckedAt = time.Time{}, time.Time{}
	if a, b := mustJSON(t, cached), mustJSON(t, fresh); a != b {
		t.Fatalf("cached posture differs from a scan:\n%s\n%s", a, b)
	}
}

// What the heartbeat pays now on an unchanged host.
func BenchmarkPostureCacheHit(b *testing.B) {
	c := NewPostureCache(postureHost(b, 50), time.Hour)
	_, _ = c.Scan()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Scan(); err != nil {
			b.Fatal(err)
		}
	}
	if c.Scans() != 1 {
		b.Fatalf("rescanned %d times", c.Scans())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

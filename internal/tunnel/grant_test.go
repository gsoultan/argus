package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

/*
Session grants: what the agent refuses. Each of these is a way a shell could
open on a host without the control plane deciding it should.
*/

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	k, err := GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

func sign(t *testing.T, k ed25519.PrivateKey, g Grant) string {
	t.Helper()
	tok, err := SignGrant(k, g)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestAGrantIsHonouredOnceForItsOwnHost(t *testing.T) {
	k := testKey(t)
	v := &GrantVerifier{Host: "web-01"}
	tok := sign(t, k, NewGrant("ana@example.com", "WEB-01", "deploy", "10.0.0.9"))

	g, err := v.Verify(pub(k), tok)
	if err != nil {
		t.Fatalf("a valid grant was refused: %v", err)
	}
	if g.Email != "ana@example.com" || g.Principal != "deploy" {
		t.Fatalf("grant read back as %+v", g)
	}
	if _, err := v.Verify(pub(k), tok); err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("a grant was honoured twice: %v", err)
	}
}

func TestAGrantSignedByAnyoneElseIsRefused(t *testing.T) {
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	v := &GrantVerifier{Host: "web-01"}
	tok := sign(t, other, NewGrant("ana@example.com", "web-01", "root", ""))
	if _, err := v.Verify(pub(testKey(t)), tok); err == nil {
		t.Fatal("a grant signed by another key opened a session")
	}
}

func TestAnAlteredGrantIsRefused(t *testing.T) {
	k := testKey(t)
	v := &GrantVerifier{Host: "web-01"}
	honest := sign(t, k, NewGrant("ana@example.com", "web-01", "deploy", ""))
	// The same signature over a payload naming root instead.
	body, sig, _ := strings.Cut(honest, ".")
	forged, _, _ := strings.Cut(sign(t, k, NewGrant("ana@example.com", "web-01", "root", "")), ".")
	if _, err := v.Verify(pub(k), forged+"."+sig); err == nil {
		t.Fatal("a payload with someone else's signature was honoured")
	}
	// Flip a bit of the decoded signature. Overwriting the last characters
	// with fixed text was a no-op for the signatures that already ended in
	// them, about one run in 256.
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 1
	if _, err := v.Verify(pub(k), body+"."+base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("a damaged signature was honoured")
	}
}

func TestAGrantForAnotherHostIsRefused(t *testing.T) {
	k := testKey(t)
	v := &GrantVerifier{Host: "web-01"}
	if _, err := v.Verify(pub(k), sign(t, k, NewGrant("a@x", "db-01", "deploy", ""))); err == nil {
		t.Fatal("a grant for db-01 opened a session on web-01")
	}
}

func TestAnExpiredOrOverlongGrantIsRefused(t *testing.T) {
	k := testKey(t)
	v := &GrantVerifier{Host: "web-01"}

	old := NewGrant("a@x", "web-01", "deploy", "")
	old.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := v.Verify(pub(k), sign(t, k, old)); err == nil {
		t.Error("an expired grant was honoured")
	}

	long := NewGrant("a@x", "web-01", "deploy", "")
	long.ExpiresAt = time.Now().Add(24 * time.Hour)
	if _, err := v.Verify(pub(k), sign(t, k, long)); err == nil {
		t.Error("a grant valid for a day was honoured; the control plane never issues one")
	}
}

func TestAnAgentWithoutTheKeyRefusesEverything(t *testing.T) {
	k := testKey(t)
	v := &GrantVerifier{Host: "web-01"}
	if _, err := v.Verify(nil, sign(t, k, NewGrant("a@x", "web-01", "deploy", ""))); err == nil {
		t.Fatal("an agent with no grant key honoured a grant")
	}
}

// The replay set is bounded, and a full one refuses rather than forgets.
func TestTheReplaySetIsBounded(t *testing.T) {
	k := testKey(t)
	now := time.Now()
	v := &GrantVerifier{Host: "web-01", now: func() time.Time { return now }}
	for range maxUsedGrants {
		if _, err := v.Verify(pub(k), sign(t, k, NewGrant("a@x", "web-01", "deploy", ""))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.Verify(pub(k), sign(t, k, NewGrant("a@x", "web-01", "deploy", ""))); err == nil {
		t.Fatal("a full replay set accepted another grant")
	}
	// Once those expire, there is room again.
	now = now.Add(2 * GrantTTL)
	g := NewGrant("a@x", "web-01", "deploy", "")
	g.ExpiresAt = now.Add(GrantTTL)
	if _, err := v.Verify(pub(k), sign(t, k, g)); err != nil {
		t.Fatalf("expired entries were not evicted: %v", err)
	}
	if len(v.used) != 1 {
		t.Fatalf("%d entries left after eviction, want 1", len(v.used))
	}
}

func TestFramesRoundTripAndAreBounded(t *testing.T) {
	var b strings.Builder
	if err := WriteFrame(&b, FrameResize, ResizePayload(132, 43)); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, FrameExit, ExitPayload(-1, "abc")); err != nil {
		t.Fatal(err)
	}
	fr := NewFrameReader(strings.NewReader(b.String()))
	typ, p, err := fr.Next()
	if c, r, ok := ParseResize(p); err != nil || typ != FrameResize || !ok || c != 132 || r != 43 {
		t.Fatalf("resize read back as %c %v %d %d", typ, err, c, r)
	}
	typ, p, err = fr.Next()
	if code, head, ok := ParseExit(p); err != nil || typ != FrameExit || !ok || code != -1 || head != "abc" {
		t.Fatalf("exit read back as %c %v %d %q", typ, err, code, head)
	}

	// A length past the bound is refused before anything is allocated.
	huge := "o\xff\xff\xff\xff"
	if _, _, err := NewFrameReader(strings.NewReader(huge)).Next(); err == nil {
		t.Fatal("a 4 GB frame header was accepted")
	}
}

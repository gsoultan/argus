package ratelimit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBurstThenThrottle(t *testing.T) {
	l := New(Limit{Rate: 10, Window: time.Minute, Burst: 3}, 100)

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("attempt %d refused inside the burst", i+1)
		}
	}
	ok, retry := l.Allow("1.2.3.4")
	if ok {
		t.Fatal("a fourth attempt was allowed past a burst of 3")
	}
	if retry <= 0 {
		t.Error("no retry-after was reported")
	}
}

func TestTokensRefillOverTime(t *testing.T) {
	l := New(Limit{Rate: 60, Window: time.Minute, Burst: 1}, 100)

	now := time.Now()
	l.now = func() time.Time { return now }

	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first attempt refused")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("second immediate attempt allowed with a burst of 1")
	}

	// 60 per minute is one per second.
	now = now.Add(2 * time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Error("tokens did not refill after the window elapsed")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(Limit{Rate: 1, Window: time.Minute, Burst: 1}, 100)

	if ok, _ := l.Allow("attacker"); !ok {
		t.Fatal("first client refused")
	}
	if ok, _ := l.Allow("attacker"); ok {
		t.Fatal("attacker was not throttled")
	}
	// One client exhausting its allowance must not affect anyone else.
	if ok, _ := l.Allow("legitimate-user"); !ok {
		t.Error("an unrelated client was throttled")
	}
}

// The table is keyed by an address the client chooses, so an unbounded map is
// itself the denial of service this was added to prevent.
func TestTableIsBounded(t *testing.T) {
	const max = 100
	l := New(Limit{Rate: 10, Window: time.Minute, Burst: 5}, max)

	for i := 0; i < max*10; i++ {
		l.Allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	if got := l.Len(); got > max {
		t.Errorf("tracking %d keys with a cap of %d", got, max)
	}
}

// Eviction must drop the least recently seen client. Dropping an active one
// would reset an attacker's allowance, which is precisely backwards.
func TestEvictionDropsTheLeastRecentlySeen(t *testing.T) {
	l := New(Limit{Rate: 1, Window: time.Minute, Burst: 1}, 3)

	l.Allow("old")
	l.Allow("attacker")
	l.Allow("attacker") // refused, but keeps it recent
	l.Allow("b")
	l.Allow("c") // forces an eviction

	// The attacker was seen most recently, so their exhausted bucket must
	// survive and continue refusing.
	if ok, _ := l.Allow("attacker"); ok {
		t.Error("the active attacker's bucket was evicted, resetting their allowance")
	}
}

func TestResetRestoresAllowance(t *testing.T) {
	l := New(Limit{Rate: 5, Window: time.Minute, Burst: 2}, 10)
	l.Allow("user")
	l.Allow("user")
	if ok, _ := l.Allow("user"); ok {
		t.Fatal("expected throttling")
	}

	// After a success, a user who mistyped once should not stay throttled.
	l.Reset("user")
	if ok, _ := l.Allow("user"); !ok {
		t.Error("Reset did not restore the allowance")
	}
}

func TestSweepDropsIdleEntries(t *testing.T) {
	l := New(Limit{Rate: 10, Window: time.Second, Burst: 5}, 100)
	now := time.Now()
	l.now = func() time.Time { return now }

	l.Allow("idle")
	now = now.Add(time.Minute)
	l.Allow("active")

	if removed := l.Sweep(); removed != 1 {
		t.Errorf("swept %d entries, want 1", removed)
	}
	if l.Len() != 1 {
		t.Errorf("%d entries remain, want 1", l.Len())
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	l := New(Limit{Rate: 1000, Window: time.Minute, Burst: 500}, 1000)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Allow(fmt.Sprintf("client-%d", i%10))
				if j%20 == 0 {
					l.Sweep()
				}
			}
		}(i)
	}
	wg.Wait()
}

/* ── Client resolution ───────────────────────────────────────────────────── */

func request(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// The failure that makes a rate limiter worthless: believing a header the
// client controls. An attacker could both evade their own limit and throttle
// someone else by claiming that person's address.
func TestForwardedHeaderIsIgnoredWithoutATrustedProxy(t *testing.T) {
	r, err := NewClientResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Key(request("203.0.113.9:1234", map[string]string{
		"X-Forwarded-For": "1.1.1.1",
	}))
	if got != "203.0.113.9" {
		t.Errorf("key = %q — a spoofable header was trusted", got)
	}
}

func TestForwardedHeaderIsUsedBehindATrustedProxy(t *testing.T) {
	r, err := NewClientResolver([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Key(request("10.0.0.5:1234", map[string]string{
		"X-Forwarded-For": "203.0.113.9, 10.0.0.5",
	}))
	if got != "203.0.113.9" {
		t.Errorf("key = %q, want the original client", got)
	}
}

// A connection from outside the trusted range must not be believed, even if it
// sets the header.
func TestUntrustedPeerCannotForge(t *testing.T) {
	r, _ := NewClientResolver([]string{"10.0.0.0/8"})
	got := r.Key(request("198.51.100.7:1234", map[string]string{
		"X-Forwarded-For": "10.0.0.1",
	}))
	if got != "198.51.100.7" {
		t.Errorf("key = %q — an untrusted peer forged its address", got)
	}
}

func TestRFC7239ForwardedHeader(t *testing.T) {
	r, _ := NewClientResolver([]string{"10.0.0.0/8"})
	got := r.Key(request("10.0.0.5:1234", map[string]string{
		"Forwarded": `for=203.0.113.9;proto=https`,
	}))
	if got != "203.0.113.9" {
		t.Errorf("key = %q", got)
	}
}

func TestMalformedForwardedValueFallsBackToPeer(t *testing.T) {
	r, _ := NewClientResolver([]string{"10.0.0.0/8"})
	for _, bad := range []string{"not-an-ip", "", "999.999.999.999", "<script>"} {
		got := r.Key(request("10.0.0.5:1234", map[string]string{"X-Forwarded-For": bad}))
		if got != "10.0.0.5" {
			t.Errorf("X-Forwarded-For %q produced key %q, want the peer", bad, got)
		}
	}
}

// A typo in the trust list that silently disabled it would be invisible until
// someone exploited it.
func TestInvalidTrustedProxyIsAnError(t *testing.T) {
	if _, err := NewClientResolver([]string{"not-a-cidr"}); err == nil {
		t.Error("accepted an unparseable CIDR")
	}
}

func TestPeerIPHandlesIPv6(t *testing.T) {
	if got := PeerIP("[2001:db8::1]:2222"); got != "2001:db8::1" {
		t.Errorf("PeerIP = %q", got)
	}
	if got := PeerIP("10.0.0.1:22"); got != "10.0.0.1" {
		t.Errorf("PeerIP = %q", got)
	}
}

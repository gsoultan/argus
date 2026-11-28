package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// maxBody bounds every request body: a value is at most MaxValueBytes, and a
// rules bundle is the largest thing argus-control sends.
const (
	maxBody      = 256 << 10
	maxRulesBody = 64 << 20
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	// Nothing argus-secret answers may be kept by a proxy or a browser.
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// statusFor maps a store error to an HTTP answer. Anything not listed is an
// internal failure, reported without its detail.
func statusFor(err error) (int, string) {
	var inv InvalidError
	switch {
	case errors.As(err, &inv):
		return http.StatusBadRequest, inv.Msg
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "no such secret, or you may not read it"
	case errors.Is(err, ErrDeleted):
		return http.StatusConflict, ErrDeleted.Error()
	case errors.Is(err, ErrNeedsApproval):
		return http.StatusForbidden, ErrNeedsApproval.Error()
	case errors.Is(err, ErrRulesStale):
		return http.StatusServiceUnavailable, ErrRulesStale.Error()
	}
	return http.StatusInternalServerError, "argus-secret could not complete the request"
}

func readJSON(r *http.Request, limit int64, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return InvalidError{"the request body could not be read"}
	}
	if int64(len(body)) > limit {
		return InvalidError{"the request body is too large"}
	}
	return decodeJSON(body, v)
}

func decodeJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return InvalidError{"the request body is not the JSON expected: " + err.Error()}
	}
	return nil
}

// ClientIP is the address a request came from. Forwarding headers are
// believed only from a trusted proxy: otherwise any caller could name any
// address, evade its own limit and put someone else's in the log.
//
// Behind Cloudflare Tunnel, cloudflared connects from loopback and sets
// CF-Connecting-IP; other proxies append to X-Forwarded-For, read from the
// right, skipping proxies we trust.
type ClientIP struct {
	Trusted []netip.Prefix
}

// Of returns the client address for r.
func (c ClientIP) Of(r *http.Request) string {
	peer := remoteAddr(r.RemoteAddr)
	if !peer.IsValid() || !c.trusts(peer) {
		return peer.String()
	}
	if cf, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
		return cf.Unmap().String()
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !c.trusts(a) {
			return a.String()
		}
	}
	return peer.String()
}

func (c ClientIP) trusts(a netip.Addr) bool {
	for _, p := range c.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func remoteAddr(s string) netip.Addr {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// ParsePrefixes reads trusted proxy ranges: CIDRs or single addresses.
func ParsePrefixes(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, errors.New("trusted_proxies: " + s + " is not an address or CIDR")
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// failLimiter counts failed sign-ins per client address in fixed one-minute
// windows. Keyed by something callers choose, so it is bounded: when full,
// the window's counts are dropped, which only ever forgives.
type failLimiter struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
	max    int
	cap    int
	now    func() time.Time
}

func newFailLimiter(max int) *failLimiter {
	return &failLimiter{counts: map[string]int{}, max: max, cap: 10_000, now: time.Now}
}

func (l *failLimiter) roll() {
	w := l.now().Truncate(time.Minute)
	if !w.Equal(l.window) {
		l.window, l.counts = w, map[string]int{}
	}
}

// Blocked is whether addr has failed too often this minute.
func (l *failLimiter) Blocked(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll()
	return l.counts[addr] >= l.max
}

// Failed counts one failure.
func (l *failLimiter) Failed(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll()
	if _, ok := l.counts[addr]; !ok && len(l.counts) >= l.cap {
		l.counts = map[string]int{}
	}
	l.counts[addr]++
}

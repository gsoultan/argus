package control

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

/*
Where a sign-in came from, for the alert that announces it.

Approximate, and said to be: an IP address names a network, and a network's
registered place is often a city away from the person, or a whole country away
behind a VPN. It is printed beside the address as "(Jakarta, ID, approximate)"
and stored nowhere -- the audit log keeps the address, which is the evidence.

Three ways to know it, chosen in Settings › Alerts:

  - "proxy" (the default): read the place the proxy in front already worked
    out. Cloudflare adds CF-IPCity, CF-Region and CF-IPCountry when "Add
    visitor location headers" is on, and CF-IPCountry by default. Believed
    only from a trusted proxy, the same rule as the address itself. Nothing
    leaves this server.
  - "lookup": ask ipinfo.io. The address is sent to a third party, which is
    why it is not the default, and the answer is cached so one address is
    asked about once a day at most.
  - "off".

Addresses on private, loopback and link-local networks are named as such and
never looked up.
*/

// Locator names an approximate place for a client address, or "".
type Locator interface {
	Locate(ctx context.Context, r *http.Request, ip string) string
}

// LocationMode is how Locate finds a place.
type LocationMode string

const (
	LocationProxy  LocationMode = "proxy"
	LocationLookup LocationMode = "lookup"
	LocationOff    LocationMode = "off"
)

// ValidLocationMode reports whether m is one this build knows. Empty is the
// default, LocationProxy.
func ValidLocationMode(m string) bool {
	switch LocationMode(m) {
	case "", LocationProxy, LocationLookup, LocationOff:
		return true
	}
	return false
}

// NewLocator returns the Locator for mode. trusted reports whether a request
// came from a trusted proxy; nil trusts none.
func NewLocator(mode LocationMode, trusted func(*http.Request) bool, client *http.Client) Locator {
	if trusted == nil {
		trusted = func(*http.Request) bool { return false }
	}
	switch mode {
	case LocationOff:
		return nil
	case LocationLookup:
		if client == nil {
			client = &http.Client{Timeout: 2 * time.Second}
		}
		return &lookupLocator{proxy: proxyLocator{trusted: trusted}, client: client,
			endpoint: "https://ipinfo.io/"}
	default:
		return proxyLocator{trusted: trusted}
	}
}

// privatePlace names an address that is not on the internet.
func privatePlace(ip string) (string, bool) {
	p := net.ParseIP(ip)
	switch {
	case p == nil:
		return "", true
	case p.IsLoopback():
		return "this server", true
	case p.IsPrivate(), p.IsLinkLocalUnicast():
		return "a private network", true
	}
	// Carrier-grade NAT, 100.64.0.0/10: Tailscale and mobile carriers.
	if v4 := p.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return "a private network", true
	}
	return "", false
}

type proxyLocator struct{ trusted func(*http.Request) bool }

func (l proxyLocator) Locate(_ context.Context, r *http.Request, ip string) string {
	if place, ok := privatePlace(ip); ok {
		return place
	}
	if r == nil || !l.trusted(r) {
		return ""
	}
	h := r.Header
	return joinPlace(h.Get("CF-IPCity"), h.Get("CF-Region"), h.Get("CF-IPCountry"))
}

// joinPlace makes "City, Region, CC" from whichever parts are known, and
// refuses parts that could not be place names.
func joinPlace(parts ...string) string {
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Cloudflare's unknown and Tor markers.
		if p == "" || p == "XX" || p == "T1" || len(p) > 64 || strings.ContainsAny(p, "<>\r\n\"") {
			continue
		}
		if len(out) > 0 && strings.EqualFold(out[len(out)-1], p) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ", ")
}

type lookupLocator struct {
	proxy    proxyLocator
	client   *http.Client
	endpoint string

	mu    sync.Mutex
	cache map[string]cachedPlace
}

type cachedPlace struct {
	place string
	at    time.Time
}

const (
	lookupCacheTTL  = 24 * time.Hour
	lookupCacheKeys = 2000
)

func (l *lookupLocator) Locate(ctx context.Context, r *http.Request, ip string) string {
	// What the proxy already knows costs nothing and sends nothing anywhere.
	if place := l.proxy.Locate(ctx, r, ip); place != "" {
		return place
	}
	if _, private := privatePlace(ip); private {
		return ""
	}
	now := time.Now()
	l.mu.Lock()
	if c, ok := l.cache[ip]; ok && now.Sub(c.at) < lookupCacheTTL {
		l.mu.Unlock()
		return c.place
	}
	l.mu.Unlock()

	place := l.ask(ctx, ip)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cache == nil || len(l.cache) >= lookupCacheKeys {
		l.cache = map[string]cachedPlace{}
	}
	l.cache[ip] = cachedPlace{place: place, at: now}
	return place
}

func (l *lookupLocator) ask(ctx context.Context, ip string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		l.endpoint+url.PathEscape(ip)+"/json", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"`
		Bogon   bool   `json:"bogon"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&body); err != nil || body.Bogon {
		return ""
	}
	return joinPlace(body.City, body.Region, body.Country)
}

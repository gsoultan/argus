package ratelimit

import (
	"net"
	"net/http"
	"strings"
)

// ClientResolver turns a request into the key a limit is applied to.
//
// This is where rate limiters are usually broken. X-Forwarded-For is set by the
// client, so trusting it unconditionally hands an attacker two gifts at once:
// they evade their own limit by rotating the header, and they can get anyone
// else throttled by claiming that person's address. It is therefore only
// consulted for requests that actually arrived from a proxy Argus was told to
// trust.
type ClientResolver struct {
	// TrustedProxies are the networks a forwarding header will be believed
	// from. Empty means the header is ignored entirely, which is the correct
	// default for a service reached directly.
	TrustedProxies []*net.IPNet
}

// NewClientResolver parses CIDR strings. An unparseable entry is an error
// rather than a silently skipped one: a typo that quietly disables the trust
// list would be invisible until someone exploited it.
func NewClientResolver(cidrs []string) (*ClientResolver, error) {
	r := &ClientResolver{}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, network, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		r.TrustedProxies = append(r.TrustedProxies, network)
	}
	return r, nil
}

// Key returns the address a limit should be applied to.
func (r *ClientResolver) Key(req *http.Request) string {
	peer := peerIP(req.RemoteAddr)

	// Only believe a forwarding header when the connection itself came from a
	// proxy we trust.
	if r != nil && len(r.TrustedProxies) > 0 && r.trusted(peer) {
		if fwd := forwardedFor(req); fwd != "" {
			return fwd
		}
	}
	return peer
}

func (r *ClientResolver) trusted(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range r.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedFor reads the client address a trusted proxy recorded.
//
// The leftmost entry is the original client; anything after it was added by
// intermediaries. A client can prepend entries, but since this is only reached
// for connections from a trusted proxy, that proxy is expected to have
// overwritten rather than appended.
func forwardedFor(req *http.Request) string {
	if h := req.Header.Get("X-Forwarded-For"); h != "" {
		first, _, _ := strings.Cut(h, ",")
		first = strings.TrimSpace(first)
		if net.ParseIP(first) != nil {
			return first
		}
	}
	// RFC 7239, as emitted by some proxies: for=192.0.2.1
	if h := req.Header.Get("Forwarded"); h != "" {
		for _, part := range strings.Split(h, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(part), "for="); ok {
				v = strings.Trim(v, `"[]`)
				v, _, _ = strings.Cut(v, ",")
				if host, _, err := net.SplitHostPort(v); err == nil {
					v = host
				}
				if net.ParseIP(strings.TrimSpace(v)) != nil {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return ""
}

// peerIP strips the port from a RemoteAddr.
func peerIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// PeerIP exposes the address of the connection itself, ignoring any header.
// Used by the SSH gateway, where there is no proxy header to consider.
func PeerIP(remoteAddr string) string { return peerIP(remoteAddr) }

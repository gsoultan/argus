package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Behind a Cloudflare Tunnel the connector runs on this host, so every request
// from the internet arrives from 127.0.0.1. /stats trusted the peer address
// alone and would have served the process's internals to anyone who found the
// tunnel's hostname.
func TestProcessStatsRefusesATunnelledRequest(t *testing.T) {
	a := NewAPI(nil, nil)
	for _, h := range []string{"CF-Connecting-IP", "X-Forwarded-For", "Forwarded"} {
		r := httptest.NewRequest(http.MethodGet, "/stats", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		r.Header.Set(h, "203.0.113.4")
		w := httptest.NewRecorder()
		a.handleProcessStats(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("loopback peer with %s got %d, want 404", h, w.Code)
		}
	}
}

// The address written into a session record and the sign-in log has to be the
// person's, not the tunnel connector's.
func TestClientAddrBelievesATrustedProxy(t *testing.T) {
	a := NewAPI(nil, nil)
	th, err := NewThrottles(ThrottleConfig{TrustedProxies: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(th.Close)
	a.SetThrottles(th)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:41000"
	r.Header.Set("CF-Connecting-IP", "198.51.100.20")
	if got := a.clientAddr(r); got != "198.51.100.20" {
		t.Errorf("clientAddr = %q, want the address Cloudflare saw", got)
	}

	// And not from anywhere else.
	r.RemoteAddr = "192.0.2.50:41000"
	if got := a.clientAddr(r); got != "192.0.2.50" {
		t.Errorf("clientAddr = %q from an untrusted peer, want the peer", got)
	}
}

// Cloudflare terminates TLS, so this process sees plain HTTP while the
// browser is on HTTPS. HSTS was only ever sent when r.TLS was set, which
// behind a tunnel is never.
func TestHSTSBehindATLSProxy(t *testing.T) {
	a := NewAPI(nil, nil)
	h := a.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS on a plain HTTP response with no proxy in front")
	}

	a.BehindTLSProxy = true
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS behind a TLS-terminating proxy")
	}
}

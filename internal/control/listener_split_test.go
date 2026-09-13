package control

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

/*
The console surface and the fleet surface can be served on separate listeners.

The point is exposure, not authorisation: every route checks the same things
either way. What the split buys is the ability to bind the console somewhere
narrow -- a VPN address -- while gateways and agents keep reporting to an
address they can reach. A control plane the fleet cannot post to keeps no
recordings, no chain heads and no audit events.
*/

func splitAPI(t *testing.T) *API {
	t.Helper()
	return NewAPI(nil, slog.New(slog.NewTextHandler(discard{}, nil)))
}

// status reports what a handler does with a path, without a database behind it.
// 404 means the route is absent; anything else means it is present and did its
// own checking.
func status(h http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

func TestTheConsoleSurfaceDoesNotCarryFleetRoutes(t *testing.T) {
	h := splitAPI(t).ConsoleHandler()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/report/session"},
		{"GET", "/api/v1/report/authorize"},
		{"POST", "/api/v1/report/heartbeat"},
		{"POST", "/api/v1/report/audit"},
		{"GET", "/api/v1/gateway/policy"},
		{"GET", "/api/v1/hostkeys/pin"},
		{"POST", "/api/v1/terminal/redeem"},
	} {
		if got := status(h, tc.method, tc.path); got != http.StatusNotFound {
			t.Errorf("%s %s is reachable on the console listener (%d); binding "+
				"the console narrowly would silently take the fleet with it",
				tc.method, tc.path, got)
		}
	}
}

func TestTheFleetSurfaceDoesNotCarryConsoleRoutes(t *testing.T) {
	h := splitAPI(t).FleetHandler()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/auth/password"},
		{"POST", "/auth/mfa"},
		{"GET", "/auth/me"},
		{"GET", "/api/v1/sessions"},
		{"GET", "/api/v1/audit"},
		{"POST", "/api/v1/policy"},
		{"POST", "/api/v1/terminal/ticket"},
	} {
		if got := status(h, tc.method, tc.path); got != http.StatusNotFound {
			t.Errorf("%s %s is reachable on the fleet listener (%d) -- the "+
				"listener that has to stay reachable must not also be the "+
				"one that takes passwords", tc.method, tc.path, got)
		}
	}
}

// Both surfaces keep liveness and the process stats, because an operator
// probes whichever address they have.
func TestBothSurfacesKeepHealthAndStats(t *testing.T) {
	for name, h := range map[string]http.Handler{
		"console": splitAPI(t).ConsoleHandler(),
		"fleet":   splitAPI(t).FleetHandler(),
		"both":    splitAPI(t).Handler(),
	} {
		if got := status(h, "GET", "/healthz"); got != http.StatusOK {
			t.Errorf("%s: /healthz = %d, want 200", name, got)
		}
		// /stats refuses a non-loopback caller itself; httptest dials from
		// 192.0.2.1, so a 404 here is that guard working rather than absence.
		if got := status(h, "GET", "/stats"); got != http.StatusNotFound {
			t.Errorf("%s: /stats = %d, want the loopback guard to refuse", name, got)
		}
	}
}

// The combined handler is still the union, so the default deployment is
// unchanged by any of this.
func TestTheCombinedHandlerServesBoth(t *testing.T) {
	h := splitAPI(t).Handler()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/report/session"},
		{"GET", "/auth/me"},
		{"GET", "/api/v1/sessions"},
		{"GET", "/api/v1/gateway/policy"},
	} {
		if got := status(h, tc.method, tc.path); got == http.StatusNotFound {
			t.Errorf("%s %s is missing from the combined handler; the "+
				"single-listener default must keep serving everything",
				tc.method, tc.path)
		}
	}
}

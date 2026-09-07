package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func statsServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		cfg:       Config{},
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions:  map[string]*Session{},
		startedAt: time.Now().Add(-90 * time.Second),
	}
}

func getStats(t *testing.T, s *Server, remote string) (*httptest.ResponseRecorder, Stats, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/stats", nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	s.handleStats()(w, r)
	body := w.Body.Bytes()
	var out Stats
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w, out, body
}

// A local caller gets the numbers.
func TestStatsReportsWhatTheProcessIsDoing(t *testing.T) {
	s := statsServer(t)
	s.sessions["a"] = &Session{ID: "a"}
	s.sessions["b"] = &Session{ID: "b"}

	res, got, _ := getStats(t, s, "127.0.0.1:51234")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", got.Sessions)
	}
	if got.Goroutines < 1 {
		t.Errorf("goroutines = %d, want at least 1", got.Goroutines)
	}
	if got.UptimeSeconds < 89 {
		t.Errorf("uptime = %ds, want about 90", got.UptimeSeconds)
	}
	if got.HeapInUseBytes == 0 {
		t.Error("heap in use is zero, which cannot be true of a running process")
	}
	if res.Header().Get("Cache-Control") != "no-store" {
		t.Error("session counts must not be cached")
	}
}

// Anyone else gets nothing, and is not told the endpoint exists.
func TestStatsIsLoopbackOnly(t *testing.T) {
	s := statsServer(t)
	for _, remote := range []string{
		"10.0.0.4:44444",
		"192.168.1.20:44444",
		"[2001:db8::1]:44444",
		"203.0.113.9:80",
	} {
		res, _, _ := getStats(t, s, remote)
		if res.Code != http.StatusNotFound {
			t.Errorf("%s got %d, want 404 -- and 404 rather than 403, so the "+
				"endpoint is not advertised to a caller who may not use it",
				remote, res.Code)
		}
	}
}

// IPv6 loopback is loopback.
func TestStatsAcceptsIPv6Loopback(t *testing.T) {
	s := statsServer(t)
	res, _, _ := getStats(t, s, "[::1]:51234")
	if res.Code != http.StatusOK {
		t.Errorf("::1 got %d, want 200", res.Code)
	}
}

// An address that will not parse is not loopback. Guessing permissively here
// would turn an unfamiliar format into an open endpoint.
func TestStatsRefusesAnUnparseableAddress(t *testing.T) {
	for _, addr := range []string{"", "not-an-address", "garbage:port:extra", "example.com:443"} {
		if isLoopback(addr) {
			t.Errorf("isLoopback(%q) = true", addr)
		}
	}
	// And the ones that must work.
	for _, addr := range []string{"127.0.0.1:1", "[::1]:1", "127.0.0.1", "::1"} {
		if !isLoopback(addr) {
			t.Errorf("isLoopback(%q) = false", addr)
		}
	}
}

// The endpoint must never grow a profile handler. A heap dump of this process
// is a transcript of every live session, including typed passwords.
func TestStatsCarriesNoProfileData(t *testing.T) {
	s := statsServer(t)
	_, _, body := getStats(t, s, "127.0.0.1:1")

	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k, v := range generic {
		switch v.(type) {
		case float64:
		default:
			t.Errorf("field %q is %T; this endpoint reports counts only, and "+
				"anything else risks carrying session content off the host", k, v)
		}
	}
}

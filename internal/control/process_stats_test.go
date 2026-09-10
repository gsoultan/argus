package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A local caller gets the numbers; nobody else learns the endpoint exists.
func TestProcessStatsIsLoopbackOnly(t *testing.T) {
	a := NewAPI(nil, nil)

	local := httptest.NewRequest(http.MethodGet, "/stats", nil)
	local.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	a.handleProcessStats(w, local)
	if w.Code != http.StatusOK {
		t.Fatalf("loopback got %d, want 200", w.Code)
	}
	var st ProcessStats
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Goroutines < 1 {
		t.Errorf("goroutines = %d", st.Goroutines)
	}
	if st.HeapInUseBytes == 0 {
		t.Error("heap in use is zero, which cannot be true of a running process")
	}

	for _, remote := range []string{"10.1.2.3:5000", "[2001:db8::9]:5000", "bogus"} {
		r := httptest.NewRequest(http.MethodGet, "/stats", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		a.handleProcessStats(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s got %d, want 404", remote, w.Code)
		}
	}
}

// A nil store must not panic the endpoint: it is the thing you reach for when
// the database is the problem.
func TestProcessStatsSurvivesAMissingStore(t *testing.T) {
	a := NewAPI(nil, nil)
	st := a.processStats()
	if st.DBConns != 0 || st.DBIdle != 0 {
		t.Errorf("expected zero pool counts with no store, got %d/%d", st.DBConns, st.DBIdle)
	}
}

// Counts only. A field that is not a number is a field that could carry data
// off the host.
func TestProcessStatsCarriesNoPayload(t *testing.T) {
	a := NewAPI(nil, nil)
	body, err := json.Marshal(a.processStats())
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatal(err)
	}
	for k, v := range generic {
		if _, ok := v.(float64); !ok {
			t.Errorf("field %q is %T; this endpoint reports counts only", k, v)
		}
	}
}

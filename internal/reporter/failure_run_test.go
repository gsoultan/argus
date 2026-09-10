package reporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A drained spool is proof the control plane is reachable.
//
// Drain delivered without ending the failure run, so the alarm an operator had
// seen never cleared, and the next single transient failure raised it again --
// timed from the outage that had already ended.
func TestDrainingTheSpoolEndsTheFailureRun(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	var logs strings.Builder
	c := New(ts.URL, "tok", filepath.Join(t.TempDir(), "spool.jsonl"),
		slog.New(slog.NewTextHandler(&logs, nil)))

	// An outage long enough to have raised the alarm.
	c.mu.Lock()
	c.failures = 12
	c.firstFailure = time.Now().Add(-2 * escalateAfter)
	c.mu.Unlock()

	c.spool(spoolEntry{Path: "/api/v1/report/session",
		Body: []byte(`{"id":"sess-1"}`), At: time.Now().UTC()})

	if n := c.Drain(context.Background()); n != 1 {
		t.Fatalf("delivered %d, want 1 -- logs:\n%s", n, logs.String())
	}

	c.mu.Lock()
	failures := c.failures
	c.mu.Unlock()
	if failures != 0 {
		t.Errorf("failures = %d after the spool drained; the run outlived the "+
			"outage, so one later blip re-raises an alarm measured from this one",
			failures)
	}
	if !strings.Contains(logs.String(), "reachable again") {
		t.Errorf("no 'reachable again' line after delivery resumed -- an "+
			"operator who saw the alarm never sees it clear. logs:\n%s", logs.String())
	}
}

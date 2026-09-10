package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/reporter"
)

// A browser-brokered Remote Desktop session reports the same risk signals as a
// proxied one.
//
// It reported a hardcoded empty list, so an administrator logon through the
// console, onto an unpinned host, whose recording cut off part-way, arrived at
// the console carrying no flags at all and read as an ordinary session.
func TestABrowserRDPSessionReportsItsRiskFlags(t *testing.T) {
	bodies := make(chan map[string]any, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var rec map[string]any
		_ = json.Unmarshal(raw, &rec)
		bodies <- rec
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	srv := testServer(t, t.TempDir())
	srv.cfg.Reporter = reporter.New(ts.URL, "tok",
		filepath.Join(t.TempDir(), "spool.jsonl"), srv.log)

	sess := &rdp.Session{
		ID: "rdp-web-1", User: "lin@northwind.id", Principal: "administrator",
		Target: "win-01", StartedAt: time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC),
	}
	srv.reportRDPWeb(sess, "", "closed")
	srv.reports.Wait()

	var rec map[string]any
	select {
	case rec = <-bodies:
	case <-time.After(5 * time.Second):
		t.Fatal("no session report reached the control plane")
	}

	flags, _ := rec["riskFlags"].([]any)
	got := map[string]bool{}
	for _, f := range flags {
		got[f.(string)] = true
	}
	for _, want := range []string{"root-principal", "unpinned-host-key", "off-hours"} {
		if !got[want] {
			t.Errorf("riskFlags = %v, missing %q", flags, want)
		}
	}
}

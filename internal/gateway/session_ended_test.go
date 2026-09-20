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

// A session that ended says so, whether or not its recording was sealed.
//
// `endedAt` used to be written inside the `chainHead != ""` branch, and
// Session.Close returns ("", nil) when there is no recorder — no error, no
// head. So a session that ended without one was reported with a terminal state
// and no end time, and the row said "terminated" and "never ended" at once. In
// dev that was 26 of 56 terminations, against 12,276 of 12,276 ordinary closes
// sealed correctly: the split fell on exactly the sessions an administrator had
// stopped because something was wrong.
//
// The console reads a null end time as still-running and showed those sessions
// with elapsed times in the hundreds of hours, and fidelityUnsupported skips
// any session with a nil EndedAt, so they were exempt from the evidence check
// by accident too.
func TestASessionThatEndedReportsAnEndTimeEvenUnsealed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string
		chainHead string
		wantEnded bool
	}{
		{"terminated without a seal", "terminated", "", true},
		{"closed without a seal", "closed", "", true},
		{"closed with a seal", "closed", "abc123", true},
		// The one case that must not carry an end time: it has not ended.
		{"still running", "active", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := captureReport(t, func(srv *Server) {
				sess := &rdp.Session{
					ID: "s-1", User: "lin@northwind.id", Principal: "ops",
					Target:    "win-01",
					StartedAt: time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC),
				}
				srv.reportRDPWeb(sess, tc.chainHead, tc.state)
			})

			_, gotEnded := rec["endedAt"]
			if gotEnded != tc.wantEnded {
				t.Errorf("state=%q chainHead=%q: endedAt present = %v, want %v",
					tc.state, tc.chainHead, gotEnded, tc.wantEnded)
			}

			// The seal keeps its own branch. An unsealed recording has no head,
			// because that is true of it — what changed is only that the
			// session stops claiming to be running.
			if _, gotHead := rec["chainHead"]; gotHead != (tc.chainHead != "") {
				t.Errorf("chainHead present = %v, want %v", gotHead, tc.chainHead != "")
			}
		})
	}
}

// captureReport runs report and returns the one record that reached the
// control plane.
func captureReport(t *testing.T, report func(*Server)) map[string]any {
	t.Helper()

	bodies := make(chan map[string]any, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var rec map[string]any
		_ = json.Unmarshal(raw, &rec)
		bodies <- rec
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ts.Close)

	srv := testServer(t, t.TempDir())
	srv.cfg.Reporter = reporter.New(ts.URL, "tok",
		filepath.Join(t.TempDir(), "spool.jsonl"), srv.log)

	report(srv)
	srv.reports.Wait()

	select {
	case rec := <-bodies:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("no session report reached the control plane")
		return nil
	}
}

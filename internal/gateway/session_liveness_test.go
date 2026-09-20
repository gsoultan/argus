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

// The keepalive has to cover every registry that can report a session active.
//
// Three of them do -- SSH and browser terminals on Server.sessions, browser
// desktops on Server.rdpWeb, proxied desktops on RDPServer.sessions -- and a
// keepalive that walks only the first leaves the other two looking abandoned
// three minutes in, while somebody is still sitting in front of them. The
// control plane cannot tell that apart from the real failure this exists to
// expose, so a missed registry does not degrade the feature, it inverts it.
func TestTheKeepaliveCoversEveryKindOfSession(t *testing.T) {
	bodies := make(chan map[string]any, 16)
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

	started := time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC)

	// One in each registry, reached the way the serving paths reach them.
	srv.trackSession(&Session{
		ID: "ssh-1", User: "lin@northwind.id", Principal: "ops",
		Target: Asset{Hostname: "pay-01"}, StartedAt: started, srv: srv,
	})
	srv.trackRDPWeb(&rdp.Session{
		ID: "rdpweb-1", User: "lin@northwind.id", Principal: "administrator",
		Target: "win-01", StartedAt: started,
	})
	rdpSrv := &RDPServer{srv: srv, sessions: map[string]*rdp.Session{}}
	rdpSrv.track(&rdp.Session{
		ID: "rdp-1", User: "lin@northwind.id", Principal: "administrator",
		Target: "win-02", StartedAt: started,
	})

	if n := reportLiveOnce(srv, rdpSrv); n != 3 {
		t.Fatalf("reported %d sessions, want 3", n)
	}
	srv.reports.Wait()

	seen := map[string]map[string]any{}
	for len(seen) < 3 {
		select {
		case rec := <-bodies:
			id, _ := rec["id"].(string)
			seen[id] = rec
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 keepalives reached the control plane: %v", len(seen), keys(seen))
		}
	}

	for _, id := range []string{"ssh-1", "rdpweb-1", "rdp-1"} {
		rec, ok := seen[id]
		if !ok {
			t.Errorf("no keepalive for %q", id)
			continue
		}
		// A keepalive says the session is still here. It must not say anything
		// that would end it: an end time or a chain head on an active row is
		// the contradiction the previous change went to some trouble to outlaw.
		if got := rec["state"]; got != "active" {
			t.Errorf("%s: state = %v, want active", id, got)
		}
		if _, ends := rec["endedAt"]; ends {
			t.Errorf("%s: keepalive carried an end time", id)
		}
		if head, sealed := rec["chainHead"]; sealed && head != "" {
			t.Errorf("%s: keepalive carried a chain head %v", id, head)
		}
	}
}

// A gateway with no RDP listener configured passes a nil *RDPServer.
func TestTheKeepaliveRunsWithoutAnRDPServer(t *testing.T) {
	srv := testServer(t, t.TempDir())
	if n := reportLiveOnce(srv, nil); n != 0 {
		t.Fatalf("reported %d sessions on an idle gateway, want 0", n)
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

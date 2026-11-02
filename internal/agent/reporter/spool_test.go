package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/*
What happens while the control plane is unreachable.

Reports spool so a restart does not lose the sessions that happened during it.
That was unbounded, on the host that must keep recording: a control plane
unreachable for a week -- which is what a misconfigured agent does, silently --
grows the file without limit. A full disk stops the recorder, and a session
that cannot be recorded is terminated, so an unreachable control plane
eventually takes privileged access down with it.

Measured on the dev fleet before this: 11.9 MB and climbing, from an agent that
had been failing for seven days with nothing but warning lines to show for it.
*/

func spoolClient(t *testing.T, max int64) (*Client, string, *strings.Builder) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.jsonl")
	var logs strings.Builder
	c := New("https://127.0.0.1:1", "tok", path,
		slog.New(slog.NewTextHandler(&logs, nil)))
	c.MaxSpoolBytes = max
	return c, path, &logs
}

// The spool stops growing, and says what it dropped.
func TestTheSpoolIsBounded(t *testing.T) {
	c, path, logs := spoolClient(t, 8<<10) // 8 KiB
	big := strings.Repeat("x", 512)

	for i := 0; i < 200; i++ {
		c.spool(spoolEntry{Path: "/api/v1/report/session",
			Body: []byte(`{"pad":"` + big + `"}`), At: time.Now().UTC()})
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 8<<10 {
		t.Errorf("spool is %d bytes, over its %d limit -- it can still fill the disk",
			fi.Size(), 8<<10)
	}
	if !strings.Contains(logs.String(), "spool is full") {
		t.Error("dropping evidence has to be said out loud")
	}
}

// What survives must still be readable: a trim that leaves a half-written line
// would make the whole spool unusable, which is worse than the overflow.
func TestTrimmingLeavesWholeRecords(t *testing.T) {
	c, path, _ := spoolClient(t, 4<<10)
	for i := 0; i < 120; i++ {
		c.spool(spoolEntry{Path: "/api/v1/report/audit",
			Body: []byte(`{"n":` + strings.Repeat("9", 200) + `}`), At: time.Now().UTC()})
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e spoolEntry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("a spooled record did not survive trimming: %v\nline: %.80s", err, line)
		}
		n++
	}
	if n == 0 {
		t.Error("trimming left nothing at all")
	}
}

// A sustained outage stops being a warning and becomes an alarm.
func TestASustainedOutageEscalates(t *testing.T) {
	c, _, logs := spoolClient(t, 1<<20)

	// Backdate the run so the escalation threshold is crossed.
	c.noteFailure(io.EOF)
	c.mu.Lock()
	c.firstFailure = time.Now().Add(-30 * time.Minute)
	c.lastEscalate = time.Now().Add(-30 * time.Minute)
	c.mu.Unlock()
	c.noteFailure(io.EOF)

	if !strings.Contains(logs.String(), "sustained period") {
		t.Errorf("a half-hour outage produced no alarm:\n%s", logs.String())
	}
}

// And clears, so an operator who saw the alarm sees it end.
func TestRecoveryIsAnnounced(t *testing.T) {
	c, _, logs := spoolClient(t, 1<<20)
	c.noteFailure(io.EOF)
	c.mu.Lock()
	c.firstFailure = time.Now().Add(-20 * time.Minute)
	c.mu.Unlock()
	c.noteSuccess()

	if !strings.Contains(logs.String(), "reachable again") {
		t.Errorf("recovery was not announced:\n%s", logs.String())
	}
}

// A brief blip must not cry wolf: reports are frequent and a restart is normal.
func TestABriefOutageDoesNotEscalate(t *testing.T) {
	c, _, logs := spoolClient(t, 1<<20)
	for i := 0; i < 20; i++ {
		c.noteFailure(io.EOF)
	}
	if strings.Contains(logs.String(), "sustained period") {
		t.Error("a few seconds of failures raised the alarm")
	}
}

// Delivery clears the failure run, so a working fleet never escalates.
func TestDeliveryClearsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var logs strings.Builder
	c := New(srv.URL, "tok", filepath.Join(t.TempDir(), "s.jsonl"),
		slog.New(slog.NewTextHandler(&logs, nil)))

	c.post(context.Background(), "/api/v1/report/heartbeat", map[string]any{"hostname": "h"})
	c.mu.Lock()
	f := c.failures
	c.mu.Unlock()
	if f != 0 {
		t.Errorf("a successful delivery left %d failures on the counter", f)
	}
}

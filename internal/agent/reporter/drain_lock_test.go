package reporter

import (
	"context"
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
Drain used to hold c.mu across every send in the pass.

post() reaches spool(), which takes the same mutex, so a control plane slow
enough to need draining was also slow enough to stall every session reporting
through it -- each one waiting behind the whole backlog rather than behind one
request. The same shape as the upload queue holding its lock across a 60s
upload, in a different package.
*/

// A report can reach the spool while a drain is on the network.
func TestSpoolingIsNotBlockedByADrainInFlight(t *testing.T) {
	sending := make(chan struct{})
	release := make(chan struct{})
	var once bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !once {
			once = true
			close(sending)
			<-release // a control plane that has accepted the request and gone quiet
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	dir := t.TempDir()
	c := New(ts.URL, "tok", filepath.Join(dir, "spool.jsonl"),
		slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))

	c.spool(spoolEntry{Path: "/api/v1/report/session",
		Body: []byte(`{"id":"already-spooled"}`), At: time.Now().UTC()})

	done := make(chan int, 1)
	go func() { done <- c.Drain(context.Background()) }()
	<-sending

	// The drain is mid-request. A session ending right now must still be able
	// to write its report to disk.
	spooled := make(chan struct{})
	go func() {
		c.spool(spoolEntry{Path: "/api/v1/report/session",
			Body: []byte(`{"id":"arrived-during-the-drain"}`), At: time.Now().UTC()})
		close(spooled)
	}()

	select {
	case <-spooled:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("spooling blocked behind a drain that was waiting on the " +
			"network -- every session reporting through this client stalls " +
			"for as long as the control plane is slow")
	}
	close(release)
	<-done

	// And the entry that arrived mid-pass survived the rewrite that ended it.
	body, err := os.ReadFile(c.spoolPath)
	if err != nil {
		t.Fatalf("the spool is gone: %v", err)
	}
	if !strings.Contains(string(body), "arrived-during-the-drain") {
		t.Errorf("a report spooled during the drain was dropped by its "+
			"rewrite; spool now holds:\n%s", body)
	}
	if strings.Contains(string(body), "already-spooled") {
		t.Error("a delivered report is still in the spool and will be sent twice")
	}
}

// Two drains do not run at once.
func TestASecondDrainDoesNotOverlapTheFirst(t *testing.T) {
	sending := make(chan struct{})
	release := make(chan struct{})
	var seen int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen++
		if seen == 1 {
			close(sending)
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", filepath.Join(t.TempDir(), "spool.jsonl"),
		slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
	c.spool(spoolEntry{Path: "/api/v1/report/session",
		Body: []byte(`{"id":"one"}`), At: time.Now().UTC()})

	first := make(chan int, 1)
	go func() { first <- c.Drain(context.Background()) }()
	<-sending

	if n := c.Drain(context.Background()); n != 0 {
		t.Errorf("a second drain delivered %d while the first was still "+
			"sending; the same entry would go twice", n)
	}
	close(release)
	if n := <-first; n != 1 {
		t.Errorf("the first drain delivered %d, want 1", n)
	}
}

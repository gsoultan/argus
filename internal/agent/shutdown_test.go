package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

/*
The agent seals recordings on the way out.

A shim stays attached for the life of the shell it records, so on any host with
someone logged in the collector's shutdown waited for a session that was not
going to end. systemd sent SIGKILL, and SIGKILL means the recording is never
sealed: no chain head, nothing to verify it against. On a fleet, an upgrade did
that to every host with an active session at once.

This is the shutdown that happens most often -- once per host, per upgrade.
*/

// testCollector starts a collector and returns it once its socket is live.
//
// The socket lives under a short path on purpose: a unix socket address is
// capped near 104 bytes, and t.TempDir() on macOS is long enough on its own to
// push past it. The failure is a bare "invalid argument" on dial.
func testCollector(t *testing.T) (*Collector, string) {
	t.Helper()
	c := NewCollector(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	sockDir, err := os.MkdirTemp("/tmp", "argus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "a.sock")

	go func() { _ = c.Listen(sock) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return c, sock
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the collector never created its socket")
	return nil, ""
}

// attachShim opens a session and leaves the connection open, as a real shim
// does for the life of the shell it is recording.
func attachShim(t *testing.T, sock string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	start := map[string]any{
		"type": "start", "user": "lin@northwind.id", "principal": "ops",
		"hostname": "db-01", "origin": "direct", "clientAddr": "10.0.0.9",
		"startedAt": time.Now().UTC(), "cols": 80, "rows": 24,
	}
	if err := json.NewEncoder(conn).Encode(start); err != nil {
		t.Fatal(err)
	}
	return conn
}

// A shim that never detaches must not hold the shutdown open.
func TestShutdownDisconnectsShimsThatOutlastTheDrain(t *testing.T) {
	c, sock := testCollector(t)
	conn := attachShim(t, sock)
	defer conn.Close()
	time.Sleep(200 * time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- c.CloseWithin(100 * time.Millisecond) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the agent shutdown never returned; one attached shim would " +
			"hold the host until SIGKILL")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("shutdown took %s; the shim was never disconnected and the "+
			"grace period merely expired", d)
	}
}

// An idle agent stops at once rather than sitting out the window.
func TestAgentShutdownIsImmediateWhenIdle(t *testing.T) {
	c, _ := testCollector(t)
	start := time.Now()
	if err := c.CloseWithin(5 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("an idle agent shutdown took %s", d)
	}
}

// Close must be safe to call more than once; a signal handler and a defer both
// reach it.
func TestAgentCloseIsIdempotent(t *testing.T) {
	c, _ := testCollector(t)
	done := make(chan struct{})
	go func() {
		_ = c.Close()
		_ = c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("a second Close deadlocked")
	}
}

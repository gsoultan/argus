//go:build !windows

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The shim runs only where sshd does, so these are not built on Windows.

// A collector holding for a restart tells a login to wait, and the shim's
// openRecording waits for the agent that replaces it rather than refusing.
func TestALoginDuringAnUpdateRestartWaitsForTheNewAgent(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "argus-upd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")

	old := NewCollector(filepath.Join(dir, "rec-old"), nil)
	go func() { _ = old.Listen(sock) }()
	waitSocket(t, sock)
	old.Hold(true)

	start := SessionStart{Type: "start", Principal: "ops", Hostname: "h", StartedAt: time.Now()}
	got := make(chan *recording, 1)
	go func() {
		rc, _ := openRecording(sock, start, restartWait, false)
		got <- rc
	}()

	// The old agent goes away after a moment, as a restart does, and a new
	// one comes up well after the five seconds a stopped agent gets.
	time.Sleep(300 * time.Millisecond)
	_ = old.CloseWithin(time.Second)
	time.Sleep(restartWait + time.Second)
	fresh := NewCollector(filepath.Join(dir, "rec-new"), nil)
	go func() { _ = fresh.Listen(sock) }()
	t.Cleanup(func() { _ = fresh.CloseWithin(time.Second) })

	select {
	case rc := <-got:
		if rc == nil {
			t.Fatal("the login was refused during the update instead of waiting")
		}
		if !rc.d.record() || rc.d.sessionID == "" {
			t.Errorf("the new agent should record it: %+v", rc.d)
		}
		_ = rc.conn.Close()
	case <-time.After(30 * time.Second):
		t.Fatal("the login never got through")
	}
}

// With no word of an update, a collector that is not there is reported down
// after a short wait, so break-glass is not held up by a stopped agent.
func TestALoginToAStoppedAgentIsNotHeldLong(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "argus-upd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	began := time.Now()
	rc, why := openRecording(filepath.Join(dir, "none.sock"), SessionStart{Type: "start"}, restartWait, false)
	if rc != nil || why == "" {
		t.Fatal("a missing agent recorded a login")
	}
	if took := time.Since(began); took > restartWait+2*time.Second {
		t.Errorf("a stopped agent held the login %s", took)
	}
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("collector never listened")
}

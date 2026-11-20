//go:build !windows

package agent

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// A login whose agent restarts under it tells the agent that replaces it
// every id it had, so watching any of them finds it.
func TestALoginResumedAfterARestartNamesWhatItContinues(t *testing.T) {
	first, sock := testCollector(t)
	first.SetDirectSSH(DirectSSHAllow)
	start := SessionStart{Type: "start", Principal: "ops", Hostname: "db-01", Origin: Direct,
		ClientAddr: "10.0.0.9", StartedAt: time.Now().UTC(), Cols: 80, Rows: 24}
	rc, why := openRecording(sock, start, 3*time.Second, false)
	if rc == nil || !rc.d.record() {
		t.Fatalf("not recorded: %s", why)
	}
	st := newStream(rc, rc.d.sessionID)
	st.socket, st.start = sock, start
	firstID := rc.d.sessionID

	// The first agent goes; its socket is taken over by the next.
	_ = first.CloseWithin(100 * time.Millisecond)
	_ = os.Remove(sock)
	next := NewCollector(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	next.SetDirectSSH(DirectSSHAllow)
	go func() { _ = next.Listen(sock) }()
	defer next.CloseWithin(time.Second)

	deadline := time.Now().Add(10 * time.Second)
	for {
		st.frame("o", []byte("after-the-restart\r\n"))
		if s, why := next.watchable(firstID, "ops"); s != nil {
			if got := continuesFrom(s.start); len(got) != 1 || got[0] != firstID {
				t.Fatalf("continues %v, want [%s]", got, firstID)
			}
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("never resumed: %s", why)
		}
		time.Sleep(50 * time.Millisecond)
	}
	st.close(0)
}

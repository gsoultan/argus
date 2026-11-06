package control

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/rdp"
)

// A viewer whose link cannot keep up is dropped by the hub, so the person
// working is never held up. For a desktop that used to end the view; now it
// is repainted and carries on from the screen as it stands.
func TestADesktopViewerThatFallsBehindIsCaughtUpNotClosed(t *testing.T) {
	a := &API{}
	sess := &liveSession{tenant: "t1", id: "s1", protocol: "rdp", hub: live.NewHub(),
		width: 1024, height: 768}
	var mu sync.Mutex
	refreshes := 0
	sess.refresh = func() {
		mu.Lock()
		refreshes++
		mu.Unlock()
		sess.hub.Broadcast([]byte("repaint"))
	}
	a.live.add(sess)

	// The viewer's link stalls on its first update, long enough for the hub
	// to give up on it, then recovers.
	stall := make(chan struct{})
	got := make(chan []byte, 1024)
	var once sync.Once
	write := func(_ websocket.MessageType, b []byte) error {
		if string(b) == "busy" {
			once.Do(func() { <-stall })
		}
		got <- append([]byte(nil), b...)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan bool, 1)
	go func() { ended <- a.watchDesktop(ctx, sess, write) }()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case b := <-got:
				if string(b) == want {
					return
				}
			case <-deadline:
				t.Fatalf("no %q", want)
			}
		}
	}
	waitFor("repaint")
	// More than the hub holds for one viewer, while it is stalled.
	for i := 0; i < 1000; i++ {
		sess.hub.Broadcast([]byte("busy"))
	}
	close(stall)

	// Caught up: told the size again and repainted, not closed.
	deadline := time.After(5 * time.Second)
	sawResize := false
	for !sawResize {
		select {
		case b := <-got:
			if len(b) >= rdp.FrameHeaderSize && b[0] == rdp.FrameResize {
				sawResize = true
			}
		case <-deadline:
			t.Fatal("the viewer was not caught up")
		}
	}
	waitFor("repaint")
	mu.Lock()
	if refreshes != 2 {
		t.Errorf("%d repaints, want 2", refreshes)
	}
	mu.Unlock()
	select {
	case <-ended:
		t.Fatal("the view ended while the session runs")
	default:
	}

	// The session ending ends the view, and says so.
	a.live.remove(sess)
	select {
	case e := <-ended:
		if !e {
			t.Error("the session's end was reported as the viewer leaving")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the view did not end with the session")
	}
	cancel()
}

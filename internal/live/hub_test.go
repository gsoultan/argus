package live

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestBroadcastReachesEveryViewerInOrder(t *testing.T) {
	h := NewHub()
	_, a, cancelA := h.Subscribe()
	_, b, cancelB := h.Subscribe()
	defer cancelA()
	defer cancelB()

	for i := range 5 {
		h.Broadcast([]byte(fmt.Sprintf("frame-%d", i)))
	}

	for name, ch := range map[string]<-chan []byte{"a": a, "b": b} {
		for i := range 5 {
			want := fmt.Sprintf("frame-%d", i)
			select {
			case got := <-ch:
				if string(got) != want {
					t.Fatalf("viewer %s: got %q, want %q", name, got, want)
				}
			case <-time.After(time.Second):
				t.Fatalf("viewer %s: no frame %d", name, i)
			}
		}
	}
}

// The property the whole design rests on: shadowing must be invisible to the
// session being shadowed. A viewer that stops reading must not slow the
// broadcaster down, or a privileged session's throughput would depend on
// whoever happens to be watching it.
func TestStalledViewerNeverBlocksBroadcast(t *testing.T) {
	h := NewHub()
	_, stalled, cancel := h.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than the subscriber buffer holds.
		for i := range subBuffer * 4 {
			h.Broadcast([]byte(fmt.Sprintf("%d", i)))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast blocked on a viewer that stopped reading")
	}

	// The stalled viewer is dropped, not merely starved: draining it must reach
	// a closed channel rather than block forever.
	deadline := time.After(time.Second)
	for {
		select {
		case _, open := <-stalled:
			if !open {
				return // dropped, as intended
			}
		case <-deadline:
			t.Fatal("stalled viewer was neither served nor dropped")
		}
	}
}

func TestBacklogIsBoundedAndReplayed(t *testing.T) {
	h := NewHub()

	frame := make([]byte, 8<<10)
	for i := range frame {
		frame[i] = 'x'
	}
	for range 200 { // 1.6 MB through a 256 KB backlog
		h.Broadcast(append([]byte(nil), frame...))
	}

	backlog, _, cancel := h.Subscribe()
	defer cancel()

	var total int
	for _, f := range backlog {
		total += len(f)
	}
	if total > MaxBacklogBytes {
		t.Fatalf("backlog holds %d bytes, over the %d cap", total, MaxBacklogBytes)
	}
	if len(backlog) == 0 {
		t.Fatal("backlog is empty; a viewer joining an idle session would see nothing")
	}
}

// A viewer that attaches to a session ending at the same moment should see the
// tail and a clean close, not an error that looks like a fault.
func TestSubscribeAfterCloseYieldsBacklogAndEnds(t *testing.T) {
	h := NewHub()
	h.Broadcast([]byte(`[0.1,"o","hello"]`))
	h.Close()

	backlog, frames, cancel := h.Subscribe()
	defer cancel()

	if len(backlog) != 0 {
		t.Fatalf("Close should drop the backlog, got %d frames", len(backlog))
	}
	select {
	case _, open := <-frames:
		if open {
			t.Fatal("a hub closed before subscribing must not deliver frames")
		}
	case <-time.After(time.Second):
		t.Fatal("channel never closed; the viewer would hang on a finished session")
	}
}

func TestCloseEndsLiveSubscriptions(t *testing.T) {
	h := NewHub()
	_, frames, cancel := h.Subscribe()
	defer cancel()

	h.Broadcast([]byte("one"))
	h.Close()

	// Buffered frames still drain, then the channel closes.
	var sawFrame bool
	for {
		select {
		case f, open := <-frames:
			if !open {
				if !sawFrame {
					t.Fatal("frames sent before Close were lost")
				}
				return
			}
			sawFrame = string(f) == "one"
		case <-time.After(time.Second):
			t.Fatal("Close did not end the subscription")
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	h := NewHub()
	h.Close()
	h.Close() // must not panic on a double close of the same channels
	h.Broadcast([]byte("after close"))
	if n := h.Viewers(); n != 0 {
		t.Fatalf("Viewers() = %d after Close, want 0", n)
	}
}

func TestConcurrentSubscribeAndBroadcast(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup

	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_, _, cancel := h.Subscribe()
				cancel()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			h.Broadcast([]byte(fmt.Sprintf("%d", i)))
		}
	}()

	wg.Wait()
	h.Close()
}

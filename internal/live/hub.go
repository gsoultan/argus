// Package live fans out in-flight session output to read-only viewers.
//
// Shadowing is what separates a system that explains what happened from one
// that can intervene while it is still happening. Until now Argus was purely
// retrospective: an auditor could prove what a session did, but only after it
// had finished doing it.
//
// The frames a shadower sees are the ones being committed to the recording —
// the tap sits on the recorder, not on the relay — so what someone watches live
// and what they replay afterwards cannot diverge. A shadow view that could
// disagree with the recording would be worse than no shadow view at all, since
// an operator would have made a decision on evidence that no longer exists.
package live

import (
	"sync"
)

// MaxBacklogBytes bounds the replay buffer held for viewers that join late.
//
// Without a backlog, shadowing an idle session shows a blank terminal, and the
// viewer cannot tell an idle session from a broken shadow. With an unbounded
// one, a session printing a large file would let any viewer's arrival pin that
// output in gateway memory. A screenful or two is the useful amount.
const MaxBacklogBytes = 256 << 10

// subBuffer is how many frames a viewer may fall behind before it is dropped.
//
// Dropping the viewer rather than dropping frames is deliberate. A shadow that
// silently skipped output would show an operator an incomplete picture of a
// session they may be about to terminate. Being told the stream could not keep
// up is recoverable; acting on a partial view is not.
const subBuffer = 256

// Hub broadcasts one session's frames to zero or more viewers.
//
// The governing constraint is that a viewer must never be able to slow down,
// block or break the session being watched. Every send is non-blocking and the
// hub never waits on a consumer, so shadowing has no effect a target could
// notice — which also means it cannot be used to detect that it is happening.
type Hub struct {
	mu           sync.Mutex
	subs         map[uint64]chan []byte
	nextID       uint64
	backlog      [][]byte
	backlogBytes int
	closed       bool
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[uint64]chan []byte)}
}

// Subscribe returns the recent backlog and a channel of subsequent frames.
//
// Both come from the same critical section: taking the backlog and registering
// the subscription separately would let a frame emitted in between be lost or
// delivered twice, and a viewer cannot tell either from a session that simply
// went quiet.
//
// The channel is closed when the session ends or when this viewer falls too far
// behind. Callers must invoke cancel to release the subscription.
func (h *Hub) Subscribe() (backlog [][]byte, frames <-chan []byte, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	backlog = make([][]byte, len(h.backlog))
	copy(backlog, h.backlog)

	ch := make(chan []byte, subBuffer)
	if h.closed {
		// The session ended between the caller looking it up and subscribing.
		// Hand back the backlog and an already-closed channel so the viewer
		// sees the tail of the session and a clean end, rather than an error
		// that looks like a fault.
		close(ch)
		return backlog, ch, func() {}
	}

	id := h.nextID
	h.nextID++
	h.subs[id] = ch

	return backlog, ch, func() { h.unsubscribe(id) }
}

func (h *Hub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// Broadcast delivers one recorded frame to every viewer.
//
// Suitable as a recorder tap: it never blocks, so it is safe to call while the
// recorder holds its lock, and calling it under that lock is what guarantees
// viewers see frames in the same order the chain committed them.
//
// The frame must not be mutated afterwards; the recorder hands over a copy.
func (h *Hub) Broadcast(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}

	h.backlog = append(h.backlog, frame)
	h.backlogBytes += len(frame)
	for h.backlogBytes > MaxBacklogBytes && len(h.backlog) > 1 {
		h.backlogBytes -= len(h.backlog[0])
		h.backlog[0] = nil
		h.backlog = h.backlog[1:]
	}

	for id, ch := range h.subs {
		select {
		case ch <- frame:
		default:
			// This viewer is not keeping up. Close it rather than block: the
			// session's throughput is not negotiable, and a stalled viewer must
			// not become back-pressure on a privileged session.
			delete(h.subs, id)
			close(ch)
		}
	}
}

// Close ends every subscription. Safe to call more than once.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, ch := range h.subs {
		delete(h.subs, id)
		close(ch)
	}
	h.backlog, h.backlogBytes = nil, 0
}

// Viewers reports how many shadowers are attached.
//
// Surfaced so the console can show that a session is being watched. Whether the
// person being shadowed is told is a policy decision that belongs to the
// deployment, not to the gateway; some jurisdictions require disclosure.
func (h *Hub) Viewers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

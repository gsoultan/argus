package rdp

import (
	"sync"
	"time"
)

/*
Pacing a Remote Desktop by the browser that draws it.

The agent sits beside the target, on its network; the person watching may be
behind a link a hundred times slower. Left to itself the agent reads a frame
the moment Windows sends it and tells Windows it is done with, so Windows
sees a client on a LAN and sends at full rate and quality. Everything the
slow link cannot carry then waits in the buffers between here and the
browser, megabytes of them, and the picture a person sees is the one from
that many seconds ago. That queue, not the codec, is what a slow link feels
as lag.

So a frame is done with when the browser has drawn it, not when the agent
has read it. Each frame the target sends is followed by a mark (EncodeMark);
the browser acknowledges a mark once everything before it is decoded.

  - On the graphics pipeline, Windows' own frame acknowledgement is held
    until then. Windows counts the frames it has not heard back about, slows
    to what comes back, and so encodes for the link the person really has.
  - Bitmap updates have no acknowledgement in the protocol. There the agent
    stops reading from the target while more than a window of bytes is
    unacknowledged; Windows, unable to send, keeps the screen's changes
    together and sends them as they are when there is room again. The window
    follows the time acknowledgements take: it grows while they come back
    promptly and shrinks when they start to queue.

A browser that never acknowledges (an older console, a tool) is not paced at
all, as before. One that did and stopped is given up on a frame at a time
after paceFloor, so the session, and its recording, keep moving.
*/

const (
	// paceFloor is how long a frame waits for the browser before the agent
	// carries on without it.
	paceFloor = time.Second

	// paceMarkEvery is how many bytes of bitmap updates go between marks.
	paceMarkEvery = 8 << 10
	// The window of unacknowledged bitmap bytes: where it starts, and its
	// bounds. The least still holds a couple of full updates; the most is
	// what a fast link clears in well under a second.
	paceWindowStart = 256 << 10
	paceWindowMin   = 32 << 10
	paceWindowMax   = 4 << 20
	// paceWindowStep is what the window grows by for each prompt
	// acknowledgement.
	paceWindowStep = 8 << 10
	// paceSlack is how much longer than the best an acknowledgement may
	// take, beyond twice it, before the window is too large.
	paceSlack = 100 * time.Millisecond
	// paceShrinkGap is the least time between two cuts of the window: one
	// queue is one cut, however many late acknowledgements report it.
	paceShrinkGap = 200 * time.Millisecond

	// paceRing is how many marks are remembered. More may be outstanding
	// only if the browser has stopped answering, and then the floor rules.
	paceRing = 1024
)

// heldFrame is a graphics frame the target sent and the browser has not
// yet drawn.
type heldFrame struct {
	mark    uint32
	frameID uint32
}

// markNote is what was true when a mark was taken.
type markNote struct {
	id   uint32
	at   time.Time
	sent int64
}

// pacer holds the target to the browser's pace. Frames and bytes are noted
// on the goroutine reading the target; acknowledgements arrive on the one
// reading the browser.
type pacer struct {
	mu sync.Mutex
	// ack writes a graphics frame acknowledgement to the target.
	ack func(frameID, decoded, depth uint32) error
	// floor is paceFloor, shorter in tests.
	floor time.Duration
	now   func() time.Time

	mark    uint32 // the last mark made
	taken   uint32 // the last mark handed out
	browser bool   // the browser acknowledges marks
	notes   [paceRing]markNote
	rtt     time.Duration
	best    time.Duration // the quickest acknowledgement seen

	// Graphics frames.
	held    []heldFrame
	decoded uint32
	err     error

	// Bitmap bytes.
	sent, acked, marked int64
	window              int64
	shrunk              time.Time

	wake   chan struct{}
	done   chan struct{}
	closed bool
}

func newPacer(ack func(frameID, decoded, depth uint32) error) *pacer {
	return &pacer{ack: ack, floor: paceFloor, now: time.Now, window: paceWindowStart,
		wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// frame notes a graphics frame the target finished sending. Until a browser
// acknowledges marks it is acknowledged to the target at once.
func (p *pacer) frame(frameID uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.mark++
	if !p.browser {
		p.decoded++
		return p.ack(frameID, p.decoded, 0)
	}
	mark := p.mark
	p.held = append(p.held, heldFrame{mark: mark, frameID: frameID})
	time.AfterFunc(p.floor, func() { p.release(mark) })
	return nil
}

// release acknowledges to the target every held frame up to mark.
func (p *pacer) release(mark uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseLocked(mark)
}

func (p *pacer) releaseLocked(mark uint32) {
	n := 0
	for n < len(p.held) && p.held[n].mark <= mark {
		n++
	}
	if n == 0 || p.closed {
		return
	}
	for i, f := range p.held[:n] {
		p.decoded++
		// The depth is the frames here and not yet drawn, which Windows
		// reads as how far behind this client is.
		if err := p.ack(f.frameID, p.decoded, uint32(len(p.held)-i-1)); err != nil && p.err == nil {
			p.err = err
		}
	}
	p.held = append(p.held[:0], p.held[n:]...)
}

// bitmaps notes n bytes of bitmap updates read from the target.
func (p *pacer) bitmaps(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent += int64(n)
	if p.sent-p.marked >= paceMarkEvery {
		p.mark++
		p.marked = p.sent
	}
}

// wait holds the reader while the browser is more than a window of bitmap
// bytes behind: until it acknowledges, or the floor passes.
func (p *pacer) wait() {
	var timeout <-chan time.Time
	for {
		p.mu.Lock()
		room := !p.browser || p.sent-p.acked <= p.window || p.closed
		p.mu.Unlock()
		if room {
			return
		}
		if timeout == nil {
			t := time.NewTimer(p.floor)
			defer t.Stop()
			timeout = t.C
		}
		select {
		case <-p.wake:
		case <-p.done:
			return
		case <-timeout:
			return
		}
	}
}

// TakeMark returns the mark to send after what was just read, if one was
// made since it was last asked, and the last round trip to the browser.
func (p *pacer) TakeMark() (uint32, time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mark == p.taken {
		return 0, 0, false
	}
	p.taken = p.mark
	p.notes[p.mark%paceRing] = markNote{id: p.mark, at: p.now(), sent: p.sent}
	return p.mark, p.rtt, true
}

// Ack is the browser saying it has drawn everything up to mark id.
func (p *pacer) Ack(id uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id == 0 || id > p.taken {
		return
	}
	p.browser = true
	if n := p.notes[id%paceRing]; n.id == id {
		p.acked = max(p.acked, n.sent)
		p.tune(p.now(), p.now().Sub(n.at))
	} else {
		// Too long ago to say how much it covered: all of it.
		p.acked = p.sent
	}
	p.releaseLocked(id)
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// tune moves the window by how long an acknowledgement took: larger while
// they are prompt, smaller once they queue.
func (p *pacer) tune(now time.Time, took time.Duration) {
	p.rtt = took
	if p.best == 0 || took < p.best {
		p.best = took
	}
	if took <= 2*p.best+paceSlack {
		p.window = min(p.window+paceWindowStep, paceWindowMax)
		return
	}
	if now.Sub(p.shrunk) >= paceShrinkGap {
		p.window = max(p.window*3/4, paceWindowMin)
		p.shrunk = now
	}
}

// close lets a waiting reader go and stops acknowledging.
func (p *pacer) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.done)
	}
}

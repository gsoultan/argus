package rdp

import (
	"testing"
	"time"
)

type sentAck struct{ frame, decoded, depth uint32 }

func testPacer(floor time.Duration) (*pacer, *[]sentAck) {
	var acks []sentAck
	p := newPacer(func(frame, decoded, depth uint32) error {
		acks = append(acks, sentAck{frame, decoded, depth})
		return nil
	})
	p.floor = floor
	return p, &acks
}

// takeAndAck plays the browser: the mark goes out and comes back drawn.
func takeAndAck(t *testing.T, p *pacer) {
	t.Helper()
	id, _, ok := p.TakeMark()
	if !ok {
		t.Fatal("no mark to take")
	}
	p.Ack(id)
}

// A browser that acknowledges nothing is not paced: the target hears at
// once, as it always did.
func TestPacerWithoutABrowserAcknowledgesAtOnce(t *testing.T) {
	p, acks := testPacer(time.Hour)
	defer p.close()
	for id := uint32(10); id < 13; id++ {
		if err := p.frame(id); err != nil {
			t.Fatal(err)
		}
	}
	want := []sentAck{{10, 1, 0}, {11, 2, 0}, {12, 3, 0}}
	if len(*acks) != 3 || (*acks)[0] != want[0] || (*acks)[2] != want[2] {
		t.Fatalf("acknowledged %v, want %v", *acks, want)
	}
}

// The point of it: once a browser acknowledges marks, the target is told a
// frame is done with when the browser has drawn it and not before.
func TestPacerHoldsAFrameUntilTheBrowserHasDrawnIt(t *testing.T) {
	p, acks := testPacer(time.Hour)
	defer p.close()
	_ = p.frame(1)
	takeAndAck(t, p)
	*acks = nil

	_ = p.frame(2)
	_ = p.frame(3)
	mark2, _, _ := p.TakeMark() // covers both: the latest mark
	_ = p.frame(4)
	if len(*acks) != 0 {
		t.Fatalf("the target was told %v before the browser drew anything", *acks)
	}
	p.Ack(mark2)
	// Frames 2 and 3 are drawn; 4 is here and waiting, which is the depth.
	want := []sentAck{{2, 2, 2}, {3, 3, 1}}
	if len(*acks) != 2 || (*acks)[0] != want[0] || (*acks)[1] != want[1] {
		t.Fatalf("acknowledged %v, want %v", *acks, want)
	}
	takeAndAck(t, p)
	if got := (*acks)[len(*acks)-1]; got != (sentAck{4, 4, 0}) {
		t.Fatalf("the last frame was acknowledged as %v", got)
	}
}

// A browser that stops answering does not stop the session: each frame is
// given up on after the floor.
func TestPacerGivesUpOnASilentBrowserAfterTheFloor(t *testing.T) {
	var got = make(chan sentAck, 4)
	p := newPacer(func(frame, decoded, depth uint32) error {
		got <- sentAck{frame, decoded, depth}
		return nil
	})
	p.floor = 20 * time.Millisecond
	defer p.close()
	_ = p.frame(1)
	<-got
	takeAndAck(t, p)
	_ = p.frame(2)
	select {
	case a := <-got:
		if a.frame != 2 {
			t.Fatalf("acknowledged frame %d, want 2", a.frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a frame the browser never drew was never acknowledged to the target")
	}
}

// Bitmap updates: nothing more is read from the target while the browser is
// more than a window behind, and reading resumes when it catches up.
func TestPacerStopsReadingBitmapsPastTheWindow(t *testing.T) {
	p, _ := testPacer(time.Hour)
	defer p.close()

	// No browser yet: never held, however much is unacknowledged.
	p.bitmaps(paceWindowMax * 2)
	done := make(chan struct{})
	go func() { p.wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reading waited for a browser that acknowledges nothing")
	}
	takeAndAck(t, p)

	p.bitmaps(int(p.window) + paceMarkEvery)
	id, _, ok := p.TakeMark()
	if !ok {
		t.Fatal("bitmap bytes made no mark")
	}
	done = make(chan struct{})
	go func() { p.wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("reading went on with more than a window unacknowledged")
	case <-time.After(50 * time.Millisecond):
	}
	p.Ack(id)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reading did not resume when the browser caught up")
	}
}

// A reader held at the window is let go when the session closes.
func TestPacerCloseReleasesAWaitingReader(t *testing.T) {
	p, _ := testPacer(time.Hour)
	_ = p.frame(1)
	takeAndAck(t, p)
	p.bitmaps(int(p.window) * 2)
	done := make(chan struct{})
	go func() { p.wait(); close(done) }()
	p.close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close left the reader waiting")
	}
}

// The window follows how long acknowledgements take: it grows while they
// are prompt and is cut, once for each queue, when they are late.
func TestPacerWindowFollowsTheAcknowledgements(t *testing.T) {
	p, _ := testPacer(time.Hour)
	defer p.close()
	now := time.Unix(1000, 0)
	p.tune(now, 20*time.Millisecond)
	if p.window != paceWindowStart+paceWindowStep {
		t.Fatalf("a prompt acknowledgement left the window at %d", p.window)
	}
	before := p.window
	p.tune(now, time.Second)
	if p.window != before*3/4 {
		t.Fatalf("a late acknowledgement left the window at %d, want %d", p.window, before*3/4)
	}
	cut := p.window
	p.tune(now.Add(10*time.Millisecond), time.Second)
	if p.window != cut {
		t.Fatal("one queue cut the window twice")
	}
	for i := range 100 {
		p.tune(now.Add(time.Duration(i+1)*time.Second), 10*time.Second)
	}
	if p.window != paceWindowMin {
		t.Fatalf("the window fell to %d, past its least of %d", p.window, paceWindowMin)
	}
}

// A graphics frame larger than the window is still read to its end: its
// bytes are marked as they come, so the browser's acknowledgements keep
// making room.
func TestPacerReadsAFrameLargerThanTheWindow(t *testing.T) {
	p, _ := testPacer(time.Hour)
	defer p.close()
	_ = p.frame(1)
	takeAndAck(t, p)
	p.window = paceWindowMin

	read := 0
	for read < 4*paceWindowMin {
		done := make(chan struct{})
		go func() { p.wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("reading stopped %d bytes into one frame, with every mark acknowledged", read)
		}
		p.graphics(1500)
		read += 1500
		if id, _, ok := p.TakeMark(); ok {
			p.Ack(id)
		}
	}
}

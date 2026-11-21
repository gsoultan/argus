package agent

import (
	"io"
	"testing"
)

// runLink plays frames through the helper as behind a link of rate bits a
// second, and then lets it go on until the browser shows the last frame.
// It returns the largest frame the helper sent, and how many frames it
// took after the last capture to show it.
func runLink(t *testing.T, frames [][]byte, w, h, rate int, scroll bool) (largest, after int) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	pace := testPace
	pace.linkRate = rate
	pace.scroll = scroll
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveScreen(&scrollScreen{w: w, h: h, frames: frames}, func() screenInputer { return noScreenInput{} },
			inR, outW, pace, nil)
		_ = outW.Close()
	}()
	defer func() {
		_ = inW.Close()
		_ = outR.Close()
		<-done
	}()
	browser := &browserScreen{w: w, h: h, fb: make([]byte, w*h*4)}
	final := frames[len(frames)-1]
	for k := 0; ; k++ {
		sent := 0
		for {
			typ, p, err := readScreenMsg(outR)
			if err != nil {
				t.Fatal(err)
			}
			if typ == scrRects {
				browser.draw(t, p)
				sent += len(p)
			}
			if typ == scrFrame {
				break
			}
		}
		largest = max(largest, sent)
		if k >= len(frames)-1 && browser.same(final) {
			return largest, k - (len(frames) - 1)
		}
		if k > len(frames)+500 {
			t.Fatalf("the browser never caught up with the screen")
		}
		if err := writeScreenMsg(inW, scrNext, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// Behind a slow link a screen that changes everywhere goes in frames the
// link carries in a moment, and the browser catches up exactly once it
// stops changing.
func TestFramesFitTheLink(t *testing.T) {
	const w, h = 1024, 768
	var frames [][]byte
	for i := range 6 {
		frames = append(frames, scrollPage(w, h, uint64(i+1)))
	}
	whole, _ := runLink(t, frames, w, h, 0, false)
	const rate = 1_000_000
	largest, after := runLink(t, frames, w, h, rate, false)
	limit := rate / 8 * int(linkFrameFor.Milliseconds()) / 1000
	t.Logf("largest frame: %d KB with no limit, %d KB behind 1 Mbit/s (%d KB a frame allowed); caught up %d frames after",
		whole/1024, largest/1024, limit/1024, after)
	// At least a few tiles go, however slow the link, and the first frame
	// guesses what a tile costs.
	least := linkFreshTiles * whole / ((w / screenTile) * (h / screenTile))
	if largest > 2*max(limit, least) {
		t.Errorf("a frame of %d bytes behind a link that carries %d in %v", largest, limit, linkFrameFor)
	}
	if after == 0 {
		t.Errorf("nothing was held back")
	}
}

// What changed last goes in the next frame, however much is owed.
func TestNewChangesGoFirst(t *testing.T) {
	const w, h = 1024, 768
	busy := scrollPage(w, h, 1)
	typed := append([]byte(nil), busy...)
	for y := 700; y < 716; y++ { // a character typed at the bottom
		for x := 900; x < 910; x++ {
			typed[(y*w+x)*4] = 0
		}
	}
	d := screenDiffer{}
	shot := func(fb []byte) screenShot { return screenShot{fb: fb, stride: w * 4, width: w, height: h} }
	d.fit(d.find(shot(make([]byte, w*h*4)), true), 0)
	if got := d.fit(d.find(shot(busy), false), 20); len(got) != 20 || d.owing == 0 {
		t.Fatalf("sent %d of a whole new screen, %d owed", len(got), d.owing)
	}
	got := d.fit(d.find(shot(typed), false), 20)
	typedTile := (700/screenTile)*d.cols + 900/screenTile
	found := false
	for _, j := range got {
		found = found || j.index == typedTile
	}
	if !found {
		t.Fatalf("the typed character waited behind %d owed tiles", d.owing)
	}
}

// Held-back tiles and moves together still leave the browser with the
// screen exactly: a move reads what the browser has, owed tiles and all.
func TestScrollingBehindASlowLink(t *testing.T) {
	const w, h = 1280, 720
	frames := scrollFrames(w, h, 40, 12)
	largest, after := runLink(t, frames, w, h, 1_000_000, true)
	t.Logf("largest frame %d KB, caught up %d frames after", largest/1024, after)
}

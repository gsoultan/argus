package agent

import (
	"bytes"
	"io"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

// scrollPage is a page taller than the screen, like text: lines of dark
// marks on white, with blank space between paragraphs.
func scrollPage(w, h int, seed uint64) []byte {
	rng := rand.New(rand.NewPCG(seed, 1))
	fb := make([]byte, w*h*4)
	for i := range fb {
		fb[i] = 0xF8
	}
	for line := 0; line*22+16 < h; line++ {
		if line%7 == 6 {
			continue // between paragraphs
		}
		for x := 8; x < w-8; x++ {
			if rng.IntN(5) == 0 {
				continue
			}
			for y := line*22 + 4; y < line*22+16; y++ {
				if rng.IntN(3) == 0 {
					o := (y*w + x) * 4
					fb[o], fb[o+1], fb[o+2] = byte(rng.IntN(60)), byte(rng.IntN(60)), byte(rng.IntN(90))
				}
			}
		}
	}
	return fb
}

// scrollScreen shows frames in turn, then nothing new.
type scrollScreen struct {
	w, h   int
	frames [][]byte
	next   int
}

func (s *scrollScreen) Capture(wait time.Duration) (screenShot, bool, error) {
	if s.next >= len(s.frames) {
		time.Sleep(wait)
		return screenShot{}, false, nil
	}
	f := s.frames[s.next]
	s.next++
	return screenShot{fb: f, stride: s.w * 4, width: s.w, height: s.h}, true, nil
}

func (s *scrollScreen) Close() {}

// scrollFrames is a page, with a sidebar that stays put, scrolled step rows
// at a time, with the pointer drawn where it is.
func scrollFrames(w, h, step, n int) [][]byte {
	const sidebar = 192
	page := scrollPage(w, h*3, 7)
	side := scrollPage(sidebar, h, 9)
	var out [][]byte
	for k := range n {
		fb := make([]byte, w*h*4)
		for y := range h {
			copy(fb[y*w*4:(y+1)*w*4], page[(y+k*step)*w*4:(y+k*step+1)*w*4])
			copy(fb[y*w*4:y*w*4+sidebar*4], side[y*sidebar*4:(y+1)*sidebar*4])
		}
		for y := 300; y < 320; y++ { // the pointer, still, over the page
			for x := 900; x < 912; x++ {
				o := (y*w + x) * 4
				fb[o], fb[o+1], fb[o+2] = 0, 0, 0
			}
		}
		out = append(out, fb)
	}
	return out
}

// browserScreen draws what the helper sends as the console does: moves of
// its own pixels, then tiles.
type browserScreen struct {
	w, h int
	fb   []byte
}

func (b *browserScreen) draw(t *testing.T, p []byte) (moves, tiles int) {
	for len(p) >= rdp.FrameHeaderSize {
		if m, ok := rdp.ParseCopy(p); ok {
			if !rdp.CopyFits(m, b.w, b.h) {
				t.Fatalf("a move off the screen: %+v", m)
			}
			// As a canvas drawn onto itself: the source as it was.
			src := make([]byte, 0, m.Width*m.Height*4)
			for y := m.SrcY; y < m.SrcY+m.Height; y++ {
				src = append(src, b.fb[(y*b.w+m.SrcX)*4:(y*b.w+m.SrcX+m.Width)*4]...)
			}
			for y := range m.Height {
				copy(b.fb[((m.Y+y)*b.w+m.X)*4:], src[y*m.Width*4:(y+1)*m.Width*4])
			}
			moves++
			p = p[rdp.FrameHeaderSize:]
			continue
		}
		n := int(p[10]) | int(p[11])<<8
		size := rdp.FrameHeaderSize + (n+3)&^3
		bm := rdp.Bitmap{X: int(p[2]) | int(p[3])<<8, Y: int(p[4]) | int(p[5])<<8,
			Width: int(p[6]) | int(p[7])<<8, Height: int(p[8]) | int(p[9])<<8,
			BPP: int(p[1] & 0x7F), Compressed: p[1]&0x80 != 0, Data: p[rdp.FrameHeaderSize : rdp.FrameHeaderSize+n]}
		r, err := bm.Decode()
		if err != nil {
			t.Fatal(err)
		}
		for y := range r.Height {
			for x := range r.Width {
				i, o := (y*r.Width+x)*4, ((r.Y+y)*b.w+r.X+x)*4
				b.fb[o], b.fb[o+1], b.fb[o+2], b.fb[o+3] = r.Pixels[i+2], r.Pixels[i+1], r.Pixels[i], 0
			}
		}
		tiles++
		p = p[size:]
	}
	return moves, tiles
}

// same says the browser shows the screen, ignoring the unused fourth byte.
func (b *browserScreen) same(fb []byte) bool {
	for i := 0; i < len(fb); i += 4 {
		if !bytes.Equal(b.fb[i:i+3], fb[i:i+3]) {
			return false
		}
	}
	return true
}

// runScroll plays frames through the helper's loop and returns what each
// frame after the first cost, checking the browser ends each frame showing
// exactly the screen.
func runScroll(t *testing.T, frames [][]byte, w, h int, scroll bool) (bytesPerFrame, moves int) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	pace := testPace
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
	total := 0
	for k := range frames {
		sent := 0
		for {
			typ, p, err := readScreenMsg(outR)
			if err != nil {
				t.Fatal(err)
			}
			if typ == scrRects {
				m, _ := browser.draw(t, p)
				moves += m
				sent += len(p)
			}
			if typ == scrFrame {
				break
			}
		}
		if !browser.same(frames[k]) {
			t.Fatalf("after frame %d the browser does not show the screen", k)
		}
		if k > 0 {
			total += sent
		}
		if err := writeScreenMsg(inW, scrNext, nil); err != nil {
			t.Fatal(err)
		}
	}
	return total / (len(frames) - 1), moves
}

// A scroll goes as moves and the rows that scrolled into view, and the
// browser ends every frame with the screen exactly: the sidebar that stayed,
// the pointer that did not move with the page, every pixel.
func TestScrollingGoesAsMoves(t *testing.T) {
	const w, h = 1280, 720
	for _, step := range []int{3, 40, -24} {
		var frames [][]byte
		if step > 0 {
			frames = scrollFrames(w, h, step, 12)
		} else {
			frames = scrollFrames(w, h, -step, 12)
			for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
				frames[i], frames[j] = frames[j], frames[i]
			}
		}
		tiles, _ := runScroll(t, frames, w, h, false)
		moved, moves := runScroll(t, frames, w, h, true)
		t.Logf("scrolling %d rows a frame: %d KB a frame as tiles, %d KB as moves (%d moves)",
			step, tiles/1024, moved/1024, moves)
		if moves == 0 {
			t.Fatalf("scrolling %d rows a frame sent no moves", step)
		}
		if moved*4 > tiles {
			t.Errorf("scrolling %d rows a frame: moves cost %d bytes a frame, tiles %d: not a quarter",
				step, moved, tiles)
		}
	}
}

// A browser that did not say it draws moves is sent none.
func TestNoMovesUnlessAsked(t *testing.T) {
	const w, h = 640, 480
	if _, moves := runScroll(t, scrollFrames(w, h, 16, 6), w, h, false); moves != 0 {
		t.Fatalf("%d moves sent unasked", moves)
	}
}

// A screen that changed in place, nothing moved, sends no moves.
func TestNoMovesForAChangeInPlace(t *testing.T) {
	const w, h = 640, 480
	frames := [][]byte{scrollPage(w, h, 1), scrollPage(w, h, 2), scrollPage(w, h, 3)}
	if _, moves := runScroll(t, frames, w, h, true); moves != 0 {
		t.Fatalf("%d moves for screens that did not scroll", moves)
	}
}

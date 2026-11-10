package agent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

// fakeVideo is an encoder that writes one NAL unit: an IDR slice when asked
// for a key frame, or for the first picture, and an ordinary slice
// otherwise.
type fakeVideo struct {
	mu      sync.Mutex
	pics    int
	fail    error
	noKey   bool
	closed  bool
	created int
}

func (v *fakeVideo) open(int, int) (screenVideo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fail != nil {
		return nil, v.fail
	}
	v.created++
	return v, nil
}

func (v *fakeVideo) Encode(_ screenShot, key bool) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pics++
	nal := byte(0x41)
	if (key || v.pics == 1) && !v.noKey {
		nal = 0x65
	}
	return []byte{0, 0, 0, 1, nal, 0xAA, 0xBB}, nil
}

func (v *fakeVideo) Close() {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
}

// helperOut reads what a helper sent, frame by frame.
type helperOut struct {
	t  *testing.T
	r  io.Reader
	in io.Writer
}

// helperFrame is one frame: its pictures, its exact tiles, and any codec
// message that came before it.
type helperFrame struct {
	videos []videoMsg
	tiles  []image.Rectangle
	codecs []string
}

func (h helperOut) next() helperFrame {
	h.t.Helper()
	var f helperFrame
	for {
		typ, p, err := readScreenMsg(h.r)
		if err != nil {
			h.t.Fatalf("the helper stopped: %v", err)
		}
		switch typ {
		case scrVideo:
			m, err := parseVideoMsg(p)
			if err != nil {
				h.t.Fatal(err)
			}
			m.h264 = append([]byte(nil), m.h264...)
			f.videos = append(f.videos, m)
		case scrRects:
			for len(p) >= rdp.FrameHeaderSize {
				x, y := int(binary.LittleEndian.Uint16(p[2:])), int(binary.LittleEndian.Uint16(p[4:]))
				w, hh := int(binary.LittleEndian.Uint16(p[6:])), int(binary.LittleEndian.Uint16(p[8:]))
				f.tiles = append(f.tiles, image.Rect(x, y, x+w, y+hh))
				n := int(binary.LittleEndian.Uint16(p[10:12]))
				p = p[rdp.FrameHeaderSize+(n+3)&^3:]
			}
		case scrCodec:
			f.codecs = append(f.codecs, string(p))
		case scrFrame:
			// The next frame is allowed at once.
			if err := writeScreenMsg(h.in, scrNext, nil); err != nil {
				h.t.Fatal(err)
			}
			return f
		}
	}
}

// startVideoHelper serves screen with video from v, and reads it.
func startVideoHelper(t *testing.T, screen *fakeScreen, v *fakeVideo) (helperOut, func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan struct{})
	go func() {
		_ = serveScreen(screen, func() screenInputer { return screen }, inR, outW, testPace, v.open)
		_ = outW.Close()
		close(served)
	}()
	stop := func() {
		_ = inW.Close()
		go func() { _, _ = io.Copy(io.Discard, outR) }()
		<-served
	}
	return helperOut{t: t, r: outR, in: inW}, stop
}

// fill paints a block of tiles, so that many tiles change at once.
func (s *fakeScreen) fill(x0, y0, x1, y1 int, v byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			o := (y*s.w + x) * 4
			s.fb[o], s.fb[o+1], s.fb[o+2] = v, v, v
		}
	}
}

func readSize(t *testing.T, h helperOut) {
	t.Helper()
	typ, _, err := readScreenMsg(h.r)
	if err != nil || typ != scrSize {
		t.Fatalf("the helper began with message %d, %v", typ, err)
	}
}

// Much of the screen changing goes as one picture showing only what
// changed; a key frame first, which shows all of it. A small change goes as
// exact tiles. Tiles shown as video come again exactly once they settle.
func TestScreenSendsMotionAsVideoThenSharpensIt(t *testing.T) {
	screen := newFakeScreen(512, 256, 0)
	v := &fakeVideo{}
	h, stop := startVideoHelper(t, screen, v)
	defer stop()
	readSize(t, h)

	first := h.next()
	if len(first.videos) != 0 || len(first.tiles) != 32 {
		t.Fatalf("the first frame went as %d pictures and %d tiles, want every tile exactly",
			len(first.videos), len(first.tiles))
	}

	// Sixteen tiles change: motion. The first picture is a key frame and
	// shows the whole screen.
	screen.fill(0, 0, 256, 256, 0x80)
	f := h.next()
	if len(f.codecs) != 1 || f.codecs[0] != "h264" {
		t.Errorf("the helper said %q of its codec, want h264", f.codecs)
	}
	if len(f.videos) != 1 || len(f.tiles) != 0 {
		t.Fatalf("motion went as %d pictures and %d tiles, want one picture", len(f.videos), len(f.tiles))
	}
	if p := f.videos[0]; !p.key || len(p.regions) != 1 || p.regions[0] != image.Rect(0, 0, 512, 256) {
		t.Fatalf("the first picture is key=%v showing %v, want a key frame of the whole screen",
			p.key, p.regions)
	}

	// More motion: an ordinary picture, showing only the tiles that changed.
	screen.fill(0, 0, 256, 128, 0x40)
	f = h.next()
	if len(f.videos) != 1 || f.videos[0].key {
		t.Fatalf("the second burst went as %+v", f)
	}
	if got := f.videos[0].regions; len(got) != 1 || got[0] != image.Rect(0, 0, 256, 128) {
		t.Errorf("the second picture shows %v, want the 4x2 tiles that changed", got)
	}

	// Once still, every tile comes again exactly, in frames of their own.
	exact := map[image.Rectangle]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for len(exact) < 32 && time.Now().Before(deadline) {
		f = h.next()
		if len(f.videos) != 0 {
			t.Fatalf("a still screen sent a picture: %+v", f.videos)
		}
		for _, r := range f.tiles {
			exact[r] = true
		}
	}
	if len(exact) != 32 {
		t.Fatalf("%d of 32 tiles came again exactly", len(exact))
	}

	// After the motion has passed, a keystroke's one tile goes exactly.
	time.Sleep(screenVideoLinger)
	screen.paint(300, 200, 0xFF)
	f = h.next()
	if len(f.videos) != 0 || len(f.tiles) != 1 {
		t.Errorf("a one-tile change went as %d pictures and %d tiles", len(f.videos), len(f.tiles))
	}
}

// A picture of changes too scattered to name one by one covers one
// rectangle around them all, so tiles that did not change are drawn from
// the video too, and those must come again exactly as well.
func TestScreenSharpensEveryTileAPictureCovered(t *testing.T) {
	screen := newFakeScreen(2048, 1024, 0)
	v := &fakeVideo{}
	h, stop := startVideoHelper(t, screen, v)
	defer stop()
	readSize(t, h)
	h.next() // every tile, exactly

	// A key frame first: it shows everything and is sharpened everywhere.
	screen.fill(0, 0, 2048, 1024, 0x80)
	if f := h.next(); len(f.videos) != 1 || !f.videos[0].key {
		t.Fatalf("the first motion went as %+v", f)
	}
	// Reads exact tiles until all have come, or for a few seconds: a
	// change in the last corner then ends the wait, since a still screen
	// with nothing left to sharpen sends nothing at all.
	drain := func() map[image.Rectangle]bool {
		exact := map[image.Rectangle]bool{}
		deadline := time.Now().Add(3 * time.Second)
		poke := time.AfterFunc(3*time.Second, func() { screen.paint(2047, 1023, 0x01) })
		defer poke.Stop()
		for time.Now().Before(deadline) {
			f := h.next()
			for _, r := range f.tiles {
				exact[r] = true
			}
			if len(exact) == 512 {
				break
			}
		}
		return exact
	}
	if got := drain(); len(got) != 512 {
		t.Fatalf("%d of 512 tiles came again after the key frame", len(got))
	}

	// A checkerboard of changes: 256 tiles, none beside another, past
	// screenMaxRegions, so the picture covers the whole screen.
	for ty := 0; ty < 16; ty++ {
		for tx := (ty % 2); tx < 32; tx += 2 {
			screen.fill(tx*screenTile, ty*screenTile, (tx+1)*screenTile, (ty+1)*screenTile, 0x20)
		}
	}
	f := h.next()
	if len(f.videos) != 1 || f.videos[0].key || len(f.videos[0].regions) != 1 {
		t.Fatalf("the checkerboard went as %+v, want one picture of one rectangle", f.videos)
	}
	if got := drain(); len(got) != 512 {
		t.Fatalf("%d of the 512 tiles the picture covered came again exactly", len(got))
	}
}

// A helper with no encoder says why, and sends motion exactly.
func TestScreenWithoutAnEncoderSendsExactTiles(t *testing.T) {
	screen := newFakeScreen(512, 256, 0)
	v := &fakeVideo{fail: errors.New("Media Foundation is not installed")}
	h, stop := startVideoHelper(t, screen, v)
	defer stop()
	readSize(t, h)
	h.next()

	screen.fill(0, 0, 256, 256, 0x80)
	f := h.next()
	if len(f.codecs) != 1 || !strings.HasPrefix(f.codecs[0], "exact: ") ||
		!strings.Contains(f.codecs[0], "Media Foundation") {
		t.Errorf("the helper said %q of its codec", f.codecs)
	}
	if len(f.videos) != 0 || len(f.tiles) != 16 {
		t.Errorf("motion without an encoder went as %d pictures and %d tiles, want 16 tiles",
			len(f.videos), len(f.tiles))
	}
}

// An encoder that gives no key frame when one is needed shows nothing a
// viewer could start on: those tiles go exactly instead.
func TestScreenNeverSendsAPictureNobodyCanDecode(t *testing.T) {
	screen := newFakeScreen(512, 256, 0)
	v := &fakeVideo{noKey: true}
	h, stop := startVideoHelper(t, screen, v)
	defer stop()
	readSize(t, h)
	h.next()

	screen.fill(0, 0, 256, 256, 0x80)
	f := h.next()
	if len(f.videos) != 0 || len(f.tiles) != 16 {
		t.Errorf("went as %d pictures and %d tiles, want 16 exact tiles", len(f.videos), len(f.tiles))
	}
}

// A refresh, which a viewer arriving late asks for, repaints exactly and
// makes the next picture a key frame.
func TestScreenRefreshMakesTheNextPictureAKeyFrame(t *testing.T) {
	screen := newFakeScreen(512, 256, 0)
	v := &fakeVideo{}
	h, stop := startVideoHelper(t, screen, v)
	defer stop()
	readSize(t, h)
	h.next()
	screen.fill(0, 0, 256, 256, 0x80)
	if f := h.next(); len(f.videos) != 1 || !f.videos[0].key {
		t.Fatalf("the first motion was %+v", f)
	}
	screen.fill(0, 0, 256, 256, 0x10)
	if f := h.next(); len(f.videos) != 1 || f.videos[0].key {
		t.Fatalf("the second motion was %+v", f)
	}

	if err := writeScreenMsg(h.in, scrRefresh, nil); err != nil {
		t.Fatal(err)
	}
	for f := h.next(); len(f.tiles) != 32; f = h.next() {
		if len(f.videos) != 0 {
			t.Fatalf("a refresh sent a picture before repainting: %+v", f)
		}
	}
	screen.fill(0, 0, 256, 256, 0x90)
	f := h.next()
	if len(f.videos) != 1 || !f.videos[0].key {
		t.Fatalf("the motion after a refresh was %+v, want a key frame", f)
	}
}

func TestTileRegionsJoinRunsAndRows(t *testing.T) {
	// A 4x3 grid of 64 px tiles on a 250x180 screen (the last column and row
	// are cut short).
	got := tileRegions(4, 250, 180, []int{0, 1, 4, 5, 7, 11})
	want := []image.Rectangle{
		image.Rect(0, 0, 128, 128),
		image.Rect(192, 64, 250, 180),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("region %d is %v, want %v", i, got[i], want[i])
		}
	}
	// A run does not wrap onto the next row.
	if got := tileRegions(4, 256, 128, []int{3, 4}); len(got) != 2 {
		t.Errorf("tiles 3 and 4 made %v", got)
	}
	// Too many to name: one around them all.
	var many []int
	for i := 0; i < 600; i += 2 {
		many = append(many, i)
	}
	if got := tileRegions(21, 1344, 1920, many); len(got) != 1 {
		t.Errorf("300 scattered tiles made %d regions, want one", len(got))
	}
}

func TestH264KeyFramesAreFoundInTheStream(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 1, 2}
	idr := []byte{0, 0, 1, 0x65, 3}
	slice := []byte{0, 0, 0, 1, 0x41, 4}
	if !h264HasIDR(append(append([]byte(nil), sps...), idr...)) {
		t.Error("an IDR after the parameter sets was missed")
	}
	if h264HasIDR(append(append([]byte(nil), sps...), slice...)) {
		t.Error("an ordinary slice was taken for a key frame")
	}
	if h264HasIDR(nil) || h264HasIDR([]byte{0, 0, 1}) {
		t.Error("nothing was taken for a key frame")
	}
}

// NV12 matches BT.601 at limited range: white, black, and the primaries'
// chroma.
func TestBGRXToNV12(t *testing.T) {
	w, h := 4, 2
	fb := make([]byte, w*h*4)
	set := func(x, y int, r, g, b byte) {
		o := (y*w + x) * 4
		fb[o], fb[o+1], fb[o+2] = b, g, r
	}
	for y := range 2 {
		set(0, y, 255, 255, 255)
		set(1, y, 255, 255, 255)
		set(2, y, 255, 0, 0)
		set(3, y, 255, 0, 0)
	}
	out := make([]byte, w*h*3/2)
	bgrxToNV12(out, fb, w*4, w, h)
	if out[0] != 235 || out[2] != 82 {
		t.Errorf("luma of white and red is %d and %d, want 235 and 82", out[0], out[2])
	}
	u, v := out[8], out[9]
	if u != 128 || v != 128 {
		t.Errorf("white's chroma is %d,%d, want 128,128", u, v)
	}
	if u, v := out[10], out[11]; u != 90 || v != 240 {
		t.Errorf("red's chroma is %d,%d, want 90,240", u, v)
	}
}

// The agent turns the helper's pictures into graphics pipeline commands
// for the browser, a key frame opening the surface first, and records the
// same commands so the replay plays them.
func TestScreenPicturesBecomeGraphicsCommandsAndAreRecorded(t *testing.T) {
	v := &fakeVideo{}
	host := &fakeScreenHost{session: 1, screens: map[uint32]*fakeScreen{1: newFakeScreen(512, 256, 0)},
		video: v.open}
	c, err := openScreen(host, true, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(host.modes) != 1 || !host.modes[0].video {
		t.Fatalf("the helper was started as %+v, want video allowed", host.modes)
	}

	var recorded bytes.Buffer
	rec, err := rdp.NewRecorder(&recorded, rdp.Header{Width: 64, Height: 64, Graphics: screenGraphicsChannel})
	if err != nil {
		t.Fatal(err)
	}
	c.SetTap(func(pdu []byte) {
		if err := rec.Write(rdp.ServerOutput, pdu); err != nil {
			t.Error(err)
		}
	})

	screen := host.screens[1]
	// Draw until a picture comes: the first frame is exact, then motion.
	var cmds [][]byte
	deadline := time.Now().Add(5 * time.Second)
	for i := 1; len(cmds) == 0 && time.Now().Before(deadline); i++ {
		screen.fill(0, 0, screen.w, screen.h, byte(i))
		if _, err := c.NextBitmaps(); err != nil {
			t.Fatal(err)
		}
		cmds = commandsIn(t, c.gfx.take(1<<30))
		if id, _, ok := c.TakeMark(); ok {
			c.Ack(id)
		}
	}
	if len(cmds) == 0 {
		t.Fatal("no picture came")
	}
	want := []uint16{rdp.GfxResetGraphics, rdp.GfxCreateSurface, rdp.GfxMapSurfaceToOutput,
		rdp.GfxStartFrame, rdp.GfxWireToSurface1, rdp.GfxEndFrame}
	if len(cmds) != len(want) {
		t.Fatalf("a key frame became %d commands, want %d", len(cmds), len(want))
	}
	for i, cmd := range cmds {
		if got := binary.LittleEndian.Uint16(cmd); got != want[i] {
			t.Errorf("command %d is 0x%04x, want 0x%04x", i, got, want[i])
		}
	}
	if _, err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	var replay bytes.Buffer
	info, err := rdp.DecodeRecording(&recorded, &replay)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Graphics {
		t.Error("the recording does not replay the pictures")
	}
}

// commandsIn splits display frames of whole graphics commands.
func commandsIn(t *testing.T, frames []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(frames) >= rdp.FrameHeaderSize {
		if frames[0] != rdp.FrameGraphics {
			t.Fatalf("display frame of kind %d", frames[0])
		}
		n := int(binary.LittleEndian.Uint32(frames[4:8]))
		out = append(out, frames[rdp.FrameHeaderSize:rdp.FrameHeaderSize+n])
		frames = frames[rdp.FrameHeaderSize+(n+3)&^3:]
	}
	return out
}

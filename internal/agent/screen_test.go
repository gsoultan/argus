package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

// fakeScreen is a screen whose pixels the test sets, and which keeps the
// input it was given.
type fakeScreen struct {
	mu     sync.Mutex
	w, h   int
	fb     []byte
	inputs []rdp.InputEvent
}

func newFakeScreen(w, h int, colour byte) *fakeScreen {
	s := &fakeScreen{w: w, h: h, fb: make([]byte, w*h*4)}
	for i := range s.fb {
		s.fb[i] = colour
	}
	return s
}

func (s *fakeScreen) Capture(time.Duration) (screenShot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return screenShot{fb: append([]byte(nil), s.fb...), stride: s.w * 4, width: s.w, height: s.h}, true, nil
}

func (s *fakeScreen) Input(e rdp.InputEvent) error {
	s.mu.Lock()
	s.inputs = append(s.inputs, e)
	s.mu.Unlock()
	return nil
}

func (s *fakeScreen) Close() {}

func (s *fakeScreen) paint(x, y int, v byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := (y*s.w + x) * 4
	s.fb[o], s.fb[o+1], s.fb[o+2] = v, v, v
}

func (s *fakeScreen) inputCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inputs)
}

// pipePair is a helper's two pipes, joined to serveScreen in a goroutine.
type pipePair struct {
	*io.PipeReader
	w      io.WriteCloser
	closed chan struct{}
}

func (p *pipePair) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipePair) Close() error {
	err := p.w.Close()
	// As the real pipe does: the helper's writes fail once the agent's end
	// is closed.
	_ = p.PipeReader.Close()
	<-p.closed
	return err
}

func startFakeHelper(s *fakeScreen, video func(int, int) (screenVideo, error)) *pipePair {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &pipePair{PipeReader: outR, w: inW, closed: make(chan struct{})}
	go func() {
		defer close(p.closed)
		_ = serveScreen(s, func() screenInputer { return s }, inR, outW, testPace, video)
		_ = outW.Close()
	}()
	return p
}

type fakeScreenHost struct {
	mu       sync.Mutex
	session  uint32
	account  string
	screens  map[uint32]*fakeScreen
	started  []uint32
	sas      int
	startErr error
	// video makes the helper's encoder, when a helper is let use one.
	video func(int, int) (screenVideo, error)
	modes []helperMode
}

func (h *fakeScreenHost) ConsoleSession() (uint32, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.session, true
}

func (h *fakeScreenHost) ConsoleAccount(uint32) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.account
}

// testPace is quick, so tests do not wait on the frame rate.
var testPace = screenPace{minGap: time.Millisecond, idleMax: 5 * time.Millisecond}

func (h *fakeScreenHost) StartHelper(session uint32, mode helperMode) (io.ReadWriteCloser, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.startErr != nil {
		return nil, h.startErr
	}
	h.started = append(h.started, session)
	h.modes = append(h.modes, mode)
	var video func(int, int) (screenVideo, error)
	if mode.video {
		video = h.video
	}
	return startFakeHelper(h.screens[session], video), nil
}

func (h *fakeScreenHost) SecureAttention() error {
	h.mu.Lock()
	h.sas++
	h.mu.Unlock()
	return nil
}

func (h *fakeScreenHost) set(session uint32, account string) {
	h.mu.Lock()
	h.session, h.account = session, account
	h.mu.Unlock()
}

// nextRects waits for the next batch of rectangles.
func nextRects(t *testing.T, c *screenClient) []rdp.Bitmap {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := c.NextBitmaps()
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 0 {
			return b
		}
	}
	t.Fatal("no rectangles arrived")
	return nil
}

func TestScreenSharesTheConsoleAndRecordsWhatItShows(t *testing.T) {
	host := &fakeScreenHost{session: 1, account: `HOST\alice`,
		screens: map[uint32]*fakeScreen{1: newFakeScreen(130, 70, 0x40)}}
	c, err := openScreen(host, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if w, h := c.Size(); w != 130 || h != 70 {
		t.Fatalf("size %dx%d", w, h)
	}
	if c.Account() != `HOST\alice` {
		t.Fatalf("account %q", c.Account())
	}

	var mu sync.Mutex
	var pdus [][]byte
	c.SetTap(func(p []byte) {
		mu.Lock()
		pdus = append(pdus, append([]byte(nil), p...))
		mu.Unlock()
	})

	// The first frame is every tile: 3 x 2 of them.
	first := nextRects(t, c)
	if len(first) != 6 {
		t.Fatalf("first frame had %d tiles, want 6", len(first))
	}
	// What was recorded reads back as the screen, through replay's decoder.
	rea := rdp.NewReassembler()
	mu.Lock()
	recorded := 0
	for _, p := range pdus {
		rects, err := rdp.DecodePDU(rea, p)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rects {
			if r.Pixels[0] != 0x40 {
				t.Fatalf("recorded pixel %#x", r.Pixels[0])
			}
		}
		recorded += len(rects)
	}
	mu.Unlock()
	if recorded != 6 {
		t.Fatalf("recorded %d tiles", recorded)
	}

	// One pixel changes: one tile is sent.
	host.screens[1].paint(100, 65, 0xEE)
	if got := nextRects(t, c); len(got) != 1 || got[0].X != 64 || got[0].Y != 64 {
		t.Fatalf("after one pixel changed: %d tiles, first at %d,%d", len(got), got[0].X, got[0].Y)
	}

	// A refresh sends everything again.
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	if got := nextRects(t, c); len(got) != 6 {
		t.Fatalf("refresh sent %d tiles", len(got))
	}
}

func TestScreenPlaysInputAndTurnsCtrlAltDelIntoTheSecureSequence(t *testing.T) {
	host := &fakeScreenHost{session: 1,
		screens: map[uint32]*fakeScreen{1: newFakeScreen(64, 64, 0)}}
	c, err := openScreen(host, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, e := range []rdp.InputEvent{
		{Kind: "mouse", X: 10, Y: 10},
		{Kind: "key", Scancode: 0x1D, Down: true},
		{Kind: "key", Scancode: 0x38, Down: true},
		{Kind: "key", Scancode: 0x53, Extended: true, Down: true},
	} {
		if err := c.Send(e); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for host.screens[1].inputCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := host.screens[1].inputCount(); n != 3 {
		t.Fatalf("%d events reached the console, want the move and two modifiers", n)
	}
	if host.sas != 1 {
		t.Fatalf("Ctrl+Alt+Del sent the secure sequence %d times", host.sas)
	}
}

func TestScreenFollowsTheConsoleAndRecordsWhoIsSignedIn(t *testing.T) {
	host := &fakeScreenHost{session: 1, screens: map[uint32]*fakeScreen{
		1: newFakeScreen(64, 64, 0x10), 2: newFakeScreen(64, 64, 0x20)}}
	c, err := openScreen(host, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.checkEvery = 10 * time.Millisecond
	var mu sync.Mutex
	var notes []map[string]any
	c.note = func(v any) error {
		mu.Lock()
		notes = append(notes, v.(map[string]any))
		mu.Unlock()
		return nil
	}
	nextRects(t, c)

	// Someone signs in, then signs out and Windows starts a new session for
	// the sign-in screen.
	host.set(1, `HOST\bob`)
	deadline := time.Now().Add(5 * time.Second)
	for v := byte(1); c.Account() != `HOST\bob` && time.Now().Before(deadline); v++ {
		// NextBitmaps returns with the screen changing; the account is
		// checked while it waits.
		time.Sleep(20 * time.Millisecond)
		host.screens[1].paint(1, 1, v)
		nextRects(t, c)
	}
	host.set(2, "")
	var got []rdp.Bitmap
	for time.Now().Before(deadline) {
		b, err := c.NextBitmaps()
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 0 && b[0].Data != nil {
			if dec, _ := b[0].Decode(); dec.Pixels[0] == 0x20 {
				got = b
				break
			}
		}
	}
	if got == nil {
		t.Fatal("the new session's screen never arrived")
	}
	if len(host.started) != 2 || host.started[1] != 2 {
		t.Fatalf("helpers started in sessions %v", host.started)
	}
	mu.Lock()
	defer mu.Unlock()
	var accounts []string
	for _, n := range notes {
		if n["type"] == "console-account" {
			accounts = append(accounts, n["account"].(string))
		}
	}
	if len(accounts) != 2 || accounts[0] != `HOST\bob` || accounts[1] != "" {
		t.Fatalf("recorded accounts %q, want bob then the sign-in screen", accounts)
	}
}

func TestScreenRefusesWhenTheHelperCannotStart(t *testing.T) {
	host := &fakeScreenHost{session: 1, startErr: errors.New("no")}
	if _, err := openScreen(host, false, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("opened without a helper")
	}
}

func TestScreenRejectsOversizedHelperMessages(t *testing.T) {
	var b bytes.Buffer
	b.Write([]byte{scrRects, 0, 0, 0, 0x7F})
	if _, _, err := readScreenMsg(&b); err == nil {
		t.Fatal("read a 2 GB message")
	}
}

// drain reads the client until a frame has ended, and returns its mark.
func drainFrame(t *testing.T, c *screenClient) uint32 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.NextBitmaps(); err != nil {
			t.Fatal(err)
		}
		if id, _, ok := c.TakeMark(); ok {
			return id
		}
	}
	t.Fatal("no frame ended")
	return 0
}

// A browser that acknowledges frames is sent at most screenWindow of them
// ahead of what it has drawn: a slow link gets the newest screen, not a
// queue of old ones.
func TestScreenSendsNoMoreFramesThanTheBrowserHasRoomFor(t *testing.T) {
	screen := newFakeScreen(130, 70, 0x40)
	host := &fakeScreenHost{session: 1, screens: map[uint32]*fakeScreen{1: screen}}
	c, err := openScreen(host, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.checkEvery = time.Hour // no floor during the test

	first := drainFrame(t, c)
	c.Ack(first) // this browser acknowledges

	// The screen keeps changing; nothing more is acknowledged.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for v := byte(0); ; v++ {
			select {
			case <-stop:
				return
			default:
			}
			screen.paint(int(v)%130, 10, v)
			time.Sleep(time.Millisecond)
		}
	}()
	var last uint32
	for range screenWindow {
		last = drainFrame(t, c)
	}
	if last-first != screenWindow {
		t.Fatalf("frames %d..%d sent; want %d after the acknowledged one", first, last, screenWindow)
	}
	// Room is full: no frame ends until one is acknowledged.
	got := make(chan uint32, 1)
	go func() {
		for {
			if _, err := c.NextBitmaps(); err != nil {
				return
			}
			if id, _, ok := c.TakeMark(); ok {
				got <- id
				return
			}
		}
	}()
	select {
	case id := <-got:
		t.Fatalf("frame %d was sent with %d unacknowledged", id, screenWindow)
	case <-time.After(150 * time.Millisecond):
	}
	c.Ack(last)
	select {
	case id := <-got:
		if id != last+1 {
			t.Errorf("after the acknowledgement frame %d came, want %d", id, last+1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame came after the browser made room")
	}
}

// slowInput is a console whose input blocks until released, so events
// queue up behind the first.
type slowInput struct {
	*fakeScreen
	gate chan struct{}
	once sync.Once
}

func (s *slowInput) Input(e rdp.InputEvent) error {
	s.once.Do(func() { <-s.gate })
	return s.fakeScreen.Input(e)
}

// Mouse moves queued behind each other are played as one, to the latest
// position; clicks and keys are never dropped and keep their order.
func TestScreenPlaysQueuedMouseMovesAsOne(t *testing.T) {
	screen := &slowInput{fakeScreen: newFakeScreen(64, 64, 0), gate: make(chan struct{})}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, outR); close(copied) }()
	served := make(chan struct{})
	go func() {
		_ = serveScreen(screen, func() screenInputer { return screen }, inR, outW, testPace, nil)
		_ = outW.Close()
		close(served)
	}()
	defer func() { _ = inW.Close(); <-served; <-copied }()

	send := func(e rdp.InputEvent) {
		p, _ := json.Marshal(e)
		if err := writeScreenMsg(inW, scrInput, p); err != nil {
			t.Fatal(err)
		}
	}
	send(rdp.InputEvent{Kind: "key", Scancode: 0x1E, Down: true}) // blocks the player
	for i := range 50 {
		send(rdp.InputEvent{Kind: "mouse", X: i, Y: i})
	}
	send(rdp.InputEvent{Kind: "button", Button: 0, Down: true, X: 49, Y: 49})
	time.Sleep(50 * time.Millisecond)
	close(screen.gate)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		screen.mu.Lock()
		last := screen.inputs
		screen.mu.Unlock()
		if len(last) > 0 && last[len(last)-1].Kind == "button" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	screen.mu.Lock()
	inputs := append([]rdp.InputEvent(nil), screen.inputs...)
	screen.mu.Unlock()
	if len(inputs) < 3 || inputs[0].Kind != "key" || inputs[len(inputs)-1].Kind != "button" {
		t.Fatalf("played %+v", inputs)
	}
	moves := inputs[1 : len(inputs)-1]
	if len(moves) > 5 {
		t.Errorf("50 queued moves were played as %d", len(moves))
	}
	if m := moves[len(moves)-1]; m.Kind != "mouse" || m.X != 49 || m.Y != 49 {
		t.Errorf("the last move played was %+v, want the pointer at 49,49", m)
	}
}

// With dirty rectangles (Desktop Duplication), only tiles they touch are
// looked at; without, every tile is.
func TestScreenDifferLooksOnlyWhereTheScreenSaysItChanged(t *testing.T) {
	w, h := 256, 128
	fb := make([]byte, w*h*4)
	var d screenDiffer
	if n := len(d.changed(screenShot{fb: fb, stride: w * 4, width: w, height: h}, false, 1<<20)); n == 0 {
		t.Fatal("the first frame sent nothing")
	}
	paint := func(x, y int) {
		o := (y*w + x) * 4
		fb[o], fb[o+1], fb[o+2] = 0xFF, 0xFF, 0xFF
	}
	tiles := func(b [][]byte) int {
		n := 0
		for _, f := range b {
			for p := f; len(p) >= rdp.FrameHeaderSize; n++ {
				l := int(p[10]) | int(p[11])<<8
				p = p[rdp.FrameHeaderSize+(l+3)&^3:]
			}
		}
		return n
	}
	paint(10, 10)   // tile 0
	paint(200, 100) // tile (3,1)
	got := d.changed(screenShot{fb: fb, stride: w * 4, width: w, height: h,
		dirty: []image.Rectangle{image.Rect(5, 5, 20, 20)}}, false, 1<<20)
	if n := tiles(got); n != 1 {
		t.Errorf("a dirty rectangle over one tile sent %d tiles", n)
	}
	got = d.changed(screenShot{fb: fb, stride: w * 4, width: w, height: h}, false, 1<<20)
	if n := tiles(got); n != 1 {
		t.Errorf("a full comparison found %d changed tiles, want the one outside the last rectangle", n)
	}
	if n := tiles(d.changed(screenShot{fb: fb, stride: w * 4, width: w, height: h}, false, 1<<20)); n != 0 {
		t.Errorf("an unchanged screen sent %d tiles", n)
	}
}

// A browser leaving a still screen ends the session: nothing more comes
// from the helper to say so, and the agent's shutdown waits on sessions.
func TestScreenEndsWhenClosedOnAStillScreen(t *testing.T) {
	host := &fakeScreenHost{session: 1, account: `HOST\alice`,
		screens: map[uint32]*fakeScreen{1: newFakeScreen(130, 70, 0x40)}}
	c, err := openScreen(host, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	nextRects(t, c)

	ended := make(chan error, 1)
	go func() {
		for {
			if _, err := c.NextBitmaps(); err != nil {
				ended <- err
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	_ = c.Close()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the session went on after it was closed")
	}
}

// noSASHost is a console with no secure attention sequence (Linux, macOS).
type noSASHost struct{ *fakeScreenHost }

func (noSASHost) SecureAttention() error { return errNoSecureAttention }

// Where there is no secure attention sequence, Ctrl+Alt+Del reaches the
// screen as the keys, as it would from a keyboard there.
func TestScreenSendsCtrlAltDelAsKeysWhereThereIsNoSecureSequence(t *testing.T) {
	host := noSASHost{&fakeScreenHost{session: 1,
		screens: map[uint32]*fakeScreen{1: newFakeScreen(64, 64, 0)}}}
	c, err := openScreen(host, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, e := range []rdp.InputEvent{
		{Kind: "key", Scancode: 0x1D, Down: true},
		{Kind: "key", Scancode: 0x38, Down: true},
		{Kind: "key", Scancode: 0x53, Extended: true, Down: true},
	} {
		if err := c.Send(e); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for host.screens[1].inputCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := host.screens[1].inputCount(); n != 3 {
		t.Fatalf("%d of the three keys reached the console", n)
	}
}

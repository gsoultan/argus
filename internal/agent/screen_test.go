package agent

import (
	"bytes"
	"errors"
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

func (s *fakeScreen) Capture() ([]byte, int, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.fb...), s.w * 4, s.w, s.h, nil
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
	io.Reader
	w      io.WriteCloser
	closed chan struct{}
}

func (p *pipePair) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipePair) Close() error {
	err := p.w.Close()
	<-p.closed
	return err
}

func startFakeHelper(s *fakeScreen) *pipePair {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &pipePair{Reader: outR, w: inW, closed: make(chan struct{})}
	go func() {
		defer close(p.closed)
		_ = serveScreen(s, inR, outW, 5*time.Millisecond)
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

func (h *fakeScreenHost) StartHelper(session uint32) (io.ReadWriteCloser, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.startErr != nil {
		return nil, h.startErr
	}
	h.started = append(h.started, session)
	return startFakeHelper(h.screens[session]), nil
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
	c, err := openScreen(host, slog.New(slog.DiscardHandler))
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
	c, err := openScreen(host, slog.New(slog.DiscardHandler))
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
	c, err := openScreen(host, slog.New(slog.DiscardHandler))
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
	if _, err := openScreen(host, slog.New(slog.DiscardHandler)); err == nil {
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

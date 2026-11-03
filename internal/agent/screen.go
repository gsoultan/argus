package agent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Screen sharing: a Windows desktop without Remote Desktop.

	browser ──wss──▶ argus-control ──"screen" stream──▶ agent ──pipes──▶ helper
	                                                                     (console session)

For a host where Remote Desktop is off, or is Windows Home and has none, the
agent shows the console itself: whatever is on the physical screen, the
sign-in screen included, and drives it with the keyboard and mouse as a
person sitting there would.

A service runs in session 0 and cannot see the console, so the agent starts
a helper -- itself, as "screen-helper" -- in the console session, as
LocalSystem, which is what lets it follow the screen onto the sign-in and
UAC desktops. The helper captures, finds the tiles that changed and encodes
them as RDP bitmaps; it talks to nothing but the two pipes the agent gave it.
The agent records those bitmaps as Fast-Path updates, the same PDUs a Remote
Desktop server sends, before passing them on: the recording, its chain, the
replay and the live view are the ones Remote Desktop already has.

Whoever is signed in at the console is whoever the session acts as, which is
the choice this mode makes. The agent therefore records which account that is
when the session opens, and again every time it changes, in the recording's
chain, so a reconstruction knows whose desktop was driven.

If the console's session changes underneath (a sign-out ends it and Windows
starts another for the sign-in screen) the agent starts a new helper in the
new one and carries on.
*/

// ScreenService is the name the control plane asks for. A grant for it
// cannot be spent on Remote Desktop, nor the reverse.
const ScreenService = "screen"

// screenProtocol is what the recording's header calls a captured session.
const screenProtocol = "screen"

// screenTile is the side of the squares the screen is compared in.
const screenTile = 64

// screenInterval paces capture: about twenty frames a second at most, and
// the encoder only sends what changed.
const screenInterval = 50 * time.Millisecond

// The helper's pipes carry messages: a type byte, a four-byte length and the
// payload.
const (
	// helper → agent
	scrSize  byte = 1 // width, height as uint16 LE
	scrRects byte = 2 // rdp.EncodeBitmap frames, back to back
	scrFail  byte = 3 // why the helper stopped, as text

	// agent → helper
	scrInput   byte = 10 // one rdp.InputEvent as JSON
	scrRefresh byte = 11 // send every tile next time
)

// maxScreenMsg bounds one message from the helper. A full 4K screen of
// incompressible tiles is about 25 MB; the helper splits what it sends well
// below that, and anything larger is not from the helper.
const maxScreenMsg = 4 << 20

func writeScreenMsg(w io.Writer, typ byte, p []byte) error {
	var h [5]byte
	h[0] = typ
	binary.LittleEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

func readScreenMsg(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(h[1:])
	if n > maxScreenMsg {
		return 0, nil, fmt.Errorf("screen helper sent a %d-byte message", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

/* ── The helper ──────────────────────────────────────────────────────────── */

// screenCapturer is the console's screen and input, as the helper sees it.
// Every call is made from the one OS thread the helper locks, which is what
// Windows needs to keep the thread on the input desktop.
type screenCapturer interface {
	// Capture returns the whole screen, top-down 32-bit BGRX, stride bytes
	// a row. The buffer may be reused by the next call.
	Capture() (fb []byte, stride, width, height int, err error)
	// Input plays one event to the console.
	Input(rdp.InputEvent) error
	Close()
}

// screenDiffer finds the tiles of a frame that differ from the last one.
type screenDiffer struct {
	prev          []byte
	width, height int
	stride        int
	scratch       []byte
}

// changed returns the changed tiles as display frames (rdp.EncodeBitmap),
// grouped into batches under limit bytes. force sends every tile.
func (d *screenDiffer) changed(fb []byte, stride, width, height int, force bool,
	limit int) [][]byte {

	if width != d.width || height != d.height || stride != d.stride || len(d.prev) != len(fb) {
		d.prev = make([]byte, len(fb))
		d.width, d.height, d.stride = width, height, stride
		force = true
	}
	var batches [][]byte
	batch := []byte(nil)
	for ty := 0; ty < height; ty += screenTile {
		th := min(screenTile, height-ty)
		for tx := 0; tx < width; tx += screenTile {
			tw := min(screenTile, width-tx)
			if !force && tileSame(d.prev, fb, stride, tx, ty, tw, th) {
				continue
			}
			b := rdp.CompressRect(d.scratch, fb, stride, tx, ty, tw, th)
			d.scratch = b.Data[:0]
			f := rdp.EncodeBitmap(b)
			if len(batch)+len(f) > limit && len(batch) > 0 {
				batches = append(batches, batch)
				batch = nil
			}
			batch = append(batch, f...)
			for r := ty; r < ty+th; r++ {
				o := r*stride + tx*4
				copy(d.prev[o:o+tw*4], fb[o:o+tw*4])
			}
		}
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

func tileSame(a, b []byte, stride, x, y, w, h int) bool {
	for r := y; r < y+h; r++ {
		o := r*stride + x*4
		if !bytes.Equal(a[o:o+w*4], b[o:o+w*4]) {
			return false
		}
	}
	return true
}

// serveScreen is the helper's loop: input from in is played to the console,
// and what changes on screen is written to out, until in closes.
func serveScreen(c screenCapturer, in io.Reader, out io.Writer, interval time.Duration) error {
	runtime.LockOSThread()
	defer c.Close()

	inputs := make(chan rdp.InputEvent, 256)
	refresh := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		for {
			typ, p, err := readScreenMsg(in)
			if err != nil {
				done <- err
				return
			}
			switch typ {
			case scrInput:
				var e rdp.InputEvent
				if json.Unmarshal(p, &e) == nil {
					inputs <- e
				}
			case scrRefresh:
				select {
				case refresh <- struct{}{}:
				default:
				}
			}
		}
	}()

	var diff screenDiffer
	tick := time.NewTicker(interval)
	defer tick.Stop()
	force := true
	lastW, lastH := 0, 0
	for {
		select {
		case err := <-done:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case e := <-inputs:
			// Played on this thread, which is on the input desktop.
			_ = c.Input(e)
			continue
		case <-refresh:
			force = true
			continue
		case <-tick.C:
		}
		fb, stride, w, h, err := c.Capture()
		if err != nil {
			// The input desktop switching (a sign-in, a UAC prompt) fails
			// one capture; the next one is on the new desktop.
			continue
		}
		if w != lastW || h != lastH {
			var p [4]byte
			binary.LittleEndian.PutUint16(p[0:], uint16(w))
			binary.LittleEndian.PutUint16(p[2:], uint16(h))
			if err := writeScreenMsg(out, scrSize, p[:]); err != nil {
				return err
			}
			lastW, lastH, force = w, h, true
		}
		for _, b := range diff.changed(fb, stride, w, h, force, desktopBatch) {
			if err := writeScreenMsg(out, scrRects, b); err != nil {
				return err
			}
		}
		force = false
	}
}

/* ── The agent's side ────────────────────────────────────────────────────── */

// screenHost is what the agent needs from Windows to share the console.
type screenHost interface {
	// ConsoleSession is the session attached to the physical console.
	ConsoleSession() (uint32, bool)
	// ConsoleAccount is who is signed in to that session, as DOMAIN\name;
	// "" for the sign-in screen.
	ConsoleAccount(session uint32) string
	// StartHelper runs the helper in the session, its stdin and stdout the
	// returned pipe. Closing the pipe ends the helper.
	StartHelper(session uint32) (io.ReadWriteCloser, error)
	// SecureAttention is Ctrl+Alt+Del, which no keystroke can send.
	SecureAttention() error
}

// screenMsg is one message from the helper, or how its pipe ended.
type screenMsg struct {
	typ byte
	p   []byte
	err error
	// gen is which helper sent it: one from a helper already replaced is
	// dropped.
	gen int
}

// screenClient is a shared console as a DesktopClient.
type screenClient struct {
	host screenHost
	log  *slog.Logger
	// note records an observation in the session's recording.
	note func(v any) error

	msgs chan screenMsg
	// done is closed with the client, so a helper's reader never blocks on
	// msgs after nobody reads it.
	done chan struct{}

	mu            sync.Mutex
	helper        io.ReadWriteCloser
	gen           int
	session       uint32
	account       string
	width, height int
	tap           func([]byte)
	closed        bool
	restarts      []time.Time

	// ctrl and alt held, so Ctrl+Alt+Del can become the secure attention
	// sequence it means.
	ctrl, alt bool

	checkEvery time.Duration
	// nextCheck is when follow runs next; NextBitmaps' goroutine only.
	nextCheck time.Time
}

// openScreen starts the helper in the console's session and waits for it
// to say how large the screen is.
func openScreen(host screenHost, log *slog.Logger) (*screenClient, error) {
	session, ok := host.ConsoleSession()
	if !ok {
		return nil, errors.New("this host has no console session to share; " +
			"it may be starting or shutting down")
	}
	c := &screenClient{host: host, log: log, msgs: make(chan screenMsg, 8),
		done: make(chan struct{}), checkEvery: time.Second}
	c.account = host.ConsoleAccount(session)
	if err := c.start(session); err != nil {
		return nil, fmt.Errorf("could not start sharing the screen: %w", err)
	}
	timeout := time.After(15 * time.Second)
	for {
		select {
		case m := <-c.msgs:
			if m.err != nil {
				_ = c.Close()
				return nil, fmt.Errorf("the screen helper stopped before showing the screen: %v", m.err)
			}
			if m.typ == scrFail {
				_ = c.Close()
				return nil, fmt.Errorf("could not capture the screen: %s", m.p)
			}
			if m.typ == scrSize && len(m.p) == 4 {
				c.width = int(binary.LittleEndian.Uint16(m.p[0:]))
				c.height = int(binary.LittleEndian.Uint16(m.p[2:]))
				// The first tiles follow; NextBitmaps reads them.
				return c, nil
			}
		case <-timeout:
			_ = c.Close()
			return nil, errors.New("the screen helper did not show the screen within 15 seconds")
		}
	}
}

// start runs a helper in session and reads it until its pipe ends.
func (c *screenClient) start(session uint32) error {
	h, err := c.host.StartHelper(session)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = h.Close()
		return errors.New("closed")
	}
	if c.helper != nil {
		_ = c.helper.Close()
	}
	c.gen++
	gen := c.gen
	c.helper, c.session = h, session
	c.mu.Unlock()
	go func() {
		for {
			typ, p, err := readScreenMsg(h)
			select {
			case c.msgs <- screenMsg{typ: typ, p: p, err: err, gen: gen}:
			case <-c.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return nil
}

// Account is who was signed in at the console when it was last checked.
func (c *screenClient) Account() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.account
}

func (c *screenClient) NextBitmaps() ([]rdp.Bitmap, error) {
	// Checked on a schedule of its own, not from each call: a busy screen
	// returns from here many times a second, and a timer started per call
	// would never fire.
	if c.nextCheck.IsZero() {
		c.nextCheck = time.Now().Add(c.checkEvery)
	}
	check := time.NewTimer(time.Until(c.nextCheck))
	defer check.Stop()
	for {
		select {
		case m := <-c.msgs:
			c.mu.Lock()
			current, closed := m.gen == c.gen, c.closed
			c.mu.Unlock()
			if closed {
				return nil, errors.New("closed")
			}
			if !current {
				continue
			}
			if m.err != nil {
				if err := c.restart("the screen helper stopped: " + m.err.Error()); err != nil {
					return nil, err
				}
				continue
			}
			switch m.typ {
			case scrFail:
				if err := c.restart("the screen helper failed: " + string(m.p)); err != nil {
					return nil, err
				}
			case scrSize:
				if len(m.p) == 4 {
					w := int(binary.LittleEndian.Uint16(m.p[0:]))
					h := int(binary.LittleEndian.Uint16(m.p[2:]))
					c.mu.Lock()
					c.width, c.height = w, h
					c.mu.Unlock()
					c.record(map[string]any{"type": "resize", "width": w, "height": h})
					return nil, nil
				}
			case scrRects:
				return c.rects(m.p)
			}
		case <-check.C:
			c.nextCheck = time.Now().Add(c.checkEvery)
			if err := c.follow(); err != nil {
				return nil, err
			}
			check.Reset(c.checkEvery)
		}
	}
}

// rects reads the helper's display frames back into bitmaps, recording them
// as the Fast-Path PDUs a Remote Desktop server would have sent.
func (c *screenClient) rects(p []byte) ([]rdp.Bitmap, error) {
	var out []rdp.Bitmap
	for len(p) >= rdp.FrameHeaderSize {
		if p[0] != rdp.FrameEncoded {
			return nil, errors.New("the screen helper sent something other than a bitmap")
		}
		n := int(binary.LittleEndian.Uint16(p[10:12]))
		size := rdp.FrameHeaderSize + (n+3)&^3
		if size > len(p) {
			return nil, errors.New("the screen helper sent a truncated bitmap")
		}
		b := rdp.Bitmap{
			X: int(binary.LittleEndian.Uint16(p[2:4])), Y: int(binary.LittleEndian.Uint16(p[4:6])),
			Width: int(binary.LittleEndian.Uint16(p[6:8])), Height: int(binary.LittleEndian.Uint16(p[8:10])),
			BPP: int(p[1] & 0x7F), Compressed: p[1]&0x80 != 0,
			Data: p[rdp.FrameHeaderSize : rdp.FrameHeaderSize+n],
		}
		if b.Width <= 0 || b.Height <= 0 || b.Width > screenTile || b.Height > screenTile {
			return nil, errors.New("the screen helper sent a bitmap of the wrong size")
		}
		out = append(out, b)
		p = p[size:]
	}
	c.mu.Lock()
	tap := c.tap
	c.mu.Unlock()
	if tap != nil {
		for _, pdu := range rdp.FastPathBitmaps(out) {
			tap(pdu)
		}
	}
	return out, nil
}

// follow notices the console changing hands: another session (a sign-out,
// a switch of user) gets a new helper, and another account is recorded.
func (c *screenClient) follow() error {
	session, ok := c.host.ConsoleSession()
	if !ok {
		return nil
	}
	c.mu.Lock()
	moved := session != c.session
	c.mu.Unlock()
	if moved {
		c.log.Info("tunnel: the console moved to another session; following it", "session", session)
		if err := c.start(session); err != nil {
			return fmt.Errorf("the console moved to another session and could not be followed: %w", err)
		}
	}
	account := c.host.ConsoleAccount(session)
	c.mu.Lock()
	changed := account != c.account
	c.account = account
	c.mu.Unlock()
	if changed {
		c.log.Info("tunnel: the console's account changed", "account", account)
		c.record(map[string]any{"type": "console-account", "account": account})
	}
	return nil
}

// restart replaces a helper that stopped, a few times a minute at most: one
// that keeps failing ends the session rather than spinning.
func (c *screenClient) restart(why string) error {
	now := time.Now()
	c.mu.Lock()
	recent := c.restarts[:0]
	for _, t := range c.restarts {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	c.restarts = append(recent, now)
	tooMany := len(c.restarts) > 3
	c.mu.Unlock()
	if tooMany {
		return errors.New(why)
	}
	c.log.Warn("tunnel: restarting the screen helper", "reason", why)
	session, ok := c.host.ConsoleSession()
	if !ok {
		return errors.New(why)
	}
	time.Sleep(200 * time.Millisecond)
	if err := c.start(session); err != nil {
		return fmt.Errorf("%s, and could not be restarted: %w", why, err)
	}
	return nil
}

func (c *screenClient) record(v any) {
	if c.note == nil {
		return
	}
	if err := c.note(v); err != nil {
		// What the console did is in the chain, or the session ends.
		c.log.Error("tunnel: recording a screen event failed; ending the session", "error", err)
		_ = c.Close()
	}
}

func (c *screenClient) send(typ byte, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.helper == nil {
		return errors.New("closed")
	}
	return writeScreenMsg(c.helper, typ, p)
}

func (c *screenClient) Send(e rdp.InputEvent) error {
	if e.Kind == "key" {
		switch {
		case e.Scancode == 0x1D: // control
			c.ctrl = e.Down
		case e.Scancode == 0x38: // alt
			c.alt = e.Down
		case e.Scancode == 0x53 && e.Extended && e.Down && c.ctrl && c.alt:
			// Delete with Ctrl and Alt held: the secure attention
			// sequence, which Windows takes from no keyboard but its own.
			if err := c.host.SecureAttention(); err != nil {
				c.log.Warn("tunnel: could not send Ctrl+Alt+Del", "error", err)
			}
			return nil
		}
	}
	p, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := c.send(scrInput, p); err != nil {
		// A helper being replaced drops the keystroke, not the session.
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return err
		}
	}
	return nil
}

func (c *screenClient) Refresh() error {
	err := c.send(scrRefresh, nil)
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return err
	}
	return nil
}

func (c *screenClient) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.width, c.height
}

func (c *screenClient) SetTap(fn func([]byte)) {
	c.mu.Lock()
	c.tap = fn
	c.mu.Unlock()
}

func (c *screenClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.done)
	if c.helper != nil {
		return c.helper.Close()
	}
	return nil
}

// connectScreen opens the console for a session through the tunnel.
func connectScreen(cfg DesktopConfig, _ string, _ tunnel.DesktopOpen,
	log *slog.Logger) (DesktopClient, DesktopInfo, error) {
	host := cfg.ScreenHost
	if host == nil {
		host = platformScreenHost()
	}
	if host == nil {
		return nil, DesktopInfo{}, errors.New("screen sharing works on Windows hosts only")
	}
	c, err := openScreen(host, log)
	if err != nil {
		return nil, DesktopInfo{}, err
	}
	return c, DesktopInfo{Screen: true}, nil
}

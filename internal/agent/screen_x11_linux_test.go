//go:build linux

package agent

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/gsoultan/argus/internal/rdp"
)

// startXvfb runs a virtual X server that admits only a cookie, and points
// the environment at it as the agent does a helper. It skips the test where
// there is no Xvfb: CI installs it and checks this test is not skipped.
func startXvfb(t *testing.T, w, h int) string {
	t.Helper()
	bin, err := exec.LookPath("Xvfb")
	if err != nil {
		t.Skip("Xvfb is not installed")
	}
	dir := t.TempDir()
	cookie := make([]byte, 16)
	_, _ = rand.Read(cookie)
	var display string
	var cmd *exec.Cmd
	for n := 91; n < 120; n++ {
		if _, err := os.Stat(fmt.Sprintf("/tmp/.X11-unix/X%d", n)); err == nil {
			continue
		}
		display = fmt.Sprintf(":%d", n)
		auth := filepath.Join(dir, "auth")
		if err := os.WriteFile(auth, xauthEntry(fmt.Sprint(n), cookie), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(bin, display, "-auth", auth, "-nolisten", "tcp", "-noreset",
			"-screen", "0", fmt.Sprintf("%dx%dx24", w, h))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// TERM, so Xvfb removes its socket and lock and the next test can use the display.
		t.Cleanup(func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() })
		hex, err := x11Cookie(auth, display)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("DISPLAY", display)
		t.Setenv(x11CookieEnv, hex)
		break
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := dialX(); err == nil {
			c.Close()
			return display
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Xvfb did not start")
	return ""
}

// xauthEntry is one Xauthority record for a local display.
func xauthEntry(number string, cookie []byte) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, 256) // FamilyLocal
	for _, f := range [][]byte{[]byte("argus-test"), []byte(number), []byte("MIT-MAGIC-COOKIE-1"), cookie} {
		b = binary.BigEndian.AppendUint16(b, uint16(len(f)))
		b = append(b, f...)
	}
	return b
}

// paint maps a window of one colour on the X screen, as an application
// drawing would.
func paint(t *testing.T, c *xgb.Conn, r image.Rectangle, pixel uint32) xproto.Window {
	t.Helper()
	wid, err := xproto.NewWindowId(c)
	if err != nil {
		t.Fatal(err)
	}
	root := xproto.Setup(c).DefaultScreen(c).Root
	if err := xproto.CreateWindowChecked(c, 0, wid, root, int16(r.Min.X), int16(r.Min.Y),
		uint16(r.Dx()), uint16(r.Dy()), 0, xproto.WindowClassInputOutput, 0,
		xproto.CwBackPixel|xproto.CwOverrideRedirect|xproto.CwEventMask,
		[]uint32{pixel, 1, xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease}).Check(); err != nil {
		t.Fatal(err)
	}
	if err := xproto.MapWindowChecked(c, wid).Check(); err != nil {
		t.Fatal(err)
	}
	return wid
}

func pixelAt(s screenShot, x, y int) [3]byte {
	o := y*s.stride + x*4
	return [3]byte{s.fb[o], s.fb[o+1], s.fb[o+2]}
}

// The X screen is read whole first, then only where DAMAGE says it
// changed, into shared memory, with the change where it happened.
func TestX11CapturesWhatChangedOnTheScreen(t *testing.T) {
	startXvfb(t, 640, 480)
	cap, err := newX11Capturer()
	if err != nil {
		t.Fatal(err)
	}
	defer cap.Close()
	if !cap.useShm || !cap.useDamage {
		t.Fatalf("capture is %s; Xvfb has MIT-SHM and DAMAGE", cap.kind())
	}
	shot, changed, err := cap.Capture(time.Second)
	if err != nil || !changed {
		t.Fatalf("first capture: changed %v, %v", changed, err)
	}
	if shot.width != 640 || shot.height != 480 || shot.dirty != nil {
		t.Fatalf("first capture is %dx%d with dirty %v; want the whole 640x480 screen", shot.width, shot.height, shot.dirty)
	}
	// A still screen: nothing, after waiting.
	if _, changed, err := cap.Capture(50 * time.Millisecond); err != nil || changed {
		t.Fatalf("a still screen came back changed (%v, %v)", changed, err)
	}

	app, err := dialX()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	paint(t, app, image.Rect(100, 120, 164, 184), 0xFF0000) // red
	var got screenShot
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, changed, err := cap.Capture(200 * time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if changed && pixelAt(s, 130, 150) == [3]byte{0, 0, 0xFF} {
			got = s
			break
		}
	}
	if got.fb == nil {
		t.Fatal("the red window never appeared in what was captured")
	}
	if got.dirty == nil {
		t.Fatal("the change came back as the whole screen, not where DAMAGE said")
	}
	covered := false
	for _, r := range got.dirty {
		if image.Pt(130, 150).In(r) {
			covered = true
		}
		if r.Dx()*r.Dy() > 640*480/2 {
			t.Errorf("a dirty rectangle %v is half the screen for a 64x64 window", r)
		}
	}
	if !covered {
		t.Fatalf("dirty %v does not cover the window", got.dirty)
	}
	if p := pixelAt(got, 20, 20); p == [3]byte{0, 0, 0xFF} {
		t.Fatal("the screen outside the window turned red")
	}
}

// Keys reach the focused window by position, characters the layout has no
// key for are typed all the same, and the pointer goes where it is sent.
func TestX11PlaysInput(t *testing.T) {
	startXvfb(t, 640, 480)
	app, err := dialX()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	win := paint(t, app, image.Rect(0, 0, 640, 480), 0x202020)
	if err := xproto.SetInputFocusChecked(app, xproto.InputFocusPointerRoot, win, 0).Check(); err != nil {
		t.Fatal(err)
	}
	in := newX11Inputer()
	if in.err != nil {
		t.Fatal(in.err)
	}
	defer in.Close()

	if err := in.Input(rdp.InputEvent{Kind: "mouse", X: 300, Y: 200}); err != nil {
		t.Fatal(err)
	}
	root := xproto.Setup(app).DefaultScreen(app).Root
	var p *xproto.QueryPointerReply
	for i := 0; i < 50; i++ {
		p, err = xproto.QueryPointer(app, root).Reply()
		if err == nil && p.RootX == 300 && p.RootY == 200 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p == nil || p.RootX != 300 || p.RootY != 200 {
		t.Fatalf("the pointer is at %d,%d, want 300,200", p.RootX, p.RootY)
	}

	key := func(e rdp.InputEvent) {
		t.Helper()
		if err := in.Input(e); err != nil {
			t.Fatal(err)
		}
	}
	sc, ext, _ := rdp.ScancodeFor("KeyA")
	key(rdp.InputEvent{Kind: "key", Scancode: sc, Extended: ext, Down: true})
	key(rdp.InputEvent{Kind: "key", Scancode: sc, Extended: ext, Down: false})
	key(rdp.InputEvent{Kind: "unicode", Unicode: 'é', Down: true})
	key(rdp.InputEvent{Kind: "unicode", Unicode: 'é', Down: false})
	// Held at the end: Close lets go of it.
	shift, _, _ := rdp.ScancodeFor("ShiftLeft")
	key(rdp.InputEvent{Kind: "key", Scancode: shift, Down: true})

	var presses []xproto.Keycode
	timeout := time.After(5 * time.Second)
	for len(presses) < 3 {
		ev := make(chan xgb.Event, 1)
		go func() { e, _ := app.WaitForEvent(); ev <- e }()
		select {
		case e := <-ev:
			if kp, ok := e.(xproto.KeyPressEvent); ok {
				presses = append(presses, kp.Detail)
			}
		case <-timeout:
			t.Fatalf("only %v were pressed", presses)
		}
	}
	if presses[0] != 38 { // KEY_A (30) + 8
		t.Errorf("KeyA arrived as keycode %d, want 38", presses[0])
	}
	if presses[1] < in.spare[len(in.spare)-1] || presses[1] > in.spare[0] {
		t.Errorf("é arrived as keycode %d, not one of the spare keycodes %v", presses[1], in.spare)
	}
	if presses[2] != 50 { // KEY_LEFTSHIFT (42) + 8
		t.Errorf("ShiftLeft arrived as keycode %d, want 50", presses[2])
	}
	in.Close()
	keys, err := xproto.QueryKeymap(app).Reply()
	if err != nil {
		t.Fatal(err)
	}
	if keys.Keys[50/8]&(1<<(50%8)) != 0 {
		t.Fatal("Shift is still held down after the session ended")
	}
}

// x11HelperHost runs the real helper, in this process, on the Xvfb screen.
type x11HelperHost struct{}

func (x11HelperHost) ConsoleSession() (uint32, bool) { return 1, true }
func (x11HelperHost) ConsoleAccount(uint32) string   { return "alice" }
func (x11HelperHost) SecureAttention() error         { return errNoSecureAttention }
func (x11HelperHost) StartHelper(_ uint32, mode helperMode) (io.ReadWriteCloser, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &pipePair{PipeReader: outR, w: inW, closed: make(chan struct{})}
	args := helperArgs(mode)[1:]
	go func() {
		defer close(p.closed)
		RunScreenHelper(args, inR, outW)
		_ = outW.Close()
	}()
	return p, nil
}

// The helper, as the agent runs it, shows the X screen as the bitmaps the
// recording and the browser take, says why motion is not video, and ends
// when the session does.
func TestX11HelperServesTheScreen(t *testing.T) {
	startXvfb(t, 640, 480)
	c, err := openScreen(x11HelperHost{}, true, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if w, h := c.Size(); w != 640 || h != 480 {
		t.Fatalf("the screen is %dx%d, want 640x480", w, h)
	}
	var recorded int
	c.SetTap(func([]byte) { recorded++ })
	tiles := 0
	deadline := time.Now().Add(5 * time.Second)
	for tiles < 10*8 && time.Now().Before(deadline) {
		tiles += len(nextRects(t, c))
	}
	if tiles < 10*8 {
		t.Fatalf("%d tiles of a 640x480 screen arrived, want all 80", tiles)
	}
	if recorded == 0 {
		t.Fatal("nothing was recorded")
	}
	if err := c.Send(rdp.InputEvent{Kind: "mouse", X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		codec := c.codec
		c.mu.Unlock()
		if strings.HasPrefix(codec, "exact: ") {
			break
		}
		if _, err := c.NextBitmaps(); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(c.codec, "exact: ") {
		t.Fatalf("the helper said motion goes as %q, want exact tiles and why", c.codec)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

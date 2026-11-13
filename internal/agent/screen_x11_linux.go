//go:build linux

package agent

import (
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
	"golang.org/x/sys/unix"

	"github.com/gsoultan/argus/internal/rdp"
)

/*
The X11 console: capture with MIT-SHM, change from DAMAGE, input with XTEST.

The X server says where the screen changed (DAMAGE, one event when it first
does, then the region fetched once a frame), and only those rectangles are
read, straight into shared memory: no copy through the socket, and nothing
read while the screen is still. Typing reads a few kilobytes a keystroke
where a whole 1080p screen is 8 MB. A server without DAMAGE is read whole
and compared tile by tile, as GDI is on Windows; one without MIT-SHM is read
through the socket.

The pointer is not in what X returns, so it is drawn in, from XFIXES's
cursor image, where QueryPointer says it is, the way the Windows helper
draws it: the recording shows where the person pointed.

X is reached as the console's own clients reach it: DISPLAY, and the
server's magic cookie, which the agent reads from the file the server was
started with and passes in ARGUS_X_COOKIE, so the helper can run as the
person at the console without reading a file of theirs or of root's.
*/

// x11CookieEnv carries the X server's MIT-MAGIC-COOKIE-1, hex, to the helper.
const x11CookieEnv = "ARGUS_X_COOKIE"

func init() {
	// xgb logs to stderr on its own; the helper's stderr is not read, and
	// what matters is returned as errors.
	xgb.Logger = log.New(io.Discard, "", 0)
}

// dialX connects to the display in DISPLAY with the cookie in the
// environment, or as the X client libraries would without one.
func dialX() (*xgb.Conn, error) {
	display := os.Getenv("DISPLAY")
	cookie := os.Getenv(x11CookieEnv)
	if cookie == "" {
		return xgb.NewConnDisplay(display)
	}
	path, err := x11SocketPath(display)
	if err != nil {
		return nil, err
	}
	// The abstract socket first: it is reached from any mount namespace,
	// the agent's private /tmp included. Then the file, which the agent's
	// unit binds into its /tmp.
	nc, err := net.Dial("unix", "@"+path)
	if err != nil {
		if nc, err = net.Dial("unix", path); err != nil {
			return nil, fmt.Errorf("connecting to the X server at %s: %w", display, err)
		}
	}
	c, err := xgb.NewConnNetWithCookieHex(nc, cookie)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	return c, nil
}

// x11SocketPath is the local socket of a display such as ":0" or ":1.0".
func x11SocketPath(display string) (string, error) {
	n, ok := x11DisplayNumber(display)
	if !ok {
		return "", fmt.Errorf("%q is not a local X display", display)
	}
	return "/tmp/.X11-unix/X" + strconv.Itoa(n), nil
}

// x11DisplayNumber reads the number of a local display: 0 from ":0" or
// ":0.0", and false for a remote one.
func x11DisplayNumber(display string) (int, bool) {
	rest, ok := strings.CutPrefix(display, ":")
	if !ok {
		rest, ok = strings.CutPrefix(display, "unix:")
	}
	if !ok {
		return 0, false
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

/* ── Capture ─────────────────────────────────────────────────────────────── */

// x11Capturer is the X screen, read as it changes.
type x11Capturer struct {
	conn          *xgb.Conn
	root          xproto.Window
	width, height int
	events        chan xgb.Event
	// lost is closed when the connection to X ends.
	lost chan struct{}

	// Shared memory: the screen (fb) and, after it, where the changed
	// rectangles are read before they are copied in (stage).
	useShm      bool
	seg         shm.Seg
	segAttached bool
	mem         []byte
	fb          []byte
	stageOff    int

	useDamage bool
	dmg       damage.Damage
	region    xfixes.Region
	// damaged is set by a DAMAGE event and cleared once the region is read.
	damaged bool
	// whole asks for the whole screen next: at the start, after a resize.
	whole bool

	// The pointer, drawn into fb.
	useCursor bool
	cursor    x11Cursor
	px, py    int
	sprite    pointerSave
	// cursorDirty is set by XFIXES when the pointer's shape changes.
	cursorDirty bool
}

// x11Cursor is the pointer's image, premultiplied ARGB.
type x11Cursor struct {
	w, h, hotX, hotY int
	argb             []uint32
}

// pointerSave is what the pointer was drawn over, to put back first.
type pointerSave struct {
	r      image.Rectangle
	pixels []byte
}

// newX11Capturer opens the display in the environment.
func newX11Capturer() (*x11Capturer, error) {
	c := &x11Capturer{}
	if err := c.open(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *x11Capturer) kind() string {
	switch {
	case c.useShm && c.useDamage:
		return "x11-shm-damage"
	case c.useShm:
		return "x11-shm"
	}
	return "x11"
}

func (c *x11Capturer) open() error {
	conn, err := dialX()
	if err != nil {
		return err
	}
	setup := xproto.Setup(conn)
	scr := setup.DefaultScreen(conn)
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		conn.Close()
		return errors.New("this X server sends pixels most significant byte first, which screen sharing does not read")
	}
	if !x11Has32bpp(setup, scr.RootDepth) {
		conn.Close()
		return fmt.Errorf("the X screen is %d bits deep, not 24 or 32", scr.RootDepth)
	}
	c.conn, c.root = conn, scr.Root
	c.width, c.height = int(scr.WidthInPixels), int(scr.HeightInPixels)
	// The extensions before any event is asked for: their constructors go
	// into xgb's shared maps, which the readers look events up in.
	hasShm := x11InitExt(conn, "MIT-SHM") == nil
	hasFixes := x11InitExt(conn, "XFIXES") == nil
	hasDamage := hasFixes && x11InitExt(conn, "DAMAGE") == nil
	c.events = make(chan xgb.Event, 64)
	c.lost = make(chan struct{})
	go c.readEvents(conn, c.events, c.lost)

	// A resize (xrandr, a monitor plugged in) is a ConfigureNotify on root.
	_ = xproto.ChangeWindowAttributesChecked(conn, c.root, xproto.CwEventMask,
		[]uint32{xproto.EventMaskStructureNotify}).Check()

	c.useShm = hasShm
	if c.useShm {
		if _, err := shm.QueryVersion(conn).Reply(); err != nil {
			c.useShm = false
		}
	}
	if hasFixes {
		if _, err := xfixes.QueryVersion(conn, 4, 0).Reply(); err == nil {
			c.useCursor = true
			_ = xfixes.SelectCursorInputChecked(conn, c.root,
				xfixes.CursorNotifyMaskDisplayCursor).Check()
			c.cursorDirty = true
			if hasDamage {
				if _, err := damage.QueryVersion(conn, 1, 1).Reply(); err == nil {
					c.useDamage = c.startDamage() == nil
				}
			}
		}
	}
	c.whole = true
	return c.alloc()
}

// x11Has32bpp says whether the server lays out pixels of depth as 32 bits,
// which is BGRX on a little-endian server: the layout the differ reads.
func x11Has32bpp(setup *xproto.SetupInfo, depth byte) bool {
	if depth != 24 && depth != 32 {
		return false
	}
	for _, f := range setup.PixmapFormats {
		if f.Depth == depth {
			return f.BitsPerPixel == 32
		}
	}
	return false
}

func (c *x11Capturer) startDamage() error {
	dmg, err := damage.NewDamageId(c.conn)
	if err != nil {
		return err
	}
	if err := damage.CreateChecked(c.conn, dmg, xproto.Drawable(c.root),
		damage.ReportLevelNonEmpty).Check(); err != nil {
		return err
	}
	region, err := xfixes.NewRegionId(c.conn)
	if err != nil {
		return err
	}
	if err := xfixes.CreateRegionChecked(c.conn, region, nil).Check(); err != nil {
		return err
	}
	c.dmg, c.region = dmg, region
	return nil
}

func (c *x11Capturer) readEvents(conn *xgb.Conn, out chan<- xgb.Event, lost chan struct{}) {
	defer close(lost)
	for {
		ev, err := conn.WaitForEvent()
		if ev == nil && err == nil {
			return
		}
		if ev != nil {
			out <- ev
		}
	}
}

// alloc sizes the shared memory to the screen: the screen itself and a
// quarter of it again for changed rectangles. Past a quarter of the screen
// changed, the whole screen is read straight into place instead.
func (c *x11Capturer) alloc() error {
	c.freeShm()
	size := c.width * c.height * 4
	c.stageOff = size
	if c.useShm {
		if err := c.attachShm(size + size/4); err == nil {
			c.fb = c.mem[:size:size]
			c.sprite = pointerSave{}
			return nil
		}
		c.useShm = false
	}
	c.fb = make([]byte, size)
	c.sprite = pointerSave{}
	return nil
}

func (c *x11Capturer) attachShm(size int) error {
	id, err := unix.SysvShmGet(unix.IPC_PRIVATE, size, unix.IPC_CREAT|0o600)
	if err != nil {
		return err
	}
	mem, err := unix.SysvShmAttach(id, 0, 0)
	if err != nil {
		_, _ = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
		return err
	}
	seg, err := shm.NewSegId(c.conn)
	if err == nil {
		err = shm.AttachChecked(c.conn, seg, uint32(id), false).Check()
	}
	// Marked for removal now: it lasts while the helper or the server has
	// it attached, and not a moment longer, however the helper ends.
	_, _ = unix.SysvShmCtl(id, unix.IPC_RMID, nil)
	if err != nil {
		_ = unix.SysvShmDetach(mem)
		return err
	}
	c.seg, c.segAttached, c.mem = seg, true, mem
	return nil
}

func (c *x11Capturer) freeShm() {
	if c.segAttached {
		_ = shm.DetachChecked(c.conn, c.seg).Check()
		c.segAttached = false
	}
	if c.mem != nil {
		_ = unix.SysvShmDetach(c.mem)
		c.mem = nil
	}
}

func (c *x11Capturer) Close() {
	if c.conn == nil {
		return
	}
	c.freeShm()
	c.conn.Close()
	c.conn = nil
}

// x11PointerPoll is how often the pointer is looked at while the screen is
// still: it moves without damaging anything.
const x11PointerPoll = 16 * time.Millisecond

// x11MaxRects is how many rectangles are read one by one before their
// bounding box is read instead.
const x11MaxRects = 48

func (c *x11Capturer) Capture(wait time.Duration) (screenShot, bool, error) {
	if c.conn == nil {
		if err := c.open(); err != nil {
			return screenShot{}, false, err
		}
	}
	if err := c.waitForChange(wait); err != nil {
		c.Close()
		return screenShot{}, false, err
	}
	moved := false
	if c.useCursor {
		var err error
		if moved, err = c.lookAtPointer(); err != nil {
			c.Close()
			return screenShot{}, false, err
		}
	}
	if !c.whole && !c.damaged && !moved && c.useDamage {
		return screenShot{}, false, nil
	}

	// Put back what the pointer covered, so what is read and compared is
	// the screen, and the pointer is drawn afresh after.
	var dirty []image.Rectangle
	if !c.sprite.r.Empty() {
		restoreRect(c.fb, c.width*4, c.sprite.pixels, c.sprite.r)
		dirty = append(dirty, c.sprite.r)
		c.sprite.r = image.Rectangle{}
	}
	switch {
	case c.whole || !c.useDamage:
		if c.useDamage {
			// Cleared first, so DAMAGE reports the next change: it says
			// only when the region stops being empty.
			if _, err := c.damage(); err != nil {
				c.Close()
				return screenShot{}, false, err
			}
		}
		if err := c.read(image.Rect(0, 0, c.width, c.height)); err != nil {
			c.Close()
			return screenShot{}, false, err
		}
		dirty = nil
		c.damaged = false
	case c.damaged:
		rects, err := c.damage()
		if err != nil {
			c.Close()
			return screenShot{}, false, err
		}
		if err := c.readRects(rects); err != nil {
			c.Close()
			return screenShot{}, false, err
		}
		dirty = append(dirty, rects...)
	}
	whole := c.whole
	c.whole = false
	if r := c.drawPointer(); !r.Empty() {
		dirty = append(dirty, r)
	}
	if whole || !c.useDamage {
		dirty = nil
	}
	return screenShot{fb: c.fb, stride: c.width * 4, width: c.width, height: c.height, dirty: dirty}, true, nil
}

// waitForChange waits up to wait for the screen to change or the pointer to
// move, and handles what X sent meanwhile.
func (c *x11Capturer) waitForChange(wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		if err := c.drain(); err != nil {
			return err
		}
		if c.damaged || c.whole || !c.useDamage {
			return nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil
		}
		step := left
		if c.useCursor {
			step = min(step, x11PointerPoll)
		}
		t := time.NewTimer(step)
		select {
		case ev := <-c.events:
			t.Stop()
			c.handle(ev)
		case <-c.lost:
			t.Stop()
			return errors.New("the X server closed the connection")
		case <-t.C:
			if c.useCursor {
				if r, err := xproto.QueryPointer(c.conn, c.root).Reply(); err != nil {
					return err
				} else if int(r.RootX) != c.px || int(r.RootY) != c.py {
					return nil
				}
			}
		}
	}
}

// drain handles the events already waiting.
func (c *x11Capturer) drain() error {
	for {
		select {
		case ev := <-c.events:
			c.handle(ev)
		case <-c.lost:
			return errors.New("the X server closed the connection")
		default:
			return nil
		}
	}
}

func (c *x11Capturer) handle(ev xgb.Event) {
	switch e := ev.(type) {
	case damage.NotifyEvent:
		c.damaged = true
	case xfixes.CursorNotifyEvent:
		c.cursorDirty = true
	case xproto.ConfigureNotifyEvent:
		if e.Window == c.root && (int(e.Width) != c.width || int(e.Height) != c.height) {
			c.width, c.height = int(e.Width), int(e.Height)
			_ = c.alloc()
			c.whole = true
		}
	}
}

// damage reads and clears the region that changed.
func (c *x11Capturer) damage() ([]image.Rectangle, error) {
	c.damaged = false
	if err := damage.SubtractChecked(c.conn, c.dmg, 0, c.region).Check(); err != nil {
		return nil, err
	}
	r, err := xfixes.FetchRegion(c.conn, c.region).Reply()
	if err != nil {
		return nil, err
	}
	bounds := image.Rect(0, 0, c.width, c.height)
	if len(r.Rectangles) > x11MaxRects {
		e := r.Extents
		return []image.Rectangle{xRect(e).Intersect(bounds)}, nil
	}
	out := make([]image.Rectangle, 0, len(r.Rectangles))
	for _, x := range r.Rectangles {
		if rr := xRect(x).Intersect(bounds); !rr.Empty() {
			out = append(out, rr)
		}
	}
	return out, nil
}

func xRect(r xproto.Rectangle) image.Rectangle {
	return image.Rect(int(r.X), int(r.Y), int(r.X)+int(r.Width), int(r.Y)+int(r.Height))
}

// readRects reads the changed rectangles into fb: each into its own place
// in the stage, all asked for before any answer is waited on, so many small
// changes cost one round trip to the server rather than one each. Past a
// quarter of the screen, the screen is read whole.
func (c *x11Capturer) readRects(rects []image.Rectangle) error {
	if !c.useShm {
		for _, r := range rects {
			if err := c.read(r); err != nil {
				return err
			}
		}
		return nil
	}
	area := 0
	for _, r := range rects {
		area += r.Dx() * r.Dy()
	}
	if area*4 > len(c.mem)-c.stageOff {
		return c.read(image.Rect(0, 0, c.width, c.height))
	}
	cookies := make([]shm.GetImageCookie, len(rects))
	offs := make([]int, len(rects))
	off := c.stageOff
	for i, r := range rects {
		offs[i] = off
		cookies[i] = shm.GetImage(c.conn, xproto.Drawable(c.root), int16(r.Min.X), int16(r.Min.Y),
			uint16(r.Dx()), uint16(r.Dy()), 0xFFFFFFFF, xproto.ImageFormatZPixmap, c.seg, uint32(off))
		off += r.Dx() * r.Dy() * 4
	}
	var first error
	for i, ck := range cookies {
		if _, err := ck.Reply(); err != nil {
			// A rectangle that went off the screen as it resized: the
			// resize reads everything again.
			if first == nil {
				first = err
			}
			continue
		}
		r := rects[i]
		restoreRect(c.fb, c.width*4, c.mem[offs[i]:offs[i]+r.Dx()*r.Dy()*4], r)
	}
	return first
}

// read reads one rectangle of the screen into fb.
func (c *x11Capturer) read(r image.Rectangle) error {
	if c.useShm && r == image.Rect(0, 0, c.width, c.height) {
		// Straight into place.
		_, err := shm.GetImage(c.conn, xproto.Drawable(c.root), 0, 0, uint16(c.width), uint16(c.height),
			0xFFFFFFFF, xproto.ImageFormatZPixmap, c.seg, 0).Reply()
		return err
	}
	if c.useShm && r.Dx()*r.Dy()*4 <= len(c.mem)-c.stageOff {
		if _, err := shm.GetImage(c.conn, xproto.Drawable(c.root), int16(r.Min.X), int16(r.Min.Y),
			uint16(r.Dx()), uint16(r.Dy()), 0xFFFFFFFF, xproto.ImageFormatZPixmap, c.seg,
			uint32(c.stageOff)).Reply(); err != nil {
			return err
		}
		restoreRect(c.fb, c.width*4, c.mem[c.stageOff:c.stageOff+r.Dx()*r.Dy()*4], r)
		return nil
	}
	// Through the socket, a band at a time so no reply is huge.
	band := max(1, (4<<20)/(r.Dx()*4))
	for y := r.Min.Y; y < r.Max.Y; y += band {
		h := min(band, r.Max.Y-y)
		reply, err := xproto.GetImage(c.conn, xproto.ImageFormatZPixmap, xproto.Drawable(c.root),
			int16(r.Min.X), int16(y), uint16(r.Dx()), uint16(h), 0xFFFFFFFF).Reply()
		if err != nil {
			return err
		}
		if len(reply.Data) < r.Dx()*h*4 {
			return errors.New("the X server sent less of the screen than was asked for")
		}
		restoreRect(c.fb, c.width*4, reply.Data, image.Rect(r.Min.X, y, r.Max.X, y+h))
	}
	return nil
}

// lookAtPointer reads where the pointer is, and its shape when that
// changed, and says whether either did.
func (c *x11Capturer) lookAtPointer() (bool, error) {
	changed := false
	if c.cursorDirty {
		img, err := xfixes.GetCursorImage(c.conn).Reply()
		if err != nil {
			return false, err
		}
		c.cursor = x11Cursor{w: int(img.Width), h: int(img.Height), hotX: int(img.Xhot),
			hotY: int(img.Yhot), argb: img.CursorImage}
		c.cursorDirty, changed = false, true
	}
	p, err := xproto.QueryPointer(c.conn, c.root).Reply()
	if err != nil {
		return false, err
	}
	if int(p.RootX) != c.px || int(p.RootY) != c.py {
		c.px, c.py = int(p.RootX), int(p.RootY)
		changed = true
	}
	return changed, nil
}

// drawPointer draws the pointer into fb, saving what it covers, and
// returns where it drew.
func (c *x11Capturer) drawPointer() image.Rectangle {
	cur := c.cursor
	if !c.useCursor || cur.w == 0 || cur.h == 0 || len(cur.argb) < cur.w*cur.h {
		return image.Rectangle{}
	}
	at := image.Rect(c.px-cur.hotX, c.py-cur.hotY, c.px-cur.hotX+cur.w, c.py-cur.hotY+cur.h)
	r := at.Intersect(image.Rect(0, 0, c.width, c.height))
	if r.Empty() {
		return r
	}
	stride := c.width * 4
	need := r.Dx() * r.Dy() * 4
	if cap(c.sprite.pixels) < need {
		c.sprite.pixels = make([]byte, need)
	}
	c.sprite.pixels = c.sprite.pixels[:need]
	saveRect(c.sprite.pixels, c.fb, stride, r)
	c.sprite.r = r
	drawARGB(c.fb, stride, r, at.Min, cur.w, cur.argb)
	return r
}

// drawARGB blends premultiplied ARGB pixels (an image w wide whose top
// left is at origin) over BGRX fb, within r.
func drawARGB(fb []byte, stride int, r image.Rectangle, origin image.Point, w int, argb []uint32) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		src := argb[(y-origin.Y)*w:]
		row := fb[y*stride:]
		for x := r.Min.X; x < r.Max.X; x++ {
			p := src[x-origin.X]
			a := p >> 24
			if a == 0 {
				continue
			}
			o := x * 4
			if a == 255 {
				row[o], row[o+1], row[o+2] = byte(p), byte(p>>8), byte(p>>16)
				continue
			}
			k := 255 - a
			row[o] = byte(p) + byte((uint32(row[o])*k+127)/255)
			row[o+1] = byte(p>>8) + byte((uint32(row[o+1])*k+127)/255)
			row[o+2] = byte(p>>16) + byte((uint32(row[o+2])*k+127)/255)
		}
	}
}

// saveRect copies r of a frame (stride bytes a row) into dst, packed.
func saveRect(dst, frame []byte, stride int, r image.Rectangle) {
	w := r.Dx() * 4
	for y := r.Min.Y; y < r.Max.Y; y++ {
		o := y*stride + r.Min.X*4
		copy(dst[(y-r.Min.Y)*w:], frame[o:o+w])
	}
}

// restoreRect copies packed src into r of a frame.
func restoreRect(frame []byte, stride int, src []byte, r image.Rectangle) {
	w := r.Dx() * 4
	for y := r.Min.Y; y < r.Max.Y; y++ {
		o := y*stride + r.Min.X*4
		copy(frame[o:o+w], src[(y-r.Min.Y)*w:])
	}
}

/* ── Input ───────────────────────────────────────────────────────────────── */

// x11Inputer plays input with XTEST, on a connection of its own.
type x11Inputer struct {
	conn *xgb.Conn
	root xproto.Window
	// minKey and maxKey bound the server's keycodes; perKey is how many
	// keysyms each has.
	minKey, maxKey xproto.Keycode
	// spare are keycodes with no keysym, borrowed to type a character the
	// layout has no key for; next is the one to borrow next.
	spare []xproto.Keycode
	next  int
	// typing maps a character held down to the keycode it was given.
	typing map[uint16]xproto.Keycode
	// keys and buttons held down, to let go of at the end.
	keys    map[xproto.Keycode]bool
	buttons map[byte]bool
	err     error
}

func newX11Inputer() *x11Inputer {
	p := &x11Inputer{typing: map[uint16]xproto.Keycode{}, keys: map[xproto.Keycode]bool{},
		buttons: map[byte]bool{}}
	conn, err := dialX()
	if err != nil {
		p.err = err
		return p
	}
	if err := x11InitExt(conn, "XTEST"); err != nil {
		conn.Close()
		p.err = fmt.Errorf("the X server has no XTEST, so input cannot be played: %w", err)
		return p
	}
	setup := xproto.Setup(conn)
	p.conn, p.root = conn, setup.DefaultScreen(conn).Root
	p.minKey, p.maxKey = setup.MinKeycode, setup.MaxKeycode
	p.findSpare()
	return p
}

// findSpare lists the keycodes the layout leaves empty, from the top.
func (p *x11Inputer) findSpare() {
	n := int(p.maxKey) - int(p.minKey) + 1
	m, err := xproto.GetKeyboardMapping(p.conn, p.minKey, byte(n)).Reply()
	if err != nil || m.KeysymsPerKeycode == 0 {
		return
	}
	per := int(m.KeysymsPerKeycode)
	for i := n - 1; i >= 0 && len(p.spare) < 4; i-- {
		empty := true
		for j := range per {
			if i*per+j < len(m.Keysyms) && m.Keysyms[i*per+j] != 0 {
				empty = false
				break
			}
		}
		if empty {
			p.spare = append(p.spare, xproto.Keycode(int(p.minKey)+i))
		}
	}
}

func (p *x11Inputer) Input(e rdp.InputEvent) error {
	if p.conn == nil {
		return p.err
	}
	switch e.Kind {
	case "key":
		k, ok := evdevKey(e.Scancode, e.Extended)
		if !ok {
			return nil
		}
		return p.key(xproto.Keycode(k+8), e.Down)
	case "unicode":
		return p.char(e.Unicode, e.Down)
	case "sync":
		return p.locks(e.Locks)
	case "mouse":
		return p.move(e.X, e.Y)
	case "button":
		if err := p.move(e.X, e.Y); err != nil {
			return err
		}
		if e.Button < 0 || e.Button > 2 {
			return nil
		}
		return p.button(byte(e.Button+1), e.Down)
	case "wheel":
		if err := p.move(e.X, e.Y); err != nil {
			return err
		}
		// One notch, in its direction: X's buttons 4 (up) and 5 (down).
		b := byte(5)
		if e.WheelDelta < 0 {
			b = 4
		}
		if err := p.button(b, true); err != nil {
			return err
		}
		return p.button(b, false)
	}
	return nil
}

func (p *x11Inputer) key(k xproto.Keycode, down bool) error {
	if k < p.minKey || k > p.maxKey {
		return nil
	}
	typ := byte(xproto.KeyPress)
	if !down {
		typ = xproto.KeyRelease
	}
	if down {
		p.keys[k] = true
	} else {
		delete(p.keys, k)
	}
	return xtest.FakeInputChecked(p.conn, typ, byte(k), 0, p.root, 0, 0, 0).Check()
}

func (p *x11Inputer) button(b byte, down bool) error {
	typ := byte(xproto.ButtonPress)
	if !down {
		typ = xproto.ButtonRelease
	}
	if down {
		p.buttons[b] = true
	} else {
		delete(p.buttons, b)
	}
	return xtest.FakeInputChecked(p.conn, typ, b, 0, p.root, 0, 0, 0).Check()
}

func (p *x11Inputer) move(x, y int) error {
	x, y = max(0, min(x, 32767)), max(0, min(y, 32767))
	// Unchecked: a drag is many of these, and the next checked request
	// reports any error the server had.
	xtest.FakeInput(p.conn, xproto.MotionNotify, 0, 0, p.root, int16(x), int16(y), 0)
	return nil
}

// char types one UTF-16 unit as itself: the keysym for it is put on a
// spare keycode, which is pressed, the way xdotool types what the layout
// has no key for. A surrogate is not a character on its own and is
// dropped.
func (p *x11Inputer) char(u uint16, down bool) error {
	if u >= 0xD800 && u <= 0xDFFF {
		return nil
	}
	if !down {
		k, ok := p.typing[u]
		if !ok {
			return nil
		}
		delete(p.typing, u)
		return p.key(k, false)
	}
	if len(p.spare) == 0 {
		return nil
	}
	k := p.spare[p.next%len(p.spare)]
	p.next++
	sym := xproto.Keysym(u)
	if u > 0xFF {
		sym = xproto.Keysym(0x01000000 | uint32(u))
	}
	if err := xproto.ChangeKeyboardMappingChecked(p.conn, 1, k, 2,
		[]xproto.Keysym{sym, sym}).Check(); err != nil {
		return err
	}
	p.typing[u] = k
	return p.key(k, true)
}

// locks sets Caps, Num and Scroll Lock as the browser has them, by tapping
// each one that differs: X's LEDs say how they are.
func (p *x11Inputer) locks(want uint8) error {
	ctl, err := xproto.GetKeyboardControl(p.conn).Reply()
	if err != nil {
		return err
	}
	for _, l := range []struct {
		bit uint8
		led uint32
		key uint16
	}{{rdp.LockCaps, 1, 58}, {rdp.LockNum, 2, 69}, {rdp.LockScroll, 4, 70}} {
		if (ctl.LedMask&l.led != 0) == (want&l.bit != 0) {
			continue
		}
		if err := p.key(xproto.Keycode(l.key+8), true); err != nil {
			return err
		}
		if err := p.key(xproto.Keycode(l.key+8), false); err != nil {
			return err
		}
	}
	return nil
}

// Close lets go of whatever is still held down: a session that ends
// mid-drag or with Shift down must not leave the console that way.
func (p *x11Inputer) Close() {
	if p.conn == nil {
		return
	}
	for k := range p.keys {
		_ = p.key(k, false)
	}
	for b := range p.buttons {
		_ = p.button(b, false)
	}
	p.conn.Close()
	p.conn = nil
}

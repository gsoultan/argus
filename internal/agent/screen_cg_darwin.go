//go:build darwin

package agent

import (
	"errors"
	"fmt"
	"image"
	"math"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/gsoultan/argus/internal/rdp"
)

/*
The Mac's display through CoreGraphics, without cgo.

The agent is built without cgo, so the frameworks are opened and called
through purego. Capture is a CGDisplayStream: the window server hands over
an IOSurface for each frame that changed, with the rectangles that did, at
up to sixty a second, the pointer drawn in. Only the newest surface is kept,
and only its changed rectangles are copied out, on the helper's capture
thread, when the next frame is wanted: a busy screen costs the copy of what
changed, not of every frame the window server drew.

The stream is at the display's size in points, not its Retina pixels: what
the person sees laid out, a quarter of the pixels to read, compare and send,
and the same coordinates input is played in.

Input is CGEvents posted to the HID event tap, as a keyboard and mouse
would: by key position (screen_keys.go), with the modifier flags held set on
every event, drags as drags, and double clicks counted, since macOS only
treats a click as a double click when the event says it is one.
*/

// cptr is memory the frameworks returned, by address. Through a pointer
// to it, so it is plainly not Go memory being hidden from the collector.
func cptr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

// cgPoint is a CGPoint.
type cgPoint struct{ X, Y float64 }

// cgRect is a CGRect.
type cgRect struct{ X, Y, W, H float64 }

// coreGraphics holds the functions the helper calls.
type coreGraphics struct {
	mainDisplayID       func() uint32
	displayPixelsWide   func(uint32) uintptr
	displayPixelsHigh   func(uint32) uintptr
	preflightCapture    func() bool
	requestCapture      func() bool
	axIsProcessTrusted  func() bool
	streamCreate        func(display uint32, w, h uintptr, format int32, props, queue uintptr, handler objc.Block) uintptr
	streamStart         func(uintptr) int32
	streamStop          func(uintptr) int32
	updateGetRects      func(update uintptr, kind int32, count *uintptr) uintptr
	eventSourceCreate   func(state int32) uintptr
	eventCreate         func(source uintptr) uintptr
	eventCreateMouse    func(source uintptr, typ uint32, at cgPoint, button uint32) uintptr
	eventCreateKeyboard func(source uintptr, key uint16, down bool) uintptr
	eventSetUnicode     func(ev uintptr, n uintptr, s *uint16)
	eventSetFlags       func(ev uintptr, flags uint64)
	eventSetType        func(ev uintptr, typ uint32)
	eventSetLocation    func(ev uintptr, at cgPoint)
	eventSetInt         func(ev uintptr, field uint32, v int64)
	eventPost           func(tap uint32, ev uintptr)

	cfRelease          func(uintptr)
	cfRetain           func(uintptr) uintptr
	cfDictionaryCreate func(alloc uintptr, keys, values *uintptr, n int, keyCB, valCB uintptr) uintptr
	cfNumberCreate     func(alloc uintptr, typ int, value unsafe.Pointer) uintptr

	surfaceLock        func(s uintptr, opts uint32, seed *uint32) int32
	surfaceUnlock      func(s uintptr, opts uint32, seed *uint32) int32
	surfaceBase        func(s uintptr) uintptr
	surfaceBytesPerRow func(s uintptr) uintptr
	surfaceWidth       func(s uintptr) uintptr
	surfaceHeight      func(s uintptr) uintptr
	surfaceIncUse      func(s uintptr)
	surfaceDecUse      func(s uintptr)

	dispatchQueueCreate func(label *byte, attr uintptr) uintptr

	// Constants the frameworks export as data.
	keyShowCursor, keyMinFrameTime uintptr
	cfBooleanTrue                  uintptr
	dictKeyCallBacks, dictValueCBs uintptr
}

var (
	cgOnce   sync.Once
	cgLoaded *coreGraphics
	cgErr    error
)

const (
	cgPath   = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"
	cfPath   = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	iosPath  = "/System/Library/Frameworks/IOSurface.framework/IOSurface"
	appsPath = "/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices"
	sysPath  = "/usr/lib/libSystem.B.dylib"
)

// loadCoreGraphics opens the frameworks once. A symbol that is missing is
// an error, not a crash later.
func loadCoreGraphics() (cg *coreGraphics, err error) {
	cgOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				cgErr = fmt.Errorf("loading CoreGraphics: %v", r)
			}
		}()
		cgLoaded, cgErr = openCoreGraphics()
	})
	return cgLoaded, cgErr
}

func openCoreGraphics() (*coreGraphics, error) {
	open := func(path string) (uintptr, error) {
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return 0, fmt.Errorf("opening %s: %w", path, err)
		}
		return h, nil
	}
	cgH, err := open(cgPath)
	if err != nil {
		return nil, err
	}
	cfH, err := open(cfPath)
	if err != nil {
		return nil, err
	}
	iosH, err := open(iosPath)
	if err != nil {
		return nil, err
	}
	appsH, err := open(appsPath)
	if err != nil {
		return nil, err
	}
	sysH, err := open(sysPath)
	if err != nil {
		return nil, err
	}
	// Each symbol is looked up first, so a missing one is an error rather
	// than RegisterLibFunc's panic.
	var missing []string
	reg := func(fptr any, h uintptr, name string) {
		if _, err := purego.Dlsym(h, name); err != nil {
			missing = append(missing, name)
			return
		}
		purego.RegisterLibFunc(fptr, h, name)
	}
	data := func(h uintptr, name string, deref bool) uintptr {
		p, err := purego.Dlsym(h, name)
		if err != nil {
			missing = append(missing, name)
			return 0
		}
		if deref {
			return *(*uintptr)(cptr(p))
		}
		return p
	}
	cg := &coreGraphics{}
	reg(&cg.mainDisplayID, cgH, "CGMainDisplayID")
	reg(&cg.displayPixelsWide, cgH, "CGDisplayPixelsWide")
	reg(&cg.displayPixelsHigh, cgH, "CGDisplayPixelsHigh")
	reg(&cg.preflightCapture, cgH, "CGPreflightScreenCaptureAccess")
	reg(&cg.requestCapture, cgH, "CGRequestScreenCaptureAccess")
	reg(&cg.axIsProcessTrusted, appsH, "AXIsProcessTrusted")
	reg(&cg.streamCreate, cgH, "CGDisplayStreamCreateWithDispatchQueue")
	reg(&cg.streamStart, cgH, "CGDisplayStreamStart")
	reg(&cg.streamStop, cgH, "CGDisplayStreamStop")
	reg(&cg.updateGetRects, cgH, "CGDisplayStreamUpdateGetRects")
	reg(&cg.eventSourceCreate, cgH, "CGEventSourceCreate")
	reg(&cg.eventCreate, cgH, "CGEventCreate")
	reg(&cg.eventCreateMouse, cgH, "CGEventCreateMouseEvent")
	reg(&cg.eventCreateKeyboard, cgH, "CGEventCreateKeyboardEvent")
	reg(&cg.eventSetUnicode, cgH, "CGEventKeyboardSetUnicodeString")
	reg(&cg.eventSetFlags, cgH, "CGEventSetFlags")
	reg(&cg.eventSetType, cgH, "CGEventSetType")
	reg(&cg.eventSetLocation, cgH, "CGEventSetLocation")
	reg(&cg.eventSetInt, cgH, "CGEventSetIntegerValueField")
	reg(&cg.eventPost, cgH, "CGEventPost")
	reg(&cg.cfRelease, cfH, "CFRelease")
	reg(&cg.cfRetain, cfH, "CFRetain")
	reg(&cg.cfDictionaryCreate, cfH, "CFDictionaryCreate")
	reg(&cg.cfNumberCreate, cfH, "CFNumberCreate")
	reg(&cg.surfaceLock, iosH, "IOSurfaceLock")
	reg(&cg.surfaceUnlock, iosH, "IOSurfaceUnlock")
	reg(&cg.surfaceBase, iosH, "IOSurfaceGetBaseAddress")
	reg(&cg.surfaceBytesPerRow, iosH, "IOSurfaceGetBytesPerRow")
	reg(&cg.surfaceWidth, iosH, "IOSurfaceGetWidth")
	reg(&cg.surfaceHeight, iosH, "IOSurfaceGetHeight")
	reg(&cg.surfaceIncUse, iosH, "IOSurfaceIncrementUseCount")
	reg(&cg.surfaceDecUse, iosH, "IOSurfaceDecrementUseCount")
	reg(&cg.dispatchQueueCreate, sysH, "dispatch_queue_create")
	cg.keyShowCursor = data(cgH, "kCGDisplayStreamShowCursor", true)
	cg.keyMinFrameTime = data(cgH, "kCGDisplayStreamMinimumFrameTime", true)
	cg.cfBooleanTrue = data(cfH, "kCFBooleanTrue", true)
	cg.dictKeyCallBacks = data(cfH, "kCFTypeDictionaryKeyCallBacks", false)
	cg.dictValueCBs = data(cfH, "kCFTypeDictionaryValueCallBacks", false)
	if len(missing) > 0 {
		return nil, fmt.Errorf("this macOS lacks what screen sharing uses: %v", missing)
	}
	return cg, nil
}

func (cg *coreGraphics) screenAllowed() bool {
	if cg.preflightCapture() {
		return true
	}
	// Puts argus-agent in the list for the person to allow, and asks them.
	cg.requestCapture()
	return false
}

func (cg *coreGraphics) inputAllowed() bool { return cg.axIsProcessTrusted() }

/* ── Capture ─────────────────────────────────────────────────────────────── */

const (
	cgFormatBGRA           = 0x42475241 // 'BGRA'
	cgFrameComplete        = 0
	cgFrameStopped         = 3
	cgUpdateDirtyRects     = 2
	ioSurfaceLockReadOnly  = 1
	cfNumberFloat64Type    = 6
	cgStreamRecreateCheck  = time.Second
	cgStreamStartupTimeout = 5 * time.Second
)

// macCapturer is the main display, as a CGDisplayStream shows it.
type macCapturer struct {
	cg      *coreGraphics
	display uint32
	queue   uintptr
	stream  uintptr
	handler objc.Block

	width, height int
	fb            []byte

	mu sync.Mutex
	// latest is the newest surface not yet read, held with a use count.
	latest uintptr
	// dirty is where the screen changed since it was last read; all
	// means everywhere.
	dirty   []image.Rectangle
	all     bool
	stopped bool
	ready   chan struct{} // a frame is waiting

	whole     bool
	lastCheck time.Time
}

func (c *macCapturer) kind() string { return "cgdisplaystream" }

func newMacCapturer(cg *coreGraphics) (*macCapturer, error) {
	label := []byte("argus.screen\x00")
	c := &macCapturer{cg: cg, queue: cg.dispatchQueueCreate(&label[0], 0), ready: make(chan struct{}, 1)}
	c.handler = objc.NewBlock(func(_ objc.Block, status int32, _ uint64, surface, update uintptr) {
		c.frame(status, surface, update)
	})
	if err := c.start(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// start opens a stream at the main display's size.
func (c *macCapturer) start() error {
	cg := c.cg
	c.display = cg.mainDisplayID()
	w, h := int(cg.displayPixelsWide(c.display)), int(cg.displayPixelsHigh(c.display))
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return fmt.Errorf("the display reports a size of %dx%d", w, h)
	}
	// Sixty frames a second at most, the pointer drawn in.
	frameTime := 1.0 / 60
	num := cg.cfNumberCreate(0, cfNumberFloat64Type, unsafe.Pointer(&frameTime))
	keys := []uintptr{cg.keyShowCursor, cg.keyMinFrameTime}
	values := []uintptr{cg.cfBooleanTrue, num}
	props := cg.cfDictionaryCreate(0, &keys[0], &values[0], len(keys), cg.dictKeyCallBacks, cg.dictValueCBs)
	cg.cfRelease(num)
	stream := cg.streamCreate(c.display, uintptr(w), uintptr(h), cgFormatBGRA, props, c.queue, c.handler)
	cg.cfRelease(props)
	if stream == 0 {
		return errors.New("macOS would not stream the display; is argus-agent allowed to record the screen?")
	}
	c.mu.Lock()
	c.stopped = false
	c.mu.Unlock()
	if rc := cg.streamStart(stream); rc != 0 {
		cg.cfRelease(stream)
		return fmt.Errorf("macOS would not start streaming the display (error %d)", rc)
	}
	c.stream, c.width, c.height = stream, w, h
	c.fb = make([]byte, w*h*4)
	c.whole = true
	c.lastCheck = time.Now()
	return nil
}

func (c *macCapturer) stop() {
	if c.stream == 0 {
		return
	}
	c.cg.streamStop(c.stream)
	c.cg.cfRelease(c.stream)
	c.stream = 0
	c.mu.Lock()
	if c.latest != 0 {
		c.cg.surfaceDecUse(c.latest)
		c.cg.cfRelease(c.latest)
		c.latest = 0
	}
	c.dirty, c.all = nil, false
	c.mu.Unlock()
}

// frame is the stream's handler, on the dispatch queue's thread: it keeps
// the newest surface and adds up where the screen changed, and copies no
// pixels.
func (c *macCapturer) frame(status int32, surface, update uintptr) {
	switch status {
	case cgFrameComplete:
	case cgFrameStopped:
		c.mu.Lock()
		c.stopped = true
		c.mu.Unlock()
		select {
		case c.ready <- struct{}{}:
		default:
		}
		return
	default:
		return // idle or blank: nothing new
	}
	if surface == 0 {
		return
	}
	var n uintptr
	rects := c.cg.updateGetRects(update, cgUpdateDirtyRects, &n)
	c.cg.cfRetain(surface)
	c.cg.surfaceIncUse(surface)
	c.mu.Lock()
	if c.latest != 0 {
		c.cg.surfaceDecUse(c.latest)
		c.cg.cfRelease(c.latest)
	}
	c.latest = surface
	if rects == 0 || n == 0 {
		c.all = true
	} else if !c.all {
		for _, r := range unsafe.Slice((*cgRect)(cptr(rects)), n) {
			c.dirty = append(c.dirty, image.Rect(int(math.Floor(r.X)), int(math.Floor(r.Y)),
				int(math.Ceil(r.X+r.W)), int(math.Ceil(r.Y+r.H))))
		}
		if len(c.dirty) > 256 {
			c.all = true
			c.dirty = nil
		}
	}
	c.mu.Unlock()
	select {
	case c.ready <- struct{}{}:
	default:
	}
}

func (c *macCapturer) Capture(wait time.Duration) (screenShot, bool, error) {
	// A change of resolution is a new stream at the new size.
	if time.Since(c.lastCheck) >= cgStreamRecreateCheck {
		c.lastCheck = time.Now()
		w, h := int(c.cg.displayPixelsWide(c.cg.mainDisplayID())), int(c.cg.displayPixelsHigh(c.cg.mainDisplayID()))
		if (w != c.width || h != c.height) && w > 0 && h > 0 {
			c.stop()
			if err := c.start(); err != nil {
				return screenShot{}, false, err
			}
		}
	}
	if c.stream == 0 {
		if err := c.start(); err != nil {
			return screenShot{}, false, err
		}
	}
	if c.whole {
		// The first frame: wait for it, however long the caller would.
		wait = max(wait, cgStreamStartupTimeout)
	}
	c.mu.Lock()
	has := c.latest != 0
	c.mu.Unlock()
	if !has {
		t := time.NewTimer(wait)
		select {
		case <-c.ready:
		case <-t.C:
		}
		t.Stop()
	}
	c.mu.Lock()
	surface, dirty, all, stopped := c.latest, c.dirty, c.all, c.stopped
	c.latest, c.dirty, c.all = 0, nil, false
	c.mu.Unlock()
	if stopped {
		c.stop()
		return screenShot{}, false, errors.New("macOS stopped streaming the display")
	}
	if surface == 0 {
		return screenShot{}, false, nil
	}
	defer func() {
		c.cg.surfaceDecUse(surface)
		c.cg.cfRelease(surface)
	}()
	bounds := image.Rect(0, 0, c.width, c.height)
	if c.whole || all {
		dirty = []image.Rectangle{bounds}
	}
	if err := c.copyOut(surface, dirty); err != nil {
		return screenShot{}, false, err
	}
	shot := screenShot{fb: c.fb, stride: c.width * 4, width: c.width, height: c.height, dirty: dirty}
	if c.whole || all {
		shot.dirty = nil
	}
	c.whole = false
	return shot, true, nil
}

// copyOut copies the rectangles of a surface into fb.
func (c *macCapturer) copyOut(surface uintptr, rects []image.Rectangle) error {
	cg := c.cg
	if rc := cg.surfaceLock(surface, ioSurfaceLockReadOnly, nil); rc != 0 {
		return fmt.Errorf("could not read the display's frame (error %d)", rc)
	}
	defer cg.surfaceUnlock(surface, ioSurfaceLockReadOnly, nil)
	base := cg.surfaceBase(surface)
	pitch := int(cg.surfaceBytesPerRow(surface))
	sw, sh := int(cg.surfaceWidth(surface)), int(cg.surfaceHeight(surface))
	if base == 0 || pitch < sw*4 {
		return errors.New("the display's frame has no pixels")
	}
	src := unsafe.Slice((*byte)(cptr(base)), pitch*sh)
	bounds := image.Rect(0, 0, min(sw, c.width), min(sh, c.height))
	stride := c.width * 4
	for _, r := range rects {
		r = r.Intersect(bounds)
		if r.Empty() {
			continue
		}
		n := r.Dx() * 4
		for y := r.Min.Y; y < r.Max.Y; y++ {
			copy(c.fb[y*stride+r.Min.X*4:y*stride+r.Min.X*4+n], src[y*pitch+r.Min.X*4:y*pitch+r.Min.X*4+n])
		}
	}
	return nil
}

func (c *macCapturer) Close() {
	streaming := c.stream != 0
	c.stop()
	if c.handler == 0 {
		return
	}
	// A stopped stream still has calls to the handler waiting on its
	// queue, the last of them saying it stopped. Freeing the handler
	// before that one crashes the process when it arrives, so wait for
	// it, and keep the handler rather than free it under a call.
	if streaming && !c.waitStopped(2*time.Second) {
		c.handler = 0
		return
	}
	c.handler.Release()
	c.handler = 0
}

// waitStopped reports whether the stream said it stopped within d.
func (c *macCapturer) waitStopped(d time.Duration) bool {
	for end := time.Now().Add(d); ; {
		c.mu.Lock()
		stopped := c.stopped
		c.mu.Unlock()
		if stopped || time.Now().After(end) {
			return stopped
		}
		time.Sleep(time.Millisecond)
	}
}

/* ── Input ───────────────────────────────────────────────────────────────── */

// CGEvent types, fields and flags.
const (
	cgEventLeftDown      = 1
	cgEventLeftUp        = 2
	cgEventRightDown     = 3
	cgEventRightUp       = 4
	cgEventMouseMoved    = 5
	cgEventLeftDragged   = 6
	cgEventRightDragged  = 7
	cgEventScrollWheel   = 22
	cgEventOtherDown     = 25
	cgEventOtherUp       = 26
	cgEventOtherDragged  = 27
	cgFieldClickState    = 1
	cgFieldScrollAxis1   = 11
	cgHIDEventTap        = 0
	cgSourceHIDState     = 1
	cgFlagCapsLock       = 0x00010000
	cgFlagShift          = 0x00020000
	cgFlagControl        = 0x00040000
	cgFlagOption         = 0x00080000
	cgFlagCommand        = 0x00100000
	macDoubleClickTime   = 500 * time.Millisecond
	macDoubleClickTravel = 4
)

// macInputer plays input as CGEvents.
type macInputer struct {
	cg     *coreGraphics
	source uintptr
	x, y   int
	// held modifier keys by key code, and the flags they make.
	mods map[uint16]bool
	caps bool
	// keys and buttons held down, to let go of at the end.
	keys    map[uint16]bool
	buttons map[int]bool
	// The last press, to count double and triple clicks.
	lastButton int
	lastPress  time.Time
	lastAt     image.Point
	clicks     int64
}

func newMacInputer(cg *coreGraphics) *macInputer {
	return &macInputer{cg: cg, source: cg.eventSourceCreate(cgSourceHIDState),
		mods: map[uint16]bool{}, keys: map[uint16]bool{}, buttons: map[int]bool{}, lastButton: -1}
}

func (p *macInputer) flags() uint64 {
	var f uint64
	for k := range p.mods {
		switch k {
		case 0x38, 0x3C:
			f |= cgFlagShift
		case 0x3B, 0x3E:
			f |= cgFlagControl
		case 0x3A, 0x3D:
			f |= cgFlagOption
		case 0x37, 0x36:
			f |= cgFlagCommand
		}
	}
	if p.caps {
		f |= cgFlagCapsLock
	}
	return f
}

// post sends an event with the modifiers held, and frees it.
func (p *macInputer) post(ev uintptr) error {
	if ev == 0 {
		return errors.New("macOS would not make the input event")
	}
	p.cg.eventSetFlags(ev, p.flags())
	p.cg.eventPost(cgHIDEventTap, ev)
	p.cg.cfRelease(ev)
	return nil
}

func (p *macInputer) Input(e rdp.InputEvent) error {
	switch e.Kind {
	case "key":
		k, ok := macKey(e.Scancode, e.Extended)
		if !ok {
			return nil
		}
		return p.key(k, e.Down)
	case "unicode":
		if e.Unicode >= 0xD800 && e.Unicode <= 0xDFFF {
			return nil
		}
		ev := p.cg.eventCreateKeyboard(p.source, 0, e.Down)
		if ev == 0 {
			return errors.New("macOS would not make the key event")
		}
		u := e.Unicode
		p.cg.eventSetUnicode(ev, 1, &u)
		return p.post(ev)
	case "sync":
		// macOS keeps Caps Lock to itself; it is not set from here.
		return nil
	case "mouse":
		return p.move(e.X, e.Y)
	case "button":
		return p.button(e.X, e.Y, e.Button, e.Down)
	case "wheel":
		p.x, p.y = e.X, e.Y
		ev := p.cg.eventCreate(p.source)
		if ev == 0 {
			return errors.New("macOS would not make the scroll event")
		}
		p.cg.eventSetType(ev, cgEventScrollWheel)
		p.cg.eventSetLocation(ev, cgPoint{float64(e.X), float64(e.Y)})
		// One notch, three lines, in its direction: positive is down in
		// the browser and up on a Mac.
		lines := int64(-3)
		if e.WheelDelta < 0 {
			lines = 3
		}
		p.cg.eventSetInt(ev, cgFieldScrollAxis1, lines)
		return p.post(ev)
	}
	return nil
}

func (p *macInputer) key(k uint16, down bool) error {
	switch k {
	case 0x38, 0x3C, 0x3B, 0x3E, 0x3A, 0x3D, 0x37, 0x36:
		if down {
			p.mods[k] = true
		} else {
			delete(p.mods, k)
		}
	case 0x39:
		if down {
			p.caps = !p.caps
		}
	}
	if down {
		p.keys[k] = true
	} else {
		delete(p.keys, k)
	}
	return p.post(p.cg.eventCreateKeyboard(p.source, k, down))
}

func (p *macInputer) move(x, y int) error {
	p.x, p.y = x, y
	typ, button := uint32(cgEventMouseMoved), uint32(0)
	switch {
	case p.buttons[0]:
		typ = cgEventLeftDragged
	case p.buttons[2]:
		typ, button = cgEventRightDragged, 1
	case p.buttons[1]:
		typ, button = cgEventOtherDragged, 2
	}
	return p.post(p.cg.eventCreateMouse(p.source, typ, cgPoint{float64(x), float64(y)}, button))
}

func (p *macInputer) button(x, y, b int, down bool) error {
	var typ, cgButton uint32
	switch b {
	case 0:
		typ, cgButton = cgEventLeftUp, 0
		if down {
			typ = cgEventLeftDown
		}
	case 2:
		typ, cgButton = cgEventRightUp, 1
		if down {
			typ = cgEventRightDown
		}
	case 1:
		typ, cgButton = cgEventOtherUp, 2
		if down {
			typ = cgEventOtherDown
		}
	default:
		return nil
	}
	at := image.Pt(x, y)
	if down {
		// A second press soon after, close by, with the same button is a
		// double click; macOS counts only what the event says.
		d := at.Sub(p.lastAt)
		if b == p.lastButton && time.Since(p.lastPress) < macDoubleClickTime &&
			d.X*d.X+d.Y*d.Y <= macDoubleClickTravel*macDoubleClickTravel {
			p.clicks++
		} else {
			p.clicks = 1
		}
		p.lastButton, p.lastPress, p.lastAt = b, time.Now(), at
		p.buttons[b] = true
	} else {
		delete(p.buttons, b)
	}
	p.x, p.y = x, y
	ev := p.cg.eventCreateMouse(p.source, typ, cgPoint{float64(x), float64(y)}, cgButton)
	if ev != 0 {
		p.cg.eventSetInt(ev, cgFieldClickState, max(p.clicks, 1))
	}
	return p.post(ev)
}

// Close lets go of whatever is still held down.
func (p *macInputer) Close() {
	for k := range p.keys {
		_ = p.key(k, false)
	}
	for b := range p.buttons {
		_ = p.button(p.x, p.y, b, false)
	}
	if p.source != 0 {
		p.cg.cfRelease(p.source)
		p.source = 0
	}
}

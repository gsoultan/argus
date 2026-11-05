package agent

import (
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/gsoultan/argus/internal/rdp"
)

/*
Screen sharing on Windows: the agent's side starts the helper in the console
session; the helper (RunScreenHelper) captures with GDI and plays input with
SendInput. See screen.go for the design.

GDI rather than the Desktop Duplication API: it works on every edition and on
virtual machines with no display driver of their own, it can follow the
screen onto the sign-in and UAC desktops, and it needs no COM. Duplication
is faster and is the change to make if capture is ever the bottleneck.
*/

var (
	modWtsapi32                     = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformationW = modWtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory               = modWtsapi32.NewProc("WTSFreeMemory")

	modSas      = windows.NewLazySystemDLL("sas.dll")
	procSendSAS = modSas.NewProc("SendSAS")

	modGdi32                  = windows.NewLazySystemDLL("gdi32.dll")
	procCreateCompatibleDC    = modGdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection      = modGdi32.NewProc("CreateDIBSection")
	procSelectObject          = modGdi32.NewProc("SelectObject")
	procBitBlt                = modGdi32.NewProc("BitBlt")
	procDeleteDC              = modGdi32.NewProc("DeleteDC")
	procDeleteObject          = modGdi32.NewProc("DeleteObject")
	procGdiFlush              = modGdi32.NewProc("GdiFlush")
	procGetDC                 = modUser32.NewProc("GetDC")
	procReleaseDC             = modUser32.NewProc("ReleaseDC")
	procGetSystemMetrics      = modUser32.NewProc("GetSystemMetrics")
	procOpenInputDesktop      = modUser32.NewProc("OpenInputDesktop")
	procSetThreadDesktop      = modUser32.NewProc("SetThreadDesktop")
	procSendInput             = modUser32.NewProc("SendInput")
	procGetCursorInfo         = modUser32.NewProc("GetCursorInfo")
	procGetIconInfo           = modUser32.NewProc("GetIconInfo")
	procDrawIconEx            = modUser32.NewProc("DrawIconEx")
	procSetProcessDpiAwareCtx = modUser32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware    = modUser32.NewProc("SetProcessDPIAware")
)

const (
	wtsUserName   = 5
	wtsDomainName = 7

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	srcCopy  = 0x00CC0020
	diNormal = 0x0003

	cursorShowing = 0x00000001

	inputMouse    = 0
	inputKeyboard = 1

	mouseMove       = 0x0001
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseWheel      = 0x0800
	mouseVirtualDsk = 0x4000
	mouseAbsolute   = 0x8000

	keyExtendedKey = 0x0001
	keyUp          = 0x0002
	keyScancode    = 0x0008

	genericAll = 0x10000000
)

/* ── The agent's side ────────────────────────────────────────────────────── */

type windowsScreenHost struct{}

func platformScreenHost() screenHost { return windowsScreenHost{} }

func (windowsScreenHost) ConsoleSession() (uint32, bool) {
	id := windows.WTSGetActiveConsoleSessionId()
	return id, id != 0xFFFFFFFF
}

func (windowsScreenHost) ConsoleAccount(session uint32) string {
	user := wtsString(session, wtsUserName)
	if user == "" {
		return ""
	}
	if domain := wtsString(session, wtsDomainName); domain != "" {
		return domain + `\` + user
	}
	return user
}

func wtsString(session uint32, class uint32) string {
	var buf *uint16
	var n uint32
	r, _, _ := procWTSQuerySessionInformationW.Call(0, uintptr(session), uintptr(class),
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == nil {
		return ""
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buf)))
	return windows.UTF16PtrToString(buf)
}

// sasPolicy is where Windows says who may send Ctrl+Alt+Del in software.
const sasPolicy = `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`

var sasOnce sync.Once

// SecureAttention sends Ctrl+Alt+Del. Windows takes it from a service only
// when the SoftwareSASGeneration policy allows services; where the policy
// is not set at all the agent sets it to that, once, and says so. A value an
// administrator set is left alone.
func (windowsScreenHost) SecureAttention() error {
	var perr error
	sasOnce.Do(func() {
		k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, sasPolicy,
			registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			perr = err
			return
		}
		defer k.Close()
		v, _, err := k.GetIntegerValue("SoftwareSASGeneration")
		switch {
		case errors.Is(err, registry.ErrNotExist):
			if err := k.SetDWordValue("SoftwareSASGeneration", 1); err != nil {
				perr = err
				return
			}
			slog.Default().Info("screen sharing: allowed services to send Ctrl+Alt+Del " +
				"(SoftwareSASGeneration = 1), so the console's sign-in screen can be reached")
		case err != nil:
			perr = err
		case v&1 == 0:
			perr = errors.New("this host's SoftwareSASGeneration policy does not let " +
				"services send Ctrl+Alt+Del")
		}
	})
	if perr != nil {
		return perr
	}
	if err := procSendSAS.Find(); err != nil {
		return err
	}
	procSendSAS.Call(0)
	return nil
}

// screenHelperPipe is the helper's stdin and stdout, and the job that ends
// it when they close.
type screenHelperPipe struct {
	r, w *os.File
	job  windows.Handle
	proc windows.Handle
	once sync.Once
}

func (p *screenHelperPipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *screenHelperPipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *screenHelperPipe) Close() error {
	p.once.Do(func() {
		// The end of its stdin tells the helper to stop, and it lets go of
		// any key or button it is holding down on the way out. Closed apart:
		// closing a pipe waits for a write still blocked on it, and one is
		// when the helper has stopped reading, which only its end undoes.
		closed := make(chan struct{})
		go func() {
			_ = p.w.Close()
			close(closed)
		}()
		// A moment to exit, then the job ends it whatever it is doing. An
		// ending session must not wait on the helper: the agent's shutdown
		// waits on the session.
		_, _ = windows.WaitForSingleObject(p.proc, uint32(screenHelperExit/time.Millisecond))
		_ = windows.TerminateJobObject(p.job, 1)
		windows.CloseHandle(p.job)
		windows.CloseHandle(p.proc)
		<-closed
		_ = p.r.Close()
	})
	return nil
}

// screenHelperExit is how long a helper told to stop has before it is ended.
const screenHelperExit = time.Second

// StartHelper runs "argus-agent screen-helper" in the session as
// LocalSystem: the agent's own token, moved to that session. Its only
// handles are the two pipes.
func (windowsScreenHost) StartHelper(session uint32, mode helperMode) (io.ReadWriteCloser, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &self); err != nil {
		return nil, err
	}
	defer self.Close()
	var token windows.Token
	if err := windows.DuplicateTokenEx(self, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityIdentification, windows.TokenPrimary, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	if err := windows.SetTokenInformation(token, windows.TokenSessionId,
		(*byte)(unsafe.Pointer(&session)), uint32(unsafe.Sizeof(session))); err != nil {
		return nil, fmt.Errorf("moving the helper to the console session: %w", err)
	}

	// Two pipes; the child's ends inheritable, ours not.
	sa := &windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, sa, 0); err != nil {
		return nil, err
	}
	// The screen's pipe holds a whole batch of tiles: the default 4 KB made
	// one 192 KB batch fifty round trips between the two processes.
	if err := windows.CreatePipe(&outR, &outW, sa, screenPipeBuffer); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, err
	}
	closeAll := func() {
		for _, h := range []windows.Handle{inR, inW, outR, outW} {
			windows.CloseHandle(h)
		}
	}
	_ = windows.SetHandleInformation(inW, windows.HANDLE_FLAG_INHERIT, 0)
	_ = windows.SetHandleInformation(outR, windows.HANDLE_FLAG_INHERIT, 0)

	// Only the two pipe ends are inherited, whatever else in this process
	// happens to be inheritable.
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		closeAll()
		return nil, err
	}
	defer attrs.Delete()
	handles := []windows.Handle{inR, outW}
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		closeAll()
		return nil, err
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop = windows.StringToUTF16Ptr(`winsta0\default`)
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput = inR, outW

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		closeAll()
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		closeAll()
		return nil, err
	}

	args := []string{exe, "screen-helper"}
	if mode.gdiOnly {
		args = append(args, "--gdi")
	}
	if mode.video {
		args = append(args, "--video")
	}
	cmdline := windows.ComposeCommandLine(args)
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(token, nil, windows.StringToUTF16Ptr(cmdline),
		nil, nil, true,
		windows.CREATE_SUSPENDED|windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_NO_WINDOW,
		nil, nil, &si.StartupInfo, &pi); err != nil {
		windows.CloseHandle(job)
		closeAll()
		return nil, fmt.Errorf("starting the screen helper: %w", err)
	}
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 255)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(job)
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		return nil, err
	}
	_, _ = windows.ResumeThread(pi.Thread)
	windows.CloseHandle(pi.Thread)
	return &screenHelperPipe{
		r:   os.NewFile(uintptr(outR), "screen-helper-out"),
		w:   os.NewFile(uintptr(inW), "screen-helper-in"),
		job: job, proc: pi.Process,
	}, nil
}

// screenPipeBuffer is the size of the pipe the screen comes through.
const screenPipeBuffer = 1 << 20

/* ── The helper ──────────────────────────────────────────────────────────── */

// RunScreenHelper is "argus-agent screen-helper": the agent runs it in the
// console session and it serves the screen on stdin and stdout until stdin
// closes. "--gdi" keeps it to GDI capture, which the agent asks for after a
// helper has failed; "--video" lets it send motion as H.264.
func RunScreenHelper(args []string, in io.Reader, out io.Writer) int {
	// Real pixels, not the scaled ones a process that is not DPI aware sees.
	if procSetProcessDpiAwareCtx.Find() == nil {
		procSetProcessDpiAwareCtx.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2, -4
	} else if procSetProcessDPIAware.Find() == nil {
		procSetProcessDPIAware.Call()
	}
	gdiOnly, video := false, false
	for _, a := range args {
		switch a {
		case "--gdi":
			gdiOnly = true
		case "--video":
			video = true
		}
	}
	c := newConsoleCapturer(gdiOnly)
	newInput := func() screenInputer { return &inputPlayer{} }
	var newVideo func(int, int) (screenVideo, error)
	if video {
		newVideo = newMFEncoder
	}
	if err := serveScreen(c, newInput, in, out, defaultScreenPace, newVideo); err != nil {
		_ = writeScreenMsg(out, scrFail, []byte(err.Error()))
		return 1
	}
	return 0
}

// inputDesktop keeps a thread on whichever desktop is taking input now: the
// user's, the sign-in screen's or a UAC prompt's. It looks again at most
// every quarter second, and at once when asked to after a failure: opening
// the desktop on every event cost a system call per mouse move.
type inputDesktop struct {
	desk   uintptr
	looked time.Time
}

func (d *inputDesktop) follow(now bool) {
	if !now && d.desk != 0 && time.Since(d.looked) < 250*time.Millisecond {
		return
	}
	d.looked = time.Now()
	h, _, _ := procOpenInputDesktop.Call(0, 0, genericAll)
	if h == 0 {
		return
	}
	if r, _, _ := procSetThreadDesktop.Call(h); r == 0 {
		procCloseDesktop.Call(h)
		return
	}
	if d.desk != 0 {
		procCloseDesktop.Call(d.desk)
	}
	d.desk = h
}

func (d *inputDesktop) close() {
	if d.desk != 0 {
		procCloseDesktop.Call(d.desk)
		d.desk = 0
	}
}

func metric(i int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int(int32(r))
}

type bitmapInfoHeader struct {
	Size          uint32
	Width, Height int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPels, YPels  int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// screenBuffer is the frame the helper sends from: a DIB section the size of
// the virtual screen, which GDI can draw the pointer onto.
type screenBuffer struct {
	dib           uintptr
	mem           uintptr
	old           uintptr
	bits          []byte
	width, height int
}

// ensure makes the buffer w by h, keeping it if it already is.
func (b *screenBuffer) ensure(w, h int) error {
	if b.dib != 0 && w == b.width && h == b.height {
		return nil
	}
	b.release()
	screen, _, _ := procGetDC.Call(0)
	if screen == 0 {
		return errors.New("could not open the screen")
	}
	defer procReleaseDC.Call(0, screen)
	bi := bitmapInfoHeader{Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	dib, _, _ := procCreateDIBSection.Call(screen, uintptr(unsafe.Pointer(&bi)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 || bits == nil {
		return errors.New("could not make a screen buffer")
	}
	mem, _, _ := procCreateCompatibleDC.Call(screen)
	if mem == 0 {
		procDeleteObject.Call(dib)
		return errors.New("could not make a drawing surface")
	}
	b.old, _, _ = procSelectObject.Call(mem, dib)
	b.dib, b.mem, b.width, b.height = dib, mem, w, h
	b.bits = unsafe.Slice((*byte)(bits), w*h*4)
	return nil
}

func (b *screenBuffer) release() {
	if b.mem != 0 {
		procSelectObject.Call(b.mem, b.old)
		procDeleteDC.Call(b.mem)
		b.mem = 0
	}
	if b.dib != 0 {
		procDeleteObject.Call(b.dib)
		b.dib, b.bits = 0, nil
	}
}

// pointerSprite draws the pointer into the buffer and puts back what was
// under it before drawing it somewhere else, so the pixels around it stay the
// screen's. The recording therefore shows where the pointer was.
type pointerSprite struct {
	saved  []byte
	at     image.Rectangle // where the saved pixels came from; empty when none
	cursor windows.Handle
	pos    image.Point
}

// restore puts back the pixels under the last pointer and says where.
func (p *pointerSprite) restore(b *screenBuffer) image.Rectangle {
	r := p.at
	if r.Empty() {
		return r
	}
	restoreRect(b.bits, b.width*4, p.saved, r)
	p.at = image.Rectangle{}
	return r
}

// draw saves what is under the pointer and draws it, returning where.
func (p *pointerSprite) draw(b *screenBuffer, originX, originY int) image.Rectangle {
	ci := cursorInfo{}
	ci.Size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 ||
		ci.Flags&cursorShowing == 0 || ci.Cursor == 0 {
		p.cursor = 0
		return image.Rectangle{}
	}
	var ii iconInfo
	if r, _, _ := procGetIconInfo.Call(uintptr(ci.Cursor), uintptr(unsafe.Pointer(&ii))); r == 0 {
		return image.Rectangle{}
	}
	if ii.Mask != 0 {
		defer procDeleteObject.Call(uintptr(ii.Mask))
	}
	if ii.Colour != 0 {
		defer procDeleteObject.Call(uintptr(ii.Colour))
	}
	px := int(ci.X) - int(ii.XHot) - originX
	py := int(ci.Y) - int(ii.YHot) - originY
	p.cursor, p.pos = ci.Cursor, image.Pt(int(ci.X), int(ci.Y))
	// A pointer is at most 256 pixels square, and most are 32 or 48.
	r := image.Rect(px, py, px+pointerBox, py+pointerBox).Intersect(image.Rect(0, 0, b.width, b.height))
	if r.Empty() {
		return r
	}
	if cap(p.saved) < r.Dx()*r.Dy()*4 {
		p.saved = make([]byte, pointerBox*pointerBox*4)
	}
	p.saved = p.saved[:r.Dx()*r.Dy()*4]
	saveRect(p.saved, b.bits, b.width*4, r)
	p.at = r
	procDrawIconEx.Call(b.mem, uintptr(px), uintptr(py), uintptr(ci.Cursor), 0, 0, 0, 0, diNormal)
	procGdiFlush.Call()
	return r
}

// moved reports whether the pointer is not where, or not what, it was drawn.
func (p *pointerSprite) moved() bool {
	ci := cursorInfo{}
	ci.Size = uint32(unsafe.Sizeof(ci))
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return false
	}
	showing := ci.Flags&cursorShowing != 0 && ci.Cursor != 0
	if !showing {
		return p.cursor != 0
	}
	return ci.Cursor != p.cursor || image.Pt(int(ci.X), int(ci.Y)) != p.pos
}

// pointerBox bounds the pixels a pointer can cover.
const pointerBox = 64

// saveRect copies the rectangle r of a frame (stride bytes a row) into a
// packed buffer.
func saveRect(dst, frame []byte, stride int, r image.Rectangle) {
	w := r.Dx() * 4
	for y := 0; y < r.Dy(); y++ {
		o := (r.Min.Y+y)*stride + r.Min.X*4
		copy(dst[y*w:(y+1)*w], frame[o:o+w])
	}
}

// restoreRect puts a packed buffer back at the rectangle r of a frame.
func restoreRect(frame []byte, stride int, src []byte, r image.Rectangle) {
	w := r.Dx() * 4
	for y := 0; y < r.Dy(); y++ {
		o := (r.Min.Y+y)*stride + r.Min.X*4
		copy(frame[o:o+w], src[y*w:(y+1)*w])
	}
}

// consoleCapturer captures with Desktop Duplication when it can and GDI when
// it cannot: on a host with no display adapter of its own, on a desktop
// Duplication is refused (the sign-in and UAC desktops, for a moment), or
// across several monitors. It tries Duplication again a second after giving
// up on it.
type consoleCapturer struct {
	desk    inputDesktop
	buf     screenBuffer
	pointer pointerSprite
	dup     *duplicator
	gdiOnly bool
	retry   time.Time
	// fresh says the buffer was refilled whole and every tile must be looked at.
	fresh bool
}

func newConsoleCapturer(gdiOnly bool) *consoleCapturer {
	return &consoleCapturer{gdiOnly: gdiOnly, fresh: true}
}

func (c *consoleCapturer) Capture(wait time.Duration) (screenShot, bool, error) {
	c.desk.follow(false)
	if c.dup == nil && !c.gdiOnly && time.Now().After(c.retry) {
		d, err := newDuplicator()
		if err != nil {
			c.retry = time.Now().Add(time.Second)
		} else {
			c.dup, c.fresh = d, true
		}
	}
	if c.dup != nil {
		shot, changed, err := c.captureDup(wait)
		if err == nil {
			return shot, changed, nil
		}
		// Lost: a desktop switch, a mode change, a driver update. GDI for
		// now, and Duplication again in a second, on the new desktop.
		c.dup.close()
		c.dup, c.fresh = nil, true
		c.retry = time.Now().Add(time.Second)
		c.desk.follow(true)
	}
	return c.captureGDI()
}

// captureDup waits for Duplication's next frame and copies what changed.
func (c *consoleCapturer) captureDup(wait time.Duration) (screenShot, bool, error) {
	w, h := c.dup.width, c.dup.height
	if err := c.buf.ensure(w, h); err != nil {
		return screenShot{}, false, err
	}
	all := c.fresh
	// The pointer first: put back what it covered, so what Duplication
	// copies in next is not undone by it, then draw it where it is now.
	moved := c.pointer.moved()
	var dirty []image.Rectangle
	if r := c.pointer.restore(&c.buf); !r.Empty() {
		dirty = append(dirty, r)
	}
	changed, got, err := c.dup.next(wait, c.buf.bits, w*4, all)
	if err != nil {
		return screenShot{}, false, err
	}
	dirty = append(dirty, changed...)
	if r := c.pointer.draw(&c.buf, c.dup.originX, c.dup.originY); !r.Empty() {
		dirty = append(dirty, r)
	}
	if !got && (all || !moved) {
		// Nothing new; and a buffer still waiting for its first whole
		// frame has nothing to show yet.
		return screenShot{}, false, nil
	}
	shot := screenShot{fb: c.buf.bits, stride: w * 4, width: w, height: h, dirty: dirty}
	if all {
		shot.dirty, c.fresh = nil, false
	}
	return shot, true, nil
}

// captureGDI copies the whole virtual screen and draws the pointer on it.
func (c *consoleCapturer) captureGDI() (screenShot, bool, error) {
	x, y := metric(smXVirtualScreen), metric(smYVirtualScreen)
	w, h := metric(smCXVirtualScreen), metric(smCYVirtualScreen)
	w, h = min(w, rdp.MaxDimension), min(h, rdp.MaxDimension)
	if w <= 0 || h <= 0 {
		return screenShot{}, false, errors.New("the screen has no size")
	}
	if err := c.buf.ensure(w, h); err != nil {
		return screenShot{}, false, err
	}
	screen, _, _ := procGetDC.Call(0)
	if screen == 0 {
		return screenShot{}, false, errors.New("could not open the screen")
	}
	defer procReleaseDC.Call(0, screen)
	// No CAPTUREBLT: with the desktop composed (Windows 8 and later always
	// is) layered windows are in the screen's pixels already, and asking for
	// them again makes BitBlt several times slower and the pointer flicker.
	r, _, err := procBitBlt.Call(c.buf.mem, 0, 0, uintptr(w), uintptr(h), screen,
		uintptr(x), uintptr(y), srcCopy)
	if r == 0 {
		c.desk.follow(true)
		return screenShot{}, false, fmt.Errorf("capturing the screen: %v", err)
	}
	c.pointer.at = image.Rectangle{} // the copy replaced whatever was under it
	c.pointer.draw(&c.buf, x, y)
	c.fresh = true
	return screenShot{fb: c.buf.bits, stride: w * 4, width: w, height: h}, true, nil
}

func (c *consoleCapturer) Close() {
	if c.dup != nil {
		c.dup.close()
	}
	c.buf.release()
	c.desk.close()
}

type cursorInfo struct {
	Size   uint32
	Flags  uint32
	Cursor windows.Handle
	X, Y   int32
}

type iconInfo struct {
	Icon         int32
	XHot, YHot   uint32
	Mask, Colour windows.Handle
}

type mouseInputT struct {
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	Extra     uintptr
}

type keybdInputT struct {
	Vk, Scan uint16
	Flags    uint32
	Time     uint32
	Extra    uintptr
}

// sendInput is INPUT: a type and a union whose largest member is
// MOUSEINPUT, aligned as C aligns it on both 32- and 64-bit Windows.
type sendInput struct {
	Type uint32
	Mi   mouseInputT
}

func send(in *sendInput) error {
	r, _, err := procSendInput.Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
	if r != 1 {
		return err
	}
	return nil
}

// inputPlayer plays input with SendInput, on its own thread and desktop.
type inputPlayer struct {
	desk inputDesktop
}

func (p *inputPlayer) Input(e rdp.InputEvent) error {
	p.desk.follow(false)
	err := p.play(e)
	if err != nil {
		// Probably the desktop changed under it: once more, on the new one.
		p.desk.follow(true)
		err = p.play(e)
	}
	return err
}

func (p *inputPlayer) Close() { p.desk.close() }

func (p *inputPlayer) play(e rdp.InputEvent) error {
	switch e.Kind {
	case "key":
		in := sendInput{Type: inputKeyboard}
		k := (*keybdInputT)(unsafe.Pointer(&in.Mi))
		k.Scan = uint16(e.Scancode)
		k.Flags = keyScancode
		if e.Extended {
			k.Flags |= keyExtendedKey
		}
		if !e.Down {
			k.Flags |= keyUp
		}
		return send(&in)
	case "mouse", "button", "wheel":
		width, height := metric(smCXVirtualScreen), metric(smCYVirtualScreen)
		if width <= 1 || height <= 1 {
			return nil
		}
		in := sendInput{Type: inputMouse}
		x, y := min(max(e.X, 0), width-1), min(max(e.Y, 0), height-1)
		in.Mi.Dx = int32(x * 65535 / (width - 1))
		in.Mi.Dy = int32(y * 65535 / (height - 1))
		in.Mi.Flags = mouseMove | mouseAbsolute | mouseVirtualDsk
		switch e.Kind {
		case "button":
			var flags [2]uint32 // down, up
			switch e.Button {
			case 0:
				flags = [2]uint32{mouseLeftDown, mouseLeftUp}
			case 1:
				flags = [2]uint32{mouseMiddleDown, mouseMiddleUp}
			case 2:
				flags = [2]uint32{mouseRightDown, mouseRightUp}
			default:
				return nil
			}
			if e.Down {
				in.Mi.Flags |= flags[0]
			} else {
				in.Mi.Flags |= flags[1]
			}
		case "wheel":
			// One notch, as the Remote Desktop path sends, in its direction.
			in.Mi.Flags |= mouseWheel
			if e.WheelDelta < 0 {
				in.Mi.MouseData = uint32(0xFFFFFF88) // -120
			} else {
				in.Mi.MouseData = 120
			}
		}
		return send(&in)
	}
	return nil
}

package agent

import (
	"errors"
	"fmt"
	"image"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

/*
Desktop Duplication: the console's screen as Windows composes it, and which
parts of it changed.

GDI copies the whole screen to find out whether anything moved, every time it
looks. Duplication instead waits until Windows presents a new frame and says
which rectangles it drew and which it moved, so an idle screen costs nothing
and a keystroke costs the few tiles around the caret. The frame is copied
from the GPU into a staging texture and only the changed rectangles are read
out of it.

It is plain COM through vtables: D3D11 for the device and the copy, DXGI for
the duplication. The method numbers are the order of each interface's
vtable in the Windows SDK headers (dxgi.h, dxgi1_2.h, d3d11.h).

It is used only where it is simple and right: one monitor, not rotated. A
console with several monitors, or a refusal (no display adapter; the
sign-in or UAC desktop for a moment) leaves capture to GDI, and consoleCapturer
tries Duplication again a second later.
*/

var (
	modD3D11              = windows.NewLazySystemDLL("d3d11.dll")
	procD3D11CreateDevice = modD3D11.NewProc("D3D11CreateDevice")
)

var (
	iidIDXGIDevice     = windows.GUID{Data1: 0x54ec77fa, Data2: 0x1377, Data3: 0x44e6, Data4: [8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c}}
	iidIDXGIOutput1    = windows.GUID{Data1: 0x00cddea8, Data2: 0x939b, Data3: 0x4b83, Data4: [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidID3D11Texture2D = windows.GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

// Vtable slots.
const (
	vtQueryInterface = 0
	vtRelease        = 2

	vtDXGIDeviceGetAdapter = 7  // IDXGIDevice
	vtAdapterEnumOutputs   = 7  // IDXGIAdapter
	vtOutputGetDesc        = 7  // IDXGIOutput
	vtOutput1Duplicate     = 22 // IDXGIOutput1::DuplicateOutput

	vtDupGetDesc        = 7 // IDXGIOutputDuplication
	vtDupAcquire        = 8
	vtDupDirtyRects     = 9
	vtDupMoveRects      = 10
	vtDupReleaseFrame   = 14
	vtDeviceCreateTex2D = 5  // ID3D11Device
	vtContextMap        = 14 // ID3D11DeviceContext
	vtContextUnmap      = 15
	vtContextCopy       = 47 // CopyResource
)

const (
	d3dDriverTypeHardware = 1
	d3d11SDKVersion       = 7
	dxgiFormatB8G8R8A8    = 87
	d3d11UsageStaging     = 3
	d3d11CPUAccessRead    = 0x20000
	d3d11MapRead          = 1

	dxgiErrWaitTimeout = 0x887A0027
	dxgiErrMoreData    = 0x887A0003

	dxgiRotationIdentity = 1

	smCMonitors = 80
)

// comObject is a COM interface pointer: its first word is the vtable.
type comObject struct {
	vtbl *[128]uintptr
}

// call calls method slot of o with args, returning the HRESULT.
//
// Callers pass pointers to their own variables as uintptr, for the method
// to write through. uintptrescapes keeps those variables on the heap and
// alive for the call: without it they can sit on the goroutine's stack,
// which moves when it grows, and the method then writes into the old one,
// so what the caller reads back is whatever was there before.
//
//go:uintptrescapes
func (o *comObject) call(slot int, args ...uintptr) uint32 {
	all := append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[slot], all...)
	return uint32(r)
}

func (o *comObject) release() {
	if o != nil {
		o.call(vtRelease)
	}
}

func (o *comObject) query(iid *windows.GUID) (*comObject, error) {
	var out *comObject
	if hr := o.call(vtQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))); failed(hr) {
		return nil, hresult("QueryInterface", hr)
	}
	return out, nil
}

func failed(hr uint32) bool { return int32(hr) < 0 }

func hresult(what string, hr uint32) error {
	return fmt.Errorf("%s: HRESULT 0x%08X", what, hr)
}

type dxgiRect struct{ Left, Top, Right, Bottom int32 }

func (r dxgiRect) image() image.Rectangle {
	return image.Rect(int(r.Left), int(r.Top), int(r.Right), int(r.Bottom))
}

type dxgiOutputDesc struct {
	DeviceName [32]uint16
	Desktop    dxgiRect
	Attached   int32
	Rotation   uint32
	Monitor    uintptr
}

type dxgiOutduplDesc struct {
	Width, Height      uint32
	RefreshNum         uint32
	RefreshDen         uint32
	Format             uint32
	ScanlineOrdering   uint32
	Scaling            uint32
	Rotation           uint32
	DesktopInSysMemory int32
}

type dxgiFrameInfo struct {
	LastPresentTime     int64
	LastMouseUpdateTime int64
	AccumulatedFrames   uint32
	RectsCoalesced      int32
	ProtectedMaskedOut  int32
	PointerX, PointerY  int32
	PointerVisible      int32
	TotalMetadataSize   uint32
	PointerShapeSize    uint32
}

type dxgiMoveRect struct {
	SrcX, SrcY int32
	Dest       dxgiRect
}

type d3dTexture2DDesc struct {
	Width, Height  uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleCount    uint32
	SampleQuality  uint32
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

type d3dMapped struct {
	Data       *byte
	RowPitch   uint32
	DepthPitch uint32
}

// duplicator is one output's Desktop Duplication.
type duplicator struct {
	device, context, dup, staging *comObject
	width, height                 int
	// originX, originY is the output's top-left in desktop coordinates,
	// where the pointer's position is given.
	originX, originY int
	meta             []byte
}

// newDuplicator duplicates the console's only monitor.
func newDuplicator() (d *duplicator, err error) {
	if metric(smCMonitors) != 1 {
		return nil, errors.New("Desktop Duplication is used for one monitor only")
	}
	if err := procD3D11CreateDevice.Find(); err != nil {
		return nil, err
	}
	d = &duplicator{}
	defer func() {
		if err != nil {
			d.close()
		}
	}()
	var level uint32
	r, _, _ := procD3D11CreateDevice.Call(0, d3dDriverTypeHardware, 0, 0, 0, 0, d3d11SDKVersion,
		uintptr(unsafe.Pointer(&d.device)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&d.context)))
	if failed(uint32(r)) || d.device == nil || d.context == nil {
		return nil, hresult("D3D11CreateDevice", uint32(r))
	}
	dxgiDevice, err := d.device.query(&iidIDXGIDevice)
	if err != nil {
		return nil, err
	}
	defer dxgiDevice.release()
	var adapter *comObject
	if hr := dxgiDevice.call(vtDXGIDeviceGetAdapter, uintptr(unsafe.Pointer(&adapter))); failed(hr) {
		return nil, hresult("GetAdapter", hr)
	}
	defer adapter.release()
	var output *comObject
	if hr := adapter.call(vtAdapterEnumOutputs, 0, uintptr(unsafe.Pointer(&output))); failed(hr) {
		return nil, hresult("EnumOutputs", hr)
	}
	defer output.release()
	var od dxgiOutputDesc
	if hr := output.call(vtOutputGetDesc, uintptr(unsafe.Pointer(&od))); failed(hr) {
		return nil, hresult("GetDesc", hr)
	}
	if od.Attached == 0 || (od.Rotation != dxgiRotationIdentity && od.Rotation != 0) {
		return nil, errors.New("the monitor is not attached, or is rotated")
	}
	output1, err := output.query(&iidIDXGIOutput1)
	if err != nil {
		return nil, err
	}
	defer output1.release()
	if hr := output1.call(vtOutput1Duplicate, uintptr(unsafe.Pointer(d.device)),
		uintptr(unsafe.Pointer(&d.dup))); failed(hr) || d.dup == nil {
		return nil, hresult("DuplicateOutput", hr)
	}
	var dd dxgiOutduplDesc
	d.dup.call(vtDupGetDesc, uintptr(unsafe.Pointer(&dd)))
	d.width, d.height = int(dd.Width), int(dd.Height)
	d.originX, d.originY = int(od.Desktop.Left), int(od.Desktop.Top)
	if d.width <= 0 || d.height <= 0 || d.width > 16384 || d.height > 16384 ||
		d.width != od.Desktop.image().Dx() || d.height != od.Desktop.image().Dy() {
		return nil, fmt.Errorf("the duplicated screen is %dx%d", d.width, d.height)
	}
	td := d3dTexture2DDesc{Width: dd.Width, Height: dd.Height, MipLevels: 1, ArraySize: 1,
		Format: dxgiFormatB8G8R8A8, SampleCount: 1, Usage: d3d11UsageStaging,
		CPUAccessFlags: d3d11CPUAccessRead}
	if hr := d.device.call(vtDeviceCreateTex2D, uintptr(unsafe.Pointer(&td)), 0,
		uintptr(unsafe.Pointer(&d.staging))); failed(hr) || d.staging == nil {
		return nil, hresult("CreateTexture2D", hr)
	}
	return d, nil
}

// next waits up to wait for a new frame and copies what changed into dst,
// a top-down BGRX frame stride bytes a row, the duplicated screen's size.
// all copies the whole screen. got is false when no new frame came; dirty
// is where dst changed.
func (d *duplicator) next(wait time.Duration, dst []byte, stride int, all bool) (
	dirty []image.Rectangle, got bool, err error) {

	var info dxgiFrameInfo
	var res *comObject
	hr := d.dup.call(vtDupAcquire, uintptr(max(wait.Milliseconds(), 0)),
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&res)))
	if hr == dxgiErrWaitTimeout {
		return nil, false, nil
	}
	if failed(hr) {
		return nil, false, hresult("AcquireNextFrame", hr)
	}
	released := false
	releaseFrame := func() {
		if !released {
			res.release()
			d.dup.call(vtDupReleaseFrame)
			released = true
		}
	}
	defer releaseFrame()
	if info.LastPresentTime == 0 || info.AccumulatedFrames == 0 {
		// Only the pointer moved; it is drawn from where it is.
		return nil, false, nil
	}
	if !all {
		dirty, err = d.changedRects()
		if err != nil {
			// Not knowing what changed, copy it all.
			all, dirty = true, nil
		}
	}
	tex, err := res.query(&iidID3D11Texture2D)
	if err != nil {
		return nil, false, err
	}
	d.context.call(vtContextCopy, uintptr(unsafe.Pointer(d.staging)), uintptr(unsafe.Pointer(tex)))
	tex.release()

	// The frame is held until the copy is done, which Map waits for. The
	// copy is only queued above: released before it ran, the frame's
	// surface could take the next frame first, and the staging copy would
	// hold part of a later screen while the dirty rectangles describe this
	// one, leaving pieces of other frames on screen.
	var m d3dMapped
	if hr := d.context.call(vtContextMap, uintptr(unsafe.Pointer(d.staging)), 0, d3d11MapRead, 0,
		uintptr(unsafe.Pointer(&m))); failed(hr) || m.Data == nil {
		return nil, false, hresult("Map", hr)
	}
	releaseFrame()
	defer d.context.call(vtContextUnmap, uintptr(unsafe.Pointer(d.staging)), 0)
	pitch := int(m.RowPitch)
	if pitch < d.width*4 {
		return nil, false, errors.New("the duplicated screen's rows are too short")
	}
	src := unsafe.Slice(m.Data, pitch*d.height)
	bounds := image.Rect(0, 0, d.width, d.height)
	copyIn := func(r image.Rectangle) {
		r = r.Intersect(bounds)
		w := r.Dx() * 4
		for y := r.Min.Y; y < r.Max.Y; y++ {
			copy(dst[y*stride+r.Min.X*4:y*stride+r.Min.X*4+w], src[y*pitch+r.Min.X*4:y*pitch+r.Min.X*4+w])
		}
	}
	if all {
		copyIn(bounds)
		return nil, true, nil
	}
	for _, r := range dirty {
		copyIn(r)
	}
	return dirty, true, nil
}

// changedRects is the frame's dirty rectangles and the destinations of its
// moves, which changed as much as anything drawn.
func (d *duplicator) changedRects() ([]image.Rectangle, error) {
	var out []image.Rectangle
	read := func(slot int) ([]byte, error) {
		if len(d.meta) == 0 {
			d.meta = make([]byte, 64<<10)
		}
		for {
			var need uint32
			hr := d.dup.call(slot, uintptr(len(d.meta)), uintptr(unsafe.Pointer(&d.meta[0])),
				uintptr(unsafe.Pointer(&need)))
			if hr == dxgiErrMoreData && int(need) > len(d.meta) && need < 64<<20 {
				d.meta = make([]byte, need)
				continue
			}
			if failed(hr) {
				return nil, hresult("frame metadata", hr)
			}
			return d.meta[:need], nil
		}
	}
	moves, err := read(vtDupMoveRects)
	if err != nil {
		return nil, err
	}
	mr := unsafe.Sizeof(dxgiMoveRect{})
	for i := 0; i+int(mr) <= len(moves); i += int(mr) {
		m := (*dxgiMoveRect)(unsafe.Pointer(&moves[i]))
		out = append(out, m.Dest.image())
	}
	rects, err := read(vtDupDirtyRects)
	if err != nil {
		return nil, err
	}
	rs := unsafe.Sizeof(dxgiRect{})
	for i := 0; i+int(rs) <= len(rects); i += int(rs) {
		out = append(out, (*dxgiRect)(unsafe.Pointer(&rects[i])).image())
	}
	return out, nil
}

func (d *duplicator) close() {
	d.staging.release()
	d.dup.release()
	d.context.release()
	d.device.release()
	d.staging, d.dup, d.context, d.device = nil, nil, nil, nil
}

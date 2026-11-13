//go:build darwin

package agent

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

/*
Motion as H.264 on a Mac, with VideoToolbox: the Mac's own hardware encoder,
through purego as CoreGraphics is.

A session is made at the screen's size, real time, no reordering, a key
frame only when asked for, at the bitrate the Windows encoder uses for the
same screen. Each frame is copied into a buffer from the session's own pool
(the encoder converts BGRA to YUV in hardware), encoded, and waited for:
VideoToolbox hands the picture to a callback as length-prefixed NAL units,
which become the Annex B the rest of the path reads, the SPS and PPS put in
front of each key frame.

Anything that fails says why, and the helper sends motion as exact tiles
from then on (motion.disable); one that crashes is restarted by the agent
without video.
*/

// macVideoBits is motion's mean cost, in bits a second for each pixel of
// the screen: the Windows encoder's, capped the same.
const (
	macVideoBits    = 4
	macVideoMaxBits = 24_000_000
)

type videoToolbox struct {
	sessionCreate  func(alloc uintptr, w, h int32, codec uint32, spec, srcAttrs, compAlloc, cb, refcon uintptr, out *uintptr) int32
	setProperty    func(s, key, value uintptr) int32
	prepare        func(s uintptr) int32
	pool           func(s uintptr) uintptr
	encodeFrame    func(s, pb uintptr, pts, dur cmTime, props, refcon uintptr, flags *uint32) int32
	completeFrames func(s uintptr, until cmTime) int32
	invalidate     func(s uintptr)
	poolCreate     func(alloc, pool uintptr, out *uintptr) int32
	pbLock         func(pb uintptr, flags uint64) int32
	pbUnlock       func(pb uintptr, flags uint64) int32
	pbBase         func(pb uintptr) uintptr
	pbBytesPerRow  func(pb uintptr) uintptr
	sbDataBuffer   func(sb uintptr) uintptr
	sbFormat       func(sb uintptr) uintptr
	bbLength       func(bb uintptr) uintptr
	bbCopy         func(bb, off, n uintptr, dst unsafe.Pointer) int32
	h264ParamSet   func(fd, idx uintptr, ptr, size, count *uintptr, hdrLen *int32) int32

	keyRealTime, keyReorder, keyProfile, keyMaxKeyInterval, keyBitRate, keyFrameRate uintptr
	profileHigh, profileMain, forceKeyFrame                                          uintptr
	cfFalse                                                                          uintptr
	pbFormatKey, pbWidthKey, pbHeightKey                                             uintptr
	callback                                                                         uintptr
}

// cmTime is a CMTime.
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

var (
	vtOnce   sync.Once
	vtLoaded *videoToolbox
	vtErr    error
	// vtEncoders finds an encoder from the callback's reference.
	vtEncoders sync.Map
	vtNext     atomic.Uintptr
)

func loadVideoToolbox() (*videoToolbox, error) {
	vtOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				vtErr = fmt.Errorf("loading VideoToolbox: %v", r)
			}
		}()
		vtLoaded, vtErr = openVideoToolbox()
	})
	return vtLoaded, vtErr
}

func openVideoToolbox() (*videoToolbox, error) {
	var missing []string
	open := func(path string) uintptr {
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			missing = append(missing, path)
		}
		return h
	}
	vtH := open("/System/Library/Frameworks/VideoToolbox.framework/VideoToolbox")
	cmH := open("/System/Library/Frameworks/CoreMedia.framework/CoreMedia")
	cvH := open("/System/Library/Frameworks/CoreVideo.framework/CoreVideo")
	cfH := open(cfPath)
	if len(missing) > 0 {
		return nil, fmt.Errorf("this macOS lacks %v", missing)
	}
	reg := func(fptr any, h uintptr, name string) {
		if _, err := purego.Dlsym(h, name); err != nil {
			missing = append(missing, name)
			return
		}
		purego.RegisterLibFunc(fptr, h, name)
	}
	data := func(h uintptr, name string) uintptr {
		p, err := purego.Dlsym(h, name)
		if err != nil {
			missing = append(missing, name)
			return 0
		}
		return *(*uintptr)(cptr(p))
	}
	v := &videoToolbox{}
	reg(&v.sessionCreate, vtH, "VTCompressionSessionCreate")
	reg(&v.setProperty, vtH, "VTSessionSetProperty")
	reg(&v.prepare, vtH, "VTCompressionSessionPrepareToEncodeFrames")
	reg(&v.pool, vtH, "VTCompressionSessionGetPixelBufferPool")
	reg(&v.encodeFrame, vtH, "VTCompressionSessionEncodeFrame")
	reg(&v.completeFrames, vtH, "VTCompressionSessionCompleteFrames")
	reg(&v.invalidate, vtH, "VTCompressionSessionInvalidate")
	reg(&v.poolCreate, cvH, "CVPixelBufferPoolCreatePixelBuffer")
	reg(&v.pbLock, cvH, "CVPixelBufferLockBaseAddress")
	reg(&v.pbUnlock, cvH, "CVPixelBufferUnlockBaseAddress")
	reg(&v.pbBase, cvH, "CVPixelBufferGetBaseAddress")
	reg(&v.pbBytesPerRow, cvH, "CVPixelBufferGetBytesPerRow")
	reg(&v.sbDataBuffer, cmH, "CMSampleBufferGetDataBuffer")
	reg(&v.sbFormat, cmH, "CMSampleBufferGetFormatDescription")
	reg(&v.bbLength, cmH, "CMBlockBufferGetDataLength")
	reg(&v.bbCopy, cmH, "CMBlockBufferCopyDataBytes")
	reg(&v.h264ParamSet, cmH, "CMVideoFormatDescriptionGetH264ParameterSetAtIndex")
	v.keyRealTime = data(vtH, "kVTCompressionPropertyKey_RealTime")
	v.keyReorder = data(vtH, "kVTCompressionPropertyKey_AllowFrameReordering")
	v.keyProfile = data(vtH, "kVTCompressionPropertyKey_ProfileLevel")
	v.keyMaxKeyInterval = data(vtH, "kVTCompressionPropertyKey_MaxKeyFrameInterval")
	v.keyBitRate = data(vtH, "kVTCompressionPropertyKey_AverageBitRate")
	v.keyFrameRate = data(vtH, "kVTCompressionPropertyKey_ExpectedFrameRate")
	v.profileHigh = data(vtH, "kVTProfileLevel_H264_High_AutoLevel")
	v.profileMain = data(vtH, "kVTProfileLevel_H264_Main_AutoLevel")
	v.forceKeyFrame = data(vtH, "kVTEncodeFrameOptionKey_ForceKeyFrame")
	v.cfFalse = data(cfH, "kCFBooleanFalse")
	v.pbFormatKey = data(cvH, "kCVPixelBufferPixelFormatTypeKey")
	v.pbWidthKey = data(cvH, "kCVPixelBufferWidthKey")
	v.pbHeightKey = data(cvH, "kCVPixelBufferHeightKey")
	if len(missing) > 0 {
		return nil, fmt.Errorf("this macOS lacks what H.264 encoding uses: %v", missing)
	}
	// One callback for every session: purego can make only so many.
	v.callback = purego.NewCallback(func(refcon, _ uintptr, status uintptr, _ uintptr, sb uintptr) {
		if e, ok := vtEncoders.Load(refcon); ok {
			e.(*macEncoder).output(int32(status), sb)
		}
	})
	return v, nil
}

// macEncoder is one VideoToolbox session.
type macEncoder struct {
	vt            *videoToolbox
	cgf           *coreGraphics
	session       uintptr
	id            uintptr
	width, height int
	keyProps      uintptr
	frame         int64

	// Set by output, on VideoToolbox's thread, read after CompleteFrames.
	mu     sync.Mutex
	out    []byte
	outErr error
}

func newMacVideo(width, height int) (screenVideo, error) {
	cg, err := loadCoreGraphics()
	if err != nil {
		return nil, err
	}
	vt, err := loadVideoToolbox()
	if err != nil {
		return nil, err
	}
	e := &macEncoder{vt: vt, cgf: cg, width: width, height: height, id: vtNext.Add(1)}
	vtEncoders.Store(e.id, e)

	// The source: BGRA buffers at the screen's size, in IOSurfaces.
	num := func(v int32) uintptr { return cg.cfNumberCreate(0, 3 /* kCFNumberSInt32Type */, unsafe.Pointer(&v)) }
	keys := []uintptr{vt.pbFormatKey, vt.pbWidthKey, vt.pbHeightKey}
	values := []uintptr{num(cgFormatBGRA), num(int32(width)), num(int32(height))}
	attrs := cg.cfDictionaryCreate(0, &keys[0], &values[0], len(keys), cg.dictKeyCallBacks, cg.dictValueCBs)
	for _, v := range values {
		cg.cfRelease(v)
	}
	var s uintptr
	rc := vt.sessionCreate(0, int32(width), int32(height), 0x61766331 /* 'avc1' */, 0, attrs, 0,
		vt.callback, e.id, &s)
	cg.cfRelease(attrs)
	if rc != 0 || s == 0 {
		vtEncoders.Delete(e.id)
		return nil, fmt.Errorf("VideoToolbox would not make an H.264 encoder (error %d)", rc)
	}
	e.session = s

	set := func(key, value uintptr) int32 { return vt.setProperty(s, key, value) }
	set(vt.keyRealTime, cg.cfBooleanTrue)
	set(vt.keyReorder, vt.cfFalse)
	if set(vt.keyProfile, vt.profileHigh) != 0 {
		set(vt.keyProfile, vt.profileMain)
	}
	// No key frame but those asked for: a key frame is the most
	// expensive picture there is, and the stream is reliable.
	interval := num(1 << 30)
	set(vt.keyMaxKeyInterval, interval)
	cg.cfRelease(interval)
	bits := num(int32(min(max(int64(width)*int64(height)*macVideoBits, 1_000_000), macVideoMaxBits)))
	set(vt.keyBitRate, bits)
	cg.cfRelease(bits)
	fps := num(60)
	set(vt.keyFrameRate, fps)
	cg.cfRelease(fps)
	if rc := vt.prepare(s); rc != 0 {
		e.Close()
		return nil, fmt.Errorf("VideoToolbox would not prepare the encoder (error %d)", rc)
	}
	k, v := vt.forceKeyFrame, cg.cfBooleanTrue
	e.keyProps = cg.cfDictionaryCreate(0, &k, &v, 1, cg.dictKeyCallBacks, cg.dictValueCBs)
	return e, nil
}

func (e *macEncoder) Encode(s screenShot, key bool) ([]byte, error) {
	if s.width != e.width || s.height != e.height {
		return nil, errors.New("the screen changed size under the encoder")
	}
	vt := e.vt
	pool := vt.pool(e.session)
	if pool == 0 {
		return nil, errors.New("the encoder has no buffers")
	}
	var pb uintptr
	if rc := vt.poolCreate(0, pool, &pb); rc != 0 || pb == 0 {
		return nil, fmt.Errorf("no buffer for the frame (error %d)", rc)
	}
	defer e.cgf.cfRelease(pb)
	if rc := vt.pbLock(pb, 0); rc != 0 {
		return nil, fmt.Errorf("could not fill the frame's buffer (error %d)", rc)
	}
	base, pitch := vt.pbBase(pb), int(vt.pbBytesPerRow(pb))
	if base == 0 || pitch < s.width*4 {
		vt.pbUnlock(pb, 0)
		return nil, errors.New("the frame's buffer has no pixels")
	}
	dst := unsafe.Slice((*byte)(cptr(base)), pitch*s.height)
	row := s.width * 4
	for y := 0; y < s.height; y++ {
		copy(dst[y*pitch:y*pitch+row], s.fb[y*s.stride:y*s.stride+row])
	}
	vt.pbUnlock(pb, 0)

	e.mu.Lock()
	e.out, e.outErr = e.out[:0], nil
	e.mu.Unlock()
	props := uintptr(0)
	if key {
		props = e.keyProps
	}
	e.frame++
	pts := cmTime{Value: e.frame, Timescale: 60, Flags: 1}
	invalid := cmTime{}
	if rc := vt.encodeFrame(e.session, pb, pts, invalid, props, 0, nil); rc != 0 {
		return nil, fmt.Errorf("the encoder refused the frame (error %d)", rc)
	}
	if rc := vt.completeFrames(e.session, invalid); rc != 0 {
		return nil, fmt.Errorf("the encoder did not finish the frame (error %d)", rc)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.outErr != nil {
		return nil, e.outErr
	}
	return append([]byte(nil), e.out...), nil
}

// output is VideoToolbox's callback: the picture, as Annex B.
func (e *macEncoder) output(status int32, sb uintptr) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if status != 0 {
		e.outErr = fmt.Errorf("the encoder failed a frame (error %d)", status)
		return
	}
	if sb == 0 {
		return // dropped
	}
	vt := e.vt
	bb := vt.sbDataBuffer(sb)
	if bb == 0 {
		return
	}
	n := vt.bbLength(bb)
	if n == 0 || n > 64<<20 {
		return
	}
	avcc := make([]byte, n)
	if rc := vt.bbCopy(bb, 0, n, unsafe.Pointer(&avcc[0])); rc != 0 {
		e.outErr = fmt.Errorf("could not read the encoded frame (error %d)", rc)
		return
	}
	var header []byte
	hdrLen := int32(4)
	if fd := vt.sbFormat(sb); fd != 0 {
		var count uintptr
		for i := uintptr(0); ; i++ {
			var p, size uintptr
			if vt.h264ParamSet(fd, i, &p, &size, &count, &hdrLen) != 0 || p == 0 {
				break
			}
			if avccKey(avcc, int(hdrLen)) {
				header = annexBNAL(header, unsafe.Slice((*byte)(cptr(p)), size))
			}
			if i+1 >= count {
				break
			}
		}
	}
	out, err := avccToAnnexB(e.out[:0], header, avcc, int(hdrLen))
	if err != nil {
		e.outErr = err
		return
	}
	e.out = out
}

// avccKey says whether length-prefixed NAL units hold an IDR slice.
func avccKey(b []byte, size int) bool {
	for len(b) > size {
		n := 0
		for _, c := range b[:size] {
			n = n<<8 | int(c)
		}
		b = b[size:]
		if n == 0 || n > len(b) {
			return false
		}
		if b[0]&0x1F == 5 {
			return true
		}
		b = b[n:]
	}
	return false
}

func (e *macEncoder) Close() {
	if e.session != 0 {
		e.vt.invalidate(e.session)
		e.cgf.cfRelease(e.session)
		e.session = 0
	}
	if e.keyProps != 0 {
		e.cgf.cfRelease(e.keyProps)
		e.keyProps = 0
	}
	vtEncoders.Delete(e.id)
}

package agent

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

/*
H.264 for motion: Microsoft's own encoder, through Media Foundation.

The encoder is the H.264 MFT every Windows desktop has (and Windows Server
has with its Media Foundation feature), used synchronously: one picture in,
one picture out, with low-latency mode on so it holds nothing back and no
B-frames to wait for. Pictures go in as NV12, converted from the captured
screen on a few cores (bgrxToNV12).

It is plain COM through vtables, as Desktop Duplication is. The method
numbers are each interface's vtable order in the Windows SDK headers
(mftransform.h, mfobjects.h, strmif.h). Anything missing (Media Foundation
not installed, the encoder refused) is an error from newMFEncoder, and the
helper then sends motion as exact tiles, as it did before there was video.
*/

var (
	modMFPlat                = windows.NewLazySystemDLL("mfplat.dll")
	procMFStartup            = modMFPlat.NewProc("MFStartup")
	procMFShutdown           = modMFPlat.NewProc("MFShutdown")
	procMFCreateMediaType    = modMFPlat.NewProc("MFCreateMediaType")
	procMFCreateSample       = modMFPlat.NewProc("MFCreateSample")
	procMFCreateMemoryBuffer = modMFPlat.NewProc("MFCreateMemoryBuffer")

	modOle32             = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = modOle32.NewProc("CoCreateInstance")
)

var (
	clsidMSH264Encoder = windows.GUID{Data1: 0x6ca50344, Data2: 0x051a, Data3: 0x4ded, Data4: [8]byte{0x97, 0x79, 0xa4, 0x33, 0x05, 0x16, 0x5e, 0x35}}
	iidIMFTransform    = windows.GUID{Data1: 0xbf94c121, Data2: 0x5b05, Data3: 0x4e6f, Data4: [8]byte{0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d}}
	iidICodecAPI       = windows.GUID{Data1: 0x901db4c7, Data2: 0x31ce, Data3: 0x41a2, Data4: [8]byte{0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda}}

	mfMediaTypeVideo = windows.GUID{Data1: 0x73646976, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	// Video subtypes are a FOURCC in the media type GUID's pattern.
	mfVideoFormatH264 = fourCCGUID('H', '2', '6', '4')
	mfVideoFormatNV12 = fourCCGUID('N', 'V', '1', '2')

	mfMTMajorType      = windows.GUID{Data1: 0x48eba18e, Data2: 0xf8c9, Data3: 0x4687, Data4: [8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f}}
	mfMTSubtype        = windows.GUID{Data1: 0xf7e34c9a, Data2: 0x42e8, Data3: 0x4714, Data4: [8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5}}
	mfMTFrameSize      = windows.GUID{Data1: 0x1652c33d, Data2: 0xd6b2, Data3: 0x4012, Data4: [8]byte{0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d}}
	mfMTFrameRate      = windows.GUID{Data1: 0xc459a2e8, Data2: 0x3d2c, Data3: 0x4e44, Data4: [8]byte{0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0}}
	mfMTPixelAspect    = windows.GUID{Data1: 0xc6376a1e, Data2: 0x8d0a, Data3: 0x4027, Data4: [8]byte{0xbe, 0x45, 0x6d, 0x9a, 0x0a, 0xd3, 0x9b, 0xb6}}
	mfMTInterlaceMode  = windows.GUID{Data1: 0xe2724bb8, Data2: 0xe676, Data3: 0x4806, Data4: [8]byte{0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd}}
	mfMTAvgBitrate     = windows.GUID{Data1: 0x20332624, Data2: 0xfb0d, Data3: 0x4d9e, Data4: [8]byte{0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e}}
	mfMTMpeg2Profile   = windows.GUID{Data1: 0xad76a80b, Data2: 0x2d5c, Data3: 0x4e0b, Data4: [8]byte{0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36}}
	mfMTSequenceHeader = windows.GUID{Data1: 0x3c036de7, Data2: 0x3ad0, Data3: 0x4c9e, Data4: [8]byte{0x92, 0x16, 0xee, 0x6d, 0x6a, 0xc2, 0x1c, 0xb3}}
	mfMTYUVMatrix      = windows.GUID{Data1: 0x3e23d450, Data2: 0x2c75, Data3: 0x4d25, Data4: [8]byte{0xa0, 0x0e, 0xb9, 0x16, 0x70, 0xd1, 0x23, 0x27}}
	mfMTNominalRange   = windows.GUID{Data1: 0xc21b8ee5, Data2: 0xb956, Data3: 0x4071, Data4: [8]byte{0x8d, 0xaf, 0x32, 0x5e, 0xdf, 0x5c, 0xab, 0x11}}
	mfMTPrimaries      = windows.GUID{Data1: 0xdbfbe4d7, Data2: 0x0740, Data3: 0x4ee0, Data4: [8]byte{0x81, 0x92, 0x85, 0x0a, 0xb0, 0xe2, 0x19, 0x35}}
	mfMTTransfer       = windows.GUID{Data1: 0x5fb0fce9, Data2: 0xbe5c, Data3: 0x4935, Data4: [8]byte{0xa8, 0x11, 0xec, 0x83, 0x8f, 0x8e, 0xed, 0x93}}

	codecAPIRateControlMode = windows.GUID{Data1: 0x1c0608e9, Data2: 0x370c, Data3: 0x4710, Data4: [8]byte{0x8a, 0x58, 0xcb, 0x61, 0x81, 0xc4, 0x24, 0x23}}
	codecAPILowLatency      = windows.GUID{Data1: 0x9c27891a, Data2: 0xed7a, Data3: 0x40e1, Data4: [8]byte{0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee}}
	codecAPIGOPSize         = windows.GUID{Data1: 0x95f31b26, Data2: 0x95a4, Data3: 0x41aa, Data4: [8]byte{0x93, 0x03, 0x24, 0x6a, 0x7f, 0xc6, 0xee, 0xf1}}
	codecAPIBPictures       = windows.GUID{Data1: 0x8d390aac, Data2: 0xdc5c, Data3: 0x4200, Data4: [8]byte{0xb5, 0x7f, 0x81, 0x4d, 0x04, 0xba, 0xba, 0xb2}}
	codecAPIMeanBitRate     = windows.GUID{Data1: 0xf7222374, Data2: 0x2144, Data3: 0x4815, Data4: [8]byte{0xb5, 0x50, 0xa3, 0x7f, 0x8e, 0x12, 0xee, 0x52}}
	codecAPIForceKeyFrame   = windows.GUID{Data1: 0x398c1b98, Data2: 0x8353, Data3: 0x475a, Data4: [8]byte{0x9e, 0xf2, 0x8f, 0x26, 0x5d, 0x26, 0x03, 0x45}}
)

// fourCCGUID is MFVideoFormat_<fourcc>: the FOURCC, then the media
// subtype pattern.
func fourCCGUID(a, b, c, d byte) windows.GUID {
	return windows.GUID{Data1: uint32(a) | uint32(b)<<8 | uint32(c)<<16 | uint32(d)<<24,
		Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
}

// Vtable slots.
const (
	// IMFTransform
	vtMFTGetOutputStreamInfo = 7
	vtMFTSetInputType        = 15
	vtMFTSetOutputType       = 16
	vtMFTGetOutputCurrent    = 18
	vtMFTProcessMessage      = 23
	vtMFTProcessInput        = 24
	vtMFTProcessOutput       = 25

	// IMFAttributes, and so IMFMediaType and IMFSample
	vtAttrGetBlobSize = 14
	vtAttrGetBlob     = 15
	vtAttrSetUINT32   = 21
	vtAttrSetUINT64   = 22
	vtAttrSetGUID     = 24

	// IMFSample
	vtSampleSetTime      = 36
	vtSampleSetDuration  = 38
	vtSampleToContiguous = 41
	vtSampleAddBuffer    = 42

	// IMFMediaBuffer
	vtBufferLock      = 3
	vtBufferUnlock    = 4
	vtBufferGetLength = 5
	vtBufferSetLength = 6

	// ICodecAPI
	vtCodecSetValue = 9
)

const (
	mfVersion       = 0x00020070
	mfStartupLite   = 1
	clsctxInproc    = 1
	coinitMulti     = 0
	rpcEChangedMode = 0x80010106

	mftMessageBeginStreaming = 0x10000000
	mftMessageStartOfStream  = 0x10000003
	mftOutputProvidesSamples = 0x100

	mfENeedMoreInput = 0xC00D6D72
	mfENotAccepting  = 0xC00D36B5

	vtUI4  = 19
	vtBool = 11

	rateControlCBR  = 0
	h264ProfileBase = 66

	// screenVideoRate is the frame rate the encoder plans its bits for; the
	// timestamps say when pictures really were.
	screenVideoRate = 60

	// screenVideoBits is what motion costs at most, in bits a second for
	// each pixel of the screen: 0.8 Mbit/s at 1024x768, 2 Mbit/s at full
	// HD. Motion is a little soft for it; it is sent again exactly a
	// quarter of a second after it stops, and the recording is kept small:
	// H.264 is already compressed, so the recording's own compression takes
	// nothing more off it.
	screenVideoBits = 1
)

// variant is a VARIANT: its type, three reserved words, and the value.
type variant struct {
	vt       uint16
	reserved [3]uint16
	val      uint64
	pad      uint64
}

type mftOutputStreamInfo struct {
	flags, size, alignment uint32
}

type mftOutputDataBuffer struct {
	streamID uint32
	sample   *comObject
	status   uint32
	events   *comObject
}

// mfEncoder is Microsoft's H.264 encoder for one screen size.
type mfEncoder struct {
	mft    *comObject
	codec  *comObject // ICodecAPI; nil if the encoder has none
	width  int
	height int
	start  time.Time
	out    mftOutputStreamInfo
	header []byte // SPS and PPS, for key frames that come without them
	com    bool   // CoInitializeEx needs undoing
	mf     bool   // MFStartup needs undoing
}

// newMFEncoder opens the encoder for a screen of width by height.
func newMFEncoder(width, height int) (screenVideo, error) {
	e := &mfEncoder{width: width, height: height, start: time.Now()}
	if err := e.open(); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}

func (e *mfEncoder) open() error {
	if err := modMFPlat.Load(); err != nil {
		return errors.New("Media Foundation is not installed on this host " +
			"(on Windows Server, add the Media Foundation feature)")
	}
	switch err := windows.CoInitializeEx(0, coinitMulti); {
	case err == nil:
		e.com = true
	case err == windows.Errno(1): // S_FALSE: already initialised on this thread
		e.com = true
	case err == windows.Errno(rpcEChangedMode):
		// Initialised otherwise; usable as it is.
	default:
		return fmt.Errorf("could not start COM: %v", err)
	}
	if hr, _, _ := procMFStartup.Call(mfVersion, mfStartupLite); failed(uint32(hr)) {
		return hresult("MFStartup", uint32(hr))
	}
	e.mf = true
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMSH264Encoder)), 0, clsctxInproc,
		uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&e.mft))); failed(uint32(hr)) {
		e.mft = nil
		return fmt.Errorf("the H.264 encoder is not available on this host "+
			"(on Windows Server, add the Media Foundation feature): HRESULT 0x%08X", uint32(hr))
	}

	// Settings first: some encoders take them only before the types.
	if c, err := e.mft.query(&iidICodecAPI); err == nil {
		e.codec = c
		e.setValue(&codecAPILowLatency, vtBool, 0xFFFF) // VARIANT_TRUE
		e.setValue(&codecAPIRateControlMode, vtUI4, rateControlCBR)
		e.setValue(&codecAPIMeanBitRate, vtUI4, uint64(e.bitrate()))
		e.setValue(&codecAPIBPictures, vtUI4, 0)
		// No key frame but those asked for: the stream is reliable, and a
		// key frame is the most expensive picture there is.
		e.setValue(&codecAPIGOPSize, vtUI4, 0xFFFF)
	}

	size := uint64(e.width)<<32 | uint64(e.height)
	rate := uint64(screenVideoRate)<<32 | 1
	bitrate := e.bitrate()

	out, err := newMediaType()
	if err != nil {
		return err
	}
	defer out.release()
	sets := []error{
		setGUID(out, &mfMTMajorType, &mfMediaTypeVideo),
		setGUID(out, &mfMTSubtype, &mfVideoFormatH264),
		setUINT32(out, &mfMTAvgBitrate, bitrate),
		setUINT64(out, &mfMTFrameSize, size),
		setUINT64(out, &mfMTFrameRate, rate),
		setUINT64(out, &mfMTPixelAspect, 1<<32|1),
		setUINT32(out, &mfMTInterlaceMode, 2), // progressive
		setUINT32(out, &mfMTMpeg2Profile, h264ProfileBase),
	}
	if err := errors.Join(sets...); err != nil {
		return err
	}
	colour(out)
	if hr := e.mft.call(vtMFTSetOutputType, 0, uintptr(unsafe.Pointer(out)), 0); failed(hr) {
		return hresult("the encoder refused H.264 at this size", hr)
	}

	in, err := newMediaType()
	if err != nil {
		return err
	}
	defer in.release()
	sets = []error{
		setGUID(in, &mfMTMajorType, &mfMediaTypeVideo),
		setGUID(in, &mfMTSubtype, &mfVideoFormatNV12),
		setUINT64(in, &mfMTFrameSize, size),
		setUINT64(in, &mfMTFrameRate, rate),
		setUINT64(in, &mfMTPixelAspect, 1<<32|1),
		setUINT32(in, &mfMTInterlaceMode, 2),
	}
	if err := errors.Join(sets...); err != nil {
		return err
	}
	colour(in)
	if hr := e.mft.call(vtMFTSetInputType, 0, uintptr(unsafe.Pointer(in)), 0); failed(hr) {
		return hresult("the encoder refused NV12 at this size", hr)
	}
	if hr := e.mft.call(vtMFTGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&e.out))); failed(hr) {
		return hresult("GetOutputStreamInfo", hr)
	}
	e.header = e.sequenceHeader()
	for _, m := range []uintptr{mftMessageBeginStreaming, mftMessageStartOfStream} {
		if hr := e.mft.call(vtMFTProcessMessage, m, 0); failed(hr) {
			return hresult("starting the encoder", hr)
		}
	}
	return nil
}

// colour says what the NV12 is (BT.601 at limited range, as bgrxToNV12
// makes it), so the stream tells the browser and nobody guesses.
func colour(t *comObject) {
	_ = setUINT32(t, &mfMTYUVMatrix, 2)    // MFVideoTransferMatrix_BT601
	_ = setUINT32(t, &mfMTNominalRange, 2) // MFNominalRange_16_235
	_ = setUINT32(t, &mfMTPrimaries, 2)    // MFVideoPrimaries_BT709
	_ = setUINT32(t, &mfMTTransfer, 7)     // MFVideoTransFunc_sRGB
}

func (e *mfEncoder) setValue(api *windows.GUID, vt uint16, v uint64) {
	val := variant{vt: vt, val: v}
	e.codec.call(vtCodecSetValue, uintptr(unsafe.Pointer(api)), uintptr(unsafe.Pointer(&val)))
}

// bitrate is what motion on this screen may cost, in bits a second.
func (e *mfEncoder) bitrate() uint32 {
	return uint32(min(max(int64(e.width)*int64(e.height)*screenVideoBits, 500_000), 8_000_000))
}

// sequenceHeader reads the SPS and PPS the encoder put on its output type.
func (e *mfEncoder) sequenceHeader() []byte {
	var t *comObject
	if hr := e.mft.call(vtMFTGetOutputCurrent, 0, uintptr(unsafe.Pointer(&t))); failed(hr) || t == nil {
		return nil
	}
	defer t.release()
	var n uint32
	if hr := t.call(vtAttrGetBlobSize, uintptr(unsafe.Pointer(&mfMTSequenceHeader)),
		uintptr(unsafe.Pointer(&n))); failed(hr) || n == 0 || n > 1<<16 {
		return nil
	}
	b := make([]byte, n)
	if hr := t.call(vtAttrGetBlob, uintptr(unsafe.Pointer(&mfMTSequenceHeader)),
		uintptr(unsafe.Pointer(&b[0])), uintptr(n), uintptr(unsafe.Pointer(&n))); failed(hr) {
		return nil
	}
	return b[:n]
}

func (e *mfEncoder) Encode(s screenShot, key bool) ([]byte, error) {
	if s.width != e.width || s.height != e.height {
		return nil, errors.New("the screen changed size under the encoder")
	}
	if key && e.codec != nil {
		e.setValue(&codecAPIForceKeyFrame, vtUI4, 1)
	}
	sample, err := e.picture(s)
	if err != nil {
		return nil, err
	}
	defer sample.release()

	var out []byte
	hr := e.mft.call(vtMFTProcessInput, 0, uintptr(unsafe.Pointer(sample)), 0)
	if hr == mfENotAccepting {
		// Output waiting first; then the picture goes in.
		if out, err = e.drain(out); err != nil {
			return nil, err
		}
		hr = e.mft.call(vtMFTProcessInput, 0, uintptr(unsafe.Pointer(sample)), 0)
	}
	if failed(hr) {
		return nil, hresult("ProcessInput", hr)
	}
	if out, err = e.drain(out); err != nil {
		return nil, err
	}
	if key && len(out) > 0 && !h264HasNAL(out, 7) && len(e.header) > 0 {
		out = append(append([]byte(nil), e.header...), out...)
	}
	return out, nil
}

// picture is the screen as an NV12 sample, stamped with when it was taken.
func (e *mfEncoder) picture(s screenShot) (*comObject, error) {
	size := e.width * e.height * 3 / 2
	var buf *comObject
	if hr, _, _ := procMFCreateMemoryBuffer.Call(uintptr(size), uintptr(unsafe.Pointer(&buf))); failed(uint32(hr)) {
		return nil, hresult("MFCreateMemoryBuffer", uint32(hr))
	}
	defer buf.release()
	var p *byte
	var maxLen, cur uint32
	if hr := buf.call(vtBufferLock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)),
		uintptr(unsafe.Pointer(&cur))); failed(hr) {
		return nil, hresult("locking a picture buffer", hr)
	}
	if int(maxLen) < size {
		buf.call(vtBufferUnlock)
		return nil, errors.New("the picture buffer is too small")
	}
	bgrxToNV12(unsafe.Slice(p, size), s.fb, s.stride, e.width, e.height)
	buf.call(vtBufferUnlock)
	if hr := buf.call(vtBufferSetLength, uintptr(size)); failed(hr) {
		return nil, hresult("SetCurrentLength", hr)
	}

	var sample *comObject
	if hr, _, _ := procMFCreateSample.Call(uintptr(unsafe.Pointer(&sample))); failed(uint32(hr)) {
		return nil, hresult("MFCreateSample", uint32(hr))
	}
	if hr := sample.call(vtSampleAddBuffer, uintptr(unsafe.Pointer(buf))); failed(hr) {
		sample.release()
		return nil, hresult("AddBuffer", hr)
	}
	// In hundreds of nanoseconds, from when the encoder opened.
	sample.call(vtSampleSetTime, uintptr(time.Since(e.start)/100))
	sample.call(vtSampleSetDuration, uintptr(time.Second/screenVideoRate/100))
	return sample, nil
}

// drain appends to out every picture the encoder has ready.
func (e *mfEncoder) drain(out []byte) ([]byte, error) {
	for range 8 {
		db := mftOutputDataBuffer{}
		if e.out.flags&mftOutputProvidesSamples == 0 {
			s, err := e.outputSample()
			if err != nil {
				return out, err
			}
			db.sample = s
		}
		var status uint32
		hr := e.mft.call(vtMFTProcessOutput, 0, 1, uintptr(unsafe.Pointer(&db)), uintptr(unsafe.Pointer(&status)))
		if db.events != nil {
			db.events.release()
		}
		if hr == mfENeedMoreInput {
			db.sample.release()
			return out, nil
		}
		if failed(hr) {
			db.sample.release()
			return out, hresult("ProcessOutput", hr)
		}
		b, err := sampleBytes(db.sample)
		db.sample.release()
		if err != nil {
			return out, err
		}
		out = append(out, b...)
	}
	return out, nil
}

// outputSample is an empty sample for the encoder to write into.
func (e *mfEncoder) outputSample() (*comObject, error) {
	size := max(int(e.out.size), e.width*e.height*3/2)
	var buf *comObject
	if hr, _, _ := procMFCreateMemoryBuffer.Call(uintptr(size), uintptr(unsafe.Pointer(&buf))); failed(uint32(hr)) {
		return nil, hresult("MFCreateMemoryBuffer", uint32(hr))
	}
	defer buf.release()
	var sample *comObject
	if hr, _, _ := procMFCreateSample.Call(uintptr(unsafe.Pointer(&sample))); failed(uint32(hr)) {
		return nil, hresult("MFCreateSample", uint32(hr))
	}
	if hr := sample.call(vtSampleAddBuffer, uintptr(unsafe.Pointer(buf))); failed(hr) {
		sample.release()
		return nil, hresult("AddBuffer", hr)
	}
	return sample, nil
}

// sampleBytes copies out what a sample holds.
func sampleBytes(sample *comObject) ([]byte, error) {
	var buf *comObject
	if hr := sample.call(vtSampleToContiguous, uintptr(unsafe.Pointer(&buf))); failed(hr) {
		return nil, hresult("ConvertToContiguousBuffer", hr)
	}
	defer buf.release()
	var p *byte
	var maxLen, cur uint32
	if hr := buf.call(vtBufferLock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)),
		uintptr(unsafe.Pointer(&cur))); failed(hr) {
		return nil, hresult("locking the encoded picture", hr)
	}
	defer buf.call(vtBufferUnlock)
	if cur == 0 || p == nil {
		return nil, nil
	}
	return append([]byte(nil), unsafe.Slice(p, cur)...), nil
}

func (e *mfEncoder) Close() {
	e.codec.release()
	e.codec = nil
	e.mft.release()
	e.mft = nil
	if e.mf {
		procMFShutdown.Call()
		e.mf = false
	}
	if e.com {
		windows.CoUninitialize()
		e.com = false
	}
}

func newMediaType() (*comObject, error) {
	var t *comObject
	if hr, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&t))); failed(uint32(hr)) {
		return nil, hresult("MFCreateMediaType", uint32(hr))
	}
	return t, nil
}

func setGUID(t *comObject, key, v *windows.GUID) error {
	if hr := t.call(vtAttrSetGUID, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(v))); failed(hr) {
		return hresult("SetGUID", hr)
	}
	return nil
}

func setUINT32(t *comObject, key *windows.GUID, v uint32) error {
	if hr := t.call(vtAttrSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(v)); failed(hr) {
		return hresult("SetUINT32", hr)
	}
	return nil
}

func setUINT64(t *comObject, key *windows.GUID, v uint64) error {
	if hr := t.call(vtAttrSetUINT64, uintptr(unsafe.Pointer(key)), uintptr(v)); failed(hr) {
		return hresult("SetUINT64", hr)
	}
	return nil
}

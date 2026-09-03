package rdp

import (
	"encoding/binary"
	"fmt"
)

// The display stream carried to a browser.
//
// Binary, not JSON. A screen update is pixel data, and base64 in a JSON string
// costs a third more bytes on the wire plus a string allocation and a decode
// pass per frame — all of it on the main thread, which is the one place a
// browser cannot afford extra work. These frames are handed to a worker as an
// ArrayBuffer and transferred, not copied.
//
// The header is twelve bytes so the pixel payload starts four-byte aligned,
// which lets the worker take a Uint32Array view of it without copying.
const (
	// FrameHeaderSize is the fixed prefix on every frame.
	FrameHeaderSize = 12

	// FrameBitmap carries pixels for one rectangle.
	FrameBitmap uint8 = 1
	// FrameResize reports a new desktop size.
	FrameResize uint8 = 2
	// FrameReady says the session is up and the first update is coming.
	FrameReady uint8 = 3
	// FrameClosed says the session ended.
	FrameClosed uint8 = 4
)

// Fast-Path update codes (MS-RDPBCGR 2.2.9.1.2.1).
const (
	fastPathUpdateOrders  = 0x0
	fastPathUpdateBitmap  = 0x1
	fastPathUpdateSurface = 0x4
)

// Fast-Path fragmentation (MS-RDPBCGR 2.2.9.1.2.1).
const (
	fragSingle = 0x0
	fragLast   = 0x1
	fragFirst  = 0x2
	fragNext   = 0x3
)

// updateTypeBitmap is the first field of a bitmap update's data.
const updateTypeBitmap = 0x0001

// EncodeRect serialises one rectangle as a display frame.
//
// The caller owns the returned buffer until it is written; nothing is retained
// here, so a rectangle is garbage the moment it has been sent. That is what
// keeps memory flat over a long session: the framebuffer lives in the browser,
// and the gateway holds only whatever is in flight.
func EncodeRect(r Rect) []byte {
	out := make([]byte, FrameHeaderSize+len(r.Pixels))
	out[0] = FrameBitmap
	binary.LittleEndian.PutUint16(out[2:4], uint16(r.X))
	binary.LittleEndian.PutUint16(out[4:6], uint16(r.Y))
	binary.LittleEndian.PutUint16(out[6:8], uint16(r.Width))
	binary.LittleEndian.PutUint16(out[8:10], uint16(r.Height))
	copy(out[FrameHeaderSize:], r.Pixels)
	return out
}

// EncodeControl serialises a frame that carries no pixels.
func EncodeControl(kind uint8, width, height int) []byte {
	out := make([]byte, FrameHeaderSize)
	out[0] = kind
	binary.LittleEndian.PutUint16(out[6:8], uint16(width))
	binary.LittleEndian.PutUint16(out[8:10], uint16(height))
	return out
}

// Reassembler extracts bitmap updates from the Fast-Path output stream.
//
// Fast-Path updates may be fragmented across PDUs, so a decoder that looked at
// each PDU alone would silently drop every update larger than one frame — which
// is every update that matters, because the large ones are the screen changing.
//
// Not safe for concurrent use: one Reassembler belongs to one direction of one
// session, and interleaving two would splice their fragments together.
type Reassembler struct {
	buf []byte
	// maxBuffer bounds a single reassembled update. The fragment count is
	// attacker-controlled, so without it a target could stream fragments
	// forever and grow this without limit.
	maxBuffer int
}

// NewReassembler returns a Reassembler bounded to a sensible update size.
func NewReassembler() *Reassembler {
	// A full-screen 4K update compresses well below this; the bound exists for
	// the case where a peer never sends the final fragment.
	return &Reassembler{maxBuffer: 16 << 20}
}

// Feed consumes one Fast-Path output PDU and returns any rectangles it
// completed.
//
// Frames that are not bitmap updates — drawing orders, pointer changes, surface
// commands — return no rectangles and no error. Argus does not need to
// understand them to broker and record the session, and treating an unsupported
// update as a failure would end sessions over content the gateway has no
// opinion about.
func (r *Reassembler) Feed(pdu []byte) ([]Rect, error) {
	if len(pdu) < 2 || pdu[0]&actionMask == actionX224 {
		return nil, nil
	}

	// The Fast-Path header's length is one or two bytes; skip past it to the
	// update payload.
	body := pdu[2:]
	if pdu[1]&0x80 != 0 {
		if len(pdu) < 3 {
			return nil, nil
		}
		body = pdu[3:]
	}

	for len(body) > 0 {
		if len(body) < 3 {
			return nil, nil
		}
		header := body[0]
		code := header & 0x0F
		fragmentation := (header >> 4) & 0x03
		compressed := (header>>6)&0x02 != 0

		at := 1
		if compressed {
			// A compression flags byte follows. Compressed updates are not
			// decoded; skipping them cleanly is better than misreading one.
			at++
		}
		if len(body) < at+2 {
			return nil, nil
		}
		size := int(binary.LittleEndian.Uint16(body[at : at+2]))
		at += 2
		if len(body) < at+size {
			return nil, nil
		}
		data := body[at : at+size]
		body = body[at+size:]

		if compressed || (code != fastPathUpdateBitmap && code != fastPathUpdateOrders &&
			code != fastPathUpdateSurface) {
			r.buf = r.buf[:0]
			continue
		}
		if code != fastPathUpdateBitmap {
			continue
		}

		switch fragmentation {
		case fragSingle:
			return r.decode(data)
		case fragFirst:
			r.buf = append(r.buf[:0], data...)
		case fragNext:
			r.buf = append(r.buf, data...)
		case fragLast:
			r.buf = append(r.buf, data...)
			complete := r.buf
			r.buf = nil
			return r.decode(complete)
		}
		if len(r.buf) > r.maxBuffer {
			// A peer that never sends a final fragment must not grow this
			// forever.
			r.buf = nil
			return nil, fmt.Errorf("%w: reassembly exceeded %d bytes",
				ErrBadBitmap, r.maxBuffer)
		}
	}
	return nil, nil
}

// decode reads a bitmap update's payload.
func (r *Reassembler) decode(data []byte) ([]Rect, error) {
	if len(data) < 2 {
		return nil, nil
	}
	if binary.LittleEndian.Uint16(data[:2]) != updateTypeBitmap {
		return nil, nil
	}
	return ParseBitmapUpdate(data[2:])
}

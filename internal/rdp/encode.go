package rdp

import (
	"encoding/binary"
)

/*
Encoding screen rectangles, for a desktop Argus captures itself.

Where the agent is the RDP client, the rectangles arrive already encoded and
are passed on as they came. Where it captures the screen itself (screen
sharing, no Remote Desktop), it has raw pixels and makes the same rectangles
from them: interleaved RLE at 24 bits per pixel, the encoding the console's
decoder and replay already read. Nothing downstream can tell the two apart,
which is the point: the browser, the live view, the recording's chain and
its replay all work unchanged.

The encoder uses four orders -- background run (the pixel above), colour run,
colour image and their mega forms -- which is a subset every decoder reads.
It never emits two background runs in a row, so the decoder's inserted
foreground pixel never applies.
*/

// Order codes this encoder emits; see pixels for the full set.
const (
	encBGRun     = 0x00
	encColorRun  = 0x03
	encColorImg  = 0x04
	encMegaBG    = 0xF0
	encMegaColor = 0xF3
	encMegaImg   = 0xF4
)

// CompressRect encodes the rectangle (x, y, width, height) of a top-down
// 32-bit BGRX framebuffer, stride bytes per row, as a 24-bit interleaved RLE
// bitmap. buf is reused for the output when it is large enough; the returned
// Bitmap's Data aliases it.
func CompressRect(buf, fb []byte, stride, x, y, width, height int) Bitmap {
	out := buf[:0]
	// RDP bitmaps are bottom-up: the first scan line is the rectangle's last.
	row := func(r int) []byte {
		start := (y+height-1-r)*stride + x*4
		return fb[start : start+width*4]
	}
	px := func(r, c int) uint32 {
		p := row(r)[c*4:]
		return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16
	}
	total := width * height
	at := func(i int) uint32 { return px(i/width, i%width) }
	above := func(i int) uint32 { return px(i/width-1, i%width) }

	literalStart := -1
	flushLiteral := func(end int) {
		if literalStart < 0 {
			return
		}
		n := end - literalStart
		out = appendOrder(out, encColorImg, encMegaImg, n)
		for i := literalStart; i < end; i++ {
			v := at(i)
			out = append(out, byte(v), byte(v>>8), byte(v>>16))
		}
		literalStart = -1
	}

	lastWasBG := false
	for i := 0; i < total; {
		// A background run copies the row above, and is only worth an order
		// of its own when it covers a few pixels. Never on the first line,
		// where "above" is black, and never straight after another one.
		if i >= width && !lastWasBG {
			n := 0
			for i+n < total && n < 0xFFFF && at(i+n) == above(i+n) {
				n++
			}
			if n >= 2 {
				flushLiteral(i)
				out = appendOrder(out, encBGRun, encMegaBG, n)
				i += n
				lastWasBG = true
				continue
			}
		}
		v := at(i)
		n := 1
		for i+n < total && n < 0xFFFF && at(i+n) == v {
			n++
		}
		if n >= 3 {
			flushLiteral(i)
			out = appendOrder(out, encColorRun, encMegaColor, n)
			out = append(out, byte(v), byte(v>>8), byte(v>>16))
			i += n
			lastWasBG = false
			continue
		}
		if literalStart < 0 {
			literalStart = i
		}
		i++
		lastWasBG = false
		if i-literalStart == 0xFFFF {
			flushLiteral(i)
		}
	}
	flushLiteral(total)
	return Bitmap{X: x, Y: y, Width: width, Height: height, BPP: 24, Compressed: true, Data: out}
}

// appendOrder writes an order header for a run of n pixels, in the shortest
// form that holds it. Regular orders keep five bits of length, one extra
// byte covers 32 to 287, and the mega form sixteen bits.
func appendOrder(out []byte, regular, mega byte, n int) []byte {
	switch {
	case n < 32:
		return append(out, regular<<5|byte(n))
	case n < 32+256:
		return append(out, regular<<5, byte(n-32))
	default:
		return append(out, mega, byte(n), byte(n>>8))
	}
}

// Bitmap data flags (MS-RDPBCGR 2.2.9.1.1.3.1.2.2).
const noBitmapCompressHdr = 0x0400

// maxFastPath is the largest Fast-Path PDU: its length field is fifteen bits.
const maxFastPath = 0x7FFF

// fastPathOverhead is the Fast-Path header (three bytes with a two-byte
// length), the update header and size (three), and the update type and
// rectangle count (four).
const fastPathOverhead = 3 + 3 + 4

// bitmapDataHeader is TS_BITMAP_DATA before its pixel data.
const bitmapDataHeader = 18

// FastPathBitmaps packs rectangles into Fast-Path bitmap update PDUs, as a
// Remote Desktop server sends them, each under the protocol's size limit.
// A rectangle too large for one PDU on its own is left out; CompressRect on
// 64-pixel tiles never makes one.
func FastPathBitmaps(bitmaps []Bitmap) [][]byte {
	var pdus [][]byte
	var group []Bitmap
	size := fastPathOverhead
	flush := func() {
		if len(group) > 0 {
			pdus = append(pdus, fastPathBitmapPDU(group))
		}
		group, size = group[:0], fastPathOverhead
	}
	for _, b := range bitmaps {
		n := bitmapDataHeader + len(b.Data)
		if fastPathOverhead+n > maxFastPath {
			continue
		}
		if size+n > maxFastPath {
			flush()
		}
		group = append(group, b)
		size += n
	}
	flush()
	return pdus
}

func fastPathBitmapPDU(bitmaps []Bitmap) []byte {
	data := make([]byte, 4, 4+len(bitmaps)*(bitmapDataHeader+1024))
	binary.LittleEndian.PutUint16(data[0:2], updateTypeBitmap)
	binary.LittleEndian.PutUint16(data[2:4], uint16(len(bitmaps)))
	for _, b := range bitmaps {
		var h [bitmapDataHeader]byte
		binary.LittleEndian.PutUint16(h[0:2], uint16(b.X))
		binary.LittleEndian.PutUint16(h[2:4], uint16(b.Y))
		binary.LittleEndian.PutUint16(h[4:6], uint16(b.X+b.Width-1))
		binary.LittleEndian.PutUint16(h[6:8], uint16(b.Y+b.Height-1))
		binary.LittleEndian.PutUint16(h[8:10], uint16(b.Width))
		binary.LittleEndian.PutUint16(h[10:12], uint16(b.Height))
		binary.LittleEndian.PutUint16(h[12:14], uint16(b.BPP))
		var flags uint16
		if b.Compressed {
			flags = bitmapCompression | noBitmapCompressHdr
		}
		binary.LittleEndian.PutUint16(h[14:16], flags)
		binary.LittleEndian.PutUint16(h[16:18], uint16(len(b.Data)))
		data = append(data, h[:]...)
		data = append(data, b.Data...)
	}
	// Fast-Path output header: action 0, no flags; then a two-byte length
	// with its top bit set, counting the whole PDU.
	total := 3 + 3 + len(data)
	pdu := make([]byte, 0, total)
	pdu = append(pdu, 0x00, byte(total>>8)|0x80, byte(total))
	pdu = append(pdu, fastPathUpdateBitmap|fragSingle<<4, 0, 0)
	binary.LittleEndian.PutUint16(pdu[4:6], uint16(len(data)))
	return append(pdu, data...)
}

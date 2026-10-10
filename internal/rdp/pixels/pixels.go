// Package pixels decodes RDP bitmap data into RGBA.
//
// It is its own package, with nothing but the standard library beneath it, so
// the same decoder can be compiled to WebAssembly and run in the browser. The
// gateway forwards screen updates still compressed, and the browser expands
// them; one decoder on both sides means a recording replays exactly as the
// live session drew.
package pixels

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrBadBitmap reports malformed bitmap data.
var ErrBadBitmap = errors.New("malformed bitmap data")

// MaxDimension bounds a single rectangle.
//
// Width and height are 16-bit fields read from the wire before anything has
// been validated, so their product bounds an allocation. 16384 is far past any
// real display and still keeps the worst case to a few hundred megabytes rather
// than four gigabytes.
const MaxDimension = 16384

// MaxPixels bounds a rectangle's area.
//
// Bounding each dimension is not enough: 16384x16384 passes both checks and is
// 268 million pixels, a gigabyte of output from a header an attacker wrote.
// Sixteen million covers a 4K display whole, with room to spare, and caps one
// rectangle at 64 MB.
const MaxPixels = 1 << 24

// CheckSize refuses a rectangle too large to allocate.
func CheckSize(width, height int) error {
	if width <= 0 || height <= 0 || width > MaxDimension || height > MaxDimension {
		return fmt.Errorf("%w: %dx%d", ErrBadBitmap, width, height)
	}
	if width*height > MaxPixels {
		return fmt.Errorf("%w: %dx%d is %d pixels, over the %d cap",
			ErrBadBitmap, width, height, width*height, MaxPixels)
	}
	return nil
}

// Decode expands one rectangle to top-down RGBA.
//
// Checked again here even though the gateway checked the header: in the
// browser this is the first code to see the bytes, and a decoder that trusted
// its caller would be only as safe as whichever caller was newest.
func Decode(data []byte, width, height, bpp int, compressed bool) ([]byte, error) {
	var d Decoder
	return d.Decode(data, width, height, bpp, compressed)
}

// Decoder decodes rectangles into buffers it keeps between calls.
//
// What the browser uses: a busy desktop is hundreds of rectangles a second, and
// allocating a fresh output for each one is garbage the WebAssembly runtime has
// to collect on the same thread that is trying to keep up.
type Decoder struct {
	scan, out []byte
}

// Decode is the package-level Decode, except that the result is only valid
// until the next call.
func (d *Decoder) Decode(data []byte, width, height, bpp int, compressed bool) ([]byte, error) {
	if err := CheckSize(width, height); err != nil {
		return nil, err
	}
	if compressed {
		return d.decodeInterleavedRLE(data, width, height, bpp)
	}
	return d.decodeRaw(data, width, height, bpp)
}

// grow returns buf resized to n, reallocating only when it is too small.
func grow(buf []byte, n int) []byte {
	if cap(buf) < n {
		return make([]byte, n)
	}
	return buf[:n]
}

// maxRLEExpansion is the most pixels one byte of compressed input can produce.
//
// A mega order is three bytes carrying a sixteen-bit run length, so one byte
// buys at most 65535/3 pixels. An earlier version assumed the regular form's
// ceiling of 287 and rejected every real screen xrdp sent, because a blank row
// arrives as a single mega background run. Checking the claimed size against
// the input still bounds the allocation to what was actually sent rather than
// what the sender asserted.
const maxRLEExpansion = 65535 / 3

// BytesPerPixel converts a colour depth to its wire size.
func BytesPerPixel(bpp int) (int, error) {
	switch bpp {
	case 15, 16:
		return 2, nil
	case 24:
		return 3, nil
	case 32:
		return 4, nil
	default:
		return 0, fmt.Errorf("%w: %d bits per pixel", ErrBadBitmap, bpp)
	}
}

// decodeRaw expands an uncompressed bitmap to RGBA.
func (d *Decoder) decodeRaw(body []byte, width, height, bpp int) ([]byte, error) {
	bpx, err := BytesPerPixel(bpp)
	if err != nil {
		return nil, err
	}
	if len(body) < width*height*bpx {
		return nil, fmt.Errorf("%w: %d bytes for %dx%d at %d bpp",
			ErrBadBitmap, len(body), width, height, bpp)
	}
	d.out = grow(d.out, width*height*4)
	toRGBA(d.out, body, width, height, bpp)
	return d.out, nil
}

// toRGBA converts bottom-up rows of RDP pixels to top-down RGBA.
//
// RDP sends bitmaps bottom-up; the rows are reversed here so a canvas can take
// the result as it is. One loop per depth rather than one switch per pixel:
// this runs for every pixel of every update, in the browser, and the branch
// was most of its cost.
func toRGBA(out, src []byte, width, height, bpp int) {
	switch bpp {
	case 15, 16:
		lut := rgbTable(bpp)
		for y := range height {
			s := src[y*width*2 : (y+1)*width*2]
			o := out[(height-1-y)*width*4 : (height-y)*width*4]
			for x := range width {
				binary.LittleEndian.PutUint32(o[x*4:], lut[binary.LittleEndian.Uint16(s[x*2:])])
			}
		}
	default: // 24 and 32: BGR(X) bytes
		bpx := bpp / 8
		for y := range height {
			s := src[y*width*bpx : (y+1)*width*bpx]
			o := out[(height-1-y)*width*4 : (height-y)*width*4]
			for x := range width {
				p := s[x*bpx : x*bpx+3]
				q := o[x*4 : x*4+4]
				q[0], q[1], q[2], q[3] = p[2], p[1], p[0], 0xFF
			}
		}
	}
}

// rgbTables map every 15- and 16-bit pixel to RGBA, built on first use.
//
// 256 KB each, against a shift-and-mask per channel per pixel. The high bits
// are replicated into the low ones, which keeps white at 0xFF rather than 0xF8
// -- otherwise a grey cast across every light surface.
var rgbTables [2]*[1 << 16]uint32

func rgbTable(bpp int) *[1 << 16]uint32 {
	i := 0
	if bpp == 15 {
		i = 1
	}
	if t := rgbTables[i]; t != nil {
		return t
	}
	t := new([1 << 16]uint32)
	for v := range 1 << 16 {
		var r, g, b uint32
		if bpp == 15 {
			// RGB555. Read as 565 it shifts every colour by a bit and green
			// takes the red channel's low bit.
			r, g, b = uint32(v>>10)&0x1F, uint32(v>>5)&0x1F, uint32(v)&0x1F
			r, g, b = r<<3|r>>2, g<<3|g>>2, b<<3|b>>2
		} else {
			r, g, b = uint32(v>>11)&0x1F, uint32(v>>5)&0x3F, uint32(v)&0x1F
			r, g, b = r<<3|r>>2, g<<2|g>>4, b<<3|b>>2
		}
		t[v] = r | g<<8 | b<<16 | 0xFF<<24
	}
	rgbTables[i] = t
	return t
}

/* ── Interleaved RLE ─────────────────────────────────────────────────────── */

// Interleaved RLE order codes (MS-RDPEGDI 2.2.2.5.1).
//
// Three encodings share one byte stream and are told apart by its high bits.
// Regular orders put a three-bit code in the top bits and a five-bit length
// below; lite orders use four and four; mega orders are a whole byte of code
// followed by a sixteen-bit length. A decoder that implements only the regular
// form works on hand-built test data and fails on the first real screen, which
// is exactly how this was found: xrdp encodes a blank row as one mega
// background run and six bytes then legitimately describe eight thousand
// pixels.
const (
	regularBGRun       = 0x00
	regularFGRun       = 0x01
	regularFGBGImage   = 0x02
	regularColourRun   = 0x03
	regularColourImage = 0x04

	liteSetFGFGRun     = 0x0C
	liteSetFGFGBGImage = 0x0D
	liteDitheredRun    = 0x0E

	megaBGRun        = 0xF0
	megaFGRun        = 0xF1
	megaFGBGImage    = 0xF2
	megaColourRun    = 0xF3
	megaColourImage  = 0xF4
	megaSetFGRun     = 0xF6
	megaSetFGBGImage = 0xF7
	megaDitheredRun  = 0xF8
	specialFGBG1     = 0xF9
	specialFGBG2     = 0xFA
	specialWhite     = 0xFD
	specialBlack     = 0xFE
)

// orderCode extracts the code, which is scaled differently per form.
func orderCode(b byte) int {
	switch {
	case b&0xC0 != 0xC0:
		return int(b >> 5) // regular
	case b&0xF0 == 0xF0:
		return int(b) // mega and special
	default:
		return int(b >> 4) // lite
	}
}

// orderRunLength reads the length for an order, returning how many header bytes
// it consumed.
func orderRunLength(code int, in []byte) (runLength, advance int, err error) {
	if len(in) == 0 {
		return 0, 0, fmt.Errorf("%w: truncated order", ErrBadBitmap)
	}
	b := in[0]

	switch code {
	case regularFGBGImage:
		// Image orders count in groups of eight, which is why the multiplier
		// appears here and not in the run orders.
		n := int(b & 0x1F)
		if n == 0 {
			if len(in) < 2 {
				return 0, 0, fmt.Errorf("%w: truncated extended order", ErrBadBitmap)
			}
			return int(in[1]) + 1, 2, nil
		}
		return n * 8, 1, nil

	case liteSetFGFGBGImage:
		n := int(b & 0x0F)
		if n == 0 {
			if len(in) < 2 {
				return 0, 0, fmt.Errorf("%w: truncated extended order", ErrBadBitmap)
			}
			return int(in[1]) + 1, 2, nil
		}
		return n * 8, 1, nil

	case regularBGRun, regularFGRun, regularColourRun, regularColourImage:
		n := int(b & 0x1F)
		if n == 0 {
			if len(in) < 2 {
				return 0, 0, fmt.Errorf("%w: truncated extended order", ErrBadBitmap)
			}
			return int(in[1]) + 32, 2, nil
		}
		return n, 1, nil

	case liteSetFGFGRun, liteDitheredRun:
		n := int(b & 0x0F)
		if n == 0 {
			if len(in) < 2 {
				return 0, 0, fmt.Errorf("%w: truncated extended order", ErrBadBitmap)
			}
			return int(in[1]) + 16, 2, nil
		}
		return n, 1, nil

	case megaBGRun, megaFGRun, megaFGBGImage, megaColourRun, megaColourImage,
		megaSetFGRun, megaSetFGBGImage, megaDitheredRun:
		if len(in) < 3 {
			return 0, 0, fmt.Errorf("%w: truncated mega order", ErrBadBitmap)
		}
		return int(in[1]) | int(in[2])<<8, 3, nil

	case specialFGBG1, specialFGBG2, specialWhite, specialBlack:
		// Fixed-size orders with no length field.
		return 0, 1, nil
	}
	return 0, 0, fmt.Errorf("%w: unsupported order code 0x%02x", ErrBadBitmap, code)
}

// decodeInterleavedRLE expands RDP's run-length encoding.
//
// The format is compact for desktop content, which is why the gateway forwards
// it untouched: a typical update is a few kilobytes even at full resolution.
//
// Every write goes through a bounds check against the output buffer. The run
// lengths are attacker-controlled, and a decoder that trusted them would be a
// heap overflow reachable from a compromised target.
func (d *Decoder) decodeInterleavedRLE(body []byte, width, height, bpp int) ([]byte, error) {
	bpx, err := BytesPerPixel(bpp)
	if err != nil {
		return nil, err
	}
	// Refused before allocating: a body far too small to encode the claimed
	// size is not a bitmap, and finding that out after reserving the buffer is
	// how a compromised target turns a header field into memory pressure.
	if width*height > len(body)*maxRLEExpansion {
		return nil, fmt.Errorf("%w: %d bytes cannot encode %dx%d",
			ErrBadBitmap, len(body), width, height)
	}

	d.scan = grow(d.scan, width*height*bpx)
	// Reused, so cleared: a stream that ends short must leave black, as a
	// fresh buffer did, not the previous rectangle's pixels.
	clear(d.scan)
	if err := rleExpand(body, d.scan, width, bpx); err != nil {
		return nil, err
	}

	d.out = grow(d.out, width*height*4)
	toRGBA(d.out, d.scan, width, height, bpp)
	return d.out, nil
}

// rle is the decompressor's write position in scan.
//
// The semantics follow MS-RDPEGDI 3.1.9 and, where the spec is terse, FreeRDP's
// decoder, which is the one Windows servers have been tested against for longest.
// Three rules matter and were missing from an earlier version, which drew
// small errors into text wherever the encoder used them:
//
//   - on every line but the first, a foreground pixel is the pixel above XOR
//     the foreground colour, not the colour itself;
//   - on the first line, "the pixel above" is black;
//   - a background run that directly follows another starts with one
//     foreground pixel, because otherwise the two runs would have been one.
type rle struct {
	scan     []byte
	out, row int // bytes written; bytes per row
	bpx      int
}

var errOverflow = fmt.Errorf("%w: run overflows the bitmap", ErrBadBitmap)

func (w *rle) room(pixels int) error {
	if pixels < 0 || w.out+pixels*w.bpx > len(w.scan) {
		return errOverflow
	}
	return nil
}

// fill writes px n times, doubling the copied span so a long run costs a
// handful of copies rather than one per pixel.
func (w *rle) fill(px []byte, n int) error {
	if err := w.room(n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	start := w.out
	end := start + n*w.bpx
	copy(w.scan[start:], px[:w.bpx])
	for done := w.bpx; done < end-start; done *= 2 {
		copy(w.scan[start+done:end], w.scan[start:start+done])
	}
	w.out = end
	return nil
}

// copyAbove writes n pixels each equal to the one a row up.
//
// Copied a row at a time at most: a run longer than a row reads pixels it has
// itself just written, and one copy() over the overlap would read them before
// they exist.
func (w *rle) copyAbove(n int) error {
	if err := w.room(n); err != nil {
		return err
	}
	for left := n * w.bpx; left > 0; {
		k := min(left, w.row)
		copy(w.scan[w.out:w.out+k], w.scan[w.out-w.row:w.out-w.row+k])
		w.out += k
		left -= k
	}
	return nil
}

// xorAbove writes n pixels each equal to the one a row up XOR px.
func (w *rle) xorAbove(px []byte, n int) error {
	if err := w.room(n); err != nil {
		return err
	}
	for range n {
		for i := range w.bpx {
			w.scan[w.out+i] = w.scan[w.out-w.row+i] ^ px[i]
		}
		w.out += w.bpx
	}
	return nil
}

// fgbg paints n pixels from a bitmask: a set bit is a foreground pixel, a clear
// bit a background one. This is how RDP encodes text.
func (w *rle) fgbg(mask byte, n int, fg []byte, firstLine bool) error {
	if err := w.room(n); err != nil {
		return err
	}
	for bit := range n {
		set := mask&(1<<bit) != 0
		switch {
		case firstLine && set:
			copy(w.scan[w.out:], fg[:w.bpx])
		case firstLine:
			clear(w.scan[w.out : w.out+w.bpx])
		case set:
			for i := range w.bpx {
				w.scan[w.out+i] = w.scan[w.out-w.row+i] ^ fg[i]
			}
		default:
			copy(w.scan[w.out:w.out+w.bpx], w.scan[w.out-w.row:])
		}
		w.out += w.bpx
	}
	return nil
}

// fgbgImage reads runLength bits of masks from in, eight pixels per byte.
func (w *rle) fgbgImage(in []byte, i, runLength int, fg []byte, firstLine bool) (int, error) {
	for remaining := runLength; remaining > 0; {
		if i >= len(in) {
			return i, fmt.Errorf("%w: fgbg image ran out of bitmask", ErrBadBitmap)
		}
		n := min(remaining, 8)
		if err := w.fgbg(in[i], n, fg, firstLine); err != nil {
			return i, err
		}
		i++
		remaining -= n
	}
	return i, nil
}

// rleExpand runs the decompressor into scan.
func rleExpand(in, scan []byte, width, bpx int) error {
	w := rle{scan: scan, row: width * bpx, bpx: bpx}

	// Fixed-size scratch: no allocation per bitmap, which in the browser is
	// garbage on the thread that is trying to keep up.
	var fgBuf, whiteBuf, blackBuf [4]byte
	fg := fgBuf[:bpx]
	for i := range fg {
		fg[i] = 0xFF
	}
	white := whiteBuf[:bpx]
	copy(white, fg)
	black := blackBuf[:bpx]

	insertFg := false
	for i := 0; i < len(in); {
		// Decided per order, not per pixel, as every encoder assumes: a run
		// that starts on the first line is a first-line run to its end.
		firstLine := w.out < w.row

		code := orderCode(in[i])
		runLength, adv, err := orderRunLength(code, in[i:])
		if err != nil {
			return err
		}
		i += adv

		if code != regularBGRun && code != megaBGRun {
			insertFg = false
		}

		switch code {
		case regularBGRun, megaBGRun:
			if insertFg && runLength > 0 {
				var err error
				if firstLine {
					err = w.fill(fg, 1)
				} else {
					err = w.xorAbove(fg, 1)
				}
				if err != nil {
					return err
				}
				runLength--
			}
			if firstLine {
				if err := w.fill(black, runLength); err != nil {
					return err
				}
			} else if err := w.copyAbove(runLength); err != nil {
				return err
			}
			insertFg = true

		case regularFGRun, megaFGRun, liteSetFGFGRun, megaSetFGRun:
			if code == liteSetFGFGRun || code == megaSetFGRun {
				if i+bpx > len(in) {
					return fmt.Errorf("%w: set-foreground run has no colour", ErrBadBitmap)
				}
				copy(fg, in[i:i+bpx])
				i += bpx
			}
			var err error
			if firstLine {
				err = w.fill(fg, runLength)
			} else {
				err = w.xorAbove(fg, runLength)
			}
			if err != nil {
				return err
			}

		case regularColourRun, megaColourRun:
			if i+bpx > len(in) {
				return fmt.Errorf("%w: colour run has no colour", ErrBadBitmap)
			}
			if err := w.fill(in[i:i+bpx], runLength); err != nil {
				return err
			}
			i += bpx

		case regularColourImage, megaColourImage:
			need := runLength * bpx
			if i+need > len(in) {
				return fmt.Errorf("%w: colour image wants %d bytes, %d remain",
					ErrBadBitmap, need, len(in)-i)
			}
			if err := w.room(runLength); err != nil {
				return err
			}
			copy(w.scan[w.out:], in[i:i+need])
			w.out += need
			i += need

		case regularFGBGImage, megaFGBGImage, liteSetFGFGBGImage, megaSetFGBGImage:
			if code == liteSetFGFGBGImage || code == megaSetFGBGImage {
				if i+bpx > len(in) {
					return fmt.Errorf("%w: set-fgbg image has no colour", ErrBadBitmap)
				}
				copy(fg, in[i:i+bpx])
				i += bpx
			}
			if i, err = w.fgbgImage(in, i, runLength, fg, firstLine); err != nil {
				return err
			}

		case liteDitheredRun, megaDitheredRun:
			if i+2*bpx > len(in) {
				return fmt.Errorf("%w: dithered run has no colours", ErrBadBitmap)
			}
			if err := w.room(2 * runLength); err != nil {
				return err
			}
			pair := in[i : i+2*bpx]
			for range runLength {
				copy(w.scan[w.out:], pair)
				w.out += 2 * bpx
			}
			i += 2 * bpx

		case specialFGBG1:
			if err := w.fgbg(0x03, 8, fg, firstLine); err != nil {
				return err
			}
		case specialFGBG2:
			if err := w.fgbg(0x05, 8, fg, firstLine); err != nil {
				return err
			}
		case specialWhite:
			if err := w.fill(white, 1); err != nil {
				return err
			}
		case specialBlack:
			if err := w.fill(black, 1); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported order code 0x%02x", ErrBadBitmap, code)
		}
	}
	return nil
}

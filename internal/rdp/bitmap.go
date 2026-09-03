package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Bitmap update parsing (MS-RDPBCGR 2.2.9.1.1.3.1.2).
//
// This is what turns a session into something a browser can draw. The gateway
// decodes here rather than in the browser for two reasons that both come down
// to what the client has to do per frame: a codec in JavaScript runs on the
// main thread and competes with rendering, and shipping one costs a megabyte of
// script before the first pixel. Decoding server-side leaves the browser doing
// nothing but blitting exact rectangles.
//
// The alternative — transcoding to video — was rejected because it loses
// exactly what a privileged session review needs: sharp text at full
// resolution. These rectangles are the original pixels.

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

// maxRLEExpansion is the most pixels one byte of compressed input can produce.
//
// A mega order is three bytes carrying a sixteen-bit run length, so one byte
// buys at most 65535/3 pixels. An earlier version assumed the regular form's
// ceiling of 287 and rejected every real screen xrdp sent, because a blank row
// arrives as a single mega background run. Checking the claimed size against
// the input still bounds the allocation to what was actually sent rather than
// what the sender asserted.
const maxRLEExpansion = 65535 / 3

var (
	// ErrNotBitmapUpdate reports a PDU that is not a bitmap update.
	ErrNotBitmapUpdate = errors.New("not a bitmap update")
	// ErrBadBitmap reports a malformed rectangle.
	ErrBadBitmap = errors.New("malformed bitmap data")
)

// Rect is one decoded screen rectangle.
//
// Pixels are BGRA, which is what both RDP and a browser canvas want, so nothing
// swizzles them on the way through.
type Rect struct {
	X, Y          int
	Width, Height int
	Pixels        []byte
}

// bitmapFlags from MS-RDPBCGR 2.2.9.1.1.3.1.2.2.
const bitmapCompression = 0x0001

// ParseBitmapUpdate decodes a Bitmap Update into screen rectangles.
//
// data is the update payload with its updateType already consumed.
func ParseBitmapUpdate(data []byte) ([]Rect, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadBitmap, len(data))
	}
	count := int(binary.LittleEndian.Uint16(data[:2]))
	data = data[2:]

	rects := make([]Rect, 0, count)
	for i := range count {
		rect, rest, err := parseBitmapRect(data)
		if err != nil {
			return nil, fmt.Errorf("rectangle %d of %d: %w", i, count, err)
		}
		rects = append(rects, rect)
		data = rest
	}
	return rects, nil
}

func parseBitmapRect(data []byte) (Rect, []byte, error) {
	// destLeft, destTop, destRight, destBottom, width, height, bitsPerPixel,
	// flags, bitmapLength — eighteen bytes before the pixel data.
	const header = 18
	if len(data) < header {
		return Rect{}, nil, fmt.Errorf("%w: %d bytes of header", ErrBadBitmap, len(data))
	}

	left := int(binary.LittleEndian.Uint16(data[0:2]))
	top := int(binary.LittleEndian.Uint16(data[2:4]))
	width := int(binary.LittleEndian.Uint16(data[8:10]))
	height := int(binary.LittleEndian.Uint16(data[10:12]))
	bpp := int(binary.LittleEndian.Uint16(data[12:14]))
	flags := binary.LittleEndian.Uint16(data[14:16])
	length := int(binary.LittleEndian.Uint16(data[16:18]))

	if width <= 0 || height <= 0 || width > MaxDimension || height > MaxDimension {
		return Rect{}, nil, fmt.Errorf("%w: %dx%d", ErrBadBitmap, width, height)
	}
	if width*height > MaxPixels {
		return Rect{}, nil, fmt.Errorf("%w: %dx%d is %d pixels, over the %d cap",
			ErrBadBitmap, width, height, width*height, MaxPixels)
	}
	if len(data) < header+length {
		return Rect{}, nil, fmt.Errorf("%w: declares %d bytes, %d remain",
			ErrBadBitmap, length, len(data)-header)
	}
	body := data[header : header+length]
	rest := data[header+length:]

	var pixels []byte
	var err error
	if flags&bitmapCompression != 0 {
		pixels, err = decodeInterleavedRLE(body, width, height, bpp)
	} else {
		pixels, err = decodeRaw(body, width, height, bpp)
	}
	if err != nil {
		return Rect{}, nil, err
	}

	return Rect{X: left, Y: top, Width: width, Height: height, Pixels: pixels}, rest, nil
}

// bytesPerPixel converts a colour depth to its wire size.
func bytesPerPixel(bpp int) (int, error) {
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

// decodeRaw expands an uncompressed bitmap to BGRA.
//
// RDP sends raw bitmaps bottom-up, so the rows are reversed here rather than in
// the browser: doing it once server-side is cheaper than making every client
// walk the buffer backwards, and it keeps the wire format uniform.
func decodeRaw(body []byte, width, height, bpp int) ([]byte, error) {
	bpx, err := bytesPerPixel(bpp)
	if err != nil {
		return nil, err
	}
	if len(body) < width*height*bpx {
		return nil, fmt.Errorf("%w: %d bytes for %dx%d at %d bpp",
			ErrBadBitmap, len(body), width, height, bpp)
	}

	out := make([]byte, width*height*4)
	for y := range height {
		src := y * width * bpx
		dst := (height - 1 - y) * width * 4
		for x := range width {
			r, g, b := readPixel(body[src+x*bpx:], bpx)
			o := dst + x*4
			out[o+0] = b
			out[o+1] = g
			out[o+2] = r
			out[o+3] = 0xFF
		}
	}
	return out, nil
}

// readPixel converts one source pixel to eight-bit components.
func readPixel(p []byte, bpx int) (r, g, b byte) {
	switch bpx {
	case 2:
		v := binary.LittleEndian.Uint16(p[:2])
		// RGB565. Replicating the high bits into the low ones is what keeps
		// white at 0xFF rather than 0xF8, which otherwise shows as a grey cast
		// across every light surface.
		r = byte((v>>11)&0x1F) << 3
		g = byte((v>>5)&0x3F) << 2
		b = byte(v&0x1F) << 3
		r |= r >> 5
		g |= g >> 6
		b |= b >> 5
	case 3:
		b, g, r = p[0], p[1], p[2]
	case 4:
		b, g, r = p[0], p[1], p[2]
	}
	return r, g, b
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
// screens rather than frames: a typical update is a few kilobytes even at full
// resolution, so nothing has to be downscaled to keep a session responsive.
//
// Every write goes through a bounds check against the output buffer. The run
// lengths are attacker-controlled, and a decoder that trusted them would be a
// heap overflow reachable from a compromised target.
func decodeInterleavedRLE(body []byte, width, height, bpp int) ([]byte, error) {
	bpx, err := bytesPerPixel(bpp)
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

	scan := make([]byte, width*height*bpx)
	if err := rleExpand(body, scan, width, bpx); err != nil {
		return nil, err
	}

	out := make([]byte, width*height*4)
	for y := range height {
		src := y * width * bpx
		// RLE bitmaps are also bottom-up.
		dst := (height - 1 - y) * width * 4
		for x := range width {
			r, g, b := readPixel(scan[src+x*bpx:], bpx)
			o := dst + x*4
			out[o+0], out[o+1], out[o+2], out[o+3] = b, g, r, 0xFF
		}
	}
	return out, nil
}

// rleExpand runs the decompressor into scan.
func rleExpand(in, scan []byte, width, bpx int) error {
	rowBytes := width * bpx
	var out int

	fg := whitePixel(bpx)
	black := make([]byte, bpx)

	put := func(px []byte) error {
		if out+bpx > len(scan) {
			return fmt.Errorf("%w: run overflows the %d-byte bitmap", ErrBadBitmap, len(scan))
		}
		copy(scan[out:out+bpx], px)
		out += bpx
		return nil
	}
	above := func() []byte {
		if out < rowBytes {
			return black
		}
		return scan[out-rowBytes : out-rowBytes+bpx]
	}
	// fgbgImage paints from a bitmask: a set bit takes the foreground, a clear
	// bit copies the row above. This is how RDP encodes text.
	fgbg := func(mask byte, n int, colour []byte) error {
		for bit := range n {
			var err error
			if mask&(1<<bit) != 0 {
				err = put(colour)
			} else {
				err = put(above())
			}
			if err != nil {
				return err
			}
		}
		return nil
	}

	for i := 0; i < len(in); {
		code := orderCode(in[i])
		runLength, adv, err := orderRunLength(code, in[i:])
		if err != nil {
			return err
		}
		i += adv

		switch code {
		case regularBGRun, megaBGRun:
			for range runLength {
				if err := put(above()); err != nil {
					return err
				}
			}
		case regularFGRun, megaFGRun:
			for range runLength {
				if err := put(fg); err != nil {
					return err
				}
			}
		case liteSetFGFGRun, megaSetFGRun:
			if i+bpx > len(in) {
				return fmt.Errorf("%w: set-foreground run has no colour", ErrBadBitmap)
			}
			fg = append([]byte(nil), in[i:i+bpx]...)
			i += bpx
			for range runLength {
				if err := put(fg); err != nil {
					return err
				}
			}
		case regularColourRun, megaColourRun:
			if i+bpx > len(in) {
				return fmt.Errorf("%w: colour run has no colour", ErrBadBitmap)
			}
			colour := in[i : i+bpx]
			i += bpx
			for range runLength {
				if err := put(colour); err != nil {
					return err
				}
			}
		case regularColourImage, megaColourImage:
			need := runLength * bpx
			if i+need > len(in) {
				return fmt.Errorf("%w: colour image wants %d bytes, %d remain",
					ErrBadBitmap, need, len(in)-i)
			}
			for n := range runLength {
				if err := put(in[i+n*bpx : i+(n+1)*bpx]); err != nil {
					return err
				}
			}
			i += need
		case regularFGBGImage, megaFGBGImage:
			remaining := runLength
			for remaining > 0 {
				if i >= len(in) {
					return fmt.Errorf("%w: fgbg image ran out of bitmask", ErrBadBitmap)
				}
				n := min(remaining, 8)
				if err := fgbg(in[i], n, fg); err != nil {
					return err
				}
				i++
				remaining -= n
			}
		case liteSetFGFGBGImage, megaSetFGBGImage:
			if i+bpx > len(in) {
				return fmt.Errorf("%w: set-fgbg image has no colour", ErrBadBitmap)
			}
			fg = append([]byte(nil), in[i:i+bpx]...)
			i += bpx
			remaining := runLength
			for remaining > 0 {
				if i >= len(in) {
					return fmt.Errorf("%w: fgbg image ran out of bitmask", ErrBadBitmap)
				}
				n := min(remaining, 8)
				if err := fgbg(in[i], n, fg); err != nil {
					return err
				}
				i++
				remaining -= n
			}
		case liteDitheredRun, megaDitheredRun:
			if i+2*bpx > len(in) {
				return fmt.Errorf("%w: dithered run has no colours", ErrBadBitmap)
			}
			a, b := in[i:i+bpx], in[i+bpx:i+2*bpx]
			i += 2 * bpx
			for range runLength {
				if err := put(a); err != nil {
					return err
				}
				if err := put(b); err != nil {
					return err
				}
			}
		case specialFGBG1:
			if err := fgbg(0x03, 8, fg); err != nil {
				return err
			}
		case specialFGBG2:
			if err := fgbg(0x05, 8, fg); err != nil {
				return err
			}
		case specialWhite:
			if err := put(whitePixel(bpx)); err != nil {
				return err
			}
		case specialBlack:
			if err := put(black); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported order code 0x%02x", ErrBadBitmap, code)
		}
	}
	return nil
}

// whitePixel is the initial foreground colour.
func whitePixel(bpx int) []byte {
	px := make([]byte, bpx)
	for i := range px {
		px[i] = 0xFF
	}
	return px
}

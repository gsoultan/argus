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
// An order byte carries a five-bit length, escaping to a following byte plus
// thirty-two, so 287 is the ceiling. Checking the claimed size against the
// input this way makes the allocation proportional to what was actually sent
// rather than to what the sender asserted — which is the difference between a
// decoder and a way to spend a gateway's memory from across the network.
const maxRLEExpansion = 287

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

// Regular interleaved RLE order codes (MS-RDPEGDI 2.2.2.5.1).
//
// The code occupies the top three bits of the order byte, so only these five
// values are representable in the regular form. The lite and mega forms use
// different encodings and are refused rather than guessed at — a
// mis-decoded order paints the wrong pixels, which in a session recording is
// worse than a visible failure.
const (
	codeBackgroundRun = 0x0
	codeForegroundRun = 0x1
	codeFgBgImage     = 0x2
	codeColourRun     = 0x3
	codeColourImage   = 0x4
)

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
	// Refused before allocating: a one-byte body claiming a megapixel is not a
	// bitmap, and finding that out after reserving the buffer is how a
	// compromised target turns a header field into memory pressure.
	if width*height > len(body)*maxRLEExpansion {
		return nil, fmt.Errorf("%w: %d bytes cannot encode %dx%d",
			ErrBadBitmap, len(body), width, height)
	}

	// Decoded into a top-down buffer of source-format pixels, then converted.
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
	var out int // write cursor, in bytes

	// The foreground colour persists across runs, and the previous scanline is
	// the implicit background.
	fg := whitePixel(bpx)

	put := func(px []byte) error {
		if out+bpx > len(scan) {
			return fmt.Errorf("%w: run overflows the %d-byte bitmap", ErrBadBitmap, len(scan))
		}
		copy(scan[out:out+bpx], px)
		out += bpx
		return nil
	}
	// above returns the pixel directly above the cursor, which is the background
	// for the first scanline's purposes as well.
	above := func() []byte {
		if out < rowBytes {
			return make([]byte, bpx)
		}
		return scan[out-rowBytes : out-rowBytes+bpx]
	}

	for i := 0; i < len(in); {
		code, runLength, adv, err := readOrder(in[i:])
		if err != nil {
			return err
		}
		i += adv

		switch code {
		case codeBackgroundRun:
			for range runLength {
				if err := put(above()); err != nil {
					return err
				}
			}
		case codeForegroundRun:
			for range runLength {
				if err := put(fg); err != nil {
					return err
				}
			}
		case codeColourRun:
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
		case codeColourImage:
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
		case codeFgBgImage:
			// One bitmask byte per eight pixels: a set bit takes the current
			// foreground, a clear bit copies the row above. This is how RDP
			// encodes text, so getting it wrong is immediately visible.
			remaining := runLength
			for remaining > 0 {
				if i >= len(in) {
					return fmt.Errorf("%w: fgbg image ran out of bitmask", ErrBadBitmap)
				}
				mask := in[i]
				i++
				n := min(remaining, 8)
				for bit := range n {
					if mask&(1<<bit) != 0 {
						err = put(fg)
					} else {
						err = put(above())
					}
					if err != nil {
						return err
					}
				}
				remaining -= n
			}
		default:
			return fmt.Errorf("%w: unsupported order code 0x%x", ErrBadBitmap, code)
		}
	}
	return nil
}

// readOrder decodes one order header.
//
// The encoding packs a short run length into the low bits of the first byte and
// escapes to a longer form when it does not fit.
func readOrder(in []byte) (code, runLength, advance int, err error) {
	if len(in) == 0 {
		return 0, 0, 0, fmt.Errorf("%w: truncated order", ErrBadBitmap)
	}
	b := in[0]

	// Regular orders: three-bit code, five-bit length.
	code = int(b >> 5)
	runLength = int(b & 0x1F)
	if runLength != 0 {
		return code, runLength, 1, nil
	}
	// A zero length escapes to the next byte plus the implicit 32.
	if len(in) < 2 {
		return 0, 0, 0, fmt.Errorf("%w: truncated extended order", ErrBadBitmap)
	}
	return code, int(in[1]) + 32, 2, nil
}

// whitePixel is the initial foreground colour.
func whitePixel(bpx int) []byte {
	px := make([]byte, bpx)
	for i := range px {
		px[i] = 0xFF
	}
	return px
}

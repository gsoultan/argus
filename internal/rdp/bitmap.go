package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gsoultan/argus/internal/rdp/pixels"
)

// Bitmap update parsing (MS-RDPBCGR 2.2.9.1.1.3.1.2).
//
// The gateway reads the rectangle headers and forwards the pixel data as the
// target sent it; the browser decodes it in a worker, with the same decoder
// compiled to WebAssembly (internal/rdp/pixels). Decoding here and sending RGBA
// was simpler, but it shipped four bytes for every pixel of every update, and
// that bandwidth is what a slow link feels as lag.
//
// The alternative — transcoding to video — was rejected because it loses
// exactly what a privileged session review needs: sharp text at full
// resolution. These rectangles are the original pixels.

// MaxDimension and MaxPixels bound a single rectangle; see package pixels.
const (
	MaxDimension = pixels.MaxDimension
	MaxPixels    = pixels.MaxPixels
)

var (
	// ErrNotBitmapUpdate reports a PDU that is not a bitmap update.
	ErrNotBitmapUpdate = errors.New("not a bitmap update")
	// ErrBadBitmap reports a malformed rectangle. The same error the pixel
	// decoder returns, so a caller need not know which layer refused it.
	ErrBadBitmap = pixels.ErrBadBitmap
)

// Rect is one decoded screen rectangle.
//
// Pixels are RGBA, the order a canvas ImageData reads. RDP sends BGR, so the
// decoder swaps them once; it once left them as BGRA, and every red on a remote
// desktop drew as blue.
type Rect struct {
	X, Y          int
	Width, Height int
	Pixels        []byte
}

// Bitmap is one screen rectangle as the target sent it, before decoding.
//
// The live path forwards these to the browser as they are. Expanding them here
// multiplied every update by the ratio of RLE to RGBA -- often twenty times or
// more -- and that, not the target, is what made a remote desktop feel remote
// on anything slower than a LAN.
type Bitmap struct {
	X, Y          int
	Width, Height int
	// BPP is the colour depth of Data: 15, 16, 24 or 32.
	BPP int
	// Compressed marks Data as interleaved RLE rather than raw rows.
	Compressed bool
	// Data aliases the PDU it was read from. Encode or decode it before the
	// next read.
	Data []byte
}

// Decode expands the bitmap to RGBA.
func (b Bitmap) Decode() (Rect, error) {
	px, err := pixels.Decode(b.Data, b.Width, b.Height, b.BPP, b.Compressed)
	if err != nil {
		return Rect{}, err
	}
	return Rect{X: b.X, Y: b.Y, Width: b.Width, Height: b.Height, Pixels: px}, nil
}

// bitmapFlags from MS-RDPBCGR 2.2.9.1.1.3.1.2.2.
const bitmapCompression = 0x0001

// ParseBitmapUpdate decodes a Bitmap Update into screen rectangles.
//
// data is the update payload with its updateType already consumed.
func ParseBitmapUpdate(data []byte) ([]Rect, error) {
	bitmaps, err := ParseBitmaps(data)
	if err != nil {
		return nil, err
	}
	return decodeAll(bitmaps)
}

// ParseBitmaps splits a Bitmap Update into its rectangles without decoding
// them.
//
// Every header is checked here, so a bitmap that comes back is one whose size
// and length are already known to be sane; only the pixel data is left
// unexamined, for whichever side decodes it.
func ParseBitmaps(data []byte) ([]Bitmap, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("%w: %d bytes", ErrBadBitmap, len(data))
	}
	count := int(binary.LittleEndian.Uint16(data[:2]))
	data = data[2:]

	bitmaps := make([]Bitmap, 0, count)
	for i := range count {
		b, rest, err := parseBitmap(data)
		if err != nil {
			return nil, fmt.Errorf("rectangle %d of %d: %w", i, count, err)
		}
		bitmaps = append(bitmaps, b)
		data = rest
	}
	return bitmaps, nil
}

func decodeAll(bitmaps []Bitmap) ([]Rect, error) {
	rects := make([]Rect, 0, len(bitmaps))
	for i, b := range bitmaps {
		r, err := b.Decode()
		if err != nil {
			return nil, fmt.Errorf("rectangle %d of %d: %w", i, len(bitmaps), err)
		}
		rects = append(rects, r)
	}
	return rects, nil
}

func parseBitmap(data []byte) (Bitmap, []byte, error) {
	// destLeft, destTop, destRight, destBottom, width, height, bitsPerPixel,
	// flags, bitmapLength — eighteen bytes before the pixel data.
	const header = 18
	if len(data) < header {
		return Bitmap{}, nil, fmt.Errorf("%w: %d bytes of header", ErrBadBitmap, len(data))
	}

	left := int(binary.LittleEndian.Uint16(data[0:2]))
	top := int(binary.LittleEndian.Uint16(data[2:4]))
	width := int(binary.LittleEndian.Uint16(data[8:10]))
	height := int(binary.LittleEndian.Uint16(data[10:12]))
	bpp := int(binary.LittleEndian.Uint16(data[12:14]))
	flags := binary.LittleEndian.Uint16(data[14:16])
	length := int(binary.LittleEndian.Uint16(data[16:18]))

	if err := pixels.CheckSize(width, height); err != nil {
		return Bitmap{}, nil, err
	}
	if _, err := pixels.BytesPerPixel(bpp); err != nil {
		return Bitmap{}, nil, err
	}
	if len(data) < header+length {
		return Bitmap{}, nil, fmt.Errorf("%w: declares %d bytes, %d remain",
			ErrBadBitmap, length, len(data)-header)
	}

	return Bitmap{
		X: left, Y: top, Width: width, Height: height, BPP: bpp,
		Compressed: flags&bitmapCompression != 0,
		Data:       data[header : header+length],
	}, data[header+length:], nil
}

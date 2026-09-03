package rdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// bitmapRect assembles one rectangle the way a server sends it.
func bitmapRect(t testing.TB, x, y, w, h, bpp int, compressed bool, body []byte) []byte {
	t.Helper()
	var flags uint16
	if compressed {
		flags = bitmapCompression
	}
	out := make([]byte, 18)
	binary.LittleEndian.PutUint16(out[0:2], uint16(x))
	binary.LittleEndian.PutUint16(out[2:4], uint16(y))
	binary.LittleEndian.PutUint16(out[4:6], uint16(x+w-1))
	binary.LittleEndian.PutUint16(out[6:8], uint16(y+h-1))
	binary.LittleEndian.PutUint16(out[8:10], uint16(w))
	binary.LittleEndian.PutUint16(out[10:12], uint16(h))
	binary.LittleEndian.PutUint16(out[12:14], uint16(bpp))
	binary.LittleEndian.PutUint16(out[14:16], flags)
	binary.LittleEndian.PutUint16(out[16:18], uint16(len(body)))
	return append(out, body...)
}

func bitmapUpdate(rects ...[]byte) []byte {
	out := make([]byte, 2)
	binary.LittleEndian.PutUint16(out, uint16(len(rects)))
	for _, r := range rects {
		out = append(out, r...)
	}
	return out
}

// RDP sends bitmaps bottom-up. Flipping them server-side keeps the wire format
// uniform and saves every client from walking the buffer backwards.
func TestRawBitmapIsFlippedToTopDown(t *testing.T) {
	// Two rows of one pixel at 24bpp: first row red, second blue, bottom-up.
	body := []byte{
		0x00, 0x00, 0xFF, // BGR: red  (this is the BOTTOM row on screen)
		0xFF, 0x00, 0x00, // BGR: blue (this is the TOP row)
	}
	rects, err := ParseBitmapUpdate(bitmapUpdate(bitmapRect(t, 10, 20, 1, 2, 24, false, body)))
	if err != nil {
		t.Fatalf("ParseBitmapUpdate: %v", err)
	}
	if len(rects) != 1 {
		t.Fatalf("got %d rectangles", len(rects))
	}
	r := rects[0]
	if r.X != 10 || r.Y != 20 || r.Width != 1 || r.Height != 2 {
		t.Errorf("geometry = %d,%d %dx%d", r.X, r.Y, r.Width, r.Height)
	}
	// Top row on screen must be blue, bottom red.
	top := r.Pixels[0:4]
	bottom := r.Pixels[4:8]
	if !bytes.Equal(top, []byte{0xFF, 0x00, 0x00, 0xFF}) {
		t.Errorf("top pixel BGRA = %v, want blue", top)
	}
	if !bytes.Equal(bottom, []byte{0x00, 0x00, 0xFF, 0xFF}) {
		t.Errorf("bottom pixel BGRA = %v, want red", bottom)
	}
}

// Without replicating the high bits into the low ones, white arrives as 0xF8
// and every light surface carries a grey cast.
func TestRGB565WhiteIsFullyWhite(t *testing.T) {
	body := []byte{0xFF, 0xFF} // all bits set
	rects, err := ParseBitmapUpdate(bitmapUpdate(bitmapRect(t, 0, 0, 1, 1, 16, false, body)))
	if err != nil {
		t.Fatal(err)
	}
	px := rects[0].Pixels
	if px[0] != 0xFF || px[1] != 0xFF || px[2] != 0xFF {
		t.Errorf("white decoded as BGR %02x%02x%02x, want ffffff", px[0], px[1], px[2])
	}
	if px[3] != 0xFF {
		t.Errorf("alpha = %02x, want ff", px[3])
	}
}

func TestRLEColourImageAndRun(t *testing.T) {
	// Four pixels wide, one row, 24bpp.
	//  - a colour image of two pixels
	//  - a colour run of two pixels
	var body []byte
	body = append(body, byte(codeColourImage<<5)|2, // colour image, run 2
		0x11, 0x22, 0x33,
		0x44, 0x55, 0x66)
	body = append(body, byte(codeColourRun<<5)|2, // colour run, run 2
		0x77, 0x88, 0x99)

	rects, err := ParseBitmapUpdate(bitmapUpdate(bitmapRect(t, 0, 0, 4, 1, 24, true, body)))
	if err != nil {
		t.Fatalf("ParseBitmapUpdate: %v", err)
	}
	px := rects[0].Pixels
	want := []byte{
		0x11, 0x22, 0x33, 0xFF,
		0x44, 0x55, 0x66, 0xFF,
		0x77, 0x88, 0x99, 0xFF,
		0x77, 0x88, 0x99, 0xFF,
	}
	if !bytes.Equal(px, want) {
		t.Errorf("pixels =\n %v\nwant\n %v", px, want)
	}
}

// A background run copies the row above, which is how RDP encodes unchanged
// vertical regions cheaply.
func TestRLEBackgroundRunCopiesTheRowAbove(t *testing.T) {
	var body []byte
	// Row 0: two pixels of a colour image.
	body = append(body, byte(codeColourImage<<5)|2, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)
	// Row 1: two pixels of background, copying above.
	body = append(body, byte(codeBackgroundRun<<5)|2)

	rects, err := ParseBitmapUpdate(bitmapUpdate(bitmapRect(t, 0, 0, 2, 2, 24, true, body)))
	if err != nil {
		t.Fatalf("ParseBitmapUpdate: %v", err)
	}
	px := rects[0].Pixels
	// Bottom-up: decoded row 1 lands on screen row 0.
	if !bytes.Equal(px[0:4], []byte{0xAA, 0xBB, 0xCC, 0xFF}) {
		t.Errorf("copied pixel = %v", px[0:4])
	}
	if !bytes.Equal(px[8:12], []byte{0xAA, 0xBB, 0xCC, 0xFF}) {
		t.Errorf("source pixel = %v", px[8:12])
	}
}

// The run lengths come from the target. A decoder that trusted them would be a
// heap overflow reachable from a compromised host.
func TestRLERunsCannotOverflowTheBitmap(t *testing.T) {
	// One pixel of space, a run claiming 200.
	body := []byte{byte(codeColourRun<<5) | 0, 200, 0x11, 0x22, 0x33}
	_, err := ParseBitmapUpdate(bitmapUpdate(bitmapRect(t, 0, 0, 1, 1, 24, true, body)))
	if !errors.Is(err, ErrBadBitmap) {
		t.Errorf("err = %v, want ErrBadBitmap", err)
	}
}

// Width and height are 16-bit fields whose product bounds an allocation.
func TestOversizedRectanglesAreRefused(t *testing.T) {
	for _, d := range []int{MaxDimension + 1, 0xFFFF} {
		rect := bitmapRect(t, 0, 0, d, d, 24, false, nil)
		if _, err := ParseBitmapUpdate(bitmapUpdate(rect)); !errors.Is(err, ErrBadBitmap) {
			t.Errorf("%dx%d gave %v, want ErrBadBitmap", d, d, err)
		}
	}
}

func TestTruncatedBitmapsAreRefused(t *testing.T) {
	// Declares more body than is present.
	rect := bitmapRect(t, 0, 0, 4, 4, 24, false, []byte{1, 2, 3})
	binary.LittleEndian.PutUint16(rect[16:18], 999)
	if _, err := ParseBitmapUpdate(bitmapUpdate(rect)); !errors.Is(err, ErrBadBitmap) {
		t.Errorf("err = %v, want ErrBadBitmap", err)
	}
	// Raw body shorter than the geometry requires.
	rect = bitmapRect(t, 0, 0, 4, 4, 24, false, []byte{1, 2, 3})
	if _, err := ParseBitmapUpdate(bitmapUpdate(rect)); !errors.Is(err, ErrBadBitmap) {
		t.Errorf("short raw body gave %v", err)
	}
}

func TestUnsupportedDepthIsRefused(t *testing.T) {
	rect := bitmapRect(t, 0, 0, 1, 1, 8, false, []byte{0})
	if _, err := ParseBitmapUpdate(bitmapUpdate(rect)); !errors.Is(err, ErrBadBitmap) {
		t.Errorf("8bpp gave %v, want ErrBadBitmap", err)
	}
}

// These bytes arrive from the target, so a malformed update must be an error
// rather than a panic in the middle of a session.
func FuzzParseBitmapUpdate(f *testing.F) {
	// Built inline rather than through the helper, which needs a testing.TB the
	// fuzz seed stage does not have.
	seed := make([]byte, 18)
	binary.LittleEndian.PutUint16(seed[8:10], 2)
	binary.LittleEndian.PutUint16(seed[10:12], 2)
	binary.LittleEndian.PutUint16(seed[12:14], 24)
	binary.LittleEndian.PutUint16(seed[16:18], 12)
	seed = append(seed, bytes.Repeat([]byte{1, 2, 3}, 4)...)
	f.Add(append([]byte{0x01, 0x00}, seed...))
	f.Add([]byte{0x01, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		rects, err := ParseBitmapUpdate(b)
		if err != nil {
			return
		}
		for _, r := range rects {
			// A rectangle that claims a size its buffer cannot hold would
			// overflow whatever draws it.
			if len(r.Pixels) != r.Width*r.Height*4 {
				t.Fatalf("%dx%d rectangle carries %d bytes",
					r.Width, r.Height, len(r.Pixels))
			}
		}
	})
}

// Bounding each dimension is not enough. 16384x16384 passes both checks and is
// 268 million pixels — a gigabyte of output from a header the sender wrote.
func TestRectangleAreaIsBounded(t *testing.T) {
	rect := bitmapRect(t, 0, 0, MaxDimension, MaxDimension, 32, false, nil)
	if _, err := ParseBitmapUpdate(bitmapUpdate(rect)); !errors.Is(err, ErrBadBitmap) {
		t.Errorf("a %d-pixel rectangle was accepted", MaxDimension*MaxDimension)
	}
}

// The compressed path must decide it cannot work before it allocates. Found by
// fuzzing: without this the decoder ran at 1,000 executions per second with
// long stalls at zero, because each pathological input reserved and walked
// hundreds of megabytes. With it the same fuzzer sustains 70,000 per second.
func TestCompressedRectIsCheckedAgainstItsInput(t *testing.T) {
	// One byte of body cannot encode a megapixel under any encoding: an order
	// byte tops out at 287 pixels.
	rect := bitmapRect(t, 0, 0, 1024, 1024, 32, true, []byte{0x00})

	done := make(chan error, 1)
	go func() { _, err := ParseBitmapUpdate(bitmapUpdate(rect)); done <- err }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrBadBitmap) {
			t.Errorf("err = %v, want ErrBadBitmap", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the decoder did not refuse promptly; it is allocating on the " +
			"claimed size rather than on the input it was given")
	}
}

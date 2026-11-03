package rdp

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/gsoultan/argus/internal/rdp/pixels"
)

// fb makes a top-down BGRX framebuffer filled by fn.
func fb(width, height int, fn func(x, y int) uint32) []byte {
	out := make([]byte, width*height*4)
	for y := range height {
		for x := range width {
			v := fn(x, y)
			p := out[(y*width+x)*4:]
			p[0], p[1], p[2], p[3] = byte(v), byte(v>>8), byte(v>>16), 0xFF
		}
	}
	return out
}

// wantRGBA is the RGBA the decoder should produce for a region of fb.
func wantRGBA(buf []byte, stride, x, y, w, h int) []byte {
	out := make([]byte, 0, w*h*4)
	for r := y; r < y+h; r++ {
		for c := x; c < x+w; c++ {
			p := buf[r*stride+c*4:]
			out = append(out, p[2], p[1], p[0], 0xFF)
		}
	}
	return out
}

func TestCompressRectRoundTrips(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	cases := map[string]func(x, y int) uint32{
		"flat":    func(int, int) uint32 { return 0x336699 },
		"stripes": func(x, _ int) uint32 { return uint32(x/5%3) * 0x404040 },
		"rows":    func(_, y int) uint32 { return uint32(y%7) * 0x111111 },
		"noise":   func(int, int) uint32 { return rng.Uint32() & 0xFFFFFF },
		"text": func(x, y int) uint32 {
			if (x*7+y*3)%11 < 2 {
				return 0
			}
			return 0xFFFFFF
		},
		"mixed": func(x, y int) uint32 {
			if y > 20 {
				return rng.Uint32() & 0xFFFFFF
			}
			return uint32(x / 9)
		},
	}
	for name, fn := range cases {
		for _, size := range [][2]int{{64, 64}, {1, 1}, {3, 50}, {22, 7}, {300, 2}} {
			const W, H = 320, 80
			buf := fb(W, H, fn)
			w, h := size[0], size[1]
			x, y := 5, 3
			b := CompressRect(nil, buf, W*4, x, y, w, h)
			got, err := pixels.Decode(b.Data, b.Width, b.Height, b.BPP, b.Compressed)
			if err != nil {
				t.Fatalf("%s %dx%d: decode: %v", name, w, h, err)
			}
			if want := wantRGBA(buf, W*4, x, y, w, h); !bytes.Equal(got, want) {
				t.Fatalf("%s %dx%d: decoded pixels differ from the source", name, w, h)
			}
		}
	}
}

func TestCompressRectIsSmallForFlatScreens(t *testing.T) {
	buf := fb(64, 64, func(int, int) uint32 { return 0x202020 })
	b := CompressRect(nil, buf, 64*4, 0, 0, 64, 64)
	if len(b.Data) > 16 {
		t.Fatalf("a flat tile took %d bytes", len(b.Data))
	}
}

// The PDUs must read back through the same path replay and the live view
// use, or a recording made from a captured screen would replay blank.
func TestFastPathBitmapsReadBack(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	const W, H = 256, 192
	buf := fb(W, H, func(int, int) uint32 { return rng.Uint32() & 0xFFFFFF })
	var bitmaps []Bitmap
	for y := 0; y < H; y += 64 {
		for x := 0; x < W; x += 64 {
			bitmaps = append(bitmaps, CompressRect(nil, buf, W*4, x, y, 64, 64))
		}
	}
	pdus := FastPathBitmaps(bitmaps)
	if len(pdus) < 2 {
		t.Fatalf("12 noisy tiles fit in %d PDUs; the size limit is not being applied", len(pdus))
	}
	rea := NewReassembler()
	n := 0
	for _, pdu := range pdus {
		if len(pdu) > maxFastPath {
			t.Fatalf("PDU of %d bytes", len(pdu))
		}
		rects, err := DecodePDU(rea, pdu)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rects {
			if want := wantRGBA(buf, W*4, r.X, r.Y, r.Width, r.Height); !bytes.Equal(r.Pixels, want) {
				t.Fatalf("rectangle at %d,%d differs", r.X, r.Y)
			}
			n++
		}
	}
	if n != len(bitmaps) {
		t.Fatalf("read back %d of %d rectangles", n, len(bitmaps))
	}
}

package pixels

import (
	"bytes"
	"testing"
)

// Order codes for hand-built streams (MS-RDPEGDI 2.2.2.5.1).
func regular(code, n byte) byte { return code<<5 | n }

// decodeRows decodes an RLE stream at 24bpp and returns its rows top-down as
// BGR, which is how the encoder wrote them, so a test can say what it expects
// in the order it built the input.
func decodeRows(t *testing.T, in []byte, width, height int) [][]byte {
	t.Helper()
	out, err := Decode(in, width, height, 24, true)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	rows := make([][]byte, height)
	for y := range height {
		// Output is top-down; the stream was bottom-up.
		src := out[(height-1-y)*width*4 : (height-y)*width*4]
		for x := range width {
			rows[y] = append(rows[y], src[x*4+2], src[x*4+1], src[x*4])
		}
	}
	return rows
}

// Past the first line, a foreground pixel is the one above XOR the colour.
// Drawing the colour itself put stray pixels into text wherever an encoder
// relied on this, which is everywhere.
func TestForegroundRunXorsTheRowAbove(t *testing.T) {
	in := []byte{
		regular(regularColourImage, 2), 0x0F, 0x00, 0x00, 0xF0, 0x00, 0x00,
		regular(regularFGRun, 2), // foreground is white by default
	}
	rows := decodeRows(t, in, 2, 2)
	want := []byte{0xF0, 0xFF, 0xFF, 0x0F, 0xFF, 0xFF}
	if !bytes.Equal(rows[1], want) {
		t.Errorf("second line = % x, want % x", rows[1], want)
	}
}

// On the first line there is no row above, and a foreground pixel is just the
// foreground colour.
func TestForegroundRunOnTheFirstLineIsTheColour(t *testing.T) {
	rows := decodeRows(t, []byte{regular(regularFGRun, 2)}, 2, 1)
	if want := bytes.Repeat([]byte{0xFF}, 6); !bytes.Equal(rows[0], want) {
		t.Errorf("first line = % x, want white", rows[0])
	}
}

// Two background runs in a row would have been one run, so the encoder means
// the second to start with a foreground pixel.
func TestBackToBackBackgroundRunsInsertAForegroundPixel(t *testing.T) {
	in := []byte{regular(regularBGRun, 1), regular(regularBGRun, 2)}
	rows := decodeRows(t, in, 3, 1)
	want := []byte{0, 0, 0, 0xFF, 0xFF, 0xFF, 0, 0, 0}
	if !bytes.Equal(rows[0], want) {
		t.Errorf("row = % x, want black, white, black", rows[0])
	}
}

// A background run longer than a row reads pixels it has just written.
func TestBackgroundRunLongerThanARow(t *testing.T) {
	in := []byte{
		regular(regularColourImage, 2), 1, 2, 3, 4, 5, 6,
		regular(regularBGRun, 4),
	}
	rows := decodeRows(t, in, 2, 3)
	for y := range 3 {
		if want := []byte{1, 2, 3, 4, 5, 6}; !bytes.Equal(rows[y], want) {
			t.Errorf("row %d = % x, want % x", y, rows[y], want)
		}
	}
}

// RGB555 is not RGB565: read as 565 every colour shifts and green borrows a
// bit of red.
func TestRGB555(t *testing.T) {
	cases := map[uint16][3]byte{
		0x7FFF: {0xFF, 0xFF, 0xFF},
		0x7C00: {0xFF, 0x00, 0x00},
		0x03E0: {0x00, 0xFF, 0x00},
		0x001F: {0x00, 0x00, 0xFF},
	}
	for v, want := range cases {
		out, err := Decode([]byte{byte(v), byte(v >> 8)}, 1, 1, 15, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := [3]byte{out[0], out[1], out[2]}; got != want {
			t.Errorf("%#04x = %v, want %v", v, got, want)
		}
	}
}

// The browser keeps one Decoder for a whole session. A stream that ends short
// must leave black behind it, as a fresh buffer would, not the last rectangle.
func TestReusedDecoderDoesNotShowThePreviousRectangle(t *testing.T) {
	var d Decoder
	if _, err := d.Decode([]byte{regular(regularColourRun, 4), 9, 9, 9}, 2, 2, 24, true); err != nil {
		t.Fatal(err)
	}
	out, err := d.Decode([]byte{regular(regularColourRun, 1), 1, 1, 1}, 2, 2, 24, true)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] == 9 || out[4] == 9 {
		t.Errorf("pixels from the previous rectangle survived: % x", out)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte{regular(regularColourImage, 2), 1, 2, 3, 4, 5, 6, regular(regularBGRun, 2)}, 2, 2, true)
	f.Add([]byte{0xF0, 0xFF, 0x00}, 64, 64, true)
	f.Fuzz(func(t *testing.T, in []byte, w, h int, compressed bool) {
		if w < 1 || h < 1 || w > 256 || h > 256 {
			return
		}
		var d Decoder
		out, err := d.Decode(in, w, h, 16, compressed)
		if err == nil && len(out) != w*h*4 {
			t.Fatalf("%d bytes for %dx%d", len(out), w, h)
		}
	})
}

package rdp

import (
	"bytes"
	"testing"
)

// A scroll is recorded as ScrBlt orders among the bitmaps around it, and
// read back by the decoder the replay and the live view use, in order.
func TestCopiesRoundTripThroughTheRecording(t *testing.T) {
	fb := make([]byte, 64*64*4)
	for i := range fb {
		fb[i] = byte(i * 7)
	}
	tile := CompressRect(nil, fb, 64*4, 0, 0, 64, 64)
	tile.Data = append([]byte(nil), tile.Data...)
	up := Bitmap{X: 64, Y: 0, Width: 640, Height: 448, SrcX: 64, SrcY: 40, Copy: true}
	down := Bitmap{X: 0, Y: 128, Width: 64, Height: 64, SrcX: 0, SrcY: 100, Copy: true}
	in := []Bitmap{up, down, tile, up}

	rea := NewReassembler()
	var got []Bitmap
	for _, pdu := range FastPathBitmaps(in) {
		bs, err := ParsePDU(rea, pdu)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, bs...)
	}
	if len(got) != len(in) {
		t.Fatalf("%d rectangles back, want %d: %+v", len(got), len(in), got)
	}
	for i := range in {
		w, g := in[i], got[i]
		if w.Copy != g.Copy || w.X != g.X || w.Y != g.Y || w.Width != g.Width || w.Height != g.Height ||
			w.SrcX != g.SrcX || w.SrcY != g.SrcY || !bytes.Equal(w.Data, g.Data) {
			t.Errorf("rectangle %d: got %+v, want %+v", i, g, w)
		}
	}

	// And to the browser as twelve bytes, whichever way it went.
	for _, b := range []Bitmap{up, down} {
		f := EncodeBitmap(b)
		if len(f) != FrameHeaderSize || f[0] != FrameCopy {
			t.Fatalf("copy frame %x", f)
		}
		back, ok := ParseCopy(f)
		if !ok || back.X != b.X || back.Y != b.Y || back.Width != b.Width || back.Height != b.Height || back.SrcX != b.SrcX || back.SrcY != b.SrcY || !back.Copy {
			t.Errorf("ParseCopy = %+v, want %+v", back, b)
		}
		r, _ := b.Decode()
		if !bytes.Equal(EncodeRect(r), f) {
			t.Errorf("a decoded copy encodes differently: %x", EncodeRect(r))
		}
		if frameSize(f) != FrameHeaderSize {
			t.Errorf("frameSize of a copy is %d", frameSize(f))
		}
	}
}

// Orders in any other form are left alone, as every order was before: a
// Remote Desktop server is told Argus draws none.
func TestOtherOrdersAreIgnored(t *testing.T) {
	pdu := FastPathCopies([]Bitmap{{X: 0, Y: 0, Width: 10, Height: 10, SrcY: 5, Copy: true}})[0]
	pdu[len(pdu)-scrBltSize] = 0x01 // no type change: depends on state
	bs, err := ParsePDU(NewReassembler(), pdu)
	if err != nil || len(bs) != 0 {
		t.Fatalf("got %+v, %v", bs, err)
	}
	// A move off the screen is not passed on.
	pdu = FastPathCopies([]Bitmap{{X: 0, Y: 0, Width: 10, Height: 10, SrcY: -5, Copy: true}})[0]
	if bs, _ := ParsePDU(NewReassembler(), pdu); len(bs) != 0 {
		t.Fatalf("a move from above the screen came back: %+v", bs)
	}
}

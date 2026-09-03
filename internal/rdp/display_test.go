package rdp

import (
	"encoding/binary"
	"testing"
)

// fastPathUpdate wraps an update payload in a Fast-Path output PDU.
func fastPathUpdate(t testing.TB, code, fragmentation byte, data []byte) []byte {
	t.Helper()
	inner := make([]byte, 3+len(data))
	inner[0] = code | fragmentation<<4
	binary.LittleEndian.PutUint16(inner[1:3], uint16(len(data)))
	copy(inner[3:], data)

	total := 2 + len(inner)
	if total > 127 {
		out := make([]byte, 3+len(inner))
		out[0] = 0x00 // fast-path action
		out[1] = byte(0x80 | (total+1)>>8)
		out[2] = byte(total + 1)
		copy(out[3:], inner)
		return out
	}
	out := make([]byte, 2+len(inner))
	out[0] = 0x00
	out[1] = byte(total)
	copy(out[2:], inner)
	return out
}

// bitmapUpdateData prefixes a bitmap update with its updateType.
func bitmapUpdateData(rects ...[]byte) []byte {
	out := make([]byte, 2)
	binary.LittleEndian.PutUint16(out, updateTypeBitmap)
	return append(out, bitmapUpdate(rects...)...)
}

func TestReassemblerDecodesASingleFragment(t *testing.T) {
	body := []byte{0x11, 0x22, 0x33} // one 24bpp pixel
	data := bitmapUpdateData(bitmapRect(t, 5, 7, 1, 1, 24, false, body))
	pdu := fastPathUpdate(t, fastPathUpdateBitmap, fragSingle, data)

	rects, err := NewReassembler().Feed(pdu)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(rects) != 1 {
		t.Fatalf("got %d rectangles, want 1", len(rects))
	}
	if rects[0].X != 5 || rects[0].Y != 7 {
		t.Errorf("position = %d,%d", rects[0].X, rects[0].Y)
	}
}

// Updates larger than one PDU arrive fragmented. A decoder that looked at each
// PDU alone would drop every update big enough to matter — which is every
// update where the screen actually changed.
func TestReassemblerJoinsFragments(t *testing.T) {
	body := make([]byte, 0, 3*16)
	for i := range 16 {
		body = append(body, byte(i), byte(i*2), byte(i*3))
	}
	data := bitmapUpdateData(bitmapRect(t, 0, 0, 4, 4, 24, false, body))

	// Split the update across three PDUs.
	a, b, c := data[:10], data[10:30], data[30:]
	r := NewReassembler()

	if rects, err := r.Feed(fastPathUpdate(t, fastPathUpdateBitmap, fragFirst, a)); err != nil || len(rects) != 0 {
		t.Fatalf("first fragment produced %d rectangles, err=%v", len(rects), err)
	}
	if rects, err := r.Feed(fastPathUpdate(t, fastPathUpdateBitmap, fragNext, b)); err != nil || len(rects) != 0 {
		t.Fatalf("middle fragment produced %d rectangles, err=%v", len(rects), err)
	}
	rects, err := r.Feed(fastPathUpdate(t, fastPathUpdateBitmap, fragLast, c))
	if err != nil {
		t.Fatalf("final fragment: %v", err)
	}
	if len(rects) != 1 {
		t.Fatalf("reassembly produced %d rectangles, want 1", len(rects))
	}
	if rects[0].Width != 4 || rects[0].Height != 4 {
		t.Errorf("geometry = %dx%d", rects[0].Width, rects[0].Height)
	}
}

// Orders, pointer updates and surface commands are content the gateway has no
// opinion about. Treating them as failures would end sessions over graphics
// Argus does not need to understand in order to broker and record them.
func TestReassemblerIgnoresOtherUpdateTypes(t *testing.T) {
	for _, code := range []byte{fastPathUpdateOrders, fastPathUpdateSurface, 0x8, 0xB} {
		pdu := fastPathUpdate(t, code, fragSingle, []byte{1, 2, 3, 4})
		rects, err := NewReassembler().Feed(pdu)
		if err != nil {
			t.Errorf("update code 0x%x gave an error: %v", code, err)
		}
		if len(rects) != 0 {
			t.Errorf("update code 0x%x produced rectangles", code)
		}
	}
}

func TestReassemblerIgnoresTPKTFrames(t *testing.T) {
	rects, err := NewReassembler().Feed(tpkt([]byte{0x02, tpduData, 0x80, 1, 2}))
	if err != nil || len(rects) != 0 {
		t.Errorf("a TPKT frame produced %d rectangles, err=%v", len(rects), err)
	}
}

// A peer that never sends a final fragment must not grow the buffer forever.
func TestReassemblyIsBounded(t *testing.T) {
	r := NewReassembler()
	r.maxBuffer = 4096

	chunk := make([]byte, 1000)
	_, _ = r.Feed(fastPathUpdate(t, fastPathUpdateBitmap, fragFirst, chunk))
	var err error
	for range 20 {
		if _, err = r.Feed(fastPathUpdate(t, fastPathUpdateBitmap, fragNext, chunk)); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("an unterminated fragment stream grew without limit")
	}
}

/* ── Wire format ─────────────────────────────────────────────────────────── */

// The payload must start four-byte aligned so a worker can take a Uint32Array
// view of the pixels without copying them.
func TestFrameHeaderKeepsPixelsAligned(t *testing.T) {
	if FrameHeaderSize%4 != 0 {
		t.Fatalf("FrameHeaderSize = %d, which leaves the payload unaligned", FrameHeaderSize)
	}
}

func TestEncodeRect(t *testing.T) {
	pixels := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	frame := EncodeRect(Rect{X: 100, Y: 200, Width: 2, Height: 1, Pixels: pixels})

	if frame[0] != FrameBitmap {
		t.Errorf("type = %d", frame[0])
	}
	if got := binary.LittleEndian.Uint16(frame[2:4]); got != 100 {
		t.Errorf("x = %d", got)
	}
	if got := binary.LittleEndian.Uint16(frame[4:6]); got != 200 {
		t.Errorf("y = %d", got)
	}
	if got := binary.LittleEndian.Uint16(frame[6:8]); got != 2 {
		t.Errorf("width = %d", got)
	}
	if len(frame) != FrameHeaderSize+len(pixels) {
		t.Errorf("frame is %d bytes, want %d", len(frame), FrameHeaderSize+len(pixels))
	}
	// No base64, no JSON: the pixels are the tail of the buffer verbatim.
	for i, b := range pixels {
		if frame[FrameHeaderSize+i] != b {
			t.Fatalf("pixel %d was altered", i)
		}
	}
}

func TestEncodeControl(t *testing.T) {
	frame := EncodeControl(FrameResize, 1920, 1080)
	if frame[0] != FrameResize || len(frame) != FrameHeaderSize {
		t.Errorf("frame = %v", frame)
	}
	if binary.LittleEndian.Uint16(frame[6:8]) != 1920 ||
		binary.LittleEndian.Uint16(frame[8:10]) != 1080 {
		t.Error("resize did not carry the dimensions")
	}
}

func FuzzReassemblerFeed(f *testing.F) {
	f.Add([]byte{0x00, 0x08, 0x01, 0x03, 0x00, 1, 2, 3})
	f.Add([]byte{0x03, 0x00, 0x00, 0x04})
	f.Fuzz(func(t *testing.T, b []byte) {
		r := NewReassembler()
		rects, err := r.Feed(b)
		if err != nil {
			return
		}
		for _, rect := range rects {
			if len(rect.Pixels) != rect.Width*rect.Height*4 {
				t.Fatalf("%dx%d rectangle carries %d bytes",
					rect.Width, rect.Height, len(rect.Pixels))
			}
		}
	})
}

package rdp

import (
	"bytes"
	"encoding/binary"
	"image"
	"strings"
	"testing"
)

// Commands screen sharing makes, recorded through the tap, replay as the
// same commands, read by the parser Remote Desktop's go through, with a
// picture large enough to span several recorded PDUs.
func TestScreenGraphicsRecordAndReplayAsSent(t *testing.T) {
	h264 := make([]byte, 40000)
	for i := range h264 {
		h264[i] = byte(i * 7)
	}
	regions := []image.Rectangle{image.Rect(0, 0, 64, 64), image.Rect(128, 64, 320, 128)}
	cmds := [][]byte{
		BuildResetGraphics(1920, 1080),
		BuildCreateSurface(1, 1920, 1080),
		BuildMapSurfaceToOutput(1, 0, 0),
		BuildStartFrame(5),
		BuildAVC420(1, 1920, 1080, regions, h264),
		BuildEndFrame(5),
	}
	if len(cmds[0]) != gfxResetSize {
		t.Errorf("ResetGraphics is %d bytes, want %d", len(cmds[0]), gfxResetSize)
	}

	var buf bytes.Buffer
	const channel = 1004
	rec, err := NewRecorder(&buf, Header{Width: 1920, Height: 1080, Graphics: channel, Protocol: "screen"})
	if err != nil {
		t.Fatal(err)
	}
	tap := NewGraphicsTap(channel)
	for _, group := range [][][]byte{cmds[:3], cmds[3:]} {
		for _, pdu := range tap.PDUs(group...) {
			if err := rec.Write(ServerOutput, pdu); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Exact tiles alongside still replay as rectangles.
	for _, pdu := range FastPathBitmaps([]Bitmap{{X: 0, Y: 0, Width: 1, Height: 1, BPP: 32, Data: []byte{1, 2, 3, 0}}}) {
		if err := rec.Write(ServerOutput, pdu); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	info, err := DecodeRecording(strings.NewReader(buf.String()), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Graphics {
		t.Error("the replay does not say it carries the graphics pipeline")
	}
	var got [][]byte
	var part []byte
	bitmaps := 0
	for _, f := range replayed(t, out.Bytes()) {
		n := binary.LittleEndian.Uint32(f[4:8])
		switch f[0] {
		case FrameGraphics:
			got = append(got, f[FrameHeaderSize:FrameHeaderSize+n])
		case FrameGraphicsPart:
			part = append(part, f[FrameHeaderSize:FrameHeaderSize+n]...)
			if len(part) == int(binary.LittleEndian.Uint32(f[8:12])) {
				got = append(got, part)
				part = nil
			}
		case FrameBitmap:
			bitmaps++
		}
	}
	if len(got) != len(cmds) {
		t.Fatalf("%d commands replayed, want %d", len(got), len(cmds))
	}
	for i := range cmds {
		if !bytes.Equal(got[i], cmds[i]) {
			t.Errorf("command %d differs", i)
		}
	}
	if bitmaps != 1 {
		t.Errorf("%d bitmap rectangles, want 1", bitmaps)
	}

	pdus, err := ParseGraphics(bytes.Join(cmds, nil))
	if err != nil {
		t.Fatal(err)
	}
	w := pdus[4]
	if w.Cmd != GfxWireToSurface1 || w.Codec != CodecAVC420 || w.Surface != 1 ||
		w.Right != 1920 || w.Bottom != 1080 {
		t.Fatalf("the picture reads back as %+v", w)
	}
	if n := binary.LittleEndian.Uint32(w.Bitmap); n != 2 {
		t.Errorf("%d regions, want 2", n)
	}
	if !bytes.Equal(w.Bitmap[4+2*10:], h264) {
		t.Error("the H.264 does not follow the regions")
	}
	if pdus[3].FrameID != 5 || pdus[5].FrameID != 5 {
		t.Errorf("frame ids %d and %d, want 5", pdus[3].FrameID, pdus[5].FrameID)
	}
}

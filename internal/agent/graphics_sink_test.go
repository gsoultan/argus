package agent

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/gsoultan/argus/internal/rdp"
)

func TestGraphicsSinkTakesWholeFrames(t *testing.T) {
	var g GraphicsSink
	if g.take(desktopBatch) != nil {
		t.Fatal("an empty sink gave something")
	}
	small := bytes.Repeat([]byte{1}, 40<<10)
	g.Add(rdp.GraphicsPDU{Raw: small})
	g.Add(rdp.GraphicsPDU{Raw: small})
	g.Add(rdp.GraphicsPDU{Raw: small})
	g.Add(rdp.GraphicsPDU{Raw: small})
	g.Add(rdp.GraphicsPDU{Raw: small})

	var batches [][]byte
	for b := g.take(desktopBatch); b != nil; b = g.take(desktopBatch) {
		if len(b) > desktopBatch {
			t.Fatalf("a %d byte batch is over the %d limit", len(b), desktopBatch)
		}
		batches = append(batches, b)
	}
	if len(batches) != 2 {
		t.Fatalf("five 40 KiB commands went in %d batches, want 2", len(batches))
	}
	frames := 0
	for _, b := range batches {
		for at := 0; at < len(b); {
			if b[at] != rdp.FrameGraphics {
				t.Fatalf("frame type %d at %d", b[at], at)
			}
			at += rdp.FrameHeaderSize + int((binary.LittleEndian.Uint32(b[at+4:at+8])+3)&^3)
			frames++
		}
	}
	if frames != 5 {
		t.Fatalf("%d frames came out of five commands", frames)
	}
}

func TestGraphicsSinkPassesAnOversizedFrame(t *testing.T) {
	// A single frame larger than the limit still goes, alone, rather than
	// sticking in the sink for ever.
	var g GraphicsSink
	g.frames = rdp.EncodeGraphics(bytes.Repeat([]byte{2}, 1000))
	if b := g.take(100); len(b) != len(rdp.EncodeGraphics(bytes.Repeat([]byte{2}, 1000))) {
		t.Fatalf("took %d bytes", len(b))
	}
	if g.take(100) != nil {
		t.Fatal("the sink is not empty")
	}
}

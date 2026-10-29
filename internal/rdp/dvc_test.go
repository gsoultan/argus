package rdp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestStaticChannelReassembles(t *testing.T) {
	msg := bytes.Repeat([]byte("0123456789"), 400)
	var s staticChannel
	var got []byte
	pdus := channelPDUs(msg)
	if len(pdus) != 3 {
		t.Fatalf("4000 bytes went out in %d chunks, want 3", len(pdus))
	}
	for i, p := range pdus {
		out, err := s.feed(p)
		if err != nil {
			t.Fatal(err)
		}
		if (out != nil) != (i == len(pdus)-1) {
			t.Fatalf("chunk %d: complete=%v", i, out != nil)
		}
		got = out
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("reassembled message differs")
	}

	// A chunk that runs past what the first one announced is refused.
	bad := channelPDUs([]byte("abc"))[0]
	binary.LittleEndian.PutUint32(bad[0:4], 2)
	if _, err := s.feed(bad); err == nil {
		t.Fatal("an overlong chunk was accepted")
	}
}

func TestDynamicChannelsOpenOnlyGraphics(t *testing.T) {
	var d dynamicChannels

	ev, err := d.handle([]byte{dvcCapability << 4, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ev.reply, []byte{dvcCapability << 4, 0, 2, 0}) {
		t.Fatalf("capabilities answered %x, want version 2", ev.reply)
	}

	create := func(id byte, name string) dvcEvent {
		ev, err := d.handle(append([]byte{dvcCreate << 4, id}, append([]byte(name), 0)...))
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	clip := create(3, "CLIPRDR")
	if clip.opened || binary.LittleEndian.Uint32(clip.reply[2:]) != statusRefused {
		t.Fatalf("clipboard was not refused: %x", clip.reply)
	}
	gfx := create(7, graphicsChannelName)
	if !gfx.opened || binary.LittleEndian.Uint32(gfx.reply[2:]) != 0 {
		t.Fatalf("graphics was not accepted: %x", gfx.reply)
	}

	// Data on a refused channel goes nowhere.
	ev, _ = d.handle([]byte{dvcData << 4, 3, 'x'})
	if ev.data != nil {
		t.Fatal("data on a refused channel was delivered")
	}

	// A message in one PDU, and one in three.
	ev, _ = d.handle([]byte{dvcData << 4, 7, 'h', 'i'})
	if string(ev.data) != "hi" {
		t.Fatalf("single PDU gave %q", ev.data)
	}
	ev, _ = d.handle([]byte{dvcDataFirst << 4, 7, 6, 'a', 'b'})
	if ev.data != nil {
		t.Fatal("a first fragment was delivered alone")
	}
	d.handle([]byte{dvcData << 4, 7, 'c', 'd'})
	ev, _ = d.handle([]byte{dvcData << 4, 7, 'e', 'f'})
	if string(ev.data) != "abcdef" {
		t.Fatalf("fragments gave %q", ev.data)
	}
	if _, err := d.handle([]byte{dvcDataCompr << 4, 7, 'x'}); err == nil {
		t.Fatal("compressed data was not refused")
	}
}

func TestDVCMessagesSplitLargeOnes(t *testing.T) {
	msg := bytes.Repeat([]byte{0xAB}, 5000)
	var d dynamicChannels
	d.handle(append([]byte{dvcCreate << 4, 9}, append([]byte(graphicsChannelName), 0)...))
	var got []byte
	for _, m := range dvcMessages(9, 0, msg) {
		if len(m) > channelChunk {
			t.Fatalf("a %d byte PDU exceeds the chunk size", len(m))
		}
		ev, err := d.handle(m)
		if err != nil {
			t.Fatal(err)
		}
		got = ev.data
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("a split message did not reassemble")
	}
}

func TestParseGraphics(t *testing.T) {
	wire := binary.LittleEndian.AppendUint16(nil, 1)           // surface
	wire = binary.LittleEndian.AppendUint16(wire, CodecAVC420) // codec
	wire = append(wire, 0x20)                                  // pixel format
	for _, v := range []uint16{10, 20, 74, 84} {
		wire = binary.LittleEndian.AppendUint16(wire, v)
	}
	wire = binary.LittleEndian.AppendUint32(wire, 3)
	wire = append(wire, 'h', '2', '6')

	msg := gfxPDU(GfxStartFrame, []byte{0, 0, 0, 0, 5, 0, 0, 0})
	msg = append(msg, gfxPDU(GfxWireToSurface1, wire)...)
	msg = append(msg, gfxPDU(GfxEndFrame, []byte{5, 0, 0, 0})...)
	pdus, err := ParseGraphics(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pdus) != 3 || pdus[0].FrameID != 5 || pdus[2].FrameID != 5 {
		t.Fatalf("parsed %+v", pdus)
	}
	w := pdus[1]
	if w.Codec != CodecAVC420 || w.Left != 10 || w.Bottom != 84 || string(w.Bitmap) != "h26" {
		t.Fatalf("WireToSurface1 parsed as %+v", w)
	}

	if _, err := ParseGraphics(msg[:len(msg)-1]); err == nil {
		t.Fatal("a truncated message parsed")
	}
}

func TestGraphicsAsksForThePipeline(t *testing.T) {
	plain := GCCRequest(ClientInfo{Width: 1920, Height: 1080})
	gfx := GCCRequest(ClientInfo{Width: 1920, Height: 1080, Graphics: true})
	if bytes.Contains(plain, []byte(drdynvcName)) {
		t.Fatal("a session without graphics asked for drdynvc")
	}
	if !bytes.Contains(gfx, []byte(drdynvcName)) {
		t.Fatal("a session with graphics did not ask for drdynvc")
	}
}

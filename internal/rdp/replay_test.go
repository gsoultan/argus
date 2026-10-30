package rdp

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// buildRecording writes a small recording the way the gateway does.
func buildRecording(t *testing.T) (body string, header Header) {
	t.Helper()
	var buf bytes.Buffer
	header = Header{Width: 640, Height: 480, Title: "ops@win-01", Protocol: "tls"}
	rec, err := NewRecorder(&buf, header)
	if err != nil {
		t.Fatal(err)
	}

	// Two bitmap updates, each a single 2x1 rectangle at 24bpp.
	for range 2 {
		body := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
		update := bitmapUpdateData(bitmapRect(t, 4, 8, 2, 1, 24, false, body))
		pdu := fastPathUpdate(t, fastPathUpdateBitmap, fragSingle, update)
		if err := rec.Write(ServerOutput, pdu); err != nil {
			t.Fatal(err)
		}
	}
	// Client input must not be replayed as graphics.
	_ = rec.Write(ClientInput, []byte{0x04, 0x02, 0x00})
	if _, err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), header
}

// The same decoder the live path uses, fed from a file. A separate replay
// decoder would be a second implementation free to disagree with the first
// about what a session showed.
func TestDecodeRecordingProducesTheSameRectangles(t *testing.T) {
	body, header := buildRecording(t)

	var out bytes.Buffer
	info, err := DecodeRecording(strings.NewReader(body), &out)
	if err != nil {
		t.Fatalf("DecodeRecording: %v", err)
	}
	if info.Width != header.Width || info.Height != header.Height {
		t.Errorf("info = %dx%d, want %dx%d", info.Width, info.Height,
			header.Width, header.Height)
	}
	if info.Title != "ops@win-01" || info.Protocol != "tls" {
		t.Errorf("info = %+v", info)
	}

	// Two rectangles, each a timestamp then an ordinary display frame.
	data := out.Bytes()
	const per = ReplayFrameHeader + FrameHeaderSize + 2*1*4
	if len(data) != 2*per {
		t.Fatalf("got %d bytes, want %d for two rectangles", len(data), 2*per)
	}
	for i := range 2 {
		at := i * per
		if kind := data[at+ReplayFrameHeader]; kind != FrameBitmap {
			t.Errorf("frame %d type = %d", i, kind)
		}
		w := binary.LittleEndian.Uint16(data[at+ReplayFrameHeader+6 : at+ReplayFrameHeader+8])
		if w != 2 {
			t.Errorf("frame %d width = %d", i, w)
		}
	}
}

// A live frame and a replayed one must be the same structure, so the browser
// draws both with one code path and only the transport differs.
func TestReplayFramesWrapOrdinaryDisplayFrames(t *testing.T) {
	body, _ := buildRecording(t)
	var out bytes.Buffer
	if _, err := DecodeRecording(strings.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()

	// Strip the timestamp and the remainder must parse as a display frame.
	frame := data[ReplayFrameHeader:]
	if frame[0] != FrameBitmap {
		t.Fatalf("stripped frame type = %d", frame[0])
	}
	w := int(binary.LittleEndian.Uint16(frame[6:8]))
	h := int(binary.LittleEndian.Uint16(frame[8:10]))
	if len(frame) < FrameHeaderSize+w*h*4 {
		t.Errorf("frame carries %d bytes for %dx%d", len(frame)-FrameHeaderSize, w, h)
	}
}

// Input frames are not graphics. Replaying them would paint keystrokes onto
// the screen as though the server had sent them.
func TestReplaySkipsClientInput(t *testing.T) {
	body, _ := buildRecording(t)
	var out bytes.Buffer
	if _, err := DecodeRecording(strings.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	const per = ReplayFrameHeader + FrameHeaderSize + 2*1*4
	if len(out.Bytes()) != 2*per {
		t.Errorf("client input contributed frames: %d bytes", len(out.Bytes()))
	}
}

// A recording truncated mid-write leaves a partial final line. Everything
// before it is still evidence, and discarding all of it would lose the session
// because the gateway was killed at the wrong moment.
func TestTruncatedRecordingReplaysItsPrefix(t *testing.T) {
	body, _ := buildRecording(t)
	truncated := body[:len(body)-20]

	var out bytes.Buffer
	if _, err := DecodeRecording(strings.NewReader(truncated), &out); err != nil {
		t.Fatalf("DecodeRecording on a truncated file: %v", err)
	}
	if out.Len() == 0 {
		t.Error("a truncated recording produced nothing at all")
	}
}

func TestEmptyRecordingIsAnError(t *testing.T) {
	var out bytes.Buffer
	if _, err := DecodeRecording(strings.NewReader(""), &out); err == nil {
		t.Error("an empty recording decoded without error")
	}
}

// One undecodable update must not hide everything after it, which is the
// opposite of what an investigator needs.
func TestOneBadFrameDoesNotStopTheReplay(t *testing.T) {
	var buf bytes.Buffer
	rec, err := NewRecorder(&buf, Header{Width: 640, Height: 480})
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.Write(ServerOutput, []byte{0x00, 0x08, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	good := bitmapUpdateData(bitmapRect(t, 0, 0, 1, 1, 24, false, []byte{1, 2, 3}))
	_ = rec.Write(ServerOutput, fastPathUpdate(t, fastPathUpdateBitmap, fragSingle, good))
	_, _ = rec.Close()

	var out bytes.Buffer
	if _, err := DecodeRecording(strings.NewReader(buf.String()), &out); err != nil {
		t.Fatalf("DecodeRecording: %v", err)
	}
	if out.Len() == 0 {
		t.Error("a single bad frame suppressed the whole replay")
	}
}

// serverOnDrdynvc wraps a drdynvc message as the server sends it: in static
// channel chunks, each in a send data indication on the channel.
func serverOnDrdynvc(t *testing.T, channel uint16, msg []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for _, pdu := range channelPDUs(msg) {
		frame := SendDataRequest(1007, channel, pdu)
		inner, err := Payload(frame)
		if err != nil {
			t.Fatal(err)
		}
		inner[3] = mcsSendDataIndication << 2
		out = append(out, frame)
	}
	return out
}

// graphicsRecording records a session that drew through the graphics
// pipeline: drdynvc opening the channel, then commands, the second large
// enough to span several static channel chunks and dynamic channel PDUs.
func graphicsRecording(t *testing.T, header Header) (string, [][]byte) {
	t.Helper()
	var buf bytes.Buffer
	rec, err := NewRecorder(&buf, header)
	if err != nil {
		t.Fatal(err)
	}
	const channel, id = 1004, 7
	write := func(msg []byte) {
		for _, f := range serverOnDrdynvc(t, channel, msg) {
			if err := rec.Write(ServerOutput, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	write([]byte{dvcCapability << 4, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	write(append([]byte{dvcCreate << 4, id}, append([]byte(graphicsChannelName), 0)...))

	big := binary.LittleEndian.AppendUint16(nil, 0)          // surface
	big = binary.LittleEndian.AppendUint16(big, CodecAVC420) // codec
	big = append(big, 0x20)                                  // pixel format
	for _, v := range []uint16{0, 0, 64, 64} {
		big = binary.LittleEndian.AppendUint16(big, v)
	}
	big = binary.LittleEndian.AppendUint32(big, 5000)
	for i := range 5000 {
		big = append(big, byte(i))
	}
	cmds := [][]byte{
		gfxPDU(GfxStartFrame, []byte{0, 0, 0, 0, 1, 0, 0, 0}),
		gfxPDU(GfxWireToSurface1, big),
		gfxPDU(GfxEndFrame, []byte{1, 0, 0, 0}),
	}
	for _, c := range cmds {
		seg := append([]byte{zgfxSingle, zgfxTypeRDP8}, c...)
		for _, m := range dvcMessages(id, 0, seg) {
			write(m)
		}
	}
	// A bitmap update alongside, which still replays as rectangles.
	update := bitmapUpdateData(bitmapRect(t, 4, 8, 2, 1, 24, false, []byte{1, 2, 3, 4, 5, 6}))
	if err := rec.Write(ServerOutput, fastPathUpdate(t, fastPathUpdateBitmap, fragSingle, update)); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), cmds
}

// replayed splits a replay into its display frames.
func replayed(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var frames [][]byte
	for len(data) > 0 {
		if len(data) < ReplayFrameHeader+FrameHeaderSize {
			t.Fatalf("%d bytes left over", len(data))
		}
		f := data[ReplayFrameHeader:]
		size := frameSize(f)
		frames = append(frames, f[:size])
		data = f[size:]
	}
	return frames
}

// A session drawn by the graphics pipeline replays as the commands Windows
// sent, the frames the browser decoded live, whether the header names the
// channel or a recording from before it did leaves it to be found.
func TestGraphicsRecordingReplaysItsCommands(t *testing.T) {
	for _, channel := range []uint16{1004, 0} {
		body, cmds := graphicsRecording(t, Header{Width: 640, Height: 480, Graphics: channel})
		var out bytes.Buffer
		info, err := DecodeRecording(strings.NewReader(body), &out)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Graphics {
			t.Errorf("channel %d: replay does not say it carries the graphics pipeline", channel)
		}
		var got [][]byte
		var bitmaps int
		for _, f := range replayed(t, out.Bytes()) {
			switch f[0] {
			case FrameGraphics:
				n := binary.LittleEndian.Uint32(f[4:8])
				got = append(got, f[FrameHeaderSize:FrameHeaderSize+n])
			case FrameBitmap:
				bitmaps++
			default:
				t.Fatalf("channel %d: unexpected frame type %d", channel, f[0])
			}
		}
		if len(got) != len(cmds) {
			t.Fatalf("channel %d: %d commands replayed, want %d", channel, len(got), len(cmds))
		}
		for i := range cmds {
			if !bytes.Equal(got[i], cmds[i]) {
				t.Errorf("channel %d: command %d differs", channel, i)
			}
		}
		if bitmaps != 1 {
			t.Errorf("channel %d: %d bitmap rectangles, want 1", channel, bitmaps)
		}
	}
}

// A bitmap session says nothing of the graphics pipeline.
func TestBitmapRecordingIsNotGraphics(t *testing.T) {
	body, _ := buildRecording(t)
	info, err := DecodeRecording(strings.NewReader(body), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Graphics {
		t.Error("a bitmap recording claims the graphics pipeline")
	}
}

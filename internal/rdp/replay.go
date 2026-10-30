package rdp

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Replay decodes a recording into timed screen rectangles.
//
// The same decoder the live path uses, fed from a file instead of a socket.
// That is the point of recording raw PDUs: a recording is replayed by the code
// that would have shown it live, so what an auditor sees months later is
// produced the same way as what the operator saw at the time. A separate replay
// decoder would be a second implementation free to disagree with the first.

// ReplayFrameHeader is the timestamp prefix on each replayed frame.
//
// Four bytes of milliseconds, then an ordinary display frame. Kept separate
// from the frame header so a live frame and a replayed one are the same
// structure — the browser draws both with the same code and only the transport
// differs.
const ReplayFrameHeader = 4

// ReplayInfo describes a recording without decoding it.
type ReplayInfo struct {
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Title     string `json:"title"`
	Protocol  string `json:"protocol"`
	Timestamp int64  `json:"timestamp"`
	// DurationMS is the timestamp of the last frame.
	DurationMS int `json:"durationMs"`
	// Graphics says the session drew through the graphics pipeline, whose
	// commands the replay carries as they were sent, for the browser to
	// decode as it did live.
	Graphics bool `json:"graphics,omitempty"`
}

// DecodeRecording reads a recording and writes replay frames to w.
//
// Streams rather than collecting: a long session's decoded rectangles are far
// larger than the recording itself, and holding them all would make replay cost
// memory proportional to session length on the server as well as the client.
func DecodeRecording(r io.Reader, w io.Writer) (ReplayInfo, error) {
	var info ReplayInfo

	sc := bufio.NewScanner(r)
	// A single frame can carry a full-screen update; the default token limit is
	// far too small.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	if !sc.Scan() {
		return info, fmt.Errorf("recording is empty")
	}
	var header Header
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		return info, fmt.Errorf("recording header: %w", err)
	}
	info = ReplayInfo{
		Width: header.Width, Height: header.Height,
		Title: header.Title, Protocol: header.Protocol,
		Timestamp: header.Timestamp,
	}

	rea := NewReassembler()
	gfx := replayGraphics{channel: header.Graphics}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var frame []any
		if err := json.Unmarshal(line, &frame); err != nil || len(frame) != 3 {
			// A recording truncated mid-write leaves a partial final line. The
			// prefix before it is still valid evidence, so replay stops rather
			// than discarding everything that came before.
			break
		}
		stream, _ := frame[1].(string)
		if Stream(stream) != ServerOutput {
			continue
		}
		payload, _ := frame[2].(string)
		pdu, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			continue
		}

		at, _ := frame[0].(float64)
		ms := int(at * 1000)
		if ms > info.DurationMS {
			info.DurationMS = ms
		}

		if cmds, ok := gfx.feed(pdu); ok {
			info.Graphics = true
			for _, raw := range cmds {
				if err := writeReplay(w, ms, EncodeGraphics(raw)); err != nil {
					return info, err
				}
			}
			continue
		}

		// The same decoder the live path calls, not a second one that could
		// disagree with it about what a session showed.
		rects, err := DecodePDU(rea, pdu)
		if err != nil {
			// One undecodable update does not invalidate the rest. Stopping
			// here would hide everything after a single malformed frame, which
			// is the opposite of what an investigator needs.
			continue
		}
		for _, rect := range rects {
			if err := writeReplay(w, ms, EncodeRect(rect)); err != nil {
				return info, err
			}
		}
	}
	return info, sc.Err()
}

// writeReplay writes display frames, each behind the timestamp they were
// received at.
func writeReplay(w io.Writer, ms int, frames []byte) error {
	for len(frames) >= FrameHeaderSize {
		size := frameSize(frames)
		if size > len(frames) {
			return fmt.Errorf("replay frame of %d bytes runs past %d", size, len(frames))
		}
		var at [ReplayFrameHeader]byte
		binary.LittleEndian.PutUint32(at[:], uint32(ms))
		if _, err := w.Write(at[:]); err != nil {
			return err
		}
		if _, err := w.Write(frames[:size]); err != nil {
			return err
		}
		frames = frames[size:]
	}
	return nil
}

// frameSize is the whole size of the display frame at the start of f.
func frameSize(f []byte) int {
	switch f[0] {
	case FrameGraphics, FrameGraphicsPart:
		return FrameHeaderSize + int((binary.LittleEndian.Uint32(f[4:8])+3)&^3)
	}
	w := int(binary.LittleEndian.Uint16(f[6:8]))
	h := int(binary.LittleEndian.Uint16(f[8:10]))
	return FrameHeaderSize + w*h*4
}

// replayGraphics finds the graphics pipeline in a recording: drdynvc's
// channel, followed as the live client followed it.
type replayGraphics struct {
	// channel is drdynvc's, from the header. Recordings made before the
	// header said have it found instead, as the channel that opens with the
	// dynamic channel protocol's first message.
	channel uint16
	ch      graphicsChannel
	// broken stops following a channel that stopped making sense, since its
	// decompressor's history is lost from that point on.
	broken bool
}

// feed reports whether the PDU was on drdynvc, and the graphics commands it
// completed.
func (g *replayGraphics) feed(pdu []byte) ([][]byte, bool) {
	if len(pdu) == 0 || pdu[0]&actionMask != actionX224 {
		return nil, false
	}
	ch, payload, err := ParseSendDataIndication(pdu)
	if err != nil || ch == ChannelGlobal || (g.channel != 0 && ch != g.channel) {
		return nil, false
	}
	if g.channel == 0 {
		if len(payload) < 9 || payload[8]>>4 != dvcCapability {
			return nil, false
		}
		g.channel = ch
	}
	if g.broken {
		return nil, true
	}
	if g.ch.zgfx == nil {
		g.ch.zgfx = NewDecompressor()
	}
	_, pdus, err := g.ch.feed(payload)
	if err != nil {
		g.broken = true
	}
	cmds := make([][]byte, 0, len(pdus))
	for _, p := range pdus {
		cmds = append(cmds, p.Raw)
	}
	return cmds, true
}

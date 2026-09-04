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
			out := make([]byte, ReplayFrameHeader, ReplayFrameHeader+FrameHeaderSize+len(rect.Pixels))
			binary.LittleEndian.PutUint32(out, uint32(ms))
			out = append(out, EncodeRect(rect)...)
			if _, werr := w.Write(out); werr != nil {
				return info, werr
			}
		}
	}
	return info, sc.Err()
}

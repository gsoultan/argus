package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

/*
The terminal and desktop services' frames, after the header and answer.

A desktop uses the same frames: output carries display frames as the browser
draws them (internal/rdp/display.go), input carries the browser's input
messages as it sent them, resize the desktop's size in pixels.

Binary and length-prefixed: a keystroke is six bytes on the wire rather than a
JSON object, output is never re-encoded, and a frame boundary never splits a
UTF-8 sequence's meaning because nothing here decodes text at all.

	type (1 byte) | length (uint32, big-endian) | payload
*/

// Terminal frame types.
const (
	// Toward the agent.
	FrameInput  byte = 'i' // keystrokes
	FrameResize byte = 'r' // cols uint16, rows uint16
	// FrameTerminate ends the session for someone in the console, so the
	// recording says who and why rather than reading as a dropped
	// connection. JSON: {"by":..., "reason":...}.
	FrameTerminate byte = 'k'
	// FrameRefresh asks a desktop to repaint, for a viewer who arrived late.
	FrameRefresh byte = 'f'
	// Toward the control plane.
	FrameOutput byte = 'o' // terminal output
	FrameExit   byte = 'x' // exit code int32, then the recording's chain head
)

// MaxFrame bounds a frame's payload. Output is read in 32 KB pieces and a
// paste is chunked by the browser; anything larger is not a frame this
// protocol produces.
const MaxFrame = 256 << 10

// WriteFrame writes one frame in a single Write, so frames from two
// goroutines cannot interleave as long as each holds the writer.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("tunnel: frame of %d bytes exceeds %d", len(payload), MaxFrame)
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// FrameReader reads frames, reusing its buffer. The payload a call returns
// is valid until the next call.
type FrameReader struct {
	r   io.Reader
	hdr [5]byte
	buf []byte
}

func NewFrameReader(r io.Reader) *FrameReader { return &FrameReader{r: r} }

func (f *FrameReader) Next() (byte, []byte, error) {
	if _, err := io.ReadFull(f.r, f.hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(f.hdr[1:5])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("tunnel: frame of %d bytes exceeds %d", n, MaxFrame)
	}
	if cap(f.buf) < int(n) {
		f.buf = make([]byte, n)
	}
	f.buf = f.buf[:n]
	if _, err := io.ReadFull(f.r, f.buf); err != nil {
		return 0, nil, err
	}
	return f.hdr[0], f.buf, nil
}

// ResizePayload and ParseResize carry a window size.
func ResizePayload(cols, rows int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], uint16(min(max(cols, 1), 1<<16-1)))
	binary.BigEndian.PutUint16(b[2:4], uint16(min(max(rows, 1), 1<<16-1)))
	return b
}

func ParseResize(p []byte) (cols, rows int, ok bool) {
	if len(p) != 4 {
		return 0, 0, false
	}
	cols, rows = int(binary.BigEndian.Uint16(p[0:2])), int(binary.BigEndian.Uint16(p[2:4]))
	return cols, rows, cols > 0 && rows > 0
}

// ExitPayload and ParseExit carry how a session ended.
func ExitPayload(code int, chainHead string) []byte {
	b := make([]byte, 4+len(chainHead))
	binary.BigEndian.PutUint32(b[0:4], uint32(int32(code)))
	copy(b[4:], chainHead)
	return b
}

func ParseExit(p []byte) (code int, chainHead string, ok bool) {
	if len(p) < 4 {
		return 0, "", false
	}
	return int(int32(binary.BigEndian.Uint32(p[0:4]))), string(p[4:]), true
}

// Termination is a FrameTerminate's payload.
type Termination struct {
	By     string `json:"by"`
	Reason string `json:"reason"`
}

// TerminatePayload and ParseTerminate carry who ended a session and why.
func TerminatePayload(by, reason string) []byte {
	b, _ := json.Marshal(Termination{By: by, Reason: reason})
	return b
}

func ParseTerminate(p []byte) (Termination, bool) {
	var t Termination
	if json.Unmarshal(p, &t) != nil || t.By == "" {
		return Termination{}, false
	}
	return t, true
}

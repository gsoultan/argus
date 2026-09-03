package rdp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gsoultan/argus/internal/hashchain"
)

// The recording format, deliberately shaped like asciicast.
//
// Newline-delimited JSON: one header object, then one array per event. Nothing
// about RDP requires this shape, and it is chosen because it streams — a killed
// gateway still leaves a replayable prefix — and because it means the chain,
// the verifier, the console's Web Worker, argus-verify and the backup drill all
// work on RDP recordings with no changes at all.
//
// The alternative was video. Video plays anywhere with no player to write, but
// a privileged session recorded as H.264 is a few gigabytes an hour, carries no
// clipboard or file-transfer events, and cannot be searched. Keeping the
// protocol stream keeps the evidence: what was copied, what was transferred,
// and what was on screen, at a size that survives a retention policy.
const (
	// FormatVersion is the recording format's own version, independent of the
	// RDP protocol version.
	FormatVersion = 1
	// Extension is what these files are called on disk.
	Extension = ".argusrdp"
)

// Stream names the direction or kind of a recorded event.
type Stream string

const (
	// ServerOutput is a PDU from the target towards the client: the graphics.
	ServerOutput Stream = "s"
	// ClientInput is a PDU from the client towards the target: keyboard, mouse
	// and the virtual channels a user drives.
	ClientInput Stream = "c"
	// Meta is a decoded observation rather than raw protocol — a clipboard
	// transfer, a redirected drive, a resolution change. Carried in the same
	// chain as the stream it was derived from, so an auditor cannot be shown a
	// clipboard event that the recording does not support.
	Meta Stream = "m"
)

// Header is the first line of a recording.
type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Protocol  string            `json:"protocol,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Recorder serialises an RDP session under a tamper-evident hash chain.
type Recorder struct {
	chain   *hashchain.Chain
	started time.Time
}

// NewRecorder writes the header and returns a Recorder positioned at t=0.
func NewRecorder(w io.Writer, h Header) (*Recorder, error) {
	h.Version = FormatVersion
	if h.Width == 0 {
		h.Width = 1024
	}
	if h.Height == 0 {
		h.Height = 768
	}
	if h.Timestamp == 0 {
		h.Timestamp = time.Now().Unix()
	}

	line, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("marshal header: %w", err)
	}

	r := &Recorder{chain: hashchain.New(w), started: time.Now()}
	// The header is inside the chain, not outside it. Otherwise the width,
	// height and protocol a session ran under could be edited without
	// invalidating anything — and "which security protocol was this brokered
	// over" is exactly the kind of claim an auditor relies on.
	if err := r.chain.Emit(line); err != nil {
		return nil, err
	}
	return r, nil
}

// Write records one PDU on the given stream.
//
// The payload is base64 because the container is JSON. That costs a third more
// bytes than the raw stream, which is the price of one format the whole
// toolchain already verifies; RDP graphics are compressed by the protocol
// itself, so the absolute size stays far below a video of the same session.
func (r *Recorder) Write(s Stream, pdu []byte) error {
	if len(pdu) == 0 {
		return nil
	}
	line, err := json.Marshal([]any{
		round3(time.Since(r.started).Seconds()),
		string(s),
		base64.StdEncoding.EncodeToString(pdu),
	})
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	return r.chain.Emit(line)
}

// Event records a decoded observation.
func (r *Recorder) Event(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	line, err := json.Marshal([]any{
		round3(time.Since(r.started).Seconds()),
		string(Meta),
		string(payload),
	})
	if err != nil {
		return fmt.Errorf("marshal event frame: %w", err)
	}
	return r.chain.Emit(line)
}

// SetTap registers a function called with every committed line, for live
// shadowing. The contract is hashchain.Chain's: it must not block.
func (r *Recorder) SetTap(fn func(line []byte)) { r.chain.SetTap(fn) }

// Head returns the current chain head without closing.
func (r *Recorder) Head() string { return r.chain.Head() }

// Stats reports progress for the session record the control plane keeps.
func (r *Recorder) Stats() (lines int, bytes int64, duration time.Duration) {
	lines, bytes = r.chain.Stats()
	return lines, bytes, time.Since(r.started)
}

// Close finalises the recording and returns the chain head.
func (r *Recorder) Close() (string, error) { return r.chain.Close() }

// round3 matches the precision the terminal recorder writes, so the two formats
// agree on how a timestamp looks.
func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

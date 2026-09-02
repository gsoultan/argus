// Package recorder writes privileged session recordings.
//
// The wire format is asciicast v2 — newline-delimited JSON, one header object
// followed by one array per event. It is chosen because it streams (a killed
// gateway still leaves a replayable prefix), and because the Argus web console
// already decodes it. The format is documented at
// https://docs.asciinema.org/manual/asciicast/v2/
package recorder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Genesis is the chain root. Every recording starts from the same value so a
// verifier needs nothing but the file to check it.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Header is the first line of an asciicast v2 document.
type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp,omitempty"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Stream identifies which direction an event travelled.
type Stream string

const (
	// Output is what the target wrote to the terminal.
	Output Stream = "o"
	// Input is what the user typed. Capturing it is what lets the console
	// reconstruct a command timeline without guessing from echoed output.
	Input Stream = "i"
)

// Recorder serialises a session to asciicast v2 while maintaining a
// tamper-evident hash chain over the emitted lines.
//
// Each link is SHA-256(prevHash || line). Editing any line invalidates every
// subsequent link, so an auditor can detect tampering with nothing but the
// recording and the published chain head.
//
// Safe for concurrent use: a proxied session writes stdout from one goroutine
// and stdin from another.
type Recorder struct {
	mu      sync.Mutex
	w       io.Writer
	started time.Time
	head    string
	lines   int
	bytes   int64
	closed  bool
}

// New writes the header and returns a Recorder positioned at t=0.
func New(w io.Writer, h Header) (*Recorder, error) {
	h.Version = 2
	if h.Width == 0 {
		h.Width = 80
	}
	if h.Height == 0 {
		h.Height = 24
	}
	if h.Timestamp == 0 {
		h.Timestamp = time.Now().Unix()
	}

	r := &Recorder{w: w, started: time.Now(), head: Genesis}

	line, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("marshal header: %w", err)
	}
	if err := r.emit(line); err != nil {
		return nil, err
	}
	return r, nil
}

// emit writes one line and advances the chain. Caller must not hold the lock.
func (r *Recorder) emit(line []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.emitLocked(line)
}

func (r *Recorder) emitLocked(line []byte) error {
	if r.closed {
		return fmt.Errorf("recorder is closed")
	}

	sum := sha256.New()
	sum.Write([]byte(r.head))
	sum.Write(line)
	r.head = hex.EncodeToString(sum.Sum(nil))

	if _, err := r.w.Write(append(line, '\n')); err != nil {
		// Recording failures are fatal by design: the caller is expected to
		// terminate the session rather than let it continue unrecorded. An
		// auditor asking "are all privileged sessions recorded?" needs that to
		// be true without an asterisk.
		return fmt.Errorf("write recording: %w", err)
	}
	r.lines++
	r.bytes += int64(len(line)) + 1
	return nil
}

// Write records a chunk on the given stream, timestamped relative to New.
func (r *Recorder) Write(s Stream, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	elapsed := time.Since(r.started).Seconds()

	// asciicast events are [time, stream, data]. Marshalling through []any
	// keeps the float formatting consistent with what players expect.
	line, err := json.Marshal([]any{round3(elapsed), string(s), string(p)})
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return r.emit(line)
}

// Resize records a terminal size change. asciicast v2 carries these as an "r"
// event whose data is "COLSxROWS"; players that predate it ignore the line.
func (r *Recorder) Resize(cols, rows int) error {
	elapsed := time.Since(r.started).Seconds()
	line, err := json.Marshal([]any{round3(elapsed), "r", fmt.Sprintf("%dx%d", cols, rows)})
	if err != nil {
		return fmt.Errorf("marshal resize: %w", err)
	}
	return r.emit(line)
}

// Close finalises the recording and returns the chain head, which the control
// plane stores alongside the session so the artefact can be verified later.
func (r *Recorder) Close() (head string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.head, nil
	}
	r.closed = true
	if c, ok := r.w.(io.Closer); ok {
		err = c.Close()
	}
	return r.head, err
}

// Head returns the current chain head without closing.
func (r *Recorder) Head() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.head
}

// Stats reports progress, for the session record the control plane keeps.
func (r *Recorder) Stats() (lines int, bytes int64, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lines, r.bytes, time.Since(r.started)
}

// round3 matches the precision asciinema itself writes. Without it the JSON
// carries float noise that makes recordings needlessly large.
func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

// Package recorder writes privileged session recordings.
//
// The wire format is asciicast v2 — newline-delimited JSON, one header object
// followed by one array per event. It is chosen because it streams (a killed
// gateway still leaves a replayable prefix), and because the Argus web console
// already decodes it. The format is documented at
// https://docs.asciinema.org/manual/asciicast/v2/
package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gsoultan/argus/internal/hashchain"
)

// Genesis is the chain root. Every recording starts from the same value so a
// verifier needs nothing but the file to check it.
const Genesis = hashchain.Genesis

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
	// Kernel is an execution the kernel observed, carrying JSON rather than
	// terminal bytes. Players that predate it ignore the line.
	Kernel Stream = "x"
)

// Recorder serialises a session to asciicast v2 under a tamper-evident hash
// chain.
//
// The chain itself lives in internal/hashchain, shared with the RDP recorder.
// One implementation rather than two: this is the property the product is sold
// on, and a second copy would be a second place for it to be quietly wrong.
//
// Safe for concurrent use: a proxied session writes stdout from one goroutine
// and stdin from another.
type Recorder struct {
	chain   *hashchain.Chain
	started time.Time
}

// SetTap registers a function called with every line as it is committed.
//
// Live shadowing hangs off this rather than off the session's relay loop, so a
// viewer sees exactly the bytes that entered the hash chain. Anything else
// would let the live view and the recording disagree, and an operator deciding
// whether to kill a session must not be looking at a different session from the
// one the auditor will replay.
//
// The tap must not block and must not call back into the Recorder.
func (r *Recorder) SetTap(fn func(line []byte)) { r.chain.SetTap(fn) }

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

	r := &Recorder{chain: hashchain.New(w), started: time.Now()}

	line, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("marshal header: %w", err)
	}
	if err := r.chain.Emit(line); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Recorder) emit(line []byte) error { return r.chain.Emit(line) }

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

// Exec records a kernel-observed execution.
//
// Deliberately inside the recording, and therefore inside the hash chain. The
// value of this evidence is that it cannot be edited after the fact; kept in a
// separate stream it would be exactly as forgeable as the shell history it
// exists to replace, and a chain that covered the terminal output but not the
// command list would guarantee the wrong half.
func (r *Recorder) Exec(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal exec event: %w", err)
	}
	elapsed := time.Since(r.started).Seconds()
	line, err := json.Marshal([]any{round3(elapsed), string(Kernel), string(payload)})
	if err != nil {
		return fmt.Errorf("marshal exec frame: %w", err)
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
func (r *Recorder) Close() (string, error) { return r.chain.Close() }

// Head returns the current chain head without closing.
func (r *Recorder) Head() string { return r.chain.Head() }

// Stats reports progress, for the session record the control plane keeps.
func (r *Recorder) Stats() (lines int, bytes int64, duration time.Duration) {
	lines, bytes = r.chain.Stats()
	return lines, bytes, time.Since(r.started)
}

// round3 matches the precision asciinema itself writes. Without it the JSON
// carries float noise that makes recordings needlessly large.
func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}

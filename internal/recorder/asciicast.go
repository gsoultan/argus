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
	"math"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

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

// eventBufs holds the scratch a line is built in. A session's output goes
// through here chunk by chunk, so the line is built once into a reused
// buffer rather than through json.Marshal of a []any, which boxed the time,
// copied the chunk to a string and allocated the result: thirteen
// allocations for a single echoed keystroke.
var eventBufs = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// maxPooledEvent keeps one huge chunk from pinning its buffer in the pool.
const maxPooledEvent = 256 << 10

// event emits one asciicast event, [time, stream, data].
//
// The bytes are exactly those json.Marshal([]any{round3(t), stream,
// string(data)}) produces -- the same escaping, the same float formatting --
// which a test holds it to: the line is hashed into the chain, and every
// recording written before this must keep verifying the same way.
func (r *Recorder) event(elapsed float64, stream string, data []byte) error {
	bp := eventBufs.Get().(*[]byte)
	line := appendEvent((*bp)[:0], round3(elapsed), stream, data)
	// One spare byte so the chain's newline does not reallocate.
	if cap(line) == len(line) {
		line = append(line, 0)[:len(line)]
	}
	err := r.emit(line)
	if cap(line) <= maxPooledEvent {
		*bp = line[:0]
		eventBufs.Put(bp)
	}
	return err
}

// appendEvent appends [t,"stream","data"] as encoding/json would write it.
func appendEvent(dst []byte, t float64, stream string, data []byte) []byte {
	dst = append(dst, '[')
	dst = appendFloat(dst, t)
	dst = append(dst, ',')
	dst = appendJSONString(dst, []byte(stream))
	dst = append(dst, ',')
	dst = appendJSONString(dst, data)
	return append(dst, ']')
}

// appendFloat formats as encoding/json does for a float64.
func appendFloat(dst []byte, f float64) []byte {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		// json.Marshal refuses these; an elapsed time is never one.
		return append(dst, '0')
	}
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		// clean up e-09 to e-9
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst
}

const hexDigits = "0123456789abcdef"

// appendJSONString quotes s as encoding/json does with HTML escaping on:
// <, > and & as \u003c and friends, each invalid UTF-8 byte replaced by
// U+FFFD (written as the character itself, as this toolchain's encoder
// does), and U+2028 and U+2029 escaped.
func appendJSONString(dst []byte, s []byte) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRune(s[i:])
		if c == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, "\uFFFD"...)
			i += size
			start = i
			continue
		}
		if c == '\u2028' || c == '\u2029' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// Write records a chunk on the given stream, timestamped relative to New.
func (r *Recorder) Write(s Stream, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	return r.event(time.Since(r.started).Seconds(), string(s), p)
}

// Exec records a kernel-observed execution.
//
// Deliberately inside the recording, and therefore inside the hash chain. The
// value of this evidence is that it cannot be edited after the fact; kept in a
// separate stream it would be exactly as forgeable as the shell history it
// exists to replace, and a chain that covered the terminal output but not the
// command list would guarantee the wrong half.
func (r *Recorder) Exec(v any) error { return r.ExecAt(time.Time{}, v) }

// ExecAt records an execution at the moment the kernel saw it.
//
// The frame's offset is what places a command in the replay, and taking it
// from time.Now() placed it where this process got round to writing it
// instead. Under a backlog -- a burst of execs, a ring buffer filling, a busy
// consumer -- those are not the same moment, and that is exactly when the
// order matters to whoever is reading it back.
//
// A zero `at`, or one from before the recording started, falls back to now:
// there is no probe timestamp to trust, and a frame at a negative offset is
// one no player will place correctly.
func (r *Recorder) ExecAt(at time.Time, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal exec event: %w", err)
	}
	elapsed := time.Since(r.started).Seconds()
	if !at.IsZero() {
		if d := at.Sub(r.started); d >= 0 {
			elapsed = d.Seconds()
		}
	}
	return r.event(elapsed, string(Kernel), payload)
}

// Resize records a terminal size change. asciicast v2 carries these as an "r"
// event whose data is "COLSxROWS"; players that predate it ignore the line.
func (r *Recorder) Resize(cols, rows int) error {
	return r.event(time.Since(r.started).Seconds(), "r", []byte(fmt.Sprintf("%dx%d", cols, rows)))
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

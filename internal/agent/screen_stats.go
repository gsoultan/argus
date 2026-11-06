package agent

import (
	"encoding/json"
	"time"
)

// scrStats is what the helper's frames have cost so far, as JSON
// (screenStats); the agent logs the last of them when sharing ends, so how
// fast the screen can go on a host is known, not guessed.
const scrStats byte = 7

// screenStatsEvery is how often the helper says what its frames cost.
const screenStatsEvery = 5 * time.Second

// screenCaptureKind is a capturer that says how it captures: "dxgi" for
// Desktop Duplication, "gdi" for copying the screen whole.
type screenCaptureKind interface{ kind() string }

// screenStats adds up where a frame's time goes, in milliseconds.
type screenStats struct {
	Capture string `json:"capture"`
	Frames  int    `json:"frames"`
	Video   int    `json:"video"`
	// Wait is the time spent waiting for the screen to change; the rest
	// is the work of a frame once it has.
	WaitMS   float64 `json:"waitMs"`
	FindMS   float64 `json:"findMs"`
	MotionMS float64 `json:"motionMs"`
	TilesMS  float64 `json:"tilesMs"`
	WriteMS  float64 `json:"writeMs"`
	// SlowestMS is the most a frame's work took, capture to sent.
	SlowestMS float64 `json:"slowestMs"`
	// Inputs counts the frames that followed a key or a click, and the two
	// sums are how long after the input the screen was captured and the
	// frame sent: the part of the time to answer a keystroke that is
	// Windows drawing it, and the part that is this helper.
	Inputs         int     `json:"inputs"`
	InputCaptureMS float64 `json:"inputCaptureMs"`
	InputSentMS    float64 `json:"inputSentMs"`
	InputSlowestMS float64 `json:"inputSlowestMs"`
	// CreditMS is the time spent waiting for the agent to have room for
	// another frame (the browser acknowledging the ones before).
	CreditMS float64 `json:"creditMs"`
	// PaceMS is the time spent keeping to the frame rate.
	PaceMS float64 `json:"paceMs"`

	sent time.Time
}

func newScreenStats() *screenStats { return &screenStats{sent: time.Now()} }

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// frame counts one frame sent: captured by c, from started (waiting) to
// done (written).
func (s *screenStats) frame(c screenCapturer, started, captured, found, coded, encoded, done time.Time,
	video bool) {
	if k, ok := c.(screenCaptureKind); ok {
		s.Capture = k.kind()
	}
	s.Frames++
	if video {
		s.Video++
	}
	s.WaitMS += millis(captured.Sub(started))
	s.FindMS += millis(found.Sub(captured))
	s.MotionMS += millis(coded.Sub(found))
	s.TilesMS += millis(encoded.Sub(coded))
	s.WriteMS += millis(done.Sub(encoded))
	s.SlowestMS = max(s.SlowestMS, millis(done.Sub(captured)))
}

// input counts a frame that went after an input made at in.
func (s *screenStats) input(in, captured, done time.Time) {
	s.Inputs++
	s.InputCaptureMS += millis(captured.Sub(in))
	s.InputSentMS += millis(done.Sub(in))
	s.InputSlowestMS = max(s.InputSlowestMS, millis(done.Sub(in)))
}

// due says whether it is time to send what the frames cost.
func (s *screenStats) due() bool {
	if time.Since(s.sent) < screenStatsEvery {
		return false
	}
	s.sent = time.Now()
	return true
}

func (s *screenStats) encode() []byte {
	b, _ := json.Marshal(s)
	return b
}

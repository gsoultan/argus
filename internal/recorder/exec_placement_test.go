package recorder

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// An execution is placed in the replay where the kernel saw it.
//
// The frame's offset is what a player uses to position a command. Taking it
// from time.Now() put it where this process got round to writing it, which
// under a backlog is somewhere else entirely -- and a burst is exactly when the
// spacing of a command list is worth reading.
func TestAnExecutionIsPlacedWhenItHappened(t *testing.T) {
	var buf bytes.Buffer
	rec, err := New(&buf, Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatal(err)
	}
	started := rec.started

	// Three execs the kernel saw a second apart, all written now because the
	// consumer had fallen behind.
	for i := 1; i <= 3; i++ {
		at := started.Add(time.Duration(i) * time.Second)
		if err := rec.ExecAt(at, map[string]any{"filename": "/bin/true"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	got := execOffsets(t, buf.String())
	want := []float64{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("got %d exec frames, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d at %.3fs, want %.3fs -- written together, they "+
				"were stamped together instead of when each happened",
				i, got[i], want[i])
		}
	}
}

// Without a kernel time it falls back to now, and never to a negative offset.
func TestAPlacementFallsBackRatherThanGoingNegative(t *testing.T) {
	var buf bytes.Buffer
	rec, err := New(&buf, Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatal(err)
	}

	// Zero: no probe timestamp at all.
	if err := rec.ExecAt(time.Time{}, map[string]any{"filename": "/bin/a"}); err != nil {
		t.Fatal(err)
	}
	// Before the recording started, which no player can place.
	if err := rec.ExecAt(rec.started.Add(-time.Hour), map[string]any{"filename": "/bin/b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	for i, off := range execOffsets(t, buf.String()) {
		if off < 0 {
			t.Errorf("frame %d is at %.3fs; a negative offset is not a place", i, off)
		}
	}
}

// execOffsets pulls the timestamp of every kernel-exec frame.
func execOffsets(t *testing.T, body string) []float64 {
	t.Helper()
	var out []float64
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.Contains(line, `"`+string(Kernel)+`"`) {
			continue
		}
		var frame []any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		if len(frame) < 2 {
			continue
		}
		if ts, ok := frame[0].(float64); ok {
			out = append(out, ts)
		}
	}
	return out
}

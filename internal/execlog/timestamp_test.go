package execlog

import (
	"testing"
	"time"
)

/*
The exec timestamp is when the kernel saw the exec, not when this process
decoded it.

Those differ exactly when it matters: a burst of executions backs the ring
buffer up, the consumer falls behind, and every event in the backlog used to be
stamped with the moment it reached the decoder. An auditor reading the command
list back got an order and a spacing the session never had.
*/

// A kernel reading becomes a real time, offset from the reference by the same
// amount the kernel's clock advanced.
func TestAKernelReadingBecomesWallClockTime(t *testing.T) {
	wall := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	c := monoClock{mono: 1_000_000_000, wall: wall}

	for _, tc := range []struct {
		name  string
		ktime uint64
		want  time.Time
	}{
		{"at the reference", 1_000_000_000, wall},
		{"250ms later", 1_250_000_000, wall.Add(250 * time.Millisecond)},
		{"an hour later", 1_000_000_000 + uint64(time.Hour), wall.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.at(tc.ktime); !got.Equal(tc.want) {
				t.Errorf("at(%d) = %s, want %s", tc.ktime, got, tc.want)
			}
		})
	}
}

// Readings the clock cannot place return the zero time, so the caller can see
// the reference is missing rather than be handed a plausible wrong answer.
func TestAnUnplaceableReadingIsZeroRatherThanWrong(t *testing.T) {
	wall := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		clock monoClock
		ktime uint64
	}{
		{"no reference was read", monoClock{}, 1_500_000_000},
		{"the event carries no stamp", monoClock{mono: 1e9, wall: wall}, 0},
		{"a reading from before the reference", monoClock{mono: 2e9, wall: wall}, 1e9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.clock.at(tc.ktime); !got.IsZero() {
				t.Errorf("at(%d) = %s, want the zero time", tc.ktime, got)
			}
		})
	}
}

// Decoding uses the kernel's stamp, and a backlog does not move it.
func TestDecodeStampsTheKernelTimeNotTheDecodeTime(t *testing.T) {
	wall := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	c := monoClock{mono: 1_000_000_000, wall: wall}

	var raw rawEvent
	raw.Ktime = 1_000_000_000 + uint64(3*time.Second)
	copy(raw.Session[:], "659bc048338d4ed2c3dd1e97ace98932")
	copy(raw.Comm[:], "bash")
	copy(raw.Filename[:], "/usr/bin/bash")

	e, err := decodeEvent(buildRecord(t, raw, []byte("bash\x00")), c)
	if err != nil {
		t.Fatal(err)
	}
	want := wall.Add(3 * time.Second)
	if !e.At.Equal(want) {
		t.Errorf("At = %s, want %s -- the decode happened now, the exec did not",
			e.At, want)
	}
	// Decoding the same bytes later must not move it.
	time.Sleep(5 * time.Millisecond)
	again, err := decodeEvent(buildRecord(t, raw, []byte("bash\x00")), c)
	if err != nil {
		t.Fatal(err)
	}
	if !again.At.Equal(e.At) {
		t.Errorf("the same event decoded twice got two times, %s then %s",
			e.At, again.At)
	}
}

// Without a reference it still produces a time, because a recording with a
// zero timestamp in it is worse than one that is merely late.
func TestWithoutAReferenceDecodeStillStampsSomething(t *testing.T) {
	var raw rawEvent
	raw.Ktime = 1_500_000_000
	copy(raw.Session[:], "659bc048338d4ed2c3dd1e97ace98932")

	e, err := decodeEvent(buildRecord(t, raw, nil), monoClock{})
	if err != nil {
		t.Fatal(err)
	}
	if e.At.IsZero() {
		t.Error("At is the zero time with no clock reference; the fallback to " +
			"decode time is the point")
	}
}

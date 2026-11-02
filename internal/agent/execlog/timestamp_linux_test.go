//go:build linux

package execlog

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// The stamp on a real event is a real time.
//
// The unit tests feed monoClock invented readings, which proves the arithmetic
// and nothing about the clock. If the probe stamped a different clock, or the
// units were off by a thousand, those would still pass and every recording
// would carry a confident wrong time. This reads what the kernel actually
// wrote.
func TestAStampedExecutionCarriesARealTime(t *testing.T) {
	probe := openProbe(t)
	p, ok := probe.(*linuxProbe)
	if !ok {
		t.Skip("not the linux probe")
	}
	if p.clock.wall.IsZero() {
		t.Fatal("no monotonic reference was read at open; every timestamp " +
			"would silently fall back to decode time")
	}
	session := "timestamp-check-0001"
	pid := kernelPID(t, probe)
	if err := p.Track(pid, session); err != nil {
		t.Fatal(err)
	}
	defer p.Untrack(pid)

	marker := "timestamp-probe-4d1f"
	before := time.Now().UTC()
	if err := exec.Command("/bin/echo", marker).Run(); err != nil {
		t.Fatal(err)
	}

	got := collect(t, probe, session, 5*time.Second, func(g []Exec) bool {
		return hasCommand(g, marker)
	})
	after := time.Now().UTC()

	var found *Exec
	for i := range got {
		if hasCommand(got[i:i+1], marker) {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the marked execution was not captured; saw: %s", render(got))
	}

	t.Logf("exec stamped %s; the window was %s .. %s", found.At, before, after)

	// Generous either side: this is checking the clock is the right one and the
	// units are nanoseconds, not measuring latency.
	if found.At.Before(before.Add(-2 * time.Second)) {
		t.Errorf("stamped %s, before the command was run (%s) -- wrong clock "+
			"or wrong units", found.At, before)
	}
	if found.At.After(after.Add(2 * time.Second)) {
		t.Errorf("stamped %s, after the command finished (%s) -- wrong clock "+
			"or wrong units", found.At, after)
	}
	if found.At.Location() != time.UTC {
		t.Errorf("stamped in %s, not UTC", found.At.Location())
	}
	_ = os.Getpid()
}

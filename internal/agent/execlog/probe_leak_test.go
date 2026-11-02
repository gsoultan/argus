//go:build linux

package execlog

import (
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

// countTracked reports how many pids the probe is currently attributing.
func countTracked(t *testing.T, p *linuxProbe) int {
	t.Helper()
	var key uint32
	var val [sessionLen]byte
	n := 0
	it := p.tracked.Iterate()
	for it.Next(&key, &val) {
		n++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterating tracked map: %v", err)
	}
	return n
}

// Threads a tracked process creates must not accumulate in the tracked map.
//
// handle_fork inserts ctx->child_pid. For a thread clone that is a tid, not a
// tgid. handle_exit only deletes when tgid == tid, so a thread's entry would
// never be removed -- and handle_exec only ever looks up by tgid, so the entry
// was never useful either. The map is bounded at MAX_TRACKED; once it fills,
// bpf_map_update_elem fails and new children stop being tracked, which means
// execve evidence stops with no failure anyone sees.
func TestThreadsDoNotAccumulateInTheTrackedMap(t *testing.T) {
	probe := openProbe(t)
	p, ok := probe.(*linuxProbe)
	if !ok {
		t.Skip("not the linux probe")
	}
	session := "leak-check-0001"
	pid := kernelPID(t, probe)
	if err := p.Track(pid, session); err != nil {
		t.Fatal(err)
	}
	defer p.Untrack(pid)

	// Let any startup churn settle before taking the baseline.
	time.Sleep(300 * time.Millisecond)
	before := countTracked(t, p)

	const threads = 64
	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()
	runtime.GC()
	time.Sleep(500 * time.Millisecond)

	after := countTracked(t, p)
	t.Logf("tracked entries: %d before, %d after %d threads came and went",
		before, after, threads)

	// A little slack for threads the runtime keeps parked; the failure this is
	// looking for is linear growth in threads created.
	if after > before+threads/4 {
		t.Errorf("tracked grew from %d to %d across %d thread lifetimes -- "+
			"entries for exited threads are never deleted, so the map fills and "+
			"new children stop being tracked", before, after, threads)
	}

	// Tracking must still work, or the assertion above could pass by the probe
	// being broken rather than tidy.
	marker := "after-leak-check-9f2a"
	if err := exec.Command("/bin/echo", marker).Run(); err != nil {
		t.Fatal(err)
	}
	got := collect(t, probe, session, 5*time.Second, func(g []Exec) bool {
		return hasCommand(g, marker)
	})
	if !hasCommand(got, marker) {
		t.Errorf("tracking was lost; commands seen: %s", render(got))
	}
}

// A drop is counted, not just logged.
//
// The events channel holds 1024. Nothing drains it here, so once it fills the
// probe discards rather than blocking the ring buffer reader -- the right
// choice, since blocking makes the kernel drop far more. What was missing is
// that the discard was invisible past a log line: the session went on claiming
// eBPF fidelity, which asserts every execve is in the recording.
func TestDroppedExecutionsAreCounted(t *testing.T) {
	probe := openProbe(t)
	p, ok := probe.(*linuxProbe)
	if !ok {
		t.Skip("not the linux probe")
	}
	session := "drop-count-0001"
	pid := kernelPID(t, probe)
	if err := p.Track(pid, session); err != nil {
		t.Fatal(err)
	}
	defer p.Untrack(pid)

	if got, known := p.Drops(session); got != 0 || !known {
		t.Fatalf("a fresh session starts with %d drops (known=%v), want 0 and known", got, known)
	}

	// Deliberately drain nothing. 1024 is the channel; go well past it.
	for i := 0; i < 2500; i++ {
		_ = exec.Command("/bin/true").Run()
	}
	time.Sleep(500 * time.Millisecond)

	got, known := p.Drops(session)
	if !known {
		t.Fatal("the drop count could not be read")
	}
	t.Logf("drops recorded: %d", got)
	if got == 0 {
		t.Error("executions were discarded with the channel full and the " +
			"counter stayed at zero -- the session would report eBPF fidelity " +
			"for a recording that is missing executions")
	}

	// Forget releases it, or the bounded map fills the way tracked used to.
	p.Forget(session)
	if got, _ := p.Drops(session); got != 0 {
		t.Errorf("Drops = %d after Forget, want 0", got)
	}
}

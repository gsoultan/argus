//go:build linux

package execlog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// openProbe loads the real probe or skips.
//
// Skipping rather than failing because most machines cannot run this: the
// kernel needs BTF for CO-RE and CONFIG_BPF_EVENTS for tracepoint attachment,
// and loading needs privileges. Where those hold, this is the only test that
// exercises the kernel side at all — everything else in the package tests the
// code around it.
func openProbe(t *testing.T) Probe {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("loading a BPF program requires root")
	}
	// These tests necessarily run wherever the test binary runs, which may be a
	// container. The probe refuses a private PID namespace because the agent
	// would silently record no commands there; the tests instead opt in and
	// translate, via kernelPID below.
	t.Setenv("ARGUS_EXECLOG_ALLOW_PIDNS", "1")
	p, err := Open(context.Background(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		if IsUnsupported(err) {
			t.Skipf("this kernel cannot run the probe: %v", err)
		}
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// kernelPID returns this process's id as the kernel reports it.
//
// Inside a PID namespace that differs from os.Getpid(), and nothing in /proc
// exposes the translation from the inside. The probe itself can answer: track
// every plausible id, run a command, and read the parent from the event that
// comes back. Scaffolding for these tests only — the agent runs in the host
// namespace, where the two are the same number.
func kernelPID(t *testing.T, p Probe) int {
	t.Helper()
	if ns, err := os.Readlink("/proc/self/ns/pid"); err == nil && ns == "pid:[4026531836]" {
		return os.Getpid() // host namespace: no translation needed
	}

	const bootstrap = "bootstrap00000000000000000000001"
	const limit = 10000
	for pid := 1; pid <= limit; pid++ {
		if err := p.Track(pid, bootstrap); err != nil {
			t.Fatalf("bootstrap track: %v", err)
		}
	}
	defer func() {
		for pid := 1; pid <= limit; pid++ {
			p.Untrack(pid)
		}
	}()

	marker := "kernel-pid-bootstrap"
	if err := exec.Command("/bin/echo", marker).Run(); err != nil {
		t.Fatalf("bootstrap command: %v", err)
	}
	got := collect(t, p, bootstrap, 5*time.Second, func(g []Exec) bool {
		return hasCommand(g, marker)
	})
	i := slices.IndexFunc(got, func(e Exec) bool {
		return strings.Contains(e.CommandLine(), marker)
	})
	if i < 0 {
		t.Fatalf("could not resolve the kernel view of this process; commands seen: %s",
			render(got))
	}
	if got[i].PPID == 0 {
		t.Fatal("the bootstrap event carried no parent id")
	}
	return got[i].PPID
}

// collect drains events for a session until timeout or until stop says enough.
func collect(t *testing.T, p Probe, session string, d time.Duration, stop func([]Exec) bool) []Exec {
	t.Helper()
	var got []Exec
	deadline := time.After(d)
	for {
		select {
		case e, open := <-p.Events():
			if !open {
				return got
			}
			if e.SessionID != session {
				continue
			}
			got = append(got, e)
			if stop != nil && stop(got) {
				return got
			}
		case <-deadline:
			return got
		}
	}
}

func hasCommand(got []Exec, substr string) bool {
	return slices.ContainsFunc(got, func(e Exec) bool {
		return strings.Contains(e.CommandLine(), substr)
	})
}

// The claim the whole tier rests on: a command whose intent is invisible in
// terminal output is fully visible here. A PTY recording of this shows an
// opaque base64 blob and a shell prompt.
func TestProbeCapturesAnObfuscatedCommand(t *testing.T) {
	p := openProbe(t)

	const session = "kerneltest0000000000000000000001"
	// Tracking this test process means the commands it runs are its
	// descendants, exactly as a shim's are.
	self := kernelPID(t, p)
	if err := p.Track(self, session); err != nil {
		t.Fatalf("Track: %v", err)
	}
	defer p.Untrack(self)

	// echo cm0gLXJmIC92YXIvbG9nCg== | base64 -d  ->  "rm -rf /var/log"
	const blob = "cm0gLXJmIC92YXIvbG9nCg=="
	cmd := exec.Command("/bin/sh", "-c", "echo "+blob+" | base64 -d >/dev/null")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running the command under test: %v", err)
	}

	got := collect(t, p, session, 5*time.Second, func(g []Exec) bool {
		return hasCommand(g, blob) && hasCommand(g, "base64")
	})

	if !hasCommand(got, blob) {
		t.Errorf("the encoded payload was never reported; commands seen: %s", render(got))
	}
	// base64 is a descendant of the shell, not of this process directly, so
	// seeing it proves inheritance rather than just direct attribution.
	if !hasCommand(got, "base64") {
		t.Errorf("a grandchild process was not attributed; commands seen: %s", render(got))
	}
}

// Inheritance happens at fork, which is what makes a re-parented process keep
// its attribution. Walking parents at exec time would lose exactly this case —
// and a process deliberately detached from its shell is the one worth catching.
func TestProbeFollowsAReparentedProcess(t *testing.T) {
	p := openProbe(t)

	const session = "kerneltest0000000000000000000002"
	self := kernelPID(t, p)
	if err := p.Track(self, session); err != nil {
		t.Fatalf("Track: %v", err)
	}
	defer p.Untrack(self)

	// The outer shell exits immediately, orphaning the inner one; by the time
	// it execs, its parent is init.
	marker := "reparented-marker-9f2c"
	cmd := exec.Command("/bin/sh", "-c",
		"( setsid /bin/sh -c 'exec /bin/echo "+marker+"' >/dev/null 2>&1 & ) ; exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running the command under test: %v", err)
	}

	got := collect(t, p, session, 5*time.Second, func(g []Exec) bool {
		return hasCommand(g, marker)
	})
	if !hasCommand(got, marker) {
		t.Errorf("a re-parented process was not attributed to its session; commands seen: %s",
			render(got))
	}
}

// Events must reach only the session that was tracked. A process outside any
// tracked tree contributes nothing, and a session id nobody registered receives
// nothing — without both, evidence could not be attributed to a person.
func TestProbeAttributesOnlyTrackedTrees(t *testing.T) {
	p := openProbe(t)

	const tracked = "kerneltest0000000000000000000003"
	const neverTracked = "kerneltest0000000000000000000099"

	self := kernelPID(t, p)
	if err := p.Track(self, tracked); err != nil {
		t.Fatalf("Track: %v", err)
	}
	defer p.Untrack(self)

	marker := "attribution-marker-4a71"
	if err := exec.Command("/bin/echo", marker).Run(); err != nil {
		t.Fatal(err)
	}

	got := collect(t, p, tracked, 5*time.Second, func(g []Exec) bool {
		return hasCommand(g, marker)
	})
	if !hasCommand(got, marker) {
		t.Fatalf("the tracked command was not reported; commands seen: %s", render(got))
	}

	// Nothing should ever surface under an id that was never registered. The
	// probe stamps the session from the map, so a leak here would mean the
	// kernel side invented an attribution.
	stray := collect(t, p, neverTracked, 2*time.Second, nil)
	if len(stray) != 0 {
		t.Errorf("events arrived for a session that was never tracked: %s", render(stray))
	}
}

// A tracked process that exits must leave the map, or a recycled PID would
// eventually attribute a stranger's process to a finished session.
func TestProbeStopsAfterUntrack(t *testing.T) {
	p := openProbe(t)

	const session = "kerneltest0000000000000000000004"
	self := kernelPID(t, p)
	if err := p.Track(self, session); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := exec.Command("/bin/echo", "before-untrack").Run(); err != nil {
		t.Fatal(err)
	}
	before := collect(t, p, session, 3*time.Second, func(g []Exec) bool {
		return hasCommand(g, "before-untrack")
	})
	if !hasCommand(before, "before-untrack") {
		t.Fatalf("tracking was not working to begin with; commands seen: %s", render(before))
	}

	p.Untrack(self)
	if err := exec.Command("/bin/echo", "after-untrack").Run(); err != nil {
		t.Fatal(err)
	}
	after := collect(t, p, session, 2*time.Second, nil)
	if hasCommand(after, "after-untrack") {
		t.Error("commands were still attributed after the session was detached")
	}
}

// argv is read from the process's own memory, which is the part most likely to
// be rejected by the verifier or to come back empty.
func TestProbeReportsFullArgv(t *testing.T) {
	p := openProbe(t)

	const session = "kerneltest0000000000000000000005"
	self := kernelPID(t, p)
	if err := p.Track(self, session); err != nil {
		t.Fatalf("Track: %v", err)
	}
	defer p.Untrack(self)

	if err := exec.Command("/bin/echo", "alpha", "beta gamma", "").Run(); err != nil {
		t.Fatal(err)
	}

	got := collect(t, p, session, 5*time.Second, func(g []Exec) bool {
		return hasCommand(g, "alpha")
	})
	i := slices.IndexFunc(got, func(e Exec) bool { return strings.Contains(e.Filename, "echo") })
	if i < 0 {
		t.Fatalf("the exec was not reported; commands seen: %s", render(got))
	}
	e := got[i]
	if len(e.Args) < 4 {
		t.Fatalf("argv = %q, want four elements including the empty one", e.Args)
	}
	if e.Args[1] != "alpha" || e.Args[2] != "beta gamma" || e.Args[3] != "" {
		t.Errorf("argv = %q", e.Args)
	}
	// Quoting is what keeps `beta gamma` from reading as two arguments.
	if !strings.Contains(e.CommandLine(), "'beta gamma'") {
		t.Errorf("CommandLine = %q, does not disambiguate the quoted argument", e.CommandLine())
	}
	if e.PID == 0 || e.PPID == 0 {
		t.Errorf("pid/ppid = %d/%d, want both populated", e.PID, e.PPID)
	}
}

func render(got []Exec) string {
	if len(got) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, e := range got {
		b.WriteString("\n    ")
		b.WriteString(e.CommandLine())
	}
	return b.String()
}

// Tracking must survive a process churning through threads.
//
// A regression guard, not a proof. sched_process_exit fires per thread while
// the tracked map is keyed by thread group, so deleting on any thread exit is
// wrong by construction — but Go parks locked threads rather than destroying
// them, and this does not manage to produce a real thread exit. The guard in
// handle_exit is reasoned from the tracepoint semantics; it is not demonstrated
// by this test, and removing it does not make this test fail.
func TestProbeSurvivesThreadExits(t *testing.T) {
	p := openProbe(t)

	const session = "kerneltest0000000000000000000006"
	self := kernelPID(t, p)
	if err := p.Track(self, session); err != nil {
		t.Fatalf("Track: %v", err)
	}
	defer p.Untrack(self)

	// LockOSThread ties each goroutine to its own OS thread, which the runtime
	// destroys when the goroutine returns. That is a thread exit with a tid
	// that differs from the tgid.
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()
	runtime.GC()
	time.Sleep(200 * time.Millisecond)

	marker := "after-thread-churn-7b3e"
	if err := exec.Command("/bin/echo", marker).Run(); err != nil {
		t.Fatal(err)
	}

	got := collect(t, p, session, 5*time.Second, func(g []Exec) bool {
		return hasCommand(g, marker)
	})
	if !hasCommand(got, marker) {
		t.Errorf("tracking was lost after threads exited; commands seen: %s", render(got))
	}
}

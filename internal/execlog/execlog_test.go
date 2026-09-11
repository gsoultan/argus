package execlog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeProbe stands in for the kernel so the tier above it can be exercised
// anywhere. The real probe needs a kernel with BTF and BPF tracepoint support;
// none of the logic under test does.
type fakeProbe struct {
	mu       sync.Mutex
	tracked  map[int]string
	untracks []int
	events   chan Exec
	trackErr error
	closed   bool
	drops    map[string]uint64
	// dropsUnreadable stands in for a probe that cannot read its own counter.
	dropsUnreadable bool
}

func newFakeProbe() *fakeProbe {
	return &fakeProbe{
		tracked: map[int]string{},
		events:  make(chan Exec, 64),
		drops:   map[string]uint64{},
	}
}

func (p *fakeProbe) Drops(sessionID string) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dropsUnreadable {
		return 0, false
	}
	return p.drops[sessionID], true
}

func (p *fakeProbe) Forget(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.drops, sessionID)
}

func (p *fakeProbe) Track(pid int, sessionID string) error {
	if p.trackErr != nil {
		return p.trackErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tracked[pid] = sessionID
	return nil
}

func (p *fakeProbe) Untrack(pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tracked, pid)
	p.untracks = append(p.untracks, pid)
}

func (p *fakeProbe) Events() <-chan Exec { return p.events }

func (p *fakeProbe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.events)
	}
	return nil
}

func (p *fakeProbe) emit(e Exec) { p.events <- e }

func TestExecsReachTheirSession(t *testing.T) {
	p := newFakeProbe()
	tr := NewTracer(p)
	defer tr.Close()

	got := make(chan Exec, 8)
	if err := tr.Attach("sess-1", 4242, func(e Exec) { got <- e }); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	p.emit(Exec{SessionID: "sess-1", PID: 4300, Filename: "/usr/bin/id"})
	select {
	case e := <-got:
		if e.Filename != "/usr/bin/id" {
			t.Errorf("Filename = %q", e.Filename)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the exec never reached the session sink")
	}

	p.mu.Lock()
	if p.tracked[4242] != "sess-1" {
		t.Errorf("probe was not asked to track the session leader: %v", p.tracked)
	}
	p.mu.Unlock()
}

// Two sessions on one host must not see each other's commands. This is the
// property that makes per-session kernel evidence usable as evidence at all.
func TestSessionsDoNotSeeEachOther(t *testing.T) {
	p := newFakeProbe()
	tr := NewTracer(p)
	defer tr.Close()

	a := make(chan Exec, 8)
	b := make(chan Exec, 8)
	_ = tr.Attach("sess-a", 100, func(e Exec) { a <- e })
	_ = tr.Attach("sess-b", 200, func(e Exec) { b <- e })

	p.emit(Exec{SessionID: "sess-a", Filename: "/bin/cat"})
	p.emit(Exec{SessionID: "sess-b", Filename: "/bin/rm"})

	select {
	case e := <-a:
		if e.Filename != "/bin/cat" {
			t.Errorf("session a received %q, which belongs to another session", e.Filename)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session a got nothing")
	}
	select {
	case e := <-b:
		if e.Filename != "/bin/rm" {
			t.Errorf("session b received %q", e.Filename)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session b got nothing")
	}
}

// An exec for a session that has been sealed means the kernel saw activity the
// recording does not contain. Counting it is the point; discarding it silently
// would hide exactly that.
func TestExecsForSealedSessionsAreCounted(t *testing.T) {
	p := newFakeProbe()
	tr := NewTracer(p)
	defer tr.Close()

	_ = tr.Attach("sess-1", 1, func(Exec) {})
	tr.Detach("sess-1", 1)

	p.emit(Exec{SessionID: "sess-1", Filename: "/bin/sh"})

	deadline := time.After(2 * time.Second)
	for tr.Late() == 0 {
		select {
		case <-deadline:
			t.Fatal("an exec after the session was sealed went unrecorded")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestAttachRollsBackWhenTheProbeRefuses(t *testing.T) {
	p := newFakeProbe()
	p.trackErr = errors.New("map is full")
	tr := NewTracer(p)
	defer tr.Close()

	if err := tr.Attach("sess-1", 1, func(Exec) {}); err == nil {
		t.Fatal("Attach reported success when the probe refused to track")
	}

	// The sink must not be left registered, or later execs from a recycled PID
	// would be attributed to a session that was never really traced.
	p.emit(Exec{SessionID: "sess-1", Filename: "/bin/sh"})
	deadline := time.After(2 * time.Second)
	for tr.Late() == 0 {
		select {
		case <-deadline:
			t.Fatal("a stale sink survived a failed Attach")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestAttachRejectsIncompleteRegistration(t *testing.T) {
	tr := NewTracer(newFakeProbe())
	defer tr.Close()

	if err := tr.Attach("", 1, func(Exec) {}); err == nil {
		t.Error("Attach accepted an empty session id")
	}
	if err := tr.Attach("sess-1", 1, nil); err == nil {
		t.Error("Attach accepted a nil sink")
	}
}

/* ── Fidelity and fallback ───────────────────────────────────────────────── */

// The most important behaviour in the package. A host without kernel tracing
// must report pty, not ebpf: a recording that claimed kernel evidence it never
// had would be a lie in the evidence chain, and worse than no tier at all.
func TestFidelityNeverOverclaims(t *testing.T) {
	var absent *Context
	if got := absent.Fidelity(); got != FidelityPTY {
		t.Errorf("nil context fidelity = %q, want %q", got, FidelityPTY)
	}

	failed := Start(context.Background(), func(context.Context) (Probe, error) {
		return nil, Unsupported("kernel lacks BTF")
	})
	if got := failed.Fidelity(); got != FidelityPTY {
		t.Errorf("failed probe fidelity = %q, want %q", got, FidelityPTY)
	}
	if failed.Reason == "" {
		t.Error("no reason recorded; an auditor cannot tell why evidence is missing")
	}

	ok := Start(context.Background(), func(context.Context) (Probe, error) {
		return newFakeProbe(), nil
	})
	defer ok.Tracer.Close()
	if got := ok.Fidelity(); got != FidelityEBPF {
		t.Errorf("working probe fidelity = %q, want %q", got, FidelityEBPF)
	}
}

// Attach and Detach on a host with no probe must be harmless, so the agent has
// one code path rather than a condition at every call site.
func TestContextWithoutProbeIsInert(t *testing.T) {
	c := Start(context.Background(), func(context.Context) (Probe, error) {
		return nil, Unsupported("no tracepoint support")
	})
	if err := c.Attach("sess-1", 1, func(Exec) {}); err != nil {
		t.Errorf("Attach on a probeless context returned %v, want nil", err)
	}
	c.Detach("sess-1", 1) // must not panic

	var nilCtx *Context
	if err := nilCtx.Attach("sess-1", 1, func(Exec) {}); err != nil {
		t.Errorf("Attach on a nil context returned %v", err)
	}
	nilCtx.Detach("sess-1", 1)
}

// "This kernel cannot" and "it broke" want different operator responses, so the
// two must stay distinguishable.
func TestUnsupportedIsDistinguishableFromFailure(t *testing.T) {
	if !IsUnsupported(Unsupported("kernel too old: %d", 4)) {
		t.Error("Unsupported was not recognised")
	}
	if IsUnsupported(errors.New("permission denied")) {
		t.Error("an ordinary error was reported as unsupported")
	}
	if got := Unsupported("kernel too old: %d", 4).Error(); got !=
		"kernel execution tracing unavailable: kernel too old: 4" {
		t.Errorf("Error() = %q", got)
	}
}

/* ── Rendering ───────────────────────────────────────────────────────────── */

// An unquoted rendering of `sh -c "rm -rf /"` reads as three harmless words.
// The whole point of this tier is that the recorded command is what ran.
func TestCommandLineIsUnambiguous(t *testing.T) {
	cases := []struct {
		name string
		e    Exec
		want string
	}{
		{
			"plain",
			Exec{Args: []string{"ls", "-la", "/tmp"}},
			"ls -la /tmp",
		},
		{
			"argument containing spaces",
			Exec{Args: []string{"sh", "-c", "rm -rf /"}},
			"sh -c 'rm -rf /'",
		},
		{
			"argument containing a quote",
			Exec{Args: []string{"sh", "-c", "echo 'hi'"}},
			`sh -c 'echo '\''hi'\'''`,
		},
		{
			"empty argument",
			Exec{Args: []string{"cmd", ""}},
			"cmd ''",
		},
		{
			"no argv falls back to the filename",
			Exec{Filename: "/usr/bin/whoami"},
			"/usr/bin/whoami",
		},
		{
			"truncation is visible in the line itself",
			Exec{Args: []string{"python3", "-c"}, Truncated: true},
			"python3 -c …[truncated]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.CommandLine(); got != tc.want {
				t.Errorf("CommandLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCloseIsIdempotentAndStopsDispatch(t *testing.T) {
	p := newFakeProbe()
	tr := NewTracer(p)

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := tr.Attach("sess-1", 1, func(Exec) {}); err == nil {
		t.Error("Attach succeeded on a closed tracer")
	}
}

// An execution that arrives after Detach has to reach the fidelity decision.
//
// It was counted in a global field that only these tests read, so the session
// it belonged to never learned about it: the recording sealed and went out
// claiming eBPF fidelity -- that every execve is in the file -- while missing
// a command the kernel had reported. Per-session, and collected by the code
// that decides what the artefact can evidence.
func TestAnExecutionAfterDetachIsChargedToItsSession(t *testing.T) {
	p := newFakeProbe()
	tr := NewTracer(p)
	defer tr.Close()

	var seen int
	if err := tr.Attach("sess-late", 111, func(Exec) { seen++ }); err != nil {
		t.Fatal(err)
	}
	tr.Detach("sess-late", 111)

	// The kernel is still reporting descendants of a session that has gone.
	p.emit(Exec{SessionID: "sess-late", Filename: "/usr/bin/whoami"})
	p.emit(Exec{SessionID: "sess-late", Filename: "/usr/bin/id"})
	for tr.Late() < 2 {
		time.Sleep(time.Millisecond)
	}

	if got, known := tr.TakeLost("sess-late"); got != 2 || !known {
		t.Errorf("TakeLost = %d, want 2 -- the session that lost these "+
			"executions is the one whose fidelity has to change", got)
	}
	// Taken once: a second call must not charge them again.
	if got, _ := tr.TakeLost("sess-late"); got != 0 {
		t.Errorf("TakeLost = %d on a second call, want 0", got)
	}
	// Another session is unaffected by its neighbour's losses.
	if got, _ := tr.TakeLost("sess-other"); got != 0 {
		t.Errorf("an unrelated session reports %d lost, want 0", got)
	}
	if seen != 0 {
		t.Errorf("the detached sink still received %d executions", seen)
	}
}

// Probe-side drops and post-detach arrivals are one number to the caller.
func TestTakeLostSumsBothSourcesOfLoss(t *testing.T) {
	p := newFakeProbe()
	tr := NewTracer(p)
	defer tr.Close()

	if err := tr.Attach("sess-both", 222, func(Exec) {}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.drops["sess-both"] = 3
	p.mu.Unlock()

	tr.Detach("sess-both", 222)
	p.emit(Exec{SessionID: "sess-both", Filename: "/usr/bin/whoami"})
	for tr.Late() == 0 {
		time.Sleep(time.Millisecond)
	}

	if got, _ := tr.TakeLost("sess-both"); got != 4 {
		t.Errorf("TakeLost = %d, want 4 (3 dropped by the probe, 1 after detach)", got)
	}
}

// A count that could not be read is not a count of zero.
//
// Drops used to return a sentinel 1 on a failed map read, which the collector
// stored and logged as a fact -- indistinguishable from a real single loss. It
// says it does not know now, and the caller decides. For this product an
// unknown means the same as a loss: eBPF fidelity asserts every execve is in
// the file, and that is not a claim to make from a counter nobody could read.
func TestAnUnreadableDropCountIsNotZero(t *testing.T) {
	p := newFakeProbe()
	p.dropsUnreadable = true
	tr := NewTracer(p)
	defer tr.Close()

	if err := tr.Attach("sess-unknown", 333, func(Exec) {}); err != nil {
		t.Fatal(err)
	}
	tr.Detach("sess-unknown", 333)

	lost, known := tr.TakeLost("sess-unknown")
	if known {
		t.Fatal("a probe that cannot read its counter reported a known count")
	}
	if lost != 0 {
		t.Errorf("lost = %d on an unreadable count; the number is meaningless "+
			"and inventing one is what this replaced", lost)
	}
}

// A host with no probe reports a known zero, not an unknown.
//
// It never claimed eBPF fidelity, so there is no claim to undermine and no
// reason to degrade a recording that was only ever terminal output.
func TestNoProbeMeansNothingLostRatherThanUnknown(t *testing.T) {
	var c *Context
	if lost, known := c.TakeLost("sess-none"); lost != 0 || !known {
		t.Errorf("TakeLost on a nil context = (%d, %v), want (0, true)", lost, known)
	}
	empty := &Context{Reason: "kernel too old"}
	if lost, known := empty.TakeLost("sess-none"); lost != 0 || !known {
		t.Errorf("TakeLost with no tracer = (%d, %v), want (0, true)", lost, known)
	}
}

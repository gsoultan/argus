// Package execlog records the commands a session actually ran, as observed by
// the kernel rather than inferred from a terminal.
//
// A PTY recording shows what crossed the wire, which is not the same as what
// ran. A user can base64 a command, source a script whose body never appears on
// screen, or drive an editor that shells out. The console says as much on every
// session: the command timeline is "an audit aid, not proof". This is the tier
// that makes it proof.
//
// The kernel-side probe is Linux-only and needs a kernel built with BTF and BPF
// tracepoint support. Everything in this file is platform-independent so that
// correlation, ordering and the fallback path can be tested anywhere, and so
// that the untestable surface is confined to the probe itself.
package execlog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrUnsupported reports that kernel-observed execution is not available here.
//
// Carries a human-readable reason because the answer an operator needs is not
// "no" but "why not, and what do I do about it". A session that silently fell
// back to PTY-only while the console showed a kernel-evidence badge would be a
// lie in the evidence chain.
type ErrUnsupported struct {
	Reason string
}

func (e *ErrUnsupported) Error() string {
	return "kernel execution tracing unavailable: " + e.Reason
}

// Unsupported builds an ErrUnsupported.
func Unsupported(format string, args ...any) error {
	return &ErrUnsupported{Reason: fmt.Sprintf(format, args...)}
}

// IsUnsupported reports whether err means "this kernel cannot do it", as
// opposed to "it broke". The two want different operator responses: one is a
// platform fact, the other is an incident.
func IsUnsupported(err error) bool {
	var u *ErrUnsupported
	return errors.As(err, &u)
}

// Exec is one process execution the kernel observed.
type Exec struct {
	// SessionID ties this to a recorded session. Empty means the probe saw an
	// exec it could not attribute, which is dropped rather than reported
	// against the wrong session.
	SessionID string `json:"sessionId"`

	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	UID  uint32 `json:"uid"`

	// Comm is the kernel's short process name, capped at 16 bytes. Kept
	// alongside Filename because they disagree in the interesting cases — a
	// process that renamed itself is worth seeing.
	Comm string `json:"comm"`
	// Filename is the executable as the kernel resolved it.
	Filename string `json:"filename"`
	// Args is the full argument vector, argv[0] included.
	Args []string `json:"args"`
	// Truncated reports that the argument vector was longer than the capture
	// buffer. Presenting a cut-off command as though it were the whole thing is
	// exactly the kind of quiet inaccuracy this tier exists to remove.
	Truncated bool `json:"truncated,omitempty"`

	At time.Time `json:"at"`
}

// CommandLine renders the execution the way an auditor reads it.
func (e Exec) CommandLine() string {
	if len(e.Args) == 0 {
		if e.Truncated {
			// The arguments did not survive the kernel read. Returning the
			// filename alone would report a command that had none, which is a
			// different and more confident statement than the probe can make.
			return e.Filename + " …[truncated]"
		}
		return e.Filename
	}
	var b strings.Builder
	for i, a := range e.Args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quoteArg(a))
	}
	if e.Truncated {
		// Visible in the rendered line, not only in a field nobody reads.
		b.WriteString(" …[truncated]")
	}
	return b.String()
}

// quoteArg makes an argument unambiguous when read back.
//
// Arguments containing whitespace or quotes are the ones worth being careful
// about: an unquoted rendering of `sh -c "rm -rf /"` reads as three harmless
// words, which misrepresents what ran.
func quoteArg(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Probe is the kernel-side source of execution events.
//
// An interface so the agent can be built and tested on any platform, and so the
// part that cannot be exercised without a suitable kernel is one small
// implementation rather than a condition threaded through the agent.
type Probe interface {
	// Track begins attributing this process and its descendants to sessionID.
	Track(pid int, sessionID string) error
	// Untrack stops attributing a process. Descendants already running keep
	// their attribution until they exit.
	Untrack(pid int)
	// Events yields executions until the probe is closed.
	Events() <-chan Exec
	// Drops reports executions for this session that never reached a sink,
	// whichever side of the ring buffer lost them. Non-zero means the
	// recording cannot claim to list everything that ran.
	//
	// The second return says whether the count is known at all. A probe that
	// cannot read its own counter must not report zero, and must not invent a
	// number either: the caller decides what an unknown means, and for this
	// product it means the same as a loss.
	Drops(sessionID string) (uint64, bool)
	// Forget releases a session's drop counter. Separate from Untrack, which
	// is per-pid: the counter is per-session, and one that is never released
	// fills a bounded map the same way stray pids used to.
	Forget(sessionID string)
	// Close detaches from the kernel.
	Close() error
}

// Tracer distributes kernel-observed executions to per-session sinks.
//
// The probe attributes an exec to a session; this decides what to do with it.
// Kept separate so the fan-out, the ordering guarantees and the handling of
// events that arrive after a session has been sealed are all testable without a
// kernel.
type Tracer struct {
	probe Probe

	mu    sync.Mutex
	sinks map[string]func(Exec)
	// late counts executions that arrived for a session already sealed. A
	// non-zero count is worth surfacing rather than hiding: it means the
	// recording was closed while the session was still running commands.
	late   map[string]uint64
	closed bool

	done chan struct{}
	wg   sync.WaitGroup
}

// NewTracer starts consuming from the probe.
func NewTracer(p Probe) *Tracer {
	t := &Tracer{
		probe: p,
		sinks: map[string]func(Exec){},
		late:  map[string]uint64{},
		done:  make(chan struct{}),
	}
	t.wg.Add(1)
	go t.run()
	return t
}

func (t *Tracer) run() {
	defer t.wg.Done()
	events := t.probe.Events()
	for {
		select {
		case <-t.done:
			return
		case e, open := <-events:
			if !open {
				return
			}
			t.dispatch(e)
		}
	}
}

func (t *Tracer) dispatch(e Exec) {
	t.mu.Lock()
	sink, ok := t.sinks[e.SessionID]
	if !ok {
		// An exec for a session nobody is recording. Counting it is the point:
		// silently discarding would hide that the kernel saw activity the
		// recording does not contain.
		t.late[e.SessionID]++
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()

	// Outside the lock: a slow sink must not stall the probe's ring buffer, and
	// holding the lock across a callback invites a deadlock through Attach.
	sink(e)
}

// Attach registers a sink and begins tracking pid's process tree.
func (t *Tracer) Attach(sessionID string, pid int, sink func(Exec)) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if sink == nil {
		return errors.New("sink is required")
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errors.New("tracer is closed")
	}
	t.sinks[sessionID] = sink
	t.mu.Unlock()

	// Registered before tracking, so an exec cannot arrive between the kernel
	// starting to report and this process knowing where to put it.
	if err := t.probe.Track(pid, sessionID); err != nil {
		t.mu.Lock()
		delete(t.sinks, sessionID)
		t.mu.Unlock()
		return err
	}
	return nil
}

// Detach stops recording a session.
// Detach stops recording a session. It deliberately leaves the loss counters
// alone -- see TakeLost, which the caller must call once it has finished with
// the recording.
func (t *Tracer) Detach(sessionID string, pid int) {
	t.probe.Untrack(pid)
	t.mu.Lock()
	delete(t.sinks, sessionID)
	t.mu.Unlock()
}

// TakeLost reports every execution this session lost, and releases the counts.
//
// Call it as late as possible and exactly once: after the recording is closed,
// before deciding what the recording can evidence. The window matters. An exec
// that arrives between Detach and the seal finds no sink, and counting it
// anywhere the fidelity decision does not read is the same as discarding it --
// which is what a global counter only the tests looked at amounted to. The
// sealed record went out claiming eBPF fidelity, that every execve is in the
// file, while missing commands the kernel had reported.
func (t *Tracer) TakeLost(sessionID string) (lost uint64, known bool) {
	if t == nil || t.probe == nil {
		// No probe, no claim to undermine: this host never reported eBPF
		// fidelity, so "nothing lost" is the truth rather than an assumption.
		return 0, true
	}
	t.mu.Lock()
	lost = t.late[sessionID]
	delete(t.late, sessionID)
	t.mu.Unlock()

	dropped, known := t.probe.Drops(sessionID)
	lost += dropped
	t.probe.Forget(sessionID)
	return lost, known
}

// Late reports executions seen for sessions with no sink.
func (t *Tracer) Late() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, v := range t.late {
		n += int(v)
	}
	return n
}

// Close stops consuming and closes the probe.
func (t *Tracer) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.done)
	t.mu.Unlock()

	err := t.probe.Close()
	t.wg.Wait()
	return err
}

// Fidelity names how much of a session's activity was actually observed.
//
// Reported per session rather than configured globally: whether the kernel tier
// was working is a property of the evidence, not of the deployment, and a
// session recorded during an outage must not claim otherwise.
const (
	// FidelityPTY means terminal output only — what crossed the wire.
	FidelityPTY = "pty"
	// FidelityEBPF means terminal output plus kernel-observed execution.
	FidelityEBPF = "ebpf"
)

// Context carries a probe that may not exist.
//
// Nil is a valid, expected value: most hosts will run without kernel tracing
// until it is rolled out, and the agent must behave identically apart from the
// fidelity it claims.
type Context struct {
	Tracer *Tracer
	// Reason explains an absent tracer, and is reported with every session so
	// the console can say why a recording is PTY-only rather than leaving an
	// auditor to guess.
	Reason string
}

// Fidelity reports what this host can actually evidence.
func (c *Context) Fidelity() string {
	if c == nil || c.Tracer == nil {
		return FidelityPTY
	}
	return FidelityEBPF
}

// Attach wires a session to the tracer when one exists.
func (c *Context) Attach(sessionID string, pid int, sink func(Exec)) error {
	if c == nil || c.Tracer == nil {
		return nil
	}
	return c.Tracer.Attach(sessionID, pid, sink)
}

// Detach releases a session.
// TakeLost reports every execution this session lost, and releases the counts.
//
// Zero when there is no probe, because a host with no kernel tier never claimed
// eBPF fidelity to begin with. Call it once, after the recording is closed:
// see Tracer.TakeLost for why the timing is the point.
func (c *Context) TakeLost(sessionID string) (lost uint64, known bool) {
	if c == nil || c.Tracer == nil {
		return 0, true
	}
	return c.Tracer.TakeLost(sessionID)
}

func (c *Context) Detach(sessionID string, pid int) {
	if c == nil || c.Tracer == nil {
		return
	}
	c.Tracer.Detach(sessionID, pid)
}

// Start builds a Context, degrading loudly rather than silently.
//
// A kernel that cannot support the probe is a supported configuration; one that
// pretends to support it is not. Either way the caller gets a usable Context
// and a reason it can report.
func Start(ctx context.Context, open func(context.Context) (Probe, error)) *Context {
	p, err := open(ctx)
	if err != nil {
		return &Context{Reason: err.Error()}
	}
	return &Context{Tracer: NewTracer(p)}
}

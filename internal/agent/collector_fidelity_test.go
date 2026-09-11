package agent

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/execlog"
	"github.com/gsoultan/argus/internal/recorder"
)

/*
What an eBPF recording is allowed to claim.

"eBPF fidelity" is the strongest statement this product makes about an
artefact: every execve is in the file. Anything that can drop an execution
while leaving the claim standing turns the badge into decoration, and an
auditor reading a command list that silently omits one command is worse off
than one who was told the recording is terminal output only.
*/

// brokenWriter accepts the header and then behaves like a full disk.
type brokenWriter struct{ broken bool }

func (b *brokenWriter) Write(p []byte) (int, error) {
	if b.broken {
		return 0, errors.New("no space left on device")
	}
	return len(p), nil
}

func sessionOn(t *testing.T, w io.Writer) *activeSession {
	t.Helper()
	rec, err := recorder.New(w, recorder.Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatal(err)
	}
	return &activeSession{id: "sess-1", rec: rec}
}

func anExec(cmd string) execlog.Exec {
	return execlog.Exec{
		SessionID: "sess-1", PID: 4242, PPID: 1, UID: 0,
		Comm: cmd, Filename: "/usr/bin/" + cmd, Args: []string{cmd},
	}
}

// The healthy case: executions are counted and the claim stands.
func TestKernelExecutionsAreCountedAndKeepEBPFFidelity(t *testing.T) {
	var out strings.Builder
	s := sessionOn(t, &out)
	ebpf := &execlog.Context{Tracer: &execlog.Tracer{}}

	for _, c := range []string{"whoami", "id", "cat"} {
		s.recordExec(anExec(c), nil)
	}
	if got := s.commandCount(); got != 3 {
		t.Errorf("counted %d executions, want 3", got)
	}
	if got := s.fidelity(ebpf); got != execlog.FidelityEBPF {
		t.Errorf("fidelity = %q, want %q", got, execlog.FidelityEBPF)
	}
	if !strings.Contains(out.String(), "whoami") {
		t.Error("the recording does not contain the executions it counted")
	}
}

// One dropped execution ends the claim for the whole recording.
func TestADroppedKernelExecutionEndsTheEBPFClaim(t *testing.T) {
	w := &brokenWriter{}
	s := sessionOn(t, w)
	ebpf := &execlog.Context{Tracer: &execlog.Tracer{}}

	s.recordExec(anExec("whoami"), nil)
	if s.fidelity(ebpf) != execlog.FidelityEBPF {
		t.Fatal("precondition: a healthy session claims eBPF fidelity")
	}

	w.broken = true
	s.recordExec(anExec("curl"), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := s.fidelity(ebpf); got != execlog.FidelityPTY {
		t.Errorf("fidelity = %q after a dropped execution, want %q -- the "+
			"recording no longer lists everything that ran", got, execlog.FidelityPTY)
	}
	if got := s.commandCount(); got != 1 {
		t.Errorf("counted %d executions, want 1: the dropped one must not be counted", got)
	}
}

// A session the probe never attached to says so, whatever the host supports.
func TestAnUnattachedSessionDoesNotClaimTheHostsFidelity(t *testing.T) {
	s := sessionOn(t, &strings.Builder{})
	s.ptyOnly = true
	if got := s.fidelity(&execlog.Context{Tracer: &execlog.Tracer{}}); got != execlog.FidelityPTY {
		t.Errorf("fidelity = %q, want %q", got, execlog.FidelityPTY)
	}
}

// A host with no probe at all reports PTY, and nothing here changes that.
func TestNoProbeMeansPTY(t *testing.T) {
	s := sessionOn(t, &strings.Builder{})
	if got := s.fidelity(nil); got != execlog.FidelityPTY {
		t.Errorf("fidelity = %q with no tracer, want %q", got, execlog.FidelityPTY)
	}
}

// An execution that never arrived ends the claim just as one that arrived and
// could not be written.
//
// These fail differently and were handled differently. recordExec sets
// execLost when a kernel execution reaches the agent and the write fails. An
// execution the kernel dropped because its ring buffer was full, or that this
// process dropped because the consumer was behind, never reaches recordExec at
// all -- so nothing was set, and the session went on reporting eBPF fidelity
// with a hole in it. That claim is the strongest this product makes about an
// artefact: every execve is in the file.
func TestAnExecutionLostBeforeItArrivedEndsTheEBPFClaim(t *testing.T) {
	var out strings.Builder
	s := sessionOn(t, &out)
	ebpf := &execlog.Context{Tracer: &execlog.Tracer{}}

	s.recordExec(anExec("whoami"), nil)
	if s.fidelity(ebpf) != execlog.FidelityEBPF {
		t.Fatal("precondition: a healthy session claims eBPF fidelity")
	}

	// What the collector does when the probe reports losses at seal time.
	s.noteExecDropped(1)

	if got := s.fidelity(ebpf); got != execlog.FidelityPTY {
		t.Errorf("fidelity = %q after an execution was dropped, want %q -- the "+
			"recording is still complete terminal output, but it cannot stand "+
			"as a list of everything that ran", got, execlog.FidelityPTY)
	}
	// The output already written is still the record, and still chained.
	if !strings.Contains(out.String(), "whoami") {
		t.Error("degrading fidelity discarded the executions that did arrive")
	}
}

// Drops accumulate rather than replace, so two losses do not read as one.
func TestDroppedExecutionsAccumulate(t *testing.T) {
	var out strings.Builder
	s := sessionOn(t, &out)
	s.noteExecDropped(2)
	s.noteExecDropped(3)

	s.mu.Lock()
	got := s.execDropped
	s.mu.Unlock()
	if got != 5 {
		t.Errorf("execDropped = %d, want 5", got)
	}
}

// A failure to record must not copy the command line into the agent's log.
//
// argv is the thing this product is built to control: `mysql -p...`,
// `curl -H "Authorization: ..."`, a postgres:// URL with its password in it.
// The recording that carries it is access-controlled and hash-chained. The
// agent's log is neither, and it was getting the full command line on every
// write failure and every dropped event.
func TestAFailedRecordingDoesNotLogTheCommandLine(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))

	// Healthy at construction -- recorder.New writes the header -- then broken,
	// which is the sequence the existing failure tests use.
	w := &brokenWriter{}
	s := sessionOn(t, w)
	w.broken = true

	e := anExec("mysql")
	e.Args = []string{"mysql", "-u", "root", "-phunter2-the-password"}

	s.recordExec(e, log)

	out := logged.String()
	if out == "" {
		t.Fatal("nothing was logged; this test can no longer tell what reaches the log")
	}
	if strings.Contains(out, "hunter2-the-password") {
		t.Errorf("the agent log contains a credential from argv:\n  %s\n"+
			"it was captured into an access-controlled recording and copied "+
			"out of it into a log that is not", out)
	}
	// The diagnostic still has to be useful.
	if !strings.Contains(out, "/usr/bin/mysql") {
		t.Errorf("the log no longer says which program failed:\n  %s", out)
	}
}

// The recording itself still carries the full argument vector. Removing it
// from the log must not remove it from the evidence.
func TestTheRecordingStillCarriesTheFullArgv(t *testing.T) {
	var out strings.Builder
	s := sessionOn(t, &out)
	e := anExec("mysql")
	e.Args = []string{"mysql", "-u", "root", "-phunter2-the-password"}

	s.recordExec(e, nil)

	if !strings.Contains(out.String(), "hunter2-the-password") {
		t.Error("the recording does not contain the argument vector; the " +
			"evidence is what is supposed to hold it")
	}
}

// A session whose losses could not be counted does not claim eBPF fidelity.
//
// An unknown is not a zero. The claim is that every execve is in the file, and
// a counter nobody could read is no basis for making it.
func TestAnUncountableLossStillEndsTheEBPFClaim(t *testing.T) {
	var out strings.Builder
	s := sessionOn(t, &out)
	ebpf := &execlog.Context{Tracer: &execlog.Tracer{}}

	s.recordExec(anExec("whoami"), nil)
	if s.fidelity(ebpf) != execlog.FidelityEBPF {
		t.Fatal("precondition: a healthy session claims eBPF fidelity")
	}

	// What seal does when TakeLost reports it cannot tell.
	s.noteExecDropped(1)

	if got := s.fidelity(ebpf); got != execlog.FidelityPTY {
		t.Errorf("fidelity = %q when the loss count was unreadable, want %q",
			got, execlog.FidelityPTY)
	}
}

package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func ended(t time.Time) *time.Time { return &t }
func count(n int) *int             { return &n }

// eBPF fidelity is a claim about evidence. A finished session with real output
// and no observed commands cannot support it -- the probe stopped, or the
// report was altered. Either way it must not sit in the log looking like
// kernel evidence.
func TestFidelityUnsupportedFlagsAnEmptyEBPFSession(t *testing.T) {
	base := Session{
		ID: "s1", Fidelity: "ebpf", State: "closed",
		EndedAt: ended(time.Now()), RecordingBytes: 64_000,
	}
	if !fidelityUnsupported(base) {
		t.Error("eBPF with 64 kB of output and no commands must be flagged")
	}
	withZero := base
	withZero.CommandCount = count(0)
	if !fidelityUnsupported(withZero) {
		t.Error("an explicit zero command count is the same contradiction as none")
	}
}

func TestFidelityUnsupportedIsQuietWhenTheClaimHolds(t *testing.T) {
	cases := map[string]Session{
		"commands were observed": {
			Fidelity: "ebpf", State: "closed", EndedAt: ended(time.Now()),
			RecordingBytes: 64_000, CommandCount: count(12),
		},
		"pty never claimed kernel evidence": {
			Fidelity: "pty", State: "closed", EndedAt: ended(time.Now()),
			RecordingBytes: 64_000,
		},
		// A session in flight has legitimately observed nothing yet, and
		// flagging every start would train people to ignore this.
		"still running": {
			Fidelity: "ebpf", State: "active", RecordingBytes: 64_000,
		},
		"ended but state still active": {
			Fidelity: "ebpf", State: "active", EndedAt: ended(time.Now()),
			RecordingBytes: 64_000,
		},
		// A refused login or a session closed before the prompt drew.
		"almost no output": {
			Fidelity: "ebpf", State: "closed", EndedAt: ended(time.Now()),
			RecordingBytes: 200,
		},
	}
	for name, s := range cases {
		if fidelityUnsupported(s) {
			t.Errorf("%s: must not be flagged", name)
		}
	}
}

// The flag has to reach the tamper-evident log, not just the process log.
func TestFidelityContradictionIsAudited(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	e, err := s.AppendAudit(ctx, AuditEvent{
		Action: "session.fidelity_unsupported", Severity: "warning",
		ActorEmail: "u@x.id", Target: "db-01",
		Detail: "Session s1 reported eBPF fidelity but recorded 64000 bytes of " +
			"output with no kernel-observed commands.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Hash == "" || e.PrevHash == "" {
		t.Error("the contradiction must be chained like any other audit event")
	}
	if !strings.Contains(e.Detail, "no kernel-observed commands") {
		t.Errorf("detail should name the contradiction: %q", e.Detail)
	}
	v, err := s.VerifyAuditChain(ctx)
	if err != nil || !v.OK {
		t.Errorf("chain must remain intact after the flag: %v %+v", err, v)
	}
}

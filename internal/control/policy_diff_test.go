package control

import (
	"strings"
	"testing"
)

func TestDefaultGatewayPolicyIsClosed(t *testing.T) {
	p := DefaultGatewayPolicy()
	if p.AllowLocalForward || p.AllowRemoteForward || p.AllowAgentForward || p.AllowX11Forward {
		t.Error("no forwarding channel may be open by default")
	}
	if !p.ProxySftpSubsystem || !p.FailClosedOnRecordingLoss ||
		!p.RequireEbpfForRoot || !p.EncryptRecordingsSeparateKey {
		t.Error("every recording guarantee must be on by default")
	}
}

func TestDiffReportsOnlyWhatMoved(t *testing.T) {
	before := DefaultGatewayPolicy()
	after := before
	after.AllowAgentForward = true

	changes := DiffGatewayPolicy(before, after)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if !changes[0].Enabled || !changes[0].Loosening {
		t.Error("enabling agent forwarding is a loosening")
	}
	if len(DiffGatewayPolicy(before, before)) != 0 {
		t.Error("an unchanged policy must produce no changes")
	}
}

// Risk is a property of the position, not of the field. Switching SFTP
// proxying *off* loses evidence, so it loosens the gateway just as much as
// turning agent forwarding on does.
func TestTurningOffARecordingGuaranteeIsALoosening(t *testing.T) {
	before := DefaultGatewayPolicy()
	after := before
	after.ProxySftpSubsystem = false

	changes := DiffGatewayPolicy(before, after)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Enabled {
		t.Error("the field was disabled")
	}
	if !changes[0].Loosening {
		t.Error("disabling SFTP proxying loses per-file audit events — that is a loosening")
	}
}

func TestTighteningIsNotFlaggedAsLoosening(t *testing.T) {
	before := DefaultGatewayPolicy()
	before.AllowX11Forward = true
	after := before
	after.AllowX11Forward = false

	changes := DiffGatewayPolicy(before, after)
	if len(changes) != 1 || changes[0].Loosening {
		t.Fatalf("closing X11 forwarding is a tightening: %+v", changes)
	}
	_, loosened := describePolicyChanges(changes)
	if loosened {
		t.Error("a tightening must not raise the audit severity")
	}
}

func TestLooseningsSortFirst(t *testing.T) {
	before := DefaultGatewayPolicy()
	after := before
	after.AllowX11Forward = true    // loosening
	after.AllowLocalForward = false // unchanged
	before.AllowAgentForward = true
	after.AllowAgentForward = false // tightening

	changes := DiffGatewayPolicy(before, after)
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(changes))
	}
	if !changes[0].Loosening || changes[1].Loosening {
		t.Errorf("loosenings must sort first, got %+v", changes)
	}
}

func TestAuditDetailNamesTheConsequence(t *testing.T) {
	before := DefaultGatewayPolicy()
	after := before
	after.AllowAgentForward = true

	detail, loosened := describePolicyChanges(DiffGatewayPolicy(before, after))
	if !loosened {
		t.Error("expected the change to be flagged as loosening")
	}
	if !strings.Contains(detail, "SSH agent forwarding") {
		t.Errorf("detail should name the field: %q", detail)
	}
	// An auditor reading the entry needs to know why it matters, not just that
	// a boolean moved.
	if !strings.Contains(detail, "sign challenges") {
		t.Errorf("detail should state the consequence: %q", detail)
	}
}

func TestNoEffectiveChangeIsStillDescribed(t *testing.T) {
	p := DefaultGatewayPolicy()
	detail, loosened := describePolicyChanges(DiffGatewayPolicy(p, p))
	if loosened {
		t.Error("no change cannot be a loosening")
	}
	if detail == "" {
		t.Error("a no-op save is still an audit entry and needs a detail string")
	}
}

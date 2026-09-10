package control

import (
	"context"
	"strings"
	"testing"
)

/*
The role exemption and the kernel-evidence requirement.

Admins are exempt from needing an approved access request, because someone has
to be able to act when the approval chain itself is broken. The exemption also
clears the kernel-evidence requirement -- deliberately, since an admin sent to
repair a broken agent cannot be blocked by that agent being broken -- but it
returned before the requirement was read, so the record said only that a role
allowed the session. The waiver had to be inferred from the absence of a
refusal.

These tests do not touch gateway_policy. It is a single shared row, and a test
that flips it changes live configuration for the length of its run.
*/

// setExecTracing declares whether the host's agent can observe execution.
func setExecTracing(t *testing.T, s *Store, host string, tracing bool) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO agents (hostname, version, last_seen_at, exec_tracing, updated_at)
		VALUES ($1, 'test', now(), $2, now())
		ON CONFLICT (hostname) DO UPDATE SET exec_tracing = $2`, host, tracing); err != nil {
		t.Fatal(err)
	}
}

// An admin still gets root on a host with no kernel probe -- and the record
// says that is what happened.
func TestAnAdminsWaiverOfKernelEvidenceIsRecorded(t *testing.T) {
	a, s := authorizeAPI(t)
	email, _ := newAccount(t, s, "admin")
	host := unique("host") + ".example"
	setExecTracing(t, s, host, false)

	got := askAuthorize(t, a, email, host, "root")
	if !got.Allowed {
		t.Fatalf("an admin was refused root: %q -- the exemption exists so a "+
			"broken approval chain cannot lock everyone out", got.Reason)
	}
	if !got.KernelEvidenceWaived {
		t.Error("KernelEvidenceWaived is false on a host with no kernel probe; " +
			"the session record cannot flag what it was never told")
	}

	action, detail := lastElevationAudit(t, s, email)
	if action != "session.elevation_allowed" {
		t.Fatalf("audit action = %q, want session.elevation_allowed", action)
	}
	if !strings.Contains(detail, "waived") {
		t.Errorf("the audit line does not say the requirement was waived:\n  %s\n"+
			"an auditor should not have to infer it from the absence of a refusal", detail)
	}
	if !strings.Contains(detail, "cannot evidence what ran") {
		t.Errorf("the audit line does not say what the session cannot do:\n  %s", detail)
	}
}

// The same admin on a host that can produce kernel evidence waives nothing, and
// the line does not claim otherwise.
func TestNoWaiverIsClaimedWhenTheProbeWorks(t *testing.T) {
	a, s := authorizeAPI(t)
	email, _ := newAccount(t, s, "admin")
	host := unique("host") + ".example"
	setExecTracing(t, s, host, true)

	got := askAuthorize(t, a, email, host, "root")
	if !got.Allowed {
		t.Fatalf("an admin was refused root: %q", got.Reason)
	}
	if got.KernelEvidenceWaived {
		t.Error("claimed a waiver on a host whose agent reports kernel tracing; " +
			"a false waiver in the audit chain is worse than a missing one")
	}
	if _, detail := lastElevationAudit(t, s, email); strings.Contains(detail, "waived") {
		t.Errorf("the audit line claims a waiver that did not happen:\n  %s", detail)
	}
}

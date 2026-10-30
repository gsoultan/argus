package agent

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

/*
Direct SSH logins held for approval or allowed by the server's setting. These
hold the closed side: a held login opens nothing until approved, a refusal or
a missing control plane refuses, a shim older than "pending" fails closed, and
an admitted login is recorded and says how it was let in.
*/

// A held login opens no recording until an admin approves it, then is
// recorded under that approval.
func TestApproveHoldsADirectLoginUntilApproved(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHApprove)
	release := make(chan Approval)
	c.AskApproval = func(ctx context.Context, m SessionStart) Approval { return <-release }
	sealed := make(chan SessionRecord, 1)
	c.OnSession = func(r SessionRecord) { sealed <- r }

	conn, dec := startAs(t, sock, Direct)
	r := readReply(t, conn, dec)
	if !r.Pending || !r.Refused || r.Error == "" {
		t.Fatalf("first answer = %+v, want pending that an old shim reads as a refusal", r)
	}
	if n := recordings(t, c); n != 0 {
		t.Fatalf("%d recordings opened before approval", n)
	}

	release <- Approval{Approved: true, ID: "req-1"}
	r = readReply(t, conn, dec)
	if !r.Record || r.SessionID == "" || r.Refused {
		t.Fatalf("after approval = %+v, want record", r)
	}
	_ = json.NewEncoder(conn).Encode(SessionEnd{Type: "end"})
	select {
	case rec := <-sealed:
		if rec.DirectApproval != "approved" || rec.DirectLoginID != "req-1" {
			t.Errorf("session does not say it was approved: %+v", rec)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the approved session was not sealed")
	}
}

// Not approved is a refusal, with the reason, and nothing recorded.
func TestApproveRefusesWhatIsNotApproved(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHApprove)
	c.AskApproval = func(ctx context.Context, m SessionStart) Approval {
		return Approval{ID: "req-2", Reason: "admin@example.com denied this login"}
	}
	conn, dec := startAs(t, sock, Direct)
	if r := readReply(t, conn, dec); !r.Pending {
		t.Fatalf("first answer = %+v", r)
	}
	r := readReply(t, conn, dec)
	if !r.Refused || r.Record || r.Reason != "admin@example.com denied this login" {
		t.Fatalf("after denial = %+v", r)
	}
	if n := recordings(t, c); n != 0 {
		t.Errorf("%d recordings opened for a denied login", n)
	}
}

// With no way to ask, "approve" refuses rather than admitting.
func TestApproveWithNoControlPlaneRefuses(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHApprove)
	conn, dec := startAs(t, sock, Direct)
	if r := readReply(t, conn, dec); !r.Refused || r.Record || r.Pending {
		t.Fatalf("answer = %+v, want refused", r)
	}
}

// A shutdown does not wait on a login held for approval.
func TestApproveGivesWayToAShutdown(t *testing.T) {
	c, sock := testCollector(t)
	c.SetDirectSSH(DirectSSHApprove)
	c.AskApproval = func(ctx context.Context, m SessionStart) Approval {
		<-ctx.Done()
		return Approval{Reason: "the agent is stopping"}
	}
	conn, dec := startAs(t, sock, Direct)
	readReply(t, conn, dec)

	done := make(chan struct{})
	go func() { _ = c.CloseWithin(2 * time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited on a login held for approval")
	}
}

// "Allow" admits and records, says so on the session, and reports it.
func TestAllowRecordsAndReports(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHAllow)
	allowed := make(chan SessionStart, 1)
	c.OnAllowed = func(m SessionStart) { allowed <- m }
	sealed := make(chan SessionRecord, 1)
	c.OnSession = func(r SessionRecord) { sealed <- r }

	conn, dec := startAs(t, sock, Direct)
	if r := readReply(t, conn, dec); !r.Record || r.Refused {
		t.Fatalf("answer = %+v, want record", r)
	}
	select {
	case <-allowed:
	case <-time.After(3 * time.Second):
		t.Fatal("an allowed login was not reported")
	}
	_ = json.NewEncoder(conn).Encode(SessionEnd{Type: "end"})
	select {
	case rec := <-sealed:
		if rec.DirectApproval != "allowed" {
			t.Errorf("session does not say it was allowed: %+v", rec)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the allowed session was not sealed")
	}
}

// An unknown mode from a confused control plane is a refusal.
func TestAnUnknownModeRefuses(t *testing.T) {
	c := &Collector{}
	c.SetDirectSSH("sometimes")
	if c.DirectSSH() != DirectSSHRefuse || !c.ConsoleOnly() {
		t.Fatalf("unknown mode applied as %q", c.DirectSSH())
	}
}

// The shim's side of a held login: pending, then the answer; and silence
// after pending refuses, where silence before it is an old daemon.
func TestTheShimReadsPending(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		_ = json.NewEncoder(b).Encode(Decision{Type: "decision", Pending: true, Refused: true,
			Reason: ApprovalPendingReason, Error: ApprovalPendingReason})
	}()
	dec := json.NewDecoder(a)
	d := awaitDecision(a, dec, 3*time.Second)
	if !d.pending || d.reason == "" {
		t.Fatalf("decision = %+v, want pending with a reason", d)
	}
	if d = awaitDecision(a, dec, 100*time.Millisecond); !d.silent {
		t.Fatalf("silence after pending = %+v, want silent", d)
	}
}

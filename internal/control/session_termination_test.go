package control

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"
)

/*
A terminated session has to survive the trip.

The gateway has always sent terminatedBy and terminationReason. The control
plane had no such fields, and decode() rejects unknown fields on purpose, so
the whole report came back 400 -- not the two fields, the report. The session
stayed `active` with no chain head and no recording key, for a session that had
ended minutes earlier with its recording sealed and uploaded.

Terminating a session is one of the controls this product exists to offer. It
was the one report guaranteed never to land.
*/

// newUUID makes an id the sessions table will accept; sessions.id is a UUID
// column and the gateway mints real ones.
func newUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// dropSession removes a test's session row when it is done.
//
// The database is shared between tests and between runs -- see unique() in
// store_test.go -- so a row left behind is a row every later test sees. These
// were the only sessions in it, which meant a test that did not seed its own
// data could look like it was comparing something when it was comparing
// whatever happened to be lying around.
func dropSession(t *testing.T, s *Store, id string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(),
			`DELETE FROM sessions WHERE id = $1`, id); err != nil {
			t.Errorf("cleaning up session %s: %v", id, err)
		}
	})
}

func TestATerminatedSessionReportIsStored(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := newUUID(t)
	dropSession(t, s, id)
	by, why := "lin@northwind.id", "credential harvesting observed in the session"
	head := "a1b2c3d4e5f60718293a4b5c6d7e8f900112233445566778899aabbccddeeff0"
	ended := time.Now().UTC()

	in := Session{
		ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: "pay-01",
		Principal: "ops", Protocol: "ssh", Origin: "brokered",
		State: "terminated", StartedAt: time.Now().UTC().Add(-time.Minute),
		EndedAt: &ended, Fidelity: "pty", ChainHead: &head,
		RiskFlags: []string{"root-principal"}, ReportedBy: "gateway",
		TerminatedBy: &by, TerminationReason: &why,
	}
	if err := s.UpsertSession(ctx, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.Session(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.State != "terminated" {
		t.Errorf("state = %q, want terminated -- a stopped session is not one "+
			"that finished, and the record has to tell them apart", got.State)
	}
	if got.TerminatedBy == nil || *got.TerminatedBy != by {
		t.Errorf("terminatedBy = %v, want %q", got.TerminatedBy, by)
	}
	if got.TerminationReason == nil || *got.TerminationReason != why {
		t.Errorf("terminationReason = %v, want %q", got.TerminationReason, why)
	}
	if got.ChainHead == nil || *got.ChainHead != head {
		t.Errorf("chainHead = %v; a terminated session's recording is still evidence", got.ChainHead)
	}
	if got.EndedAt == nil {
		t.Error("endedAt is nil; the session would read as still running")
	}
}

// A later report that says nothing about the termination must not erase it.
func TestALaterReportDoesNotForgetWhyASessionEnded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := newUUID(t)
	dropSession(t, s, id)
	by, why := "argus", "the gateway is shutting down"

	base := Session{
		ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: "pay-01",
		Principal: "ops", Protocol: "ssh", Origin: "brokered",
		State: "terminated", StartedAt: time.Now().UTC(), Fidelity: "pty",
		RiskFlags: []string{}, ReportedBy: "gateway",
		TerminatedBy: &by, TerminationReason: &why,
	}
	if err := s.UpsertSession(ctx, base); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// A retry from the spool, or a second reporter, with no termination fields.
	quiet := base
	quiet.TerminatedBy, quiet.TerminationReason = nil, nil
	if err := s.UpsertSession(ctx, quiet); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := s.Session(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.TerminatedBy == nil || *got.TerminatedBy != by {
		t.Errorf("a later report erased who ended the session: %v", got.TerminatedBy)
	}
	if got.TerminationReason == nil || *got.TerminationReason != why {
		t.Errorf("a later report erased why: %v", got.TerminationReason)
	}
}

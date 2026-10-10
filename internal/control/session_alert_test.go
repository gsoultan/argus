package control

import (
	"context"
	"testing"
	"time"
)

// The gateway and the host agent both report every session, and keep
// reporting it while it runs. An administrator alerted on each report would get
// a dozen messages per login and stop reading them, so only the report that
// creates the row may count as the session starting.
func TestOnlyTheFirstReportOfASessionIsNew(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := newUUID(t)
	dropSession(t, s, id)

	in := Session{
		ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: "pay-01",
		Principal: "ops", Protocol: "ssh", Origin: "brokered",
		State: "active", StartedAt: time.Now().UTC().Add(-time.Minute),
		Fidelity: "pty", RiskFlags: []string{}, ReportedBy: "gateway",
	}
	created, err := s.RecordSession(ctx, in)
	if err != nil {
		t.Fatalf("first report: %v", err)
	}
	if !created {
		t.Fatal("the first report of a session did not count as new")
	}

	for _, by := range []string{"agent", "gateway"} {
		in.ReportedBy = by
		created, err = s.RecordSession(ctx, in)
		if err != nil {
			t.Fatalf("report by %s: %v", by, err)
		}
		if created {
			t.Fatalf("a later report by %s counted as a new session", by)
		}
	}
}

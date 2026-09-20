package control

import (
	"context"
	"testing"
	"time"
)

// A session cannot be stored saying both that it ended and that it never did.
//
// `state` and `ended_at` are written by different branches of the gateway's
// report, and for a session with no recorder the end time was omitted while the
// terminal state was not. In dev that left 26 of 56 terminations claiming
// "terminated" and "never ended" at once, against 12,276 of 12,276 ordinary
// closes stored correctly — the split falling on exactly the sessions someone
// had stopped because something was wrong.
//
// The gateway no longer sends one, but this side owns the invariant: an older
// gateway reporting into a newer control plane is precisely when it would be
// broken again, and the null is not inert. The console reads it as
// still-running — the Overview showed those sessions at hundreds of hours — and
// fidelityUnsupported skips any session with a nil EndedAt, on the fair ground
// that one still in flight has legitimately observed nothing yet, so they were
// exempt from the evidence check too.
func TestATerminalSessionCannotBeStoredWithoutAnEndTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		state   string
		wantEnd bool
	}{
		{"terminated with no end time", "terminated", true},
		{"closed with no end time", "closed", true},
		// The one state where a null end time is the truth.
		{"active", "active", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := newUUID(t)
			dropSession(t, s, id)

			in := Session{
				ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: "pay-01",
				Principal: "ops", Protocol: "ssh", Origin: "brokered",
				State: tc.state, StartedAt: time.Now().UTC().Add(-time.Minute),
				// No EndedAt and no ChainHead: a session whose recorder never
				// existed, which is the case that produced the bad rows.
				EndedAt: nil, ChainHead: nil,
				Fidelity: "pty", RiskFlags: []string{}, ReportedBy: "gateway",
			}
			if err := s.UpsertSession(ctx, in); err != nil {
				t.Fatalf("upsert: %v", err)
			}

			var endedAt *time.Time
			err := s.pool.QueryRow(ctx,
				`SELECT ended_at FROM sessions WHERE id = $1`, id).Scan(&endedAt)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}

			if got := endedAt != nil; got != tc.wantEnd {
				t.Errorf("state=%q: ended_at set = %v, want %v (got %v)",
					tc.state, got, tc.wantEnd, endedAt)
			}
		})
	}
}

package control

import (
	"context"
	"testing"
	"time"
)

// An active session that nothing has spoken for in minutes is not live.
//
// `state` alone could not say so: a gateway killed rather than drained never
// reports the end, so fifteen sessions in dev were `active` for up to nineteen
// days and the console counted every one of them as running. Nothing could reap
// them either — a gateway carries no identity to attribute orphans to, and with
// no maximum session duration, age alone cannot separate a dead session from a
// long-running one. The fix is not a reaper but a fact: gateways re-report what
// they hold once a minute, and silence past three intervals is reported as
// silence rather than as life.
func TestAnActiveSessionGoneQuietIsNotReportedAsLive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Well past the threshold, and far short of the nineteen days that
	// prompted this, so the test does not encode the accident.
	const quiet = SessionSilenceThreshold + time.Minute

	for _, tc := range []struct {
		name       string
		state      string
		quietFor   time.Duration
		wantSilent bool
	}{
		{"active and just reported", "active", 0, false},
		{"active but unreported past the threshold", "active", quiet, true},
		// A session that told us it ended is finished, not silent, however
		// long ago it said so. Only the unknown is silent.
		{"closed long ago", "closed", 30 * 24 * time.Hour, false},
		{"terminated long ago", "terminated", 30 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := newUUID(t)
			dropSession(t, s, id)

			end := time.Now().UTC().Add(-tc.quietFor)
			in := Session{
				ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: "pay-01",
				Principal: "ops", Protocol: "ssh", Origin: "brokered",
				State: tc.state, StartedAt: time.Now().UTC().Add(-time.Hour),
				Fidelity: "pty", RiskFlags: []string{}, ReportedBy: "gateway",
			}
			if tc.state != "active" {
				in.EndedAt = &end
			}
			if err := s.UpsertSession(ctx, in); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			// The upsert stamps now() on every report, so backdating has to
			// happen after it — which is also the only way to reach this state
			// without waiting out the threshold.
			backdateReport(t, s, id, tc.quietFor)

			got, err := s.Session(ctx, id)
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if got == nil {
				t.Fatal("session not found")
			}
			if got.Silent != tc.wantSilent {
				t.Errorf("Session(): silent = %v, want %v (last reported %s ago)",
					got.Silent, tc.wantSilent, time.Since(got.LastReportedAt).Round(time.Second))
			}

			// The list is the page that says "N live". It reads through the
			// generated query rather than the hand-written one above, and the
			// two have disagreed before: terminated_by and termination_reason
			// were returned by one and dropped by the other.
			list, err := s.Sessions(ctx, SessionFilter{Limit: 500})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var found *Session
			for i := range list {
				if list[i].ID == id {
					found = &list[i]
					break
				}
			}
			if found == nil {
				t.Fatal("session missing from list")
			}
			if found.Silent != tc.wantSilent {
				t.Errorf("Sessions(): silent = %v, want %v", found.Silent, tc.wantSilent)
			}
			if found.LastReportedAt.IsZero() {
				t.Error("Sessions(): lastReportedAt is zero; the list dropped the column")
			}
		})
	}
}

// And the keepalive is what clears it: a gateway still holding the session says
// so once a minute, and the next report has to count as a sign of life whatever
// else it carries.
func TestReportingALiveSessionAgainClearsSilence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := newUUID(t)
	dropSession(t, s, id)

	in := Session{
		ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: "pay-01",
		Principal: "ops", Protocol: "ssh", Origin: "brokered",
		State: "active", StartedAt: time.Now().UTC().Add(-time.Hour),
		Fidelity: "pty", RiskFlags: []string{}, ReportedBy: "gateway",
	}
	if err := s.UpsertSession(ctx, in); err != nil {
		t.Fatalf("first report: %v", err)
	}
	backdateReport(t, s, id, SessionSilenceThreshold+time.Minute)

	got, err := s.Session(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("fetch: %v", err)
	}
	if !got.Silent {
		t.Fatal("precondition: session should be silent before the keepalive")
	}

	// The keepalive the gateway sends: the same record, still active, no end
	// time and no chain head.
	if err := s.UpsertSession(ctx, in); err != nil {
		t.Fatalf("keepalive: %v", err)
	}

	got, err = s.Session(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("refetch: %v", err)
	}
	if got.Silent {
		t.Errorf("still silent after a keepalive (last reported %s ago)",
			time.Since(got.LastReportedAt).Round(time.Second))
	}
	if got.State != "active" || got.EndedAt != nil {
		t.Errorf("keepalive changed the session: state=%q endedAt=%v", got.State, got.EndedAt)
	}
}

// backdateReport moves a session's last report into the past.
func backdateReport(t *testing.T, s *Store, id string, d time.Duration) {
	t.Helper()
	if d == 0 {
		return
	}
	_, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET last_reported_at = now() - $2::interval WHERE id = $1`,
		id, d.String())
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

package control

import (
	"context"
	"testing"
	"time"
)

// A session whose gateway never came back is closed at the last moment it was
// seen alive, and the record says the end was deduced.
//
// Fifteen sessions in dev sat `active` for up to nineteen days because a gateway
// killed rather than drained never reports the end. Silence made that visible;
// nothing resolved it, so the unknown count only grew.
//
// The end time is the crux. Writing `now()` would invent a minute nobody
// watched. `last_reported_at` is an observation -- the session was alive at
// least until then -- and `end_inferred` is what stops a reader taking it for a
// clean logout.
func TestAnAbandonedSessionIsClosedAtItsLastReport(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		quietFor  time.Duration
		state     string
		wantClose bool
	}{
		{"silent for longer than the threshold", SessionAbandonedAfter + time.Hour, "active", true},
		// Unknown, not abandoned. The console says it does not know, and a
		// gateway coming back clears it outright.
		{"silent but inside the threshold", SessionAbandonedAfter - time.Hour, "active", false},
		{"reporting normally", 0, "active", false},
		// Already finished; there is nothing to infer.
		{"already closed", SessionAbandonedAfter * 30, "closed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := newUUID(t)
			dropSession(t, s, id)

			started := time.Now().UTC().Add(-30 * 24 * time.Hour)
			in := Session{
				ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: "pay-01",
				Principal: "ops", Protocol: "ssh", Origin: "brokered",
				State: tc.state, StartedAt: started,
				Fidelity: "pty", RiskFlags: []string{}, ReportedBy: "gateway",
			}
			if tc.state != "active" {
				end := time.Now().UTC().Add(-tc.quietFor)
				in.EndedAt = &end
			}
			if err := s.UpsertSession(ctx, in); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			backdateReport(t, s, id, tc.quietFor)

			closed, err := s.CloseAbandonedSessions(ctx, SessionAbandonedAfter)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			var hit *Session
			for i := range closed {
				if closed[i].ID == id {
					hit = &closed[i]
				}
			}
			if (hit != nil) != tc.wantClose {
				t.Fatalf("closed = %v, want %v", hit != nil, tc.wantClose)
			}

			got, err := s.Session(ctx, id)
			if err != nil || got == nil {
				t.Fatalf("fetch: %v", err)
			}
			if !tc.wantClose {
				if got.EndInferred {
					t.Error("a session that was not swept is marked as an inferred end")
				}
				return
			}

			if got.State != "closed" {
				t.Errorf("state = %q, want closed", got.State)
			}
			if !got.EndInferred {
				t.Error("the end was deduced and the record does not say so")
			}
			if got.EndedAt == nil {
				t.Fatal("closed with no end time")
			}
			// The observation, not the sweep's clock. A session closed at
			// now() would claim to have run until the moment nobody was
			// watching it, which on a 19-day-old row is a fabrication.
			if !got.EndedAt.Equal(got.LastReportedAt) {
				t.Errorf("endedAt = %v, want the last report %v",
					got.EndedAt, got.LastReportedAt)
			}
			if got.Silent {
				t.Error("still reported as silent after being closed")
			}
		})
	}
}

// The sweep returns what it closed, because each one is an audit event.
func TestTheSweepReportsWhatItClosedSoItCanBeAudited(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := newUUID(t)
	dropSession(t, s, id)
	in := Session{
		ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: "pay-01",
		Principal: "ops", Protocol: "ssh", Origin: "brokered",
		State: "active", StartedAt: time.Now().UTC().Add(-48 * time.Hour),
		Fidelity: "pty", RiskFlags: []string{}, ReportedBy: "gateway",
	}
	if err := s.UpsertSession(ctx, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	backdateReport(t, s, id, SessionAbandonedAfter+time.Hour)

	closed, err := s.CloseAbandonedSessions(ctx, SessionAbandonedAfter)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var hit *Session
	for i := range closed {
		if closed[i].ID == id {
			hit = &closed[i]
		}
	}
	if hit == nil {
		t.Fatal("the sweep closed the session but did not report it")
	}
	// Everything the audit entry has to name.
	if hit.UserEmail == "" || hit.AssetHostname == "" || hit.Principal == "" {
		t.Errorf("incomplete record for the audit entry: %+v", hit)
	}
	if hit.LastReportedAt.IsZero() {
		t.Error("no last report to cite as the end")
	}

	// And a second pass finds nothing: it is closed, so it cannot be swept
	// twice into two audit entries for one event.
	again, err := s.CloseAbandonedSessions(ctx, SessionAbandonedAfter)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	for _, c := range again {
		if c.ID == id {
			t.Error("the same session was closed twice")
		}
	}
}

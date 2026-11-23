package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A session the agent reported and no longer has is ended at the next
// heartbeat, not left on Live: one the login carried on under a new id is
// ended as carried on, and Watch live finds the new one from the old.
func TestSessionsTheAgentNoLongerHasAreEnded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	host := "imp-crm-" + newUUID(t)[:8]
	reporter := "agent:" + host
	before := time.Now().UTC().Add(-2 * time.Hour)

	lost, carried, carrier, running, fresh, gateway :=
		newUUID(t), newUUID(t), newUUID(t), newUUID(t), newUUID(t), newUUID(t)
	add := func(id, by, reason string, started time.Time) {
		t.Helper()
		if err := s.UpsertSession(ctx, Session{ID: id, UserEmail: "ops@" + host,
			AssetHostname: host, Principal: "ops", Protocol: "ssh", Origin: "direct",
			OriginReason: &reason, State: "active", StartedAt: started, Fidelity: "pty",
			RiskFlags: []string{}, ReportedBy: by}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
		t.Cleanup(func() { dropSession(t, s, id) })
	}
	add(lost, reporter, "", before)
	add(carried, reporter, "", before)
	add(carrier, reporter, "continues session "+carried+" after the host agent restarted; x",
		before.Add(time.Hour))
	add(running, reporter, "", before)
	// Began after the beat was taken: its row can arrive first.
	add(fresh, reporter, "", time.Now().UTC().Add(time.Minute))
	// Not this agent's to speak for.
	add(gateway, "gateway", "", before)

	ended, err := s.EndSessionsAgentLost(ctx, reporter, AgentRecording{
		At: time.Now().UTC(),
		Sessions: []AgentRecordingSession{
			{ID: carrier, Continues: []string{carried}}, {ID: running},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range ended {
		got[v.ID] = *v.TerminationReason
	}
	if len(got) != 2 || got[lost] != agentLostReason ||
		!strings.Contains(got[carried], "carried on as session "+carrier) {
		t.Fatalf("ended %v", got)
	}
	for _, id := range []string{running, fresh, gateway, carrier} {
		if v, _ := s.Session(ctx, id); v == nil || v.State != "active" {
			t.Errorf("%s was ended", id)
		}
	}
	if v, _ := s.Session(ctx, lost); v == nil || !v.EndInferred || v.EndedAt == nil {
		t.Errorf("lost session: %+v", v)
	}
	if next, err := s.ContinuedAs(ctx, carried); err != nil || next != carrier {
		t.Errorf("ContinuedAs = %q, %v; want %s", next, err, carrier)
	}
	if next, _ := s.ContinuedAs(ctx, lost); next != "" {
		t.Errorf("a lost session continues as %q", next)
	}

	// A list too long to trust ends nothing.
	long := AgentRecording{At: time.Now().UTC(), Sessions: make([]AgentRecordingSession, agentRosterMax+1)}
	if ended, _ := s.EndSessionsAgentLost(ctx, reporter, long); len(ended) != 0 {
		t.Errorf("an oversized list ended %d", len(ended))
	}
}

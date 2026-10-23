package agent

import (
	"testing"
	"time"
)

// A session through the tunnel is reported as running when it starts and
// for as long as it runs, and once it is sealed it is gone from that list
// and reported as ended -- by whoever ended it, when someone did.
func TestATunnelSessionIsReportedWhileItRuns(t *testing.T) {
	c := NewCollector(t.TempDir(), nil)
	var active []SessionRecord
	var sealed []SessionRecord
	c.OnActive = func(r SessionRecord) { active = append(active, r) }
	c.OnSession = func(r SessionRecord) { sealed = append(sealed, r) }

	rec, err := c.StartRecording(SessionStart{Type: "start", Principal: "ana", Origin: Brokered,
		Hostname: "h", StartedAt: time.Now().UTC(), Actor: "ana@example.com"}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].State != "active" || active[0].ID != rec.ID() ||
		active[0].Actor != "ana@example.com" {
		t.Fatalf("on start: %+v", active)
	}
	if got := c.ActiveTunnelRecords(); len(got) != 1 || got[0].ID != rec.ID() {
		t.Fatalf("while running: %+v", got)
	}
	_ = rec.Output([]byte("hello"))
	rec.Terminate("admin@example.com", "left open")
	rec.Terminate("someone@example.com", "second")
	rec.Seal(129)

	if got := c.ActiveTunnelRecords(); len(got) != 0 {
		t.Errorf("still listed after the seal: %+v", got)
	}
	if len(sealed) != 1 || sealed[0].State != "terminated" ||
		sealed[0].TerminatedBy != "admin@example.com" || sealed[0].TerminationReason != "left open" {
		t.Errorf("sealed as %+v", sealed)
	}
}

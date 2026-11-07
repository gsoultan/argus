package control

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"
)

func homeNow(t *testing.T, s *Store) HomeSnapshot {
	t.Helper()
	ctx := context.Background()
	var h HomeSnapshot
	if err := s.homeFast(ctx, &h); err != nil {
		t.Fatalf("fast: %v", err)
	}
	if err := s.homeSlow(ctx, &h, time.Now()); err != nil {
		t.Fatalf("slow: %v", err)
	}
	return h
}

// Each number on Home moves with the row it counts: a live session as root,
// a failed sign-in, a busy server with a filling disk.
func TestHomeCountsWhatTheStoreHolds(t *testing.T) {
	s, h, _ := reportAPI(t)
	ctx := context.Background()
	before := homeNow(t, s)

	asset, cred := enrolledAsset(t, s, h)
	beat := `{"hostname":"` + asset.Hostname + `","version":"0.9.5","usage":{"at":"` +
		time.Now().UTC().Add(-10*time.Second).Format(time.RFC3339) +
		`","load":[1,1,1],"cpu_busy":100,"mem_total":1000,"mem_available":400,"swap_total":0,"swap_free":0,` +
		`"disks":[{"mount":"/data","fs":"ext4","total":100,"used":99,"avail":1},{"mount":"/","fs":"ext4","total":100,"used":10,"avail":90}]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, beat); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}

	id := newUUID(t)
	dropSession(t, s, id)
	if err := s.UpsertSession(ctx, Session{
		ID: id, UserEmail: "dewi.p@northwind.id", AssetHostname: asset.Hostname,
		Principal: "root", Protocol: "ssh", Origin: "brokered", State: "active",
		StartedAt: time.Now().UTC(), Fidelity: "pty", RiskFlags: []string{}, ReportedBy: "agent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAudit(ctx, AuditEvent{Action: "auth.password_failed", Severity: "warning",
		ActorEmail: "who@x.id", Target: "console"}); err != nil {
		t.Fatal(err)
	}

	after := homeNow(t, s)
	if after.Live.Count != before.Live.Count+1 || after.Live.Root != before.Live.Root+1 {
		t.Errorf("live %+v, before %+v", after.Live, before.Live)
	}
	if !slices.ContainsFunc(after.LiveList, func(x HomeSession) bool { return x.ID == id && x.Principal == "root" }) {
		t.Errorf("the new session is not in Live now: %+v", after.LiveList)
	}
	if after.Sessions.Last24h != before.Sessions.Last24h+1 || after.Sessions.Hours[23][0] < 1 {
		t.Errorf("sessions %+v", after.Sessions)
	}
	if after.SignIns.Failed15m != before.SignIns.Failed15m+1 || after.SignIns.Failed24 != before.SignIns.Failed24+1 {
		t.Errorf("sign-ins %+v, before %+v", after.SignIns, before.SignIns)
	}
	if after.Servers.Total != before.Servers.Total+1 {
		t.Errorf("servers %+v, before %+v", after.Servers, before.Servers)
	}
	if len(after.Processor.Busiest) == 0 || after.Processor.Busiest[0].Busy != 100 || after.Processor.Average == nil {
		t.Errorf("processor %+v", after.Processor)
	}
	if !slices.ContainsFunc(after.Disks, func(d HomeDisk) bool { return d.AssetID == asset.ID && d.Mount == "/data" && d.Used == 99 }) {
		t.Errorf("a 99%% disk is not filling up: %+v", after.Disks)
	}
	if slices.ContainsFunc(after.Disks, func(d HomeDisk) bool { return d.AssetID == asset.ID && d.Mount == "/" }) {
		t.Errorf("a 10%% disk is listed as filling up: %+v", after.Disks)
	}
	for _, e := range after.Audit {
		if e.Hash != "" || e.PrevHash != "" {
			t.Fatalf("Home carries the chain's hashes: %+v", e)
		}
	}
}

// A usage report older than homeUsageFresh is the past: it does not count
// towards the fleet's processor now.
func TestHomeLeavesOutStaleUsage(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	beat := `{"hostname":"` + asset.Hostname + `","version":"0.9.5","usage":{"at":"` +
		time.Now().UTC().Add(-10*time.Minute).Format(time.RFC3339) +
		`","load":[1,1,1],"cpu_busy":100,"mem_total":1,"mem_available":1,"swap_total":0,"swap_free":0,"disks":[]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, beat); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	got := homeNow(t, s)
	if slices.ContainsFunc(got.Processor.Busiest, func(b HomeBusy) bool { return b.AssetID == asset.ID }) {
		t.Fatalf("a ten-minute-old report is among the busiest now: %+v", got.Processor.Busiest)
	}
}

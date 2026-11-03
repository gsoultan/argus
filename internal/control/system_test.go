package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func getSystem(t *testing.T, h http.Handler, path string) ServerSystem {
	t.Helper()
	w := do(t, h, http.MethodGet, path, "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	var got ServerSystem
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSystemDetailsReachTheServerPage(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/system"

	if got := getSystem(t, h, path); got.Reported || got.System != nil || got.Usage != nil {
		t.Fatalf("before a report: %+v", got)
	}

	facts := `{"hostname":"` + asset.Hostname + `","os":"Ubuntu 24.04.1 LTS","system":{` +
		`"kernel":"6.8.0-45-generic","arch":"x86_64","cpu_model":"Intel Xeon Gold 6248R","cpus":8,` +
		`"memory_bytes":34359738368,"machine":"virtual","hypervisor":"KVM",` +
		`"booted_at":"2026-10-11T02:14:00Z","time_zone":"Asia/Jakarta","utc_offset_seconds":25200}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, facts); w.Code != http.StatusOK {
		t.Fatalf("facts: %d %s", w.Code, w.Body)
	}
	got := getSystem(t, h, path)
	if !got.Reported || got.OS != "Ubuntu 24.04.1 LTS" || got.System == nil ||
		got.System.Kernel != "6.8.0-45-generic" || got.System.CPUs != 8 || got.System.Hypervisor != "KVM" ||
		got.System.OffsetSeconds != 25200 || got.Usage != nil {
		t.Fatalf("after facts: %+v", got)
	}

	// Usage rides the heartbeat. Its time is the agent's sample, not the
	// moment it arrived.
	sampled := time.Now().UTC().Add(-20 * time.Second).Truncate(time.Second)
	beat := func(at time.Time, busy string) string {
		return `{"hostname":"` + asset.Hostname + `","version":"0.9.5","usage":{"at":"` + at.Format(time.RFC3339) +
			`","load":[1.42,1.3,1.18],"cpu_busy":` + busy + `,"mem_total":1000,"mem_available":400,` +
			`"swap_total":100,"swap_free":90,"disks":[` +
			`{"mount":"/","fs":"ext4","total":100,"used":47,"avail":48},` +
			`{"mount":"/broken","fs":"ext4","total":10,"used":50,"avail":0}]}}`
	}
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, beat(sampled, "18")); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	got = getSystem(t, h, path)
	if got.Usage == nil || got.UsageAt == nil || !got.UsageAt.Equal(sampled) {
		t.Fatalf("usage: %+v at %v", got.Usage, got.UsageAt)
	}
	if got.Usage.CPUBusy == nil || *got.Usage.CPUBusy != 18 || got.Usage.MemAvailable != 400 {
		t.Errorf("usage = %+v", got.Usage)
	}
	// A disk that uses more than it holds cannot be true and is dropped.
	if len(got.Usage.Disks) != 1 || got.Usage.Disks[0].Mount != "/" {
		t.Errorf("disks = %+v", got.Usage.Disks)
	}

	// A heartbeat without usage, and one replayed from the spool carrying an
	// older sample, both leave the newer sample where it is.
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred,
		`{"hostname":"`+asset.Hostname+`","version":"0.9.5"}`); w.Code != http.StatusOK {
		t.Fatalf("plain heartbeat: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, beat(sampled.Add(-time.Hour), "99")); w.Code != http.StatusOK {
		t.Fatalf("replayed heartbeat: %d %s", w.Code, w.Body)
	}
	got = getSystem(t, h, path)
	if got.UsageAt == nil || !got.UsageAt.Equal(sampled) || *got.Usage.CPUBusy != 18 {
		t.Errorf("an older or absent sample replaced a newer one: %+v at %v", got.Usage, got.UsageAt)
	}

	// An agent older than system details: what the row holds stays.
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred,
		`{"hostname":"`+asset.Hostname+`","os":"Ubuntu 24.04.1 LTS"}`); w.Code != http.StatusOK {
		t.Fatalf("old facts: %d %s", w.Code, w.Body)
	}
	if got := getSystem(t, h, path); got.System == nil || got.System.Kernel == "" {
		t.Errorf("an old agent's facts erased the system details: %+v", got)
	}

	if w := do(t, h, http.MethodGet, path, "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator read a server's system details: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/assets/00000000-0000-0000-0000-000000000000/system", "admin-token", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown asset: %d", w.Code)
	}
}

func TestSystemReportIsBounded(t *testing.T) {
	sys := (&HostSystem{Kernel: strings.Repeat("k", 5000), Machine: "mainframe", CPUs: -1, OffsetSeconds: 99 * 3600}).bounded()
	if len(sys.Kernel) != maxSystemText || sys.Machine != "" || sys.CPUs != 0 || sys.OffsetSeconds != 0 {
		t.Errorf("system = %+v", sys)
	}
	busy := 140.0
	u := &HostUsage{CPUBusy: &busy, MemTotal: 10, MemAvailable: 20, Load: [3]float64{-1, 2, 3}}
	for i := 0; i < maxSystemDisks+10; i++ {
		u.Disks = append(u.Disks, HostDisk{Mount: strings.Repeat("/x", 1000), Total: 10, Used: 5, Avail: 5})
	}
	b := u.bounded()
	if b.CPUBusy != nil || b.MemAvailable != 10 || b.Load[0] != 0 || len(b.Disks) != maxSystemDisks ||
		len(b.Disks[0].Mount) != maxMountText {
		t.Errorf("usage = %+v", b)
	}
	if (*HostSystem)(nil).bounded() != nil || (*HostUsage)(nil).bounded() != nil {
		t.Error("nil became a report")
	}
}

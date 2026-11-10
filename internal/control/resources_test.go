package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

/*
When an asset is running low: the one rule, the System card carrying its
answer, the fleet list (fresh usage only, fleet roles only), and the bell,
once a day per asset, resource and level.
*/

func TestLowResourcesRule(t *testing.T) {
	const gib = 1 << 30
	busy := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		u    HostUsage
		want string // resource:level[:mount], in order
	}{
		{"nothing low", HostUsage{MemTotal: 32 * gib, MemAvailable: 12 * gib, CPUBusy: busy(40),
			Disks: []HostDisk{{Mount: "/", Total: 100, Used: 47, Avail: 48}}}, ""},
		{"memory at 10% left is low", HostUsage{MemTotal: 100 * gib, MemAvailable: 10 * gib}, "memory:low"},
		{"memory at 4% left is critical", HostUsage{MemTotal: 100 * gib, MemAvailable: 4 * gib}, "memory:critical"},
		// Counted like df: 89 used of 89+10 usable is 90%, though only 89% of the total.
		{"disk counted like df", HostUsage{Disks: []HostDisk{{Mount: "/", Total: 100, Used: 89, Avail: 10}}}, "disk:low:/"},
		{"disk at 95% is critical", HostUsage{Disks: []HostDisk{{Mount: "D:", Total: 100, Used: 95, Avail: 5}}}, "disk:critical:D:"},
		{"swap 90% used", HostUsage{SwapTotal: 100, SwapFree: 10}, "swap:low"},
		{"no swap is not full swap", HostUsage{SwapTotal: 0, SwapFree: 0}, ""},
		{"busy processor", HostUsage{CPUBusy: busy(93.4)}, "cpu:low"},
		{"impossible figures warn nothing", HostUsage{MemTotal: 10, MemAvailable: 20, CPUBusy: busy(140),
			Disks: []HostDisk{{Mount: "/", Total: 10, Used: 0, Avail: 0}}}, ""},
		{"critical first", HostUsage{CPUBusy: busy(99), MemTotal: 100, MemAvailable: 8,
			Disks: []HostDisk{{Mount: "/data", Total: 100, Used: 97, Avail: 3}}}, "disk:critical:/data memory:low cpu:low"},
	}
	for _, c := range cases {
		var got []string
		for _, w := range LowResources(c.u) {
			s := w.Resource + ":" + w.Level
			if w.Mount != "" {
				s += ":" + w.Mount
			}
			got = append(got, s)
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s: got %q, want %q", c.name, g, c.want)
		}
	}
	w := LowResources(HostUsage{MemTotal: 32 * gib, MemAvailable: gib + gib/2})
	if len(w) != 1 || w[0].Percent != 4 || w[0].Left != gib+gib/2 || w[0].Total != 32*gib {
		t.Errorf("memory warning = %+v, want 4%% left of 32 GiB", w)
	}
	if title, detail := resourceAlertText("db-02.example", w[0]); title != "db-02: memory almost gone" ||
		detail != "1.5 GB of 32 GB left (4%). Open the asset to see how busy it is." {
		t.Errorf("alert text = %q / %q", title, detail)
	}
}

func TestResourcesLowAcrossTheFleetAndInTheBell(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	operator, _ := newAccount(t, s, "operator")
	a.UserTokens["operator-token"] = operator
	ctx := context.Background()

	low, lowCred := enrolledAsset(t, s, h)
	stale, staleCred := enrolledAsset(t, s, h)
	fine, fineCred := enrolledAsset(t, s, h)
	_, _ = s.pool.Exec(ctx, `DELETE FROM console_alerts WHERE kind = 'resources'`)

	now := time.Now().UTC().Truncate(time.Second)
	// A Windows agent: no load average, drives by letter, traffic per
	// interface. 1 GB of memory left of 32 and C: 96% full.
	beat := func(cred, host string, at time.Time, avail, used int) {
		t.Helper()
		body := `{"hostname":"` + host + `","version":"0.9.9","usage":{"at":"` + at.Format(time.RFC3339) +
			`","cpu_busy":22,"mem_total":34359738368,"mem_available":` + itoa(avail) + `,"swap_total":0,"swap_free":0,` +
			`"disks":[{"mount":"C:","fs":"ntfs","total":100,"used":` + itoa(used) + `,"avail":` + itoa(100-used) + `}],` +
			`"net":[{"name":"Ethernet","rx_bps":125000,"tx_bps":2500,"speed_mbps":1000},{"name":"Wi-Fi","speed_mbps":866}]}}`
		if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body); w.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
		}
	}
	beat(lowCred, low.Hostname, now, 1<<30, 96)
	beat(staleCred, stale.Hostname, now.Add(-20*time.Minute), 1<<30, 99)
	beat(fineCred, fine.Hostname, now, 20<<30, 40)

	// The card carries the rule's answer, and says there is no load average
	// rather than an idle machine.
	sys := getSystem(t, h, "/api/v1/assets/"+low.ID+"/system")
	if sys.Usage == nil || sys.Usage.Load != nil {
		t.Fatalf("usage = %+v, want no load average from a Windows agent", sys.Usage)
	}
	if len(sys.Usage.Net) != 2 || sys.Usage.Net[0].Name != "Ethernet" || sys.Usage.Net[0].RxBps == nil ||
		*sys.Usage.Net[0].RxBps != 125000 || sys.Usage.Net[1].RxBps != nil || sys.Usage.Net[1].SpeedMbps != 866 {
		t.Errorf("net = %+v", sys.Usage.Net)
	}
	if len(sys.Warnings) != 2 || sys.Warnings[0].Resource != ResourceMemory || sys.Warnings[1].Mount != "C:" {
		t.Errorf("warnings = %+v", sys.Warnings)
	}
	if fineSys := getSystem(t, h, "/api/v1/assets/"+fine.ID+"/system"); fineSys.Warnings == nil || len(fineSys.Warnings) != 0 {
		t.Errorf("a healthy asset's warnings = %#v, want an empty list", fineSys.Warnings)
	}

	// The fleet list: the low asset, not the healthy one, and not the one
	// whose only report is twenty minutes old.
	w := do(t, h, http.MethodGet, "/api/v1/resources-low", "admin-token", "")
	var v ResourcesLow
	if _ = json.Unmarshal(w.Body.Bytes(), &v); w.Code != http.StatusOK {
		t.Fatalf("resources-low: %d %s", w.Code, w.Body)
	}
	listed := map[string]bool{}
	for _, r := range v.Assets {
		listed[r.AssetID] = true
	}
	if !listed[low.ID] || listed[fine.ID] || listed[stale.ID] {
		t.Errorf("listed = %v (low %s, fine %s, stale %s)", listed, low.ID, fine.ID, stale.ID)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/resources-low", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator read which assets are running low: %d", w.Code)
	}

	// The bell: one alert per resource and level, however often it looks.
	count := func(like string) int {
		var n int
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM console_alerts WHERE kind = 'resources' AND subject LIKE $1`, like).Scan(&n)
		return n
	}
	a.watchResources(ctx, time.Now().UTC())
	a.watchResources(ctx, time.Now().UTC())
	if n := count("resources:" + low.ID + ":memory:critical:%"); n != 1 {
		t.Errorf("memory alerts = %d, want 1", n)
	}
	if n := count("resources:" + low.ID + ":disk:C::critical:%"); n != 1 {
		t.Errorf("disk alerts = %d, want 1", n)
	}
	if n := count("resources:" + stale.ID + ":%"); n != 0 {
		t.Errorf("a quiet agent's old figures raised %d alerts", n)
	}
	var sev, link string
	_ = s.pool.QueryRow(ctx, `SELECT severity, link FROM console_alerts WHERE kind = 'resources' AND subject LIKE $1`,
		"resources:"+low.ID+":memory:%").Scan(&sev, &link)
	if sev != "critical" || link != "/assets/"+low.ID {
		t.Errorf("alert = %s %s", sev, link)
	}
}

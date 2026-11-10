package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

/*
The Metrics tab's history: samples fold into 5-minute buckets with the
heartbeat that carries them, a bucket with no report is absent rather than
zero, wider ranges average and keep peaks, old buckets are pruned, and only
the fleet roles read it.
*/

func TestUsageHistoryFoldsSamplesAndKeepsGaps(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	operator, _ := newAccount(t, s, "operator")
	a.UserTokens["operator-token"] = operator
	ctx := context.Background()
	asset, cred := enrolledAsset(t, s, h)

	now := time.Now().UTC()
	b0 := now.Truncate(historyBucket).Add(-time.Hour) // a full bucket an hour ago
	beat := func(at time.Time, busy float64, avail int, rx float64) {
		t.Helper()
		body := fmt.Sprintf(`{"hostname":%q,"version":"0.9.9","usage":{"at":%q,"load":[1,1,1],"cpu_busy":%g,`+
			`"mem_total":1000,"mem_available":%d,"swap_total":0,"swap_free":0,`+
			`"disks":[{"mount":"/","fs":"ext4","total":100,"used":%d,"avail":%d}],`+
			`"net":[{"name":"eth0","rx_bps":%g,"tx_bps":10},{"name":"eth1","rx_bps":5}]}}`,
			asset.Hostname, at.Format(time.RFC3339), busy, avail, 100-avail/10, avail/10, rx)
		if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body); w.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
		}
	}
	// Two samples in one bucket, then a 20-minute silence, then one more.
	beat(b0.Add(30*time.Second), 20, 600, 100)
	beat(b0.Add(90*time.Second), 60, 400, 300)
	beat(b0.Add(25*time.Minute), 10, 800, 0)

	get := func(rng string) AssetMetrics {
		t.Helper()
		w := do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/metrics?range="+rng, "admin-token", "")
		var m AssetMetrics
		if _ = json.Unmarshal(w.Body.Bytes(), &m); w.Code != http.StatusOK {
			t.Fatalf("metrics %s: %d %s", rng, w.Code, w.Body)
		}
		return m
	}
	m := get("6h")
	if !m.Reported || m.StepSeconds != 300 || len(m.Points) != 2 {
		t.Fatalf("6h = %+v, want two points with the silence left out", m)
	}
	p := m.Points[0]
	if !p.At.Equal(b0) || p.Samples != 2 || p.CPU == nil || *p.CPU != 40 || *p.CPUMax != 60 || *p.Mem != 60 {
		t.Errorf("first bucket = %+v, want average 40, peak 60, memory peak 60%%", p)
	}
	if p.Rx == nil || *p.Rx != 205 || *p.RxMax != 305 || *p.Tx != 10 {
		t.Errorf("network = rx %v (max %v) tx %v, want both interfaces summed, averaged and peaked", p.Rx, p.RxMax, p.Tx)
	}
	if p.Disks["/"] != 60 || len(m.Mounts) != 1 || m.Mounts[0] != "/" {
		t.Errorf("disks = %v mounts %v", p.Disks, m.Mounts)
	}
	if !m.Points[1].At.Equal(b0.Add(25 * time.Minute)) {
		t.Errorf("second point at %v", m.Points[1].At)
	}

	// A wider range folds the buckets into wider steps, keeping peaks. The
	// samples fall in one two-hour step or straddle two, by the time of day
	// the test runs, so the peak is looked for in any of them.
	if m := get("30d"); len(m.Points) == 0 || m.StepSeconds != 7200 {
		t.Errorf("30d = %+v", m)
	} else {
		peak := 0.0
		for _, p := range m.Points {
			if p.CPUMax != nil {
				peak = max(peak, *p.CPUMax)
			}
		}
		if peak < 60 {
			t.Errorf("30d kept a peak of %v, want 60: %+v", peak, m)
		}
	}

	if w := do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/metrics?range=1y", "admin-token", ""); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown range: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/metrics", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator read an asset's metrics: %d", w.Code)
	}

	// Pruning drops what is past the retention and keeps the rest.
	old := now.Add(-historyRetention - time.Hour).Truncate(historyBucket)
	if _, err := s.pool.Exec(ctx, `INSERT INTO usage_history (hostname, bucket, samples) VALUES ($1, $2, 1)`,
		asset.Hostname, old); err != nil {
		t.Fatal(err)
	}
	if err := s.pruneUsageHistory(ctx, now); err != nil {
		t.Fatal(err)
	}
	var left, gone int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE bucket > $2), count(*) FILTER (WHERE bucket <= $2)
		FROM usage_history WHERE hostname = $1`, asset.Hostname, old).Scan(&left, &gone)
	if left != 2 || gone != 0 {
		t.Errorf("after prune: %d kept, %d old left", left, gone)
	}
}

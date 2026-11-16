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

	if w := do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/metrics?range=2y", "admin-token", ""); w.Code != http.StatusBadRequest {
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

func TestUsageHistoryKeepsLoadStealConnectionsAndDisks(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	facts := `{"hostname":"` + asset.Hostname + `","os":"Ubuntu 24.04.1 LTS","system":{"cpus":4}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, facts); w.Code != http.StatusOK {
		t.Fatalf("facts: %d %s", w.Code, w.Body)
	}
	b0 := time.Now().UTC().Truncate(historyBucket).Add(-time.Hour)
	beat := func(at time.Time, extra string) {
		t.Helper()
		body := fmt.Sprintf(`{"hostname":%q,"version":"0.9.9","usage":{"at":%q,"cpu_busy":10,`+
			`"mem_total":1000,"mem_available":500,"swap_total":0,"swap_free":0,"disks":[]%s}}`,
			asset.Hostname, at.Format(time.RFC3339), extra)
		if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body); w.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
		}
	}
	beat(b0.Add(30*time.Second), `,"load":[6,1,1],"steal":10,"tcp_conns":120,`+
		`"disk_io":[{"name":"nvme0n1","read_ops":100,"write_ops":50,"read_bps":4000,"write_bps":2000},{"name":"nvme1n1","read_ops":10}]`)
	beat(b0.Add(90*time.Second), `,"load":[2,1,1],"steal":30,"tcp_conns":80,`+
		`"disk_io":[{"name":"nvme0n1","read_ops":40,"write_ops":10,"read_bps":1000,"write_bps":1000}]`)
	// An older agent: none of the new figures, which is not zero.
	beat(b0.Add(25*time.Minute), ``)
	// A disk on its first sighting has no rate; the report adds no activity.
	beat(b0.Add(26*time.Minute), `,"disk_io":[{"name":"nvme0n1"}],"steal":250,"tcp_conns":-3`)

	m, err := s.MetricsFor(context.Background(), asset.ID, "6h", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Points) != 2 {
		t.Fatalf("points = %+v", m.Points)
	}
	p := m.Points[0]
	// The busiest load (6) over 4 processors.
	if p.LoadPerCore == nil || *p.LoadPerCore != 1.5 {
		t.Errorf("load per core = %v, want 1.5", p.LoadPerCore)
	}
	if p.Steal == nil || *p.Steal != 20 || p.StealMax == nil || *p.StealMax != 30 {
		t.Errorf("steal = %v (max %v), want 20 and 30", p.Steal, p.StealMax)
	}
	if p.TCP == nil || *p.TCP != 120 {
		t.Errorf("tcp = %v, want the most open at once, 120", p.TCP)
	}
	// Disks summed per report (110 and 40 read ops), then averaged.
	if p.ReadOps == nil || *p.ReadOps != 75 || *p.WriteOps != 30 || *p.ReadBps != 2500 || *p.WriteBps != 1500 {
		t.Errorf("disk = r %v w %v rb %v wb %v", p.ReadOps, p.WriteOps, p.ReadBps, p.WriteBps)
	}
	if p.IOPSMax == nil || *p.IOPSMax != 160 || p.IOBpsMax == nil || *p.IOBpsMax != 6000 {
		t.Errorf("busiest disk moment = %v ops, %v B/s, want 160 and 6000", p.IOPSMax, p.IOBpsMax)
	}
	q := m.Points[1]
	if q.LoadPerCore != nil || q.Steal != nil || q.TCP != nil || q.ReadOps != nil || q.IOPSMax != nil {
		t.Errorf("an agent that sent none of them, or nothing that can be true, shows them anyway: %+v", q)
	}
}

func TestUsageRollsUpByTheHour(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	ctx := context.Background()
	asset, cred := enrolledAsset(t, s, h)
	beat := `{"hostname":"` + asset.Hostname + `","version":"0.9.9"}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, beat); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	now := time.Now().UTC()
	// Two buckets in an hour 40 days ago (already past the 5-minute
	// retention by the time it is pruned) and one in the hour now running.
	hour := now.Truncate(time.Hour).Add(-40 * 24 * time.Hour)
	ins := func(at time.Time, cpu float64, cpuMax float32, disks string) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, `INSERT INTO usage_history (hostname, bucket, samples, cpu_sum, cpu_n, cpu_max, disks, tcp_max)
			VALUES ($1, $2, 5, $3, 5, $4, $5::jsonb, 7)`, asset.Hostname, at, cpu*5, cpuMax, disks); err != nil {
			t.Fatal(err)
		}
	}
	// The first roll-up reaches back only as far as 5-minute buckets are
	// kept, so the old hour is put in place the way an earlier roll-up
	// would have, and a later one must leave it alone.
	ins(now.Truncate(time.Hour).Add(-2*time.Hour), 20, 30, `{"/":40}`)
	ins(now.Truncate(time.Hour).Add(-2*time.Hour+10*time.Minute), 40, 90, `{"/":41}`)
	ins(now.Truncate(historyBucket), 99, 99, `{"/":99}`)
	if _, err := s.pool.Exec(ctx, `INSERT INTO usage_hourly (hostname, bucket, samples, cpu_sum, cpu_n) VALUES ($1, $2, 12, 120, 12)`,
		asset.Hostname, hour); err != nil {
		t.Fatal(err)
	}
	for range 2 { // twice: a roll-up run again changes nothing
		if err := s.pruneUsageHistory(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.MetricsFor(ctx, asset.ID, "90d", now)
	if err != nil {
		t.Fatal(err)
	}
	if m.StepSeconds != 6*3600 || len(m.Points) != 2 {
		t.Fatalf("90d = %+v, want the old hour and the finished one, not the hour still running", m.Points)
	}
	old, recent := m.Points[0], m.Points[1]
	if old.CPU == nil || *old.CPU != 10 || old.Samples != 12 {
		t.Errorf("old hour = %+v, want it kept as it was", old)
	}
	if recent.Samples != 10 || recent.CPU == nil || *recent.CPU != 30 || *recent.CPUMax != 90 ||
		recent.Disks["/"] != 41 || recent.TCP == nil || *recent.TCP != 7 {
		t.Errorf("rolled-up hour = %+v, want 10 samples averaging 30, peak 90, the last disk reading, 7 connections", recent)
	}

	// Past 13 months an hour goes.
	if _, err := s.pool.Exec(ctx, `INSERT INTO usage_hourly (hostname, bucket, samples) VALUES ($1, $2, 1)`,
		asset.Hostname, now.Add(-hourlyRetention-2*time.Hour).Truncate(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.pruneUsageHistory(ctx, now); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM usage_hourly WHERE hostname = $1`, asset.Hostname).Scan(&n)
	if n != 2 {
		t.Errorf("hours kept = %d, want 2", n)
	}
	if m, _ := s.MetricsFor(ctx, asset.ID, "1y", now); m.StepSeconds != 86400 || len(m.Points) != 2 {
		t.Errorf("1y = %+v", m)
	}
}

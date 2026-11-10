package control

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

/*
How an asset's resources have been over time: the Metrics tab's charts.

Each usage report (about one a minute, system.go) is folded into a 5-minute
bucket per agent in the same transaction as the heartbeat that carries it,
so history costs one upsert a minute per agent and no new round trip.
Buckets are kept 30 days and pruned hourly by the resource watch.

	GET /api/v1/assets/{id}/metrics?range=6h|24h|7d|30d

A bucket with no report is absent from the answer, never zero: the console
breaks the line there rather than draw figures nobody measured.
*/

const (
	historyBucket    = 5 * time.Minute
	historyRetention = 30 * 24 * time.Hour
	// maxHistoryMounts bounds the disks kept per bucket, as the report is.
	maxHistoryMounts = maxSystemDisks
)

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// recordUsageHistory folds one usage sample into its bucket.
func recordUsageHistory(ctx context.Context, db execer, hostname string, at time.Time, u *HostUsage) error {
	bucket := at.UTC().Truncate(historyBucket)
	var (
		cpuSum, rxSum, txSum                  float64
		cpuN, netN                            int
		cpuMax, memPct, swapPct, rxMax, txMax *float64
		disks                                 []byte
	)
	if u.CPUBusy != nil {
		v := *u.CPUBusy
		cpuSum, cpuN, cpuMax = v, 1, &v
	}
	if u.MemTotal > 0 {
		v := float64(u.MemTotal-u.MemAvailable) / float64(u.MemTotal) * 100
		memPct = &v
	}
	if u.SwapTotal > 0 {
		v := float64(u.SwapTotal-u.SwapFree) / float64(u.SwapTotal) * 100
		swapPct = &v
	}
	if len(u.Disks) > 0 {
		m := make(map[string]float64, min(len(u.Disks), maxHistoryMounts))
		for _, d := range u.Disks {
			if len(m) == maxHistoryMounts {
				break
			}
			if usable := d.Used + d.Avail; usable > 0 {
				m[d.Mount] = math.Round(float64(d.Used)/float64(usable)*1000) / 10
			}
		}
		disks, _ = json.Marshal(m)
	}
	var rx, tx float64
	var any bool
	for _, n := range u.Net {
		if n.RxBps != nil {
			rx += *n.RxBps
			any = true
		}
		if n.TxBps != nil {
			tx += *n.TxBps
			any = true
		}
	}
	if any {
		rxSum, txSum, netN, rxMax, txMax = rx, tx, 1, &rx, &tx
	}
	_, err := db.Exec(ctx, `
		INSERT INTO usage_history AS h (hostname, bucket, samples, cpu_sum, cpu_n, cpu_max, mem_pct, swap_pct,
		                                disks, rx_sum, tx_sum, net_n, rx_max, tx_max)
		VALUES ($1, $2, 1, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11, $12, $13)
		ON CONFLICT (hostname, bucket) DO UPDATE SET
			samples  = h.samples + 1,
			cpu_sum  = h.cpu_sum + EXCLUDED.cpu_sum,
			cpu_n    = h.cpu_n + EXCLUDED.cpu_n,
			cpu_max  = GREATEST(h.cpu_max, EXCLUDED.cpu_max),
			mem_pct  = GREATEST(h.mem_pct, EXCLUDED.mem_pct),
			swap_pct = GREATEST(h.swap_pct, EXCLUDED.swap_pct),
			disks    = COALESCE(EXCLUDED.disks, h.disks),
			rx_sum   = h.rx_sum + EXCLUDED.rx_sum,
			tx_sum   = h.tx_sum + EXCLUDED.tx_sum,
			net_n    = h.net_n + EXCLUDED.net_n,
			rx_max   = GREATEST(h.rx_max, EXCLUDED.rx_max),
			tx_max   = GREATEST(h.tx_max, EXCLUDED.tx_max)`,
		hostname, bucket, cpuSum, cpuN, cpuMax, memPct, swapPct, nullJSON(disks), rxSum, txSum, netN, rxMax, txMax)
	return err
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// pruneUsageHistory drops buckets older than the retention.
func (s *Store) pruneUsageHistory(ctx context.Context, now time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM usage_history WHERE bucket < $1`, now.Add(-historyRetention))
	return err
}

// metricsRanges are the ranges the Metrics tab offers and the step of each.
var metricsRanges = map[string]struct{ span, step time.Duration }{
	"6h":  {6 * time.Hour, 5 * time.Minute},
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
	"30d": {30 * 24 * time.Hour, 2 * time.Hour},
}

// MetricsPoint is one step of history. Nil means not reported in it.
type MetricsPoint struct {
	At      time.Time          `json:"at"`
	CPU     *float64           `json:"cpu"`
	CPUMax  *float64           `json:"cpuMax"`
	Mem     *float64           `json:"mem"`
	Swap    *float64           `json:"swap"`
	Disks   map[string]float64 `json:"disks"`
	Rx      *float64           `json:"rx"`
	Tx      *float64           `json:"tx"`
	RxMax   *float64           `json:"rxMax"`
	TxMax   *float64           `json:"txMax"`
	Samples int                `json:"samples"`
}

// AssetMetrics is the Metrics tab's history.
type AssetMetrics struct {
	Range       string    `json:"range"`
	StepSeconds int       `json:"stepSeconds"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	// Reported is false when no agent reports for the asset.
	Reported bool `json:"reported"`
	// Mounts is every disk seen in the range, fullest last seen first.
	Mounts []string       `json:"mounts"`
	Points []MetricsPoint `json:"points"`
}

// errBadRange is a range the tab does not offer.
var errBadRange = errors.New("range must be 6h, 24h, 7d or 30d")

// MetricsFor reads an asset's history over a range ending now.
func (s *Store) MetricsFor(ctx context.Context, id, rng string, now time.Time) (AssetMetrics, error) {
	r, ok := metricsRanges[rng]
	if !ok {
		return AssetMetrics{}, errBadRange
	}
	to := now.UTC()
	from := to.Add(-r.span).Truncate(r.step)
	out := AssetMetrics{Range: rng, StepSeconds: int(r.step / time.Second), From: from, To: to,
		Mounts: []string{}, Points: []MetricsPoint{}}

	var host *string
	err := s.db(ctx).QueryRow(ctx, `
		SELECT ag.hostname
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT hostname FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.id::text = $1 AND a.archived_at IS NULL`, id).Scan(&host)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrAssetNotFound
	}
	if err != nil {
		return out, err
	}
	if host == nil {
		return out, nil
	}
	out.Reported = true

	rows, err := s.db(ctx).Query(ctx, `
		SELECT bucket, samples, cpu_sum, cpu_n, cpu_max, mem_pct, swap_pct, disks, rx_sum, tx_sum, net_n, rx_max, tx_max
		  FROM usage_history
		 WHERE hostname = $1 AND bucket >= $2 AND bucket <= $3
		 ORDER BY bucket`, *host, from, to)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	type acc struct {
		p                    MetricsPoint
		cpuSum, rxSum, txSum float64
		cpuN, netN           int
	}
	var cur *acc
	flush := func() {
		if cur == nil {
			return
		}
		if cur.cpuN > 0 {
			v := round1(cur.cpuSum / float64(cur.cpuN))
			cur.p.CPU = &v
		}
		if cur.netN > 0 {
			rx, tx := math.Round(cur.rxSum/float64(cur.netN)), math.Round(cur.txSum/float64(cur.netN))
			cur.p.Rx, cur.p.Tx = &rx, &tx
		}
		out.Points = append(out.Points, cur.p)
		cur = nil
	}
	maxp := func(a *float64, b *float32) *float64 {
		if b == nil {
			return a
		}
		v := round1(float64(*b))
		if a == nil || v > *a {
			return &v
		}
		return a
	}
	maxf := func(a, b *float64) *float64 {
		if b == nil {
			return a
		}
		if a == nil || *b > *a {
			v := math.Round(*b)
			return &v
		}
		return a
	}
	lastFull := map[string]float64{}
	for rows.Next() {
		var (
			bucket                  time.Time
			samples, cpuN, netN     int
			cpuSum, rxSum, txSum    float64
			cpuMax, memPct, swapPct *float32
			disks                   []byte
			rxMax, txMax            *float64
		)
		if err := rows.Scan(&bucket, &samples, &cpuSum, &cpuN, &cpuMax, &memPct, &swapPct, &disks,
			&rxSum, &txSum, &netN, &rxMax, &txMax); err != nil {
			return out, err
		}
		step := bucket.UTC().Truncate(r.step)
		if cur == nil || !cur.p.At.Equal(step) {
			flush()
			cur = &acc{p: MetricsPoint{At: step}}
		}
		cur.p.Samples += samples
		cur.cpuSum, cur.cpuN = cur.cpuSum+cpuSum, cur.cpuN+cpuN
		cur.rxSum, cur.txSum, cur.netN = cur.rxSum+rxSum, cur.txSum+txSum, cur.netN+netN
		cur.p.CPUMax = maxp(cur.p.CPUMax, cpuMax)
		cur.p.Mem = maxp(cur.p.Mem, memPct)
		cur.p.Swap = maxp(cur.p.Swap, swapPct)
		cur.p.RxMax = maxf(cur.p.RxMax, rxMax)
		cur.p.TxMax = maxf(cur.p.TxMax, txMax)
		if len(disks) > 0 {
			var m map[string]float64
			if json.Unmarshal(disks, &m) == nil {
				if cur.p.Disks == nil {
					cur.p.Disks = map[string]float64{}
				}
				for k, v := range m {
					if v > cur.p.Disks[k] {
						cur.p.Disks[k] = v
					}
					lastFull[k] = v
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	flush()
	for k := range lastFull {
		out.Mounts = append(out.Mounts, k)
	}
	sort.Slice(out.Mounts, func(i, j int) bool {
		a, b := out.Mounts[i], out.Mounts[j]
		if lastFull[a] != lastFull[b] {
			return lastFull[a] > lastFull[b]
		}
		return a < b
	})
	if len(out.Mounts) > maxHistoryMounts {
		out.Mounts = out.Mounts[:maxHistoryMounts]
	}
	return out, nil
}

// getMetrics serves the Metrics tab. Fleet roles only, like the System card.
func (a *API) getMetrics(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing an asset's metrics requires the admin, owner or auditor role")
		return
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "24h"
	}
	out, err := a.store.MetricsFor(r.Context(), r.PathValue("id"), rng, time.Now())
	if errors.Is(err, errBadRange) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		a.failAsset(w, "metrics", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

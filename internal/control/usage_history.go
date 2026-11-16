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
Buckets are kept 30 days and pruned hourly by the resource watch, which
first rolls them up by the hour into usage_hourly, kept 13 months: the
90-day and 1-year ranges read that, so a reservation or a right-sizing is
judged on a year rather than a month.

	GET /api/v1/assets/{id}/metrics?range=6h|24h|7d|30d|90d|1y

A bucket with no report is absent from the answer, never zero: the console
breaks the line there rather than draw figures nobody measured.
*/

const (
	historyBucket    = 5 * time.Minute
	historyRetention = 30 * 24 * time.Hour
	// hourlyRetention is 13 months: a year to judge a one-year commitment
	// on, and a month either side of it.
	hourlyRetention = 395 * 24 * time.Hour
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
	var loadMax, stealMax *float64
	var stealSum float64
	var stealN int
	if u.Load != nil {
		v := u.Load[0]
		loadMax = &v
	}
	if u.Steal != nil {
		v := *u.Steal
		stealSum, stealN, stealMax = v, 1, &v
	}
	var tcp *int64
	if u.TCPConns != nil {
		v := int64(*u.TCPConns)
		tcp = &v
	}
	io := sumDiskIO(u.DiskIO)
	_, err := db.Exec(ctx, `
		INSERT INTO usage_history AS h (hostname, bucket, samples, cpu_sum, cpu_n, cpu_max, mem_pct, swap_pct,
		                                disks, rx_sum, tx_sum, net_n, rx_max, tx_max,
		                                load_max, steal_sum, steal_n, steal_max, tcp_max,
		                                rops_sum, wops_sum, rbps_sum, wbps_sum, io_n, iops_max, io_bps_max)
		VALUES ($1, $2, 1, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11, $12, $13,
		        $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
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
			tx_max   = GREATEST(h.tx_max, EXCLUDED.tx_max),
			load_max   = GREATEST(h.load_max, EXCLUDED.load_max),
			steal_sum  = h.steal_sum + EXCLUDED.steal_sum,
			steal_n    = h.steal_n + EXCLUDED.steal_n,
			steal_max  = GREATEST(h.steal_max, EXCLUDED.steal_max),
			tcp_max    = GREATEST(h.tcp_max, EXCLUDED.tcp_max),
			rops_sum   = h.rops_sum + EXCLUDED.rops_sum,
			wops_sum   = h.wops_sum + EXCLUDED.wops_sum,
			rbps_sum   = h.rbps_sum + EXCLUDED.rbps_sum,
			wbps_sum   = h.wbps_sum + EXCLUDED.wbps_sum,
			io_n       = h.io_n + EXCLUDED.io_n,
			iops_max   = GREATEST(h.iops_max, EXCLUDED.iops_max),
			io_bps_max = GREATEST(h.io_bps_max, EXCLUDED.io_bps_max)`,
		hostname, bucket, cpuSum, cpuN, cpuMax, memPct, swapPct, nullJSON(disks), rxSum, txSum, netN, rxMax, txMax,
		loadMax, stealSum, stealN, stealMax, tcp,
		io.rops, io.wops, io.rbps, io.wbps, io.n, io.opsMax, io.bpsMax)
	return err
}

// diskIOSum is one report's disk activity summed over its disks, the way
// the console charts it: a server's disks together, against what its
// volumes are provisioned for.
type diskIOSum struct {
	rops, wops, rbps, wbps float64
	n                      int
	opsMax, bpsMax         *float64
}

// sumDiskIO adds up the disks that reported a rate. A disk on its first
// sighting has none and adds nothing; a report where no disk had one is no
// report (n 0), never zero activity.
func sumDiskIO(in []HostDiskIO) diskIOSum {
	var s diskIOSum
	var any bool
	val := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		any = true
		return *p
	}
	for _, d := range in {
		s.rops += val(d.ReadOps)
		s.wops += val(d.WriteOps)
		s.rbps += val(d.ReadBps)
		s.wbps += val(d.WriteBps)
	}
	if !any {
		return diskIOSum{}
	}
	ops, bps := s.rops+s.wops, s.rbps+s.wbps
	s.n, s.opsMax, s.bpsMax = 1, &ops, &bps
	return s
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// pruneUsageHistory rolls the finished hours up into usage_hourly, then
// drops 5-minute buckets older than 30 days and hours older than 13 months.
func (s *Store) pruneUsageHistory(ctx context.Context, now time.Time) error {
	if err := s.rollUpUsage(ctx, now); err != nil {
		return err
	}
	if _, err := s.db(ctx).Exec(ctx, `DELETE FROM usage_history WHERE bucket < $1`, now.Add(-historyRetention)); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM usage_hourly WHERE bucket < $1`, now.Add(-hourlyRetention))
	return err
}

// rollUpUsage folds each finished hour of 5-minute buckets into one row.
// It starts an hour before the newest hour already rolled up, so a late
// report folded into a finished hour is counted, and an hour is rewritten
// whole each time, so running twice changes nothing. The first run rolls
// up everything still kept. Disks are the hour's last reading per mount
// set, which is what a fullness chart over a year needs.
func (s *Store) rollUpUsage(ctx context.Context, now time.Time) error {
	until := now.UTC().Truncate(time.Hour)
	var since time.Time
	err := s.db(ctx).QueryRow(ctx, `
		SELECT COALESCE(max(bucket) - interval '1 hour', $1) FROM usage_hourly`,
		until.Add(-historyRetention)).Scan(&since)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `
		INSERT INTO usage_hourly AS u (hostname, bucket, samples, cpu_sum, cpu_n, cpu_max, mem_pct, swap_pct,
		                               disks, rx_sum, tx_sum, net_n, rx_max, tx_max,
		                               load_max, steal_sum, steal_n, steal_max, tcp_max,
		                               rops_sum, wops_sum, rbps_sum, wbps_sum, io_n, iops_max, io_bps_max)
		SELECT hostname, date_trunc('hour', bucket), sum(samples), sum(cpu_sum), sum(cpu_n), max(cpu_max),
		       max(mem_pct), max(swap_pct),
		       (array_agg(disks ORDER BY bucket DESC) FILTER (WHERE disks IS NOT NULL))[1],
		       sum(rx_sum), sum(tx_sum), sum(net_n), max(rx_max), max(tx_max),
		       max(load_max), sum(steal_sum), sum(steal_n), max(steal_max), max(tcp_max),
		       sum(rops_sum), sum(wops_sum), sum(rbps_sum), sum(wbps_sum), sum(io_n), max(iops_max), max(io_bps_max)
		  FROM usage_history
		 WHERE bucket >= $1 AND bucket < $2
		 GROUP BY hostname, date_trunc('hour', bucket)
		ON CONFLICT (hostname, bucket) DO UPDATE SET
			samples = EXCLUDED.samples, cpu_sum = EXCLUDED.cpu_sum, cpu_n = EXCLUDED.cpu_n,
			cpu_max = EXCLUDED.cpu_max, mem_pct = EXCLUDED.mem_pct, swap_pct = EXCLUDED.swap_pct,
			disks = EXCLUDED.disks, rx_sum = EXCLUDED.rx_sum, tx_sum = EXCLUDED.tx_sum, net_n = EXCLUDED.net_n,
			rx_max = EXCLUDED.rx_max, tx_max = EXCLUDED.tx_max, load_max = EXCLUDED.load_max,
			steal_sum = EXCLUDED.steal_sum, steal_n = EXCLUDED.steal_n, steal_max = EXCLUDED.steal_max,
			tcp_max = EXCLUDED.tcp_max, rops_sum = EXCLUDED.rops_sum, wops_sum = EXCLUDED.wops_sum,
			rbps_sum = EXCLUDED.rbps_sum, wbps_sum = EXCLUDED.wbps_sum, io_n = EXCLUDED.io_n,
			iops_max = EXCLUDED.iops_max, io_bps_max = EXCLUDED.io_bps_max`,
		since.UTC().Truncate(time.Hour), until)
	return err
}

// metricsRanges are the ranges the Metrics tab offers and the step of each.
// The long ranges read the hourly roll-up.
var metricsRanges = map[string]struct {
	span, step time.Duration
	hourly     bool
}{
	"6h":  {6 * time.Hour, 5 * time.Minute, false},
	"24h": {24 * time.Hour, 5 * time.Minute, false},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute, false},
	"30d": {30 * 24 * time.Hour, 2 * time.Hour, false},
	"90d": {90 * 24 * time.Hour, 6 * time.Hour, true},
	"1y":  {365 * 24 * time.Hour, 24 * time.Hour, true},
}

// MetricsPoint is one step of history. Nil means not reported in it.
type MetricsPoint struct {
	At     time.Time          `json:"at"`
	CPU    *float64           `json:"cpu"`
	CPUMax *float64           `json:"cpuMax"`
	Mem    *float64           `json:"mem"`
	Swap   *float64           `json:"swap"`
	Disks  map[string]float64 `json:"disks"`
	Rx     *float64           `json:"rx"`
	Tx     *float64           `json:"tx"`
	RxMax  *float64           `json:"rxMax"`
	TxMax  *float64           `json:"txMax"`
	// LoadPerCore is the busiest 1-minute load average divided by the
	// processors: above 1, work was waiting for a processor. Nil where the
	// system keeps no load average (Windows) or its processors are unknown.
	LoadPerCore *float64 `json:"loadPerCore"`
	// Steal is the average share the hypervisor took, StealMax the busiest.
	Steal    *float64 `json:"steal"`
	StealMax *float64 `json:"stealMax"`
	// TCP is the most TCP connections open at once.
	TCP *float64 `json:"tcp"`
	// Disk activity over all physical disks: average operations and bytes
	// per second each way, and the busiest moment's totals.
	ReadOps  *float64 `json:"readOps"`
	WriteOps *float64 `json:"writeOps"`
	ReadBps  *float64 `json:"readBps"`
	WriteBps *float64 `json:"writeBps"`
	IOPSMax  *float64 `json:"iopsMax"`
	IOBpsMax *float64 `json:"ioBpsMax"`
	Samples  int      `json:"samples"`
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
	// Events are what changed on the server in the range, oldest first:
	// cloud changes Argus made, package updates and restarts.
	Events []MetricsEvent `json:"events"`
	// Cloud is the instance the server runs as, when one in a connected
	// account matches; nil otherwise.
	Cloud *MetricsCloud `json:"cloud"`
}

// errBadRange is a range the tab does not offer.
var errBadRange = errors.New("range must be 6h, 24h, 7d, 30d, 90d or 1y")

// MetricsFor reads an asset's history over a range ending now.
func (s *Store) MetricsFor(ctx context.Context, id, rng string, now time.Time) (AssetMetrics, error) {
	r, ok := metricsRanges[rng]
	if !ok {
		return AssetMetrics{}, errBadRange
	}
	to := now.UTC()
	from := to.Add(-r.span).Truncate(r.step)
	out := AssetMetrics{Range: rng, StepSeconds: int(r.step / time.Second), From: from, To: to,
		Mounts: []string{}, Points: []MetricsPoint{}, Events: []MetricsEvent{}}

	var host *string
	var cpus *int
	err := s.db(ctx).QueryRow(ctx, `
		SELECT ag.hostname, ag.cpus
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT hostname, NULLIF((g.system->>'cpus'), '')::int AS cpus FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.id::text = $1 AND a.archived_at IS NULL`, id).Scan(&host, &cpus)
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

	table := "usage_history"
	if r.hourly {
		table = "usage_hourly"
	}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT bucket, samples, cpu_sum, cpu_n, cpu_max, mem_pct, swap_pct, disks, rx_sum, tx_sum, net_n, rx_max, tx_max,
		       load_max, steal_sum, steal_n, steal_max, tcp_max,
		       rops_sum, wops_sum, rbps_sum, wbps_sum, io_n, iops_max, io_bps_max
		  FROM `+table+`
		 WHERE hostname = $1 AND bucket >= $2 AND bucket <= $3
		 ORDER BY bucket`, *host, from, to)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	type acc struct {
		p                                  MetricsPoint
		cpuSum, rxSum, txSum, stealSum     float64
		ropsSum, wopsSum, rbpsSum, wbpsSum float64
		cpuN, netN, stealN, ioN            int
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
		if cur.stealN > 0 {
			v := round1(cur.stealSum / float64(cur.stealN))
			cur.p.Steal = &v
		}
		if cur.ioN > 0 {
			n := float64(cur.ioN)
			ro, wo := round1(cur.ropsSum/n), round1(cur.wopsSum/n)
			rb, wb := math.Round(cur.rbpsSum/n), math.Round(cur.wbpsSum/n)
			cur.p.ReadOps, cur.p.WriteOps, cur.p.ReadBps, cur.p.WriteBps = &ro, &wo, &rb, &wb
		}
		if cur.p.LoadPerCore != nil {
			if cpus == nil || *cpus <= 0 {
				cur.p.LoadPerCore = nil
			} else {
				v := math.Round(*cur.p.LoadPerCore/float64(*cpus)*100) / 100
				cur.p.LoadPerCore = &v
			}
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
	// maxRaw keeps the largest value as read; flush and the console do the
	// rounding, so the load average is divided by the processors unrounded.
	maxRaw := func(a *float64, b *float32) *float64 {
		if b == nil {
			return a
		}
		v := float64(*b)
		if a == nil || v > *a {
			return &v
		}
		return a
	}
	lastFull := map[string]float64{}
	for rows.Next() {
		var (
			bucket                             time.Time
			samples, cpuN, netN, stealN, ioN   int
			cpuSum, rxSum, txSum, stealSum     float64
			ropsSum, wopsSum, rbpsSum, wbpsSum float64
			cpuMax, memPct, swapPct            *float32
			loadMax, stealMax                  *float32
			tcpMax                             *int32
			disks                              []byte
			rxMax, txMax, iopsMax, ioBpsMax    *float64
		)
		if err := rows.Scan(&bucket, &samples, &cpuSum, &cpuN, &cpuMax, &memPct, &swapPct, &disks,
			&rxSum, &txSum, &netN, &rxMax, &txMax,
			&loadMax, &stealSum, &stealN, &stealMax, &tcpMax,
			&ropsSum, &wopsSum, &rbpsSum, &wbpsSum, &ioN, &iopsMax, &ioBpsMax); err != nil {
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
		cur.stealSum, cur.stealN = cur.stealSum+stealSum, cur.stealN+stealN
		cur.ropsSum, cur.wopsSum = cur.ropsSum+ropsSum, cur.wopsSum+wopsSum
		cur.rbpsSum, cur.wbpsSum, cur.ioN = cur.rbpsSum+rbpsSum, cur.wbpsSum+wbpsSum, cur.ioN+ioN
		cur.p.LoadPerCore = maxRaw(cur.p.LoadPerCore, loadMax)
		cur.p.StealMax = maxp(cur.p.StealMax, stealMax)
		if tcpMax != nil {
			v := float64(*tcpMax)
			cur.p.TCP = maxf(cur.p.TCP, &v)
		}
		cur.p.IOPSMax = maxf(cur.p.IOPSMax, iopsMax)
		cur.p.IOBpsMax = maxf(cur.p.IOBpsMax, ioBpsMax)
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
	out.Events, err = s.hostEvents(ctx, *host, from, to)
	return out, err
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
	if err == nil {
		err = a.addMetricsCloud(r.Context(), r.PathValue("id"), &out)
	}
	if err != nil {
		a.failAsset(w, "metrics", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

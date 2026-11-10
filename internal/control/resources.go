package control

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

/*
When an asset is running low on something, decided once.

The agent reports usage about once a minute (system.go). LowResources turns
one report into warnings by the rule below, and everything that warns reads
that one answer: the System card, the asset list's Running low filter and
badges, and the bell. A rule repeated in the console would drift.

	GET /api/v1/resources-low    admins, owners, auditors

Only usage reported in the last resourcesFresh counts for the list and the
bell, so an agent that went quiet neither raises nor keeps a warning. The
card shows its own (possibly old) figures with their age, as it always has.
*/

// Resource names in a warning.
const (
	ResourceDisk   = "disk"
	ResourceMemory = "memory"
	ResourceSwap   = "swap"
	ResourceCPU    = "cpu"
)

// Warning levels.
const (
	LevelLow      = "low"
	LevelCritical = "critical"
)

// The thresholds. Disks are counted like df (used over used plus available,
// so the root reserve is not room), and match the card's colours since it
// first had them.
const (
	diskLowPct      = 90
	diskCriticalPct = 95
	memLowLeftPct   = 10
	memCritLeftPct  = 5
	swapLowPct      = 90
	cpuBusyPct      = 90
)

// resourcesFresh is how recent usage must be to warn on the list and bell.
const resourcesFresh = 5 * time.Minute

// ResourceWarning is one resource running low.
type ResourceWarning struct {
	Resource string `json:"resource"`
	Level    string `json:"level"`
	// Mount names the disk; empty for the others.
	Mount string `json:"mount,omitempty"`
	// Percent is how full (disk, swap), how busy (cpu) or, for memory, how
	// much is left, as a whole number the way the console writes it.
	Percent int `json:"percent"`
	// Left and Total are bytes (disk, memory, swap); 0 for the processor.
	Left  uint64 `json:"left"`
	Total uint64 `json:"total"`
}

// LowResources is every resource in u that is running low, critical first.
func LowResources(u HostUsage) []ResourceWarning {
	out := []ResourceWarning{}
	if u.MemTotal > 0 && u.MemAvailable <= u.MemTotal {
		left := float64(u.MemAvailable) / float64(u.MemTotal) * 100
		if left <= memLowLeftPct {
			lvl := LevelLow
			if left <= memCritLeftPct {
				lvl = LevelCritical
			}
			out = append(out, ResourceWarning{Resource: ResourceMemory, Level: lvl,
				Percent: int(math.Floor(left)), Left: u.MemAvailable, Total: u.MemTotal})
		}
	}
	for _, d := range u.Disks {
		usable := d.Used + d.Avail
		if usable == 0 || d.Used > usable {
			continue
		}
		pct := int(math.Ceil(float64(d.Used) / float64(usable) * 100))
		if pct < diskLowPct {
			continue
		}
		lvl := LevelLow
		if pct >= diskCriticalPct {
			lvl = LevelCritical
		}
		out = append(out, ResourceWarning{Resource: ResourceDisk, Level: lvl, Mount: d.Mount,
			Percent: min(pct, 100), Left: d.Avail, Total: d.Total})
	}
	if u.SwapTotal > 0 && u.SwapFree <= u.SwapTotal {
		used := u.SwapTotal - u.SwapFree
		pct := int(math.Ceil(float64(used) / float64(u.SwapTotal) * 100))
		if pct >= swapLowPct {
			out = append(out, ResourceWarning{Resource: ResourceSwap, Level: LevelLow,
				Percent: min(pct, 100), Left: u.SwapFree, Total: u.SwapTotal})
		}
	}
	if b := u.CPUBusy; b != nil && !math.IsNaN(*b) && *b >= cpuBusyPct && *b <= 100 {
		out = append(out, ResourceWarning{Resource: ResourceCPU, Level: LevelLow, Percent: int(math.Round(*b))})
	}
	order := map[string]int{ResourceMemory: 0, ResourceDisk: 1, ResourceSwap: 2, ResourceCPU: 3}
	slices.SortStableFunc(out, func(a, b ResourceWarning) int {
		if (a.Level == LevelCritical) != (b.Level == LevelCritical) {
			if a.Level == LevelCritical {
				return -1
			}
			return 1
		}
		return order[a.Resource] - order[b.Resource]
	})
	return out
}

// ResourcesLowRow is one asset with something running low.
type ResourcesLowRow struct {
	AssetID  string            `json:"assetId"`
	Hostname string            `json:"hostname"`
	UsageAt  time.Time         `json:"usageAt"`
	Warnings []ResourceWarning `json:"warnings"`
}

// ResourcesLow is the fleet's view.
type ResourcesLow struct {
	Assets []ResourcesLowRow `json:"assets"`
	// FreshSeconds is how recent usage had to be to count.
	FreshSeconds int `json:"freshSeconds"`
}

// ResourcesLow lists each asset whose recent usage has a warning, the ones
// with something critical first.
func (s *Store) ResourcesLow(ctx context.Context, now time.Time) (ResourcesLow, error) {
	out := ResourcesLow{Assets: []ResourcesLowRow{}, FreshSeconds: int(resourcesFresh / time.Second)}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, a.hostname, ag.usage, ag.usage_at
		  FROM assets a
		  JOIN LATERAL (
		       SELECT usage, usage_at FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL AND a.health <> 'unreachable'
		   AND ag.usage IS NOT NULL AND ag.usage_at > $1::timestamptz`, now.Add(-resourcesFresh))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, host string
			raw      []byte
			at       time.Time
		)
		if err := rows.Scan(&id, &host, &raw, &at); err != nil {
			return out, err
		}
		var u HostUsage
		if json.Unmarshal(raw, &u) != nil {
			continue
		}
		if w := LowResources(u); len(w) > 0 {
			out.Assets = append(out.Assets, ResourcesLowRow{AssetID: id, Hostname: host, UsageAt: at, Warnings: w})
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	crit := func(r ResourcesLowRow) bool { return r.Warnings[0].Level == LevelCritical }
	slices.SortFunc(out.Assets, func(a, b ResourcesLowRow) int {
		if crit(a) != crit(b) {
			if crit(a) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Hostname, b.Hostname)
	})
	return out, nil
}

func (a *API) registerResourcesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/resources-low", a.getResourcesLow)
	mux.HandleFunc("GET /api/v1/assets/{id}/metrics", a.getMetrics)
}

// getResourcesLow serves the fleet's warnings. Fleet roles only, like the
// System card the figures come from.
func (a *API) getResourcesLow(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "which assets are running low is for administrators, owners and auditors")
		return
	}
	v, err := a.store.ResourcesLow(r.Context(), time.Now())
	if err != nil {
		a.fail(w, "resources low", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

/* ── The bell ────────────────────────────────────────────────────────────── */

// ResourceWatchInterval is how often the bell looks for assets running low.
const ResourceWatchInterval = 5 * time.Minute

// maxResourceAlertsPerSweep bounds one sweep, so a fleet that fills at once
// (a shared NFS mount on every host) does not pour hundreds of alerts into
// the bell in one go; the rest follow on the next sweeps.
const maxResourceAlertsPerSweep = 25

// RunResourceWatch raises resource alerts for each organization until ctx
// ends.
func (a *API) RunResourceWatch(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(3 * time.Minute)
	defer t.Stop()
	var pruned time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now().UTC()
		prune := now.Sub(pruned) >= time.Hour
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			a.watchResources(tctx, now)
			if prune {
				if err := a.store.pruneUsageHistory(tctx, now); err != nil {
					a.log.Error("usage history prune", "error", err)
				}
			}
		})
		if prune {
			pruned = now
		}
		if err != nil {
			a.log.Error("resource watch", "error", err)
		}
		t.Reset(interval)
	}
}

// resourceSubject names one alert: an asset, a resource (and disk), a level
// and a day, so each is raised at most once a day and a disk that goes from
// low to critical is raised again.
func resourceSubject(assetID string, w ResourceWarning, day string) string {
	res := w.Resource
	if w.Mount != "" {
		res += ":" + clipText(w.Mount, 200)
	}
	return "resources:" + assetID + ":" + res + ":" + w.Level + ":" + day
}

// watchResources raises an alert for each disk or memory warning that has
// not had one today. A busy processor and a full swap are left to the card
// and the list: a minute at 95% is often just a build.
func (a *API) watchResources(ctx context.Context, now time.Time) {
	v, err := a.store.ResourcesLow(ctx, now)
	if err != nil {
		a.log.Error("resource watch: cannot read usage", "error", err)
		return
	}
	if len(v.Assets) == 0 {
		return
	}
	day := now.Format("2006-01-02")
	sent := map[string]bool{}
	rows, err := a.store.db(ctx).Query(ctx,
		`SELECT subject FROM console_alerts WHERE kind = $1 AND at >= $2`, AlertResources, now.Add(-36*time.Hour))
	if err != nil {
		a.log.Error("resource watch: cannot read today's alerts", "error", err)
		return
	}
	for rows.Next() {
		var s *string
		if rows.Scan(&s) == nil && s != nil {
			sent[*s] = true
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	raised := 0
	for _, r := range v.Assets {
		for _, w := range r.Warnings {
			if w.Resource != ResourceDisk && w.Resource != ResourceMemory {
				continue
			}
			subject := resourceSubject(r.AssetID, w, day)
			if sent[subject] {
				continue
			}
			if raised == maxResourceAlertsPerSweep {
				return
			}
			title, detail := resourceAlertText(r.Hostname, w)
			sev := "warning"
			if w.Level == LevelCritical {
				sev = "critical"
			}
			a.raiseAlert(ctx, ConsoleAlert{Kind: AlertResources, Severity: sev, Audience: AudienceAdmins,
				Title: title, Detail: detail, Link: "/assets/" + r.AssetID, Subject: subject})
			sent[subject] = true
			raised++
		}
	}
}

// resourceAlertText is an alert's title and detail.
func resourceAlertText(host string, w ResourceWarning) (string, string) {
	name := clipText(strings.SplitN(host, ".", 2)[0], 80)
	switch w.Resource {
	case ResourceMemory:
		t := name + ": memory running low"
		if w.Level == LevelCritical {
			t = name + ": memory almost gone"
		}
		return t, fmt.Sprintf("%s of %s left (%d%%). Open the asset to see how busy it is.",
			sizeText(w.Left), sizeText(w.Total), w.Percent)
	default:
		return fmt.Sprintf("%s: %s is %d%% full", name, clipText(w.Mount, 120), w.Percent),
			fmt.Sprintf("%s left of %s.", sizeText(w.Left), sizeText(w.Total))
	}
}

// sizeText writes a size the way the System card does.
func sizeText(n uint64) string {
	const gib = 1 << 30
	v := float64(n) / gib
	trim := func(s string) string { return strings.TrimSuffix(s, ".0") }
	switch {
	case v >= 1024:
		return trim(fmt.Sprintf("%.1f", v/1024)) + " TB"
	case v >= 100:
		return fmt.Sprintf("%.0f GB", v)
	case v >= 1:
		return trim(fmt.Sprintf("%.1f", v)) + " GB"
	}
	return fmt.Sprintf("%d MB", n>>20)
}

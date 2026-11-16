package control

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"time"
)

/*
The usage row on each Assets card: how busy the processor is, how much
memory is in use and how full the fullest disk is, from the agent's last
report. Everyone gets it for the assets they can see on that page:

	GET /api/v1/assets-usage    every role

Admins, owners and viewers see every asset on Assets, so they get every
asset's figures. Members and approvers see the assets given to them, by name
or through a team, so they get those and no others, by the same
heldAssignmentsSQL the page's own list is drawn from. A cache or a shortcut
that ignored who asked would hand one person's fleet to another.

Only three numbers and the running-low warnings leave here. Processes,
network, disk activity, the cloud instance and its cost stay with the
Metrics tab, which is still for the fleet roles. Like the running-low list,
only usage reported in the last resourcesFresh counts: a card does not show
an agent's last words from yesterday as the server's state now.
*/

// AssetUsageDisk is the fullest disk on an asset.
type AssetUsageDisk struct {
	Mount string `json:"mount"`
	// Percent is how full, counted like df and rounded up, as LowResources
	// counts it, so the bar and the badge cannot disagree.
	Percent int `json:"percent"`
}

// AssetUsageRow is one asset's figures for its card.
type AssetUsageRow struct {
	AssetID string    `json:"assetId"`
	UsageAt time.Time `json:"usageAt"`
	// CPU is how busy the processor was over the last minute, nil where
	// the agent did not say.
	CPU *int `json:"cpu"`
	// Memory is the share of memory in use, nil where it is not known.
	Memory *int `json:"memory"`
	// Disk is the fullest disk, nil where none was reported.
	Disk     *AssetUsageDisk   `json:"disk"`
	Warnings []ResourceWarning `json:"warnings"`
}

// AssetsUsage is every visible asset with fresh figures.
type AssetsUsage struct {
	Assets       []AssetUsageRow `json:"assets"`
	FreshSeconds int             `json:"freshSeconds"`
}

// usageFigures turns one report into the card's figures.
func usageFigures(u HostUsage) (cpu, mem *int, disk *AssetUsageDisk) {
	if b := u.CPUBusy; b != nil && !math.IsNaN(*b) && *b >= 0 && *b <= 100 {
		v := int(math.Round(*b))
		cpu = &v
	}
	if u.MemTotal > 0 && u.MemAvailable <= u.MemTotal {
		v := int(math.Round(float64(u.MemTotal-u.MemAvailable) / float64(u.MemTotal) * 100))
		mem = &v
	}
	for _, d := range u.Disks {
		usable := d.Used + d.Avail
		if usable == 0 || d.Used > usable {
			continue
		}
		pct := min(int(math.Ceil(float64(d.Used)/float64(usable)*100)), 100)
		if disk == nil || pct > disk.Percent {
			disk = &AssetUsageDisk{Mount: clipText(d.Mount, maxMountText), Percent: pct}
		}
	}
	return cpu, mem, disk
}

// AssetsUsage reads fresh usage for every live asset, or, when email is
// not empty, only for the assets that person holds.
func (s *Store) AssetsUsage(ctx context.Context, now time.Time, email string) (AssetsUsage, error) {
	out := AssetsUsage{Assets: []AssetUsageRow{}, FreshSeconds: int(resourcesFresh / time.Second)}
	q := `
		SELECT a.id::text, ag.usage, ag.usage_at
		  FROM assets a
		  JOIN LATERAL (
		       SELECT usage, usage_at FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL AND a.health <> 'unreachable'
		   AND ag.usage IS NOT NULL AND ag.usage_at > $2::timestamptz`
	args := []any{email, now.Add(-resourcesFresh)}
	if email == "" {
		// $1 unused: keep the numbering so one statement serves both.
		q += ` AND $1::text = ''`
	} else {
		q += ` AND a.id IN (SELECT asset_id FROM (` + heldAssignmentsSQL + `) g)`
	}
	rows, err := s.db(ctx).Query(ctx, q, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  string
			raw []byte
			at  time.Time
		)
		if err := rows.Scan(&id, &raw, &at); err != nil {
			return out, err
		}
		var u HostUsage
		if json.Unmarshal(raw, &u) != nil {
			continue
		}
		cpu, mem, disk := usageFigures(u)
		if cpu == nil && mem == nil && disk == nil {
			continue
		}
		out.Assets = append(out.Assets, AssetUsageRow{AssetID: id, UsageAt: at,
			CPU: cpu, Memory: mem, Disk: disk, Warnings: LowResources(u)})
	}
	return out, rows.Err()
}

// getAssetsUsage serves the Assets cards' usage row: the whole fleet for
// the roles that see it, otherwise only what the caller holds.
func (a *API) getAssetsUsage(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	scope := sess.Email
	if seesWholeFleet(sess.Role) {
		scope = ""
	} else if scope == "" {
		// No one to scope to is never the whole fleet.
		writeJSON(w, http.StatusOK, AssetsUsage{Assets: []AssetUsageRow{}, FreshSeconds: int(resourcesFresh / time.Second)})
		return
	}
	v, err := a.store.AssetsUsage(r.Context(), time.Now(), scope)
	if err != nil {
		a.fail(w, "assets usage", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

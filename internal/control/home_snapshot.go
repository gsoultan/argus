package control

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

/*
Home's live dashboard: what one snapshot holds, and how it is read.

Every figure is counted from the store or read from an agent's last usage
report. Nothing is projected. The snapshot is built by the home hub (see
home_hub.go) for the roles that see the whole fleet, at most once every few
seconds per organization however many people are watching, and only while
someone is.
*/

// HomeSnapshot is the whole of Home's live dashboard for the fleet roles.
type HomeSnapshot struct {
	Servers   HomeServers   `json:"servers"`
	Live      HomeLive      `json:"live"`
	Sessions  HomeSessions  `json:"sessions"`
	Waiting   HomeWaiting   `json:"waiting"`
	Processor HomeProcessor `json:"processor"`
	Disks     []HomeDisk    `json:"disks"`
	SignIns   HomeSignIns   `json:"signIns"`
	LiveList  []HomeSession `json:"liveList"`
	Audit     []AuditEvent  `json:"audit"`
	Trends    HomeTrends    `json:"trends"`
	// Unreviewed is agents reporting from outside the inventory, waiting
	// for someone to add or ignore them.
	Unreviewed int       `json:"unreviewedHosts"`
	At         time.Time `json:"at"`
}

// HomeServers counts every server in the inventory by whether it answers.
//
// Down is unreachable. Quiet answers but its agent has gone stale, so what it
// reports cannot be believed as current. Answering is the rest.
type HomeServers struct {
	Total     int `json:"total"`
	Answering int `json:"answering"`
	Quiet     int `json:"quiet"`
	Down      int `json:"down"`
}

// HomeLive counts sessions a reporter still speaks for.
type HomeLive struct {
	Count    int `json:"count"`
	Root     int `json:"root"`
	Database int `json:"database"`
	// Silent are active on paper and unheard from; never counted as live.
	Silent int `json:"silent"`
}

// HomeSessions is the last 24 hours of sessions, by hour and kind.
type HomeSessions struct {
	Last24h int `json:"last24h"`
	// WeekAgo is the same 24 hours a week earlier, for "against last week".
	WeekAgo int `json:"weekAgo"`
	// Hours is oldest first, 24 entries, each [terminal, desktop, database].
	// Files sessions count as terminal: they open from one.
	Hours [24][3]int `json:"hours"`
	// Kinds totals Hours by kind.
	Kinds [3]int `json:"kinds"`
}

// HomeWaiting is what waits on an approver.
type HomeWaiting struct {
	Count    int        `json:"count"`
	OldestAt *time.Time `json:"oldestAt"`
}

// HomeProcessor is the fleet's processor, from each agent's last report.
//
// Only reports younger than homeUsageFresh count: a figure from an agent that
// has stopped talking is not "now".
type HomeProcessor struct {
	// Average is nil when no server has a fresh report.
	Average   *float64   `json:"average"`
	Reporting int        `json:"reporting"`
	Busiest   []HomeBusy `json:"busiest"`
}

// HomeBusy is one of the busiest servers.
type HomeBusy struct {
	AssetID string  `json:"assetId"`
	Name    string  `json:"name"`
	Busy    float64 `json:"busy"`
}

// HomeDisk is a filesystem at homeDiskWarn or fuller, counted like df.
type HomeDisk struct {
	AssetID string  `json:"assetId"`
	Name    string  `json:"name"`
	Mount   string  `json:"mount"`
	Used    float64 `json:"used"`
}

// HomeSignIns is the last 24 hours of signing in to the console.
type HomeSignIns struct {
	Last24h  int `json:"last24h"`
	Failed24 int `json:"failed24h"`
	// Failed15m is failed sign-ins in the last 15 minutes, for "Needs you".
	Failed15m int `json:"failed15m"`
	// Hours is successful sign-ins by hour, oldest first.
	Hours [24]int `json:"hours"`
}

// HomeSession is one live session, as Live now shows it.
type HomeSession struct {
	ID        string    `json:"id"`
	User      string    `json:"user"`
	Principal string    `json:"principal"`
	Host      string    `json:"host"`
	Kind      string    `json:"kind"`
	StartedAt time.Time `json:"startedAt"`
}

// HomeTrends is the last hour of four numbers, one point a minute, oldest
// first. Held in memory by the hub: Since says where they begin, so a line
// shorter than an hour is labelled rather than padded.
type HomeTrends struct {
	Since     *time.Time `json:"since"`
	Answering []int      `json:"answering"`
	Live      []int      `json:"live"`
	Waiting   []int      `json:"waiting"`
	Processor []*float64 `json:"processor"`
}

const (
	// homeUsageFresh is the System card's rule: an older usage report is a
	// past, not a present.
	homeUsageFresh = 3 * time.Minute
	homeDiskWarn   = 90.0
	homeBusiest    = 5
	homeDisksMax   = 5
	homeLiveMax    = 8
	homeAuditMax   = 6
)

// homeFast reads what changes from second to second: servers, live sessions,
// waiting approvals, the latest audit entries.
func (s *Store) homeFast(ctx context.Context, h *HomeSnapshot) error {
	silence := fmt.Sprintf("%d seconds", int(SessionSilenceThreshold.Seconds()))
	var held int
	var heldAt *time.Time
	err := s.db(ctx).QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM assets WHERE archived_at IS NULL),
		  (SELECT count(*) FROM assets WHERE archived_at IS NULL AND health = 'unreachable'),
		  (SELECT count(*) FROM assets WHERE archived_at IS NULL AND health <> 'unreachable' AND agent_state = 'stale'),
		  (SELECT count(*) FROM sessions WHERE state = 'active' AND last_reported_at <= now() - $1::interval),
		  (SELECT count(*) FROM access_requests WHERE state = 'pending'),
		  (SELECT min(created_at) FROM access_requests WHERE state = 'pending'),
		  (SELECT count(*) FROM direct_login_requests WHERE state = 'pending' AND expires_at > now()),
		  (SELECT min(created_at) FROM direct_login_requests WHERE state = 'pending' AND expires_at > now()),
		  (SELECT count(*) FROM agents WHERE matched_asset = false AND enrollment_state = 'unreviewed')`,
		silence).Scan(&h.Servers.Total, &h.Servers.Down, &h.Servers.Quiet, &h.Live.Silent,
		&h.Waiting.Count, &h.Waiting.OldestAt, &held, &heldAt, &h.Unreviewed)
	if err != nil {
		return err
	}
	// Held direct logins wait on an approver too: the same sum as the menu
	// badge.
	h.Waiting.Count += held
	if heldAt != nil && (h.Waiting.OldestAt == nil || heldAt.Before(*h.Waiting.OldestAt)) {
		h.Waiting.OldestAt = heldAt
	}
	h.Servers.Answering = max(0, h.Servers.Total-h.Servers.Down-h.Servers.Quiet)

	rows, err := s.db(ctx).Query(ctx, `
		SELECT id::text, user_email, principal, asset_hostname, kind, started_at
		  FROM sessions
		 WHERE state = 'active' AND last_reported_at > now() - $1::interval
		 ORDER BY started_at DESC`, silence)
	if err != nil {
		return err
	}
	h.LiveList = []HomeSession{}
	for rows.Next() {
		var x HomeSession
		if err := rows.Scan(&x.ID, &x.User, &x.Principal, &x.Host, &x.Kind, &x.StartedAt); err != nil {
			rows.Close()
			return err
		}
		h.Live.Count++
		if isAdminAccount(x.Principal) {
			h.Live.Root++
		}
		if x.Kind == "database" {
			h.Live.Database++
		}
		if len(h.LiveList) < homeLiveMax {
			x.StartedAt = x.StartedAt.UTC()
			h.LiveList = append(h.LiveList, x)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	h.Audit, err = s.AuditEvents(ctx, homeAuditMax, 0)
	if err != nil {
		return err
	}
	// The hashes are for verifying the chain, which Home does not do.
	for i := range h.Audit {
		h.Audit[i].PrevHash, h.Audit[i].Hash = "", ""
	}
	return nil
}

// isAdminAccount is whether a session runs as the machine's administrator.
func isAdminAccount(p string) bool {
	return p == "root" || strings.EqualFold(p, "administrator")
}

// homeSlow reads what moves on the scale of a minute: sessions and sign-ins
// by hour, and the fleet's usage, which agents report about once a minute.
func (s *Store) homeSlow(ctx context.Context, h *HomeSnapshot, now time.Time) error {
	// Hour buckets end at now, so the last one is the hour so far.
	start := now.Add(-24 * time.Hour)
	rows, err := s.db(ctx).Query(ctx, `
		SELECT floor(extract(epoch FROM started_at - $1::timestamptz) / 3600)::int AS b, kind, count(*)
		  FROM sessions
		 WHERE started_at > $1::timestamptz AND started_at <= $2::timestamptz
		 GROUP BY 1, 2`, start, now)
	if err != nil {
		return err
	}
	h.Sessions = HomeSessions{}
	for rows.Next() {
		var b, n int
		var kind string
		if err := rows.Scan(&b, &kind, &n); err != nil {
			rows.Close()
			return err
		}
		if b < 0 || b > 23 {
			continue
		}
		k := 0
		switch kind {
		case "desktop":
			k = 1
		case "database":
			k = 2
		}
		h.Sessions.Hours[b][k] += n
		h.Sessions.Kinds[k] += n
		h.Sessions.Last24h += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := s.db(ctx).QueryRow(ctx, `
		SELECT count(*) FROM sessions
		 WHERE started_at > $1::timestamptz - interval '7 days'
		   AND started_at <= $2::timestamptz - interval '7 days'`, start, now).Scan(&h.Sessions.WeekAgo); err != nil {
		return err
	}

	h.SignIns = HomeSignIns{}
	rows, err = s.db(ctx).Query(ctx, `
		SELECT floor(extract(epoch FROM at - $1::timestamptz) / 3600)::int, action,
		       at > $2::timestamptz - interval '15 minutes', count(*)
		  FROM audit_events
		 WHERE at > $1::timestamptz AND at <= $2::timestamptz
		   AND action IN ('auth.login', 'auth.password_failed', 'auth.mfa_failed')
		 GROUP BY 1, 2, 3`, start, now)
	if err != nil {
		return err
	}
	for rows.Next() {
		var b, n int
		var action string
		var recent bool
		if err := rows.Scan(&b, &action, &recent, &n); err != nil {
			rows.Close()
			return err
		}
		if action == "auth.login" {
			h.SignIns.Last24h += n
			if b >= 0 && b < 24 {
				h.SignIns.Hours[b] += n
			}
			continue
		}
		h.SignIns.Failed24 += n
		if recent {
			h.SignIns.Failed15m += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return s.homeUsage(ctx, h, now)
}

// homeUsage reads each server's last usage report, matched to its agent the
// way the System card matches it.
func (s *Store) homeUsage(ctx context.Context, h *HomeSnapshot, now time.Time) error {
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
		   AND ag.usage IS NOT NULL AND ag.usage_at > $1::timestamptz`, now.Add(-homeUsageFresh))
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		sum   float64
		busy  []HomeBusy
		disks []HomeDisk
	)
	h.Processor = HomeProcessor{Busiest: []HomeBusy{}}
	for rows.Next() {
		var id, name string
		var raw []byte
		var at *time.Time
		if err := rows.Scan(&id, &name, &raw, &at); err != nil {
			return err
		}
		var u HostUsage
		if json.Unmarshal(raw, &u) != nil {
			continue
		}
		if u.CPUBusy != nil && *u.CPUBusy >= 0 && *u.CPUBusy <= 100 {
			sum += *u.CPUBusy
			h.Processor.Reporting++
			busy = append(busy, HomeBusy{AssetID: id, Name: name, Busy: round1(*u.CPUBusy)})
		}
		for _, d := range u.Disks {
			// Like df: used over used plus available, so the root reserve
			// does not count as room.
			if d.Used+d.Avail == 0 {
				continue
			}
			pct := float64(d.Used) / float64(d.Used+d.Avail) * 100
			if pct >= homeDiskWarn {
				disks = append(disks, HomeDisk{AssetID: id, Name: name, Mount: clipText(d.Mount, 120), Used: round1(pct)})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if h.Processor.Reporting > 0 {
		avg := round1(sum / float64(h.Processor.Reporting))
		h.Processor.Average = &avg
	}
	slices.SortFunc(busy, func(a, b HomeBusy) int {
		if a.Busy != b.Busy {
			if a.Busy > b.Busy {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	h.Processor.Busiest = append(h.Processor.Busiest, busy[:min(len(busy), homeBusiest)]...)
	slices.SortFunc(disks, func(a, b HomeDisk) int {
		if a.Used != b.Used {
			if a.Used > b.Used {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name+a.Mount, b.Name+b.Mount)
	})
	h.Disks = append([]HomeDisk{}, disks[:min(len(disks), homeDisksMax)]...)
	return nil
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

package control

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Suggestions from Argus's own metrics: a server that has been busy gets
"add a server behind its load balancer" when it is behind one, or "resize
up" when it is alone (one bigger server is simpler than two); a server idle
for two weeks gets "resize down". Read from the usage history every agent
already reports, for servers Argus has matched to a cloud instance, so AWS
is never asked and nothing costs money.

A suggestion never acts. It is shown on the server's Overview and Metrics
tabs and on Cloud › Scaling, and once a day per server in the bell; pressing
it opens the change it names with the answers filled in, which then goes
through the usual plan and approval.

	GET /api/v1/cloud/suggestions       every suggestion, Cloud roles
	GET /api/v1/assets/{id}/suggestion  one server's, or null
*/

// The rules. Busy is an hour's processor average at or over busyCPU, or
// its memory at or over busyMem; a server is busy when busyHours of the
// last 24 were, out of at least minHours24 reported. Idle is every hour of
// the last 14 days under idleCPU and idleMem, out of at least minHours14.
const (
	busyCPU    = 80.0
	busyMem    = 90.0
	busyHours  = 6
	minHours24 = 12
	idleCPU    = 10.0
	idleMem    = 60.0
	idleDays   = 14
	minHours14 = 12 * 24
)

// Suggestion kinds.
const (
	SuggestAddServer  = "add-server"
	SuggestResizeUp   = "resize-up"
	SuggestResizeDown = "resize-down"
)

// usageHour is one hour of a server's history: its processor average and
// its fullest memory, nil when not reported.
type usageHour struct {
	At  time.Time
	CPU *float64
	Mem *float64
}

// serverLoad is what the rules read from a server's hours.
type serverLoad struct {
	// Hours24 is how many of the last 24 hours reported; BusyHours how many
	// were busy, CPUHours and MemHours by which.
	Hours24, BusyHours, CPUHours, MemHours int
	// PeakCPU and PeakMem are the busiest hour's, over the last 24.
	PeakCPU, PeakMem float64
	// Hours14 is how many of the last 14 days' hours reported; MaxCPU14 and
	// MaxMem14 the busiest of them.
	Hours14            int
	MaxCPU14, MaxMem14 float64
}

func loadOf(hours []usageHour, now time.Time) serverLoad {
	var l serverLoad
	day := now.Add(-24 * time.Hour)
	for _, h := range hours {
		if h.CPU == nil && h.Mem == nil {
			continue
		}
		cpu, mem := 0.0, 0.0
		if h.CPU != nil {
			cpu = *h.CPU
		}
		if h.Mem != nil {
			mem = *h.Mem
		}
		l.Hours14++
		l.MaxCPU14, l.MaxMem14 = max(l.MaxCPU14, cpu), max(l.MaxMem14, mem)
		if h.At.Before(day) {
			continue
		}
		l.Hours24++
		l.PeakCPU, l.PeakMem = max(l.PeakCPU, cpu), max(l.PeakMem, mem)
		if cpu >= busyCPU {
			l.CPUHours++
		}
		if mem >= busyMem {
			l.MemHours++
		}
		if cpu >= busyCPU || mem >= busyMem {
			l.BusyHours++
		}
	}
	return l
}

func (l serverLoad) busy() bool { return l.Hours24 >= minHours24 && l.BusyHours >= busyHours }
func (l serverLoad) idle() bool {
	return l.Hours14 >= minHours14 && l.MaxCPU14 < idleCPU && l.MaxMem14 < idleMem
}

// sizeSteps are the sizes in a family, smallest first.
var sizeSteps = []string{"nano", "micro", "small", "medium", "large", "xlarge", "2xlarge", "4xlarge", "8xlarge", "12xlarge", "16xlarge"}

// nextSize is the size one step up (or down) in the same family that the
// price estimate knows, or "".
func nextSize(size string, up bool) string {
	fam, step, ok := strings.Cut(size, ".")
	if !ok {
		return ""
	}
	i := slices.Index(sizeSteps, step)
	if i < 0 {
		return ""
	}
	for {
		if up {
			i++
		} else {
			i--
		}
		if i < 0 || i >= len(sizeSteps) {
			return ""
		}
		s := fam + "." + sizeSteps[i]
		if _, ok := cloud.HourlyPrice(s); ok {
			return s
		}
	}
}

// monthly is a size's on-demand price a month, when the estimate knows it.
func monthly(size string) (float64, bool) {
	p, ok := cloud.HourlyPrice(size)
	return cents(p * cloud.HoursPerMonth), ok
}

// ServerSuggestion is one suggestion for one server.
type ServerSuggestion struct {
	Kind     string `json:"kind"`
	AssetID  string `json:"assetId"`
	Hostname string `json:"hostname"`
	// Title and Detail say what was seen and what would help, in words.
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// Instance is the server's cloud instance, which the change acts on.
	Instance CloudItem `json:"instance"`
	// Size is what to resize to, for a resize.
	Size string `json:"size,omitempty"`
	// Balancer and TargetGroup are what an added server goes behind.
	Balancer    string `json:"balancer,omitempty"`
	TargetGroup string `json:"targetGroup,omitempty"`
	// Monthly is what the suggestion costs a month (negative when it saves),
	// when the estimate knows the sizes.
	Monthly *float64 `json:"monthly,omitempty"`
	// Load is what the rules saw.
	Load SuggestionLoad `json:"load"`
}

// SuggestionLoad is the evidence a suggestion shows.
type SuggestionLoad struct {
	BusyHours int     `json:"busyHours"`
	Hours     int     `json:"hours"`
	PeakCPU   float64 `json:"peakCpu"`
	PeakMem   float64 `json:"peakMem"`
	// Days is how many days an idle server was read over.
	Days int `json:"days,omitempty"`
}

// behind is the load balancer and target group an instance is behind.
type behind struct{ balancer, targetGroup string }

// suggest is the suggestion for one server, or nil.
func suggest(host string, l serverLoad, inst CloudItem, lb *behind) *ServerSuggestion {
	r := inst.Resource
	if r.State != "running" {
		return nil
	}
	s := &ServerSuggestion{AssetID: inst.Server.ID, Hostname: host, Instance: inst,
		Load: SuggestionLoad{BusyHours: l.BusyHours, Hours: l.Hours24, PeakCPU: cents(l.PeakCPU), PeakMem: cents(l.PeakMem)}}
	// price is the difference a resize makes a month.
	price := func(from, to string) {
		a, ok1 := monthly(from)
		b, ok2 := monthly(to)
		if ok1 && ok2 {
			v := cents(b - a)
			s.Monthly = &v
		}
	}
	switch {
	case l.busy():
		what := fmt.Sprintf("over %.0f%% processor", busyCPU)
		if l.CPUHours == 0 {
			what = fmt.Sprintf("over %.0f%% memory", busyMem)
		} else if l.MemHours > 0 {
			what += " or memory"
		}
		s.Title = fmt.Sprintf("%s has been %s for %d of the last 24 hours", host, what, l.BusyHours)
		if lb != nil {
			s.Kind, s.Balancer, s.TargetGroup = SuggestAddServer, lb.balancer, lb.targetGroup
			s.Detail = fmt.Sprintf("It is behind %s. Another %s behind it would share the load", lb.balancer, r.Size)
			if m, ok := monthly(r.Size); ok {
				s.Monthly = &m
				s.Detail += fmt.Sprintf(", for about %s a month", money(m))
			}
			s.Detail += "."
			return s
		}
		to := nextSize(r.Size, true)
		if to == "" {
			return nil
		}
		s.Kind, s.Size = SuggestResizeUp, to
		price(r.Size, to)
		s.Detail = fmt.Sprintf("It is not behind a load balancer, so one bigger server is simpler than two: resize it to %s", to)
		if s.Monthly != nil {
			s.Detail += fmt.Sprintf(", about %s a month more", money(*s.Monthly))
		}
		s.Detail += ". Resizing stops and starts it."
		return s
	case l.idle():
		to := nextSize(r.Size, false)
		if to == "" {
			return nil
		}
		s.Kind, s.Size = SuggestResizeDown, to
		s.Load.Days = idleDays
		price(r.Size, to)
		s.Title = fmt.Sprintf("%s has been idle for %d days", host, idleDays)
		s.Detail = fmt.Sprintf("Its processor stayed under %.0f%% and memory under %.0f%% every hour. %s would do", idleCPU, idleMem, to)
		if s.Monthly != nil {
			s.Detail += fmt.Sprintf(" and saves about %s a month", money(-*s.Monthly))
		}
		s.Detail += ". Resizing stops and starts it."
		return s
	}
	return nil
}

// cents rounds to the cent.
func cents(f float64) float64 { return math.Round(f*100) / 100 }

// serverSuggestions works out the suggestions for every server matched to
// a cloud instance, or for one asset when only is set.
func (a *API) serverSuggestions(ctx context.Context, now time.Time, only string) ([]ServerSuggestion, error) {
	items, err := a.store.CloudResources(ctx, cloud.KindInstance)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	assets, err := a.store.Assets(ctx)
	if err != nil {
		return nil, err
	}
	reported, err := a.store.AgentAddresses(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := a.store.AgentInstances(ctx)
	if err != nil {
		return nil, err
	}
	matchServers(items, assets, reported, instances)
	var mine []CloudItem
	ids := []string{}
	for _, it := range items {
		if it.Server == nil || (only != "" && it.Server.ID != only) || it.Resource.State != "running" {
			continue
		}
		mine = append(mine, it)
		ids = append(ids, it.Server.ID)
	}
	if len(mine) == 0 {
		return nil, nil
	}
	hours, hosts, err := a.store.usageHours(ctx, ids, now.Add(-idleDays*24*time.Hour))
	if err != nil {
		return nil, err
	}
	groups, err := a.store.CloudResources(ctx, cloud.KindTargetGroup)
	if err != nil {
		return nil, err
	}
	out := []ServerSuggestion{}
	for _, it := range mine {
		host := hosts[it.Server.ID]
		if host == "" {
			host = it.Server.Hostname
		}
		if s := suggest(host, loadOf(hours[it.Server.ID], now), it, behindOf(it, groups)); s != nil {
			out = append(out, *s)
		}
	}
	slices.SortStableFunc(out, func(x, y ServerSuggestion) int { return strings.Compare(x.Hostname, y.Hostname) })
	return out, nil
}

// behindOf is the load balancer an instance is a target of, through a
// target group in its account; nil when none.
func behindOf(it CloudItem, groups []CloudItem) *behind {
	for _, g := range groups {
		if g.AccountID != it.AccountID || g.Resource.Region != it.Resource.Region {
			continue
		}
		in, lb := false, ""
		for _, l := range g.Resource.Links {
			if l.Kind == cloud.KindInstance && l.ID == it.Resource.ID {
				in = true
			}
			if l.Kind == cloud.KindLoadBalancer && lb == "" {
				lb = l.Name
				if lb == "" {
					lb = l.ID
				}
			}
		}
		if in && lb != "" {
			return &behind{balancer: lb, targetGroup: g.Resource.ID}
		}
	}
	return nil
}

// usageHours reads the hourly processor average and fullest memory of each
// asset's agent since since, and the agent's hostname.
func (s *Store) usageHours(ctx context.Context, assetIDs []string, since time.Time) (map[string][]usageHour, map[string]string, error) {
	rows, err := s.db(ctx).Query(ctx, `
		WITH h AS (
		  SELECT a.id::text AS asset, ag.hostname
		    FROM assets a
		    JOIN LATERAL (
		         SELECT hostname FROM agents g
		          WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		             OR g.hostname = split_part(a.hostname, '.', 1)
		          ORDER BY g.last_seen_at DESC NULLS LAST
		          LIMIT 1) ag ON TRUE
		   WHERE a.id::text = ANY($1) AND a.archived_at IS NULL)
		SELECT h.asset, h.hostname, date_trunc('hour', u.bucket) AS hr,
		       sum(u.cpu_sum) / NULLIF(sum(u.cpu_n), 0), max(u.mem_pct)
		  FROM h JOIN usage_history u ON u.hostname = h.hostname AND u.bucket >= $2
		 GROUP BY 1, 2, 3
		 ORDER BY 1, 3`, assetIDs, since)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out, hosts := map[string][]usageHour{}, map[string]string{}
	for rows.Next() {
		var id, host string
		var h usageHour
		var mem *float32
		if err := rows.Scan(&id, &host, &h.At, &h.CPU, &mem); err != nil {
			return nil, nil, err
		}
		if mem != nil {
			v := float64(*mem)
			h.Mem = &v
		}
		hosts[id] = host
		out[id] = append(out[id], h)
	}
	return out, hosts, rows.Err()
}

func (a *API) getCloudSuggestions(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	out, err := a.serverSuggestions(r.Context(), time.Now().UTC(), "")
	if err != nil {
		a.fail(w, "suggestions", err)
		return
	}
	if out == nil {
		out = []ServerSuggestion{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
}

func (a *API) getAssetSuggestion(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := a.store.AssetByID(ctx, id); err != nil {
		a.failAsset(w, "suggestion", err)
		return
	}
	out, err := a.serverSuggestions(ctx, time.Now().UTC(), id)
	if err != nil {
		a.fail(w, "suggestion", err)
		return
	}
	var s *ServerSuggestion
	if len(out) > 0 {
		s = &out[0]
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"suggestion": s})
}

// maxSuggestionAlertsPerSweep bounds one sweep, like the resource alerts.
const maxSuggestionAlertsPerSweep = 25

func suggestionSubject(s ServerSuggestion, day string) string {
	return "suggestion:" + s.AssetID + ":" + s.Kind + ":" + day
}

// watchSuggestions puts each suggestion in the bell once a day per server.
func (a *API) watchSuggestions(ctx context.Context, now time.Time) {
	list, err := a.serverSuggestions(ctx, now, "")
	if err != nil {
		a.log.Error("suggestion watch", "error", err)
		return
	}
	if len(list) == 0 {
		return
	}
	day := now.Format("2006-01-02")
	sent := map[string]bool{}
	rows, err := a.store.db(ctx).Query(ctx,
		`SELECT subject FROM console_alerts WHERE kind = $1 AND at >= $2`, AlertCloud, now.Add(-36*time.Hour))
	if err != nil {
		a.log.Error("suggestion watch: cannot read today's alerts", "error", err)
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
	for _, s := range list {
		subject := suggestionSubject(s, day)
		if sent[subject] {
			continue
		}
		if raised == maxSuggestionAlertsPerSweep {
			return
		}
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertCloud, Severity: "info", Audience: AudienceAdmins,
			Title: s.Title, Detail: s.Detail + " Argus suggests this; nothing changes until someone asks for it.",
			Link: "/assets/" + s.AssetID + "?tab=metrics", Subject: subject})
		sent[subject] = true
		raised++
	}
}

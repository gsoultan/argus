package control

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/gsoultan/caduceus"
)

/*
Which programs used a server, over time: the Metrics tab's "Busiest
programs".

Each usage report names the programs using the machine most (the agent adds
up the processes sharing a name, system.go's HostProcess). recordUsageHistory
keeps them with the 5-minute bucket, in usage_history.programs, as each
program's busiest moment in the bucket: the highest processor share, memory,
disk read and write rates and process count any report gave it. Peaks rather
than averages, because a program that is absent from a report was not among
the busiest then, not idle: an average over the reports that named it would
be an average of its worst moments, and one over all of them would invent
zeros.

	GET /api/v1/assets/{id}/programs?from=<RFC 3339>&to=<RFC 3339>

answers the same peaks over any stretch of the last 30 days, by program,
with how many of the stretch's 5-minute periods named it. Fleet roles only,
like the history itself. The hourly roll-up keeps no programs, so the 90-day
and 1-year ranges have none.
*/

const (
	// maxProgramsAnswer bounds the programs one answer lists.
	maxProgramsAnswer = 30
	// programsTop is how many programs the answer keeps by each measure.
	programsTop = 10
)

// programPeak is one program's busiest moment in a bucket, as stored. Short
// keys, because a bucket holds up to fifteen of these a report, every five
// minutes, for 30 days.
type programPeak struct {
	CPU    *float64 `json:"c,omitempty"`
	Memory float64  `json:"m"`
	Read   *float64 `json:"r,omitempty"`
	Write  *float64 `json:"w,omitempty"`
	Count  int      `json:"n"`
	Hidden bool     `json:"h,omitempty"`
}

// programsJSON is one report's programs as a bucket keeps them; nil when the
// report named none. top has been through boundedTop.
func programsJSON(top []HostProcess) []byte {
	if len(top) == 0 {
		return nil
	}
	m := make(map[string]programPeak, len(top))
	for _, p := range top {
		q := programPeak{CPU: p.CPU, Memory: p.Memory, Read: p.ReadBps, Write: p.WriteBps, Count: max(1, p.Count), Hidden: p.IOHidden}
		if o, ok := m[p.Name]; ok { // a name twice: keep the larger of each
			q = maxPeak(o, q)
		}
		m[p.Name] = q
	}
	b, _ := json.Marshal(m)
	return b
}

func maxPeak(a, b programPeak) programPeak {
	hi := func(x, y *float64) *float64 {
		if x == nil || (y != nil && *y > *x) {
			return y
		}
		return x
	}
	return programPeak{CPU: hi(a.CPU, b.CPU), Memory: math.Max(a.Memory, b.Memory), Read: hi(a.Read, b.Read),
		Write: hi(a.Write, b.Write), Count: max(a.Count, b.Count), Hidden: a.Hidden || b.Hidden}
}

// mergePrograms folds a report's programs into its bucket's in the upsert:
// for each name the larger of each figure. jsonb_strip_nulls drops a figure
// neither side had, so a missing rate stays missing rather than becoming 0.
const mergePrograms = `CASE
	WHEN EXCLUDED.programs IS NULL THEN h.programs
	WHEN h.programs IS NULL THEN EXCLUDED.programs
	ELSE (SELECT jsonb_object_agg(k, o) FROM (
		SELECT k, jsonb_strip_nulls(jsonb_build_object(
			'c', max((v->>'c')::float8), 'm', max((v->>'m')::float8),
			'r', max((v->>'r')::float8), 'w', max((v->>'w')::float8),
			'n', max((v->>'n')::int), 'h', CASE WHEN bool_or((v->>'h')::boolean) THEN true END)) AS o
		  FROM (SELECT * FROM jsonb_each(h.programs) UNION ALL SELECT * FROM jsonb_each(EXCLUDED.programs)) e(k, v)
		 GROUP BY k) g)
	END`

// ProgramUse is one program over a stretch of history: its busiest moment
// by each measure, which need not be the same moment.
type ProgramUse struct {
	Name string `json:"name"`
	// CPU is the highest share of the whole machine's processor; null when
	// no report had one.
	CPU    *float64 `json:"cpu"`
	Memory float64  `json:"memory"`
	// ReadBps and WriteBps are the highest bytes per second; null when the
	// agent did not measure them (an older agent, or the system refused it:
	// Hidden).
	ReadBps  *float64 `json:"readBps"`
	WriteBps *float64 `json:"writeBps"`
	Hidden   bool     `json:"ioHidden"`
	// Count is the most processes it ran as at once.
	Count int `json:"count"`
	// Periods is how many of the stretch's 5-minute periods named it.
	Periods int `json:"periods"`
}

// AssetPrograms is the answer to GET /api/v1/assets/{id}/programs.
type AssetPrograms struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Periods is how many 5-minute periods in the stretch had programs
	// reported: 0 means nothing to show, whatever the reason.
	Periods  int          `json:"periods"`
	Programs []ProgramUse `json:"programs"`
}

var errBadStretch = errors.New("from and to must be RFC 3339 times, from before to, within the last 30 days")

// ProgramsFor reads an asset's busiest programs between from and to.
func (s *Store) ProgramsFor(ctx context.Context, id string, from, to, now time.Time) (AssetPrograms, error) {
	from, to = from.UTC(), to.UTC()
	out := AssetPrograms{From: from, To: to, Programs: []ProgramUse{}}
	if !from.Before(to) || to.After(now.Add(time.Hour)) || from.Before(now.Add(-historyRetention-historyBucket)) {
		return out, errBadStretch
	}
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
	if errors.Is(err, caduceus.ErrNoRows) {
		return out, ErrAssetNotFound
	}
	if err != nil || host == nil {
		return out, err
	}
	err = s.db(ctx).QueryRow(ctx, `
		SELECT count(*) FROM usage_history
		 WHERE hostname = $1 AND bucket >= $2 AND bucket < $3 AND programs IS NOT NULL`,
		*host, from, to).Scan(&out.Periods)
	if err != nil || out.Periods == 0 {
		return out, err
	}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT e.k, max((e.v->>'c')::float8), COALESCE(max((e.v->>'m')::float8), 0),
		       max((e.v->>'r')::float8), max((e.v->>'w')::float8), COALESCE(bool_or((e.v->>'h')::boolean), false),
		       COALESCE(max((e.v->>'n')::int), 1), count(*)
		  FROM usage_history h CROSS JOIN LATERAL jsonb_each(h.programs) e(k, v)
		 WHERE h.hostname = $1 AND h.bucket >= $2 AND h.bucket < $3 AND h.programs IS NOT NULL
		 GROUP BY e.k`, *host, from, to)
	if err != nil {
		return out, err
	}
	all, err := caduceus.CollectRows(rows, func(r caduceus.CollectableRow) (ProgramUse, error) {
		var p ProgramUse
		err := r.Scan(&p.Name, &p.CPU, &p.Memory, &p.ReadBps, &p.WriteBps, &p.Hidden, &p.Count, &p.Periods)
		return p, err
	})
	if err != nil {
		return out, err
	}
	out.Programs = pickPrograms(all)
	return out, nil
}

// pickPrograms keeps the programsTop busiest by processor, by memory and by
// disk, at most maxProgramsAnswer, busiest by processor first. A program
// with disk hidden in one period and measured in another keeps its
// measured peak, and says some was hidden.
func pickPrograms(all []ProgramUse) []ProgramUse {
	val := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	disk := func(p ProgramUse) float64 { return max(val(p.ReadBps), 0) + max(val(p.WriteBps), 0) }
	keep := map[string]bool{}
	by := func(less func(a, b ProgramUse) bool, ok func(ProgramUse) bool) {
		sort.SliceStable(all, func(i, j int) bool { return less(all[i], all[j]) })
		for i, p := range all {
			if i == programsTop || !ok(p) {
				break
			}
			keep[p.Name] = true
		}
	}
	byName := func(a, b ProgramUse) bool { return a.Name < b.Name }
	sort.Slice(all, func(i, j int) bool { return byName(all[i], all[j]) })
	by(func(a, b ProgramUse) bool { return val(a.CPU) > val(b.CPU) }, func(p ProgramUse) bool { return p.CPU != nil })
	by(func(a, b ProgramUse) bool { return a.Memory > b.Memory }, func(ProgramUse) bool { return true })
	by(func(a, b ProgramUse) bool { return disk(a) > disk(b) }, func(p ProgramUse) bool { return disk(p) > 0 })
	out := make([]ProgramUse, 0, len(keep))
	sort.SliceStable(all, func(i, j int) bool {
		if val(all[i].CPU) != val(all[j].CPU) {
			return val(all[i].CPU) > val(all[j].CPU)
		}
		return all[i].Memory > all[j].Memory
	})
	for _, p := range all {
		if keep[p.Name] && len(out) < maxProgramsAnswer {
			out = append(out, p)
		}
	}
	return out
}

// getPrograms serves the Metrics tab's busiest programs. Fleet roles only,
// like the history it is part of.
func (a *API) getPrograms(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing an asset's programs requires the admin, owner or auditor role")
		return
	}
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, errBadStretch.Error())
		return
	}
	out, err := a.store.ProgramsFor(r.Context(), r.PathValue("id"), from, to, time.Now())
	if errors.Is(err, errBadStretch) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		a.failAsset(w, "programs", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

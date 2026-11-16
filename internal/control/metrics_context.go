package control

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
What the Metrics charts are read against: what changed on the server in the
range, the limits of the instance and volumes it runs on, and what the
instance cost by the hour.

Events come from Argus's own records (cloud changes it applied, package
updates it ran, the agent's last boot). A change made in the cloud's console
or by another tool is not among them, and the tab says so.

Limits are the instance size's baseline network bandwidth and the sum of
its volumes' provisioned IOPS and throughput. A limit is left out when it is
not known for every part, so a line is never drawn too low.

Cost is the size's on-demand list price (us-east-1, cloud.HourlyPrice), as
steps: the instance's state and size now, walked back through the start,
stop and resize changes Argus applied in the range. Nothing while stopped.
*/

// maxMetricsEvents bounds the markers on a chart.
const maxMetricsEvents = 200

// MetricsEvent is one marker on the charts.
type MetricsEvent struct {
	At time.Time `json:"at"`
	// Kind is "cloud", "update" or "restart".
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	By      string `json:"by"`
}

// MetricsCloud is the instance under a server, for the charts.
type MetricsCloud struct {
	InstanceID   string `json:"instanceId"`
	InstanceType string `json:"instanceType"`
	// NetworkBps is the size's baseline bandwidth, in bytes per second.
	NetworkBps *float64 `json:"networkBps"`
	// DiskIOPS and DiskBps are the attached volumes' provisioned totals.
	DiskIOPS *float64 `json:"diskIops"`
	DiskBps  *float64 `json:"diskBps"`
	// Cost is the price per hour in US dollars, in steps, oldest first.
	Cost []CostStep `json:"cost"`
}

// CostStep is the price per hour from a time until the next step. PerHour
// is nil where the size's price is not known, or before the instance was
// launched.
type CostStep struct {
	From    time.Time `json:"from"`
	PerHour *float64  `json:"perHour"`
}

// hostEvents reads the package updates and the last boot of the server an
// agent reports as, in a range.
func (s *Store) hostEvents(ctx context.Context, host string, from, to time.Time) ([]MetricsEvent, error) {
	out := []MetricsEvent{}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT finished_at, state, requested_by, jsonb_array_length(upgraded), rebooted
		  FROM package_runs
		 WHERE hostname = $1 AND state IN ('done', 'failed') AND finished_at >= $2 AND finished_at <= $3
		 ORDER BY finished_at DESC
		 LIMIT $4`, host, from, to, maxMetricsEvents)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var at time.Time
		var state, by string
		var n int
		var rebooted bool
		if err := rows.Scan(&at, &state, &by, &n, &rebooted); err != nil {
			return out, err
		}
		e := MetricsEvent{At: at, Kind: "update", By: by, Summary: "Package update failed"}
		if state == "done" {
			e.Summary = "Updated " + strconv.Itoa(n) + " package" + plural(n)
			if rebooted {
				e.Summary += " and restarted"
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	var boot *time.Time
	err = s.db(ctx).QueryRow(ctx, `
		SELECT NULLIF(system->>'booted_at', '')::timestamptz FROM agents WHERE hostname = $1`, host).Scan(&boot)
	if err == nil && boot != nil && !boot.Before(from) && !boot.After(to) {
		out = append(out, MetricsEvent{At: *boot, Kind: "restart", Summary: "Server started"})
	}
	sortEvents(out)
	return out, nil
}

func sortEvents(es []MetricsEvent) {
	sort.SliceStable(es, func(i, j int) bool { return es[i].At.Before(es[j].At) })
}

// appliedChange is a cloud change Argus applied to an instance.
type appliedChange struct {
	At      time.Time
	Action  cloud.Action
	Summary string
	By      string
	Spec    json.RawMessage
}

// addMetricsCloud adds the instance a server runs as, when one matches,
// with its limits, cost and the cloud changes in the range.
func (a *API) addMetricsCloud(ctx context.Context, id string, out *AssetMetrics) error {
	instances, err := a.store.AgentInstances(ctx)
	if err != nil {
		return err
	}
	inst, err := a.assetInstance(ctx, id, instances)
	if err != nil || inst == nil {
		return err
	}
	vols, err := a.store.CloudResources(ctx, cloud.KindVolume)
	if err != nil {
		return err
	}
	changes, err := a.store.appliedChanges(ctx, inst.AccountID, inst.Resource.ID, out.From)
	if err != nil {
		return err
	}
	c := &MetricsCloud{InstanceID: inst.Resource.ID, InstanceType: inst.Resource.Size}
	if v, ok := cloud.NetworkBaseline(inst.Resource.Size); ok {
		c.NetworkBps = &v
	}
	c.DiskIOPS, c.DiskBps = volumeLimits(inst, vols)
	c.Cost = costSteps(inst.Resource.State, inst.Resource.Size, changes, out.From)
	out.Cloud = c
	for _, ch := range changes {
		if ch.At.After(out.To) {
			continue
		}
		out.Events = append(out.Events, MetricsEvent{At: ch.At, Kind: "cloud", Summary: ch.Summary, By: ch.By})
	}
	sortEvents(out.Events)
	if len(out.Events) > maxMetricsEvents {
		out.Events = out.Events[len(out.Events)-maxMetricsEvents:]
	}
	return nil
}

// appliedChanges reads the changes Argus applied to an instance since a
// time, oldest first.
func (s *Store) appliedChanges(ctx context.Context, account, instance string, since time.Time) ([]appliedChange, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT applied_at, action, summary, requested_by, spec
		  FROM cloud_changes
		 WHERE account_id::text = $1 AND resource_id = $2 AND state = 'applied' AND applied_at >= $3
		 ORDER BY applied_at
		 LIMIT $4`, account, instance, since, maxMetricsEvents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []appliedChange
	for rows.Next() {
		var c appliedChange
		var action string
		if err := rows.Scan(&c.At, &action, &c.Summary, &c.By, &c.Spec); err != nil {
			return nil, err
		}
		c.Action = cloud.Action(action)
		out = append(out, c)
	}
	return out, rows.Err()
}

// volumeLimits sums the provisioned IOPS and throughput of the volumes
// attached to an instance. Each is nil unless every volume states it.
func volumeLimits(inst *CloudItem, vols []CloudItem) (iops, bps *float64) {
	var sumIOPS, sumMiB float64
	n, haveIOPS, haveMiB := 0, true, true
	for _, v := range vols {
		if v.AccountID != inst.AccountID || !linkedTo(v.Resource, inst.Resource.ID) {
			continue
		}
		n++
		if x, err := strconv.Atoi(v.Resource.Details["iops"]); err == nil && x > 0 {
			sumIOPS += float64(x)
		} else {
			haveIOPS = false
		}
		if x, err := strconv.Atoi(v.Resource.Details["throughput"]); err == nil && x > 0 {
			sumMiB += float64(x)
		} else {
			haveMiB = false
		}
	}
	if n == 0 {
		return nil, nil
	}
	if haveIOPS {
		iops = &sumIOPS
	}
	if haveMiB {
		b := sumMiB * (1 << 20)
		bps = &b
	}
	return iops, bps
}

// linkedTo is whether a volume is attached to an instance; a multi-attach
// volume names several.
func linkedTo(v cloud.Resource, instance string) bool {
	for _, l := range v.Links {
		if l.Kind == cloud.KindInstance && l.ID == instance {
			return true
		}
	}
	return false
}

// costSteps is the price per hour over a range, walked back from the
// instance's state and size now through the changes applied in the range.
func costSteps(state, size string, changes []appliedChange, from time.Time) []CostStep {
	type st struct {
		running, exists bool
		size            string
	}
	price := func(s st) *float64 {
		if !s.exists {
			return nil
		}
		if !s.running {
			v := 0.0
			return &v
		}
		if p, ok := cloud.HourlyPrice(s.size); ok {
			return &p
		}
		return nil
	}
	cur := st{running: state == "running" || state == "pending", exists: true, size: size}
	steps := []CostStep{}
	for i := len(changes) - 1; i >= 0; i-- {
		ch := changes[i]
		after := cur
		switch ch.Action {
		case cloud.ActionStop:
			cur.running = true
		case cloud.ActionStart:
			cur.running = false
		case cloud.ActionResize:
			var s cloudResizeSpec
			if json.Unmarshal(ch.Spec, &s) == nil && s.From != "" {
				cur.size = s.From
			}
		case cloud.ActionLaunch:
			cur.exists = false
		default:
			continue
		}
		steps = append(steps, CostStep{From: ch.At, PerHour: price(after)})
	}
	steps = append(steps, CostStep{From: from, PerHour: price(cur)})
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	// Drop a step that does not change the price.
	out := steps[:1]
	for _, s := range steps[1:] {
		if !samePrice(out[len(out)-1].PerHour, s.PerHour) {
			out = append(out, s)
		}
	}
	return out
}

func samePrice(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

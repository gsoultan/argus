package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// The cost walks back from the instance as it is now through what Argus
// changed: before a stop it was running, before a resize it was the old
// size, and before its launch it cost nothing because it was not there.
func TestCostStepsWalkBackThroughChanges(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return from.Add(time.Duration(h) * time.Hour) }
	changes := []appliedChange{
		{At: at(1), Action: cloud.ActionLaunch},
		{At: at(2), Action: cloud.ActionResize, Spec: json.RawMessage(`{"size":"m6i.large","from":"t3.large"}`)},
		{At: at(3), Action: cloud.ActionTags},
		{At: at(4), Action: cloud.ActionStop},
	}
	got := costSteps("stopped", "m6i.large", changes, from)
	want := []struct {
		at    time.Time
		price float64
		none  bool
	}{{from, 0, true}, {at(1), 0.0832, false}, {at(2), 0.096, false}, {at(4), 0, false}}
	if len(got) != len(want) {
		t.Fatalf("steps = %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if !g.From.Equal(w.at) || (g.PerHour == nil) != w.none || (g.PerHour != nil && *g.PerHour != w.price) {
			t.Errorf("step %d = %v %v, want %v %v (none=%v)", i, g.From, g.PerHour, w.at, w.price, w.none)
		}
	}
	// No changes: one step at today's price; an unknown size has none.
	if s := costSteps("running", "x9.huge", nil, from); len(s) != 1 || s[0].PerHour != nil {
		t.Errorf("unknown size = %+v", s)
	}
}

// A limit is drawn only when every attached volume states it.
func TestVolumeLimitsNeedEveryVolume(t *testing.T) {
	inst := &CloudItem{AccountID: "a", Resource: cloud.Resource{ID: "i-1"}}
	vol := func(account, instance, iops, mib string) CloudItem {
		d := map[string]string{}
		if iops != "" {
			d["iops"] = iops
		}
		if mib != "" {
			d["throughput"] = mib
		}
		return CloudItem{AccountID: account, Resource: cloud.Resource{Kind: cloud.KindVolume, Details: d,
			Links: []cloud.Ref{{Kind: cloud.KindInstance, ID: instance}}}}
	}
	iops, bps := volumeLimits(inst, []CloudItem{vol("a", "i-1", "3000", "125"), vol("a", "i-1", "100", ""),
		vol("b", "i-1", "9999", "999"), vol("a", "i-2", "9999", "999")})
	if iops == nil || *iops != 3100 {
		t.Errorf("iops = %v, want 3100 from this instance's two volumes", iops)
	}
	if bps != nil {
		t.Errorf("bps = %v, want none when a volume states no throughput", *bps)
	}
	if iops, bps := volumeLimits(inst, nil); iops != nil || bps != nil {
		t.Error("no volumes, no limits")
	}
}

// A bad process row is dropped, a name is cleaned, and no more than ten
// are kept.
func TestBoundedTop(t *testing.T) {
	cpu, bad := 12.5, 140.0
	in := []HostProcess{
		{Name: "post\x1bgres\n", PID: 10, CPU: &cpu, Memory: 1 << 30},
		{Name: "", PID: 11},
		{Name: "neg", PID: -1},
		{Name: "huge", PID: 12, Memory: 1 << 50},
		{Name: "busy", PID: 13, CPU: &bad, Memory: 5},
	}
	for range 20 {
		in = append(in, HostProcess{Name: "x", PID: 99})
	}
	out := boundedTop(in)
	if len(out) != maxTopProcs {
		t.Fatalf("kept %d, want %d", len(out), maxTopProcs)
	}
	if out[0].Name != "postgres" || out[0].CPU == nil || *out[0].CPU != 12.5 {
		t.Errorf("first = %+v", out[0])
	}
	if out[1].Name != "busy" || out[1].CPU != nil {
		t.Errorf("an impossible share must be dropped, not the process: %+v", out[1])
	}
}

// The Metrics tab gets the instance's limits and cost, and markers for the
// changes Argus made to it and the updates it ran.
func TestMetricsCarryCloudContextAndEvents(t *testing.T) {
	e := cloudAPI(t)
	e.fake.Roles[e.roleARN()] = e.prepare(t).ExternalID
	id := "i-0" + e.account
	e.fake.Resources["ap-southeast-3"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: id, Name: "web-01", Region: "ap-southeast-3", State: "running", Size: "m6i.large"},
		{Kind: cloud.KindVolume, ID: "vol-1" + e.account, Region: "ap-southeast-3", Size: "40 GiB gp3",
			Details: map[string]string{"iops": "3000", "throughput": "125"}, Links: []cloud.Ref{{Kind: cloud.KindInstance, ID: id}}},
	}
	host := unique("web") + ".example"
	ctx := context.Background()
	asset, err := e.s.CreateAsset(ctx, AssetInput{Hostname: host, Address: "203.0.113.78",
		Principals: []string{"ops"}, CredentialRef: "injected"})
	if err != nil {
		t.Fatal(err)
	}
	boot := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	sys := `{"booted_at":"` + boot.Format(time.RFC3339) + `","cloud":{"provider":"aws","account":"` + e.account + `","region":"ap-southeast-3","id":"` + id + `","type":"m6i.large"}}`
	if _, err := e.s.pool.Exec(ctx, `INSERT INTO agents (hostname, version, last_seen_at, updated_at, system)
		VALUES ($1, 'test', now(), now(), $2::jsonb)`, host, sys); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.s.pool.Exec(context.Background(), `DELETE FROM agents WHERE hostname = $1`, host) })
	c := e.connect(t, http.StatusCreated)
	waitCloudSync(t, e, c)

	resized := time.Now().UTC().Add(-3 * time.Hour)
	if _, err := e.s.pool.Exec(ctx, `INSERT INTO cloud_changes (account_id, region, action, level, environment, resource_id,
		spec, summary, state, requested_by, applied_at)
		VALUES ($1, 'ap-southeast-3', 'resize', 'change', 'production', $2, '{"size":"m6i.large","from":"t3.large"}',
		        'Resize web-01 to m6i.large', 'applied', 'dana@example.com', $3)`, c.ID, id, resized); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.pool.Exec(ctx, `INSERT INTO package_runs (hostname, asset_hostname, mode, trigger, state, requested_by,
		finished_at, upgraded) VALUES ($1, $1, 'all', 'manual', 'done', 'lee@example.com', $2, '[{},{},{}]')`,
		host, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/metrics?range=24h", "auditor-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("metrics: %d %s", w.Code, w.Body)
	}
	var m AssetMetrics
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Cloud == nil || m.Cloud.InstanceID != id || m.Cloud.InstanceType != "m6i.large" {
		t.Fatalf("cloud = %+v", m.Cloud)
	}
	if m.Cloud.NetworkBps == nil || *m.Cloud.NetworkBps != 97625000 ||
		m.Cloud.DiskIOPS == nil || *m.Cloud.DiskIOPS != 3000 || m.Cloud.DiskBps == nil || *m.Cloud.DiskBps != 125<<20 {
		t.Errorf("limits = %v %v %v", m.Cloud.NetworkBps, m.Cloud.DiskIOPS, m.Cloud.DiskBps)
	}
	if len(m.Cloud.Cost) != 2 || *m.Cloud.Cost[0].PerHour != 0.0832 || *m.Cloud.Cost[1].PerHour != 0.096 {
		t.Errorf("cost = %+v, want the old size's price, then the new one's", m.Cloud.Cost)
	}
	if len(m.Events) != 3 || m.Events[0].Kind != "cloud" || m.Events[0].By != "dana@example.com" ||
		m.Events[1].Kind != "update" || m.Events[1].Summary != "Updated 3 packages" || m.Events[2].Kind != "restart" {
		t.Errorf("events = %+v", m.Events)
	}
}

// What is using a server now rides the heartbeat to the System card.
func TestSystemShowsTopProcesses(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	beat := `{"hostname":"` + asset.Hostname + `","version":"0.9.9","usage":{"cpu":10,"top":[` +
		`{"name":"postgres","pid":812,"cpu":41.5,"memory":2147483648},{"name":"bad","pid":0,"memory":1}]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, beat); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	top := getSystem(t, h, "/api/v1/assets/"+asset.ID+"/system").Usage.Top
	if len(top) != 1 || top[0].Name != "postgres" || *top[0].CPU != 41.5 {
		t.Fatalf("top = %+v", top)
	}
}

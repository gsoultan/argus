package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func hoursOf(now time.Time, n int, cpu, mem float64) []usageHour {
	out := make([]usageHour, n)
	for i := range out {
		c, m := cpu, mem
		out[i] = usageHour{At: now.Add(-time.Duration(n-i) * time.Hour), CPU: &c, Mem: &m}
	}
	return out
}

func TestSuggestionRules(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	inst := CloudItem{AccountID: "a1", Resource: cloud.Resource{ID: "i-1", Region: "r", State: "running", Size: "m6i.large"},
		Server: &CloudServerMatch{ID: "asset-1", Hostname: "web-01"}}

	// Quiet, then 8 busy hours: busy, and behind a load balancer it adds a server.
	hours := append(hoursOf(now.Add(-8*time.Hour), 16, 30, 40), hoursOf(now, 8, 92, 50)...)
	l := loadOf(hours, now)
	if !l.busy() || l.BusyHours != 8 || l.Hours24 != 24 || l.PeakCPU != 92 {
		t.Fatalf("load: %+v", l)
	}
	s := suggest("web-01", l, inst, &behind{balancer: "web-lb", targetGroup: "tg-1"})
	if s == nil || s.Kind != SuggestAddServer || s.Balancer != "web-lb" || s.Monthly == nil || *s.Monthly != 70.08 ||
		s.Title != "web-01 has been over 80% processor for 8 of the last 24 hours" ||
		s.Detail != "It is behind web-lb. Another m6i.large behind it would share the load, for about $70.08 a month." {
		t.Fatalf("add: %+v", s)
	}
	// Alone, it resizes up one step.
	s = suggest("web-01", l, inst, nil)
	if s == nil || s.Kind != SuggestResizeUp || s.Size != "m6i.xlarge" || s.Monthly == nil || *s.Monthly != 70.08 ||
		!strings.Contains(s.Detail, "one bigger server is simpler than two") {
		t.Fatalf("resize up: %+v", s)
	}
	// Five busy hours, or too little history, is not busy.
	if loadOf(append(hoursOf(now.Add(-5*time.Hour), 19, 30, 40), hoursOf(now, 5, 95, 40)...), now).busy() ||
		loadOf(hoursOf(now, 8, 95, 40), now).busy() {
		t.Fatal("not busy enough")
	}
	// Memory counts too, and says so.
	s = suggest("db-01", loadOf(hoursOf(now, 24, 20, 95), now), inst, nil)
	if s == nil || s.Title != "db-01 has been over 90% memory for 24 of the last 24 hours" {
		t.Fatalf("memory: %+v", s)
	}
	// Fourteen quiet days resize down, and say what that saves.
	s = suggest("etl-01", loadOf(hoursOf(now, 14*24, 4, 30), now), CloudItem{Resource: cloud.Resource{State: "running", Size: "m6i.xlarge"},
		Server: &CloudServerMatch{ID: "asset-2"}}, nil)
	if s == nil || s.Kind != SuggestResizeDown || s.Size != "m6i.large" || *s.Monthly != -70.08 ||
		!strings.Contains(s.Detail, "saves about $70.08 a month") {
		t.Fatalf("idle: %+v", s)
	}
	// One busy hour in the fortnight keeps it; the smallest known size has nowhere to go; a stopped one is left.
	quiet := hoursOf(now, 14*24, 4, 30)
	busy := 50.0
	quiet[100].CPU = &busy
	if loadOf(quiet, now).idle() {
		t.Fatal("a busy hour is not idle")
	}
	if suggest("x", loadOf(hoursOf(now, 14*24, 4, 30), now), CloudItem{Resource: cloud.Resource{State: "running", Size: "m6i.large"},
		Server: &CloudServerMatch{ID: "a"}}, nil) != nil {
		t.Fatal("m6i.large has no smaller size the estimate knows")
	}
	stopped := inst
	stopped.Resource.State = "stopped"
	if suggest("web-01", l, stopped, nil) != nil {
		t.Fatal("stopped")
	}
	for in, want := range map[string]string{"t3.micro": "t3.small", "c7i.4xlarge": "", "x.large": "", "m6i": ""} {
		if got := nextSize(in, true); got != want {
			t.Errorf("next %s = %q", in, got)
		}
	}
}

// From the usage history an agent reported, through the matched instance and
// its target group, to the asset's suggestion, the list and the bell.
func TestSuggestionsFromHistory(t *testing.T) {
	e := cloudAPI(t)
	e.fake.Roles[e.roleARN()] = e.prepare(t).ExternalID
	id := "i-0" + e.account
	region := "ap-southeast-3"
	e.fake.Resources[region] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: id, Name: "web-01", Region: region, State: "running", Size: "m6i.large"},
		{Kind: cloud.KindTargetGroup, ID: "arn:tg/web" + e.account, Name: "web", Region: region,
			Links: []cloud.Ref{{Kind: cloud.KindInstance, ID: id, Name: "healthy"}, {Kind: cloud.KindLoadBalancer, ID: "arn:lb", Name: "web-lb"}}},
	}
	host := unique("web") + ".example"
	ctx := context.Background()
	asset, err := e.s.CreateAsset(ctx, AssetInput{Hostname: host, Address: "203.0.113.79", Principals: []string{"ops"}, CredentialRef: "injected"})
	if err != nil {
		t.Fatal(err)
	}
	sys := `{"cloud":{"provider":"aws","account":"` + e.account + `","region":"` + region + `","id":"` + id + `","type":"m6i.large"}}`
	if _, err := e.s.pool.Exec(ctx, `INSERT INTO agents (hostname, version, last_seen_at, updated_at, system)
		VALUES ($1, 'test', now(), now(), $2::jsonb)`, host, sys); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM agents WHERE hostname = $1`, host)
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM usage_history WHERE hostname = $1`, host)
	})
	now := time.Now().UTC()
	// 20 hours of history, the last 10 busy, two 5-minute buckets an hour.
	for i := 20; i > 0; i-- {
		cpu := 30.0
		if i <= 10 {
			cpu = 91
		}
		for _, m := range []int{5, 35} {
			at := now.Truncate(time.Hour).Add(-time.Duration(i)*time.Hour + time.Duration(m)*time.Minute)
			if _, err := e.s.pool.Exec(ctx, `INSERT INTO usage_history (hostname, bucket, samples, cpu_sum, cpu_n, mem_pct)
				VALUES ($1, $2, 5, $3, 5, 50)`, host, at, cpu*5); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := e.connect(t, http.StatusCreated)
	waitCloudSync(t, e, c)

	w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/suggestion", "auditor-token", "")
	var one struct{ Suggestion *ServerSuggestion }
	_ = json.Unmarshal(w.Body.Bytes(), &one)
	s := one.Suggestion
	if w.Code != http.StatusOK || s == nil || s.Kind != SuggestAddServer || s.Balancer != "web-lb" || s.Load.BusyHours != 10 ||
		s.Instance.Resource.ID != id || s.Hostname != host {
		t.Fatalf("asset: %d %s", w.Code, w.Body)
	}
	w = do(t, e.h, http.MethodGet, "/api/v1/cloud/suggestions", "auditor-token", "")
	var all struct{ Suggestions []ServerSuggestion }
	_ = json.Unmarshal(w.Body.Bytes(), &all)
	found := false
	for _, x := range all.Suggestions {
		found = found || x.AssetID == asset.ID
	}
	if !found {
		t.Fatalf("list: %s", w.Body)
	}

	// The bell, once a day.
	e.a.watchSuggestions(ctx, now)
	e.a.watchSuggestions(ctx, now)
	var n int
	if err := e.s.pool.QueryRow(ctx, `SELECT count(*) FROM console_alerts WHERE subject = $1`,
		suggestionSubject(*s, now.Format("2006-01-02"))).Scan(&n); err != nil || n != 1 {
		t.Fatalf("alerts: %d %v", n, err)
	}
}

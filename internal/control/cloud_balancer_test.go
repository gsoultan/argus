package control

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestHealthCheckRules(t *testing.T) {
	h := cloud.DefaultHealthCheck("HTTP")
	if err := h.Check(); err != nil || h.Words() != "HTTP / answering 200, every 30s, healthy after 5, unhealthy after 2" {
		t.Fatalf("default: %v %q", err, h.Words())
	}
	for _, bad := range []cloud.HealthCheck{
		{Protocol: "HTTP", Path: "health", IntervalSeconds: 30, Healthy: 3, Unhealthy: 3},
		{Protocol: "HTTP", Path: "/", Matcher: "ok", IntervalSeconds: 30, Healthy: 3, Unhealthy: 3},
		{Protocol: "TCP", Port: "70000", IntervalSeconds: 30, Healthy: 3, Unhealthy: 3},
		{Protocol: "TCP", IntervalSeconds: 1, Healthy: 3, Unhealthy: 3},
		{Protocol: "UDP", IntervalSeconds: 30, Healthy: 3, Unhealthy: 3},
	} {
		if err := bad.Check(); err == nil {
			t.Fatalf("%+v passed", bad)
		}
	}
	if err := (cloud.GroupSize{Min: 2, Max: 1, Desired: 1}).Check(); err == nil {
		t.Fatal("min above max")
	}
}

// A target group, a load balancer in front of it, a changed health check and
// an Auto Scaling group's size, each through the usual change; production
// waits for a second person, development runs at once.
func TestCloudBalancing(t *testing.T) {
	e := networkAPI(t)
	region := storageRegion
	res := e.fake.Resources[region]
	e.fake.Resources[region] = append(res,
		cloud.Resource{Kind: cloud.KindSubnet, ID: e.account + "-subnet-2", Name: "private-b", Region: region, Zone: region + "b",
			Network: "vpc-1", Size: "10.0.2.0/24"},
		cloud.Resource{Kind: cloud.KindScalingGroup, ID: "web-asg", Name: "web-asg", Region: region, State: "active", Size: "2",
			Tags: map[string]string{"Environment": "development"}, Details: map[string]string{"min": "1", "max": "4", "desired": "2"}})
	e.resync(t)
	body := func(action, extra string) string {
		return `{"accountId":"` + e.acct.ID + `","region":"` + region + `","action":"` + action + `"` + extra + `}`
	}
	health := `"health":{"protocol":"HTTP","path":"/health","intervalSeconds":15,"healthy":3,"unhealthy":2}`
	e.ask(t, "admin-token", body("target-group-create", `,"spec":{"network":"vpc-1","name":"-bad","protocol":"HTTP","port":8080,`+health+`}`), http.StatusBadRequest)
	e.ask(t, "admin-token", body("target-group-create", `,"spec":{"network":"vpc-9","name":"api","protocol":"HTTP","port":8080,`+health+`}`), http.StatusBadRequest)
	e.ask(t, "admin-token", body("target-group-create", `,"spec":{"network":"vpc-1","name":"web","protocol":"HTTP","port":8080,`+health+`}`), http.StatusConflict)
	tg := e.ask(t, "admin-token", body("target-group-create", `,"reason":"new api tier","spec":{"network":"vpc-1","name":"api","protocol":"HTTP","port":8080,`+health+`}`), http.StatusAccepted)
	if tg.Summary != "Make target group api for HTTP 8080, checked by HTTP /health answering 200, every 15s, healthy after 3, unhealthy after 2" {
		t.Fatalf("tg: %q", tg.Summary)
	}
	tg = e.decide(t, "approver-token", tg.ID, true)
	if tg.State != "applied" || !strings.Contains(tg.ResourceID, "targetgroup/api/") {
		t.Fatalf("tg applied: %+v", tg)
	}
	e.resync(t)

	lb := func(extra string, want int) CloudChange {
		return e.ask(t, "admin-token", body("balancer-create", `,"reason":"api front","spec":{"name":"api-front","type":"application",`+
			`"firewalls":["`+e.account+`-sg-1"],"port":80,"targetGroup":"`+tg.ResourceID+`"`+extra+`}`), want)
	}
	lb(`,"subnets":["`+e.account+`-subnet-1"]`, http.StatusBadRequest)
	lb(`,"subnets":["`+e.account+`-subnet-1","`+e.account+`-subnet-1"]`, http.StatusBadRequest)
	c := lb(`,"subnets":["`+e.account+`-subnet-1","`+e.account+`-subnet-2"]`, http.StatusAccepted)
	if !strings.Contains(c.Summary, "Make internet-facing application load balancer api-front in 2 zones, HTTP 80 forwarding to api (about $16 a month before traffic)") {
		t.Fatalf("lb: %q", c.Summary)
	}
	if d := e.decide(t, "approver-token", c.ID, true); d.State != "applied" {
		t.Fatalf("lb applied: %+v", d)
	}
	e.resync(t)
	// The target group is behind it now; a second one is refused.
	e.ask(t, "admin-token", body("balancer-create", `,"spec":{"name":"api-two","type":"network","port":80,"targetGroup":"`+tg.ResourceID+
		`","subnets":["`+e.account+`-subnet-1","`+e.account+`-subnet-2"]}`), http.StatusBadRequest)

	h := e.ask(t, "admin-token", body("health-check", `,"resourceId":"`+tg.ResourceID+`","reason":"faster","spec":{"protocol":"HTTP","path":"/ready","intervalSeconds":10,"healthy":2,"unhealthy":2,"matcher":"200-299"}`), http.StatusAccepted)
	if h.Summary != "Check api by HTTP /ready answering 200-299, every 10s, healthy after 2, unhealthy after 2" {
		t.Fatalf("health: %q", h.Summary)
	}
	e.decide(t, "approver-token", h.ID, true)

	// A development group runs at once.
	e.ask(t, "admin-token", body("group-size", `,"resourceId":"web-asg","spec":{"min":1,"max":4,"desired":6}`), http.StatusBadRequest)
	g := e.ask(t, "admin-token", body("group-size", `,"resourceId":"web-asg","spec":{"min":2,"max":6,"desired":4}`), http.StatusOK)
	if g.State != "applied" || g.Summary != "Size web-asg to 4 (from 2), between 2 and 6; AWS starts 2 more" {
		t.Fatalf("size: %+v", g)
	}
	e.resync(t)
	if r, err := e.s.CloudResourceOf(t.Context(), e.acct.ID, region, cloud.KindScalingGroup, "web-asg"); err != nil || r.Details["desired"] != "4" {
		t.Fatalf("read back: %+v %v", r, err)
	}
}

package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 4: security groups and load balancer targets, against the
fake provider. The account is Production; groups tagged Staging or
Development show which changes wait.
*/

// networkAPI is changeAPI with the instance in sg-web, a staging group
// nothing uses that the database group's rule names, a development group
// and a target group the instance is behind.
func networkAPI(t *testing.T) changeEnv {
	t.Helper()
	e := changeAPI(t)
	res := e.fake.Resources[storageRegion]
	res[0].Network = "vpc-1"
	res[0].Links = append(res[0].Links, cloud.Ref{Kind: cloud.KindFirewall, ID: e.account + "-sg-1", Name: "sg-web"})
	res[2].Rules = []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 443, ToPort: 443, Source: "0.0.0.0/0"}}
	e.fake.Resources[storageRegion] = append(res,
		cloud.Resource{Kind: cloud.KindNetwork, ID: "vpc-1", Name: "main", Region: storageRegion, Size: "10.0.0.0/16"},
		cloud.Resource{Kind: cloud.KindFirewall, ID: "sg-0000000000000aaa1", Name: "app", Region: storageRegion, Network: "vpc-1",
			Tags: map[string]string{"Environment": "staging"}},
		cloud.Resource{Kind: cloud.KindFirewall, ID: "sg-0000000000000bbb2", Name: "db", Region: storageRegion, Network: "vpc-1",
			Tags:  map[string]string{"Environment": "staging"},
			Rules: []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 5432, ToPort: 5432, Source: "sg-0000000000000aaa1"}}},
		cloud.Resource{Kind: cloud.KindFirewall, ID: "sg-0000000000000ccc3", Name: "sandbox", Region: storageRegion, Network: "vpc-1",
			Tags: map[string]string{"Environment": "development"}},
		cloud.Resource{Kind: cloud.KindTargetGroup, ID: "arn:tg/web", Name: "web", Region: storageRegion, Network: "vpc-1",
			Details: map[string]string{"targetType": "instance", "port": "8080"},
			Links: []cloud.Ref{{Kind: cloud.KindLoadBalancer, ID: "arn:lb/front", Name: "front"},
				{Kind: cloud.KindInstance, ID: e.instance(), Name: "healthy"}}},
	)
	e.resync(t)
	return e
}

func rulesSpec(add, remove string) string {
	return `,"spec":{"add":[` + add + `],"remove":[` + remove + `]}`
}

// A rule that lets the internet reach SSH is raised to remove, so it waits
// even on a staging group; a narrow one there runs at once.
func TestCloudFirewallRulesExposure(t *testing.T) {
	e := networkAPI(t)
	app := "sg-0000000000000aaa1"
	narrow := e.ask(t, "admin-token", e.storage("firewall-rules", app, rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":22,"source":"203.0.113.9"}`, "")), http.StatusOK)
	if narrow.State != "applied" || narrow.Level != cloud.LevelChange || narrow.Environment != "staging" ||
		narrow.Summary != "Change rules on app: add tcp 22 from 203.0.113.9/32" {
		t.Fatalf("narrow rule: %+v", narrow)
	}
	e.called(t, "firewall-rules "+app+" "+e.admin)
	e.resync(t)

	// Moving it to the whole internet: take the narrow one away, add the open one.
	open := e.ask(t, "admin-token", e.storage("firewall-rules", app, rulesSpec(
		`{"inbound":true,"protocol":"tcp","fromPort":22,"toPort":22,"source":"0.0.0.0/0"}`,
		`{"inbound":true,"protocol":"tcp","fromPort":22,"toPort":22,"source":"203.0.113.9/32"}`)), http.StatusAccepted)
	if open.Level != cloud.LevelRemove || !strings.Contains(open.Summary, "opens SSH (22) to the whole internet") ||
		!strings.Contains(open.Summary, "remove tcp 22 from 203.0.113.9/32") {
		t.Fatalf("open rule: %+v", open)
	}
	done := e.approveChange(t, open)
	if done.State != "applied" {
		t.Fatalf("open rule did not apply: %+v", done)
	}
	e.resync(t)
	g, _ := e.s.CloudResourceOf(t.Context(), e.acct.ID, storageRegion, cloud.KindFirewall, app)
	if len(g.Rules) != 1 || g.Rules[0].Source != "0.0.0.0/0" {
		t.Fatalf("rules after: %+v", g.Rules)
	}

	// Web ports alone are not an exposure; nor is anything on development.
	web := e.ask(t, "admin-token", e.storage("firewall-rules", app, rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":80,"source":"0.0.0.0/0"}`, "")), http.StatusOK)
	if web.Level != cloud.LevelChange {
		t.Fatalf("port 80 is not an exposure: %+v", web)
	}
	dev := e.ask(t, "admin-token", e.storage("firewall-rules", "sg-0000000000000ccc3", rulesSpec(`{"inbound":true,"protocol":"all","source":"0.0.0.0/0"}`, "")), http.StatusOK)
	if dev.Level != cloud.LevelRemove || dev.State != "applied" || !strings.Contains(dev.Summary, "opens every port") {
		t.Fatalf("development runs at once, but says what it opens: %+v", dev)
	}
}

func TestCloudFirewallRulesRefused(t *testing.T) {
	e := networkAPI(t)
	app := "sg-0000000000000aaa1"
	for _, bad := range []string{
		rulesSpec("", ""),
		rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":22,"source":"10.0.0.1/8"}`, ""),
		rulesSpec(`{"inbound":true,"protocol":"gre","source":"10.0.0.0/8"}`, ""),
		rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":90,"toPort":80,"source":"10.0.0.0/8"}`, ""),
		rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":22,"source":"sg-00000000000009999"}`, ""),
		rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":22,"source":"10.0.0.0/8"},{"inbound":true,"protocol":"tcp","fromPort":22,"source":"10.0.0.0/8"}`, ""),
	} {
		e.ask(t, "admin-token", e.storage("firewall-rules", app, bad), http.StatusBadRequest)
	}
	// Taking away a rule it does not have, or adding one it has.
	e.ask(t, "admin-token", e.storage("firewall-rules", app, rulesSpec("", `{"inbound":true,"protocol":"tcp","fromPort":22,"source":"10.0.0.0/8"}`)), http.StatusConflict)
	e.ask(t, "admin-token", e.storage("firewall-rules", e.account+"-sg-1", rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":443,"source":"0.0.0.0/0"}`, "")), http.StatusConflict)
}

// A group a production instance uses is a production change, whatever its
// own tag says.
func TestCloudFirewallEnvironmentIsItsUsers(t *testing.T) {
	e := networkAPI(t)
	e.fake.Resources[storageRegion][2].Tags = map[string]string{"Environment": "development"}
	e.resync(t)
	c := e.ask(t, "admin-token", e.storage("firewall-rules", e.account+"-sg-1", rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":8443,"source":"10.0.0.0/8"}`, "")), http.StatusAccepted)
	if c.Environment != "production" || !strings.Contains(c.Summary, "used by 1 instance") {
		t.Fatalf("shared group: %+v", c)
	}
}

func TestCloudFirewallCreateAndDelete(t *testing.T) {
	e := networkAPI(t)
	spec := `,"spec":{"network":"vpc-1","name":"workers","description":"queue workers","tags":{"Environment":"development"},` +
		`"rules":[{"inbound":true,"protocol":"tcp","fromPort":9000,"toPort":9100,"source":"sg-0000000000000aaa1"}]}`
	c := e.ask(t, "admin-token", e.storage("firewall-create", "", spec), http.StatusOK)
	if c.State != "applied" || !strings.HasPrefix(c.ResourceID, "sg-") || c.Summary != "Make security group workers in main with 1 inbound rule" {
		t.Fatalf("create: %+v", c)
	}
	e.called(t, "firewall-create workers "+e.admin)
	// The same name in the same network, a name AWS keeps, and a network not here.
	e.resync(t)
	e.ask(t, "admin-token", e.storage("firewall-create", "", `,"spec":{"network":"vpc-1","name":"Workers"}`), http.StatusConflict)
	e.ask(t, "admin-token", e.storage("firewall-create", "", `,"spec":{"network":"vpc-1","name":"sg-mine"}`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("firewall-create", "", `,"spec":{"network":"vpc-9","name":"other"}`), http.StatusBadRequest)

	// Delete: in use, named by another group's rule, the default, or not typed.
	e.ask(t, "admin-token", e.storage("firewall-delete", e.account+"-sg-1", `,"confirm":"sg-web"`), http.StatusConflict)
	e.ask(t, "admin-token", e.storage("firewall-delete", "sg-0000000000000aaa1", `,"confirm":"app"`), http.StatusConflict)
	e.ask(t, "admin-token", e.storage("firewall-delete", "sg-0000000000000bbb2", `,"confirm":"nope"`), http.StatusBadRequest)
	e.madeByArgus(t, "firewall-create", "sg-0000000000000bbb2")
	d := e.ask(t, "admin-token", e.storage("firewall-delete", "sg-0000000000000bbb2", `,"confirm":"db"`), http.StatusAccepted)
	if d.Level != cloud.LevelRemove {
		t.Fatalf("delete level: %+v", d)
	}
	if done := e.approveChange(t, d); done.State != "applied" {
		t.Fatalf("delete: %+v", done)
	}
	e.called(t, "firewall-delete sg-0000000000000bbb2 "+e.admin)
}

func TestCloudInstanceFirewalls(t *testing.T) {
	e := networkAPI(t)
	for i, r := range e.fake.Resources[storageRegion] {
		if r.ID == "sg-0000000000000ccc3" {
			e.fake.Resources[storageRegion][i].Rules = []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 3389, ToPort: 3389, Source: "::/0"}}
		}
	}
	e.resync(t)
	body := func(ids ...string) string {
		b, _ := json.Marshal(map[string]any{"firewalls": ids})
		return `,"spec":` + string(b)
	}
	e.ask(t, "admin-token", e.storage("instance-firewalls", e.instance(), body(e.account+"-sg-1")), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("instance-firewalls", e.instance(), body()), http.StatusBadRequest)
	c := e.ask(t, "admin-token", e.storage("instance-firewalls", e.instance(), body(e.account+"-sg-1", "sg-0000000000000ccc3")), http.StatusAccepted)
	if c.Level != cloud.LevelRemove || !strings.Contains(c.Summary, "to sg-web, sandbox; opens Remote Desktop (3389)") {
		t.Fatalf("instance groups: %+v", c)
	}
	if done := e.approveChange(t, c); done.State != "applied" {
		t.Fatalf("instance groups: %+v", done)
	}
	e.called(t, "instance-firewalls "+e.instance()+" "+e.admin)
}

func TestCloudTargets(t *testing.T) {
	e := networkAPI(t)
	tg := "arn:tg/web"
	in := `,"spec":{"instance":"` + e.instance() + `"}`
	e.ask(t, "admin-token", e.storage("target-register", tg, in), http.StatusConflict)
	c := e.ask(t, "admin-token", e.storage("target-deregister", tg, in), http.StatusAccepted)
	if c.Summary != "Take pay-api-01 out of web on port 8080; it leaves front with no healthy target, so it stops answering" {
		t.Fatalf("deregister: %q", c.Summary)
	}
	if done := e.approveChange(t, c); done.State != "applied" {
		t.Fatalf("deregister: %+v", done)
	}
	e.called(t, "target-deregister "+tg+" "+e.instance()+" "+e.admin)
	e.resync(t)
	r := e.ask(t, "admin-token", e.storage("target-register", tg, in), http.StatusAccepted)
	if r.Summary != "Put pay-api-01 behind web on port 8080" {
		t.Fatalf("register: %q", r.Summary)
	}
}

// An account connected before this phase is told to update its role, not
// sent to AWS to fail.
func TestCloudNetworkNeedsTheNewRole(t *testing.T) {
	e := networkAPI(t)
	e.fake.Missing = []string{"ec2:firewall-rules"}
	e.resync(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		e.storage("firewall-rules", "sg-0000000000000aaa1", rulesSpec(`{"inbound":true,"protocol":"tcp","fromPort":22,"source":"10.0.0.0/8"}`, "")))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "argus-access stack") {
		t.Fatalf("old role: %d %s", w.Code, w.Body)
	}
}

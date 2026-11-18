package gcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestRulesMustBeEveryPortFromEverySource(t *testing.T) {
	r := func(proto string, from, to int32, src string) cloud.Rule {
		return cloud.Rule{Inbound: true, Protocol: proto, FromPort: from, ToPort: to, Source: src}
	}
	allowed, ranges, tags, err := shape([]cloud.Rule{
		r("tcp", 443, 443, "10.0.0.0/8"), r("tcp", 8000, 8100, "10.0.0.0/8"), r("udp", 0, 65535, "10.0.0.0/8"),
		r("tcp", 443, 443, "tag:lb"), r("tcp", 8000, 8100, "tag:lb"), r("udp", 0, 65535, "tag:lb"),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(allowed)
	if string(b) != `[{"IPProtocol":"tcp","ports":["443","8000-8100"]},{"IPProtocol":"udp"}]` ||
		strings.Join(ranges, ",") != "10.0.0.0/8" || strings.Join(tags, ",") != "lb" {
		t.Fatalf("allowed %s ranges %v tags %v", b, ranges, tags)
	}
	if p := RulesProblem([]cloud.Rule{r("tcp", 443, 443, "0.0.0.0/0"), r("tcp", 22, 22, "10.0.0.0/8")}); !strings.Contains(p, "Make another firewall rule") {
		t.Fatalf("a set that is not every port from every source: %q", p)
	}
	if RulesProblem(nil) == "" {
		t.Fatal("no rules at all was taken")
	}
	bad := []cloud.Rule{
		{Inbound: false, Protocol: "tcp", FromPort: 443, Source: "10.0.0.0/8"},
		{Inbound: true, Protocol: "tcp", FromPort: 443, Source: "sg-0123456789abcdef0"},
		{Inbound: true, Protocol: "tcp", FromPort: 443, Source: "tag:Web"},
		{Inbound: true, Protocol: "gre", Source: "10.0.0.0/8"},
	}
	for _, x := range bad {
		if err := CheckRule(&x); err == nil {
			t.Errorf("%+v was taken", x)
		}
	}
	ok := cloud.Rule{Inbound: true, Protocol: "TCP", FromPort: 443, Source: "tag:web"}
	if err := CheckRule(&ok); err != nil || ok.Source != "tag:web" || ok.Protocol != "tcp" || ok.ToPort != 443 {
		t.Fatalf("%+v %v", ok, err)
	}
}

func TestAFirewallRuleIsMadeChangedAndDeleted(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	ctx := context.Background()
	res, err := p.CreateFirewall(ctx, acct, "alice@example.com", "global", cloud.FirewallSpec{Network: "700", Name: "api-in", Description: "API",
		Rules: []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 8443, Source: "10.1.0.0/16"}}})
	if err != nil || res.ResourceID != "908" {
		t.Fatalf("create: %+v %v", res, err)
	}
	var made map[string]any
	_ = json.Unmarshal([]byte(g.bodies[0]), &made)
	if made["direction"] != "INGRESS" || !strings.HasSuffix(made["network"].(string), "/global/networks/default") ||
		g.bodies[0] == "" || !strings.Contains(g.bodies[0], `"targetTags":["api-in"]`) || !strings.Contains(g.bodies[0], `"sourceRanges":["10.1.0.0/16"]`) {
		t.Fatalf("create body = %s", g.bodies[0])
	}

	// web-in allows tcp 443 and 8080 from 10.0.0.0/8; 8080 goes, 9090 comes.
	g.posts, g.bodies = nil, nil
	add := []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 9090, ToPort: 9090, Source: "10.0.0.0/8"}}
	remove := []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 8080, ToPort: 8080, Source: "10.0.0.0/8"}}
	if _, err := p.ChangeRules(ctx, acct, "alice@example.com", "global", "903", add, remove); err != nil {
		t.Fatal(err)
	}
	if len(g.posts) != 1 || g.posts[0] != "PUT /compute/v1/projects/payments-prod-4182/global/firewalls/web-in" ||
		!strings.Contains(g.bodies[0], `"allowed":[{"IPProtocol":"tcp","ports":["443","9090"]}]`) || !strings.Contains(g.bodies[0], `"fingerprint":"fw-1"`) {
		t.Fatalf("change: %v %s", g.posts, g.bodies)
	}
	// A source for one port only cannot be one Google rule.
	open := []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "0.0.0.0/0"}}
	if _, err := p.ChangeRules(ctx, acct, "alice@example.com", "global", "903", open, nil); err == nil {
		t.Fatal("a rule from another source was added")
	}
	if _, err := p.ChangeRules(ctx, acct, "alice@example.com", "global", "904", add, nil); err == nil || !strings.Contains(err.Error(), "denies") {
		t.Fatalf("a deny rule was changed: %v", err)
	}

	g.posts = nil
	if _, err := p.DeleteFirewall(ctx, acct, "alice@example.com", "global", "903"); err != nil {
		t.Fatal(err)
	}
	if len(g.posts) != 1 || g.posts[0] != "DELETE /compute/v1/projects/payments-prod-4182/global/firewalls/903" {
		t.Fatalf("delete: %v", g.posts)
	}
}

func TestAVMIsGivenFirewallRulesByItsNetworkTags(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	ctx := context.Background()
	// web-1 carries http-server (allow-http) and team-x (no rule); it gets web-in instead of allow-http.
	if _, err := p.SetInstanceFirewalls(ctx, acct, "alice@example.com", "us-central1", "101", []string{"903"}); err != nil {
		t.Fatal(err)
	}
	if len(g.posts) != 1 || !strings.HasSuffix(g.posts[0], "/zones/us-central1-a/instances/101/setTags") ||
		g.bodies[0] != `{"fingerprint":"tg-1","items":["team-x","web-in"]}` {
		t.Fatalf("setTags: %v %v", g.posts, g.bodies)
	}
	for _, picked := range [][]string{{"905"}, {"906"}, {"907"}} {
		if _, err := p.SetInstanceFirewalls(ctx, acct, "alice@example.com", "us-central1", "101", picked); err == nil {
			t.Errorf("%v was taken", picked)
		}
	}
}

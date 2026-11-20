package azure

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

const (
	subnetNSG = "ffffffff-0000-4000-8000-000000000002"
	farNSG    = "ffffffff-0000-4000-8000-000000000003"
	appNSG    = "ffffffff-0000-4000-8000-000000000004"
)

// groupWritten is the group as the last change wrote it: its rules by
// name, and the whole body.
func groupWritten(t *testing.T, m *fakeMicrosoft, name string) (map[string]rawRule, map[string]any) {
	t.Helper()
	got := m.changed()
	body, ok := strings.CutPrefix(got[len(got)-1], "PUT nsg "+name+" ")
	if !ok {
		t.Fatalf("the last change was not %s: %q", name, got[len(got)-1])
	}
	var whole map[string]any
	var g struct {
		Properties struct {
			SecurityRules []rawRule `json:"securityRules"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &whole); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		t.Fatal(err)
	}
	rules := map[string]rawRule{}
	for _, r := range g.Properties.SecurityRules {
		rules[r.Name] = r
	}
	return rules, whole
}

func tcp(port int32, from string) cloud.Rule {
	return cloud.Rule{Inbound: true, Protocol: "tcp", FromPort: port, ToPort: port, Source: from}
}

func TestRulesAreAddedAfterTheOthersAndAheadOfTheDenyThatClosesTheList(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	add := []cloud.Rule{
		{Inbound: true, Protocol: "tcp", FromPort: 8000, ToPort: 8080, Source: "10.3.0.0/16", Description: "the workers"},
		{Inbound: true, Protocol: "icmp", FromPort: -1, ToPort: -1, Source: "tag:VirtualNetwork"},
		{Protocol: "udp", FromPort: 53, ToPort: 53, Source: "10.9.0.53/32"},
	}
	res, err := p.ChangeRules(ctx, acct, "dana@northwind.id", "eastus", appNSG, add, nil)
	if err != nil || res.RequestID != "corr-1234" || res.ResourceID != appNSG {
		t.Fatalf("add: %+v %v", res, err)
	}
	rules, whole := groupWritten(t, m, "app-nsg")
	if len(rules) != 7 {
		t.Fatalf("four rules were there and three added: %v", rules)
	}
	var workers, ping, dns rawRule
	for n, r := range rules {
		switch {
		case strings.HasPrefix(n, "argus-tcp-8000-8080-"):
			workers = r
		case strings.HasPrefix(n, "argus-icmp-"):
			ping = r
		case strings.HasPrefix(n, "argus-udp-53-out-"):
			dns = r
		}
	}
	w := workers.Properties
	if w.Priority != 220 || w.Access != "Allow" || w.Direction != "Inbound" || w.Protocol != "Tcp" || w.SourceAddressPrefix != "10.3.0.0/16" ||
		w.DestinationAddressPrefix != "*" || w.DestinationPortRange != "8000-8080" || w.Description != "the workers" {
		t.Fatalf("the workers' rule: %+v", w)
	}
	if g := ping.Properties; g.Priority != 230 || g.Protocol != "Icmp" || g.SourceAddressPrefix != "VirtualNetwork" || g.DestinationPortRange != "*" {
		t.Fatalf("ping: %+v", g)
	}
	// Nothing goes out of this group yet, so the first rule out starts
	// where Azure's portal starts.
	if g := dns.Properties; g.Priority != 1000 || g.Direction != "Outbound" || g.Protocol != "Udp" || g.SourceAddressPrefix != "*" ||
		g.DestinationAddressPrefix != "10.9.0.53/32" || g.DestinationPortRange != "53" {
		t.Fatalf("dns: %+v", g)
	}
	// The rest of the group goes back as it was read.
	if rules["web"].Properties.Priority != 200 || rules["deny-rest"].Properties.Access != "Deny" || whole["tags"].(map[string]any)["keep"] != "me" ||
		whole["location"] != "eastus" {
		t.Fatalf("the rest of the group must be untouched: %v", whole)
	}
	// Read again, the group lists what was added under the rule's name.
	var back []rawRule
	for _, r := range rules {
		back = append(back, r)
	}
	found := false
	for _, l := range listed(back) {
		for _, x := range l.rules {
			if x.Same(add[0]) && x.Description == l.name+": the workers" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the rule added must read back as the same rule")
	}
}

func TestARuleADenyWouldStillBlockIsNotAdded(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	n := len(m.changed())
	for _, x := range []cloud.Rule{tcp(23, "0.0.0.0/0"), tcp(23, "198.51.100.0/24"), {Inbound: true, Protocol: "all", FromPort: 0, ToPort: 65535, Source: "tag:VirtualNetwork"}} {
		_, err := p.ChangeRules(ctx, acct, "dana@northwind.id", "eastus", appNSG, []cloud.Rule{x}, nil)
		if !errors.Is(err, cloud.ErrUnsupported) || !strings.Contains(err.Error(), "deny rule no-telnet (priority 150) would still block") {
			t.Fatalf("%+v behind a deny: %v", x, err)
		}
	}
	// subnet-nsg denies everything first: nothing added after it would
	// get through, and there is no room ahead of it Argus would take.
	if _, err := p.ChangeRules(ctx, acct, "dana@northwind.id", "eastus", subnetNSG, []cloud.Rule{tcp(443, "10.0.0.0/8")}, nil); !errors.Is(err, cloud.ErrUnsupported) ||
		!strings.Contains(err.Error(), "deny rule shut") {
		t.Fatalf("behind a deny-all: %v", err)
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("nothing may be written: %q", got)
	}
	// Another port, or addresses the deny does not cover, is not blocked.
	if _, err := p.ChangeRules(ctx, acct, "dana@northwind.id", "eastus", appNSG, []cloud.Rule{tcp(2323, "0.0.0.0/0")}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTakingARuleAwayDeletesOrNarrowsTheAzureRuleThatHoldsIt(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	const who = "dana@northwind.id"

	// One port for one source, of a rule with two of each, cannot be said.
	n := len(m.changed())
	_, err := p.ChangeRules(ctx, acct, who, "eastus", appNSG, nil, []cloud.Rule{tcp(80, "10.1.0.0/16")})
	if !errors.Is(err, cloud.ErrUnsupported) || !strings.Contains(err.Error(), "Azure rule web") || len(since(m, n)) != 0 {
		t.Fatalf("a removal that cannot be said: %v %q", err, since(m, n))
	}
	// A rule the group does not have is refused too.
	if _, err := p.ChangeRules(ctx, acct, who, "eastus", appNSG, nil, []cloud.Rule{tcp(8443, "10.1.0.0/16")}); err == nil || !strings.Contains(err.Error(), "no longer has") {
		t.Fatalf("a rule that is gone: %v", err)
	}

	// All of one port: the Azure rule keeps both sources and the other port.
	res, err := p.ChangeRules(ctx, acct, who, "eastus", appNSG, nil, []cloud.Rule{tcp(80, "10.1.0.0/16"), tcp(80, "10.2.0.0/16")})
	if err != nil || res.Detail != "Azure rules: narrowed 1" {
		t.Fatalf("narrow: %+v %v", res, err)
	}
	rules, _ := groupWritten(t, m, "app-nsg")
	web := rules["web"].Properties
	if web.DestinationPortRange != "443" || len(web.DestinationPortRanges) != 0 || len(web.SourceAddressPrefixes) != 2 || web.Priority != 200 {
		t.Fatalf("web narrowed to 443: %+v", web)
	}
	// Then all of one source, and the bastion's rule whole.
	res, err = p.ChangeRules(ctx, acct, who, "eastus", appNSG, nil, []cloud.Rule{tcp(443, "10.2.0.0/16"), tcp(22, "203.0.113.7")})
	if err != nil || res.Detail != "Azure rules: deleted 1, narrowed 1" {
		t.Fatalf("delete and narrow: %+v %v", res, err)
	}
	rules, _ = groupWritten(t, m, "app-nsg")
	web = rules["web"].Properties
	if _, there := rules["admin"]; there || web.SourceAddressPrefix != "10.1.0.0/16" || len(web.SourceAddressPrefixes) != 0 {
		t.Fatalf("admin deleted and web narrowed to one source: %v", rules)
	}
	// The deny rules are never touched.
	if rules["no-telnet"].Properties.Access != "Deny" || rules["deny-rest"].Properties.Priority != 4000 {
		t.Fatalf("deny rules must stay: %v", rules)
	}

	// A rule moved from one source to another is one write.
	n = len(m.changed())
	if _, err := p.ChangeRules(ctx, acct, who, "eastus", appNSG, []cloud.Rule{tcp(443, "10.5.0.0/16")}, []cloud.Rule{tcp(443, "10.1.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	rules, _ = groupWritten(t, m, "app-nsg")
	if _, there := rules["web"]; there || len(since(m, n)) != 1 || len(rules) != 3 {
		t.Fatalf("a move is one write that deletes one rule and adds one: %v", rules)
	}
}

func TestAGroupSomeoneElseChangedIsNotOverwritten(t *testing.T) {
	p, m, acct := managed(t)
	m.mu.Lock()
	m.raceGroup = true
	m.mu.Unlock()
	n := len(m.changed())
	_, err := p.ChangeRules(context.Background(), acct, "dana@northwind.id", "eastus", appNSG, []cloud.Rule{tcp(443, "10.5.0.0/16")}, nil)
	if err == nil || !strings.Contains(err.Error(), "someone else changed it") || len(since(m, n)) != 0 {
		t.Fatalf("a group changed in between: %v %q", err, since(m, n))
	}
}

func TestAGroupIsMadeAndOnlyAnUnusedOneDeleted(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	const who = "dana@northwind.id"
	spec := cloud.FirewallSpec{Group: "Web-RG", Name: "api-nsg", Tags: map[string]string{"team": "payments"},
		Rules: []cloud.Rule{tcp(443, "0.0.0.0/0"), tcp(22, "203.0.113.7")}}
	res, err := p.CreateFirewall(ctx, acct, who, "eastus", spec)
	if err != nil || res.ResourceID != "ffffffff-0000-4000-8000-0000000000ff" || res.Detail != "Made api-nsg in Web-RG" {
		t.Fatalf("make: %+v %v", res, err)
	}
	rules, whole := groupWritten(t, m, "api-nsg")
	if len(rules) != 2 || whole["location"] != "eastus" || whole["tags"].(map[string]any)["team"] != "payments" {
		t.Fatalf("the new group: %v", whole)
	}
	pr := map[int]string{}
	for _, r := range rules {
		pr[r.Properties.Priority] = r.Properties.DestinationPortRange + " " + r.Properties.SourceAddressPrefix
	}
	if pr[1000] != "443 0.0.0.0/0" || pr[1010] != "22 203.0.113.7/32" {
		t.Fatalf("priorities in the order given: %v", pr)
	}
	// A name in use is refused before any write, as is a name Azure would
	// not take and a rule Argus does not add.
	n := len(m.changed())
	for why, s := range map[string]cloud.FirewallSpec{
		"already has":    {Group: "Web-RG", Name: "web-nsg"},
		"does not name":  {Group: "Web-RG", Name: "bad/name"},
		"not a tag":      {Group: "Web-RG", Name: "x", Rules: []cloud.Rule{tcp(443, "tag:Internet")}},
		"not an address": {Group: "Web-RG", Name: "x", Rules: []cloud.Rule{tcp(443, "sg-0123456789abcdef0")}},
	} {
		if _, err := p.CreateFirewall(ctx, acct, who, "eastus", s); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v", why, err)
		}
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("nothing may be written: %q", got)
	}

	if _, err := p.DeleteFirewall(ctx, acct, who, "eastus", webNSG); err == nil || !strings.Contains(err.Error(), "web-nsg is on 1 network interface") {
		t.Fatalf("a group in use: %v", err)
	}
	if _, err := p.DeleteFirewall(ctx, acct, who, "eastus", farNSG); err == nil || !strings.Contains(err.Error(), "is in westeurope, not eastus") {
		t.Fatalf("a group elsewhere: %v", err)
	}
	if _, err := p.DeleteFirewall(ctx, acct, who, "eastus", appNSG); err != nil {
		t.Fatal(err)
	}
	if got := m.changed(); got[len(got)-1] != "DELETE nsg app-nsg" {
		t.Fatalf("delete: %q", got[len(got)-1])
	}
}

func TestAVMsGroupIsSetOnItsInterfaceAndItsSubnetsLeftAlone(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	const who = "dana@northwind.id"
	n := len(m.changed())
	for why, list := range map[string][]string{
		"already has":                {subnetNSG, webNSG},
		"is on web-1's subnet":       {appNSG},
		"takes one network security": {subnetNSG, webNSG, appNSG},
		"has no network security":    {subnetNSG, farNSG},
	} {
		if _, err := p.SetInstanceFirewalls(ctx, acct, who, "eastus", web1, list); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v", why, err)
		}
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("nothing may be written: %q", got)
	}

	res, err := p.SetInstanceFirewalls(ctx, acct, who, "eastus", web1, []string{subnetNSG, appNSG})
	if err != nil || res.ResourceID != web1 {
		t.Fatalf("swap: %+v %v", res, err)
	}
	props := written(t, m)["properties"].(map[string]any)
	if got := props["networkSecurityGroup"].(map[string]any)["id"]; got != netPath+"networkSecurityGroups/app-nsg" {
		t.Fatalf("the interface must point at the group by a path Argus built: %v", got)
	}
	if props["enableAcceleratedNetworking"] != true || len(props["ipConfigurations"].([]any)) != 1 {
		t.Fatalf("the rest of the interface must be untouched: %v", props)
	}
	// With only the subnet's group asked for, the interface's own comes off.
	if _, err := p.SetInstanceFirewalls(ctx, acct, who, "eastus", web1, []string{subnetNSG}); err != nil {
		t.Fatal(err)
	}
	if _, still := written(t, m)["properties"].(map[string]any)["networkSecurityGroup"]; still {
		t.Fatal("the interface's group must be gone")
	}
}

func TestWhatCanBeTakenOutOfAGroupIsKnownBeforeAsking(t *testing.T) {
	g := cloud.Resource{Name: "app-nsg", Rules: []cloud.Rule{
		{Inbound: true, Protocol: "tcp", FromPort: 80, ToPort: 80, Source: "10.1.0.0/16", Description: "web"},
		{Inbound: true, Protocol: "tcp", FromPort: 443, ToPort: 443, Source: "10.1.0.0/16", Description: "web"},
		{Inbound: true, Protocol: "tcp", FromPort: 80, ToPort: 80, Source: "10.2.0.0/16", Description: "web"},
		{Inbound: true, Protocol: "tcp", FromPort: 443, ToPort: 443, Source: "10.2.0.0/16", Description: "web"},
		{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "203.0.113.7", Description: "admin: the bastion"},
	}}
	if why := RemovalProblem(g, []cloud.Rule{tcp(80, "10.1.0.0/16")}); !strings.Contains(why, "Azure rule web") {
		t.Fatalf("one of four: %q", why)
	}
	for _, ok := range [][]cloud.Rule{
		{tcp(22, "203.0.113.7")},
		{tcp(80, "10.1.0.0/16"), tcp(80, "10.2.0.0/16")},
		{tcp(80, "10.1.0.0/16"), tcp(443, "10.1.0.0/16")},
		g.Rules,
	} {
		if why := RemovalProblem(g, ok); why != "" {
			t.Fatalf("%v can be said: %q", ok, why)
		}
	}
	if RuleName(g.Rules[4]) != "admin" {
		t.Fatal("a rule's name is the start of its description")
	}
	bad := cloud.Rule{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "tag:Storage"}
	good := cloud.Rule{Inbound: true, Protocol: "TCP", FromPort: 22, Source: "tag:virtualnetwork"}
	if CheckRule(&bad) == nil || CheckRule(&good) != nil || good.Source != "tag:VirtualNetwork" || good.ToPort != 22 || good.Protocol != "tcp" {
		t.Fatalf("tags Argus adds a rule for: %+v", good)
	}
}

package cloud

import (
	"testing"
	"time"
)

func byCheck(fs []Finding) map[string][]Finding {
	m := map[string][]Finding{}
	for _, f := range fs {
		m[f.Check] = append(m[f.Check], f)
	}
	return m
}

func ptr[T any](v T) *T { return &v }

var now = time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)

func TestFindingsOpenPortIsHighAndFixable(t *testing.T) {
	rs := []Resource{
		{Kind: KindFirewall, ID: "sg-web", Name: "sg-web", Details: map[string]string{"interfaces": "1"},
			Rules: []Rule{{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "0.0.0.0/0"},
				{Inbound: true, Protocol: "tcp", FromPort: 443, ToPort: 443, Source: "0.0.0.0/0"}}},
		{Kind: KindInstance, ID: "i-1", Name: "web-01", State: "running", Links: []Ref{{Kind: KindFirewall, ID: "sg-web"}}},
	}
	got := byCheck(Findings(rs, nil, now))["rule-open"]
	if len(got) != 1 {
		t.Fatalf("want only SSH flagged (443 is a web port), got %+v", got)
	}
	f := got[0]
	if f.Severity != SeverityHigh || f.Title != "SSH open to the whole internet" || f.Fix != ActionFirewallRules || f.Rule == nil || f.Rule.FromPort != 22 {
		t.Fatalf("finding = %+v", f)
	}
	if f.Source != SourceCloud {
		t.Fatalf("an open rule is a fact AWS reported, got source %q", f.Source)
	}
}

func TestFindingsUnusedGroupNeedsItsUseRead(t *testing.T) {
	rules := []Rule{{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "0.0.0.0/0"}}
	unread := []Resource{{Kind: KindFirewall, ID: "sg-a", Name: "a", Details: map[string]string{}, Rules: rules}}
	m := byCheck(Findings(unread, nil, now))
	if len(m["group-unused"]) != 0 {
		t.Fatal("a group whose use was not read must not be called unused")
	}
	if len(m["rule-open"]) != 1 {
		t.Fatal("with its use unknown, an open rule is still a risk")
	}

	read := []Resource{{Kind: KindFirewall, ID: "sg-a", Name: "a", Details: map[string]string{"interfaces": "0"}, Rules: rules}}
	m = byCheck(Findings(read, nil, now))
	if len(m["group-unused"]) != 1 || m["group-unused"][0].Fix != ActionFirewallDelete {
		t.Fatalf("unused group: %+v", m["group-unused"])
	}
	if len(m["rule-open"]) != 0 {
		t.Fatal("an open rule on a group nothing uses is listed as the unused group, not as a risk")
	}

	// Named by another group's rule: AWS would refuse the delete.
	ref := append(read, Resource{Kind: KindFirewall, ID: "sg-b", Name: "b", Details: map[string]string{"interfaces": "2"},
		Rules: []Rule{{Inbound: true, Protocol: "tcp", FromPort: 5432, ToPort: 5432, Source: "sg-a"}}})
	if len(byCheck(Findings(ref, nil, now))["group-unused"]) != 0 {
		t.Fatal("a group another rule names is not unused")
	}

	def := []Resource{{Kind: KindFirewall, ID: "sg-d", Name: "default", Details: map[string]string{"interfaces": "0", "default": "yes"}}}
	if len(byCheck(Findings(def, nil, now))["group-unused"]) != 0 {
		t.Fatal("the default group cannot be deleted and is never unused")
	}
}

func TestFindingsCoveredRule(t *testing.T) {
	rs := []Resource{{Kind: KindFirewall, ID: "sg-a", Name: "a", Details: map[string]string{"interfaces": "1"}, Rules: []Rule{
		{Inbound: true, Protocol: "tcp", FromPort: 443, ToPort: 443, Source: "10.0.0.0/16"},
		{Inbound: true, Protocol: "tcp", FromPort: 443, ToPort: 443, Source: "10.0.1.0/24"},
		{Inbound: true, Protocol: "tcp", FromPort: 5432, ToPort: 5432, Source: "10.9.0.0/24"},
		{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "sg-x"},
	}}}
	got := byCheck(Findings(rs, nil, now))["rule-covered"]
	if len(got) != 1 || got[0].Rule.Source != "10.0.1.0/24" {
		t.Fatalf("covered = %+v", got)
	}

	dup := []Resource{{Kind: KindFirewall, ID: "sg-a", Name: "a", Details: map[string]string{"interfaces": "1"}, Rules: []Rule{
		{Inbound: true, Protocol: "tcp", FromPort: 80, ToPort: 80, Source: "10.0.0.0/16"},
		{Inbound: true, Protocol: "tcp", FromPort: 80, ToPort: 80, Source: "10.0.0.0/16"},
	}}}
	if got := byCheck(Findings(dup, nil, now))["rule-covered"]; len(got) != 1 {
		t.Fatalf("of two identical rules exactly one is covered, got %d", len(got))
	}
}

func TestFindingsNothingListensNeedsEveryAgentFresh(t *testing.T) {
	fw := Resource{Kind: KindFirewall, ID: "sg-api", Name: "sg-api", Details: map[string]string{"interfaces": "2"},
		Rules: []Rule{{Inbound: true, Protocol: "tcp", FromPort: 8080, ToPort: 8080, Source: "10.20.0.0/16"}}}
	i1 := Resource{Kind: KindInstance, ID: "i-1", Name: "api-1", State: "running", Links: []Ref{{Kind: KindFirewall, ID: "sg-api"}}}
	i2 := Resource{Kind: KindInstance, ID: "i-2", Name: "api-2", State: "running", Links: []Ref{{Kind: KindFirewall, ID: "sg-api"}}}
	fresh := ptr(now.Add(-10 * time.Minute))
	facts := map[string]ServerFacts{
		"i-1": {ServerID: "s1", SeenAt: fresh, Listening: []int{22, 443}, ListenAt: fresh},
		"i-2": {ServerID: "s2", SeenAt: fresh, Listening: []int{22, 443}, ListenAt: fresh},
	}
	rs := []Resource{fw, i1, i2}
	if got := byCheck(Findings(rs, facts, now))["rule-nothing-listens"]; len(got) != 1 || got[0].Source != SourceAgent {
		t.Fatalf("want one agent-measured finding, got %+v", got)
	}

	stale := map[string]ServerFacts{"i-1": facts["i-1"], "i-2": {ServerID: "s2", SeenAt: fresh, Listening: []int{22}, ListenAt: ptr(now.Add(-5 * time.Hour))}}
	if got := byCheck(Findings(rs, stale, now))["rule-nothing-listens"]; len(got) != 0 {
		t.Fatal("one stale report means Argus does not know")
	}

	noAgent := map[string]ServerFacts{"i-1": facts["i-1"]}
	if got := byCheck(Findings(rs, noAgent, now))["rule-nothing-listens"]; len(got) != 0 {
		t.Fatal("an instance without the agent could be listening")
	}

	other := []Resource{{Kind: KindFirewall, ID: "sg-api", Name: "sg-api", Details: map[string]string{"interfaces": "3"}, Rules: fw.Rules}, i1, i2}
	if got := byCheck(Findings(other, facts, now))["rule-nothing-listens"]; len(got) != 0 {
		t.Fatal("a group something other than these instances uses (a load balancer) is not judged by them")
	}

	listening := map[string]ServerFacts{"i-1": facts["i-1"], "i-2": {ServerID: "s2", SeenAt: fresh, Listening: []int{8080}, ListenAt: fresh}}
	if got := byCheck(Findings(rs, listening, now))["rule-nothing-listens"]; len(got) != 0 {
		t.Fatal("one instance listens, so the rule is needed")
	}
}

func TestFindingsIdleInstanceIsAJudgement(t *testing.T) {
	inst := Resource{Kind: KindInstance, ID: "i-1", Name: "worker", State: "running", Size: "t3.medium"}
	quiet := ServerFacts{ServerID: "s1", SeenAt: ptr(now), HistoryFor: 30 * 24 * time.Hour, CPUP95: ptr(1.5), NetBps: ptr(800.0)}
	got := byCheck(Findings([]Resource{inst}, map[string]ServerFacts{"i-1": quiet}, now))["instance-idle"]
	if len(got) != 1 || got[0].Source != SourceJudgement || got[0].Fix != ActionStop || got[0].Monthly <= 0 {
		t.Fatalf("idle = %+v", got)
	}

	used := quiet
	used.LastSession = ptr(now.Add(-3 * 24 * time.Hour))
	if len(byCheck(Findings([]Resource{inst}, map[string]ServerFacts{"i-1": used}, now))["instance-idle"]) != 0 {
		t.Fatal("someone used it through Argus this month")
	}
	short := quiet
	short.HistoryFor = 3 * 24 * time.Hour
	if len(byCheck(Findings([]Resource{inst}, map[string]ServerFacts{"i-1": short}, now))["instance-idle"]) != 0 {
		t.Fatal("three days of readings are too few to call it idle")
	}
	if len(byCheck(Findings([]Resource{inst}, nil, now))["instance-idle"]) != 0 {
		t.Fatal("without the agent there are no readings")
	}
}

func TestFindingsInstanceRisks(t *testing.T) {
	rs := []Resource{{Kind: KindInstance, ID: "i-1", Name: "batch", State: "running", PublicIP: "1.2.3.4", Details: map[string]string{"metadataTokens": "optional"}}}
	m := byCheck(Findings(rs, nil, now))
	if len(m["public-no-agent"]) != 1 || len(m["imds-v1"]) != 1 {
		t.Fatalf("got %+v", m)
	}
	m = byCheck(Findings(rs, map[string]ServerFacts{"i-1": {ServerID: "s", SeenAt: ptr(now)}}, now))
	if len(m["public-no-agent"]) != 0 {
		t.Fatal("it has an Argus server")
	}
}

func TestFindingsStorageAndImages(t *testing.T) {
	old := now.Add(-400 * 24 * time.Hour)
	rs := []Resource{
		{Kind: KindSnapshot, ID: "snap-1", Name: "snap-1", Size: "8 GiB", CreatedAt: &old, Details: map[string]string{"public": "yes"}},
		{Kind: KindImage, ID: "ami-1", Name: "old", Size: "8 GiB", CreatedAt: &old, Details: map[string]string{"templatesChecked": "yes"}},
		{Kind: KindImage, ID: "ami-2", Name: "used", Size: "8 GiB", CreatedAt: &old, Details: map[string]string{"templatesChecked": "yes"}},
		{Kind: KindImage, ID: "ami-3", Name: "template", Size: "8 GiB", CreatedAt: &old, Details: map[string]string{"templatesChecked": "yes", "inTemplate": "yes"}},
		{Kind: KindImage, ID: "ami-4", Name: "unknown", Size: "8 GiB", CreatedAt: &old, Details: map[string]string{}},
		{Kind: KindInstance, ID: "i-1", Name: "x", State: "running", Details: map[string]string{"image": "ami-2"}},
		{Kind: KindVolume, ID: "vol-1", Name: "data", State: "in-use", Size: "10 GiB gp3", Details: map[string]string{"encrypted": "no"}},
	}
	m := byCheck(Findings(rs, nil, now))
	if len(m["snapshot-public"]) != 1 || m["snapshot-public"][0].Severity != SeverityHigh {
		t.Fatalf("public snapshot: %+v", m["snapshot-public"])
	}
	if len(m["image-unused"]) != 1 || m["image-unused"][0].ResourceID != "ami-1" {
		t.Fatalf("unused images: %+v", m["image-unused"])
	}
	if len(m["disk-unencrypted"]) != 1 {
		t.Fatal("unencrypted disk")
	}
}

func TestFindingsAccount(t *testing.T) {
	rs := []Resource{
		{Kind: KindAccount, ID: "123", Name: "root", Details: map[string]string{"rootMfa": "no", "rootKeys": "1"}},
		{Kind: KindUser, ID: "u1", Name: "ana", Details: map[string]string{"password": "yes", "mfa": "no",
			"key1Active": "yes", "key1Rotated": now.Add(-200 * 24 * time.Hour).Format(time.RFC3339), "key1LastUsed": now.Add(-120 * 24 * time.Hour).Format(time.RFC3339),
			"key2Active": "yes", "key2Rotated": now.Add(-100 * 24 * time.Hour).Format(time.RFC3339)}},
		{Kind: KindUser, ID: "u2", Name: "ci", Details: map[string]string{"password": "no", "mfa": "no",
			"key1Active": "yes", "key1Rotated": now.Add(-10 * 24 * time.Hour).Format(time.RFC3339), "key1LastUsed": now.Format(time.RFC3339)}},
		{Kind: KindBucket, ID: "b1", Name: "logs", Details: map[string]string{"publicBlock": "none"}},
		{Kind: KindBucket, ID: "b2", Name: "safe", Details: map[string]string{"publicBlock": "all"}},
		{Kind: KindFirewall, ID: "sg-db", Name: "sg-db", Details: map[string]string{"interfaces": "1"},
			Rules: []Rule{{Inbound: true, Protocol: "tcp", FromPort: 5432, ToPort: 5432, Source: "0.0.0.0/0"}}},
		{Kind: KindDatabase, ID: "db1", Name: "reports", Details: map[string]string{"public": "yes", "port": "5432", "engine": "postgres", "encrypted": "no"},
			Links: []Ref{{Kind: KindFirewall, ID: "sg-db"}}},
	}
	m := byCheck(Findings(rs, nil, now))
	for check, n := range map[string]int{"root-no-mfa": 1, "root-keys": 1, "user-no-mfa": 1, "key-unused": 2, "bucket-public-block": 1, "database-public": 1, "database-unencrypted": 1} {
		if len(m[check]) != n {
			t.Errorf("%s: want %d, got %+v", check, n, m[check])
		}
	}
	if m["database-public"][0].Severity != SeverityHigh {
		t.Error("a public database behind an open group is high")
	}
}

func TestFindingsAgreeWithIdle(t *testing.T) {
	rs := []Resource{
		{Kind: KindVolume, ID: "vol-1", Name: "loose", State: "available", Size: "100 GiB gp3"},
		{Kind: KindAddress, ID: "eip-1", Name: "1.2.3.4", State: "unattached"},
	}
	idle := Idle(rs, now)
	var waste []Finding
	for _, f := range Findings(rs, nil, now) {
		if f.Group == GroupWaste {
			waste = append(waste, f)
		}
	}
	if len(waste) != len(idle) {
		t.Fatalf("Costs lists %d idle, Risks & waste %d", len(idle), len(waste))
	}
}

func TestFindingBasisIsStable(t *testing.T) {
	rs := []Resource{{Kind: KindFirewall, ID: "sg-a", Name: "a", Details: map[string]string{"interfaces": "1"},
		Rules: []Rule{{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "0.0.0.0/0"}}}}
	a := Findings(rs, nil, now)[0]
	b := Findings(rs, nil, now.Add(48*time.Hour))[0]
	if a.ID != b.ID || a.Basis != b.Basis {
		t.Fatal("the same rule on another day is the same finding")
	}
}

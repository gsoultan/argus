package tunnel

import (
	"slices"
	"strings"
	"testing"
)

/*
The commands Argus runs for a line on each firewall are the ones the review
screen shows, carry the line's mark, and never let a value that did not pass
Validate through.
*/

func TestFirewallLineValidate(t *testing.T) {
	good := FirewallLine{Key: "s1a2b3", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "80,443,6000-6010",
		Sources: []string{"10.0.0.5", "10.0.0.0/8", "10.0.0.5"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(good.Sources, []string{"10.0.0.0/8", "10.0.0.5"}) {
		t.Errorf("sources are made unique and ordered: %v", good.Sources)
	}
	for name, l := range map[string]FirewallLine{
		"key":        {Key: "A;rm", Direction: "in", Action: "allow", Protocol: "tcp"},
		"direction":  {Key: "k", Direction: "fwd", Action: "allow", Protocol: "tcp"},
		"port text":  {Key: "k", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22;reboot"},
		"port range": {Key: "k", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "70000"},
		"backwards":  {Key: "k", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "90-80"},
		"any port":   {Key: "k", Direction: "in", Action: "allow", Protocol: "any", Ports: "22"},
		"source":     {Key: "k", Direction: "in", Action: "allow", Protocol: "tcp", Sources: []string{"10.0.0.1 -j ACCEPT"}},
		"every addr": {Key: "k", Direction: "in", Action: "allow", Protocol: "tcp", Sources: []string{"0.0.0.0/0"}},
	} {
		if err := l.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, l)
		}
	}
}

func TestUFWCommands(t *testing.T) {
	in := FirewallLine{Key: "k1", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "6000-6010", Sources: []string{"10.0.0.0/8", "fd00::/8"}}
	got := UFWAdd(in)
	want := []string{
		"ufw allow in proto tcp from 10.0.0.0/8 to any port 6000:6010 comment 'argus fw-k1'",
		"ufw allow in proto tcp from fd00::/8 to any port 6000:6010 comment 'argus fw-k1'",
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("ufw add %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
	deny := UFWAdd(FirewallLine{Key: "k2", Direction: "in", Action: "deny", Protocol: "any", Sources: []string{"198.51.100.23"}})
	if s := deny[0].String(); s != "ufw prepend deny in from 198.51.100.23 to any comment 'argus fw-k2'" {
		t.Errorf("a block goes first: %s", s)
	}
	out := UFWAdd(FirewallLine{Key: "k3", Direction: "out", Action: "allow", Protocol: "tcp", Ports: "443"})
	if s := out[0].String(); s != "ufw allow out proto tcp to any port 443 comment 'argus fw-k3'" {
		t.Errorf("going out: %s", s)
	}
}

func TestFirewalldCommands(t *testing.T) {
	l := FirewallLine{Key: "k1", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22,443", Sources: []string{"10.8.0.0/24"}}
	add := FirewalldAdd("public", l)
	if len(add) != 4 {
		t.Fatalf("two ports, now and after a restart: %v", add)
	}
	if s := add[0].String(); s != `firewall-cmd --zone=public '--add-rich-rule=rule family="ipv4" source address="10.8.0.0/24" port port="22" protocol="tcp" accept'` {
		t.Errorf("rich rule: %s", s)
	}
	if add[1][1] != "--permanent" {
		t.Errorf("the second is permanent: %v", add[1])
	}
	rm := FirewalldRemove(add)
	if !strings.Contains(rm[0].String(), "--remove-rich-rule=") || strings.Contains(rm[0].String(), "--add-") {
		t.Errorf("remove undoes add: %s", rm[0])
	}
	out := FirewalldAdd("public", FirewallLine{Key: "k2", Direction: "out", Action: "allow", Protocol: "tcp", Ports: "443", Sources: []string{"203.0.113.9"}})
	if s := out[0].String(); s != "firewall-cmd --direct --add-rule ipv4 filter OUTPUT 1 -p tcp -d 203.0.113.9 --dport 443 -m comment --comment 'argus fw-k2' -j ACCEPT" {
		t.Errorf("going out is a direct rule: %s", s)
	}
	if cs, err := FirewalldTakeOver("zone public: service ssh"); err != nil || cs[0].String() != "firewall-cmd --zone=public --remove-service=ssh" {
		t.Errorf("take over a service: %v %v", cs, err)
	}
	if _, err := FirewalldTakeOver("zone trusted: target ACCEPT"); err == nil {
		t.Error("a zone's target is not a rule to take over")
	}
}

func TestNftTable(t *testing.T) {
	text := NftTable([]FirewallLine{
		{Key: "a", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22", Sources: []string{"10.0.0.0/8", "fd00::/8"}},
		{Key: "b", Direction: "in", Action: "deny", Protocol: "any", Sources: []string{"198.51.100.23"}},
		{Key: "c", Direction: "out", Action: "allow", Protocol: "udp", Ports: "53"},
	}, "drop", "accept")
	for _, want := range []string{
		"delete table inet argus",
		"type filter hook input priority filter - 5; policy drop;",
		`ip saddr { 10.0.0.0/8 } tcp dport { 22 } accept comment "argus fw-a"`,
		`ip6 saddr { fd00::/8 } tcp dport { 22 } accept comment "argus fw-a"`,
		`udp dport { 53 } accept comment "argus fw-c"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Index(text, "argus fw-b") > strings.Index(text, "argus fw-a") {
		t.Error("a block comes before every allow")
	}
}

func TestWinCommands(t *testing.T) {
	got := WinAdd(FirewallLine{Key: "k1", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "3389", Sources: []string{"10.0.0.0/8"}})
	if got[0] != "New-NetFirewallRule -DisplayName 'argus fw-k1' -Group 'Argus' -Direction Inbound -Action Allow -Protocol TCP -Profile Any -LocalPort 3389 -RemoteAddress 10.0.0.0/8 | Out-Null" {
		t.Errorf("windows: %s", got[0])
	}
	if s := WinTakeOver("It's mine"); s != "Disable-NetFirewallRule -DisplayName 'It''s mine'" {
		t.Errorf("a rule made by hand is switched off, its name quoted: %s", s)
	}
}

func TestArgusLinesComeBackTogether(t *testing.T) {
	fw := HostFirewall{Kind: FirewallFirewalld, Active: true, Rules: []FirewallRule{
		{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "443", Sources: []string{"10.8.0.0/24"}, Name: "argus fw-k1"},
		{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22", Sources: []string{"10.8.0.0/24"}, Name: "argus fw-k1"},
		{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "6379", Name: "argus 6379"},
		{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "80"},
	}}
	got := fw.ArgusLines()
	want := FirewallLine{Key: "k1", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22,443", Sources: []string{"10.8.0.0/24"}}
	if len(got) != 1 || !SameLine(got["k1"], want) {
		t.Errorf("lines: %+v", got)
	}
	h := fw.Hash()
	fw.Rules[3].Ports = "8080"
	if fw.Hash() == h {
		t.Error("a rule changed by hand changes the hash")
	}
}

func TestFirewallPreview(t *testing.T) {
	have := map[string]FirewallLine{
		"old": {Key: "old", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "8080"},
		"ssh": {Key: "ssh", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22", Sources: []string{"10.8.0.0/24"}},
	}
	spec := FirewallChangeSpec{ID: "11111111-2222", Lines: []FirewallLine{
		have["ssh"],
		{Key: "web", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "80,443"},
	}, TakeOver: []string{"80/tcp ALLOW IN Anywhere"}}
	got := FirewallPreview(FirewallUFW, "", have, spec)
	want := []string{
		"ufw --force delete <each rule marked argus fw-old>",
		"ufw --force delete <the rule 80/tcp ALLOW IN Anywhere>",
		"ufw allow in proto tcp from any to any port 80,443 comment 'argus fw-web'",
	}
	if !slices.Equal(got, want) {
		t.Errorf("preview:\n got %q\nwant %q", got, want)
	}
}

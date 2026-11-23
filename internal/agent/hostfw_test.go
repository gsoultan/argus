package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

const ufwVerboseOut = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    10.8.0.0/24
80,443/tcp                 ALLOW IN    Anywhere
5432/tcp                   ALLOW IN    10.20.0.21                 # argus 5432
5432/tcp                   DENY IN     Anywhere                   # argus 5432
OpenSSH                    ALLOW IN    Anywhere
6000:6010/tcp              ALLOW IN    10.0.0.0/8
203.0.113.9                DENY OUT    Anywhere
22/tcp (v6)                ALLOW IN    fd00::/64
`

func TestParseUFWVerbose(t *testing.T) {
	fw := parseUFWVerbose(ufwVerboseOut)
	if !fw.Active || fw.DefaultIn != "deny" || fw.DefaultOut != "allow" {
		t.Fatalf("state: %+v", fw)
	}
	if len(fw.Rules) != 8 {
		t.Fatalf("want 8 rules, got %d: %+v", len(fw.Rules), fw.Rules)
	}
	ssh := fw.Rules[0]
	if ssh.Ports != "22" || ssh.Protocol != "tcp" || ssh.Direction != "in" || ssh.Action != "allow" || ssh.Sources[0] != "10.8.0.0/24" {
		t.Errorf("ssh: %+v", ssh)
	}
	if web := fw.Rules[1]; web.Ports != "80,443" || len(web.Sources) != 0 {
		t.Errorf("web: %+v", web)
	}
	if pg := fw.Rules[2]; !pg.Argus || pg.ArgusPort != 5432 || pg.Sources[0] != "10.20.0.21" {
		t.Errorf("argus rule: %+v", pg)
	}
	if app := fw.Rules[4]; app.Service != "OpenSSH" || app.Ports != "22" {
		t.Errorf("app profile: %+v", app)
	}
	if rng := fw.Rules[5]; rng.Ports != "6000-6010" {
		t.Errorf("range: %+v", rng)
	}
	if out := fw.Rules[6]; out.Direction != "out" || out.Action != "deny" || out.Sources[0] != "203.0.113.9" || out.Ports != "" {
		t.Errorf("outgoing: %+v", out)
	}
	if v6 := fw.Rules[7]; !v6.IPv6 || v6.Sources[0] != "fd00::/64" {
		t.Errorf("v6: %+v", v6)
	}
	if !parseUFWVerbose("Status: inactive\n").Active == false {
		t.Error("inactive read as active")
	}
}

func TestReachOfUFW(t *testing.T) {
	fw := parseUFWVerbose(ufwVerboseOut)
	if r := fw.ReachOf(443); !r.Everyone || !r.Public {
		t.Errorf("443 should be open to everyone: %+v", r)
	}
	// The Argus deny follows the Argus allow: only 10.20.0.21.
	if r := fw.ReachOf(5432); r.Everyone || len(r.Sources) != 1 || r.Sources[0] != "10.20.0.21" || r.Public {
		t.Errorf("5432: %+v", r)
	}
	// ufw's OpenSSH profile opens 22 to everyone, past the VPN-only rule.
	if r := fw.ReachOf(22); !r.Everyone {
		t.Errorf("22: %+v", r)
	}
	if r := fw.ReachOf(3000); r.Everyone || len(r.Sources) != 0 {
		t.Errorf("3000 should be closed: %+v", r)
	}
	off := tunnel.HostFirewall{Kind: "ufw", Installed: []string{"ufw"}}
	if r := off.ReachOf(3000); !r.Everyone {
		t.Errorf("a firewall that is off lets everyone in: %+v", r)
	}
	broken := tunnel.HostFirewall{Error: "no"}
	if r := broken.ReachOf(22); !r.Unknown {
		t.Errorf("an unread firewall must not claim anything: %+v", r)
	}
}

const zonePublic = `public (active)
  target: default
  icmp-block-inversion: no
  interfaces: eth0
  sources:
  services: cockpit ssh
  ports: 9100/tcp 3306/tcp
  protocols:
  forward: yes
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
	rule family="ipv4" source address="10.20.0.21" port port="5432" protocol="tcp" accept
	rule family="ipv4" source address="198.51.100.7" drop
`

const zoneTrusted = `trusted (active)
  target: ACCEPT
  interfaces:
  sources: 10.99.0.0/16
  services:
  ports:
  rich rules:
`

func fakeRun(answers map[string]string) runCmd {
	return func(_ context.Context, name string, args ...string) (string, error) {
		k := name + " " + strings.Join(args, " ")
		if v, ok := answers[k]; ok {
			return v, nil
		}
		return "", errors.New("not found: " + k)
	}
}

func TestReadFirewalld(t *testing.T) {
	run := fakeRun(map[string]string{
		"firewall-cmd --get-active-zones":        "public\n  interfaces: eth0\ntrusted\n  sources: 10.99.0.0/16\n",
		"firewall-cmd --get-default-zone":        "public\n",
		"firewall-cmd --zone=public --list-all":  zonePublic,
		"firewall-cmd --zone=trusted --list-all": zoneTrusted,
		"firewall-cmd --info-service=ssh":        "ssh\n  ports: 22/tcp\n  protocols: \n",
		"firewall-cmd --info-service=cockpit":    "cockpit\n  ports: 9090/tcp\n",
	})
	mine := firewalldMine{"5432": {`rule family="ipv4" source address="10.20.0.21" port port="5432" protocol="tcp" accept`}}
	fw := readFirewalld(context.Background(), run, mine)
	if fw.DefaultIn != "reject" || strings.Join(fw.Zones, ",") != "public,trusted" {
		t.Fatalf("zones: %+v", fw)
	}
	byRaw := map[string]tunnel.FirewallRule{}
	for _, r := range fw.Rules {
		byRaw[r.Raw] = r
	}
	if r := byRaw["zone public: service ssh"]; r.Ports != "22" || r.Protocol != "tcp" {
		t.Errorf("ssh: %+v", r)
	}
	pg := byRaw[`rule family="ipv4" source address="10.20.0.21" port port="5432" protocol="tcp" accept`]
	if !pg.Argus || pg.ArgusPort != 5432 || pg.Sources[0] != "10.20.0.21" {
		t.Errorf("argus rich rule: %+v", pg)
	}
	if r := byRaw[`rule family="ipv4" source address="198.51.100.7" drop`]; r.Action != "deny" {
		t.Errorf("drop: %+v", r)
	}
	if r := byRaw["zone trusted: target ACCEPT"]; r.Action != "allow" || r.Sources[0] != "10.99.0.0/16" {
		t.Errorf("trusted zone: %+v", r)
	}
	if r := fw.ReachOf(3306); !r.Everyone {
		t.Errorf("3306 is open in the public zone: %+v", r)
	}
	if r := fw.ReachOf(5432); r.Everyone || strings.Join(r.Sources, ",") != "10.20.0.21,10.99.0.0/16" {
		t.Errorf("5432: %+v", r)
	}
}

func TestParseIptables(t *testing.T) {
	lines := strings.Split(`-P INPUT DROP
-P FORWARD DROP
-P OUTPUT ACCEPT
-N DOCKER
-A INPUT -i lo -j ACCEPT
-A INPUT -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A INPUT -s 10.8.0.0/24 -p tcp -m tcp --dport 22 -j ACCEPT
-A INPUT -p tcp -m multiport --dports 80,443 -j ACCEPT
-A INPUT -j f2b-sshd`, "\n")
	fw := parseIptables(lines)
	if !fw.Active || fw.DefaultIn != "deny" || fw.DefaultOut != "allow" || len(fw.Rules) != 3 {
		t.Fatalf("%+v", fw)
	}
	if r := fw.Rules[0]; r.Ports != "22" || r.Sources[0] != "10.8.0.0/24" || r.Action != "allow" {
		t.Errorf("ssh: %+v", r)
	}
	if r := fw.Rules[2]; r.Action != "other" {
		t.Errorf("a jump is not put in words: %+v", r)
	}
	if parseIptables(strings.Split("-P INPUT ACCEPT\n-P OUTPUT ACCEPT", "\n")).Active {
		t.Error("an empty INPUT that accepts does not filter")
	}
}

const nftOut = `{"nftables":[{"metainfo":{"version":"1.0.9"}},
{"table":{"family":"inet","name":"filter","handle":1}},
{"chain":{"family":"inet","table":"filter","name":"input","handle":1,"type":"filter","hook":"input","prio":0,"policy":"drop"}},
{"chain":{"family":"inet","table":"filter","name":"output","handle":2,"type":"filter","hook":"output","prio":0,"policy":"accept"}},
{"rule":{"family":"inet","table":"filter","chain":"input","handle":3,"expr":[{"match":{"op":"==","left":{"meta":{"key":"iif"}},"right":"lo"}},{"accept":null}]}},
{"rule":{"family":"inet","table":"filter","chain":"input","handle":4,"expr":[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},{"accept":null}]}},
{"rule":{"family":"inet","table":"filter","chain":"input","handle":5,"expr":[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":{"prefix":{"addr":"10.8.0.0","len":24}}}},{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":22}},{"accept":null}]}},
{"rule":{"family":"inet","table":"filter","chain":"input","handle":6,"expr":[{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":{"set":[80,443]}}},{"counter":{"packets":0,"bytes":0}},{"accept":null}]}},
{"rule":{"family":"inet","table":"filter","chain":"input","handle":7,"comment":"rate","expr":[{"jump":{"target":"limits"}}]}}
]}`

func TestParseNftJSON(t *testing.T) {
	fw, err := parseNftJSON([]byte(nftOut))
	if err != nil {
		t.Fatal(err)
	}
	if !fw.Active || fw.DefaultIn != "deny" || fw.DefaultOut != "allow" || len(fw.Rules) != 3 {
		t.Fatalf("%+v", fw)
	}
	if r := fw.Rules[0]; r.Ports != "22" || r.Sources[0] != "10.8.0.0/24" || r.Action != "allow" || r.Protocol != "tcp" {
		t.Errorf("ssh: %+v", r)
	}
	if r := fw.Rules[1]; r.Ports != "80,443" || len(r.Sources) != 0 {
		t.Errorf("web: %+v", r)
	}
	if r := fw.Rules[2]; r.Action != "other" || r.Name != "rate" {
		t.Errorf("jump: %+v", r)
	}
}

func TestReadLinuxFirewallChoosesTheOneInCharge(t *testing.T) {
	has := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, x := range names {
				if x == n {
					return true
				}
			}
			return false
		}
	}
	// ufw installed but off, firewalld running: firewalld is in charge.
	run := fakeRun(map[string]string{
		"ufw status verbose":                    "Status: inactive\n",
		"firewall-cmd --state":                  "running\n",
		"firewall-cmd --get-active-zones":       "public\n  interfaces: eth0\n",
		"firewall-cmd --get-default-zone":       "public\n",
		"firewall-cmd --zone=public --list-all": zonePublic,
	})
	fw := readLinuxFirewall(context.Background(), run, has("ufw", "firewall-cmd"), "")
	if fw.Kind != "firewalld" || !fw.Active || !fw.Manageable || strings.Join(fw.Installed, ",") != "ufw,firewalld" {
		t.Errorf("%+v", fw)
	}
	// Nothing filters: the installed one is named, and off.
	run = fakeRun(map[string]string{"ufw status verbose": "Status: inactive\n", "iptables -S": "-P INPUT ACCEPT\n-P OUTPUT ACCEPT\n"})
	fw = readLinuxFirewall(context.Background(), run, has("ufw", "iptables"), "")
	if fw.Kind != "ufw" || fw.Active || fw.ReachOf(80).Everyone != true {
		t.Errorf("off: %+v", fw)
	}
	// Nothing installed.
	fw = readLinuxFirewall(context.Background(), run, has(), "")
	if fw.Kind != "" || fw.Active || fw.Rules == nil {
		t.Errorf("none: %+v", fw)
	}
}

const winOut = `{"profiles":[{"name":"Domain","enabled":"True","in":"NotConfigured","out":"NotConfigured"},{"name":"Private","enabled":"True","in":"Block","out":"Allow"},{"name":"Public","enabled":"False","in":"Block","out":"Allow"}],
"categories":["DomainAuthenticated"],
"rules":[{"name":"Remote Desktop","group":"Argus","dir":"Inbound","action":"Allow","profile":"Any","proto":"TCP","port":"3389","remote":["10.8.0.0/255.255.255.0"]},
{"name":"File and Printer Sharing (SMB-In)","group":"@FirewallAPI.dll,-28502","dir":"Inbound","action":"Allow","profile":"Private, Public","proto":"TCP","port":"445","remote":["LocalSubnet"]},
{"name":"World Wide Web Services (HTTP Traffic-In)","group":"","dir":"Inbound","action":"Allow","profile":"Any","proto":"TCP","port":"80","remote":["Any"]},
{"name":"RPC","group":"","dir":"Inbound","action":"Allow","profile":"Domain","proto":"TCP","port":"RPCEPMap","remote":["Any"]}],
"off":214}`

func TestParseWinFirewall(t *testing.T) {
	fw, err := parseWinFirewall([]byte(winOut))
	if err != nil {
		t.Fatal(err)
	}
	if !fw.Active || fw.DefaultIn != "deny" || fw.DefaultOut != "allow" || fw.Hidden != 214 || len(fw.Profiles) != 3 {
		t.Fatalf("%+v", fw)
	}
	if !fw.Profiles[0].InUse || fw.Profiles[2].Enabled {
		t.Errorf("profiles: %+v", fw.Profiles)
	}
	rdp := fw.Rules[0]
	if !rdp.Argus || rdp.Ports != "3389" || rdp.Sources[0] != "10.8.0.0/24" {
		t.Errorf("rdp: %+v", rdp)
	}
	if r := fw.Rules[3]; r.Service != "RPCEPMap" || r.Ports != "" {
		t.Errorf("rpc: %+v", r)
	}
	// 445 is only for Private and Public; this server is on its domain.
	if r := fw.ReachOf(445); r.Everyone || len(r.Sources) != 0 {
		t.Errorf("445 should be closed on the Domain network type: %+v", r)
	}
	if r := fw.ReachOf(80); !r.Everyone {
		t.Errorf("80: %+v", r)
	}
}

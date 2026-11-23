package tunnel

import (
	"fmt"
	"slices"
	"strings"
)

/*
The commands that make a firewall hold Argus's lines, one builder per
firewall. The agent runs them; the review screen shows them. Every value in
a command comes from a line that passed Validate (keys of [a-z0-9], ports of
digits, commas and dashes, sources parsed as addresses), so nothing a person
typed reaches a shell or PowerShell unchecked.
*/

// FirewallCmd is one command, as its arguments.
type FirewallCmd []string

// String is the command as a person would type it.
func (c FirewallCmd) String() string {
	parts := make([]string, len(c))
	for i, a := range c {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=,@+%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

/* ── ufw ─────────────────────────────────────────────────────────────────── */

// UFWAdd is the commands that add a line, one per address. A line that
// blocks goes first ("prepend"), so it wins over every allow.
func UFWAdd(l FirewallLine) []FirewallCmd {
	var out []FirewallCmd
	for _, s := range lineSources(l) {
		verb := []string{"ufw", l.Action}
		if l.Action == "deny" {
			verb = []string{"ufw", "prepend", "deny"}
		}
		c := append(FirewallCmd{}, verb...)
		c = append(c, l.Direction)
		if l.Protocol == "tcp" || l.Protocol == "udp" {
			c = append(c, "proto", l.Protocol)
		}
		addr := s
		if addr == "" {
			addr = "any"
		}
		if l.Direction == "in" {
			c = append(c, "from", addr, "to", "any")
		} else {
			c = append(c, "to", addr)
		}
		if l.Ports != "" {
			c = append(c, "port", strings.ReplaceAll(l.Ports, "-", ":"))
		}
		c = append(c, "comment", FirewallLineMark(l.Key))
		out = append(out, c)
	}
	return out
}

// UFWDelete removes the rule numbered n in `ufw status numbered`.
func UFWDelete(n int) FirewallCmd { return FirewallCmd{"ufw", "--force", "delete", fmt.Sprint(n)} }

// UFWDefault sets what happens to traffic no rule names.
func UFWDefault(dir, policy string) FirewallCmd {
	d := "incoming"
	if dir == "out" {
		d = "outgoing"
	}
	return FirewallCmd{"ufw", "default", policy, d}
}

// UFWEnable switches ufw on.
func UFWEnable() FirewallCmd { return FirewallCmd{"ufw", "--force", "enable"} }

/* ── firewalld ───────────────────────────────────────────────────────────── */

// FirewalldAdd is the commands that add a line in zone: rich rules for
// traffic coming in, direct rules for traffic going out (firewalld's zones
// only filter what comes in). Each is run twice, for now and for after a
// restart; FirewalldRemove undoes exactly these.
func FirewalldAdd(zone string, l FirewallLine) []FirewallCmd {
	var out []FirewallCmd
	for _, c := range firewalldRules(zone, l) {
		out = append(out, c, append(FirewallCmd{c[0], "--permanent"}, c[1:]...))
	}
	return out
}

// FirewalldRemove is the commands that take a line FirewalldAdd added away.
func FirewalldRemove(add []FirewallCmd) []FirewallCmd {
	out := make([]FirewallCmd, 0, len(add))
	for _, c := range add {
		r := slices.Clone(c)
		for i, a := range r {
			switch {
			case strings.HasPrefix(a, "--add-rich-rule="):
				r[i] = "--remove-rich-rule=" + strings.TrimPrefix(a, "--add-rich-rule=")
			case a == "--add-rule":
				r[i] = "--remove-rule"
			}
		}
		out = append(out, r)
	}
	return out
}

func firewalldRules(zone string, l FirewallLine) []FirewallCmd {
	var out []FirewallCmd
	if l.Direction == "out" {
		for _, s := range lineSources(l) {
			fams := []string{"ipv4", "ipv6"}
			if s != "" {
				fams = []string{"ipv4"}
				if isV6(s) {
					fams = []string{"ipv6"}
				}
			}
			for _, fam := range fams {
				prio := "1"
				target := "ACCEPT"
				if l.Action == "deny" {
					prio, target = "0", "DROP"
				}
				c := FirewallCmd{"firewall-cmd", "--direct", "--add-rule", fam, "filter", "OUTPUT", prio}
				c = append(c, iptMatch(fam, l, s, "-d")...)
				c = append(c, "-m", "comment", "--comment", FirewallLineMark(l.Key), "-j", target)
				out = append(out, c)
			}
		}
		return out
	}
	verdict := "accept"
	if l.Action == "deny" {
		verdict = "drop"
	}
	ports := []string{""}
	if l.Ports != "" {
		ports = strings.Split(l.Ports, ",")
	}
	for _, s := range lineSources(l) {
		fams := []string{""}
		if s != "" {
			fams = []string{"ipv4"}
			if isV6(s) {
				fams = []string{"ipv6"}
			}
		} else if l.Protocol == "icmp" || l.Ports == "" {
			fams = []string{"ipv4", "ipv6"}
		}
		for _, fam := range fams {
			for _, p := range ports {
				rr := "rule"
				if fam != "" {
					rr += ` family="` + fam + `"`
				}
				if s != "" {
					rr += ` source address="` + s + `"`
				}
				switch {
				case p != "":
					rr += ` port port="` + p + `" protocol="` + l.Protocol + `"`
				case l.Protocol == "icmp":
					proto := "icmp"
					if fam == "ipv6" {
						proto = "ipv6-icmp"
					}
					rr += ` protocol value="` + proto + `"`
				case l.Protocol == "tcp" || l.Protocol == "udp":
					rr += ` protocol value="` + l.Protocol + `"`
				}
				rr += " " + verdict
				out = append(out, FirewallCmd{"firewall-cmd", "--zone=" + zone, "--add-rich-rule=" + rr})
			}
		}
	}
	return out
}

// iptMatch is the iptables match for a line's protocol, ports and one
// address on the other side (flag -s or -d).
func iptMatch(fam string, l FirewallLine, addr, flag string) []string {
	var m []string
	switch l.Protocol {
	case "tcp", "udp":
		m = append(m, "-p", l.Protocol)
	case "icmp":
		if fam == "ipv6" {
			m = append(m, "-p", "ipv6-icmp")
		} else {
			m = append(m, "-p", "icmp")
		}
	}
	if addr != "" {
		m = append(m, flag, addr)
	}
	if l.Ports != "" {
		p := strings.ReplaceAll(l.Ports, "-", ":")
		if strings.Contains(p, ",") {
			m = append(m, "-m", "multiport", "--dports", p)
		} else {
			m = append(m, "--dport", p)
		}
	}
	return m
}

// FirewalldOutBase is what blocking everything going out needs first: the
// loopback and replies stay open. The drop itself is FirewalldOutDrop.
func FirewalldOutBase() []FirewallCmd {
	var out []FirewallCmd
	for _, fam := range []string{"ipv4", "ipv6"} {
		for _, c := range []FirewallCmd{
			{"firewall-cmd", "--direct", "--add-rule", fam, "filter", "OUTPUT", "0", "-o", "lo", "-m", "comment", "--comment", FirewallLineMark("base"), "-j", "ACCEPT"},
			{"firewall-cmd", "--direct", "--add-rule", fam, "filter", "OUTPUT", "0", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-m", "comment", "--comment", FirewallLineMark("base"), "-j", "ACCEPT"},
			{"firewall-cmd", "--direct", "--add-rule", fam, "filter", "OUTPUT", "9", "-m", "comment", "--comment", FirewallLineMark("outdrop"), "-j", "DROP"},
		} {
			out = append(out, c, append(FirewallCmd{c[0], "--permanent"}, c[1:]...))
		}
	}
	return out
}

// FirewalldTarget sets what a zone does with traffic no rule names, after
// a restart; FirewalldReload makes it so now.
func FirewalldTarget(zone, policy string) FirewallCmd {
	t := "default"
	if policy == "allow" {
		t = "ACCEPT"
	} else if policy == "deny" {
		t = "DROP"
	}
	return FirewallCmd{"firewall-cmd", "--permanent", "--zone=" + zone, "--set-target=" + t}
}

// FirewalldReload loads the saved settings.
func FirewalldReload() FirewallCmd { return FirewallCmd{"firewall-cmd", "--reload"} }

// FirewalldEnable starts firewalld now and at every boot.
func FirewalldEnable() FirewallCmd { return FirewallCmd{"systemctl", "enable", "--now", "firewalld"} }

// FirewalldTakeOver is what removes a rule made by hand, by the text the
// reader gave it: a service or port of a zone, or a rich rule.
func FirewalldTakeOver(raw string) ([]FirewallCmd, error) {
	pair := func(c FirewallCmd) []FirewallCmd {
		return []FirewallCmd{c, append(FirewallCmd{c[0], "--permanent"}, c[1:]...)}
	}
	if zone, rest, ok := strings.Cut(raw, ": "); ok && strings.HasPrefix(zone, "zone ") {
		z := strings.TrimPrefix(zone, "zone ")
		switch {
		case strings.HasPrefix(rest, "service "):
			return pair(FirewallCmd{"firewall-cmd", "--zone=" + z, "--remove-service=" + strings.TrimPrefix(rest, "service ")}), nil
		case strings.HasPrefix(rest, "port "):
			return pair(FirewallCmd{"firewall-cmd", "--zone=" + z, "--remove-port=" + strings.TrimPrefix(rest, "port ")}), nil
		}
		return nil, fmt.Errorf("Argus cannot take over %q: change the zone itself", raw)
	}
	if strings.HasPrefix(raw, "rule ") {
		return pair(FirewallCmd{"firewall-cmd", "--remove-rich-rule=" + raw}), nil
	}
	return nil, fmt.Errorf("Argus cannot take over %q", raw)
}

/* ── nftables ────────────────────────────────────────────────────────────── */

// NftTable is Argus's own nftables table holding every line, written whole
// each time: "table inet argus" and nothing else in the ruleset is touched.
// in and out are the chains' policies, "accept" or "drop".
func NftTable(lines []FirewallLine, in, out string) string {
	var b strings.Builder
	b.WriteString("table inet argus\ndelete table inet argus\ntable inet argus {\n")
	for _, ch := range []struct{ name, dir, policy string }{{"input", "in", in}, {"output", "out", out}} {
		fmt.Fprintf(&b, "\tchain %s {\n\t\ttype filter hook %s priority filter - 5; policy %s;\n", ch.name, ch.name, ch.policy)
		lo := "iif"
		if ch.dir == "out" {
			lo = "oif"
		}
		fmt.Fprintf(&b, "\t\t%s \"lo\" accept\n\t\tct state established,related accept\n", lo)
		ordered := slices.Clone(lines)
		slices.SortStableFunc(ordered, func(a, b FirewallLine) int {
			if (a.Action == "deny") == (b.Action == "deny") {
				return 0
			}
			if a.Action == "deny" {
				return -1
			}
			return 1
		})
		for _, l := range ordered {
			if l.Direction != ch.dir {
				continue
			}
			for _, r := range nftRules(l) {
				b.WriteString("\t\t" + r + "\n")
			}
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func nftRules(l FirewallLine) []string {
	addrKey := "saddr"
	if l.Direction == "out" {
		addrKey = "daddr"
	}
	var v4, v6 []string
	for _, s := range l.Sources {
		if isV6(s) {
			v6 = append(v6, s)
		} else {
			v4 = append(v4, s)
		}
	}
	verdict := "accept"
	if l.Action == "deny" {
		verdict = "drop"
	}
	match := ""
	switch {
	case l.Ports != "":
		match = l.Protocol + " dport { " + strings.ReplaceAll(l.Ports, ",", ", ") + " } "
	case l.Protocol == "icmp":
		match = "meta l4proto { icmp, ipv6-icmp } "
	case l.Protocol == "tcp" || l.Protocol == "udp":
		match = "meta l4proto " + l.Protocol + " "
	}
	tail := match + verdict + ` comment "` + FirewallLineMark(l.Key) + `"`
	if len(l.Sources) == 0 {
		return []string{tail}
	}
	var out []string
	if len(v4) > 0 {
		out = append(out, "ip "+addrKey+" { "+strings.Join(v4, ", ")+" } "+tail)
	}
	if len(v6) > 0 {
		out = append(out, "ip6 "+addrKey+" { "+strings.Join(v6, ", ")+" } "+tail)
	}
	return out
}

/* ── Windows ─────────────────────────────────────────────────────────────── */

// WinGroup is the rule group Argus's Windows Firewall rules go in.
const WinGroup = "Argus"

// WinAdd is the PowerShell that adds a line as one rule (two for ping:
// ICMPv4 and ICMPv6).
func WinAdd(l FirewallLine) []string {
	dir, portFlag := "Inbound", "-LocalPort"
	if l.Direction == "out" {
		dir, portFlag = "Outbound", "-RemotePort"
	}
	act := "Allow"
	if l.Action == "deny" {
		act = "Block"
	}
	protos := []string{"Any"}
	switch l.Protocol {
	case "tcp":
		protos = []string{"TCP"}
	case "udp":
		protos = []string{"UDP"}
	case "icmp":
		protos = []string{"ICMPv4", "ICMPv6"}
	}
	var out []string
	for _, p := range protos {
		c := fmt.Sprintf("New-NetFirewallRule -DisplayName %s -Group %s -Direction %s -Action %s -Protocol %s -Profile Any",
			psQuote(FirewallLineMark(l.Key)), psQuote(WinGroup), dir, act, p)
		if l.Ports != "" {
			c += " " + portFlag + " " + l.Ports
		}
		if len(l.Sources) > 0 {
			c += " -RemoteAddress " + strings.Join(l.Sources, ",")
		}
		out = append(out, c+" | Out-Null")
	}
	return out
}

// WinRemove is the PowerShell that removes a line.
func WinRemove(key string) string {
	return "Remove-NetFirewallRule -DisplayName " + psQuote(FirewallLineMark(key)) + " -ErrorAction SilentlyContinue"
}

// WinTakeOver switches off a rule made by hand, by its display name:
// Windows's own rules are switched off, never deleted.
func WinTakeOver(name string) string {
	return "Disable-NetFirewallRule -DisplayName " + psQuote(name)
}

// WinDefault sets what happens to traffic no rule names, on every profile.
func WinDefault(dir, policy string) string {
	flag := "-DefaultInboundAction"
	if dir == "out" {
		flag = "-DefaultOutboundAction"
	}
	v := "Allow"
	if policy == "deny" {
		v = "Block"
	}
	return "Set-NetFirewallProfile -All " + flag + " " + v
}

// WinEnable switches Windows Firewall on for every profile.
func WinEnable() string { return "Set-NetFirewallProfile -All -Enabled True" }

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

/* ── The review screen ───────────────────────────────────────────────────── */

// FirewallPreview is what the agent will run for spec on a firewall of kind
// whose Argus lines are now have: removals first, then additions, then the
// defaults and switching on. ufw deletions are by number, which the agent
// reads just before; the preview names the line instead.
func FirewallPreview(kind, zone string, have map[string]FirewallLine, spec FirewallChangeSpec) []string {
	if spec.PutBack != "" {
		return []string{"put back the firewall saved before change " + spec.PutBack}
	}
	want := map[string]FirewallLine{}
	for _, l := range append(slices.Clone(spec.Lines), spec.Protect...) {
		want[l.Key] = l
	}
	var drop, add []FirewallLine
	for _, k := range sortedKeys(have) {
		if w, ok := want[k]; !ok || !SameLine(w, have[k]) {
			drop = append(drop, have[k])
		}
	}
	for _, k := range sortedKeys(want) {
		if h, ok := have[k]; !ok || !SameLine(h, want[k]) {
			add = append(add, want[k])
		}
	}
	var out []string
	if spec.Install != "" {
		out = append(out, InstallCmd(spec.Install).String())
	}
	switch kind {
	case FirewallUFW:
		for _, l := range drop {
			out = append(out, "ufw --force delete <each rule marked "+FirewallLineMark(l.Key)+">")
		}
		for _, raw := range spec.TakeOver {
			out = append(out, "ufw --force delete <the rule "+raw+">")
		}
		for _, l := range add {
			for _, c := range UFWAdd(l) {
				out = append(out, c.String())
			}
		}
		if spec.DefaultIn != "" {
			out = append(out, UFWDefault("in", spec.DefaultIn).String())
		}
		if spec.DefaultOut != "" {
			out = append(out, UFWDefault("out", spec.DefaultOut).String())
		}
		if spec.TurnOn {
			out = append(out, UFWEnable().String())
		}
	case FirewallFirewalld:
		for _, l := range drop {
			for _, c := range FirewalldRemove(FirewalldAdd(zone, l)) {
				out = append(out, c.String())
			}
		}
		for _, raw := range spec.TakeOver {
			cs, err := FirewalldTakeOver(raw)
			if err != nil {
				out = append(out, "# "+err.Error())
			}
			for _, c := range cs {
				out = append(out, c.String())
			}
		}
		for _, l := range add {
			for _, c := range FirewalldAdd(zone, l) {
				out = append(out, c.String())
			}
		}
		if spec.DefaultOut == "deny" {
			for _, c := range FirewalldOutBase() {
				out = append(out, c.String())
			}
		}
		if spec.DefaultIn != "" {
			out = append(out, FirewalldTarget(zone, spec.DefaultIn).String(), FirewalldReload().String())
		}
		if spec.TurnOn {
			out = append(out, FirewalldEnable().String())
		}
	case FirewallNftables:
		in, outP := "accept", "accept"
		if spec.DefaultIn == "deny" || spec.TurnOn {
			in = "drop"
		}
		if spec.DefaultOut == "deny" {
			outP = "drop"
		}
		out = append(out, "nft -f - <<EOF", strings.TrimSuffix(NftTable(append(slices.Clone(spec.Lines), spec.Protect...), in, outP), "\n"), "EOF")
	case FirewallWindows:
		for _, l := range drop {
			out = append(out, WinRemove(l.Key))
		}
		for _, raw := range spec.TakeOver {
			out = append(out, WinTakeOver(raw))
		}
		for _, l := range add {
			out = append(out, WinAdd(l)...)
		}
		if spec.DefaultIn != "" {
			out = append(out, WinDefault("in", spec.DefaultIn))
		}
		if spec.DefaultOut != "" {
			out = append(out, WinDefault("out", spec.DefaultOut))
		}
		if spec.TurnOn {
			out = append(out, WinEnable())
		}
	}
	return out
}

// InstallCmd installs a firewall with the server's own package manager.
func InstallCmd(fw string) FirewallCmd {
	if fw == FirewallFirewalld {
		return FirewallCmd{"dnf", "-y", "install", "firewalld"}
	}
	return FirewallCmd{"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "-y", "install", "ufw"}
}

func sortedKeys(m map[string]FirewallLine) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

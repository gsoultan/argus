package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Reading the whole firewall on a server, for the Firewall tab (tunnel/hostfirewall.go).

Read only. On Linux the daemon's sandbox cannot run ufw or firewall-cmd, so it
runs `argus-agent firewall-read` as root through systemd-run, like the
per-port firewall helper (firewall.go); on Windows the service already runs
as Local System and asks PowerShell itself (hostfw_windows.go).

Which firewall is in charge, in order: ufw when it is active, firewalld when
it is running, iptables when its INPUT chain filters anything, nftables when
a table of its own has an input hook; otherwise the first one installed, and
off. The readers never fail the whole report: a part they could not read is
said in Note, and a rule they cannot put in words keeps its own text in Raw.
*/

const firewallReadTimeout = 40 * time.Second

// firewallReadRequest is what the helper is asked: only where Argus keeps
// the firewalld rules it added, so those can be told apart.
type firewallReadRequest struct {
	StateFile string `json:"stateFile,omitempty"`
}

// RunFirewallRead is `argus-agent firewall-read`: the request on stdin,
// the report on stdout. It runs as root, outside the daemon's sandbox.
func RunFirewallRead(in io.Reader, out io.Writer) int {
	var req firewallReadRequest
	_ = json.NewDecoder(io.LimitReader(in, 1<<12)).Decode(&req)
	ctx, cancel := context.WithTimeout(context.Background(), firewallReadTimeout)
	defer cancel()
	res := readLinuxFirewall(ctx, execRun, lookPath, req.StateFile)
	_ = json.NewEncoder(out).Encode(res.Bounded())
	return 0
}

// readLinuxFirewall finds the firewall in charge and reads it.
func readLinuxFirewall(ctx context.Context, run runCmd, has func(string) bool, stateFile string) tunnel.HostFirewall {
	fw := tunnel.HostFirewall{Rules: []tunnel.FirewallRule{}, ReadAt: time.Now().UTC()}
	for _, t := range []struct{ kind, bin string }{
		{tunnel.FirewallUFW, "ufw"}, {tunnel.FirewallFirewalld, "firewall-cmd"},
		{tunnel.FirewallNftables, "nft"}, {tunnel.FirewallIptables, "iptables"},
	} {
		if has(t.bin) {
			fw.Installed = append(fw.Installed, t.kind)
		}
	}
	installed := func(k string) bool { return slices.Contains(fw.Installed, k) }
	var ipt []string
	if installed(tunnel.FirewallIptables) {
		if out, err := run(ctx, "iptables", "-S"); err == nil {
			ipt = strings.Split(out, "\n")
			fw.Docker = slices.ContainsFunc(ipt, func(l string) bool { return strings.HasPrefix(l, "-N DOCKER") })
		}
	}

	if installed(tunnel.FirewallUFW) {
		if out, err := run(ctx, "ufw", "status", "verbose"); err == nil {
			u := parseUFWVerbose(out)
			if u.Active {
				return finish(fw, u)
			}
		}
	}
	if installed(tunnel.FirewallFirewalld) {
		if out, _ := run(ctx, "firewall-cmd", "--state"); strings.TrimSpace(out) == "running" {
			return finish(fw, readFirewalld(ctx, run, readFirewalldMine(stateFile), argusRichRules(stateFile)))
		}
	}
	if ipt != nil {
		if r := parseIptables(ipt); r.Active {
			return finish(fw, r)
		}
	}
	if installed(tunnel.FirewallNftables) {
		if out, err := run(ctx, "nft", "-j", "list", "ruleset"); err == nil {
			if r, err := parseNftJSON([]byte(out)); err == nil && r.Active {
				return finish(fw, r)
			} else if err != nil {
				fw.Note = "the nftables rules could not be read: " + err.Error()
			}
		}
	}
	// Nothing filters. Name the firewall that is there and off.
	for _, k := range []string{tunnel.FirewallUFW, tunnel.FirewallFirewalld, tunnel.FirewallNftables, tunnel.FirewallIptables} {
		if installed(k) {
			fw.Kind = k
			break
		}
	}
	fw.Manageable = tunnel.ManageableFirewall(fw.Kind)
	return fw
}

// finish copies what a reader found onto the report.
func finish(fw, r tunnel.HostFirewall) tunnel.HostFirewall {
	fw.Kind, fw.Active = r.Kind, r.Active
	fw.DefaultIn, fw.DefaultOut, fw.Zones = r.DefaultIn, r.DefaultOut, r.Zones
	fw.Rules = r.Rules
	if fw.Rules == nil {
		fw.Rules = []tunnel.FirewallRule{}
	}
	if r.Note != "" {
		fw.Note = r.Note
	}
	fw.Manageable = tunnel.ManageableFirewall(fw.Kind)
	return fw
}

// ---------------------------------------------------------------- ufw

var (
	ufwVerboseRule = regexp.MustCompile(`^(.+?)\s+(ALLOW|DENY|REJECT|LIMIT)(?: (IN|OUT|FWD))?\s+(.+?)\s*(?:#\s*(.*))?$`)
	ufwDefault     = regexp.MustCompile(`(allow|deny|reject|disabled) \((incoming|outgoing)\)`)
)

// ufwApps are the application profiles ufw ships, by the name its status shows.
var ufwApps = map[string]string{
	"OpenSSH": "22", "Nginx Full": "80,443", "Nginx HTTP": "80", "Nginx HTTPS": "443",
	"Apache": "80", "Apache Full": "80,443", "Apache Secure": "443", "WWW": "80", "WWW Full": "80,443",
	"WWW Secure": "443", "Postfix": "25", "Dovecot IMAP": "143", "Dovecot Secure IMAP": "993",
	"Samba": "137,138,139,445", "CUPS": "631", "Bind9": "53",
}

func parseUFWVerbose(out string) tunnel.HostFirewall {
	fw := tunnel.HostFirewall{Kind: tunnel.FirewallUFW}
	inRules := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, " \r")
		switch {
		case strings.HasPrefix(line, "Status:"):
			fw.Active = strings.TrimSpace(strings.TrimPrefix(line, "Status:")) == "active"
			continue
		case strings.HasPrefix(line, "Default:"):
			for _, m := range ufwDefault.FindAllStringSubmatch(line, -1) {
				v := m[1]
				if v == "disabled" {
					v = ""
				}
				if m[2] == "incoming" {
					fw.DefaultIn = v
				} else {
					fw.DefaultOut = v
				}
			}
			continue
		case strings.HasPrefix(line, "--"):
			inRules = true
			continue
		}
		if !inRules || strings.TrimSpace(line) == "" {
			continue
		}
		m := ufwVerboseRule.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		to, act, dir, from, comment := strings.TrimSpace(m[1]), m[2], m[3], strings.TrimSpace(m[4]), strings.TrimSpace(m[5])
		r := tunnel.FirewallRule{Direction: "in", Action: strings.ToLower(act), Protocol: "any",
			Name: comment, Raw: strings.Join(strings.Fields(line), " ")}
		if dir == "OUT" {
			r.Direction = "out"
		} else if dir == "FWD" {
			continue // routed traffic is not this server's own
		}
		if strings.HasSuffix(to, "(v6)") || strings.HasSuffix(from, "(v6)") {
			r.IPv6 = true
			to = strings.TrimSpace(strings.TrimSuffix(to, "(v6)"))
			from = strings.TrimSpace(strings.TrimSuffix(from, "(v6)"))
		}
		// The side that is not this server: where traffic comes from (in)
		// or goes to (out). The server's side names the port.
		here, there := to, from
		if r.Direction == "out" {
			here, there = from, to
		}
		r.Ports, r.Protocol, r.Service = ufwPortSpec(here)
		if a := ufwAddr(there); a != "" {
			r.Sources = []string{a}
		}
		if p := tunnel.ArgusCommentPort(comment); p > 0 {
			r.Argus, r.ArgusPort = true, p
		}
		fw.Rules = append(fw.Rules, r)
	}
	return fw
}

// ufwPortSpec reads the port part of a ufw "To": "22/tcp", "80,443/tcp",
// "6000:7000/tcp", "53", "10.0.0.5 22/tcp", "Anywhere", "OpenSSH".
func ufwPortSpec(s string) (ports, proto, service string) {
	s = strings.TrimSpace(strings.Split(s, " on ")[0])
	if p, ok := ufwApps[s]; ok {
		return p, "tcp", s
	}
	f := strings.Fields(s)
	if len(f) == 0 {
		return "", "any", ""
	}
	last := f[len(f)-1]
	if strings.HasPrefix(last, "Anywhere") || isAddr(last) {
		return "", "any", ""
	}
	spec, pr, _ := strings.Cut(last, "/")
	if pr == "" {
		pr = "any"
	}
	if _, err := strconv.Atoi(strings.NewReplacer(",", "", ":", "").Replace(spec)); err != nil {
		return "", "any", s // an application profile ufw did not ship
	}
	return strings.ReplaceAll(spec, ":", "-"), pr, ""
}

// ufwAddr reads the address part of a ufw "From" or "To"; "" is anywhere.
func ufwAddr(s string) string {
	s = strings.TrimSpace(strings.Split(s, " on ")[0])
	f := strings.Fields(s)
	if len(f) == 0 || strings.HasPrefix(f[0], "Anywhere") {
		return ""
	}
	if isAddr(f[0]) {
		return f[0]
	}
	return ""
}

func isAddr(s string) bool {
	if _, err := netip.ParsePrefix(s); err == nil {
		return true
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}

// ---------------------------------------------------------------- firewalld

var richParts = struct{ src, port, proto, svc, family *regexp.Regexp }{
	src:    regexp.MustCompile(`source address="([^"]+)"`),
	port:   regexp.MustCompile(`port port="([^"]+)"`),
	proto:  regexp.MustCompile(`protocol="([^"]+)"`),
	svc:    regexp.MustCompile(`service name="([^"]+)"`),
	family: regexp.MustCompile(`family="ipv6"`),
}

// firewalldZone is one zone's --list-all.
type firewalldZone struct {
	name, target                  string
	interfaces, sources, services []string
	ports, rich                   []string
}

func parseFirewalldZone(out string) firewalldZone {
	var z firewalldZone
	lines := strings.Split(out, "\n")
	if len(lines) > 0 {
		z.name, _, _ = strings.Cut(strings.TrimSpace(lines[0]), " ")
	}
	inRich := false
	for _, l := range lines[1:] {
		t := strings.TrimSpace(l)
		if inRich && strings.HasPrefix(t, "rule ") {
			z.rich = append(z.rich, t)
			continue
		}
		inRich = false
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "target":
			z.target = v
		case "interfaces":
			z.interfaces = strings.Fields(v)
		case "sources":
			z.sources = strings.Fields(v)
		case "services":
			z.services = strings.Fields(v)
		case "ports":
			z.ports = strings.Fields(v)
		case "rich rules":
			inRich = true
		}
	}
	return z
}

// argusRichRules are the firewalld rules Argus added for its lines, by the
// rule's text, with the line's key (hostfw_linux_backend.go keeps them).
func argusRichRules(stateFile string) map[string]string {
	out := map[string]string{}
	if stateFile == "" {
		return out
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(stateFile), firewallLines))
	if err != nil {
		return out
	}
	var m map[string]fwdLine
	if json.Unmarshal(b, &m) != nil {
		return out
	}
	for k, l := range m {
		for _, c := range l.Cmds {
			for _, a := range c {
				if rr, ok := strings.CutPrefix(a, "--add-rich-rule="); ok {
					out[rr] = k
				}
			}
		}
	}
	return out
}

func readFirewalld(ctx context.Context, run runCmd, mine firewalldMine, lines map[string]string) tunnel.HostFirewall {
	fw := tunnel.HostFirewall{Kind: tunnel.FirewallFirewalld, Active: true, DefaultOut: "allow"}
	out, err := run(ctx, "firewall-cmd", "--get-active-zones")
	if err != nil {
		fw.Note = "the active zones could not be read"
		return fw
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			names = append(names, strings.TrimSpace(l))
		}
	}
	if def, err := run(ctx, "firewall-cmd", "--get-default-zone"); err == nil {
		if d := strings.TrimSpace(def); d != "" && !slices.Contains(names, d) {
			names = append(names, d) // takes every interface no zone names
		}
	}
	argus := map[string]bool{}
	for _, rs := range mine {
		for _, r := range rs {
			argus[r] = true
		}
	}
	svcPorts := map[string]string{}
	portsOf := func(svc string) string {
		if p, ok := svcPorts[svc]; ok {
			return p
		}
		p := ""
		if out, err := run(ctx, "firewall-cmd", "--info-service="+svc); err == nil {
			p = firewalldServicePorts(out)
		}
		svcPorts[svc] = p
		return p
	}
	for _, n := range names {
		out, err := run(ctx, "firewall-cmd", "--zone="+n, "--list-all")
		if err != nil {
			continue
		}
		z := parseFirewalldZone(out)
		fw.Zones = append(fw.Zones, z.name)
		var src []string
		if len(z.sources) > 0 {
			src = z.sources
		}
		// A zone bound to sources lets them in whole when its target is
		// ACCEPT; the zone that holds interfaces says what happens to the
		// rest of the traffic.
		switch target := strings.ToUpper(z.target); {
		case len(z.sources) > 0:
			if target == "ACCEPT" {
				fw.Rules = append(fw.Rules, tunnel.FirewallRule{Direction: "in", Action: "allow", Protocol: "any",
					Sources: src, Name: "zone " + z.name, Raw: "zone " + z.name + ": target ACCEPT"})
			}
		case fw.DefaultIn == "":
			switch target {
			case "ACCEPT":
				fw.DefaultIn = "allow"
			case "DROP":
				fw.DefaultIn = "deny"
			default: // "default" and REJECT
				fw.DefaultIn = "reject"
			}
		}
		for _, s := range z.services {
			p, pr := splitPortProto(portsOf(s))
			fw.Rules = append(fw.Rules, tunnel.FirewallRule{Direction: "in", Action: "allow", Protocol: pr, Ports: p,
				Service: s, Sources: src, Name: "zone " + z.name, Raw: "zone " + z.name + ": service " + s})
		}
		for _, p := range z.ports {
			port, pr, _ := strings.Cut(p, "/")
			r := tunnel.FirewallRule{Direction: "in", Action: "allow", Protocol: orAny(pr), Ports: port,
				Sources: src, Name: "zone " + z.name, Raw: "zone " + z.name + ": port " + p}
			if argus["port"] {
				// Argus lists an opened port as "port" under its number.
				for k, rs := range mine {
					if slices.Contains(rs, "port") && k == port {
						r.Argus, r.ArgusPort = true, fwAtoi(k)
					}
				}
			}
			fw.Rules = append(fw.Rules, r)
		}
		for _, rr := range z.rich {
			r := richToRule(rr, z.name, argus[rr], portsOf)
			if k, ok := lines[rr]; ok {
				r.Name = tunnel.FirewallLineMark(k)
			}
			fw.Rules = append(fw.Rules, r)
		}
	}
	// Going out: firewalld's zones do not filter it; direct rules do.
	if out, err := run(ctx, "firewall-cmd", "--direct", "--get-all-rules"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			r, drop, ok := firewalldDirectRule(l)
			if drop {
				fw.DefaultOut = "deny"
			}
			if ok {
				fw.Rules = append(fw.Rules, r)
			}
		}
	}
	return fw
}

// shellFields splits like a shell: 'a b' and "a b" are one field.
func shellFields(s string) []string {
	var out []string
	var cur strings.Builder
	var q rune
	in := false
	for _, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			q, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// firewalldDirectRule reads one line of `firewall-cmd --direct
// --get-all-rules` on the OUTPUT chain: "ipv4 filter OUTPUT 1 -p tcp -d
// 203.0.113.9 --dport 443 -m comment --comment 'argus fw-k' -j ACCEPT".
// drop says it is Argus's "block everything else going out".
func firewalldDirectRule(line string) (r tunnel.FirewallRule, drop, ok bool) {
	f := shellFields(line)
	if len(f) < 4 || f[1] != "filter" || f[2] != "OUTPUT" {
		return r, false, false
	}
	r = tunnel.FirewallRule{Direction: "out", Action: "allow", Protocol: "any", IPv6: f[0] == "ipv6",
		Raw: strings.TrimSpace(line)}
	for i := 4; i < len(f); i++ {
		next := func() string {
			if i+1 < len(f) {
				i++
				return f[i]
			}
			return ""
		}
		switch f[i] {
		case "-p":
			switch p := next(); p {
			case "tcp", "udp":
				r.Protocol = p
			case "icmp", "ipv6-icmp":
				r.Protocol = "icmp"
			}
		case "-d":
			r.Sources = []string{next()}
		case "--dport", "--dports":
			r.Ports = strings.ReplaceAll(next(), ":", "-")
		case "--comment":
			r.Name = next()
		case "-o", "--ctstate":
			return r, false, false // the loopback and replies
		case "-j":
			switch next() {
			case "DROP", "REJECT":
				r.Action = "deny"
			}
		}
	}
	if r.Name == tunnel.FirewallLineMark("outdrop") {
		return r, true, false
	}
	if r.Name == tunnel.FirewallLineMark("base") {
		return r, false, false
	}
	return r, false, true
}

func richToRule(rr, zone string, argus bool, portsOf func(string) string) tunnel.FirewallRule {
	r := tunnel.FirewallRule{Direction: "in", Action: "allow", Protocol: "any", Name: "zone " + zone, Raw: rr, Argus: argus}
	switch {
	case strings.HasSuffix(rr, " drop"):
		r.Action = "deny"
	case strings.Contains(rr, " reject"):
		r.Action = "reject"
	case strings.Contains(rr, " accept"):
	default:
		r.Action = "other"
	}
	if strings.Contains(rr, "source NOT") || strings.Contains(rr, "destination ") {
		r.Action = "other" // not something a sentence can carry; shown as written
	}
	if m := richParts.src.FindStringSubmatch(rr); m != nil {
		r.Sources = []string{m[1]}
	}
	if m := richParts.port.FindStringSubmatch(rr); m != nil {
		r.Ports = m[1]
		if p := richParts.proto.FindStringSubmatch(rr); p != nil {
			r.Protocol = p[1]
		}
	} else if m := richParts.svc.FindStringSubmatch(rr); m != nil {
		r.Service = m[1]
		r.Ports, r.Protocol = splitPortProto(portsOf(m[1]))
	}
	r.IPv6 = richParts.family.MatchString(rr)
	if argus && r.Ports != "" {
		r.ArgusPort = fwAtoi(r.Ports)
	}
	return r
}

// firewalldServicePorts reads "ports: 22/tcp" from --info-service.
func firewalldServicePorts(out string) string {
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if ok && k == "ports" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

// splitPortProto turns "80/tcp 443/tcp" into "80,443" and "tcp"; mixed
// protocols are "any".
func splitPortProto(s string) (string, string) {
	var ports []string
	proto := ""
	for _, f := range strings.Fields(s) {
		p, pr, _ := strings.Cut(f, "/")
		ports = append(ports, p)
		if proto == "" {
			proto = pr
		} else if proto != pr {
			proto = "any"
		}
	}
	return strings.Join(ports, ","), orAny(proto)
}

func orAny(s string) string {
	if s == "" {
		return "any"
	}
	return s
}

func fwAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ---------------------------------------------------------------- iptables

// parseIptables reads `iptables -S`: the INPUT and OUTPUT policies and the
// rules in those two chains. A rule that jumps to another chain is kept as
// written. Active when INPUT drops by default or holds any rule that is not
// the usual loopback and established-connection lines.
func parseIptables(lines []string) tunnel.HostFirewall {
	fw := tunnel.HostFirewall{Kind: tunnel.FirewallIptables}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		switch {
		case f[0] == "-P" && (f[1] == "INPUT" || f[1] == "OUTPUT"):
			v := "allow"
			if f[2] == "DROP" {
				v = "deny"
			} else if f[2] == "REJECT" {
				v = "reject"
			}
			if f[1] == "INPUT" {
				fw.DefaultIn = v
				fw.Active = fw.Active || v != "allow"
			} else {
				fw.DefaultOut = v
			}
		case f[0] == "-A" && (f[1] == "INPUT" || f[1] == "OUTPUT"):
			r, skip := iptablesRule(f)
			if skip {
				continue
			}
			if f[1] == "INPUT" {
				fw.Active = true
			}
			fw.Rules = append(fw.Rules, r)
		}
	}
	return fw
}

func iptablesRule(f []string) (tunnel.FirewallRule, bool) {
	r := tunnel.FirewallRule{Direction: "in", Protocol: "any", Raw: strings.Join(f, " ")}
	if f[1] == "OUTPUT" {
		r.Direction = "out"
	}
	for i := 2; i < len(f); i++ {
		arg := func() string {
			if i+1 < len(f) {
				i++
				return f[i]
			}
			return ""
		}
		switch f[i] {
		case "-i", "-o":
			if arg() == "lo" {
				return r, true
			}
		case "--ctstate", "--state":
			if v := arg(); strings.Contains(v, "ESTABLISHED") {
				return r, true
			}
		case "-s":
			if r.Direction == "in" {
				r.Sources = []string{strings.TrimSuffix(arg(), "/32")}
			} else {
				arg()
			}
		case "-d":
			if r.Direction == "out" {
				r.Sources = []string{strings.TrimSuffix(arg(), "/32")}
			} else {
				arg()
			}
		case "-p":
			r.Protocol = arg()
		case "--dport", "--dports":
			r.Ports = strings.ReplaceAll(arg(), ":", "-")
		case "--comment":
			r.Name = strings.Trim(arg(), `"`)
		case "-j":
			switch arg() {
			case "ACCEPT":
				r.Action = "allow"
			case "DROP":
				r.Action = "deny"
			case "REJECT":
				r.Action = "reject"
			default:
				r.Action = "other"
			}
		}
	}
	if r.Action == "" {
		r.Action = "other"
	}
	return r, false
}

// ---------------------------------------------------------------- nftables

// parseNftJSON reads `nft -j list ruleset`: base chains on the input and
// output hooks, their policies, and the rules in them that say plainly which
// protocol, ports and sources they accept or drop. A rule it cannot put in
// words is kept with action "other" and the chain it is in, so it is shown,
// not dropped.
func parseNftJSON(b []byte) (tunnel.HostFirewall, error) {
	fw := tunnel.HostFirewall{Kind: tunnel.FirewallNftables}
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return fw, err
	}
	type chain struct{ dir string }
	chains := map[string]chain{}
	key := func(fam, table, name string) string { return fam + " " + table + " " + name }
	for _, o := range doc.Nftables {
		raw, ok := o["chain"]
		if !ok {
			continue
		}
		var c struct {
			Family, Table, Name, Type, Hook, Policy string
		}
		if json.Unmarshal(raw, &c) != nil || c.Type != "filter" || (c.Hook != "input" && c.Hook != "output") {
			continue
		}
		if c.Family != "inet" && c.Family != "ip" && c.Family != "ip6" {
			continue
		}
		dir := "in"
		if c.Hook == "output" {
			dir = "out"
		}
		chains[key(c.Family, c.Table, c.Name)] = chain{dir}
		v := "allow"
		if c.Policy == "drop" {
			v = "deny"
		}
		if dir == "in" {
			if fw.DefaultIn != "deny" {
				fw.DefaultIn = v
			}
			if v == "deny" {
				fw.Active = true
			}
		} else if fw.DefaultOut != "deny" {
			fw.DefaultOut = v
		}
	}
	for _, o := range doc.Nftables {
		raw, ok := o["rule"]
		if !ok {
			continue
		}
		var rr struct {
			Family, Table, Chain, Comment string
			Handle                        int
			Expr                          []map[string]json.RawMessage
		}
		if json.Unmarshal(raw, &rr) != nil {
			continue
		}
		c, ok := chains[key(rr.Family, rr.Table, rr.Chain)]
		if !ok {
			continue
		}
		r, skip := nftRule(rr.Expr)
		if skip {
			continue
		}
		r.Direction = c.dir
		r.Name = rr.Comment
		r.Raw = fmt.Sprintf("%s %s chain %s, rule %d", rr.Family, rr.Table, rr.Chain, rr.Handle)
		r.IPv6 = rr.Family == "ip6"
		if c.dir == "in" {
			fw.Active = true
		}
		fw.Rules = append(fw.Rules, r)
	}
	return fw, nil
}

// nftRule reads one rule's expressions. skip is true for the loopback and
// established-connection rules every ruleset starts with.
func nftRule(expr []map[string]json.RawMessage) (r tunnel.FirewallRule, skip bool) {
	r = tunnel.FirewallRule{Protocol: "any"}
	plain := true
	for _, e := range expr {
		switch {
		case e["accept"] != nil:
			r.Action = "allow"
		case e["drop"] != nil:
			r.Action = "deny"
		case e["reject"] != nil:
			r.Action = "reject"
		case e["counter"] != nil, e["log"] != nil, e["limit"] != nil:
		case e["match"] != nil:
			var m struct {
				Op    string          `json:"op"`
				Left  json.RawMessage `json:"left"`
				Right json.RawMessage `json:"right"`
			}
			if json.Unmarshal(e["match"], &m) != nil {
				plain = false
				continue
			}
			var left struct {
				Payload *struct{ Protocol, Field string } `json:"payload"`
				Meta    *struct{ Key string }             `json:"meta"`
				Ct      *struct{ Key string }             `json:"ct"`
			}
			_ = json.Unmarshal(m.Left, &left)
			switch {
			case left.Meta != nil && (left.Meta.Key == "iif" || left.Meta.Key == "iifname" || left.Meta.Key == "oif" || left.Meta.Key == "oifname"):
				var s string
				if json.Unmarshal(m.Right, &s) == nil && s == "lo" {
					return r, true
				}
				plain = false
			case left.Ct != nil && left.Ct.Key == "state":
				if strings.Contains(string(m.Right), "established") {
					return r, true
				}
				plain = false
			case left.Meta != nil && left.Meta.Key == "l4proto":
				var s string
				if json.Unmarshal(m.Right, &s) == nil {
					r.Protocol = s
				} else {
					plain = false
				}
			case left.Payload != nil && (left.Payload.Field == "dport"):
				r.Protocol = left.Payload.Protocol
				if p := nftPorts(m.Right); p != "" && (m.Op == "==" || m.Op == "in" || m.Op == "") {
					r.Ports = p
				} else {
					plain = false
				}
			case left.Payload != nil && (left.Payload.Field == "saddr" || left.Payload.Field == "daddr"):
				if a := nftAddrs(m.Right); a != nil && (m.Op == "==" || m.Op == "in" || m.Op == "") {
					r.Sources = a
				} else {
					plain = false
				}
			default:
				plain = false
			}
		default:
			plain = false // jump, goto, nat, sets by name, ...
		}
	}
	if !plain || r.Action == "" {
		r.Action = "other"
	}
	return r, false
}

func nftPorts(raw json.RawMessage) string {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return strconv.Itoa(n)
	}
	var set struct {
		Set   []json.RawMessage `json:"set"`
		Range []int             `json:"range"`
	}
	if json.Unmarshal(raw, &set) != nil {
		return ""
	}
	if len(set.Range) == 2 {
		return fmt.Sprintf("%d-%d", set.Range[0], set.Range[1])
	}
	var parts []string
	for _, e := range set.Set {
		p := nftPorts(e)
		if p == "" {
			return ""
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ",")
}

func nftAddrs(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.HasPrefix(s, "@") || !isAddr(s) {
			return nil
		}
		return []string{s}
	}
	var o struct {
		Prefix *struct {
			Addr string `json:"addr"`
			Len  int    `json:"len"`
		} `json:"prefix"`
		Set []json.RawMessage `json:"set"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return nil
	}
	if o.Prefix != nil {
		return []string{fmt.Sprintf("%s/%d", o.Prefix.Addr, o.Prefix.Len)}
	}
	var out []string
	for _, e := range o.Set {
		a := nftAddrs(e)
		if a == nil {
			return nil
		}
		out = append(out, a...)
	}
	return out
}

// ---------------------------------------------------------------- Windows

// winFirewallScript prints Windows Firewall's profiles, the network types in
// use, and every rule that is switched on with its ports and addresses, as
// JSON. Filters are read once for all rules and joined by InstanceID: one
// Get-NetFirewallPortFilter per rule takes minutes on a server with hundreds.
const winFirewallScript = `$ErrorActionPreference='Stop'
$prof = @(Get-NetFirewallProfile | ForEach-Object { [pscustomobject]@{ name=$_.Name; enabled=$_.Enabled.ToString(); in=$_.DefaultInboundAction.ToString(); out=$_.DefaultOutboundAction.ToString() } })
$cat = @(Get-NetConnectionProfile -ErrorAction SilentlyContinue | ForEach-Object { $_.NetworkCategory.ToString() })
$ports = @{}; Get-NetFirewallPortFilter -All | ForEach-Object { $ports[$_.InstanceID] = $_ }
$addrs = @{}; Get-NetFirewallAddressFilter -All | ForEach-Object { $addrs[$_.InstanceID] = $_ }
$all = @(Get-NetFirewallRule)
$on = @($all | Where-Object { $_.Enabled.ToString() -eq 'True' })
$rules = @(foreach ($r in $on) { $p = $ports[$r.InstanceID]; $a = $addrs[$r.InstanceID]
  [pscustomobject]@{ name=[string]$r.DisplayName; group=[string]$r.Group; dir=$r.Direction.ToString(); action=$r.Action.ToString(); profile=$r.Profile.ToString();
    proto=[string]$p.Protocol; port=(@($p.LocalPort) -join ','); remote=@($a.RemoteAddress | ForEach-Object { [string]$_ }) } })
[pscustomobject]@{ profiles=$prof; categories=$cat; rules=$rules; off=($all.Count - $on.Count) } | ConvertTo-Json -Depth 4 -Compress`

// winGroup is the Windows Firewall rule group Argus's own rules go in.
const winGroup = "Argus"

func parseWinFirewall(b []byte) (tunnel.HostFirewall, error) {
	fw := tunnel.HostFirewall{Kind: tunnel.FirewallWindows, Installed: []string{tunnel.FirewallWindows},
		Rules: []tunnel.FirewallRule{}, ReadAt: time.Now().UTC(), Manageable: true}
	var doc struct {
		Profiles []struct {
			Name, Enabled, In, Out string
		} `json:"profiles"`
		Categories []string `json:"categories"`
		Rules      []struct {
			Name, Group, Dir, Action, Profile, Proto, Port string
			Remote                                         []string
		} `json:"rules"`
		Off int `json:"off"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return fw, err
	}
	// "DomainAuthenticated" is the Domain profile.
	inUse := map[string]bool{}
	for _, c := range doc.Categories {
		c = strings.TrimSuffix(c, "Authenticated")
		inUse[c] = true
	}
	winDefault := func(v, dflt string) string {
		switch v {
		case "Allow":
			return "allow"
		case "Block":
			return "deny"
		}
		return dflt // NotConfigured: Windows blocks in and allows out
	}
	for _, p := range doc.Profiles {
		pr := tunnel.FirewallProfile{Name: p.Name, Enabled: p.Enabled == "True", InUse: inUse[p.Name],
			DefaultIn: winDefault(p.In, "deny"), DefaultOut: winDefault(p.Out, "allow")}
		fw.Profiles = append(fw.Profiles, pr)
		// The report's own state is the profile in use (or Public, which
		// Windows falls back to, when nothing says).
		if pr.InUse || (fw.DefaultIn == "" && p.Name == "Public" && len(inUse) == 0) {
			fw.Active = fw.Active || pr.Enabled
			if fw.DefaultIn == "" || pr.InUse {
				fw.DefaultIn, fw.DefaultOut = pr.DefaultIn, pr.DefaultOut
			}
		}
	}
	for _, r := range doc.Rules {
		rule := tunnel.FirewallRule{Direction: "in", Action: "allow", Protocol: "any", Name: r.Name, Profile: r.Profile,
			Raw: fmt.Sprintf("%s · %s · %s · profiles %s", r.Name, r.Dir, r.Action, r.Profile)}
		if r.Dir == "Outbound" {
			rule.Direction = "out"
		}
		if r.Action == "Block" {
			rule.Action = "deny"
		}
		switch strings.ToUpper(r.Proto) {
		case "TCP", "6":
			rule.Protocol = "tcp"
		case "UDP", "17":
			rule.Protocol = "udp"
		case "ICMPV4", "ICMPV6", "1", "58":
			rule.Protocol = "icmp"
		}
		if p := r.Port; p != "" && p != "Any" {
			if _, err := strconv.Atoi(strings.NewReplacer(",", "", "-", "").Replace(p)); err == nil {
				rule.Ports = p
			} else {
				rule.Service = p // RPC, RPCEPMap, IPHTTPS...: dynamic ports
			}
		}
		for _, a := range r.Remote {
			if a == "" || a == "Any" || a == "*" {
				rule.Sources = nil
				break
			}
			rule.Sources = append(rule.Sources, winAddr(a))
		}
		rule.Argus = r.Group == winGroup
		fw.Rules = append(fw.Rules, rule)
	}
	fw.Hidden = doc.Off
	return fw, nil
}

// winAddr turns Windows's "10.0.0.0/255.0.0.0" into "10.0.0.0/8"; keywords
// such as LocalSubnet are kept as they are.
func winAddr(a string) string {
	addr, mask, ok := strings.Cut(a, "/")
	if !ok {
		return a
	}
	m, err := netip.ParseAddr(mask)
	if err != nil || !m.Is4() {
		return a
	}
	bits := 0
	for _, by := range m.As4() {
		for i := 7; i >= 0; i-- {
			if by&(1<<i) != 0 {
				bits++
			}
		}
	}
	return fmt.Sprintf("%s/%d", addr, bits)
}

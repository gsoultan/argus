package tunnel

import (
	"net/netip"
	"strconv"
	"strings"
	"time"
)

/*
The whole firewall on a server, as its agent reads it: which firewall runs,
whether it filters, what it does with traffic no rule names, and every rule.

It rides the agent's facts report every ten minutes (cmd/argus-agent) and is
what the server page's Firewall tab and Assets › Firewalls show. Nothing here
changes a firewall: the per-port change for a database is firewall.go, and
changes from the Firewall tab come later (plans/server-firewall/design.md).

A rule Argus added carries its mark: ufw's comment "argus <port>", firewalld's
rules listed in the agent's own state file. Every other rule is reported as
made outside Argus, with the firewall's own text kept in Raw so the console
can show exactly what is there.
*/

// The firewalls the agent reads.
const (
	FirewallUFW       = "ufw"
	FirewallFirewalld = "firewalld"
	FirewallNftables  = "nftables"
	FirewallIptables  = "iptables"
	FirewallWindows   = "windows"
)

// Bounds on one report, applied by the agent and again by the control plane.
const (
	MaxHostFirewallRules = 500
	maxFirewallText      = 300
)

// HostFirewall is a server's firewall as its agent found it.
type HostFirewall struct {
	// Kind is the firewall in charge: ufw, firewalld, nftables, iptables or
	// windows; "" when none is installed.
	Kind string `json:"kind"`
	// Installed lists the firewalls whose tools are on the server, so the
	// console can say "firewalld is installed but not running".
	Installed []string `json:"installed,omitempty"`
	// Active says the firewall filters traffic now.
	Active bool `json:"active"`
	// DefaultIn and DefaultOut are what happens to traffic no rule names:
	// allow, deny (dropped) or reject; "" when it could not be read.
	DefaultIn  string `json:"defaultIn,omitempty"`
	DefaultOut string `json:"defaultOut,omitempty"`
	// Zones are firewalld's active zones, by name.
	Zones []string `json:"zones,omitempty"`
	// Profiles are Windows Firewall's three network types.
	Profiles []FirewallProfile `json:"profiles,omitempty"`
	// Rules are the rules that are switched on, in the firewall's order.
	Rules []FirewallRule `json:"rules"`
	// Hidden counts rules left out: Windows's built-in rules that are off,
	// and anything past MaxHostFirewallRules.
	Hidden int `json:"hidden,omitempty"`
	// Docker says Docker's own chains are present: ports Docker publishes
	// pass around ufw and firewalld.
	Docker bool `json:"docker,omitempty"`
	// Manageable says Argus can change this firewall (ufw, firewalld,
	// nftables and Windows Firewall); iptables written by hand is read only.
	Manageable bool      `json:"manageable"`
	ReadAt     time.Time `json:"readAt"`
	// Note is a plain sentence on anything the reader could not do.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// FirewallProfile is one of Windows Firewall's network types.
type FirewallProfile struct {
	Name       string `json:"name"` // Domain, Private, Public
	Enabled    bool   `json:"enabled"`
	InUse      bool   `json:"inUse,omitempty"`
	DefaultIn  string `json:"defaultIn,omitempty"`
	DefaultOut string `json:"defaultOut,omitempty"`
}

// FirewallRule is one rule, in words the console can show.
type FirewallRule struct {
	// Direction is "in" or "out".
	Direction string `json:"direction"`
	// Action is allow, deny, reject or limit.
	Action string `json:"action"`
	// Protocol is tcp, udp, icmp or "any".
	Protocol string `json:"protocol"`
	// Ports is "22", "80,443", "6000-7000", or "" for every port.
	Ports string `json:"ports,omitempty"`
	// Service names the rule when the firewall does (firewalld "ssh").
	Service string `json:"service,omitempty"`
	// Sources are where the traffic may come from (for "in"), addresses or
	// networks; empty is anywhere.
	Sources []string `json:"sources,omitempty"`
	// Name is the rule's own name or comment.
	Name string `json:"name,omitempty"`
	// Argus says Argus added it; ArgusPort is the database port it is for.
	Argus     bool `json:"argus,omitempty"`
	ArgusPort int  `json:"argusPort,omitempty"`
	// Profile is the Windows network types the rule is for ("Any",
	// "Domain, Private"); "" elsewhere.
	Profile string `json:"profile,omitempty"`
	// IPv6 says the rule is for IPv6 only (ufw lists those twice).
	IPv6 bool `json:"ipv6,omitempty"`
	// Raw is the rule as the firewall prints it.
	Raw string `json:"raw,omitempty"`
}

// Bounded trims a report to what the console needs and the database keeps.
func (f HostFirewall) Bounded() HostFirewall {
	cut := func(s string) string {
		if len(s) > maxFirewallText {
			return s[:maxFirewallText]
		}
		return s
	}
	if len(f.Rules) > MaxHostFirewallRules {
		f.Hidden += len(f.Rules) - MaxHostFirewallRules
		f.Rules = f.Rules[:MaxHostFirewallRules]
	}
	rules := make([]FirewallRule, 0, len(f.Rules))
	for _, r := range f.Rules {
		r.Ports, r.Service, r.Name, r.Raw = cut(r.Ports), cut(r.Service), cut(r.Name), cut(r.Raw)
		if len(r.Sources) > 64 {
			r.Sources = r.Sources[:64]
		}
		for i := range r.Sources {
			r.Sources[i] = cut(r.Sources[i])
		}
		rules = append(rules, r)
	}
	f.Rules = rules
	if len(f.Installed) > 8 {
		f.Installed = f.Installed[:8]
	}
	if len(f.Zones) > 16 {
		f.Zones = f.Zones[:16]
	}
	if len(f.Profiles) > 3 {
		f.Profiles = f.Profiles[:3]
	}
	f.Note, f.Error = cut(f.Note), cut(f.Error)
	return f
}

// ArgusCommentPort reads the database port from a ufw comment Argus wrote
// ("argus 6379"); 0 when the comment is not one of Argus's.
func ArgusCommentPort(comment string) int {
	rest, ok := strings.CutPrefix(strings.TrimSpace(comment), "argus ")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

// PortsCover says whether a rule's ports ("" for all, "22", "80,443",
// "6000-7000", "6000:7000") include port.
func PortsCover(ports string, port int) bool {
	ports = strings.TrimSpace(ports)
	if ports == "" {
		return true
	}
	for _, part := range strings.Split(ports, ",") {
		part = strings.TrimSpace(part)
		lo, hi, rng := strings.Cut(part, "-")
		if !rng {
			lo, hi, rng = strings.Cut(part, ":")
		}
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			continue
		}
		b := a
		if rng {
			if v, err := strconv.Atoi(strings.TrimSpace(hi)); err == nil {
				b = v
			}
		}
		if port >= a && port <= b {
			return true
		}
	}
	return false
}

// PortReach is who a firewall lets reach one TCP port coming in.
type PortReach struct {
	// Everyone: the firewall is off, lets everything in by default, or a
	// rule lets any address in.
	Everyone bool `json:"everyone"`
	// Public: one of the allowed sources is a public address or network,
	// so the internet can reach it (Everyone implies it).
	Public bool `json:"public"`
	// Sources are the addresses and networks allowed, when not everyone.
	Sources []string `json:"sources,omitempty"`
	// Unknown: the firewall could not be read, or is one whose rules Argus
	// cannot put in words for this port.
	Unknown bool `json:"unknown,omitempty"`
}

// ReachOf works out who f lets reach TCP port coming in. Rules are taken in
// order and a deny before an allow wins for the sources it names, the way
// ufw, iptables and nftables read them; firewalld and Windows have no order
// that matters for allows, so the same reading is right for them too.
func (f HostFirewall) ReachOf(port int) PortReach {
	if f.Error != "" || (f.Kind == "" && len(f.Installed) == 0 && f.ReadAt.IsZero()) {
		return PortReach{Unknown: true}
	}
	if !f.Active || f.Kind == "" {
		return PortReach{Everyone: true, Public: true}
	}
	inUse := map[string]bool{}
	for _, p := range f.Profiles {
		if p.InUse {
			inUse[p.Name] = true
		}
	}
	var srcs []string
	denied := map[string]bool{}
	deniedAll := false
	for _, r := range f.Rules {
		if r.Direction != "in" || (r.Protocol != "tcp" && r.Protocol != "any") || !PortsCover(r.Ports, port) {
			continue
		}
		if r.Service != "" && r.Ports == "" {
			continue // a named service whose ports were not read
		}
		if !profileApplies(r.Profile, inUse) {
			continue
		}
		switch r.Action {
		case "allow", "limit":
			if deniedAll {
				continue
			}
			if len(r.Sources) == 0 {
				return PortReach{Everyone: true, Public: true}
			}
			for _, s := range r.Sources {
				if !denied[s] && !contains(srcs, s) {
					srcs = append(srcs, s)
				}
			}
		case "deny", "reject":
			if len(r.Sources) == 0 {
				deniedAll = true
			}
			for _, s := range r.Sources {
				denied[s] = true
			}
		}
	}
	if !deniedAll && f.DefaultIn == "allow" {
		return PortReach{Everyone: true, Public: true}
	}
	out := PortReach{Sources: srcs}
	for _, s := range srcs {
		if isPublic(s) {
			out.Public = true
		}
	}
	return out
}

// profileApplies says a Windows rule is for a network type in use; a rule
// with no profile (every other firewall), "Any", or a server whose type in
// use is not known, applies.
func profileApplies(profile string, inUse map[string]bool) bool {
	if profile == "" || profile == "Any" || len(inUse) == 0 {
		return true
	}
	for _, p := range strings.Split(profile, ",") {
		if inUse[strings.TrimSpace(p)] {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// isPublic says a source is, or includes, an address on the internet.
func isPublic(s string) bool {
	var p netip.Prefix
	if q, err := netip.ParsePrefix(s); err == nil {
		p = q
	} else if a, err := netip.ParseAddr(s); err == nil {
		p = netip.PrefixFrom(a, a.BitLen())
	} else {
		return false // a name or a set Argus cannot read: not claimed either way
	}
	a := p.Addr().Unmap()
	if p.Bits() < 8 {
		return true
	}
	return !(a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || isCGNAT(a))
}

func isCGNAT(a netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// ManageableFirewall says Argus can change a firewall of this kind.
func ManageableFirewall(kind string) bool {
	switch kind {
	case FirewallUFW, FirewallFirewalld, FirewallNftables, FirewallWindows:
		return true
	}
	return false
}

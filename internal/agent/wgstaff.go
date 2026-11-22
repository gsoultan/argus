package agent

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

/*
The staff VPN's hub: a WireGuard network whose peers are people's devices,
not servers. On top of the network, the hub filters what each device may
reach (its people's teams' ranges, with SSH and RDP refused unless a team
allows direct logins) and sends the traffic on as itself (masquerade), so
the servers behind it need no route back. A device on full traffic may also
reach the internet through the hub.

The rules are one nftables table, inet argus_<iface>, made by the
interface's PostUp and removed by its PostDown. So the table is always the
interface's: it comes and goes with it, and the helper's trial and rollback
of the file covers it too.
*/

// WGStaff is a staff hub's filtering.
type WGStaff struct {
	// Rules is what each device may reach.
	Rules []WGStaffRule `json:"rules"`
	// Internet is the devices on full traffic, which may reach any public
	// address through the hub.
	Internet []string `json:"internet,omitempty"`
}

// WGStaffRule is one device's reach.
type WGStaffRule struct {
	// From is the device's address: 10.77.0.5/32.
	From string `json:"from"`
	// To is the private ranges it may reach.
	To []string `json:"to"`
	// Logins allows SSH (22) and RDP (3389) to them. Off, those ports are
	// refused, so logins go through Argus, which records them.
	Logins bool `json:"logins,omitempty"`
}

const wgStaffMaxRules = 1000

func hostIn(network netip.Prefix, s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p.Bits() != 32 || !network.Contains(p.Addr()) {
		return netip.Prefix{}, fmt.Errorf("device address %q is not one address in %s", s, network)
	}
	return p, nil
}

func validWGStaff(st *WGStaff, network netip.Prefix) error {
	if st == nil {
		return nil
	}
	if len(st.Rules) > wgStaffMaxRules || len(st.Internet) > wgStaffMaxRules {
		return errors.New("too many staff rules")
	}
	for _, r := range st.Rules {
		if _, err := hostIn(network, r.From); err != nil {
			return err
		}
		if len(r.To) > 64 {
			return errors.New("a device may reach at most 64 ranges")
		}
		for _, t := range r.To {
			p, err := netip.ParsePrefix(t)
			if err != nil || !p.Addr().Is4() || p != p.Masked() || !wgPrivateRange(p) {
				return fmt.Errorf("staff range %q is not a private IPv4 range", t)
			}
		}
	}
	for _, a := range st.Internet {
		if _, err := hostIn(network, a); err != nil {
			return err
		}
	}
	return nil
}

func nftSet(xs []string) string {
	s := append([]string(nil), xs...)
	sort.Strings(s)
	return "{ " + strings.Join(s, ", ") + " }"
}

// wgStaffNft is the nft script for a hub, on one line, for PostUp. Every
// part was checked as an address or range, so nothing in it is free text.
func wgStaffNft(iface string, network netip.Prefix, st *WGStaff) string {
	t := "inet argus_" + iface
	in := `iifname "` + iface + `"`
	var cmds []string
	add := func(f string, a ...any) { cmds = append(cmds, fmt.Sprintf(f, a...)) }
	// Made afresh: an old table of the same name goes first.
	add("add table %s", t)
	add("delete table %s", t)
	add("add table %s", t)
	add("add chain %s fwd { type filter hook forward priority -5 ; policy accept ; }", t)
	add("add chain %s inbound { type filter hook input priority -5 ; policy accept ; }", t)
	add("add chain %s post { type nat hook postrouting priority 100 ; policy accept ; }", t)
	add("add rule %s fwd %s ct state established,related accept", t, in)
	rules := append([]WGStaffRule(nil), st.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].From < rules[j].From })
	for _, r := range rules {
		if len(r.To) == 0 {
			continue
		}
		from := strings.TrimSuffix(r.From, "/32")
		if !r.Logins {
			add("add rule %s fwd %s ip saddr %s ip daddr %s tcp dport { 22, 3389 } reject with tcp reset", t, in, from, nftSet(r.To))
		}
		add("add rule %s fwd %s ip saddr %s ip daddr %s accept", t, in, from, nftSet(r.To))
	}
	if len(st.Internet) > 0 {
		var hosts []string
		for _, a := range st.Internet {
			hosts = append(hosts, strings.TrimSuffix(a, "/32"))
		}
		var private []string
		for _, p := range wgPrivateRanges {
			private = append(private, p.String())
		}
		add("add rule %s fwd %s ip saddr %s ip daddr != %s accept", t, in, nftSet(hosts), nftSet(private))
	}
	add("add rule %s fwd %s reject", t, in)
	// The hub itself: replies and ping only.
	add("add rule %s inbound %s ct state established,related accept", t, in)
	add("add rule %s inbound %s icmp type echo-request accept", t, in)
	add("add rule %s inbound %s drop", t, in)
	add(`add rule %s post ip saddr %s oifname != "%s" masquerade`, t, network, iface)
	return strings.Join(cmds, " ; ")
}

// wgStaffHooks are the interface's PostUp and PostDown lines for a hub.
func wgStaffHooks(iface string, network netip.Prefix, st *WGStaff) (up, down []string) {
	up = append(up, "nft '"+wgStaffNft(iface, network, st)+"'")
	// ufw drops forwarded traffic by default; the table above decides.
	up = append(up, "if ufw status 2>/dev/null | grep -q 'Status: active'; then ufw route allow in on "+iface+" >/dev/null; fi")
	down = append(down, "nft delete table inet argus_"+iface+" 2>/dev/null || true")
	down = append(down, "if ufw status 2>/dev/null | grep -q 'Status: active'; then ufw route delete allow in on "+iface+" >/dev/null; fi")
	return up, down
}

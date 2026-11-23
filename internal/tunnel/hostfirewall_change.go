package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

/*
Changing a server's own firewall from the Firewall tab and from firewall
policies (plans/server-firewall/design.md).

The control plane works out the whole set of lines Argus keeps on a server:
the server's own lines, every line of the policies on it, and, for lines
that name servers in Argus or servers with a policy, those servers' current
addresses. It sends that set; the agent makes the firewall hold exactly it.
Lines Argus keeps carry the mark "argus fw-<key>" (a ufw comment, an
nftables comment, a Windows rule's display name, or, for firewalld, the
agent's own list), so the agent only ever adds or removes its own, plus the
rules made by hand that someone chose to take over.

Every change is a trial: the agent saves the firewall first, makes the
change, and puts the saved copy back by itself unless a heartbeat reaches
the control plane within two minutes. Turning a firewall on waits, on top
of that, for a person to press Keep within ten minutes.

The commands for each firewall are built here, not in the agent, so the
review screen shows exactly what the agent will run.
*/

// FirewallLine is one line Argus keeps on a server: let in (or keep out) one
// kind of traffic, from (or, going out, to) some addresses.
type FirewallLine struct {
	// Key names the line; its mark on the server is "argus fw-<key>".
	Key string `json:"key"`
	// Direction is "in" or "out".
	Direction string `json:"direction"`
	// Action is "allow" or "deny".
	Action string `json:"action"`
	// Protocol is tcp, udp, icmp or any. Ports need tcp or udp.
	Protocol string `json:"protocol"`
	// Ports is "22", "80,443" or "6000-6010"; "" is every port.
	Ports string `json:"ports,omitempty"`
	// Sources are the addresses and networks on the other side; empty is
	// everyone.
	Sources []string `json:"sources,omitempty"`
}

// FirewallChangeSpec is one change, as the agent receives it.
type FirewallChangeSpec struct {
	ID string `json:"id"`
	// Lines is the whole set Argus keeps after the change. An Argus line
	// not in it is removed.
	Lines []FirewallLine `json:"lines"`
	// TakeOver are rules made by hand, by their Raw text as the agent last
	// reported them, to remove: lines in Lines replace them.
	TakeOver []string `json:"takeOver,omitempty"`
	// DefaultIn and DefaultOut set what happens to traffic no line names:
	// "allow" or "deny"; "" leaves it as it is.
	DefaultIn  string `json:"defaultIn,omitempty"`
	DefaultOut string `json:"defaultOut,omitempty"`
	// TurnOn switches the firewall on; Install names the one to install
	// first (ufw or firewalld) when none is.
	TurnOn  bool   `json:"turnOn,omitempty"`
	Install string `json:"install,omitempty"`
	// PutBack restores the copy the agent saved before change PutBack,
	// instead of anything above.
	PutBack string `json:"putBack,omitempty"`
	// ExpectHash is the firewall's Hash when the change was planned; the
	// agent refuses when the firewall changed since. "" skips the check.
	ExpectHash string `json:"expectHash,omitempty"`
	// Keep says the change waits for a person to press Keep, on top of a
	// heartbeat getting through.
	Keep bool `json:"keep,omitempty"`
	// Protect are ports coming in that must stay open to everyone: the
	// control plane's own HTTPS and QUIC ports on the server it runs on.
	Protect []FirewallLine `json:"protect,omitempty"`
	// TrialSeconds and KeepSeconds bound the waits; 0 is the default.
	TrialSeconds int `json:"trialSeconds,omitempty"`
	KeepSeconds  int `json:"keepSeconds,omitempty"`
}

// Firewall run states, as the agent reports them.
const (
	FirewallRunApplying   = "applying"
	FirewallRunTrial      = "trial"
	FirewallRunWaitKeep   = "waiting_keep"
	FirewallRunApplied    = "applied"
	FirewallRunFailed     = "failed"
	FirewallRunRolledBack = "rolled_back"
)

// FirewallRun is how a change went on the server.
type FirewallRun struct {
	ID      string    `json:"id"`
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
	// Ran are the commands, in order, with how each ended.
	Ran []FirewallRan `json:"ran,omitempty"`
	// After is the firewall read back once the run ended.
	After *HostFirewall `json:"after,omitempty"`
	// Unit is the systemd unit the helper runs in, while it runs.
	Unit string `json:"unit,omitempty"`
}

// FirewallRan is one command a change ran.
type FirewallRan struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Output  string `json:"output,omitempty"`
}

// Finished says the agent is done with the run.
func (r FirewallRun) Finished() bool {
	switch r.State {
	case FirewallRunApplied, FirewallRunFailed, FirewallRunRolledBack:
		return true
	}
	return false
}

// FirewallBeat rides the heartbeat.
type FirewallBeat struct {
	// Supported says this agent changes its firewall; Reason says why not.
	Supported bool         `json:"supported"`
	Reason    string       `json:"reason,omitempty"`
	Run       *FirewallRun `json:"run,omitempty"`
}

// FirewallOrders rides the heartbeat's reply.
type FirewallOrders struct {
	// Change is the change to make; one the agent already ran is ignored.
	Change *FirewallChangeSpec `json:"change,omitempty"`
	// Keep is a change waiting for Keep that a person kept.
	Keep string `json:"keep,omitempty"`
	// Read asks for the firewall to be read now; a new value each time.
	Read string `json:"read,omitempty"`
}

// Bounds on a change.
const (
	MaxFirewallLines   = 200
	FirewallTrial      = 2 * time.Minute
	FirewallKeepWindow = 10 * time.Minute
)

var (
	reFirewallKey  = regexp.MustCompile(`^[a-z0-9]{1,24}$`)
	reFirewallRun  = regexp.MustCompile(`^[0-9a-f-]{8,40}$`)
	reFirewallPort = regexp.MustCompile(`^[0-9]{1,5}(-[0-9]{1,5})?(,[0-9]{1,5}(-[0-9]{1,5})?)*$`)
)

// FirewallLineMark is the mark a line carries on the server.
func FirewallLineMark(key string) string { return "argus fw-" + key }

// ArgusLineKey reads a line's key from its mark; "" when it is not one.
func ArgusLineKey(mark string) string {
	k, ok := strings.CutPrefix(strings.TrimSpace(mark), "argus fw-")
	if !ok || !reFirewallKey.MatchString(k) {
		return ""
	}
	return k
}

// ValidFirewallRunID says whether id can name a change on the server.
func ValidFirewallRunID(id string) bool { return reFirewallRun.MatchString(id) }

// Validate refuses a line the agent would not write.
func (l *FirewallLine) Validate() error {
	if !reFirewallKey.MatchString(l.Key) {
		return fmt.Errorf("%q is not a line's key", l.Key)
	}
	if l.Direction != "in" && l.Direction != "out" {
		return errors.New("a line goes in or out")
	}
	if l.Action != "allow" && l.Action != "deny" {
		return errors.New("a line allows or blocks")
	}
	switch l.Protocol {
	case "tcp", "udp":
	case "any", "icmp":
		if l.Ports != "" {
			return errors.New("ports need TCP or UDP")
		}
	default:
		return fmt.Errorf("%q is not tcp, udp, icmp or any", l.Protocol)
	}
	if l.Ports != "" {
		if !reFirewallPort.MatchString(l.Ports) {
			return fmt.Errorf("%q is not a port, a list like 80,443 or a range like 6000-6010", l.Ports)
		}
		for _, part := range strings.Split(l.Ports, ",") {
			lo, hi, rng := strings.Cut(part, "-")
			a, _ := strconv.Atoi(lo)
			b := a
			if rng {
				b, _ = strconv.Atoi(hi)
			}
			if a < 1 || b > 65535 || b < a {
				return fmt.Errorf("%q is not between 1 and 65535", part)
			}
		}
		if strings.Count(l.Ports, ",") >= 15 {
			return errors.New("at most 15 ports or ranges in one line")
		}
	}
	if len(l.Sources) > 256 {
		return errors.New("at most 256 addresses in one line")
	}
	out := make([]string, 0, len(l.Sources))
	for _, s := range l.Sources {
		p, err := ParseSource(s)
		if err != nil {
			return err
		}
		t := SourceText(p)
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	l.Sources = out
	return nil
}

// Validate refuses a change the agent would not make.
func (c *FirewallChangeSpec) Validate() error {
	if !ValidFirewallRunID(c.ID) {
		return errors.New("the change has no id")
	}
	if c.PutBack != "" {
		if !ValidFirewallRunID(c.PutBack) {
			return errors.New("the copy to put back has no id")
		}
		return nil
	}
	if len(c.Lines)+len(c.Protect) > MaxFirewallLines {
		return fmt.Errorf("at most %d lines", MaxFirewallLines)
	}
	seen := map[string]bool{}
	for i := range c.Lines {
		if err := c.Lines[i].Validate(); err != nil {
			return err
		}
		if seen[c.Lines[i].Key] {
			return fmt.Errorf("two lines are both %q", c.Lines[i].Key)
		}
		seen[c.Lines[i].Key] = true
	}
	for i := range c.Protect {
		if err := c.Protect[i].Validate(); err != nil {
			return err
		}
	}
	for _, d := range []string{c.DefaultIn, c.DefaultOut} {
		if d != "" && d != "allow" && d != "deny" {
			return fmt.Errorf("%q is not allow or deny", d)
		}
	}
	if c.Install != "" && c.Install != FirewallUFW && c.Install != FirewallFirewalld {
		return fmt.Errorf("Argus installs ufw or firewalld, not %q", c.Install)
	}
	if len(c.TakeOver) > MaxHostFirewallRules {
		return errors.New("too many rules to take over")
	}
	return nil
}

// Hash names the state of a firewall, so a change planned against one read
// is refused when the server's firewall changed since.
func (f HostFirewall) Hash() string {
	type rule struct {
		D, A, P, Ports, S string
		Src               []string
		R                 string
	}
	v := struct {
		K, In, Out string
		On         bool
		Rules      []rule
	}{K: f.Kind, In: f.DefaultIn, Out: f.DefaultOut, On: f.Active}
	for _, r := range f.Rules {
		v.Rules = append(v.Rules, rule{r.Direction, r.Action, r.Protocol, r.Ports, r.Service, r.Sources, r.Raw})
	}
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// ArgusLines are the lines Argus keeps that the read found, by key, put
// back together from the rules (one rule per address on most firewalls).
func (f HostFirewall) ArgusLines() map[string]FirewallLine {
	out := map[string]FirewallLine{}
	for _, r := range f.Rules {
		k := ArgusLineKey(r.Name)
		if k == "" {
			continue
		}
		l, ok := out[k]
		if !ok {
			act := r.Action
			if act == "reject" {
				act = "deny"
			}
			l = FirewallLine{Key: k, Direction: r.Direction, Action: act, Protocol: r.Protocol}
		}
		if r.Ports != "" {
			l.Ports = normalPorts(l.Ports + "," + r.Ports)
		}
		for _, s := range r.Sources {
			if !slices.Contains(l.Sources, s) {
				l.Sources = append(l.Sources, s)
			}
		}
		slices.Sort(l.Sources)
		out[k] = l
	}
	return out
}

// normalPorts writes ports one way: dashes for ranges, each once, in order.
func normalPorts(p string) string {
	var parts []string
	for _, part := range strings.Split(strings.ReplaceAll(p, ":", "-"), ",") {
		if part = strings.TrimSpace(part); part != "" && !slices.Contains(parts, part) {
			parts = append(parts, part)
		}
	}
	slices.SortFunc(parts, func(a, b string) int {
		x, _ := strconv.Atoi(strings.SplitN(a, "-", 2)[0])
		y, _ := strconv.Atoi(strings.SplitN(b, "-", 2)[0])
		return x - y
	})
	return strings.Join(parts, ",")
}

// SameLine says two lines let the same traffic through.
func SameLine(a, b FirewallLine) bool {
	sa, sb := slices.Clone(a.Sources), slices.Clone(b.Sources)
	slices.Sort(sa)
	slices.Sort(sb)
	return a.Direction == b.Direction && a.Action == b.Action && a.Protocol == b.Protocol &&
		normalPorts(a.Ports) == normalPorts(b.Ports) && slices.Equal(sa, sb)
}

// lineSources is a line's addresses one at a time, "" for everyone: ufw
// and firewalld want one rule per address.
func lineSources(l FirewallLine) []string {
	if len(l.Sources) == 0 {
		return []string{""}
	}
	return l.Sources
}

func isV6(s string) bool {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		a, err := netip.ParseAddr(s)
		return err == nil && a.Is6() && !a.Is4In6()
	}
	return p.Addr().Is6()
}

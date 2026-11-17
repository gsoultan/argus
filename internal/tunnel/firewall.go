package tunnel

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

/*
Who may connect to a port on a server, enforced by the server's own
firewall.

A database's own listen setting (Redis's bind, Postgres's listen_addresses)
only chooses which of the server's addresses it accepts connections on; it
cannot name the machines that connect. So "selected machines" is the listen
setting plus a firewall rule, and the agent writes both.

The agent changes ufw or firewalld, and only rules it added itself: a ufw
rule carries the comment "argus <port>", a firewalld rule is kept in the
agent's own list. It never turns a firewall on (with ufw that applies
default-deny and can cut the SSH people sign in with) and never removes a
rule it did not add; a rule like that which already lets everyone in is
reported, so the console can say "selected machines" is not what happens.

The agent itself is untouched by these rules: it opens no port and only
dials out to the control plane, and it reaches a database on this server
over the loopback.
*/

// Reach is who may connect: the four answers the console offers.
const (
	ReachLocal    = "local"    // this server only
	ReachSelected = "selected" // the listed addresses and ranges
	ReachNetwork  = "network"  // the private networks the server is on
	ReachAny      = "any"      // every network, the internet included
)

// MaxFirewallSources bounds the addresses one change lists.
const MaxFirewallSources = 32

// FirewallRequest is one question to, or change of, the server's firewall,
// for one TCP port.
type FirewallRequest struct {
	Port int `json:"port"`
	// Reach is what the rules should allow; with Check, nothing changes and
	// the answer is what the firewall holds now.
	Reach   string   `json:"reach,omitempty"`
	Sources []string `json:"sources,omitempty"`
	Check   bool     `json:"check,omitempty"`
	// StateFile is where the agent keeps the firewalld rules it added.
	StateFile string `json:"stateFile,omitempty"`
}

// FirewallResult is the firewall as the agent found it, and what it ran.
type FirewallResult struct {
	// Firewall is "ufw", "firewalld", or "" when neither is installed.
	Firewall string `json:"firewall"`
	// Active says it filters traffic now. An inactive firewall is left off.
	Active bool `json:"active"`
	// Ran are the commands a change ran, in order.
	Ran []string `json:"ran,omitempty"`
	// Mine are the rules Argus added for this port, as the firewall lists them.
	Mine []string `json:"mine,omitempty"`
	// Sources are the machines Argus's rules let in; AllowAll says an
	// Argus rule lets everyone in.
	Sources  []string `json:"sources,omitempty"`
	AllowAll bool     `json:"allowAll,omitempty"`
	// Others are rules Argus did not add that mention this port.
	Others []string `json:"others,omitempty"`
	// OpenToAll says a rule Argus did not add already lets everyone reach
	// the port, so narrowing it with Argus's rules changes nothing.
	OpenToAll bool   `json:"openToAll,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ValidReach says whether r is one of the four answers.
func ValidReach(r string) bool {
	switch r {
	case ReachLocal, ReachSelected, ReachNetwork, ReachAny:
		return true
	}
	return false
}

// ParseSource reads one source: an IP address or a network in CIDR form,
// returned as a prefix. 0.0.0.0/0 and ::/0 are refused: that is "anywhere",
// which is asked for as such.
func ParseSource(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	var p netip.Prefix
	if strings.Contains(s, "/") {
		q, err := netip.ParsePrefix(s)
		if err != nil {
			return p, fmt.Errorf("%q is not an address or a network like 10.0.4.0/24", s)
		}
		p = q.Masked()
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" {
			return p, fmt.Errorf("%q is not an address or a network like 10.0.4.0/24", s)
		}
		p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
	}
	if p.Bits() == 0 {
		return p, fmt.Errorf("%s is every address; choose Anywhere instead", s)
	}
	if p.Addr().IsLoopback() {
		return p, fmt.Errorf("%s is this server itself, which can always connect", s)
	}
	return p, nil
}

// SourceText is how a source is written in a rule: an address alone, or a
// network in CIDR form.
func SourceText(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// NormalSources parses, dedupes and orders sources, and refuses any that
// is not one.
func NormalSources(in []string) ([]string, error) {
	if len(in) > MaxFirewallSources {
		return nil, fmt.Errorf("at most %d addresses", MaxFirewallSources)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		p, err := ParseSource(s)
		if err != nil {
			return nil, err
		}
		t := SourceText(p)
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

// Validate refuses a request the agent would not run.
func (r *FirewallRequest) Validate() error {
	if r.Port < 1 || r.Port > 65535 {
		return errors.New("the port must be between 1 and 65535")
	}
	if r.Check {
		return nil
	}
	if !ValidReach(r.Reach) {
		return fmt.Errorf("%q is not who may connect: local, selected, network or any", r.Reach)
	}
	src, err := NormalSources(r.Sources)
	if err != nil {
		return err
	}
	if r.Reach == ReachSelected && len(src) == 0 {
		return errors.New("selected machines needs at least one address")
	}
	if r.Reach != ReachSelected && r.Reach != ReachNetwork && len(src) > 0 {
		return errors.New("addresses are only for selected machines or the private network")
	}
	r.Sources = src
	return nil
}

// FirewallText is a change as the lines an approval is given for. The agent
// writes it for the firewall it finds; this is what it will allow.
func FirewallText(reach string, sources []string) string {
	switch reach {
	case ReachSelected, ReachNetwork:
		return "FIREWALL ALLOW " + strings.Join(sources, " ") + "\nFIREWALL DENY OTHERS"
	case ReachAny:
		return "FIREWALL ALLOW ANY"
	}
	return "FIREWALL REMOVE ARGUS RULES"
}

// OpensToAnywhere says an approval's text is a network change that opens a
// database to anywhere, which waits for approval on every database.
func OpensToAnywhere(text string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == FirewallText(ReachAny, nil) {
			return true
		}
	}
	return false
}

// FirewallComment is the comment on every ufw rule Argus adds for port.
func FirewallComment(port int) string { return "argus " + strconv.Itoa(port) }

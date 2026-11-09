package cloud

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

/*
Firewalls and load balancers: the network half of a cloud account, changed
through the same requested -> (approved) -> applied path as instances and
disks. Networks and subnets are read only; they are rarely changed by hand
and belong to whatever built them.

A rules change is a diff, rules added and rules taken away, never a whole
new set, so two people editing one group do not undo each other and the
approver reads exactly what moves. A change that lets the whole internet
reach anything but a web port is raised to the remove level, so it needs a
second person on staging as well as production.
*/

const (
	KindLoadBalancer Kind = "load-balancer"
	KindTargetGroup  Kind = "target-group"
)

// Network actions.
const (
	ActionFirewallCreate    Action = "firewall-create"
	ActionFirewallRules     Action = "firewall-rules"
	ActionFirewallDelete    Action = "firewall-delete"
	ActionInstanceFirewalls Action = "instance-firewalls"
	ActionTargetRegister    Action = "target-register"
	ActionTargetDeregister  Action = "target-deregister"
)

// NetworkActions are the actions a Network provider carries out.
var NetworkActions = []Action{
	ActionFirewallCreate, ActionFirewallRules, ActionFirewallDelete,
	ActionInstanceFirewalls, ActionTargetRegister, ActionTargetDeregister,
}

// IsNetwork reports whether a is one of NetworkActions.
func IsNetwork(a Action) bool {
	for _, x := range NetworkActions {
		if x == a {
			return true
		}
	}
	return false
}

// NetworkKindOf is the kind of resource a network action works on.
func NetworkKindOf(a Action) Kind {
	switch a {
	case ActionFirewallCreate, ActionFirewallRules, ActionFirewallDelete:
		return KindFirewall
	case ActionInstanceFirewalls:
		return KindInstance
	case ActionTargetRegister, ActionTargetDeregister:
		return KindTargetGroup
	}
	return ""
}

// FirewallSpec is a new security group.
type FirewallSpec struct {
	Network     string            `json:"network"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Rules       []Rule            `json:"rules,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
}

// Network is a provider that changes firewalls and load balancer targets.
// Every call runs as actor, with the account's manage role.
type Network interface {
	CreateFirewall(ctx context.Context, a Account, actor Actor, region string, s FirewallSpec) (Result, error)
	// ChangeRules takes rules away first, then adds, so a rule moved from
	// one source to another is never briefly open to both.
	ChangeRules(ctx context.Context, a Account, actor Actor, region, firewall string, add, remove []Rule) (Result, error)
	DeleteFirewall(ctx context.Context, a Account, actor Actor, region, firewall string) (Result, error)
	SetInstanceFirewalls(ctx context.Context, a Account, actor Actor, region, instance string, firewalls []string) (Result, error)
	RegisterTarget(ctx context.Context, a Account, actor Actor, region, targetGroup, instance string, port int32) (Result, error)
	DeregisterTarget(ctx context.Context, a Account, actor Actor, region, targetGroup, instance string, port int32) (Result, error)
}

// RiskyPorts are services that should never be reachable from the whole
// internet, by port.
var RiskyPorts = map[int32]string{
	21: "FTP", 22: "SSH", 23: "Telnet", 445: "SMB", 1433: "SQL Server", 2375: "Docker", 3306: "MySQL",
	3389: "Remote Desktop", 5432: "PostgreSQL", 5900: "VNC", 6379: "Redis", 9200: "Elasticsearch",
	11211: "Memcached", 27017: "MongoDB",
}

// Exposure says what an inbound rule lets the whole internet reach, in
// words, or "" when it does not. Web ports alone are not an exposure.
func (r Rule) Exposure() string {
	if !r.Open() {
		return ""
	}
	switch r.Protocol {
	case "all":
		return "every port"
	case "icmp":
		return ""
	}
	if r.Protocol == "tcp" && (r.FromPort == 80 || r.FromPort == 443) && r.FromPort == r.ToPort {
		return ""
	}
	var names []string
	ports := make([]int32, 0, len(RiskyPorts))
	for p := range RiskyPorts {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	for _, p := range ports {
		if r.Covers(p) || (r.Protocol == "udp" && r.FromPort <= p && p <= r.ToPort) {
			names = append(names, fmt.Sprintf("%s (%d)", RiskyPorts[p], p))
		}
	}
	if len(names) > 0 {
		return strings.Join(names, ", ")
	}
	return r.PortText()
}

// PortText is the rule's ports in words: "tcp 8080", "udp 1000-2000".
func (r Rule) PortText() string {
	switch r.Protocol {
	case "all":
		return "all traffic"
	case "icmp":
		return "ping (ICMP)"
	}
	if r.FromPort == r.ToPort {
		return fmt.Sprintf("%s %d", r.Protocol, r.FromPort)
	}
	return fmt.Sprintf("%s %d-%d", r.Protocol, r.FromPort, r.ToPort)
}

// Same reports whether two rules allow the same traffic, whatever their
// descriptions say.
func (r Rule) Same(o Rule) bool {
	return r.Inbound == o.Inbound && r.Protocol == o.Protocol && r.FromPort == o.FromPort &&
		r.ToPort == o.ToPort && strings.EqualFold(r.Source, o.Source)
}

var (
	reFirewallRef = regexp.MustCompile(`^sg-[0-9a-f]{8,17}$`)
	rePrefixList  = regexp.MustCompile(`^pl-[0-9a-f]{8,17}$`)
	reFirewallNm  = regexp.MustCompile(`^[A-Za-z0-9 ._\-:/()#,@\[\]+=&;{}!$*]{1,255}$`)
)

// IsFirewallRef reports whether s names a security group rather than a
// range of addresses.
func IsFirewallRef(s string) bool { return reFirewallRef.MatchString(s) }

// ValidFirewallName reports whether n can be a security group's name or
// description. AWS keeps "sg-" names for its ids.
func ValidFirewallName(n string) bool {
	return reFirewallNm.MatchString(n) && !strings.HasPrefix(strings.ToLower(n), "sg-")
}

// Check normalizes a rule and says what is wrong with it. A source is a
// CIDR range, a single address (made a /32 or /128), a security group's
// id, or a prefix list's.
func (r *Rule) Check() error {
	r.Protocol = strings.ToLower(strings.TrimSpace(r.Protocol))
	r.Source = strings.TrimSpace(r.Source)
	r.Description = strings.TrimSpace(r.Description)
	switch r.Protocol {
	case "all":
		r.FromPort, r.ToPort = 0, 65535
	case "icmp":
		r.FromPort, r.ToPort = -1, -1
	case "tcp", "udp":
		if r.ToPort == 0 {
			r.ToPort = r.FromPort
		}
		if r.FromPort < 0 || r.ToPort > 65535 || r.FromPort > r.ToPort {
			return fmt.Errorf("%s ports are 0 to 65535, the first no higher than the last", r.Protocol)
		}
	default:
		return fmt.Errorf("%q is not a protocol here: tcp, udp, icmp or all", r.Protocol)
	}
	if len(r.Description) > 255 {
		return fmt.Errorf("keep a rule's description under 255 characters")
	}
	switch {
	case r.Source == "":
		return fmt.Errorf("a rule needs a source: an address range like 10.0.0.0/16, an address, or a security group")
	case IsFirewallRef(r.Source), rePrefixList.MatchString(r.Source):
		return nil
	}
	if p, err := netip.ParsePrefix(r.Source); err == nil {
		if p.Masked() != p {
			return fmt.Errorf("%s is not the start of its range; did you mean %s?", r.Source, p.Masked())
		}
		return nil
	}
	if a, err := netip.ParseAddr(r.Source); err == nil {
		r.Source = netip.PrefixFrom(a, a.BitLen()).String()
		return nil
	}
	return fmt.Errorf("%q is not an address range, address or security group", r.Source)
}

// IPv6 reports whether the rule's source is an IPv6 range.
func (r Rule) IPv6() bool { return strings.Contains(r.Source, ":") && !IsFirewallRef(r.Source) }

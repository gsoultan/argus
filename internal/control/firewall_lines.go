package control

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The lines Argus keeps on a server's firewall, as people write them
(plans/server-firewall/design.md, "Let something in").

A line says who: everyone, private networks, addresses, servers in Argus,
servers with a tag, or servers with a policy. The agent only understands
addresses, so before a change is sent each line is worked out into the
addresses those servers report now. A line that names servers none of which
has an address is left off rather than sent with no addresses, which on
every firewall would mean "from anywhere".

Keys: a server's own line is "s" and eight letters; a policy's line on a
server is "p", the policy's first eight letters and the line's own key, so
the same policy line has the same key everywhere and a new version only
touches the lines that changed.
*/

// Who a line is for.
const (
	FWFromEveryone = "everyone"
	FWFromPrivate  = "private"
	FWFromAddress  = "address"
	FWFromServer   = "server"
	FWFromTag      = "tag"
	FWFromPolicy   = "policy"
)

// FWSource is one "from" (or, going out, "to") of a line.
type FWSource struct {
	Kind string `json:"kind"`
	// Value is the address or network, the server's asset id, the tag or
	// the policy's id; empty for everyone and private networks.
	Value string `json:"value,omitempty"`
	// Label is how the console shows it (a server's name, a policy's name);
	// filled in on reading, never trusted on writing.
	Label string `json:"label,omitempty"`
}

// FWLine is one line as a person wrote it.
type FWLine struct {
	Key       string     `json:"key"`
	Direction string     `json:"direction"`
	Action    string     `json:"action"`
	Protocol  string     `json:"protocol"`
	Ports     string     `json:"ports,omitempty"`
	From      []FWSource `json:"from"`
	Note      string     `json:"note,omitempty"`
	// Until ends the line by itself: Argus takes it off at the first
	// heartbeat after.
	Until   *time.Time `json:"until,omitempty"`
	AddedBy string     `json:"addedBy,omitempty"`
	AddedAt *time.Time `json:"addedAt,omitempty"`
}

// privateNets are the networks "private networks" means.
var privateNets = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

// Bounds on what a person writes.
const (
	maxFWLineSources = 50
	maxFWNote        = 200
	maxFWOwnLines    = 100
)

// validate checks a line as written and puts it in its usual form. key is
// set by the caller.
func (l *FWLine) validate() error {
	if l.Direction == "" {
		l.Direction = "in"
	}
	if l.Action == "" {
		l.Action = "allow"
	}
	l.Ports = strings.ReplaceAll(strings.TrimSpace(l.Ports), " ", "")
	l.Note = strings.TrimSpace(l.Note)
	if len(l.Note) > maxFWNote {
		return fmt.Errorf("the note is at most %d characters", maxFWNote)
	}
	if len(l.From) == 0 {
		return errors.New("say who the line is for")
	}
	if len(l.From) > maxFWLineSources {
		return fmt.Errorf("at most %d sources in one line", maxFWLineSources)
	}
	// What the agent would refuse, checked with a stand-in source.
	probe := tunnel.FirewallLine{Key: "x", Direction: l.Direction, Action: l.Action, Protocol: l.Protocol, Ports: l.Ports}
	if err := probe.Validate(); err != nil {
		return err
	}
	if (l.Protocol == "tcp" || l.Protocol == "udp") && l.Ports == "" {
		return errors.New("say which ports; choose Any protocol for every port")
	}
	seen := map[string]bool{}
	out := make([]FWSource, 0, len(l.From))
	for _, s := range l.From {
		s.Label = ""
		s.Value = strings.TrimSpace(s.Value)
		switch s.Kind {
		case FWFromEveryone, FWFromPrivate:
			s.Value = ""
		case FWFromAddress:
			p, err := tunnel.ParseSource(s.Value)
			if err != nil {
				return err
			}
			s.Value = tunnel.SourceText(p)
		case FWFromServer, FWFromPolicy:
			if !isUUID(s.Value) {
				return fmt.Errorf("%q is not a server or policy in Argus", s.Value)
			}
			s.Value = strings.ToLower(s.Value)
		case FWFromTag:
			if s.Value == "" || len(s.Value) > 64 {
				return errors.New("name the tag")
			}
		default:
			return fmt.Errorf("%q is not who a line can be for", s.Kind)
		}
		k := s.Kind + ":" + strings.ToLower(s.Value)
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	if seen[FWFromEveryone+":"] && len(out) > 1 {
		out = []FWSource{{Kind: FWFromEveryone}}
	}
	l.From = out
	if l.Until != nil {
		u := l.Until.UTC().Truncate(time.Second)
		l.Until = &u
	}
	return nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

// opensToEveryone says the line lets anyone in.
func (l FWLine) opensToEveryone() bool {
	if l.Direction != "in" || l.Action != "allow" {
		return false
	}
	for _, s := range l.From {
		if s.Kind == FWFromEveryone {
			return true
		}
	}
	return false
}

// expired says Until has passed.
func (l FWLine) expired(now time.Time) bool { return l.Until != nil && !now.Before(*l.Until) }

// coversPort says the line lets traffic to port in.
func (l FWLine) coversPort(proto string, port int) bool {
	if l.Direction != "in" || l.Action != "allow" {
		return false
	}
	if l.Protocol == "any" {
		return true
	}
	if l.Protocol != proto {
		return false
	}
	return portsCover(l.Ports, port)
}

func portsCover(ports string, port int) bool {
	for _, part := range strings.Split(ports, ",") {
		lo, hi, rng := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if rng {
			b, _ = strconv.Atoi(hi)
		}
		if port >= a && port <= b {
			return true
		}
	}
	return false
}

// words is a line as a sentence, for summaries and the audit log.
func (l FWLine) words() string {
	what := "everything"
	switch l.Protocol {
	case "tcp", "udp":
		what = strings.ToUpper(l.Protocol) + " " + l.Ports
	case "icmp":
		what = "ping"
	}
	var who []string
	for _, s := range l.From {
		who = append(who, s.words())
	}
	verb, prep := "Let in", "from"
	switch {
	case l.Direction == "in" && l.Action == "deny":
		verb = "Keep out"
	case l.Direction == "out" && l.Action == "allow":
		verb, prep = "Let out", "to"
	case l.Direction == "out":
		verb, prep = "Block going out", "to"
	}
	return fmt.Sprintf("%s %s %s %s", verb, what, prep, strings.Join(who, ", "))
}

func (s FWSource) words() string {
	name := s.Label
	if name == "" {
		name = s.Value
	}
	switch s.Kind {
	case FWFromEveryone:
		return "everyone"
	case FWFromPrivate:
		return "private networks"
	case FWFromTag:
		return "servers tagged " + s.Value
	case FWFromPolicy:
		return "servers with " + name
	}
	return name
}

// fwResolver turns servers, tags and policies into addresses.
type fwResolver struct {
	// addrs are each server's addresses, by asset id.
	addrs map[string][]string
	// tags are each tag's servers, by lower-case tag.
	tags map[string][]string
	// policies are the servers each policy is on, by policy id.
	policies map[string][]string
}

// resolve is the line as the agent will hold it, under key; ok is false
// when it names only servers with no address.
func (r fwResolver) resolve(l FWLine, key string) (tunnel.FirewallLine, bool) {
	out := tunnel.FirewallLine{Key: key, Direction: l.Direction, Action: l.Action, Protocol: l.Protocol, Ports: l.Ports}
	var srcs []string
	addServers := func(ids []string) {
		for _, id := range ids {
			srcs = append(srcs, r.addrs[id]...)
		}
	}
	for _, s := range l.From {
		switch s.Kind {
		case FWFromEveryone:
			out.Sources = nil
			return out, out.Validate() == nil
		case FWFromPrivate:
			srcs = append(srcs, privateNets...)
		case FWFromAddress:
			srcs = append(srcs, s.Value)
		case FWFromServer:
			addServers([]string{s.Value})
		case FWFromTag:
			addServers(r.tags[strings.ToLower(s.Value)])
		case FWFromPolicy:
			addServers(r.policies[s.Value])
		}
	}
	if len(srcs) == 0 {
		return out, false
	}
	out.Sources = srcs
	if len(out.Sources) > 256 {
		out.Sources = out.Sources[:256]
	}
	return out, out.Validate() == nil
}

// usableAddr says an address a server reports can stand for it in a rule.
func usableAddr(raw string) (string, bool) {
	raw = strings.SplitN(strings.TrimSpace(raw), "/", 2)[0]
	a, err := netip.ParseAddr(raw)
	if err != nil {
		return "", false
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast() || a.Zone() != "" {
		return "", false
	}
	return a.String(), true
}

// policyLineKey is the key a policy's line has on a server.
func policyLineKey(policyID, lineKey string) string {
	return "p" + strings.ReplaceAll(policyID, "-", "")[:8] + lineKey
}

// newLineKey is a new own or policy line's key.
func newLineKey(prefix string, n int) string {
	return prefix + randomKey(n)
}

func randomKey(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	r := make([]byte, n)
	_, _ = rand.Read(r)
	for i := range b {
		b[i] = letters[int(r[i])%len(letters)]
	}
	return string(b)
}

// sortFWLines puts lines in a steady order: going in first, blocks first.
func sortFWLines(ls []FWLine) {
	slices.SortStableFunc(ls, func(a, b FWLine) int {
		if a.Direction != b.Direction {
			return strings.Compare(a.Direction, b.Direction)
		}
		if a.Action != b.Action {
			return strings.Compare(b.Action, a.Action) // deny before allow
		}
		return strings.Compare(a.Key, b.Key)
	})
}

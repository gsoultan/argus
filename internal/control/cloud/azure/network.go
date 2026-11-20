package azure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Network security groups, changed the way the other clouds' firewalls are:
as a list of what is let through, with rules added and taken away.

Azure's own shape is richer than that list. A rule has a priority, it can
deny, and one rule can name several sources and several spans of ports at
once, which Argus shows as one neutral rule for each pair. Three choices
keep a change saying exactly what the approver read:

  - Taking a rule away takes that traffic out of every Azure rule that lets
    it through. An Azure rule left with nothing is deleted; one left with
    fewer sources or fewer ports is narrowed. When what would be left
    cannot be said as sources and ports (one port for one source only, of a
    rule with two of each), Argus refuses and changes nothing.
  - A rule Argus adds goes after the group's other rules and ahead of a
    deny-everything rule that closes the list, the usual place for one. It
    never goes ahead of a deny rule that would still block it: Argus
    refuses instead, so "added" always means the traffic gets through, and
    a deny someone wrote is never overruled without their seeing it.
  - Argus never writes or removes a deny rule.

A rules change is one write of the whole group under the etag Argus read,
so all of it happens or none of it, and a change made by someone else in
between is refused, not overwritten.

A VM's group is the one on its network interface. The group on its subnet
applies to everything in the subnet and is not Argus's to move for one VM.
*/

// MaxRuleDescription is the longest description Azure keeps on a rule.
const MaxRuleDescription = 140

// SourceTags are the service tags Argus lets a new rule name, beyond a
// range of addresses.
var SourceTags = []string{"VirtualNetwork", "AzureLoadBalancer"}

// ValidGroupName reports whether n can name a network security group.
func ValidGroupName(n string) bool { return reNICName.MatchString(n) }

// CheckRule normalizes a rule Argus is asked to add and says what is wrong
// with it. Its source is a range of addresses, one address, or tag: and one
// of SourceTags.
func CheckRule(r *cloud.Rule) error {
	r.Description = strings.TrimSpace(r.Description)
	if len(r.Description) > MaxRuleDescription {
		return fmt.Errorf("keep a rule's description under %d characters", MaxRuleDescription)
	}
	if tag, ok := strings.CutPrefix(strings.TrimSpace(r.Source), "tag:"); ok {
		for _, known := range SourceTags {
			if strings.EqualFold(tag, known) {
				probe := *r
				probe.Source = "10.0.0.0/8"
				if err := probe.Check(); err != nil {
					return err
				}
				probe.Source = "tag:" + known
				*r = probe
				return nil
			}
		}
		return fmt.Errorf("%q is not a tag Argus adds a rule for: %s", tag, strings.Join(SourceTags, " or "))
	}
	if err := r.Check(); err != nil {
		return err
	}
	if cloud.IsFirewallRef(r.Source) || strings.HasPrefix(r.Source, "pl-") {
		return fmt.Errorf("%q is not an address range, an address or tag:%s", r.Source, SourceTags[0])
	}
	return nil
}

// RuleName is the name of the Azure rule a listed rule came from: the
// start of its description.
func RuleName(r cloud.Rule) string {
	name, _, _ := strings.Cut(r.Description, ": ")
	return name
}

func ruleKey(r cloud.Rule) string {
	return fmt.Sprintf("%t|%s|%d|%d|%s", r.Inbound, r.Protocol, r.FromPort, r.ToPort, strings.ToLower(r.Source))
}

// left is what stays of one Azure rule's neutral rules once remove is
// taken away, and whether that can still be said as one Azure rule: every
// source that stays with every span of ports that stays.
func left(rules, remove []cloud.Rule) (keep []cloud.Rule, sayable bool) {
	seen := map[string]bool{}
	sources, spans := map[string]bool{}, map[[2]int32]bool{}
	for _, r := range rules {
		if slices.ContainsFunc(remove, r.Same) || seen[ruleKey(r)] {
			continue
		}
		seen[ruleKey(r)] = true
		keep = append(keep, r)
		sources[strings.ToLower(r.Source)] = true
		spans[[2]int32{r.FromPort, r.ToPort}] = true
	}
	return keep, len(keep) == len(sources)*len(spans)
}

// RemovalProblem says why remove cannot be taken out of a group as Argus
// read it, or "" when it can.
func RemovalProblem(g cloud.Resource, remove []cloud.Rule) string {
	byName := map[string][]cloud.Rule{}
	var names []string
	for _, r := range g.Rules {
		n := RuleName(r)
		if _, ok := byName[n]; !ok {
			names = append(names, n)
		}
		byName[n] = append(byName[n], r)
	}
	for _, n := range names {
		if _, ok := left(byName[n], remove); !ok {
			return "Azure rule " + n + " lets several sources reach several ports together, and what would be left of it cannot be said as one rule; take away all of a source or all of a port, or change it in the Azure portal"
		}
	}
	return ""
}

// securityGroupBody is a network security group as Azure returned it, kept whole so
// that writing it back changes only its rules.
type securityGroupBody struct {
	body  map[string]any
	etag  string
	props map[string]any
	// rules are its own rules, the same maps body holds.
	rules []map[string]any
	// uses is how many interfaces and subnets it is on.
	uses int
}

func (p *Provider) readGroup(ctx context.Context, t token, actor cloud.Actor, path string) (securityGroupBody, error) {
	var g securityGroupBody
	if err := p.get(ctx, t, actor, p.ep.arm+path+"?api-version="+networkAPI, &g.body); err != nil {
		return g, err
	}
	g.etag, _ = g.body["etag"].(string)
	if g.etag == "" {
		return g, errors.New("Azure gave the network security group no etag, so Argus cannot change it safely")
	}
	g.props, _ = g.body["properties"].(map[string]any)
	if g.props == nil {
		return g, errors.New("Azure's answer has no network security group in it")
	}
	list, _ := g.props["securityRules"].([]any)
	for _, r := range list {
		m, ok := r.(map[string]any)
		if !ok {
			return g, errors.New("Azure's answer has a rule Argus cannot read")
		}
		g.rules = append(g.rules, m)
	}
	for _, k := range []string{"networkInterfaces", "subnets"} {
		on, _ := g.props[k].([]any)
		g.uses += len(on)
	}
	return g, nil
}

func asRule(m map[string]any) (rawRule, error) {
	var r rawRule
	b, err := json.Marshal(m)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

func strs(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// keepSome writes back, of the values under one and many, those keep
// allows: one value under one, several under many.
func keepSome(props map[string]any, one, many string, keep func(string) bool) {
	var all []string
	if s, _ := props[one].(string); strings.TrimSpace(s) != "" {
		all = append(all, s)
	}
	all = append(all, strs(props[many])...)
	var kept []string
	for _, s := range all {
		if keep(strings.TrimSpace(s)) {
			kept = append(kept, s)
		}
	}
	delete(props, one)
	props[many] = []string{}
	switch len(kept) {
	case 0:
	case 1:
		props[one] = kept[0]
	default:
		props[many] = kept
	}
}

// narrow rewrites an Azure rule so that it lets through only keep.
func narrow(m map[string]any, inbound bool, keep []cloud.Rule) {
	props, _ := m["properties"].(map[string]any)
	sources, spans := map[string]bool{}, map[[2]int32]bool{}
	for _, r := range keep {
		sources[strings.ToLower(r.Source)] = true
		spans[[2]int32{r.FromPort, r.ToPort}] = true
	}
	side := "source"
	if !inbound {
		side = "destination"
	}
	keepSome(props, side+"AddressPrefix", side+"AddressPrefixes", func(s string) bool { return sources[strings.ToLower(source(s))] })
	if asgs, ok := props[side+"ApplicationSecurityGroups"].([]any); ok {
		var kept []any
		for _, a := range asgs {
			ref, _ := a.(map[string]any)
			id, _ := ref["id"].(string)
			if sources["group:"+strings.ToLower(last(id))] {
				kept = append(kept, a)
			}
		}
		if len(kept) == 0 {
			delete(props, side+"ApplicationSecurityGroups")
		} else {
			props[side+"ApplicationSecurityGroups"] = kept
		}
	}
	if len(spans) == 1 && (spans[[2]int32{-1, -1}] || spans[[2]int32{0, 65535}]) {
		return
	}
	keepSome(props, "destinationPortRange", "destinationPortRanges", func(s string) bool {
		from, to, ok := portSpan(s)
		return ok && spans[[2]int32{from, to}]
	})
}

func prefixesOf(r rawRule, inbound bool) []string {
	pr := r.Properties
	if inbound {
		return append(slices.Clone(pr.SourceAddressPrefixes), pr.SourceAddressPrefix)
	}
	return append(slices.Clone(pr.DestinationAddressPrefixes), pr.DestinationAddressPrefix)
}

// blocks reports whether deny rule d may stop some of what x lets through.
// It says no only when it is sure: another direction or protocol, ports
// that do not meet, or ranges of addresses that do not meet.
func blocks(d rawRule, x cloud.Rule) bool {
	pr := d.Properties
	if !strings.EqualFold(pr.Access, "Deny") || x.Inbound == strings.EqualFold(pr.Direction, "Outbound") {
		return false
	}
	if dp := protocol(pr.Protocol); dp != "all" && x.Protocol != "all" && dp != x.Protocol {
		return false
	}
	if x.Protocol == "tcp" || x.Protocol == "udp" {
		meet, named := false, false
		for _, ps := range append(slices.Clone(pr.DestinationPortRanges), pr.DestinationPortRange) {
			if from, to, ok := portSpan(strings.TrimSpace(ps)); ok {
				named = true
				meet = meet || (from <= x.ToPort && x.FromPort <= to)
			}
		}
		if named && !meet {
			return false
		}
	}
	want, err := netip.ParsePrefix(x.Source)
	if err != nil {
		return true
	}
	asgs := pr.SourceASGs
	if !x.Inbound {
		asgs = pr.DestinationASGs
	}
	if len(asgs) > 0 {
		return true
	}
	for _, s := range prefixesOf(d, x.Inbound) {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		have, err := netip.ParsePrefix(s)
		if err != nil {
			if a, aerr := netip.ParseAddr(s); aerr == nil {
				have = netip.PrefixFrom(a, a.BitLen())
			} else {
				return true
			}
		}
		if have.Overlaps(want) {
			return true
		}
	}
	return false
}

const (
	minPriority   = 100
	maxPriority   = 4096
	firstPriority = 1000
)

// priorityFor picks the priority a new allow rule x takes among rules, or says
// why Argus will not add it.
func priorityFor(rules []rawRule, x cloud.Rule) (int, error) {
	base, ceiling := 0, maxPriority+1
	var same []rawRule
	for _, r := range rules {
		if x.Inbound == strings.EqualFold(r.Properties.Direction, "Outbound") {
			continue
		}
		same = append(same, r)
		if !denyAll(r, x.Inbound) && r.Properties.Priority > base {
			base = r.Properties.Priority
		}
	}
	for _, r := range same {
		if denyAll(r, x.Inbound) && r.Properties.Priority > base && r.Properties.Priority < ceiling {
			ceiling = r.Properties.Priority
		}
	}
	first := base + 10
	if base == 0 {
		first = firstPriority
	}
	pick := 0
	for _, c := range []int{first, max(base+1, minPriority)} {
		if c < ceiling && c <= maxPriority {
			pick = c
			break
		}
	}
	if pick == 0 {
		return 0, fmt.Errorf("%w: the group has no priority left for a new rule after its others; make room in the Azure portal", cloud.ErrUnsupported)
	}
	for _, r := range same {
		if r.Properties.Priority < pick && blocks(r, x) {
			return 0, fmt.Errorf("%w: deny rule %s (priority %d) would still block %s, and Argus does not put a rule ahead of a deny; change it in the Azure portal",
				cloud.ErrUnsupported, r.Name, r.Properties.Priority, x.PortText())
		}
	}
	return pick, nil
}

func azureProtocol(p string) string {
	switch p {
	case "tcp":
		return "Tcp"
	case "udp":
		return "Udp"
	case "icmp":
		return "Icmp"
	}
	return "*"
}

// newRule is the Azure rule that lets x through, at priority.
func newRule(x cloud.Rule, priority int, taken func(string) bool) map[string]any {
	ports := "*"
	if x.Protocol == "tcp" || x.Protocol == "udp" {
		ports = strconv.Itoa(int(x.FromPort))
		if x.ToPort != x.FromPort {
			ports += "-" + strconv.Itoa(int(x.ToPort))
		}
		if x.FromPort == 0 && x.ToPort == 65535 {
			ports = "*"
		}
	}
	other := strings.TrimPrefix(x.Source, "tag:")
	sum := sha256.Sum256([]byte(ruleKey(x)))
	name := "argus-" + x.Protocol
	if ports != "*" {
		name += "-" + ports
	}
	if !x.Inbound {
		name += "-out"
	}
	name += "-" + hex.EncodeToString(sum[:3])
	for n, base := 2, name; taken(name); n++ {
		name = base + "-" + strconv.Itoa(n)
	}
	props := map[string]any{"priority": priority, "access": "Allow", "protocol": azureProtocol(x.Protocol),
		"sourcePortRange": "*", "destinationPortRange": ports}
	if x.Inbound {
		props["direction"], props["sourceAddressPrefix"], props["destinationAddressPrefix"] = "Inbound", other, "*"
	} else {
		props["direction"], props["sourceAddressPrefix"], props["destinationAddressPrefix"] = "Outbound", "*", other
	}
	if x.Description != "" {
		props["description"] = x.Description
	}
	return map[string]any{"name": name, "properties": props}
}

// addRules appends an Azure rule for each of add to rules, each placed by
// priorityFor among those before it.
func addRules(rules []map[string]any, add []cloud.Rule) ([]map[string]any, error) {
	for _, x := range add {
		if err := CheckRule(&x); err != nil {
			return nil, err
		}
		var have []rawRule
		for _, m := range rules {
			r, err := asRule(m)
			if err != nil {
				return nil, err
			}
			have = append(have, r)
		}
		pr, err := priorityFor(have, x)
		if err != nil {
			return nil, err
		}
		rules = append(rules, newRule(x, pr, func(n string) bool {
			return slices.ContainsFunc(have, func(r rawRule) bool { return strings.EqualFold(r.Name, n) })
		}))
	}
	return rules, nil
}

// ChangeRules takes rules out of a group and adds others, in one write.
func (p *Provider) ChangeRules(ctx context.Context, a cloud.Account, actor cloud.Actor, region, firewall string, add, remove []cloud.Rule) (cloud.Result, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	path, err := p.securityGroupPath(ctx, t, actor, a.AccountID, region, firewall)
	if err != nil {
		return cloud.Result{}, err
	}
	g, err := p.readGroup(ctx, t, actor, path)
	if err != nil {
		return cloud.Result{}, err
	}
	var raws []rawRule
	for _, m := range g.rules {
		r, err := asRule(m)
		if err != nil {
			return cloud.Result{}, err
		}
		raws = append(raws, r)
	}
	shown := map[string]bool{}
	for _, l := range listed(raws) {
		shown[l.name] = true
	}
	found := make([]bool, len(remove))
	var next []map[string]any
	deleted, narrowed := 0, 0
	for i, m := range g.rules {
		r := raws[i]
		if !shown[r.Name] {
			next = append(next, m)
			continue
		}
		all := allowed(r)
		hit := false
		for j, x := range remove {
			if slices.ContainsFunc(all, x.Same) {
				found[j], hit = true, true
			}
		}
		if !hit {
			next = append(next, m)
			continue
		}
		keep, ok := left(all, remove)
		switch {
		case !ok:
			return cloud.Result{}, fmt.Errorf("%w: what would be left of Azure rule %s cannot be said as one rule; change it in the Azure portal", cloud.ErrUnsupported, r.Name)
		case len(keep) == 0:
			deleted++
		default:
			narrow(m, !strings.EqualFold(r.Properties.Direction, "Outbound"), keep)
			next = append(next, m)
			narrowed++
		}
	}
	for j, ok := range found {
		if !ok {
			return cloud.Result{}, fmt.Errorf("the group no longer has %s; read the subscription again", remove[j].PortText())
		}
	}
	if next, err = addRules(next, add); err != nil {
		return cloud.Result{}, err
	}
	out := make([]any, 0, len(next))
	for _, m := range next {
		out = append(out, m)
	}
	g.props["securityRules"] = out
	req, err := p.sendIf(ctx, t, actor, http.MethodPut, p.ep.arm+path+"?api-version="+networkAPI, g.etag, g.body, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	var did []string
	if n := len(add); n > 0 {
		did = append(did, fmt.Sprintf("added %d", n))
	}
	if deleted > 0 {
		did = append(did, fmt.Sprintf("deleted %d", deleted))
	}
	if narrowed > 0 {
		did = append(did, fmt.Sprintf("narrowed %d", narrowed))
	}
	return cloud.Result{ResourceID: firewall, RequestID: req, Detail: "Azure rules: " + strings.Join(did, ", ")}, nil
}

// CreateFirewall makes a network security group in s.Group. Azure keeps no
// description on a group, so s.Description is not used.
func (p *Provider) CreateFirewall(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, s cloud.FirewallSpec) (cloud.Result, error) {
	if !ValidGroup(s.Group) || !ValidGroupName(s.Name) {
		return cloud.Result{}, fmt.Errorf("%q does not name a network security group in a resource group", s.Group+"/"+s.Name)
	}
	if place(region) != region || region == "" {
		return cloud.Result{}, fmt.Errorf("%q is not a region", region)
	}
	for k, v := range s.Tags {
		if why := TagProblem(k, v); why != "" {
			return cloud.Result{}, fmt.Errorf("%s=%s: %s", k, v, why)
		}
	}
	rules, err := addRules(nil, s.Rules)
	if err != nil {
		return cloud.Result{}, err
	}
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	path := SubscriptionScope(a.AccountID) + "/resourceGroups/" + url.PathEscape(s.Group) + "/providers/Microsoft.Network/networkSecurityGroups/" + url.PathEscape(s.Name)
	u := p.ep.arm + path + "?api-version=" + networkAPI
	if there, err := p.exists(ctx, t, actor, u, nil); err != nil {
		return cloud.Result{}, err
	} else if there {
		return cloud.Result{}, fmt.Errorf("resource group %s already has a network security group named %s", s.Group, s.Name)
	}
	if rules == nil {
		rules = []map[string]any{}
	}
	body := map[string]any{"location": region, "properties": map[string]any{"securityRules": rules}}
	if len(s.Tags) > 0 {
		body["tags"] = s.Tags
	}
	var made raw
	req, err := p.send(ctx, t, actor, http.MethodPut, u, body, &made)
	if err != nil {
		return cloud.Result{}, err
	}
	res := cloud.Result{ResourceID: strings.ToLower(made.Properties.ResourceGUID), RequestID: req, Detail: "Made " + s.Name + " in " + s.Group}
	if !ValidGUID(res.ResourceID) {
		res.ResourceID = pathID(path)
	}
	return res, nil
}

// DeleteFirewall deletes a group nothing uses.
func (p *Provider) DeleteFirewall(ctx context.Context, a cloud.Account, actor cloud.Actor, region, firewall string) (cloud.Result, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	path, err := p.securityGroupPath(ctx, t, actor, a.AccountID, region, firewall)
	if err != nil {
		return cloud.Result{}, err
	}
	g, err := p.readGroup(ctx, t, actor, path)
	if err != nil {
		return cloud.Result{}, err
	}
	if g.uses > 0 {
		return cloud.Result{}, fmt.Errorf("%s is on %d network interface%s or subnet%s; take it off first", last(path), g.uses, plural(g.uses), plural(g.uses))
	}
	req, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.arm+path+"?api-version="+networkAPI, nil, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: firewall, RequestID: req}, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// groupPaths are the region's network security groups by resourceGuid,
// each a path built from its checked names.
func (p *Provider) groupPaths(ctx context.Context, t token, actor cloud.Actor, subscription, region string) (map[string]string, error) {
	out := map[string]string{}
	err := p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Network/networkSecurityGroups?api-version="+networkAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for _, g := range items {
				rg := group(g.ID)
				id := strings.ToLower(g.Properties.ResourceGUID)
				if place(g.Location) != region || !ValidGUID(id) || !ValidGroup(rg) || !ValidGroupName(g.Name) {
					continue
				}
				out[id] = SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(rg) + "/providers/Microsoft.Network/networkSecurityGroups/" + url.PathEscape(g.Name)
			}
			return nil
		})
	return out, err
}

// subnetGroups is the network security group on each subnet that has one,
// both as Azure's paths in lower case.
func (p *Provider) subnetGroups(ctx context.Context, t token, actor cloud.Actor, subscription string) (map[string]string, error) {
	out := map[string]string{}
	err := p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Network/virtualNetworks?api-version="+networkAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for _, nw := range items {
				for _, s := range nw.Properties.Subnets {
					if s.Properties.NSG.ID != "" {
						out[strings.ToLower(s.ID)] = strings.ToLower(s.Properties.NSG.ID)
					}
				}
			}
			return nil
		})
	return out, err
}

// SetInstanceFirewalls sets the group on a VM's network interface.
// firewalls is every group the VM is to be behind: the ones on its subnets,
// which stay as they are, and at most one more, which goes on the
// interface. With none more, the interface's own group is taken off.
func (p *Provider) SetInstanceFirewalls(ctx context.Context, a cloud.Account, actor cloud.Actor, region, instance string, firewalls []string) (cloud.Result, error) {
	t, m, err := p.manage(ctx, a, actor, region, instance)
	if err != nil {
		return cloud.Result{}, err
	}
	if m.NICs != 1 || m.NIC == "" {
		return cloud.Result{}, fmt.Errorf("%w: %s has %d network interfaces, and Argus sets the group of a VM with one; set them in the Azure portal", cloud.ErrUnsupported, m.Name, m.NICs)
	}
	path, ok := nicPath(a.AccountID, m.NIC)
	if !ok {
		return cloud.Result{}, fmt.Errorf("Azure named %s's network interface in a way Argus cannot address", m.Name)
	}
	paths, err := p.groupPaths(ctx, t, actor, a.AccountID, region)
	if err != nil {
		return cloud.Result{}, err
	}
	onSubnet, err := p.subnetGroups(ctx, t, actor, a.AccountID)
	if err != nil {
		return cloud.Result{}, err
	}
	n, err := p.readNIC(ctx, t, actor, path)
	if err != nil {
		return cloud.Result{}, err
	}
	fixed := map[string]bool{}
	for _, c := range n.configs {
		ref, _ := configProps(c)["subnet"].(map[string]any)
		id, _ := ref["id"].(string)
		if g := onSubnet[strings.ToLower(id)]; g != "" {
			fixed[g] = true
		}
	}
	own := ""
	wanted := map[string]bool{}
	for _, id := range firewalls {
		gp, ok := paths[strings.ToLower(id)]
		if !ok {
			return cloud.Result{}, fmt.Errorf("subscription %s has no network security group with id %s in %s", a.AccountID, id, region)
		}
		wanted[strings.ToLower(gp)] = true
		if fixed[strings.ToLower(gp)] {
			continue
		}
		if own != "" {
			return cloud.Result{}, fmt.Errorf("a network interface takes one network security group, and %s was asked to take %s and %s", m.Name, last(own), last(gp))
		}
		own = gp
	}
	for g := range fixed {
		if !wanted[g] {
			return cloud.Result{}, fmt.Errorf("%s is on %s's subnet, which Argus does not change for one VM", last(g), m.Name)
		}
	}
	props, _ := n.body["properties"].(map[string]any)
	ref, _ := props["networkSecurityGroup"].(map[string]any)
	has, _ := ref["id"].(string)
	switch {
	case strings.EqualFold(has, own):
		return cloud.Result{}, fmt.Errorf("%s already has those network security groups", m.Name)
	case own == "" && len(fixed) == 0:
		return cloud.Result{}, fmt.Errorf("that would leave %s behind no network security group at all", m.Name)
	case own == "":
		delete(props, "networkSecurityGroup")
	default:
		props["networkSecurityGroup"] = map[string]any{"id": own}
	}
	req, err := p.writeNIC(ctx, t, actor, path, n)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: instance, RequestID: req}, nil
}

// RegisterTarget is not something Argus does in Azure yet.
func (p *Provider) RegisterTarget(context.Context, cloud.Account, cloud.Actor, string, string, string, int32) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("put a VM behind a load balancer")
}

// DeregisterTarget is not something Argus does in Azure yet.
func (p *Provider) DeregisterTarget(context.Context, cloud.Account, cloud.Actor, string, string, string, int32) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("take a VM out of a load balancer")
}

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Firewall rules, as Argus changes them in Google Cloud.

A Google firewall rule is one direction, a list of protocols and ports it
allows, a list of sources, and targets. It allows every port it lists from
every source it lists. Argus shows it as rules, one per port and source,
the way it shows an AWS security group, so a change must leave a set that
is every port from every source; anything else could not be one Google
rule, and Argus will not quietly widen it.

Argus changes only inbound rules that allow traffic and reach VMs by
network tag. A rule Argus makes targets the tag of its own name, so a VM
gets it by carrying that tag; giving a VM a set of rules is setting its
tags. Deny rules, outbound rules, and rules that target service accounts
are left to the Google Cloud console.
*/

// CheckRule normalizes a rule Argus would add to a Google firewall rule
// and says what is wrong with it. A source is an address range, a single
// address, or tag:NAME for VMs carrying a network tag.
func CheckRule(r *cloud.Rule) error {
	if !r.Inbound {
		return errors.New("Argus changes inbound rules in Google Cloud; change outbound rules in the Google Cloud console")
	}
	src := strings.TrimSpace(r.Source)
	tag, isTag := strings.CutPrefix(src, "tag:")
	if isTag {
		if !ValidName(tag) {
			return fmt.Errorf("%q is not a network tag: lowercase letters, digits and -, up to 63, starting with a letter", tag)
		}
		// Check the rest of the rule against any address, then put the tag back.
		r.Source = "10.0.0.0/8"
	} else if cloud.IsFirewallRef(src) || strings.HasPrefix(src, "pl-") {
		return errors.New("a Google rule's source is an address range or tag:NAME, not an AWS group")
	}
	err := r.Check()
	if isTag {
		r.Source = "tag:" + tag
	}
	if err != nil {
		return err
	}
	if !isTag {
		if _, perr := netip.ParsePrefix(r.Source); perr != nil {
			return fmt.Errorf("%q is not an address range", r.Source)
		}
	}
	return nil
}

// spanKey is a rule's protocol and ports, the part Google lists in allowed.
func spanKey(r cloud.Rule) string { return fmt.Sprintf("%s/%d/%d", r.Protocol, r.FromPort, r.ToPort) }

// shape turns rules into one Google rule's allowed list and sources, or
// says why they cannot be one: every port must come from every source.
func shape(rules []cloud.Rule) (allowed []map[string]any, ranges, tags []string, err error) {
	if len(rules) == 0 {
		return nil, nil, nil, errors.New("a Google firewall rule allows something; to allow nothing, delete it")
	}
	var spans []cloud.Rule
	sources := map[string][]string{}
	for _, r := range rules {
		k := spanKey(r)
		if _, ok := sources[k]; !ok {
			spans = append(spans, r)
		}
		if !slices.Contains(sources[k], r.Source) {
			sources[k] = append(sources[k], r.Source)
		}
	}
	first := slices.Clone(sources[spanKey(spans[0])])
	slices.Sort(first)
	for _, s := range spans[1:] {
		have := slices.Clone(sources[spanKey(s)])
		slices.Sort(have)
		if !slices.Equal(first, have) {
			return nil, nil, nil, fmt.Errorf("one Google firewall rule lets every port it lists in from every source it lists; %s would come from %s but the rest from %s. Make another firewall rule for it",
				s.PortText(), strings.Join(have, ", "), strings.Join(first, ", "))
		}
	}
	for _, src := range first {
		if t, ok := strings.CutPrefix(src, "tag:"); ok {
			tags = append(tags, t)
		} else {
			ranges = append(ranges, src)
		}
	}
	ports := map[string][]string{}
	var protos []string
	for _, s := range spans {
		if !slices.Contains(protos, s.Protocol) {
			protos = append(protos, s.Protocol)
		}
		if (s.Protocol == "tcp" || s.Protocol == "udp") && !(s.FromPort == 0 && s.ToPort == 65535) {
			p := strconv.Itoa(int(s.FromPort))
			if s.ToPort != s.FromPort {
				p += "-" + strconv.Itoa(int(s.ToPort))
			}
			ports[s.Protocol] = append(ports[s.Protocol], p)
		} else if s.Protocol == "tcp" || s.Protocol == "udp" {
			ports[s.Protocol] = append(ports[s.Protocol], "")
		}
	}
	for _, p := range protos {
		a := map[string]any{"IPProtocol": p}
		if ps := ports[p]; len(ps) > 0 && !slices.Contains(ps, "") {
			a["ports"] = ps
		}
		allowed = append(allowed, a)
	}
	return allowed, ranges, tags, nil
}

// RulesProblem says why rules cannot be one Google firewall rule, or "".
func RulesProblem(rules []cloud.Rule) string {
	if _, _, _, err := shape(rules); err != nil {
		return err.Error()
	}
	return ""
}

// Uneditable says why Argus leaves a firewall rule to the Google Cloud
// console, from what the inventory read of it, or "".
func Uneditable(f cloud.Resource) string {
	switch {
	case f.Details["action"] == "deny":
		return f.Name + " denies traffic; Argus changes rules that allow it"
	case f.Details["direction"] == "egress":
		return f.Name + " is an outbound rule; Argus changes inbound rules"
	case f.Details["targetServiceAccounts"] != "":
		return f.Name + " reaches VMs by service account; Argus changes rules that reach them by network tag"
	case f.State == "disabled":
		return f.Name + " is turned off; turn it on in the Google Cloud console first"
	}
	return ""
}

// TargetTags are the network tags a firewall rule reaches VMs by, from
// what the inventory read of it.
func TargetTags(f cloud.Resource) []string {
	var out []string
	for _, t := range strings.Split(f.Details["targetTags"], ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (p *Provider) firewallsURL(project string) string {
	return p.ep.compute + "/compute/v1/projects/" + project + "/global/firewalls"
}

// CreateFirewall makes an inbound firewall rule in a network that reaches
// VMs carrying the network tag of its own name.
func (p *Provider) CreateFirewall(ctx context.Context, a cloud.Account, actor cloud.Actor, _ string, s cloud.FirewallSpec) (cloud.Result, error) {
	if !ValidName(s.Name) {
		return cloud.Result{}, fmt.Errorf("%q cannot name a firewall rule: lowercase letters, digits and -, up to 63, starting with a letter", s.Name)
	}
	if !reID.MatchString(s.Network) {
		return cloud.Result{}, fmt.Errorf("%q is not a Compute Engine id", s.Network)
	}
	for i := range s.Rules {
		if err := CheckRule(&s.Rules[i]); err != nil {
			return cloud.Result{}, err
		}
	}
	allowed, ranges, tags, err := shape(s.Rules)
	if err != nil {
		return cloud.Result{}, err
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	var nw struct {
		SelfLink string `json:"selfLink"`
	}
	if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+a.AccountID+"/global/networks/"+s.Network+"?fields=selfLink", &nw); err != nil {
		return cloud.Result{}, err
	}
	body := map[string]any{
		"name": s.Name, "description": s.Description, "network": nw.SelfLink, "direction": "INGRESS", "priority": 1000,
		"allowed": allowed, "targetTags": []string{s.Name},
	}
	if len(ranges) > 0 {
		body["sourceRanges"] = ranges
	}
	if len(tags) > 0 {
		body["sourceTags"] = tags
	}
	op, err := p.send(ctx, t, actor, http.MethodPost, p.firewallsURL(a.AccountID), body)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: op.TargetID, RequestID: op.Name}, nil
}

// ChangeRules takes rules away from a firewall rule and adds others, in
// one update: Google replaces its lists at once, so it is never briefly
// open to both.
func (p *Provider) ChangeRules(ctx context.Context, a cloud.Account, actor cloud.Actor, _ string, id string, add, remove []cloud.Rule) (cloud.Result, error) {
	if !reID.MatchString(id) {
		return cloud.Result{}, fmt.Errorf("%q is not a Compute Engine id", id)
	}
	for i := range add {
		if err := CheckRule(&add[i]); err != nil {
			return cloud.Result{}, err
		}
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	var b json.RawMessage
	if err := p.get(ctx, t, actor, p.firewallsURL(a.AccountID)+"/"+id, &b); err != nil {
		return cloud.Result{}, err
	}
	var r raw
	var whole map[string]any
	if err := json.Unmarshal(b, &r); err != nil {
		return cloud.Result{}, err
	}
	if err := json.Unmarshal(b, &whole); err != nil {
		return cloud.Result{}, err
	}
	have := firewall(r, map[string]cloud.Ref{})
	if why := Uneditable(have); why != "" {
		return cloud.Result{}, errors.New(why)
	}
	var next []cloud.Rule
	for _, x := range have.Rules {
		if !slices.ContainsFunc(remove, x.Same) {
			next = append(next, x)
		}
	}
	for _, x := range add {
		if !slices.ContainsFunc(next, x.Same) {
			next = append(next, x)
		}
	}
	allowed, ranges, tags, err := shape(next)
	if err != nil {
		return cloud.Result{}, err
	}
	whole["allowed"] = allowed
	whole["sourceRanges"] = ranges
	whole["sourceTags"] = tags
	if ranges == nil {
		whole["sourceRanges"] = []string{}
	}
	if tags == nil {
		whole["sourceTags"] = []string{}
	}
	op, err := p.send(ctx, t, actor, http.MethodPut, p.firewallsURL(a.AccountID)+"/"+r.Name, whole)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: op.Name}, nil
}

// DeleteFirewall deletes a firewall rule. VMs it reached lose what it let
// in at once.
func (p *Provider) DeleteFirewall(ctx context.Context, a cloud.Account, actor cloud.Actor, _ string, firewall string) (cloud.Result, error) {
	if !reID.MatchString(firewall) {
		return cloud.Result{}, fmt.Errorf("%q is not a Compute Engine id", firewall)
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	op, err := p.send(ctx, t, actor, http.MethodDelete, p.firewallsURL(a.AccountID)+"/"+firewall, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: firewall, RequestID: op.Name}, nil
}

// SetInstanceFirewalls gives a VM exactly the tag-targeted firewall rules
// picked, in its network, by setting its network tags: the picked rules'
// target tags go on, and the tags only other rules target come off. Rules
// with no targets reach every VM in the network whatever its tags.
func (p *Provider) SetInstanceFirewalls(ctx context.Context, a cloud.Account, actor cloud.Actor, region, instance string, firewalls []string) (cloud.Result, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	w, err := p.locate(ctx, t, actor, a.AccountID, region, "instances", instance)
	if err != nil {
		return cloud.Result{}, err
	}
	var vm struct {
		Name string `json:"name"`
		Tags struct {
			Items       []string `json:"items"`
			Fingerprint string   `json:"fingerprint"`
		} `json:"tags"`
		NetworkInterfaces []struct {
			Network string `json:"network"`
		} `json:"networkInterfaces"`
	}
	u := p.ep.compute + w.path(a.AccountID, "instances") + instance
	if err := p.get(ctx, t, actor, u+"?fields=name,tags,networkInterfaces(network)", &vm); err != nil {
		return cloud.Result{}, err
	}
	if len(vm.NetworkInterfaces) == 0 {
		return cloud.Result{}, fmt.Errorf("%s has no network", vm.Name)
	}
	all, err := p.list(ctx, t, p.firewallsURL(a.AccountID), "")
	if err != nil {
		return cloud.Result{}, err
	}
	items, err := retag(vm.Tags.Items, trimLink(vm.NetworkInterfaces[0].Network), all, firewalls)
	if err != nil {
		return cloud.Result{}, err
	}
	op, err := p.send(ctx, t, actor, http.MethodPost, u+"/setTags", map[string]any{"items": items, "fingerprint": vm.Tags.Fingerprint})
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: instance, RequestID: op.Name}, nil
}

// retag is a VM's network tags once it has exactly the picked rules of
// its network that reach VMs by tag.
func retag(have []string, network string, all []raw, picked []string) ([]string, error) {
	var want, other []string
	seen := map[string]bool{}
	for _, f := range all {
		if trimLink(f.Network) != network {
			continue
		}
		in := slices.Contains(picked, f.ID)
		if in {
			seen[f.ID] = true
			if len(f.TargetTags) == 0 || len(f.TargetSAs) > 0 {
				return nil, fmt.Errorf("%s does not reach VMs by network tag, so it cannot be given or taken away", f.Name)
			}
			want = append(want, f.TargetTags...)
		} else {
			other = append(other, f.TargetTags...)
		}
	}
	for _, id := range picked {
		if !seen[id] {
			return nil, fmt.Errorf("firewall rule %s is not in the VM's network", id)
		}
	}
	var out []string
	for _, tg := range have {
		if slices.Contains(other, tg) && !slices.Contains(want, tg) {
			continue
		}
		if !slices.Contains(out, tg) {
			out = append(out, tg)
		}
	}
	for _, tg := range want {
		if !slices.Contains(out, tg) {
			out = append(out, tg)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// RegisterTarget is not built for Google Cloud yet.
func (p *Provider) RegisterTarget(context.Context, cloud.Account, cloud.Actor, string, string, string, int32) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("put a VM behind a load balancer")
}

// DeregisterTarget is not built for Google Cloud yet.
func (p *Provider) DeregisterTarget(context.Context, cloud.Account, cloud.Actor, string, string, string, int32) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("take a VM out of a load balancer")
}

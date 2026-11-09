package cloud

import (
	"context"
	"errors"
	"fmt"
)

// The fake as Network: each call edits Resources the way AWS would, and
// refuses what AWS refuses.

func (f *Fake) CreateFirewall(_ context.Context, a Account, actor Actor, region string, s FirewallSpec) (Result, error) {
	if err := f.storageCall(a, ActionFirewallCreate, s.Name, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.Resources[region] {
		if r.Kind == KindFirewall && r.Network == s.Network && r.Name == s.Name {
			return Result{}, errors.New("InvalidGroup.Duplicate: the security group '" + s.Name + "' already exists")
		}
	}
	rules := append([]Rule(nil), s.Rules...)
	rules = append(rules, Rule{Protocol: "all", FromPort: 0, ToPort: 65535, Source: "0.0.0.0/0"})
	id := f.add(region, Resource{Kind: KindFirewall, Name: s.Name, Network: s.Network, Rules: rules,
		Tags: map[string]string{"argus:created-by": string(actor)}, Details: map[string]string{"description": s.Description}})
	return Result{ResourceID: id, RequestID: f.request()}, nil
}

func (f *Fake) ChangeRules(_ context.Context, a Account, actor Actor, region, firewall string, add, remove []Rule) (Result, error) {
	if err := f.storageCall(a, ActionFirewallRules, firewall, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.findKind(region, KindFirewall, firewall)
	if err != nil {
		return Result{}, err
	}
	g := &f.Resources[region][i]
	for _, x := range remove {
		at := -1
		for j, r := range g.Rules {
			if r.Same(x) {
				at = j
			}
		}
		if at < 0 {
			return Result{}, errors.New("InvalidPermission.NotFound: the rule " + x.PortText() + " from " + x.Source + " does not exist")
		}
		g.Rules = append(g.Rules[:at], g.Rules[at+1:]...)
	}
	for _, x := range add {
		for _, r := range g.Rules {
			if r.Same(x) {
				return Result{}, errors.New("InvalidPermission.Duplicate: the rule " + x.PortText() + " from " + x.Source + " already exists")
			}
		}
		g.Rules = append(g.Rules, x)
	}
	return Result{ResourceID: firewall, RequestID: f.request()}, nil
}

func (f *Fake) DeleteFirewall(_ context.Context, a Account, actor Actor, region, firewall string) (Result, error) {
	if err := f.storageCall(a, ActionFirewallDelete, firewall, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.findKind(region, KindFirewall, firewall)
	if err != nil {
		return Result{}, err
	}
	for _, r := range f.Resources[region] {
		for _, l := range r.Links {
			if l.Kind == KindFirewall && l.ID == firewall && r.State != "terminated" {
				return Result{}, fmt.Errorf("DependencyViolation: resource %s has a dependent object", firewall)
			}
		}
	}
	f.drop(region, i)
	return Result{ResourceID: firewall, RequestID: f.request()}, nil
}

func (f *Fake) SetInstanceFirewalls(_ context.Context, a Account, actor Actor, region, instance string, firewalls []string) (Result, error) {
	if err := f.storageCall(a, ActionInstanceFirewalls, instance, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.find(region, instance)
	if err != nil {
		return Result{}, err
	}
	var refs []Ref
	for _, id := range firewalls {
		j, err := f.findKind(region, KindFirewall, id)
		if err != nil {
			return Result{}, err
		}
		refs = append(refs, Ref{Kind: KindFirewall, ID: id, Name: f.Resources[region][j].Name})
	}
	r := &f.Resources[region][i]
	out := []Ref{}
	for _, l := range r.Links {
		if l.Kind != KindFirewall {
			out = append(out, l)
		}
	}
	r.Links = append(refs, out...)
	return Result{ResourceID: instance, RequestID: f.request()}, nil
}

func (f *Fake) RegisterTarget(_ context.Context, a Account, actor Actor, region, targetGroup, instance string, port int32) (Result, error) {
	if err := f.storageCall(a, ActionTargetRegister, targetGroup+" "+instance, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.findKind(region, KindTargetGroup, targetGroup)
	if err != nil {
		return Result{}, err
	}
	if _, err := f.find(region, instance); err != nil {
		return Result{}, err
	}
	f.link(region, i, Ref{Kind: KindInstance, ID: instance, Name: "initial"})
	return Result{ResourceID: targetGroup, RequestID: f.request()}, nil
}

func (f *Fake) DeregisterTarget(_ context.Context, a Account, actor Actor, region, targetGroup, instance string, port int32) (Result, error) {
	if err := f.storageCall(a, ActionTargetDeregister, targetGroup+" "+instance, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.findKind(region, KindTargetGroup, targetGroup)
	if err != nil {
		return Result{}, err
	}
	f.unlink(region, i, KindInstance, instance)
	return Result{ResourceID: targetGroup, RequestID: f.request()}, nil
}

var _ Network = (*Fake)(nil)

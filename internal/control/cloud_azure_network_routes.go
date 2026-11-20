package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/control/cloud/azure"
)

/*
Network security groups in an Azure subscription, planned the way security
groups are in AWS but in Azure's shape: a group lives in a resource group
and a region, not in a network; a VM is behind the group on its network
interface and the one on its subnet, and only the first is Argus's to set
for one VM; and a rule can be taken away only when what is left of the
Azure rule that holds it can still be said. See azure/network.go.
*/

func (a *API) planAzureNetwork(ctx context.Context, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	bad := http.StatusBadRequest
	switch in.Action {
	case cloud.ActionFirewallCreate:
		return a.planAzureFirewallCreate(ctx, m, in, ch)
	case cloud.ActionFirewallRules, cloud.ActionFirewallDelete, cloud.ActionInstanceFirewalls:
	default:
		return ch, http.StatusNotImplemented, fmt.Errorf("Argus cannot %s in Azure yet", in.Action)
	}
	res, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.NetworkKindOf(in.Action), in.ResourceID)
	if err != nil {
		return ch, http.StatusNotFound, err
	}
	ch.ResourceID, ch.ResourceName = res.ID, res.Name
	ch.Environment = cloudEnvOf(res.Tags, m.acct.Environment)

	switch in.Action {
	case cloud.ActionFirewallRules:
		var s cloudRulesSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, bad, errors.New("a rules change is {add: [rule], remove: [rule]}")
		}
		if len(s.Add) == 0 && len(s.Remove) == 0 {
			return ch, bad, errors.New("nothing to change")
		}
		if len(s.Add)+len(s.Remove) > maxRuleChange {
			return ch, bad, fmt.Errorf("change at most %d rules at once", maxRuleChange)
		}
		for i := range s.Remove {
			// What is taken away is named as Argus listed it, which a tag
			// or an application security group is not a range for.
			x := &s.Remove[i]
			x.Protocol, x.Source = strings.ToLower(strings.TrimSpace(x.Protocol)), strings.TrimSpace(x.Source)
			n := slices.IndexFunc(res.Rules, x.Same)
			if n < 0 {
				return ch, http.StatusConflict, fmt.Errorf("%s has no rule %s; read the subscription again", res.Name, ruleWords(*x))
			}
			x.Description = res.Rules[n].Description
		}
		if why := azure.RemovalProblem(res, s.Remove); why != "" {
			return ch, http.StatusConflict, errors.New(why)
		}
		for i := range s.Add {
			if err := azure.CheckRule(&s.Add[i]); err != nil {
				return ch, bad, err
			}
			x := s.Add[i]
			if slices.ContainsFunc(res.Rules, x.Same) && !slices.ContainsFunc(s.Remove, x.Same) {
				return ch, http.StatusConflict, fmt.Errorf("%s already has %s", res.Name, ruleWords(x))
			}
			if slices.ContainsFunc(s.Add[:i], x.Same) {
				return ch, bad, fmt.Errorf("%s is in the change twice", ruleWords(x))
			}
		}
		users := a.firewallUsers(ctx, m.acct.ID, in.Region, res.ID)
		ch.Environment = firewallEnv(res, users, m.acct.Environment)
		s.Users = len(users)
		s.Exposes = exposures(s.Add)
		var parts []string
		for _, x := range s.Add {
			parts = append(parts, "add "+ruleWords(x))
		}
		for _, x := range s.Remove {
			parts = append(parts, "remove "+ruleWords(x))
		}
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Change rules on " + res.Name + ": " + strings.Join(parts, "; ")
		if len(s.Exposes) > 0 {
			ch.Level = cloud.LevelRemove
			ch.Summary += exposeWords(s.Exposes)
		}
		if s.Users > 0 {
			ch.Summary += fmt.Sprintf(" (in front of %d VM%s)", s.Users, plural(s.Users))
		}
		if len(s.Add) > 0 && res.Details["denyRules"] != "" {
			ch.Summary += "; the group has deny rules, and Azure is asked for nothing if one would still block what is added"
		}

	case cloud.ActionFirewallDelete:
		if err := confirmTyped(in, res); err != nil {
			return ch, bad, err
		}
		if users := a.firewallUsers(ctx, m.acct.ID, in.Region, res.ID); len(users) > 0 {
			var names []string
			for _, u := range users {
				names = append(names, u.Name)
			}
			return ch, http.StatusConflict, errors.New(res.Name + " is in front of " + strings.Join(names, ", ") + "; take it off them first")
		}
		if n := res.Details["interfaces"]; n != "" && n != "0" {
			return ch, http.StatusConflict, errors.New(res.Name + " is on " + n + " network interfaces or subnets; take it off them in the Azure portal first")
		}
		ch.Spec = json.RawMessage("{}")
		ch.Summary = "Delete network security group " + res.Name + " (" + res.Details["resourceGroup"] + ")"

	case cloud.ActionInstanceFirewalls:
		var s cloudInstanceFirewallsSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, bad, errors.New("pick the VM's network security group")
		}
		if !liveInstance(res) {
			return ch, http.StatusConflict, errors.New(res.Name + " is being removed")
		}
		if set := res.Details["scaleSet"]; set != "" {
			return ch, http.StatusConflict, errors.New(res.Name + " belongs to scale set " + set + ", which sets its network interface; change the group there")
		}
		// The groups on its subnets stay: they apply to everything there.
		var fixed []string
		for _, l := range res.Links {
			switch l.Kind {
			case cloud.KindFirewall:
				s.From = append(s.From, l.ID)
			case cloud.KindSubnet:
				sn, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindSubnet, l.ID)
				if err != nil {
					continue
				}
				for _, g := range sn.Links {
					if g.Kind == cloud.KindFirewall && !slices.Contains(fixed, g.ID) {
						fixed = append(fixed, g.ID)
					}
				}
			}
		}
		if len(s.Firewalls) == 0 {
			return ch, bad, errors.New("a VM stays behind at least one network security group")
		}
		if sameSet(s.From, s.Firewalls) {
			return ch, bad, errors.New("nothing to change")
		}
		s.Names = nil
		var added []cloud.Rule
		own := 0
		for i, id := range s.Firewalls {
			if slices.Contains(s.Firewalls[:i], id) {
				return ch, bad, errors.New("a network security group is in the list twice")
			}
			g, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindFirewall, id)
			if err != nil {
				return ch, bad, errors.New("pick network security groups in " + in.Region)
			}
			s.Names = append(s.Names, g.Name)
			if !slices.Contains(fixed, id) {
				own++
			}
			if !slices.Contains(s.From, id) {
				added = append(added, g.Rules...)
				ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(g.Tags, m.acct.Environment))
			}
		}
		if own > 1 {
			return ch, bad, errors.New("a VM's network interface takes one network security group")
		}
		for _, id := range fixed {
			if !slices.Contains(s.Firewalls, id) {
				name := id
				if g, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindFirewall, id); err == nil {
					name = g.Name
				}
				return ch, bad, errors.New(name + " is on " + res.Name + "'s subnet and applies to everything in it; Argus does not take it off one VM")
			}
		}
		s.Exposes = exposures(added)
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Set the network security groups of " + res.Name + " to " + strings.Join(s.Names, ", ")
		if own == 0 {
			ch.Summary += "; its network interface is left with none of its own"
		}
		if len(s.Exposes) > 0 {
			ch.Level = cloud.LevelRemove
			ch.Summary += exposeWords(s.Exposes)
		}
	}
	return ch, http.StatusOK, nil
}

func (a *API) planAzureFirewallCreate(ctx context.Context, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	bad := http.StatusBadRequest
	var s cloudFirewallCreateSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, bad, errors.New("a network security group takes {group, name, rules}")
	}
	s.Name, s.Group = strings.TrimSpace(s.Name), strings.TrimSpace(s.Group)
	s.Network, s.NetworkName, s.Description = "", "", ""
	if !azure.ValidGroupName(s.Name) {
		return ch, bad, errors.New("a network security group's name is up to 80 letters, digits and ._-, starting with a letter or digit and not ending in . or -")
	}
	if !slices.Contains(m.acct.Regions, in.Region) {
		return ch, bad, errors.New("pick a region Argus reads in this subscription")
	}
	n := slices.IndexFunc(a.azureGroups(ctx, m.acct.ID), func(g string) bool { return strings.EqualFold(g, s.Group) })
	if n < 0 {
		return ch, bad, errors.New("pick a resource group Argus has read something in")
	}
	s.Group = a.azureGroups(ctx, m.acct.ID)[n]
	for _, rg := range m.acct.Regions {
		for _, g := range a.regionItems(ctx, m.acct.ID, rg, cloud.KindFirewall) {
			if strings.EqualFold(g.Name, s.Name) && strings.EqualFold(g.Details["resourceGroup"], s.Group) {
				return ch, http.StatusConflict, errors.New(s.Group + " already has a network security group named " + g.Name)
			}
		}
	}
	if len(s.Rules) > maxRuleChange {
		return ch, bad, fmt.Errorf("start with at most %d rules", maxRuleChange)
	}
	for i := range s.Rules {
		if err := azure.CheckRule(&s.Rules[i]); err != nil {
			return ch, bad, err
		}
		if slices.ContainsFunc(s.Rules[:i], s.Rules[i].Same) {
			return ch, bad, fmt.Errorf("%s is in the list twice", ruleWords(s.Rules[i]))
		}
	}
	if len(s.Tags) > azure.MaxTags-1 {
		return ch, bad, fmt.Errorf("Azure keeps at most %d tags on a resource", azure.MaxTags)
	}
	for k, v := range s.Tags {
		if why := azure.TagProblem(k, v); why != "" {
			return ch, bad, fmt.Errorf("%s=%s: %s", k, v, why)
		}
	}
	if e := cloudEnvOf(s.Tags, ""); e != "" {
		ch.Environment = e
	}
	s.Exposes = exposures(s.Rules)
	inbound := 0
	for _, x := range s.Rules {
		if x.Inbound {
			inbound++
		}
	}
	ch.ResourceName = s.Name
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = fmt.Sprintf("Make network security group %s in resource group %s with %d inbound rule%s", s.Name, s.Group, inbound, plural(inbound))
	if len(s.Exposes) > 0 {
		ch.Level = cloud.LevelRemove
		ch.Summary += exposeWords(s.Exposes)
	}
	return ch, http.StatusOK, nil
}

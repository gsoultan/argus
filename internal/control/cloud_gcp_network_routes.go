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
	"github.com/gsoultan/argus/internal/control/cloud/gcp"
)

/*
Firewall rules in a Google Cloud project, planned the way security groups
are in AWS but in Google's shape: a rule is global, belongs to one network,
and reaches VMs by network tag. See gcp/network.go.
*/

// maxGCPFirewalls is how many tag-targeted firewall rules Argus gives one VM.
const maxGCPFirewalls = 10

// gcpFirewallUsers are the live VMs a firewall rule reaches, across the
// project's regions.
func (a *API) gcpFirewallUsers(ctx context.Context, m cloudManaged, id string) []cloud.Resource {
	var out []cloud.Resource
	for _, rg := range m.acct.Regions {
		if rg == gcp.GlobalRegion {
			continue
		}
		out = append(out, a.firewallUsers(ctx, m.acct.ID, rg, id)...)
	}
	return out
}

func gcpRuleList(rules []cloud.Rule) string {
	var spans, sources []string
	for _, r := range rules {
		if p := r.PortText(); !slices.Contains(spans, p) {
			spans = append(spans, p)
		}
		src := r.Source
		if r.Open() {
			src = "anywhere"
		}
		if !slices.Contains(sources, src) {
			sources = append(sources, src)
		}
	}
	return strings.Join(spans, ", ") + " from " + strings.Join(sources, ", ")
}

func (a *API) planGCPNetwork(ctx context.Context, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	if in.Action == cloud.ActionFirewallCreate {
		return a.planGCPFirewallCreate(ctx, m, in, ch)
	}
	region := in.Region
	if in.Action != cloud.ActionInstanceFirewalls {
		region = gcp.GlobalRegion
	}
	res, err := a.store.CloudResourceOf(ctx, m.acct.ID, region, cloud.NetworkKindOf(in.Action), in.ResourceID)
	if err != nil {
		return ch, http.StatusNotFound, err
	}
	ch.ResourceID, ch.ResourceName = res.ID, res.Name
	ch.Environment = cloudEnvOf(res.Tags, m.acct.Environment)

	switch in.Action {
	case cloud.ActionFirewallRules:
		if why := gcp.Uneditable(res); why != "" {
			return ch, http.StatusConflict, errors.New(why + "; change it in the Google Cloud console")
		}
		var s cloudRulesSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("a rules change is {add: [rule], remove: [rule]}")
		}
		if len(s.Add) == 0 && len(s.Remove) == 0 {
			return ch, http.StatusBadRequest, errors.New("nothing to change")
		}
		if len(s.Add)+len(s.Remove) > maxRuleChange {
			return ch, http.StatusBadRequest, fmt.Errorf("change at most %d rules at once", maxRuleChange)
		}
		for i := range s.Add {
			if err := gcp.CheckRule(&s.Add[i]); err != nil {
				return ch, http.StatusBadRequest, err
			}
		}
		if s.Remove, err = storedRules(res, s.Remove, "project"); err != nil {
			return ch, http.StatusConflict, err
		}
		next := slices.DeleteFunc(slices.Clone(res.Rules), func(x cloud.Rule) bool { return slices.ContainsFunc(s.Remove, x.Same) })
		for i, x := range s.Add {
			if slices.ContainsFunc(next, x.Same) {
				return ch, http.StatusConflict, fmt.Errorf("%s already has %s", res.Name, ruleWords(x))
			}
			if slices.ContainsFunc(s.Add[:i], x.Same) {
				return ch, http.StatusBadRequest, fmt.Errorf("%s is in the change twice", ruleWords(x))
			}
		}
		next = append(next, s.Add...)
		if why := gcp.RulesProblem(next); why != "" {
			return ch, http.StatusBadRequest, errors.New(why)
		}
		users := a.gcpFirewallUsers(ctx, m, res.ID)
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
		ch.Summary = "Change firewall rule " + res.Name + ": " + strings.Join(parts, "; ")
		if len(s.Exposes) > 0 {
			ch.Level = cloud.LevelRemove
			ch.Summary += exposeWords(s.Exposes)
		}
		if s.Users > 0 {
			ch.Summary += fmt.Sprintf(" (reaches %d VM%s)", s.Users, plural(s.Users))
		}

	case cloud.ActionFirewallDelete:
		if err := confirmTyped(in, res); err != nil {
			return ch, http.StatusBadRequest, err
		}
		users := a.gcpFirewallUsers(ctx, m, res.ID)
		ch.Environment = firewallEnv(res, users, m.acct.Environment)
		// Google deletes a rule whatever reaches it, and a VM keeps its
		// network tag, so deleting one only ever lets in less. It is how a
		// rule's last allow is taken away, so it is not refused while VMs
		// are behind it: the summary names them, and the change is a
		// remove, which waits for approval where the environment says so.
		ch.Spec = json.RawMessage("{}")
		ch.Summary = "Delete firewall rule " + res.Name + " (" + res.ID + ")"
		if len(users) > 0 {
			var names []string
			for _, u := range users {
				names = append(names, u.Name)
			}
			ch.Summary += fmt.Sprintf("; the %d VM%s it reaches (%s) lose what it lets in", len(users), plural(len(users)), strings.Join(names, ", "))
		}

	case cloud.ActionInstanceFirewalls:
		var s cloudInstanceFirewallsSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("pick the VM's firewall rules")
		}
		if !liveInstance(res) {
			return ch, http.StatusConflict, errors.New(res.Name + " is being removed")
		}
		for _, l := range res.Links {
			if l.Kind == cloud.KindFirewall {
				s.From = append(s.From, l.ID)
			}
		}
		if sameSet(s.From, s.Firewalls) {
			return ch, http.StatusBadRequest, errors.New("nothing to change")
		}
		s.Names = nil
		var added []cloud.Rule
		tagged := 0
		pick := func(id string) (cloud.Resource, error) {
			g, err := a.store.CloudResourceOf(ctx, m.acct.ID, gcp.GlobalRegion, cloud.KindFirewall, id)
			if err != nil {
				return g, errors.New("pick firewall rules Argus has read in this project")
			}
			return g, nil
		}
		for i, id := range s.Firewalls {
			if slices.Contains(s.Firewalls[:i], id) {
				return ch, http.StatusBadRequest, errors.New("a firewall rule is in the list twice")
			}
			g, err := pick(id)
			if err != nil {
				return ch, http.StatusBadRequest, err
			}
			if g.Network != res.Network {
				return ch, http.StatusBadRequest, errors.New(g.Name + " is in another network than " + res.Name)
			}
			s.Names = append(s.Names, g.Name)
			if slices.Contains(s.From, id) {
				continue
			}
			if len(gcp.TargetTags(g)) == 0 || g.Details["targetServiceAccounts"] != "" {
				return ch, http.StatusBadRequest, errors.New(g.Name + " does not reach VMs by network tag, so Argus cannot give it to one")
			}
			if g.Details["action"] != "deny" && g.State != "disabled" {
				added = append(added, g.Rules...)
			}
			ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(g.Tags, m.acct.Environment))
		}
		for _, id := range s.From {
			if slices.Contains(s.Firewalls, id) {
				continue
			}
			g, err := pick(id)
			if err != nil {
				return ch, http.StatusConflict, errors.New("read the project again: " + err.Error())
			}
			if len(gcp.TargetTags(g)) == 0 || g.Details["targetServiceAccounts"] != "" {
				return ch, http.StatusBadRequest, errors.New(g.Name + " reaches every VM in the network, or reaches them by service account; Argus cannot take it off one")
			}
		}
		for _, id := range s.Firewalls {
			if g, err := pick(id); err == nil && len(gcp.TargetTags(g)) > 0 {
				tagged++
			}
		}
		if tagged > maxGCPFirewalls {
			return ch, http.StatusBadRequest, fmt.Errorf("give a VM at most %d firewall rules by tag", maxGCPFirewalls)
		}
		s.Exposes = exposures(added)
		ch.Spec, _ = json.Marshal(s)
		names := "none of its own"
		if len(s.Names) > 0 {
			names = strings.Join(s.Names, ", ")
		}
		ch.Summary = "Set the firewall rules of " + res.Name + " to " + names + ", by its network tags"
		if len(s.Exposes) > 0 {
			ch.Level = cloud.LevelRemove
			ch.Summary += exposeWords(s.Exposes)
		}

	default:
		return ch, http.StatusBadRequest, fmt.Errorf("Argus cannot %s in Google Cloud yet", in.Action)
	}
	return ch, http.StatusOK, nil
}

func (a *API) planGCPFirewallCreate(ctx context.Context, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	var s cloudFirewallCreateSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a firewall rule takes {network, name, description, rules}")
	}
	s.Name, s.Description = strings.TrimSpace(s.Name), strings.TrimSpace(s.Description)
	if !gcp.ValidName(s.Name) {
		return ch, http.StatusBadRequest, errors.New("a firewall rule's name is lowercase letters, digits and -, up to 63, starting with a letter")
	}
	if len(s.Description) > 2048 {
		return ch, http.StatusBadRequest, errors.New("keep the description under 2048 characters")
	}
	if len(s.Tags) > 0 {
		return ch, http.StatusBadRequest, errors.New("a Google firewall rule takes no labels")
	}
	nw, err := a.store.CloudResourceOf(ctx, m.acct.ID, gcp.GlobalRegion, cloud.KindNetwork, s.Network)
	if err != nil {
		return ch, http.StatusBadRequest, errors.New("pick a network of this project")
	}
	for _, g := range a.regionItems(ctx, m.acct.ID, gcp.GlobalRegion, cloud.KindFirewall) {
		if g.Name == s.Name {
			return ch, http.StatusConflict, errors.New("this project already has a firewall rule named " + s.Name)
		}
	}
	if len(s.Rules) > maxRuleChange {
		return ch, http.StatusBadRequest, fmt.Errorf("start with at most %d rules", maxRuleChange)
	}
	for i := range s.Rules {
		if err := gcp.CheckRule(&s.Rules[i]); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if slices.ContainsFunc(s.Rules[:i], s.Rules[i].Same) {
			return ch, http.StatusBadRequest, fmt.Errorf("%s is in the list twice", ruleWords(s.Rules[i]))
		}
	}
	if why := gcp.RulesProblem(s.Rules); why != "" {
		return ch, http.StatusBadRequest, errors.New(why)
	}
	s.NetworkName = nw.Name
	s.Exposes = exposures(s.Rules)
	ch.ResourceName = s.Name
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = "Make firewall rule " + s.Name + " in " + nw.Name + ": allow " + gcpRuleList(s.Rules) + " to VMs tagged " + s.Name
	if len(s.Exposes) > 0 {
		ch.Level = cloud.LevelRemove
		ch.Summary += exposeWords(s.Exposes)
	}
	return ch, http.StatusOK, nil
}

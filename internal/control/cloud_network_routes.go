package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 4: security groups and load balancer targets.

The same path as every other change (cloud_change_routes.go). Levels:
making a group, changing its rules, changing an instance's groups and
putting instances behind or taking them out of a target group are change;
deleting a group is remove, and needs its name typed.

Two rules make these safer than the AWS console:

  - A security group's environment is the strictest of its own and of every
    instance and load balancer that uses it, so loosening a group shared
    with production is a production change.
  - Anything that lets the whole internet reach more than a web port (80 or
    443) is raised to the remove level: it needs a second person on staging
    as well as production, and its summary says what it exposes.

A rules change is a diff, rules added and taken away, so the approver reads
exactly what moves and two people editing one group do not undo each other.
*/

type cloudFirewallCreateSpec struct {
	cloud.FirewallSpec
	NetworkName string   `json:"networkName,omitempty"`
	Exposes     []string `json:"exposes,omitempty"`
}

type cloudRulesSpec struct {
	Add     []cloud.Rule `json:"add"`
	Remove  []cloud.Rule `json:"remove"`
	Exposes []string     `json:"exposes,omitempty"`
	// Users is how many instances and load balancers use the group.
	Users int `json:"users"`
}

type cloudInstanceFirewallsSpec struct {
	Firewalls []string `json:"firewalls"`
	Names     []string `json:"names,omitempty"`
	From      []string `json:"from,omitempty"`
	Exposes   []string `json:"exposes,omitempty"`
}

type cloudTargetSpec struct {
	Instance     string `json:"instance"`
	InstanceName string `json:"instanceName,omitempty"`
	Port         int32  `json:"port,omitempty"`
	// Last is a deregister that leaves no healthy target.
	Last bool `json:"last,omitempty"`
}

// maxRuleChange is the most rules one change adds and takes away.
const maxRuleChange = 50

// firewallUsers is every live instance and load balancer that uses a group.
func (a *API) firewallUsers(ctx context.Context, accountID, region, id string) []cloud.Resource {
	var out []cloud.Resource
	for _, k := range []cloud.Kind{cloud.KindInstance, cloud.KindLoadBalancer} {
		for _, r := range a.regionItems(ctx, accountID, region, k) {
			if k == cloud.KindInstance && !liveInstance(r) {
				continue
			}
			for _, l := range r.Links {
				if l.Kind == cloud.KindFirewall && l.ID == id {
					out = append(out, r)
					break
				}
			}
		}
	}
	return out
}

// firewallEnv is the strictest environment of a group and what uses it.
func firewallEnv(g cloud.Resource, users []cloud.Resource, account string) string {
	env := cloudEnvOf(g.Tags, account)
	for _, u := range users {
		env = stricterEnv(env, cloudEnvOf(u.Tags, account))
	}
	return env
}

// exposures is what a set of rules opens to the whole internet, beyond web
// ports, in words.
func exposures(rules []cloud.Rule) []string {
	var out []string
	for _, r := range rules {
		if e := r.Exposure(); e != "" && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

func ruleWords(r cloud.Rule) string {
	dir, src := "from", r.Source
	if !r.Inbound {
		dir = "to"
	}
	if r.Open() || (!r.Inbound && (src == "0.0.0.0/0" || src == "::/0")) {
		src = "anywhere"
	}
	return r.PortText() + " " + dir + " " + src
}

// checkRules normalizes rules and checks each source group exists here.
func (a *API) checkRules(ctx context.Context, accountID, region string, rules []cloud.Rule) error {
	for i := range rules {
		if err := rules[i].Check(); err != nil {
			return err
		}
		if cloud.IsFirewallRef(rules[i].Source) {
			if _, err := a.store.CloudResourceOf(ctx, accountID, region, cloud.KindFirewall, rules[i].Source); err != nil {
				return fmt.Errorf("%s is not a security group Argus has read in %s", rules[i].Source, region)
			}
		}
	}
	return nil
}

func exposeWords(e []string) string {
	return "; opens " + strings.Join(e, ", ") + " to the whole internet"
}

// planNetwork checks a network request and works out what it will do.
func (a *API) planNetwork(r *http.Request, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	nw, ok := m.manager.(cloud.Network)
	if !ok || nw == nil {
		return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot change firewalls yet")
	}
	if err := a.storageMissing(m, in.Action); err != nil {
		return ch, http.StatusConflict, err
	}
	ctx := r.Context()
	ch.Environment = m.acct.Environment
	if in.Action == cloud.ActionFirewallCreate {
		return a.planFirewallCreate(ctx, m, in, ch)
	}

	kind := cloud.NetworkKindOf(in.Action)
	res, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, kind, in.ResourceID)
	if err != nil {
		return ch, http.StatusNotFound, err
	}
	ch.ResourceID, ch.ResourceName = res.ID, res.Name
	ch.Environment = cloudEnvOf(res.Tags, m.acct.Environment)
	stack := res.Tags["aws:cloudformation:stack-name"]

	switch in.Action {
	case cloud.ActionFirewallRules:
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
		if err := a.checkRules(ctx, m.acct.ID, in.Region, s.Add); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if err := a.checkRules(ctx, m.acct.ID, in.Region, s.Remove); err != nil {
			return ch, http.StatusBadRequest, err
		}
		for i, x := range s.Remove {
			found := false
			for _, have := range res.Rules {
				if have.Same(x) {
					found = true
					s.Remove[i].Description = have.Description
				}
			}
			if !found {
				return ch, http.StatusConflict, fmt.Errorf("%s has no rule %s; read the account again", res.Name, ruleWords(x))
			}
		}
		for i, x := range s.Add {
			for _, have := range res.Rules {
				// A rule moving from one description to another is taken
				// away and added again, which is allowed.
				if have.Same(x) && !slices.ContainsFunc(s.Remove, x.Same) {
					return ch, http.StatusConflict, fmt.Errorf("%s already has %s", res.Name, ruleWords(x))
				}
			}
			for _, y := range s.Add[:i] {
				if y.Same(x) {
					return ch, http.StatusBadRequest, fmt.Errorf("%s is in the change twice", ruleWords(x))
				}
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
			ch.Summary += fmt.Sprintf(" (used by %d instance%s or load balancer%s)", s.Users, plural(s.Users), plural(s.Users))
		}

	case cloud.ActionFirewallDelete:
		if err := confirmTyped(in, res); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if res.Name == "default" {
			return ch, http.StatusConflict, errors.New("every network keeps its default security group; AWS does not delete it")
		}
		if stack != "" {
			return ch, http.StatusConflict, errors.New(res.Name + " belongs to CloudFormation stack " + stack + "; delete it there, or the stack breaks")
		}
		if users := a.firewallUsers(ctx, m.acct.ID, in.Region, res.ID); len(users) > 0 {
			var names []string
			for _, u := range users {
				names = append(names, u.Name)
			}
			return ch, http.StatusConflict, errors.New(res.Name + " is in use by " + strings.Join(names, ", ") + "; take it off them first")
		}
		for _, g := range a.regionItems(ctx, m.acct.ID, in.Region, cloud.KindFirewall) {
			for _, x := range g.Rules {
				if x.Source == res.ID && g.ID != res.ID {
					return ch, http.StatusConflict, errors.New("a rule in " + g.Name + " names " + res.Name + "; remove that rule first")
				}
			}
		}
		ch.Summary = "Delete security group " + res.Name + " (" + res.ID + ")"

	case cloud.ActionInstanceFirewalls:
		var s cloudInstanceFirewallsSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("pick the instance's security groups")
		}
		if len(s.Firewalls) == 0 || len(s.Firewalls) > 5 {
			return ch, http.StatusBadRequest, errors.New("an instance has one to five security groups")
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
		for i, id := range s.Firewalls {
			if slices.Contains(s.Firewalls[:i], id) {
				return ch, http.StatusBadRequest, errors.New("a security group is in the list twice")
			}
			g, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindFirewall, id)
			if err != nil {
				return ch, http.StatusBadRequest, errors.New("pick security groups in " + in.Region)
			}
			if g.Network != res.Network {
				return ch, http.StatusBadRequest, errors.New(g.Name + " is in another network than " + res.Name)
			}
			s.Names = append(s.Names, g.Name)
			if !slices.Contains(s.From, id) {
				added = append(added, g.Rules...)
				ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(g.Tags, m.acct.Environment))
			}
		}
		s.Exposes = exposures(added)
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Set the security groups of " + res.Name + " to " + strings.Join(s.Names, ", ")
		if len(s.Exposes) > 0 {
			ch.Level = cloud.LevelRemove
			ch.Summary += exposeWords(s.Exposes)
		}

	case cloud.ActionTargetRegister, cloud.ActionTargetDeregister:
		var s cloudTargetSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil || s.Instance == "" {
			return ch, http.StatusBadRequest, errors.New("pick the instance")
		}
		if t := res.Details["targetType"]; t != "" && t != "instance" {
			return ch, http.StatusBadRequest, errors.New(res.Name + " sends traffic to " + t + " targets, not instances; change it in AWS")
		}
		if s.Port < 0 || s.Port > 65535 {
			return ch, http.StatusBadRequest, errors.New("a port is 1 to 65535, or empty for the group's own")
		}
		inst, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, s.Instance)
		if err != nil {
			return ch, http.StatusBadRequest, err
		}
		s.InstanceName = inst.Name
		var member bool
		healthy := 0
		for _, l := range res.Links {
			if l.Kind != cloud.KindInstance {
				continue
			}
			if l.ID == inst.ID {
				member = true
			} else if l.Name == "healthy" {
				healthy++
			}
		}
		ch.Environment = stricterEnv(ch.Environment, cloudEnvOf(inst.Tags, m.acct.Environment))
		port := res.Details["port"]
		if s.Port > 0 {
			port = strconv.Itoa(int(s.Port))
		}
		if in.Action == cloud.ActionTargetRegister {
			if member {
				return ch, http.StatusConflict, errors.New(inst.Name + " is already in " + res.Name)
			}
			if !liveInstance(inst) {
				return ch, http.StatusConflict, errors.New(inst.Name + " is being removed")
			}
			if res.Network != "" && inst.Network != res.Network {
				return ch, http.StatusBadRequest, errors.New(inst.Name + " is in another network than " + res.Name)
			}
			ch.Summary = "Put " + inst.Name + " behind " + res.Name
		} else {
			if !member {
				return ch, http.StatusConflict, errors.New(inst.Name + " is not in " + res.Name)
			}
			s.Last = healthy == 0 && lbOf(res) != ""
			ch.Summary = "Take " + inst.Name + " out of " + res.Name
		}
		if port != "" {
			ch.Summary += " on port " + port
		}
		if s.Last {
			ch.Summary += "; it leaves " + lbOf(res) + " with no healthy target, so it stops answering"
		}
		ch.Spec, _ = json.Marshal(s)
	default:
		return ch, http.StatusBadRequest, fmt.Errorf("%q is not a network change", in.Action)
	}
	if stack != "" && in.Action != cloud.ActionFirewallDelete {
		ch.Summary += " (it belongs to CloudFormation stack " + stack + ", which may change it back)"
	}
	return ch, http.StatusOK, nil
}

// lbOf names the load balancers a target group serves.
func lbOf(tg cloud.Resource) string {
	var names []string
	for _, l := range tg.Links {
		if l.Kind == cloud.KindLoadBalancer {
			names = append(names, cmpName(l))
		}
	}
	return strings.Join(names, " and ")
}

func cmpName(l cloud.Ref) string {
	if l.Name != "" {
		return l.Name
	}
	return l.ID
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(x, y)
}

func (a *API) planFirewallCreate(ctx context.Context, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	var s cloudFirewallCreateSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a security group takes {network, name, description, rules}")
	}
	s.Name, s.Description = strings.TrimSpace(s.Name), strings.TrimSpace(s.Description)
	if s.Description == "" {
		s.Description = s.Name
	}
	if !cloud.ValidFirewallName(s.Name) {
		return ch, http.StatusBadRequest, errors.New("a security group's name is up to 255 letters, digits, spaces and ._-:/()#,@[]+=&;{}!$*, and does not start with sg-")
	}
	if !cloud.ValidFirewallName(s.Description) {
		return ch, http.StatusBadRequest, errors.New("a description is up to 255 letters, digits, spaces and ._-:/()#,@[]+=&;{}!$*")
	}
	nw, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindNetwork, s.Network)
	if err != nil {
		return ch, http.StatusBadRequest, errors.New("pick a network in " + in.Region)
	}
	for _, g := range a.regionItems(ctx, m.acct.ID, in.Region, cloud.KindFirewall) {
		if g.Network == nw.ID && strings.EqualFold(g.Name, s.Name) {
			return ch, http.StatusConflict, errors.New(nw.Name + " already has a security group named " + g.Name)
		}
	}
	if len(s.Rules) > maxRuleChange {
		return ch, http.StatusBadRequest, fmt.Errorf("start with at most %d rules", maxRuleChange)
	}
	if err := a.checkRules(ctx, m.acct.ID, in.Region, s.Rules); err != nil {
		return ch, http.StatusBadRequest, err
	}
	for i, x := range s.Rules {
		for _, y := range s.Rules[:i] {
			if y.Same(x) {
				return ch, http.StatusBadRequest, fmt.Errorf("%s is in the list twice", ruleWords(x))
			}
		}
	}
	for k, v := range s.Tags {
		if !cloud.ValidTagKey(k) || len(v) > 256 || k == "Name" {
			return ch, http.StatusBadRequest, fmt.Errorf("%q cannot be a tag here", k)
		}
	}
	if e := cloudEnvOf(s.Tags, ""); e != "" {
		ch.Environment = e
	}
	s.NetworkName = nw.Name
	s.Exposes = exposures(s.Rules)
	inbound := 0
	for _, x := range s.Rules {
		if x.Inbound {
			inbound++
		}
	}
	ch.ResourceName = s.Name
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = fmt.Sprintf("Make security group %s in %s with %d inbound rule%s", s.Name, nw.Name, inbound, plural(inbound))
	if len(s.Exposes) > 0 {
		ch.Level = cloud.LevelRemove
		ch.Summary += exposeWords(s.Exposes)
	}
	return ch, http.StatusOK, nil
}

// applyNetwork runs a network change as the person who asked.
func (a *API) applyNetwork(ctx context.Context, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	nw, ok := m.manager.(cloud.Network)
	if !ok {
		return cloud.Result{}, errors.New(m.acct.Provider + " cannot change firewalls yet")
	}
	actor := cloud.Actor(ch.RequestedBy)
	t, region, id := m.target, ch.Region, ch.ResourceID
	switch ch.Action {
	case cloud.ActionFirewallCreate:
		var s cloudFirewallCreateSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		if s.Tags == nil {
			s.Tags = map[string]string{}
		}
		s.Tags["argus:change"] = ch.ID
		return nw.CreateFirewall(ctx, t, actor, region, s.FirewallSpec)
	case cloud.ActionFirewallRules:
		var s cloudRulesSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		return nw.ChangeRules(ctx, t, actor, region, id, s.Add, s.Remove)
	case cloud.ActionFirewallDelete:
		return nw.DeleteFirewall(ctx, t, actor, region, id)
	case cloud.ActionInstanceFirewalls:
		var s cloudInstanceFirewallsSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		return nw.SetInstanceFirewalls(ctx, t, actor, region, id, s.Firewalls)
	case cloud.ActionTargetRegister, cloud.ActionTargetDeregister:
		var s cloudTargetSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		if ch.Action == cloud.ActionTargetRegister {
			res, err := nw.RegisterTarget(ctx, t, actor, region, id, s.Instance, s.Port)
			if err == nil {
				res.Detail = "The load balancer sends it traffic once its health check passes"
			}
			return res, err
		}
		res, err := nw.DeregisterTarget(ctx, t, actor, region, id, s.Instance, s.Port)
		if err == nil {
			res.Detail = "The load balancer drains its open connections first"
		}
		return res, err
	}
	return cloud.Result{}, fmt.Errorf("%q is not a network change", ch.Action)
}

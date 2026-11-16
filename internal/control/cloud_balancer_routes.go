package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Target groups, load balancers and Auto Scaling group sizes (cloud/balancer.go
says what each is). All Change level, through the usual plan, approval and
apply. A load balancer bills from the moment it exists, about $16 to $22 a
month before traffic, and its plan says so.
*/

// balancerMonthly is AWS's hourly charge for an idle load balancer of a
// type, a month, in the cheaper regions: what the plan quotes.
var balancerMonthly = map[string]float64{"application": 0.0225 * cloud.HoursPerMonth, "network": 0.0225 * cloud.HoursPerMonth}

func (a *API) planBalancing(r *http.Request, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	if _, ok := m.manager.(cloud.Balancing); !ok {
		return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot make load balancers from Argus yet")
	}
	if err := a.storageMissing(m, in.Action); err != nil {
		return ch, http.StatusConflict, err
	}
	ctx := r.Context()
	ch.Environment = m.acct.Environment
	switch in.Action {
	case cloud.ActionTargetGroupCreate:
		var s cloud.TargetGroupSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("a target group takes {network, name, protocol, port, health}")
		}
		s.Tags = nil
		if err := s.Check(); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if _, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindNetwork, s.Network); err != nil {
			return ch, http.StatusBadRequest, errors.New("pick a network in " + in.Region)
		}
		if a.nameTaken(ctx, m.acct.ID, in.Region, cloud.KindTargetGroup, s.Name) {
			return ch, http.StatusConflict, errors.New("there is already a target group called " + s.Name + " in " + in.Region)
		}
		ch.Spec, _ = json.Marshal(s)
		ch.ResourceName = s.Name
		ch.Summary = fmt.Sprintf("Make target group %s for %s %d, checked by %s", s.Name, s.Protocol, s.Port, s.Health.Words())
		return ch, http.StatusOK, nil

	case cloud.ActionBalancerCreate:
		var s cloud.BalancerSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("a load balancer takes {name, type, subnets, port, targetGroup}")
		}
		s.Tags = nil
		if err := s.Check(); err != nil {
			return ch, http.StatusBadRequest, err
		}
		tg, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindTargetGroup, s.TargetGroup)
		if err != nil {
			return ch, http.StatusBadRequest, errors.New("pick a target group in " + in.Region)
		}
		proto := tg.Details["protocol"]
		if s.Type == "application" && proto != "HTTP" && proto != "HTTPS" {
			return ch, http.StatusBadRequest, errors.New(tg.Name + " is " + proto + "; an application load balancer forwards to an HTTP or HTTPS target group")
		}
		if s.Type == "network" && proto != "TCP" {
			return ch, http.StatusBadRequest, errors.New(tg.Name + " is " + proto + "; a network load balancer forwards to a TCP target group")
		}
		for _, l := range tg.Links {
			if l.Kind == cloud.KindLoadBalancer {
				return ch, http.StatusConflict, errors.New(tg.Name + " is already behind " + l.Name + "; make another target group for a second load balancer")
			}
		}
		zones := map[string]bool{}
		for _, id := range s.Subnets {
			sn, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindSubnet, id)
			if err != nil || sn.Network != tg.Network {
				return ch, http.StatusBadRequest, errors.New("pick subnets in " + tg.Name + "'s network")
			}
			if zones[sn.Zone] {
				return ch, http.StatusBadRequest, errors.New("pick one subnet in each zone")
			}
			zones[sn.Zone] = true
		}
		for _, id := range s.Firewalls {
			fw, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindFirewall, id)
			if err != nil || fw.Network != tg.Network {
				return ch, http.StatusBadRequest, errors.New("pick security groups in " + tg.Name + "'s network")
			}
		}
		if a.nameTaken(ctx, m.acct.ID, in.Region, cloud.KindLoadBalancer, s.Name) {
			return ch, http.StatusConflict, errors.New("there is already a load balancer called " + s.Name + " in " + in.Region)
		}
		ch.Spec, _ = json.Marshal(s)
		ch.ResourceName = s.Name
		face := "internet-facing"
		if s.Internal {
			face = "internal"
		}
		ch.Summary = fmt.Sprintf("Make %s %s load balancer %s in %d zones, %s %d forwarding to %s (about $%.0f a month before traffic)",
			face, s.Type, s.Name, len(zones), s.ListenerProtocol(), s.Port, tg.Name, balancerMonthly[s.Type])
		return ch, http.StatusOK, nil
	}

	kind := cloud.KindTargetGroup
	if in.Action == cloud.ActionGroupSize {
		kind = cloud.KindScalingGroup
	}
	res, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, kind, in.ResourceID)
	if err != nil {
		return ch, http.StatusNotFound, err
	}
	ch.ResourceID, ch.ResourceName = res.ID, res.Name
	ch.Environment = cloudEnvOf(res.Tags, m.acct.Environment)
	if in.Action == cloud.ActionHealthCheck {
		var h cloud.HealthCheck
		if err := json.Unmarshal(in.Spec, &h); err != nil {
			return ch, http.StatusBadRequest, errors.New("a health check takes {protocol, path, port, intervalSeconds, healthy, unhealthy, matcher}")
		}
		if err := h.Check(); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if p := res.Details["protocol"]; p != "TCP" && h.Protocol == "TCP" {
			return ch, http.StatusBadRequest, errors.New("an HTTP target group checks over HTTP or HTTPS")
		}
		ch.Spec, _ = json.Marshal(h)
		ch.Summary = "Check " + res.Name + " by " + h.Words()
		return ch, http.StatusOK, nil
	}
	var g cloud.GroupSize
	if err := json.Unmarshal(in.Spec, &g); err != nil {
		return ch, http.StatusBadRequest, errors.New("a group's size is {min, max, desired}")
	}
	if err := g.Check(); err != nil {
		return ch, http.StatusBadRequest, err
	}
	now, _ := strconv.Atoi(res.Details["desired"])
	ch.Spec, _ = json.Marshal(g)
	ch.Summary = fmt.Sprintf("Size %s to %d (from %d), between %d and %d", res.Name, g.Desired, now, g.Min, g.Max)
	switch {
	case int(g.Desired) > now:
		ch.Summary += fmt.Sprintf("; AWS starts %d more", int(g.Desired)-now)
	case int(g.Desired) < now:
		ch.Summary += fmt.Sprintf("; AWS stops %d by its termination policy", now-int(g.Desired))
	}
	if stack := res.Tags["aws:cloudformation:stack-name"]; stack != "" {
		ch.Summary += " (it belongs to CloudFormation stack " + stack + ", which may change it back)"
	}
	return ch, http.StatusOK, nil
}

// nameTaken is whether the inventory has a kind of that name in a region.
func (a *API) nameTaken(ctx context.Context, acct, region string, kind cloud.Kind, name string) bool {
	items, err := a.store.CloudResources(ctx, kind)
	if err != nil {
		return false
	}
	for _, it := range items {
		if it.AccountID == acct && it.Resource.Region == region && strings.EqualFold(it.Resource.Name, name) {
			return true
		}
	}
	return false
}

func (a *API) applyBalancing(ctx context.Context, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	b, ok := m.manager.(cloud.Balancing)
	if !ok {
		return cloud.Result{}, errors.New(m.acct.Provider + " cannot make load balancers from Argus yet")
	}
	actor := cloud.Actor(ch.RequestedBy)
	tags := map[string]string{"argus:change": ch.ID, "argus:launched-by": ch.RequestedBy}
	switch ch.Action {
	case cloud.ActionTargetGroupCreate:
		var s cloud.TargetGroupSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		s.Tags = tags
		return b.CreateTargetGroup(ctx, m.target, actor, ch.Region, s)
	case cloud.ActionBalancerCreate:
		var s cloud.BalancerSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		s.Tags = tags
		res, err := b.CreateBalancer(ctx, m.target, actor, ch.Region, s)
		if err == nil {
			res.Detail = "Made " + s.Name + "; it bills from now"
		}
		return res, err
	case cloud.ActionHealthCheck:
		var h cloud.HealthCheck
		if err := json.Unmarshal(ch.Spec, &h); err != nil {
			return cloud.Result{}, err
		}
		return b.SetHealthCheck(ctx, m.target, actor, ch.Region, ch.ResourceID, h)
	}
	var g cloud.GroupSize
	if err := json.Unmarshal(ch.Spec, &g); err != nil {
		return cloud.Result{}, err
	}
	return b.SetGroupSize(ctx, m.target, actor, ch.Region, ch.ResourceID, g)
}

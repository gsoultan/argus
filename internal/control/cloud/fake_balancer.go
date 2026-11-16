package cloud

import (
	"context"
	"fmt"
	"strconv"
)

// CreateTargetGroup adds a target group with its health check, as AWS's
// read would show it.
func (f *Fake) CreateTargetGroup(_ context.Context, a Account, actor Actor, region string, s TargetGroupSpec) (Result, error) {
	if err := f.storageCall(a, ActionTargetGroupCreate, s.Name, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.made++
	arn := fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:targetgroup/%s/fake%04d", region, a.AccountID, s.Name, f.made)
	r := Resource{Kind: KindTargetGroup, ID: arn, Name: s.Name, Region: region, Network: s.Network, State: "unused",
		Size: fmt.Sprintf("%s %d", s.Protocol, s.Port), Tags: s.Tags,
		Details: map[string]string{"protocol": s.Protocol, "port": strconv.Itoa(int(s.Port)), "targetType": "instance", "targets": "0", "healthy": "0"}}
	healthDetails(r.Details, s.Health)
	if f.Resources == nil {
		f.Resources = map[string][]Resource{}
	}
	f.Resources[region] = append(f.Resources[region], r)
	return Result{ResourceID: arn, RequestID: f.request()}, nil
}

func healthDetails(d map[string]string, h HealthCheck) {
	d["healthCheckProtocol"], d["healthCheckPath"], d["healthCheckPort"] = h.Protocol, h.Path, h.Port
	d["healthCheckInterval"] = strconv.Itoa(int(h.IntervalSeconds))
	d["healthyThreshold"], d["unhealthyThreshold"] = strconv.Itoa(int(h.Healthy)), strconv.Itoa(int(h.Unhealthy))
	d["matcher"] = h.Matcher
}

// SetHealthCheck changes a target group's health check.
func (f *Fake) SetHealthCheck(_ context.Context, a Account, actor Actor, region, targetGroup string, h HealthCheck) (Result, error) {
	if err := f.storageCall(a, ActionHealthCheck, targetGroup, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.findKind(region, KindTargetGroup, targetGroup)
	if err != nil {
		return Result{}, err
	}
	if f.Resources[region][i].Details == nil {
		f.Resources[region][i].Details = map[string]string{}
	}
	healthDetails(f.Resources[region][i].Details, h)
	return Result{ResourceID: targetGroup, RequestID: f.request()}, nil
}

// CreateBalancer adds a load balancer and points the target group at it.
func (f *Fake) CreateBalancer(_ context.Context, a Account, actor Actor, region string, s BalancerSpec) (Result, error) {
	if err := f.storageCall(a, ActionBalancerCreate, s.Name, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.findKind(region, KindTargetGroup, s.TargetGroup)
	if err != nil {
		return Result{}, err
	}
	f.made++
	kind := "app"
	if s.Type == "network" {
		kind = "net"
	}
	arn := fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:loadbalancer/%s/%s/fake%04d", region, a.AccountID, kind, s.Name, f.made)
	scheme := "internet-facing"
	if s.Internal {
		scheme = "internal"
	}
	r := Resource{Kind: KindLoadBalancer, ID: arn, Name: s.Name, Region: region, Size: s.Type, State: "provisioning", Tags: s.Tags,
		Network: f.Resources[region][t].Network,
		Details: map[string]string{"scheme": scheme, "dnsName": s.Name + ".elb.example"},
		Links:   []Ref{{Kind: KindTargetGroup, ID: s.TargetGroup, Name: f.Resources[region][t].Name}}}
	for _, g := range s.Firewalls {
		r.Links = append(r.Links, Ref{Kind: KindFirewall, ID: g})
	}
	for _, sn := range s.Subnets {
		r.Links = append(r.Links, Ref{Kind: KindSubnet, ID: sn})
	}
	f.Resources[region] = append(f.Resources[region], r)
	f.link(region, t, Ref{Kind: KindLoadBalancer, ID: arn, Name: s.Name})
	if f.Resources[region][t].State == "unused" {
		f.Resources[region][t].State = "empty"
	}
	return Result{ResourceID: arn, RequestID: f.request()}, nil
}

// SetGroupSize sets an Auto Scaling group's size.
func (f *Fake) SetGroupSize(_ context.Context, a Account, actor Actor, region, group string, g GroupSize) (Result, error) {
	if err := f.storageCall(a, ActionGroupSize, group, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.findKind(region, KindScalingGroup, group)
	if err != nil {
		return Result{}, err
	}
	d := f.Resources[region][i].Details
	d["min"], d["max"], d["desired"] = strconv.Itoa(int(g.Min)), strconv.Itoa(int(g.Max)), strconv.Itoa(int(g.Desired))
	f.Resources[region][i].Size = d["desired"]
	return Result{ResourceID: group, RequestID: f.request()}, nil
}

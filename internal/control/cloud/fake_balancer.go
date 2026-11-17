package cloud

import (
	"context"
	"fmt"
	"strconv"
	"time"
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

// Floors the fake made and the policies on them, by group name.
type FakeFloor struct {
	Spec   FloorSpec
	Policy *FloorPolicy
}

// CreateFloor adds an Auto Scaling group with its launch template, as
// AWS's read would show it. It starts no servers: ScaleOut does, as AWS
// would.
func (f *Fake) CreateFloor(_ context.Context, a Account, actor Actor, region string, s FloorSpec) (Result, error) {
	if err := f.storageCall(a, ActionFloorCreate, s.Name, actor); err != nil {
		return Result{}, err
	}
	if err := s.Check(); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Floors == nil {
		f.Floors = map[string]*FakeFloor{}
	}
	if _, ok := f.Floors[s.Name]; ok {
		return Result{}, fmt.Errorf("AlreadyExists: an Auto Scaling group named %s already exists", s.Name)
	}
	f.made++
	tpl := fmt.Sprintf("lt-0fake%012d", f.made)
	f.Floors[s.Name] = &FakeFloor{Spec: s}
	if f.Resources == nil {
		f.Resources = map[string][]Resource{}
	}
	d := map[string]string{"min": strconv.Itoa(int(s.Count.Min)), "max": strconv.Itoa(int(s.Count.Max)),
		"desired": strconv.Itoa(int(s.Count.Desired)), "running": "0", "healthCheck": "ELB", "launchTemplate": s.Template}
	f.Resources[region] = append(f.Resources[region], Resource{Kind: KindScalingGroup, ID: s.Name, Name: s.Name, Region: region,
		State: "active", Size: d["desired"], Tags: s.Tags, Details: d,
		Links: []Ref{{Kind: KindTargetGroup, ID: s.TargetGroup}}})
	return Result{ResourceID: s.Name, Detail: tpl, RequestID: f.request()}, nil
}

// SetFloorPolicy records the target-tracking policy on a group.
func (f *Fake) SetFloorPolicy(_ context.Context, a Account, actor Actor, region, group string, p FloorPolicy) (Result, error) {
	if err := f.storageCall(a, ActionFloorPolicy, group, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fl := f.Floors[group]
	if fl == nil {
		return Result{}, fmt.Errorf("ValidationError: no Auto Scaling group named %s", group)
	}
	fl.Policy = &p
	return Result{ResourceID: group, RequestID: f.request()}, nil
}

// FloorInstance reads one server's Auto Scaling group and start.
func (f *Fake) FloorInstance(_ context.Context, a Account, _ Actor, region, instance string) (FloorInstance, error) {
	if err := f.admit(a); err != nil {
		return FloorInstance{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.find(region, instance)
	if err != nil {
		return FloorInstance{}, err
	}
	r := f.Resources[region][i]
	at := time.Now()
	if r.CreatedAt != nil {
		at = *r.CreatedAt
	}
	return FloorInstance{Group: r.Tags["aws:autoscaling:groupName"], State: r.State, LaunchedAt: at}, nil
}

// ScaleOut starts one server in an Auto Scaling group the fake made, as
// AWS does to keep its desired count, and answers its id.
func (f *Fake) ScaleOut(region, group string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl := f.Floors[group]
	f.launched++
	id := fmt.Sprintf("i-0fa4e%012d", f.launched)
	now := time.Now()
	tags := map[string]string{"aws:autoscaling:groupName": group}
	size := ""
	if fl != nil {
		size = fl.Spec.Size
		for k, v := range fl.Spec.Tags {
			tags[k] = v
		}
		f.LastUserData = fl.Spec.UserData
	}
	f.Resources[region] = append(f.Resources[region], Resource{Kind: KindInstance, ID: id, Name: id, Region: region,
		State: "running", Size: size, Tags: tags, CreatedAt: &now, Details: map[string]string{}})
	return id
}

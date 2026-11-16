package aws

import (
	"context"
	"errors"
	"strconv"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Making target groups and load balancers, changing a health check, and sizing
an Auto Scaling group, through the manage role as the person who asked; and
reading Auto Scaling groups with the inventory, through the read role.
*/

// scalingGroups reads a region's Auto Scaling groups.
func scalingGroups(ctx context.Context, cfg awssdk.Config, region string) ([]cloud.Resource, error) {
	c := autoscaling.NewFromConfig(cfg)
	var out []cloud.Resource
	pg := autoscaling.NewDescribeAutoScalingGroupsPaginator(c, &autoscaling.DescribeAutoScalingGroupsInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range page.AutoScalingGroups {
			out = append(out, ScalingGroup(g, region))
		}
	}
	return out, nil
}

// ScalingGroup maps one Auto Scaling group. Its id is its name, which every
// call about it takes; its instances and target groups are links.
func ScalingGroup(g astypes.AutoScalingGroup, region string) cloud.Resource {
	r := cloud.Resource{Kind: cloud.KindScalingGroup, ID: awssdk.ToString(g.AutoScalingGroupName), Name: awssdk.ToString(g.AutoScalingGroupName),
		Region: region, CreatedAt: g.CreatedTime, State: "active", Tags: map[string]string{}, Details: map[string]string{}}
	if s := awssdk.ToString(g.Status); s != "" {
		r.State = strings.ToLower(s)
	}
	for _, t := range g.Tags {
		r.Tags[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
	min, max, want := awssdk.ToInt32(g.MinSize), awssdk.ToInt32(g.MaxSize), awssdk.ToInt32(g.DesiredCapacity)
	r.Size = strconv.Itoa(int(want))
	set(r.Details, "min", strconv.Itoa(int(min)))
	set(r.Details, "max", strconv.Itoa(int(max)))
	set(r.Details, "desired", strconv.Itoa(int(want)))
	set(r.Details, "running", strconv.Itoa(len(g.Instances)))
	set(r.Details, "healthCheck", awssdk.ToString(g.HealthCheckType))
	if lt := g.LaunchTemplate; lt != nil {
		set(r.Details, "launchTemplate", awssdk.ToString(lt.LaunchTemplateName))
	} else if g.MixedInstancesPolicy != nil && g.MixedInstancesPolicy.LaunchTemplate != nil &&
		g.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification != nil {
		set(r.Details, "launchTemplate", awssdk.ToString(g.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification.LaunchTemplateName))
	}
	set(r.Details, "launchConfiguration", awssdk.ToString(g.LaunchConfigurationName))
	if len(g.SuspendedProcesses) > 0 {
		set(r.Details, "suspended", strconv.Itoa(len(g.SuspendedProcesses)))
	}
	for _, i := range g.Instances {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindInstance, ID: awssdk.ToString(i.InstanceId), Name: string(i.LifecycleState)})
	}
	for _, t := range g.TargetGroupARNs {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindTargetGroup, ID: t})
	}
	if z := awssdk.ToString(g.VPCZoneIdentifier); z != "" {
		for _, s := range strings.Split(z, ",") {
			r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindSubnet, ID: strings.TrimSpace(s)})
		}
	}
	return r
}

func (p *Provider) elbClient(ctx context.Context, a cloud.Account, actor cloud.Actor, region string) (*elb.Client, error) {
	cfg, err := p.assumed(ctx, a, actor, region)
	if err != nil {
		return nil, err
	}
	return elb.NewFromConfig(cfg), nil
}

func elbTagList(name string, tags map[string]string) []elbtypes.Tag {
	out := []elbtypes.Tag{{Key: awssdk.String("Name"), Value: awssdk.String(name)}}
	for k, v := range tags {
		out = append(out, elbtypes.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return out
}

// healthInput is h as ModifyTargetGroup and CreateTargetGroup take it.
func healthInput(h cloud.HealthCheck) (proto elbtypes.ProtocolEnum, path, port *string, interval, healthy, unhealthy *int32, matcher *elbtypes.Matcher) {
	proto = elbtypes.ProtocolEnum(h.Protocol)
	if h.Path != "" {
		path = awssdk.String(h.Path)
	}
	port = awssdk.String(h.Port)
	interval, healthy, unhealthy = awssdk.Int32(h.IntervalSeconds), awssdk.Int32(h.Healthy), awssdk.Int32(h.Unhealthy)
	if h.Matcher != "" {
		matcher = &elbtypes.Matcher{HttpCode: awssdk.String(h.Matcher)}
	}
	return
}

// CreateTargetGroup makes a target group of instances.
func (p *Provider) CreateTargetGroup(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, s cloud.TargetGroupSpec) (cloud.Result, error) {
	c, err := p.elbClient(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	hp, path, port, interval, healthy, unhealthy, matcher := healthInput(s.Health)
	out, err := c.CreateTargetGroup(ctx, &elb.CreateTargetGroupInput{
		Name: awssdk.String(s.Name), Protocol: elbtypes.ProtocolEnum(s.Protocol), Port: awssdk.Int32(s.Port),
		VpcId: awssdk.String(s.Network), TargetType: elbtypes.TargetTypeEnumInstance,
		HealthCheckEnabled: awssdk.Bool(true), HealthCheckProtocol: hp, HealthCheckPath: path, HealthCheckPort: port,
		HealthCheckIntervalSeconds: interval, HealthyThresholdCount: healthy, UnhealthyThresholdCount: unhealthy, Matcher: matcher,
		Tags: elbTagList(s.Name, s.Tags),
	})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	id := ""
	if len(out.TargetGroups) > 0 {
		id = awssdk.ToString(out.TargetGroups[0].TargetGroupArn)
	}
	return cloud.Result{ResourceID: id, RequestID: requestID(out.ResultMetadata)}, nil
}

// SetHealthCheck changes a target group's health check.
func (p *Provider) SetHealthCheck(ctx context.Context, a cloud.Account, actor cloud.Actor, region, targetGroup string, h cloud.HealthCheck) (cloud.Result, error) {
	c, err := p.elbClient(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	hp, path, port, interval, healthy, unhealthy, matcher := healthInput(h)
	out, err := c.ModifyTargetGroup(ctx, &elb.ModifyTargetGroupInput{TargetGroupArn: awssdk.String(targetGroup),
		HealthCheckEnabled: awssdk.Bool(true), HealthCheckProtocol: hp, HealthCheckPath: path, HealthCheckPort: port,
		HealthCheckIntervalSeconds: interval, HealthyThresholdCount: healthy, UnhealthyThresholdCount: unhealthy, Matcher: matcher})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: targetGroup, RequestID: requestID(out.ResultMetadata)}, nil
}

// CreateBalancer makes a load balancer and its one listener. If the
// listener fails, the load balancer stays, and the error says so: it bills
// from now, and the person decides whether to keep it.
func (p *Provider) CreateBalancer(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, s cloud.BalancerSpec) (cloud.Result, error) {
	c, err := p.elbClient(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	in := &elb.CreateLoadBalancerInput{Name: awssdk.String(s.Name), Subnets: s.Subnets, Type: elbtypes.LoadBalancerTypeEnum(s.Type),
		Scheme: elbtypes.LoadBalancerSchemeEnumInternetFacing, Tags: elbTagList(s.Name, s.Tags)}
	if s.Internal {
		in.Scheme = elbtypes.LoadBalancerSchemeEnumInternal
	}
	if len(s.Firewalls) > 0 {
		in.SecurityGroups = s.Firewalls
	}
	out, err := c.CreateLoadBalancer(ctx, in)
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	if len(out.LoadBalancers) == 0 {
		return cloud.Result{}, errors.New("AWS made no load balancer")
	}
	arn := awssdk.ToString(out.LoadBalancers[0].LoadBalancerArn)
	res := cloud.Result{ResourceID: arn, RequestID: requestID(out.ResultMetadata)}
	if _, err := c.CreateListener(ctx, &elb.CreateListenerInput{LoadBalancerArn: awssdk.String(arn),
		Protocol: elbtypes.ProtocolEnum(s.ListenerProtocol()), Port: awssdk.Int32(s.Port),
		DefaultActions: []elbtypes.Action{{Type: elbtypes.ActionTypeEnumForward, TargetGroupArn: awssdk.String(s.TargetGroup)}},
	}); err != nil {
		return res, errors.New("made the load balancer " + s.Name + ", but not its listener: " + plain(err) +
			". It bills from now; add the listener in AWS or remove it there")
	}
	return res, nil
}

// SetGroupSize sets an Auto Scaling group's minimum, maximum and desired
// size.
func (p *Provider) SetGroupSize(ctx context.Context, a cloud.Account, actor cloud.Actor, region, group string, g cloud.GroupSize) (cloud.Result, error) {
	cfg, err := p.assumed(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := autoscaling.NewFromConfig(cfg).UpdateAutoScalingGroup(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: awssdk.String(group), MinSize: awssdk.Int32(g.Min), MaxSize: awssdk.Int32(g.Max),
		DesiredCapacity: awssdk.Int32(g.Desired)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: group, RequestID: requestID(out.ResultMetadata)}, nil
}

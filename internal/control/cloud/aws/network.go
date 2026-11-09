package aws

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Security groups and load balancer targets, through the manage role as the
person who asked; and the load balancers and target groups the inventory
reads, through the read role. Application, network and gateway load
balancers only (elbv2): classic load balancers are read by nothing yet.
*/

// balancing reads a region's load balancers and target groups together,
// so each can name the other.
func balancing(ctx context.Context, cfg awssdk.Config, region string) (lbs, tgs []cloud.Resource, err error) {
	c := elb.NewFromConfig(cfg)
	var raw []elbtypes.LoadBalancer
	pl := elb.NewDescribeLoadBalancersPaginator(c, &elb.DescribeLoadBalancersInput{})
	for pl.HasMorePages() {
		page, err := pl.NextPage(ctx)
		if err != nil {
			return nil, nil, err
		}
		raw = append(raw, page.LoadBalancers...)
	}
	var groups []elbtypes.TargetGroup
	pt := elb.NewDescribeTargetGroupsPaginator(c, &elb.DescribeTargetGroupsInput{})
	for pt.HasMorePages() {
		page, err := pt.NextPage(ctx)
		if err != nil {
			return nil, nil, err
		}
		groups = append(groups, page.TargetGroups...)
	}
	var arns []string
	for _, l := range raw {
		arns = append(arns, awssdk.ToString(l.LoadBalancerArn))
	}
	for _, g := range groups {
		arns = append(arns, awssdk.ToString(g.TargetGroupArn))
	}
	tags, err := elbTags(ctx, c, arns)
	if err != nil {
		return nil, nil, err
	}
	lbName := map[string]string{}
	for _, l := range raw {
		r := LoadBalancer(l, region, tags[awssdk.ToString(l.LoadBalancerArn)])
		lbName[r.ID] = r.Name
		lbs = append(lbs, r)
	}
	for _, g := range groups {
		arn := awssdk.ToString(g.TargetGroupArn)
		h, err := c.DescribeTargetHealth(ctx, &elb.DescribeTargetHealthInput{TargetGroupArn: g.TargetGroupArn})
		if err != nil {
			return nil, nil, err
		}
		r := TargetGroup(g, region, tags[arn], h.TargetHealthDescriptions, lbName)
		tgs = append(tgs, r)
		for i := range lbs {
			for _, l := range g.LoadBalancerArns {
				if lbs[i].ID == l {
					lbs[i].Links = append(lbs[i].Links, cloud.Ref{Kind: cloud.KindTargetGroup, ID: arn, Name: r.Name})
				}
			}
		}
	}
	return lbs, tgs, nil
}

// elbTags reads tags 20 at a time, the most DescribeTags takes.
func elbTags(ctx context.Context, c *elb.Client, arns []string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for len(arns) > 0 {
		n := min(20, len(arns))
		page, err := c.DescribeTags(ctx, &elb.DescribeTagsInput{ResourceArns: arns[:n]})
		if err != nil {
			return nil, err
		}
		for _, d := range page.TagDescriptions {
			m := map[string]string{}
			for _, t := range d.Tags {
				m[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
			}
			out[awssdk.ToString(d.ResourceArn)] = m
		}
		arns = arns[n:]
	}
	return out, nil
}

// LoadBalancer maps one application, network or gateway load balancer.
// Its id is its ARN, which every call about it takes.
func LoadBalancer(l elbtypes.LoadBalancer, region string, tags map[string]string) cloud.Resource {
	r := cloud.Resource{Kind: cloud.KindLoadBalancer, ID: awssdk.ToString(l.LoadBalancerArn), Name: awssdk.ToString(l.LoadBalancerName),
		Region: region, Size: string(l.Type), Network: awssdk.ToString(l.VpcId), CreatedAt: l.CreatedTime, Tags: tags,
		Details: map[string]string{}}
	if l.State != nil {
		r.State = string(l.State.Code)
	}
	for _, g := range l.SecurityGroups {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindFirewall, ID: g})
	}
	for _, z := range l.AvailabilityZones {
		if s := awssdk.ToString(z.SubnetId); s != "" {
			r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindSubnet, ID: s, Name: awssdk.ToString(z.ZoneName)})
		}
	}
	set(r.Details, "dnsName", awssdk.ToString(l.DNSName))
	set(r.Details, "scheme", string(l.Scheme))
	set(r.Details, "ipAddressType", string(l.IpAddressType))
	if l.Scheme == elbtypes.LoadBalancerSchemeEnumInternetFacing {
		r.PublicIP = awssdk.ToString(l.DNSName)
	}
	return r
}

// TargetGroup maps one target group. Each instance target is a link whose
// name is its health: healthy, unhealthy, initial, draining, unused.
func TargetGroup(g elbtypes.TargetGroup, region string, tags map[string]string, health []elbtypes.TargetHealthDescription,
	lbName map[string]string) cloud.Resource {
	r := cloud.Resource{Kind: cloud.KindTargetGroup, ID: awssdk.ToString(g.TargetGroupArn), Name: awssdk.ToString(g.TargetGroupName),
		Region: region, Network: awssdk.ToString(g.VpcId), Tags: tags, Details: map[string]string{}}
	if g.Port != nil {
		r.Size = fmt.Sprintf("%s %d", g.Protocol, *g.Port)
		set(r.Details, "port", strconv.Itoa(int(*g.Port)))
	} else {
		r.Size = string(g.Protocol)
	}
	set(r.Details, "protocol", string(g.Protocol))
	set(r.Details, "targetType", string(g.TargetType))
	set(r.Details, "healthCheckPath", awssdk.ToString(g.HealthCheckPath))
	set(r.Details, "healthCheckProtocol", string(g.HealthCheckProtocol))
	for _, l := range g.LoadBalancerArns {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindLoadBalancer, ID: l, Name: lbName[l]})
	}
	healthy := 0
	for _, h := range health {
		state := "unknown"
		if h.TargetHealth != nil {
			state = string(h.TargetHealth.State)
		}
		if state == "healthy" {
			healthy++
		}
		if h.Target == nil {
			continue
		}
		id := awssdk.ToString(h.Target.Id)
		if g.TargetType == elbtypes.TargetTypeEnumInstance {
			r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindInstance, ID: id, Name: state})
		}
	}
	set(r.Details, "targets", strconv.Itoa(len(health)))
	set(r.Details, "healthy", strconv.Itoa(healthy))
	switch {
	case len(g.LoadBalancerArns) == 0:
		r.State = "unused"
	case len(health) == 0:
		r.State = "empty"
	case healthy == len(health):
		r.State = "healthy"
	default:
		r.State = "degraded"
	}
	return r
}

// permission turns a neutral rule into AWS's shape. Descriptions go only
// on a rule being added; AWS matches a revoke without one.
func permission(r cloud.Rule, withDesc bool) types.IpPermission {
	p := types.IpPermission{IpProtocol: awssdk.String(r.Protocol)}
	switch r.Protocol {
	case "all":
		p.IpProtocol = awssdk.String("-1")
	default:
		p.FromPort, p.ToPort = awssdk.Int32(r.FromPort), awssdk.Int32(r.ToPort)
	}
	var desc *string
	if withDesc && r.Description != "" {
		desc = awssdk.String(r.Description)
	}
	switch {
	case cloud.IsFirewallRef(r.Source):
		p.UserIdGroupPairs = []types.UserIdGroupPair{{GroupId: awssdk.String(r.Source), Description: desc}}
	case strings.HasPrefix(r.Source, "pl-"):
		p.PrefixListIds = []types.PrefixListId{{PrefixListId: awssdk.String(r.Source), Description: desc}}
	case r.IPv6():
		p.Ipv6Ranges = []types.Ipv6Range{{CidrIpv6: awssdk.String(r.Source), Description: desc}}
	default:
		p.IpRanges = []types.IpRange{{CidrIp: awssdk.String(r.Source), Description: desc}}
	}
	return p
}

func permissions(rules []cloud.Rule, inbound, withDesc bool) []types.IpPermission {
	var out []types.IpPermission
	for _, r := range rules {
		if r.Inbound == inbound {
			out = append(out, permission(r, withDesc))
		}
	}
	return out
}

// CreateFirewall makes a security group with its inbound rules. AWS gives
// every new group one outbound rule, all traffic anywhere; Argus leaves it.
func (p *Provider) CreateFirewall(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, s cloud.FirewallSpec) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: awssdk.String(s.Name),
		Description: awssdk.String(s.Description), VpcId: awssdk.String(s.Network),
		TagSpecifications: nameTags(s.Name, string(actor), s.Tags, types.ResourceTypeSecurityGroup)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	id := awssdk.ToString(out.GroupId)
	r := cloud.Result{ResourceID: id, RequestID: requestID(out.ResultMetadata)}
	if in := permissions(s.Rules, true, true); len(in) > 0 {
		if _, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: out.GroupId, IpPermissions: in}); err != nil {
			return r, fmt.Errorf("made %s, but adding its rules failed; add them from its panel: %s", id, plain(err))
		}
	}
	if eg := permissions(s.Rules, false, true); len(eg) > 0 {
		if _, err := c.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{GroupId: out.GroupId, IpPermissions: eg}); err != nil {
			return r, fmt.Errorf("made %s, but adding its outbound rules failed; add them from its panel: %s", id, plain(err))
		}
	}
	return r, nil
}

// ChangeRules takes rules away, then adds. If adding fails after rules
// were taken away, it says so: the group is then narrower than before,
// never wider.
func (p *Provider) ChangeRules(ctx context.Context, a cloud.Account, actor cloud.Actor, region, firewall string, add, remove []cloud.Rule) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	g := awssdk.String(firewall)
	r := cloud.Result{ResourceID: firewall}
	var ids []string
	if x := permissions(remove, true, false); len(x) > 0 {
		out, err := c.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: g, IpPermissions: x})
		if err != nil {
			return r, errors.New(plain(err))
		}
		if len(out.UnknownIpPermissions) > 0 {
			return r, errors.New("AWS did not find some of the inbound rules to take away; read the group again and retry")
		}
		ids = append(ids, requestID(out.ResultMetadata))
	}
	if x := permissions(remove, false, false); len(x) > 0 {
		out, err := c.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{GroupId: g, IpPermissions: x})
		if err != nil {
			return r, partial(ids, plain(err))
		}
		if len(out.UnknownIpPermissions) > 0 {
			return r, partial(ids, "AWS did not find some of the outbound rules to take away")
		}
		ids = append(ids, requestID(out.ResultMetadata))
	}
	if x := permissions(add, true, true); len(x) > 0 {
		out, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: g, IpPermissions: x})
		if err != nil {
			return r, partial(ids, plain(err))
		}
		ids = append(ids, requestID(out.ResultMetadata))
	}
	if x := permissions(add, false, true); len(x) > 0 {
		out, err := c.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{GroupId: g, IpPermissions: x})
		if err != nil {
			return r, partial(ids, plain(err))
		}
		ids = append(ids, requestID(out.ResultMetadata))
	}
	r.RequestID = strings.Join(ids, ", ")
	return r, nil
}

func partial(done []string, cause string) error {
	if len(done) == 0 {
		return errors.New(cause)
	}
	return fmt.Errorf("%s. Part of the change was made first (AWS request %s): the rules taken away are gone, so the group allows less than before, not more",
		cause, strings.Join(done, ", "))
}

// DeleteFirewall deletes a security group. AWS refuses while anything uses
// it or another group's rule names it.
func (p *Provider) DeleteFirewall(ctx context.Context, a cloud.Account, actor cloud.Actor, region, firewall string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: awssdk.String(firewall)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: firewall, RequestID: requestID(out.ResultMetadata)}, nil
}

// SetInstanceFirewalls replaces the security groups on an instance's
// primary network interface.
func (p *Provider) SetInstanceFirewalls(ctx context.Context, a cloud.Account, actor cloud.Actor, region, instance string, firewalls []string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: awssdk.String(instance), Groups: firewalls})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: instance, RequestID: requestID(out.ResultMetadata)}, nil
}

func (p *Provider) elb(ctx context.Context, a cloud.Account, actor cloud.Actor, region string) (*elb.Client, error) {
	cfg, err := p.assumed(ctx, a, actor, region)
	if err != nil {
		return nil, err
	}
	return elb.NewFromConfig(cfg), nil
}

func target(instance string, port int32) []elbtypes.TargetDescription {
	t := elbtypes.TargetDescription{Id: awssdk.String(instance)}
	if port > 0 {
		t.Port = awssdk.Int32(port)
	}
	return []elbtypes.TargetDescription{t}
}

// RegisterTarget puts an instance behind a target group. The load
// balancer sends it traffic once its health check passes.
func (p *Provider) RegisterTarget(ctx context.Context, a cloud.Account, actor cloud.Actor, region, targetGroup, instance string, port int32) (cloud.Result, error) {
	c, err := p.elb(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.RegisterTargets(ctx, &elb.RegisterTargetsInput{TargetGroupArn: awssdk.String(targetGroup), Targets: target(instance, port)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: targetGroup, RequestID: requestID(out.ResultMetadata)}, nil
}

// DeregisterTarget takes an instance out. The load balancer drains its
// open connections first, for the group's deregistration delay.
func (p *Provider) DeregisterTarget(ctx context.Context, a cloud.Account, actor cloud.Actor, region, targetGroup, instance string, port int32) (cloud.Result, error) {
	c, err := p.elb(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.DeregisterTargets(ctx, &elb.DeregisterTargetsInput{TargetGroupArn: awssdk.String(targetGroup), Targets: target(instance, port)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: targetGroup, RequestID: requestID(out.ResultMetadata)}, nil
}

var _ cloud.Network = (*Provider)(nil)

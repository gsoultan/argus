package aws

import (
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestPermissionShapes(t *testing.T) {
	all := permission(cloud.Rule{Inbound: true, Protocol: "all", FromPort: 0, ToPort: 65535, Source: "10.0.0.0/8"}, true)
	if awssdk.ToString(all.IpProtocol) != "-1" || all.FromPort != nil || len(all.IpRanges) != 1 {
		t.Fatalf("all: %+v", all)
	}
	sg := permission(cloud.Rule{Inbound: true, Protocol: "tcp", FromPort: 5432, ToPort: 5432, Source: "sg-0123456789abcdef0", Description: "app"}, true)
	if len(sg.UserIdGroupPairs) != 1 || awssdk.ToString(sg.UserIdGroupPairs[0].Description) != "app" || awssdk.ToInt32(sg.ToPort) != 5432 {
		t.Fatalf("group source: %+v", sg)
	}
	v6 := permission(cloud.Rule{Inbound: true, Protocol: "tcp", FromPort: 443, ToPort: 443, Source: "::/0", Description: "web"}, false)
	if len(v6.Ipv6Ranges) != 1 || v6.Ipv6Ranges[0].Description != nil {
		t.Fatalf("a revoke carries no description: %+v", v6)
	}
	if got := permissions([]cloud.Rule{{Inbound: true, Protocol: "tcp", Source: "1.2.3.4/32"}, {Protocol: "all", Source: "0.0.0.0/0"}}, false, true); len(got) != 1 {
		t.Fatalf("outbound only: %+v", got)
	}
}

func TestTargetGroupHealth(t *testing.T) {
	g := elbtypes.TargetGroup{TargetGroupArn: awssdk.String("arn:tg/web"), TargetGroupName: awssdk.String("web"), Port: awssdk.Int32(8080),
		Protocol: elbtypes.ProtocolEnumHttp, TargetType: elbtypes.TargetTypeEnumInstance, LoadBalancerArns: []string{"arn:lb/front"}}
	h := []elbtypes.TargetHealthDescription{
		{Target: &elbtypes.TargetDescription{Id: awssdk.String("i-1")}, TargetHealth: &elbtypes.TargetHealth{State: elbtypes.TargetHealthStateEnumHealthy}},
		{Target: &elbtypes.TargetDescription{Id: awssdk.String("i-2")}, TargetHealth: &elbtypes.TargetHealth{State: elbtypes.TargetHealthStateEnumUnhealthy}},
	}
	r := TargetGroup(g, "ap-southeast-3", nil, h, map[string]string{"arn:lb/front": "front"})
	if r.State != "degraded" || r.Size != "HTTP 8080" || r.Details["healthy"] != "1" || len(r.Links) != 3 {
		t.Fatalf("target group: %+v", r)
	}
	if r.Links[0].Kind != cloud.KindLoadBalancer || r.Links[0].Name != "front" || r.Links[2].Name != "unhealthy" {
		t.Fatalf("links: %+v", r.Links)
	}
	g.LoadBalancerArns = nil
	if TargetGroup(g, "x", nil, nil, nil).State != "unused" {
		t.Fatal("a group no load balancer uses is unused")
	}
}

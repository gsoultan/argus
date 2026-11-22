package aws

import (
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestVPNConnectionOneTunnelDown(t *testing.T) {
	v := types.VpnConnection{
		VpnConnectionId:   awssdk.String("vpn-1"),
		CustomerGatewayId: awssdk.String("cgw-1"),
		VpnGatewayId:      awssdk.String("vgw-1"),
		State:             types.VpnStateAvailable,
		Type:              types.GatewayTypeIpsec1,
		// The keys live here; the mapping must never copy them.
		CustomerGatewayConfiguration: awssdk.String("<pre_shared_key>secret</pre_shared_key>"),
		Options:                      &types.VpnConnectionOptions{StaticRoutesOnly: awssdk.Bool(true)},
		Routes:                       []types.VpnStaticRoute{{DestinationCidrBlock: awssdk.String("10.1.0.0/16")}},
		VgwTelemetry: []types.VgwTelemetry{
			{OutsideIpAddress: awssdk.String("3.3.3.1"), Status: types.TelemetryStatusUp},
			{OutsideIpAddress: awssdk.String("3.3.3.2"), Status: types.TelemetryStatusDown, StatusMessage: awssdk.String("IPSEC IS DOWN")},
		},
	}
	r := VPNConnection(v, types.CustomerGateway{IpAddress: awssdk.String("203.0.113.9")}, "ap-southeast-3")
	if r.State != cloud.VPNPartial || r.Details[cloud.DetailTunnelsUp] != "1" || r.Details[cloud.DetailTunnelsTotal] != "2" {
		t.Fatalf("state %q details %v", r.State, r.Details)
	}
	if r.Details[cloud.DetailRemoteAddress] != "203.0.113.9" || r.Details[cloud.DetailRouting] != "static" ||
		r.Details[cloud.DetailRemoteNetworks] != "10.1.0.0/16" || r.Details[cloud.DetailGateway] != "vgw-1" {
		t.Fatalf("details %v", r.Details)
	}
	for k, d := range r.Details {
		if d == "secret" || k == "customerGatewayConfiguration" {
			t.Fatalf("key copied into %s", k)
		}
	}
}

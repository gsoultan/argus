package aws

import (
	"context"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// vpnConnections reads the region's Site-to-Site VPN connections with each
// tunnel's state, and the customer gateways for the far end's address.
// CustomerGatewayConfiguration holds the tunnels' pre-shared keys and is
// never read into the resource.
func vpnConnections(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	out, err := c.DescribeVpnConnections(ctx, &ec2.DescribeVpnConnectionsInput{})
	if err != nil {
		return nil, err
	}
	cgws := map[string]types.CustomerGateway{}
	if len(out.VpnConnections) > 0 {
		if g, err := c.DescribeCustomerGateways(ctx, &ec2.DescribeCustomerGatewaysInput{}); err == nil {
			for _, x := range g.CustomerGateways {
				cgws[awssdk.ToString(x.CustomerGatewayId)] = x
			}
		}
	}
	var res []cloud.Resource
	for _, v := range out.VpnConnections {
		if v.State == types.VpnStateDeleted {
			continue
		}
		res = append(res, VPNConnection(v, cgws[awssdk.ToString(v.CustomerGatewayId)], region))
	}
	return res, nil
}

// VPNConnection maps one Site-to-Site VPN connection.
func VPNConnection(v types.VpnConnection, cgw types.CustomerGateway, region string) cloud.Resource {
	id := awssdk.ToString(v.VpnConnectionId)
	tags := tagMap(v.Tags)
	r := cloud.Resource{Kind: cloud.KindVPN, ID: id, Name: cloud.Name(tags, id), Region: region,
		State: string(v.State), Tags: tags, Details: map[string]string{}}
	gw := awssdk.ToString(v.VpnGatewayId)
	if gw == "" {
		gw = awssdk.ToString(v.TransitGatewayId)
	}
	set(r.Details, cloud.DetailGateway, gw)
	set(r.Details, cloud.DetailVPNType, string(v.Type))
	set(r.Details, cloud.DetailRemoteAddress, awssdk.ToString(cgw.IpAddress))
	set(r.Details, "customerGateway", awssdk.ToString(v.CustomerGatewayId))
	routing := "bgp"
	var nets []string
	if v.Options != nil && awssdk.ToBool(v.Options.StaticRoutesOnly) {
		routing = "static"
	}
	for _, rt := range v.Routes {
		if rt.State != types.VpnStateDeleted {
			nets = append(nets, awssdk.ToString(rt.DestinationCidrBlock))
		}
	}
	if len(nets) == 0 && v.Options != nil {
		if n := awssdk.ToString(v.Options.RemoteIpv4NetworkCidr); n != "" && n != "0.0.0.0/0" {
			nets = append(nets, n)
		}
	}
	set(r.Details, cloud.DetailRouting, routing)
	set(r.Details, cloud.DetailRemoteNetworks, strings.Join(nets, ","))
	if v.State != types.VpnStateAvailable {
		return r // pending or deleting: the tunnels say nothing yet
	}
	ts := make([]cloud.Tunnel, 0, len(v.VgwTelemetry))
	for _, t := range v.VgwTelemetry {
		ts = append(ts, cloud.Tunnel{Address: awssdk.ToString(t.OutsideIpAddress), Up: t.Status == types.TelemetryStatusUp,
			Why: awssdk.ToString(t.StatusMessage)})
	}
	cloud.SetTunnels(&r, ts)
	return r
}

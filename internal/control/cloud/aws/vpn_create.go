package aws

import (
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// vpnAPI is the part of EC2 that making a VPN connection calls.
type vpnAPI interface {
	DescribeCustomerGateways(context.Context, *ec2.DescribeCustomerGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeCustomerGatewaysOutput, error)
	CreateCustomerGateway(context.Context, *ec2.CreateCustomerGatewayInput, ...func(*ec2.Options)) (*ec2.CreateCustomerGatewayOutput, error)
	DescribeVpnGateways(context.Context, *ec2.DescribeVpnGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeVpnGatewaysOutput, error)
	CreateVpnGateway(context.Context, *ec2.CreateVpnGatewayInput, ...func(*ec2.Options)) (*ec2.CreateVpnGatewayOutput, error)
	AttachVpnGateway(context.Context, *ec2.AttachVpnGatewayInput, ...func(*ec2.Options)) (*ec2.AttachVpnGatewayOutput, error)
	CreateVpnConnection(context.Context, *ec2.CreateVpnConnectionInput, ...func(*ec2.Options)) (*ec2.CreateVpnConnectionOutput, error)
	CreateVpnConnectionRoute(context.Context, *ec2.CreateVpnConnectionRouteInput, ...func(*ec2.Options)) (*ec2.CreateVpnConnectionRouteOutput, error)
	DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
	EnableVgwRoutePropagation(context.Context, *ec2.EnableVgwRoutePropagationInput, ...func(*ec2.Options)) (*ec2.EnableVgwRoutePropagationOutput, error)
}

// CreateVPN makes a Site-to-Site VPN connection for an IPsec link: see
// cloud/vpn_create.go.
func (p *Provider) CreateVPN(ctx context.Context, a cloud.Account, actor cloud.Actor, s cloud.VPNSpec) (cloud.VPNMade, error) {
	if err := s.Check(); err != nil {
		return cloud.VPNMade{}, err
	}
	if s.PSK == "" {
		return cloud.VPNMade{}, errors.New("no pre-shared key for the connection")
	}
	cfg, err := p.assumed(ctx, a, actor, s.Region)
	if err != nil {
		return cloud.VPNMade{}, err
	}
	return createVPN(ctx, ec2.NewFromConfig(cfg), string(actor), s)
}

func filter(name string, values ...string) types.Filter {
	return types.Filter{Name: awssdk.String(name), Values: values}
}

// vpnTunnel is the options both tunnels take: the link's key and ciphers.
func vpnTunnel(s cloud.VPNSpec) types.VpnTunnelOptionsSpecification {
	enc, integ, dh := "AES256-GCM-16", "SHA2-384", int32(20)
	if s.Ciphers == "compatible" {
		enc, integ, dh = "AES256", "SHA2-256", 14
	}
	return types.VpnTunnelOptionsSpecification{
		PreSharedKey:               awssdk.String(s.PSK),
		IKEVersions:                []types.IKEVersionsRequestListValue{{Value: awssdk.String("ikev2")}},
		Phase1EncryptionAlgorithms: []types.Phase1EncryptionAlgorithmsRequestListValue{{Value: awssdk.String(enc)}},
		Phase1IntegrityAlgorithms:  []types.Phase1IntegrityAlgorithmsRequestListValue{{Value: awssdk.String(integ)}},
		Phase1DHGroupNumbers:       []types.Phase1DHGroupNumbersRequestListValue{{Value: awssdk.Int32(dh)}},
		Phase2EncryptionAlgorithms: []types.Phase2EncryptionAlgorithmsRequestListValue{{Value: awssdk.String(enc)}},
		Phase2IntegrityAlgorithms:  []types.Phase2IntegrityAlgorithmsRequestListValue{{Value: awssdk.String(integ)}},
		Phase2DHGroupNumbers:       []types.Phase2DHGroupNumbersRequestListValue{{Value: awssdk.Int32(dh)}},
		DPDTimeoutAction:           awssdk.String("restart"),
	}
}

func createVPN(ctx context.Context, c vpnAPI, actor string, s cloud.VPNSpec) (cloud.VPNMade, error) {
	var made cloud.VPNMade
	tags := map[string]string{cloud.VPNLinkTag: s.Name}
	for k, v := range s.Tags {
		tags[k] = v
	}
	// The customer gateway: AWS's record of the Argus server. One already
	// at that address is used again.
	cgw := ""
	g, err := c.DescribeCustomerGateways(ctx, &ec2.DescribeCustomerGatewaysInput{Filters: []types.Filter{
		filter("ip-address", s.CustomerAddress), filter("state", "available"), filter("type", "ipsec.1")}})
	if err != nil {
		return made, errors.New(plain(err))
	}
	if len(g.CustomerGateways) > 0 {
		cgw = awssdk.ToString(g.CustomerGateways[0].CustomerGatewayId)
	} else {
		out, err := c.CreateCustomerGateway(ctx, &ec2.CreateCustomerGatewayInput{Type: types.GatewayTypeIpsec1,
			IpAddress: awssdk.String(s.CustomerAddress), BgpAsn: awssdk.Int32(65000),
			TagSpecifications: nameTags(s.Name, actor, tags, types.ResourceTypeCustomerGateway)})
		if err != nil {
			return made, errors.New(plain(err))
		}
		cgw = awssdk.ToString(out.CustomerGateway.CustomerGatewayId)
	}
	// The VPC's virtual private gateway, made and attached when it has none.
	v, err := c.DescribeVpnGateways(ctx, &ec2.DescribeVpnGatewaysInput{Filters: []types.Filter{
		filter("attachment.vpc-id", s.Network), filter("attachment.state", "attached", "attaching")}})
	if err != nil {
		return made, errors.New(plain(err))
	}
	if len(v.VpnGateways) > 0 {
		made.Gateway = awssdk.ToString(v.VpnGateways[0].VpnGatewayId)
	} else {
		out, err := c.CreateVpnGateway(ctx, &ec2.CreateVpnGatewayInput{Type: types.GatewayTypeIpsec1,
			TagSpecifications: nameTags(s.Name, actor, tags, types.ResourceTypeVpnGateway)})
		if err != nil {
			return made, fmt.Errorf("%s (customer gateway %s is made)", plain(err), cgw)
		}
		made.Gateway, made.NewGateway = awssdk.ToString(out.VpnGateway.VpnGatewayId), true
		if _, err := c.AttachVpnGateway(ctx, &ec2.AttachVpnGatewayInput{VpcId: awssdk.String(s.Network),
			VpnGatewayId: awssdk.String(made.Gateway)}); err != nil {
			return made, fmt.Errorf("%s (customer gateway %s and virtual private gateway %s are made)", plain(err), cgw, made.Gateway)
		}
	}
	t := vpnTunnel(s)
	out, err := c.CreateVpnConnection(ctx, &ec2.CreateVpnConnectionInput{CustomerGatewayId: awssdk.String(cgw),
		Type: awssdk.String("ipsec.1"), VpnGatewayId: awssdk.String(made.Gateway),
		Options: &types.VpnConnectionOptionsSpecification{StaticRoutesOnly: awssdk.Bool(true),
			// AWS's "local" is the customer's side: the server's range.
			LocalIpv4NetworkCidr: awssdk.String(s.ServerRange), RemoteIpv4NetworkCidr: awssdk.String(s.CloudRange),
			TunnelOptions: []types.VpnTunnelOptionsSpecification{t, t}},
		TagSpecifications: nameTags(s.Name, actor, tags, types.ResourceTypeVpnConnection)})
	if err != nil {
		return made, fmt.Errorf("%s (customer gateway %s and virtual private gateway %s are in place)", plain(err), cgw, made.Gateway)
	}
	conn := out.VpnConnection
	made.ResourceID, made.RequestID = awssdk.ToString(conn.VpnConnectionId), requestID(out.ResultMetadata)
	if conn.Options != nil {
		for _, o := range conn.Options.TunnelOptions {
			if a := awssdk.ToString(o.OutsideIpAddress); a != "" {
				made.Tunnels = append(made.Tunnels, a)
			}
		}
	}
	if len(made.Tunnels) == 0 {
		for _, tl := range conn.VgwTelemetry {
			if a := awssdk.ToString(tl.OutsideIpAddress); a != "" {
				made.Tunnels = append(made.Tunnels, a)
			}
		}
	}
	if _, err := c.CreateVpnConnectionRoute(ctx, &ec2.CreateVpnConnectionRouteInput{VpnConnectionId: conn.VpnConnectionId,
		DestinationCidrBlock: awssdk.String(s.ServerRange)}); err != nil {
		return made, fmt.Errorf("%s (connection %s is made, without its route to %s)", plain(err), made.ResourceID, s.ServerRange)
	}
	if s.Propagate {
		made.Propagated = propagate(ctx, c, s.Network, made.Gateway)
	}
	return made, nil
}

// propagate has every route table of the VPC learn the gateway's routes.
// A gateway still attaching refuses; that is reported, not failed.
func propagate(ctx context.Context, c vpnAPI, vpc, gw string) bool {
	rt, err := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: []types.Filter{filter("vpc-id", vpc)}})
	if err != nil || len(rt.RouteTables) == 0 {
		return false
	}
	for _, t := range rt.RouteTables {
		if _, err := c.EnableVgwRoutePropagation(ctx, &ec2.EnableVgwRoutePropagationInput{GatewayId: awssdk.String(gw),
			RouteTableId: t.RouteTableId}); err != nil {
			return false
		}
	}
	return true
}

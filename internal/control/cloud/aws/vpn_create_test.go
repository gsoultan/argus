package aws

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

type fakeVPNAPI struct {
	calls     []string
	haveCGW   bool
	haveVGW   bool
	attachErr error
	conn      *ec2.CreateVpnConnectionInput
	route     string
	propagate []string
}

func (f *fakeVPNAPI) DescribeCustomerGateways(context.Context, *ec2.DescribeCustomerGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeCustomerGatewaysOutput, error) {
	f.calls = append(f.calls, "describe-cgw")
	if f.haveCGW {
		return &ec2.DescribeCustomerGatewaysOutput{CustomerGateways: []types.CustomerGateway{{CustomerGatewayId: awssdk.String("cgw-old")}}}, nil
	}
	return &ec2.DescribeCustomerGatewaysOutput{}, nil
}
func (f *fakeVPNAPI) CreateCustomerGateway(_ context.Context, in *ec2.CreateCustomerGatewayInput, _ ...func(*ec2.Options)) (*ec2.CreateCustomerGatewayOutput, error) {
	f.calls = append(f.calls, "create-cgw "+awssdk.ToString(in.IpAddress))
	return &ec2.CreateCustomerGatewayOutput{CustomerGateway: &types.CustomerGateway{CustomerGatewayId: awssdk.String("cgw-new")}}, nil
}
func (f *fakeVPNAPI) DescribeVpnGateways(context.Context, *ec2.DescribeVpnGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeVpnGatewaysOutput, error) {
	f.calls = append(f.calls, "describe-vgw")
	if f.haveVGW {
		return &ec2.DescribeVpnGatewaysOutput{VpnGateways: []types.VpnGateway{{VpnGatewayId: awssdk.String("vgw-old")}}}, nil
	}
	return &ec2.DescribeVpnGatewaysOutput{}, nil
}
func (f *fakeVPNAPI) CreateVpnGateway(context.Context, *ec2.CreateVpnGatewayInput, ...func(*ec2.Options)) (*ec2.CreateVpnGatewayOutput, error) {
	f.calls = append(f.calls, "create-vgw")
	return &ec2.CreateVpnGatewayOutput{VpnGateway: &types.VpnGateway{VpnGatewayId: awssdk.String("vgw-new")}}, nil
}
func (f *fakeVPNAPI) AttachVpnGateway(_ context.Context, in *ec2.AttachVpnGatewayInput, _ ...func(*ec2.Options)) (*ec2.AttachVpnGatewayOutput, error) {
	f.calls = append(f.calls, "attach "+awssdk.ToString(in.VpcId))
	return &ec2.AttachVpnGatewayOutput{}, f.attachErr
}
func (f *fakeVPNAPI) CreateVpnConnection(_ context.Context, in *ec2.CreateVpnConnectionInput, _ ...func(*ec2.Options)) (*ec2.CreateVpnConnectionOutput, error) {
	f.calls = append(f.calls, "create-vpn "+awssdk.ToString(in.CustomerGatewayId)+" "+awssdk.ToString(in.VpnGatewayId))
	f.conn = in
	return &ec2.CreateVpnConnectionOutput{VpnConnection: &types.VpnConnection{VpnConnectionId: awssdk.String("vpn-1"),
		Options: &types.VpnConnectionOptions{TunnelOptions: []types.TunnelOption{
			{OutsideIpAddress: awssdk.String("52.1.1.1")}, {OutsideIpAddress: awssdk.String("52.2.2.2")}}}}}, nil
}
func (f *fakeVPNAPI) CreateVpnConnectionRoute(_ context.Context, in *ec2.CreateVpnConnectionRouteInput, _ ...func(*ec2.Options)) (*ec2.CreateVpnConnectionRouteOutput, error) {
	f.route = awssdk.ToString(in.DestinationCidrBlock)
	return &ec2.CreateVpnConnectionRouteOutput{}, nil
}
func (f *fakeVPNAPI) DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	return &ec2.DescribeRouteTablesOutput{RouteTables: []types.RouteTable{{RouteTableId: awssdk.String("rtb-a")}, {RouteTableId: awssdk.String("rtb-b")}}}, nil
}
func (f *fakeVPNAPI) EnableVgwRoutePropagation(_ context.Context, in *ec2.EnableVgwRoutePropagationInput, _ ...func(*ec2.Options)) (*ec2.EnableVgwRoutePropagationOutput, error) {
	f.propagate = append(f.propagate, awssdk.ToString(in.RouteTableId))
	return &ec2.EnableVgwRoutePropagationOutput{}, nil
}

func vpnSpec() cloud.VPNSpec {
	return cloud.VPNSpec{Region: "ap-southeast-3", Name: "Jakarta office", Network: "vpc-0abc1234", CustomerAddress: "36.72.1.9",
		CloudRange: "10.30.0.0/16", ServerRange: "192.168.10.0/24", Ciphers: "strong", PSK: "Abcdefghijklmnop1234", Propagate: true}
}

// A VPC with no gateway gets one, attached; the connection carries the
// key and the link's ciphers on both tunnels, the server's range is the
// customer side, and the route tables learn it.
func TestCreateVPNMakesWhatIsMissing(t *testing.T) {
	f := &fakeVPNAPI{}
	made, err := createVPN(context.Background(), f, "dev@x", vpnSpec())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"describe-cgw", "create-cgw 36.72.1.9", "describe-vgw", "create-vgw", "attach vpc-0abc1234", "create-vpn cgw-new vgw-new"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v", f.calls)
	}
	o := f.conn.Options
	if awssdk.ToString(o.LocalIpv4NetworkCidr) != "192.168.10.0/24" || awssdk.ToString(o.RemoteIpv4NetworkCidr) != "10.30.0.0/16" || !awssdk.ToBool(o.StaticRoutesOnly) {
		t.Fatalf("options %+v", o)
	}
	if len(o.TunnelOptions) != 2 {
		t.Fatal("want both tunnels set")
	}
	for _, tn := range o.TunnelOptions {
		if awssdk.ToString(tn.PreSharedKey) != "Abcdefghijklmnop1234" || awssdk.ToString(tn.Phase1EncryptionAlgorithms[0].Value) != "AES256-GCM-16" ||
			awssdk.ToInt32(tn.Phase2DHGroupNumbers[0].Value) != 20 || awssdk.ToString(tn.IKEVersions[0].Value) != "ikev2" {
			t.Fatalf("tunnel %+v", tn)
		}
	}
	if !slices.ContainsFunc(f.conn.TagSpecifications[0].Tags, func(tg types.Tag) bool {
		return awssdk.ToString(tg.Key) == cloud.VPNLinkTag && awssdk.ToString(tg.Value) == "Jakarta office"
	}) {
		t.Fatal("connection not tagged with its link")
	}
	if f.route != "192.168.10.0/24" || len(f.propagate) != 2 {
		t.Fatalf("route %q propagate %v", f.route, f.propagate)
	}
	if made.ResourceID != "vpn-1" || !made.NewGateway || made.Gateway != "vgw-new" || !made.Propagated ||
		!slices.Equal(made.Tunnels, []string{"52.1.1.1", "52.2.2.2"}) {
		t.Fatalf("made %+v", made)
	}
}

// Gateways already there are used again; compatible ciphers are AWS's
// AES256 with SHA2-256 and group 14.
func TestCreateVPNReusesGateways(t *testing.T) {
	f := &fakeVPNAPI{haveCGW: true, haveVGW: true}
	s := vpnSpec()
	s.Ciphers, s.Propagate = "compatible", false
	made, err := createVPN(context.Background(), f, "dev@x", s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"describe-cgw", "describe-vgw", "create-vpn cgw-old vgw-old"}) || made.NewGateway || made.Propagated {
		t.Fatalf("calls %v made %+v", f.calls, made)
	}
	tn := f.conn.Options.TunnelOptions[0]
	if awssdk.ToString(tn.Phase1EncryptionAlgorithms[0].Value) != "AES256" || awssdk.ToString(tn.Phase1IntegrityAlgorithms[0].Value) != "SHA2-256" ||
		awssdk.ToInt32(tn.Phase1DHGroupNumbers[0].Value) != 14 {
		t.Fatalf("tunnel %+v", tn)
	}
}

// A failure part way says what is already made.
func TestCreateVPNSaysWhatIsLeft(t *testing.T) {
	f := &fakeVPNAPI{attachErr: errors.New("boom")}
	_, err := createVPN(context.Background(), f, "dev@x", vpnSpec())
	if err == nil || !strings.Contains(err.Error(), "cgw-new") || !strings.Contains(err.Error(), "vgw-new") {
		t.Fatalf("err %v", err)
	}
}

func TestVPNSpecCheck(t *testing.T) {
	bad := map[string]func(*cloud.VPNSpec){
		"private address": func(s *cloud.VPNSpec) { s.CustomerAddress = "10.0.0.5" },
		"cgnat address":   func(s *cloud.VPNSpec) { s.CustomerAddress = "100.64.1.1" },
		"overlap":         func(s *cloud.VPNSpec) { s.ServerRange = "10.30.5.0/24" },
		"vpc":             func(s *cloud.VPNSpec) { s.Network = "subnet-1" },
		"ciphers":         func(s *cloud.VPNSpec) { s.Ciphers = "weak" },
		"key":             func(s *cloud.VPNSpec) { s.PSK = "0starts-with-digit" },
		"not start":       func(s *cloud.VPNSpec) { s.CloudRange = "10.30.0.1/16" },
	}
	for name, f := range bad {
		s := vpnSpec()
		f(&s)
		if s.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := vpnSpec().Check(); err != nil {
		t.Fatal(err)
	}
}

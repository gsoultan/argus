package aws

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestTrustPrincipal(t *testing.T) {
	cases := map[string]string{
		"arn:aws:sts::603914770215:assumed-role/argus-control/i-0abc":     "arn:aws:iam::603914770215:role/argus-control",
		"arn:aws-cn:sts::603914770215:assumed-role/argus-control/session": "arn:aws-cn:iam::603914770215:role/argus-control",
		"arn:aws:iam::603914770215:user/argus":                            "arn:aws:iam::603914770215:user/argus",
		"arn:aws:sts::603914770215:federated-user/someone":                "arn:aws:sts::603914770215:federated-user/someone",
		"not an arn": "not an arn",
	}
	for in, want := range cases {
		if got := TrustPrincipal(in); got != want {
			t.Errorf("TrustPrincipal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidRoleARN(t *testing.T) {
	good := []string{"arn:aws:iam::412788931204:role/ArgusRead", "arn:aws:iam::412788931204:role/path/ArgusRead"}
	bad := []string{"", "arn:aws:iam::412788931204:user/x", "arn:aws:iam::41278893120:role/x",
		"arn:aws:sts::412788931204:role/x", "arn:aws:iam::412788931204:role/", "arn:aws:iam:us-east-1:412788931204:role/x"}
	for _, s := range good {
		if !ValidRoleARN(s) {
			t.Errorf("%q should be a role ARN", s)
		}
	}
	for _, s := range bad {
		if ValidRoleARN(s) {
			t.Errorf("%q should not be a role ARN", s)
		}
	}
}

// The refusal: an actor that cannot be a source identity is refused before
// any call, so no role session is ever opened without a person on it.
func TestAssumedRefusesBadActor(t *testing.T) {
	p := New(Options{})
	_, err := p.Verify(context.Background(), cloud.Account{AccountID: "412788931204",
		RoleARN: "arn:aws:iam::412788931204:role/ArgusRead", ExternalID: "x"}, cloud.Actor("has space@example.com"))
	if err == nil || !strings.Contains(err.Error(), "source identity") {
		t.Fatalf("want a source identity refusal, got %v", err)
	}
	_, err = p.Verify(context.Background(), cloud.Account{RoleARN: "arn:aws:iam::412788931204:user/x"}, "a@b.c")
	if err == nil || !strings.Contains(err.Error(), "not an IAM role ARN") {
		t.Fatalf("want a role ARN refusal, got %v", err)
	}
}

func TestCredentialsFileMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are checked by ACL on Windows")
	}
	f := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(f, []byte("[default]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(Options{CredentialsFile: f})
	_, err := p.Base(context.Background())
	if err == nil || !strings.Contains(err.Error(), "credentials file") {
		t.Fatalf("a world-readable credentials file must be refused, got %v", err)
	}
}

func TestInstanceMapping(t *testing.T) {
	launched := time.Date(2026, 10, 28, 10, 40, 0, 0, time.UTC)
	i := types.Instance{
		InstanceId: awssdk.String("i-07c2aa3f0d19e4c91"), InstanceType: types.InstanceTypeM6iXlarge,
		State:            &types.InstanceState{Name: types.InstanceStateNameRunning},
		Placement:        &types.Placement{AvailabilityZone: awssdk.String("ap-southeast-3a")},
		PrivateIpAddress: awssdk.String("10.20.1.33"), PublicIpAddress: awssdk.String("43.218.7.21"),
		VpcId: awssdk.String("vpc-1"), SubnetId: awssdk.String("subnet-1"), LaunchTime: &launched,
		ImageId:        awssdk.String("ami-1"),
		Tags:           []types.Tag{{Key: awssdk.String("Name"), Value: awssdk.String("pay-api-01")}, {Key: awssdk.String("Environment"), Value: awssdk.String("production")}},
		SecurityGroups: []types.GroupIdentifier{{GroupId: awssdk.String("sg-1"), GroupName: awssdk.String("sg-pay-api")}},
		BlockDeviceMappings: []types.InstanceBlockDeviceMapping{{DeviceName: awssdk.String("/dev/xvda"),
			Ebs: &types.EbsInstanceBlockDevice{VolumeId: awssdk.String("vol-1")}}},
		NetworkInterfaces: []types.InstanceNetworkInterface{{PrivateIpAddresses: []types.InstancePrivateIpAddress{
			{PrivateIpAddress: awssdk.String("10.20.1.33")}, {PrivateIpAddress: awssdk.String("10.20.1.34")}}}},
	}
	r := Instance(i, "ap-southeast-3")
	if r.Name != "pay-api-01" || r.State != "running" || r.Size != "m6i.xlarge" || r.Zone != "ap-southeast-3a" {
		t.Fatalf("core fields wrong: %+v", r)
	}
	if strings.Join(r.Addresses, ",") != "10.20.1.33,10.20.1.34" || r.PublicIP != "43.218.7.21" {
		t.Fatalf("addresses wrong: %v %q", r.Addresses, r.PublicIP)
	}
	if len(r.Links) != 3 || r.Links[0].Name != "sg-pay-api" || r.Links[2].Kind != cloud.KindVolume {
		t.Fatalf("links wrong: %+v", r.Links)
	}
	if r.Details["image"] != "ami-1" || r.Tags["Environment"] != "production" {
		t.Fatalf("details or tags wrong: %+v %+v", r.Details, r.Tags)
	}
}

func TestFirewallRules(t *testing.T) {
	g := types.SecurityGroup{GroupId: awssdk.String("sg-1"), GroupName: awssdk.String("sg-web-public"),
		IpPermissions: []types.IpPermission{
			{IpProtocol: awssdk.String("tcp"), FromPort: awssdk.Int32(22), ToPort: awssdk.Int32(22),
				IpRanges: []types.IpRange{{CidrIp: awssdk.String("0.0.0.0/0")}}},
			{IpProtocol: awssdk.String("-1"), UserIdGroupPairs: []types.UserIdGroupPair{{GroupId: awssdk.String("sg-2")}}},
		},
		IpPermissionsEgress: []types.IpPermission{{IpProtocol: awssdk.String("-1"),
			IpRanges: []types.IpRange{{CidrIp: awssdk.String("0.0.0.0/0")}}}},
	}
	r := Firewall(g, "ap-southeast-3")
	if len(r.Rules) != 3 {
		t.Fatalf("want 3 rules, got %+v", r.Rules)
	}
	ssh := r.Rules[0]
	if !ssh.Open() || !ssh.Covers(22) || ssh.Covers(80) {
		t.Fatalf("ssh rule misread: %+v", ssh)
	}
	if r.Rules[1].Protocol != "all" || r.Rules[1].Open() || !r.Rules[1].Covers(3389) {
		t.Fatalf("group rule misread: %+v", r.Rules[1])
	}
	if r.Rules[2].Inbound || r.Rules[2].Open() {
		t.Fatalf("egress is not an open inbound rule: %+v", r.Rules[2])
	}
}

type templateDoc struct {
	Resources map[string]struct {
		Type       string `yaml:"Type"`
		Properties struct {
			RoleName                 string `yaml:"RoleName"`
			AssumeRolePolicyDocument struct {
				Statement []struct {
					Principal map[string]string            `yaml:"Principal"`
					Action    []string                     `yaml:"Action"`
					Condition map[string]map[string]string `yaml:"Condition"`
				} `yaml:"Statement"`
			} `yaml:"AssumeRolePolicyDocument"`
			Policies []struct {
				PolicyDocument struct {
					Statement []struct {
						Action []string `yaml:"Action"`
					} `yaml:"Statement"`
				} `yaml:"PolicyDocument"`
			} `yaml:"Policies"`
		} `yaml:"Properties"`
	} `yaml:"Resources"`
	Outputs map[string]any `yaml:"Outputs"`
}

func TestTemplate(t *testing.T) {
	tpl := Template("arn:aws:iam::603914770215:role/argus-control", "argus-0123abcd", false)
	var doc templateDoc
	if err := yaml.Unmarshal([]byte(tpl), &doc); err != nil {
		t.Fatalf("template is not YAML: %v\n%s", err, tpl)
	}
	role := doc.Resources["ArgusRead"]
	if role.Type != "AWS::IAM::Role" || role.Properties.RoleName != ReadRoleName {
		t.Fatalf("role wrong: %+v", role)
	}
	st := role.Properties.AssumeRolePolicyDocument.Statement[0]
	if st.Principal["AWS"] != "arn:aws:iam::603914770215:role/argus-control" {
		t.Fatalf("trusts the wrong principal: %v", st.Principal)
	}
	if st.Condition["StringEquals"]["sts:ExternalId"] != "argus-0123abcd" || st.Condition["Null"]["sts:SourceIdentity"] != "false" {
		t.Fatalf("trust must require the external id and a source identity: %v", st.Condition)
	}
	acts := role.Properties.Policies[0].PolicyDocument.Statement[0].Action
	for _, a := range acts {
		if a == "s3:GetObject" || strings.HasSuffix(a, ":*") || strings.Contains(a, "Put") || strings.Contains(a, "Delete") {
			t.Fatalf("the read role must not be able to read data or change anything: %s", a)
		}
	}
	if _, ok := doc.Resources["ArgusManage"]; ok {
		t.Fatal("a read-only template must not make the manage role")
	}
	if !strings.Contains(DeployCommand("p", "e", false), "aws cloudformation deploy") {
		t.Fatal("deploy command lost its deploy")
	}
}

func TestManageTemplate(t *testing.T) {
	tpl := Template("arn:aws:iam::603914770215:role/argus-control", "argus-0123abcd", true)
	var doc templateDoc
	if err := yaml.Unmarshal([]byte(tpl), &doc); err != nil {
		t.Fatalf("template is not YAML: %v\n%s", err, tpl)
	}
	m, ok := doc.Resources["ArgusManage"]
	if !ok || m.Properties.RoleName != ManageRoleName {
		t.Fatalf("manage role missing: %+v", doc.Resources)
	}
	if _, ok := doc.Resources["ArgusRead"]; !ok {
		t.Fatal("the manage template must keep the read role")
	}
	st := m.Properties.AssumeRolePolicyDocument.Statement[0]
	if st.Condition["StringEquals"]["sts:ExternalId"] != "argus-0123abcd" || st.Condition["Null"]["sts:SourceIdentity"] != "false" {
		t.Fatalf("the manage role must require the external id and a source identity: %v", st.Condition)
	}
	for _, a := range m.Properties.Policies[0].PolicyDocument.Statement[0].Action {
		if strings.HasPrefix(a, "iam:") || a == "*" || a == "ec2:*" {
			t.Fatalf("the manage role must not touch IAM or have wildcards: %s", a)
		}
	}
	if _, ok := doc.Outputs["ManageRoleArn"]; !ok {
		t.Fatal("no ManageRoleArn output")
	}
}

func TestDescribeSourceStaticKey(t *testing.T) {
	if got := describeSource(credentials.StaticCredentialsName); !strings.Contains(got, "Cloud identities") {
		t.Errorf("describeSource(static) = %q, want it to name Settings", got)
	}
}

// A key entered in Settings is the identity, ahead of anything on the host,
// and a key id without its secret is refused rather than half-used.
func TestStaticKeyIsTheBase(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENVIRONMENTXXXXX")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "from-the-environment")
	p := New(Options{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"})
	cfg, err := p.base(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "AKIAIOSFODNN7EXAMPLE" || creds.Source != credentials.StaticCredentialsName {
		t.Errorf("credentials = %s from %q, want the entered key", creds.AccessKeyID, creds.Source)
	}

	if _, err := New(Options{AccessKeyID: "AKIAIOSFODNN7EXAMPLE"}).base(context.Background()); err == nil {
		t.Error("a key id without its secret was accepted")
	}
}

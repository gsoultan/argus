package aws

import (
	"net/url"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// A manage role made from the phase-2 template lacks the storage
// permissions; one made from today's lacks nothing. IAM returns the
// document URL-encoded.
func TestMissingFromThePolicy(t *testing.T) {
	old := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Resource":"*","Action":["ec2:Describe*","ec2:RunInstances",` +
		`"ec2:StartInstances","ec2:StopInstances","ec2:RebootInstances","ec2:TerminateInstances","ec2:ModifyInstanceAttribute",` +
		`"ec2:CreateTags","ec2:DeleteTags","ec2:CreateSnapshot"]}]}`
	missing, err := MissingFrom(url.QueryEscape(old))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 20 || missing[0] != "ec2:DeleteSnapshot" {
		t.Fatalf("missing: %v", missing)
	}
	now := `{"Statement":[{"Effect":"Allow","Action":["` + strings.Join(ManagePolicy, `","`) + `"]}]}`
	if missing, _ := MissingFrom(now); len(missing) != 0 {
		t.Fatalf("today's template lacks %v", missing)
	}
	// Every storage and network action's permissions are in the template.
	p := &Provider{}
	for _, a := range append(append([]cloud.Action(nil), cloud.StorageActions...), cloud.NetworkActions...) {
		for _, need := range p.Needs(a) {
			found := false
			for _, have := range ManagePolicy {
				found = found || have == need
			}
			if !found {
				t.Errorf("%s needs %s, which the template does not grant", a, need)
			}
		}
		if len(p.Needs(a)) == 0 {
			t.Errorf("%s names no permission", a)
		}
	}
	if !strings.Contains(Template("arn:aws:iam::1:role/x", "e", true), "ec2:CreateVolume") {
		t.Fatal("the manage template must grant the storage actions")
	}
}

func TestSnapshotAndImageMapping(t *testing.T) {
	s := Snapshot(types.Snapshot{SnapshotId: awssdk.String("snap-1"), VolumeId: awssdk.String("vol-ffffffff"),
		VolumeSize: awssdk.Int32(30), State: types.SnapshotStateCompleted, Encrypted: awssdk.Bool(true)}, "eu-west-1")
	if s.Kind != cloud.KindSnapshot || s.Size != "30 GiB" || len(s.Links) != 0 || s.Details["encrypted"] != "yes" {
		t.Fatalf("snapshot: %+v", s)
	}
	im := OwnImage(types.Image{ImageId: awssdk.String("ami-1"), Name: awssdk.String("web"), CreationDate: awssdk.String("2026-11-01T10:00:00.000Z"),
		Public: awssdk.Bool(true), BlockDeviceMappings: []types.BlockDeviceMapping{{DeviceName: awssdk.String("/dev/xvda"),
			Ebs: &types.EbsBlockDevice{SnapshotId: awssdk.String("snap-1"), VolumeSize: awssdk.Int32(30)}}}}, "eu-west-1")
	if im.Name != "web" || im.CreatedAt == nil || im.Details["public"] != "yes" || len(im.Links) != 1 || im.Links[0].ID != "snap-1" {
		t.Fatalf("image: %+v", im)
	}
}

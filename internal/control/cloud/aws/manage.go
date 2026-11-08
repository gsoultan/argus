package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go/middleware"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Changes, through the account's manage role, as the person who asked. Every
call's AWS request id is returned so the audit row and CloudTrail can be
matched; the role session's source identity is the person's email, so
CloudTrail names them too.
*/

// imageFamilies are the images a launch offers: the newest of each, read
// from its publisher's own account so a look-alike image in someone else's
// account is never picked.
var imageFamilies = []struct {
	family, label, owner, name string
	linux                      bool
}{
	{"ubuntu-24.04", "Ubuntu 24.04 LTS", "099720109477", "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*", true},
	{"ubuntu-22.04", "Ubuntu 22.04 LTS", "099720109477", "ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*", true},
	{"debian-12", "Debian 12", "136693071363", "debian-12-amd64-*", true},
	{"amazon-linux-2023", "Amazon Linux 2023", "137112412989", "al2023-ami-2023.*-x86_64", true},
	{"windows-2022", "Windows Server 2022", "801119661308", "Windows_Server-2022-English-Full-Base-*", false},
}

func (p *Provider) client(ctx context.Context, a cloud.Account, actor cloud.Actor, region string) (*ec2.Client, error) {
	cfg, err := p.assumed(ctx, a, actor, region)
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(cfg), nil
}

func requestID(md middleware.Metadata) string {
	id, _ := awsmiddleware.GetRequestIDMetadata(md)
	return id
}

// Images finds the newest image of each family in region.
func (p *Provider) Images(ctx context.Context, a cloud.Account, actor cloud.Actor, region string) ([]cloud.Image, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return nil, err
	}
	out := []cloud.Image{}
	for _, f := range imageFamilies {
		res, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{
			Owners: []string{f.owner},
			Filters: []types.Filter{
				{Name: awssdk.String("name"), Values: []string{f.name}},
				{Name: awssdk.String("state"), Values: []string{"available"}},
				{Name: awssdk.String("architecture"), Values: []string{"x86_64"}},
			},
		})
		if err != nil {
			return nil, errors.New(plain(err))
		}
		if len(res.Images) == 0 {
			continue
		}
		sort.Slice(res.Images, func(i, j int) bool {
			return awssdk.ToString(res.Images[i].CreationDate) > awssdk.ToString(res.Images[j].CreationDate)
		})
		im := res.Images[0]
		out = append(out, cloud.Image{Family: f.family, Label: f.label, ID: awssdk.ToString(im.ImageId),
			Name: awssdk.ToString(im.Name), Linux: f.linux})
	}
	return out, nil
}

// Launch starts one instance: IMDSv2 only, an encrypted root disk, the
// person's tags and the agent's first-boot script.
func (p *Provider) Launch(ctx context.Context, a cloud.Account, actor cloud.Actor, s cloud.LaunchSpec) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, s.Region)
	if err != nil {
		return cloud.Result{}, err
	}
	img, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{s.Image}})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	if len(img.Images) == 0 {
		return cloud.Result{}, fmt.Errorf("image %s is not available in %s", s.Image, s.Region)
	}
	tags := []types.Tag{{Key: awssdk.String("Name"), Value: awssdk.String(s.Name)}}
	keys := make([]string, 0, len(s.Tags))
	for k := range s.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "Name" {
			tags = append(tags, types.Tag{Key: awssdk.String(k), Value: awssdk.String(s.Tags[k])})
		}
	}
	in := &ec2.RunInstancesInput{
		ImageId:      awssdk.String(s.Image),
		InstanceType: types.InstanceType(s.Size),
		MinCount:     awssdk.Int32(1),
		MaxCount:     awssdk.Int32(1),
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: img.Images[0].RootDeviceName,
			Ebs: &types.EbsBlockDevice{VolumeSize: awssdk.Int32(s.DiskGB), VolumeType: types.VolumeTypeGp3,
				Encrypted: awssdk.Bool(true), DeleteOnTermination: awssdk.Bool(true)},
		}},
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex: awssdk.Int32(0), SubnetId: awssdk.String(s.Subnet), Groups: s.Firewalls,
			AssociatePublicIpAddress: awssdk.Bool(s.PublicIP), DeleteOnTermination: awssdk.Bool(true),
		}},
		MetadataOptions: &types.InstanceMetadataOptionsRequest{HttpTokens: types.HttpTokensStateRequired,
			HttpEndpoint: types.InstanceMetadataEndpointStateEnabled},
		DisableApiTermination: awssdk.Bool(s.Protect),
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
	}
	if s.UserData != "" {
		in.UserData = awssdk.String(base64.StdEncoding.EncodeToString([]byte(s.UserData)))
	}
	out, err := c.RunInstances(ctx, in)
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	r := cloud.Result{RequestID: requestID(out.ResultMetadata)}
	if len(out.Instances) > 0 {
		r.ResourceID = awssdk.ToString(out.Instances[0].InstanceId)
	}
	return r, nil
}

// Power starts, stops or reboots an instance.
func (p *Provider) Power(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, act cloud.Action) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	var md middleware.Metadata
	switch act {
	case cloud.ActionStart:
		out, e := c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}})
		if e == nil {
			md = out.ResultMetadata
		}
		err = e
	case cloud.ActionStop:
		out, e := c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}})
		if e == nil {
			md = out.ResultMetadata
		}
		err = e
	case cloud.ActionReboot:
		out, e := c.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: []string{id}})
		if e == nil {
			md = out.ResultMetadata
		}
		err = e
	default:
		return cloud.Result{}, fmt.Errorf("%q is not a power action", act)
	}
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: id, RequestID: requestID(md)}, nil
}

// Resize changes a stopped instance's type.
func (p *Provider) Resize(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id, size string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: awssdk.String(id),
		InstanceType: &types.AttributeValue{Value: awssdk.String(size)}})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: id, RequestID: requestID(out.ResultMetadata)}, nil
}

// SetTags adds or changes tags and removes others.
func (p *Provider) SetTags(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, set map[string]string, remove []string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out := cloud.Result{ResourceID: id}
	if len(set) > 0 {
		var tags []types.Tag
		for k, v := range set {
			tags = append(tags, types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
		}
		res, err := c.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: tags})
		if err != nil {
			return cloud.Result{}, errors.New(plain(err))
		}
		out.RequestID = requestID(res.ResultMetadata)
	}
	if len(remove) > 0 {
		var tags []types.Tag
		for _, k := range remove {
			tags = append(tags, types.Tag{Key: awssdk.String(k)})
		}
		res, err := c.DeleteTags(ctx, &ec2.DeleteTagsInput{Resources: []string{id}, Tags: tags})
		if err != nil {
			return out, fmt.Errorf("the new tags were set, but removing the others failed: %s", plain(err))
		}
		if out.RequestID == "" {
			out.RequestID = requestID(res.ResultMetadata)
		}
	}
	return out, nil
}

// Protect turns termination protection on or off.
func (p *Provider) Protect(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, on bool) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: awssdk.String(id),
		DisableApiTermination: &types.AttributeBooleanValue{Value: awssdk.Bool(on)}})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: id, RequestID: requestID(out.ResultMetadata)}, nil
}

// PlanRemove reads what terminating id takes with it.
func (p *Provider) PlanRemove(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string) (cloud.RemovePlan, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.RemovePlan{}, err
	}
	res, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return cloud.RemovePlan{}, errors.New(plain(err))
	}
	if len(res.Reservations) == 0 || len(res.Reservations[0].Instances) == 0 {
		return cloud.RemovePlan{}, fmt.Errorf("no instance %s in %s", id, region)
	}
	inst := res.Reservations[0].Instances[0]
	r := Instance(inst, region)
	plan := cloud.RemovePlan{ID: id, Name: r.Name, State: r.State, Size: r.Size, PublicIP: r.PublicIP, Volumes: []cloud.Volume{}}
	if s := r.Tags["aws:cloudformation:stack-name"]; s != "" {
		plan.ManagedBy = "CloudFormation stack " + s
	}
	attr, err := c.DescribeInstanceAttribute(ctx, &ec2.DescribeInstanceAttributeInput{InstanceId: awssdk.String(id),
		Attribute: types.InstanceAttributeNameDisableApiTermination})
	if err != nil {
		return cloud.RemovePlan{}, errors.New(plain(err))
	}
	plan.Protected = attr.DisableApiTermination != nil && awssdk.ToBool(attr.DisableApiTermination.Value)

	addrs, err := c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: []types.Filter{
		{Name: awssdk.String("instance-id"), Values: []string{id}}}})
	if err == nil && len(addrs.Addresses) > 0 {
		plan.ElasticIP = awssdk.ToString(addrs.Addresses[0].PublicIp)
		if plan.ElasticIP == plan.PublicIP {
			plan.PublicIP = ""
		}
	}

	var vols []string
	deleted := map[string]bool{}
	device := map[string]string{}
	for _, b := range inst.BlockDeviceMappings {
		if b.Ebs == nil {
			continue
		}
		v := awssdk.ToString(b.Ebs.VolumeId)
		vols = append(vols, v)
		deleted[v] = awssdk.ToBool(b.Ebs.DeleteOnTermination)
		device[v] = awssdk.ToString(b.DeviceName)
	}
	if len(vols) > 0 {
		vr, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: vols})
		if err != nil {
			return cloud.RemovePlan{}, errors.New(plain(err))
		}
		sizes := map[string]int32{}
		for _, v := range vr.Volumes {
			sizes[awssdk.ToString(v.VolumeId)] = awssdk.ToInt32(v.Size)
		}
		snaps, _ := c.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{OwnerIds: []string{"self"},
			Filters: []types.Filter{{Name: awssdk.String("volume-id"), Values: vols}}})
		last := map[string]types.Snapshot{}
		if snaps != nil {
			for _, s := range snaps.Snapshots {
				v := awssdk.ToString(s.VolumeId)
				if prev, ok := last[v]; !ok || (s.StartTime != nil && prev.StartTime != nil && s.StartTime.After(*prev.StartTime)) {
					last[v] = s
				}
			}
		}
		for _, v := range vols {
			vol := cloud.Volume{ID: v, Device: device[v], SizeGB: sizes[v], Deleted: deleted[v]}
			if s, ok := last[v]; ok {
				vol.LastSnapID = awssdk.ToString(s.SnapshotId)
				if s.StartTime != nil {
					vol.LastSnapAt = s.StartTime.UTC().Format("2006-01-02T15:04:05Z")
				}
			}
			plan.Volumes = append(plan.Volumes, vol)
		}
	}
	return plan, nil
}

// Remove terminates id, snapshotting the disks that go with it first when
// asked. A snapshot captures the disk as it is when it is asked for, so the
// instance does not wait for it to finish.
func (p *Provider) Remove(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, finalSnapshots bool) (cloud.Result, error) {
	plan, err := p.PlanRemove(ctx, a, actor, region, id)
	if err != nil {
		return cloud.Result{}, err
	}
	if plan.Protected {
		return cloud.Result{}, cloud.ErrProtected
	}
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out := cloud.Result{ResourceID: id}
	if finalSnapshots {
		out.Snapshots = map[string]string{}
		for _, v := range plan.Volumes {
			if !v.Deleted {
				continue
			}
			s, err := c.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: awssdk.String(v.ID),
				Description: awssdk.String("Final snapshot of " + plan.Name + " (" + id + ") before Argus removed it"),
				TagSpecifications: []types.TagSpecification{{ResourceType: types.ResourceTypeSnapshot,
					Tags: []types.Tag{{Key: awssdk.String("Name"), Value: awssdk.String(plan.Name + " final")}}}}})
			if err != nil {
				return out, fmt.Errorf("nothing was removed: the final snapshot of %s failed: %s", v.ID, plain(err))
			}
			out.Snapshots[v.ID] = awssdk.ToString(s.SnapshotId)
		}
	}
	res, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		if len(out.Snapshots) > 0 {
			return out, fmt.Errorf("the final snapshots were taken (%s), but the instance was not removed: %s",
				strings.Join(sortedValues(out.Snapshots), ", "), plain(err))
		}
		return out, errors.New(plain(err))
	}
	out.RequestID = requestID(res.ResultMetadata)
	return out, nil
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

var _ cloud.Manager = (*Provider)(nil)

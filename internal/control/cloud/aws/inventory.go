package aws

import (
	"context"
	"fmt"
	"strconv"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// Inventory reads one region, as the sync identity. Describe calls are free
// in AWS; nothing here is billed.
func (p *Provider) Inventory(ctx context.Context, a cloud.Account, region string, kinds []cloud.Kind) (cloud.Snapshot, error) {
	cfg, err := p.assumed(ctx, a, cloud.SyncActor, region)
	if err != nil {
		return cloud.Snapshot{}, err
	}
	c := ec2.NewFromConfig(cfg)
	snap := cloud.Snapshot{Region: region, Kinds: kinds, ReadAt: time.Now().UTC()}
	for _, k := range kinds {
		var got []cloud.Resource
		switch k {
		case cloud.KindInstance:
			got, err = instances(ctx, c, region)
		case cloud.KindVolume:
			got, err = volumes(ctx, c, region)
		case cloud.KindFirewall:
			got, err = firewalls(ctx, c, region)
		case cloud.KindNetwork:
			got, err = networks(ctx, c, region)
		case cloud.KindSubnet:
			got, err = subnets(ctx, c, region)
		case cloud.KindAddress:
			got, err = addresses(ctx, c, region)
		case cloud.KindSnapshot:
			got, err = snapshots(ctx, c, region)
		case cloud.KindImage:
			got, err = images(ctx, c, region)
		default:
			err = fmt.Errorf("AWS has no inventory of %q yet", k)
		}
		if err != nil {
			return cloud.Snapshot{}, fmt.Errorf("%s in %s: %s", k, region, plain(err))
		}
		snap.Resources = append(snap.Resources, got...)
	}
	return snap, nil
}

func instances(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	var out []cloud.Resource
	pg := ec2.NewDescribeInstancesPaginator(c, &ec2.DescribeInstancesInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				if i.State != nil && i.State.Name == types.InstanceStateNameTerminated {
					continue
				}
				out = append(out, Instance(i, region))
			}
		}
	}
	return out, nil
}

// Instance maps one EC2 instance.
func Instance(i types.Instance, region string) cloud.Resource {
	id := awssdk.ToString(i.InstanceId)
	tags := tagMap(i.Tags)
	r := cloud.Resource{
		Kind: cloud.KindInstance, ID: id, Name: cloud.Name(tags, id), Region: region,
		Size: string(i.InstanceType), Network: awssdk.ToString(i.VpcId), Tags: tags,
		CreatedAt: i.LaunchTime, PublicIP: awssdk.ToString(i.PublicIpAddress),
		Details: map[string]string{},
	}
	if i.Placement != nil {
		r.Zone = awssdk.ToString(i.Placement.AvailabilityZone)
	}
	if i.State != nil {
		r.State = string(i.State.Name)
	}
	if ip := awssdk.ToString(i.PrivateIpAddress); ip != "" {
		r.Addresses = append(r.Addresses, ip)
	}
	for _, n := range i.NetworkInterfaces {
		for _, a := range n.PrivateIpAddresses {
			if ip := awssdk.ToString(a.PrivateIpAddress); ip != "" && !contains(r.Addresses, ip) {
				r.Addresses = append(r.Addresses, ip)
			}
		}
	}
	for _, g := range i.SecurityGroups {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindFirewall, ID: awssdk.ToString(g.GroupId), Name: awssdk.ToString(g.GroupName)})
	}
	if s := awssdk.ToString(i.SubnetId); s != "" {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindSubnet, ID: s})
	}
	for _, b := range i.BlockDeviceMappings {
		if b.Ebs != nil {
			r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindVolume, ID: awssdk.ToString(b.Ebs.VolumeId), Name: awssdk.ToString(b.DeviceName)})
		}
	}
	set(r.Details, "image", awssdk.ToString(i.ImageId))
	set(r.Details, "platform", awssdk.ToString(i.PlatformDetails))
	set(r.Details, "architecture", string(i.Architecture))
	set(r.Details, "keyPair", awssdk.ToString(i.KeyName))
	set(r.Details, "lifecycle", string(i.InstanceLifecycle))
	set(r.Details, "rootDevice", string(i.RootDeviceType))
	set(r.Details, "rootDeviceName", awssdk.ToString(i.RootDeviceName))
	if i.IamInstanceProfile != nil {
		set(r.Details, "instanceProfile", awssdk.ToString(i.IamInstanceProfile.Arn))
	}
	if i.MetadataOptions != nil {
		set(r.Details, "metadataTokens", string(i.MetadataOptions.HttpTokens))
	}
	if i.StateReason != nil {
		set(r.Details, "stateReason", awssdk.ToString(i.StateReason.Message))
	}
	return r
}

func volumes(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	var out []cloud.Resource
	pg := ec2.NewDescribeVolumesPaginator(c, &ec2.DescribeVolumesInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range page.Volumes {
			out = append(out, Volume(v, region))
		}
	}
	return out, nil
}

// Volume maps one EBS volume.
func Volume(v types.Volume, region string) cloud.Resource {
	id := awssdk.ToString(v.VolumeId)
	tags := tagMap(v.Tags)
	r := cloud.Resource{
		Kind: cloud.KindVolume, ID: id, Name: cloud.Name(tags, id), Region: region,
		Zone: awssdk.ToString(v.AvailabilityZone), State: string(v.State), CreatedAt: v.CreateTime,
		Size: fmt.Sprintf("%d GiB %s", awssdk.ToInt32(v.Size), v.VolumeType), Tags: tags,
		Details: map[string]string{},
	}
	for _, a := range v.Attachments {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindInstance, ID: awssdk.ToString(a.InstanceId), Name: awssdk.ToString(a.Device)})
		if awssdk.ToBool(a.DeleteOnTermination) {
			set(r.Details, "deleteOnTermination", "yes")
		}
	}
	set(r.Details, "encrypted", yesNo(awssdk.ToBool(v.Encrypted)))
	if v.Iops != nil {
		set(r.Details, "iops", strconv.Itoa(int(*v.Iops)))
	}
	set(r.Details, "snapshot", awssdk.ToString(v.SnapshotId))
	return r
}

func firewalls(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	var out []cloud.Resource
	pg := ec2.NewDescribeSecurityGroupsPaginator(c, &ec2.DescribeSecurityGroupsInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range page.SecurityGroups {
			out = append(out, Firewall(g, region))
		}
	}
	return out, nil
}

// Firewall maps one security group.
func Firewall(g types.SecurityGroup, region string) cloud.Resource {
	id := awssdk.ToString(g.GroupId)
	tags := tagMap(g.Tags)
	name := awssdk.ToString(g.GroupName)
	if name == "" {
		name = cloud.Name(tags, id)
	}
	r := cloud.Resource{
		Kind: cloud.KindFirewall, ID: id, Name: name, Region: region, Network: awssdk.ToString(g.VpcId),
		Tags: tags, Details: map[string]string{},
	}
	set(r.Details, "description", awssdk.ToString(g.Description))
	r.Rules = append(r.Rules, rules(g.IpPermissions, true)...)
	r.Rules = append(r.Rules, rules(g.IpPermissionsEgress, false)...)
	return r
}

func rules(perms []types.IpPermission, inbound bool) []cloud.Rule {
	var out []cloud.Rule
	for _, p := range perms {
		proto := awssdk.ToString(p.IpProtocol)
		switch proto {
		case "-1":
			proto = "all"
		case "6":
			proto = "tcp"
		case "17":
			proto = "udp"
		case "1":
			proto = "icmp"
		}
		from, to := awssdk.ToInt32(p.FromPort), awssdk.ToInt32(p.ToPort)
		if proto == "all" {
			from, to = 0, 65535
		}
		base := cloud.Rule{Inbound: inbound, Protocol: proto, FromPort: from, ToPort: to}
		for _, r := range p.IpRanges {
			x := base
			x.Source, x.Description = awssdk.ToString(r.CidrIp), awssdk.ToString(r.Description)
			out = append(out, x)
		}
		for _, r := range p.Ipv6Ranges {
			x := base
			x.Source, x.Description = awssdk.ToString(r.CidrIpv6), awssdk.ToString(r.Description)
			out = append(out, x)
		}
		for _, r := range p.UserIdGroupPairs {
			x := base
			x.Source, x.Description = awssdk.ToString(r.GroupId), awssdk.ToString(r.Description)
			out = append(out, x)
		}
		for _, r := range p.PrefixListIds {
			x := base
			x.Source, x.Description = awssdk.ToString(r.PrefixListId), awssdk.ToString(r.Description)
			out = append(out, x)
		}
	}
	return out
}

func networks(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	var out []cloud.Resource
	pg := ec2.NewDescribeVpcsPaginator(c, &ec2.DescribeVpcsInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range page.Vpcs {
			id := awssdk.ToString(v.VpcId)
			tags := tagMap(v.Tags)
			r := cloud.Resource{Kind: cloud.KindNetwork, ID: id, Name: cloud.Name(tags, id), Region: region,
				State: string(v.State), Size: awssdk.ToString(v.CidrBlock), Tags: tags, Details: map[string]string{}}
			if awssdk.ToBool(v.IsDefault) {
				r.Details["default"] = "yes"
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func subnets(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	var out []cloud.Resource
	pg := ec2.NewDescribeSubnetsPaginator(c, &ec2.DescribeSubnetsInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range page.Subnets {
			id := awssdk.ToString(s.SubnetId)
			tags := tagMap(s.Tags)
			r := cloud.Resource{Kind: cloud.KindSubnet, ID: id, Name: cloud.Name(tags, id), Region: region,
				Zone: awssdk.ToString(s.AvailabilityZone), State: string(s.State), Size: awssdk.ToString(s.CidrBlock),
				Network: awssdk.ToString(s.VpcId), Tags: tags, Details: map[string]string{}}
			set(r.Details, "freeAddresses", strconv.Itoa(int(awssdk.ToInt32(s.AvailableIpAddressCount))))
			set(r.Details, "publicOnLaunch", yesNo(awssdk.ToBool(s.MapPublicIpOnLaunch)))
			out = append(out, r)
		}
	}
	return out, nil
}

func addresses(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	page, err := c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return nil, err
	}
	var out []cloud.Resource
	for _, a := range page.Addresses {
		out = append(out, Address(a, region))
	}
	return out, nil
}

// Address maps one Elastic IP.
func Address(a types.Address, region string) cloud.Resource {
	id := awssdk.ToString(a.AllocationId)
	ip := awssdk.ToString(a.PublicIp)
	if id == "" {
		id = ip
	}
	tags := tagMap(a.Tags)
	r := cloud.Resource{Kind: cloud.KindAddress, ID: id, Name: cloud.Name(tags, ip), Region: region,
		PublicIP: ip, Tags: tags, State: "unattached", Details: map[string]string{}}
	if i := awssdk.ToString(a.InstanceId); i != "" {
		r.State = "attached"
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindInstance, ID: i})
	} else if awssdk.ToString(a.AssociationId) != "" {
		r.State = "attached"
	}
	if ip := awssdk.ToString(a.PrivateIpAddress); ip != "" {
		r.Addresses = []string{ip}
	}
	return r
}

func snapshots(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	var out []cloud.Resource
	pg := ec2.NewDescribeSnapshotsPaginator(c, &ec2.DescribeSnapshotsInput{OwnerIds: []string{"self"}})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range page.Snapshots {
			out = append(out, Snapshot(s, region))
		}
	}
	return out, nil
}

// Snapshot maps one EBS snapshot this account owns.
func Snapshot(s types.Snapshot, region string) cloud.Resource {
	id := awssdk.ToString(s.SnapshotId)
	tags := tagMap(s.Tags)
	r := cloud.Resource{
		Kind: cloud.KindSnapshot, ID: id, Name: cloud.Name(tags, id), Region: region,
		State: string(s.State), CreatedAt: s.StartTime, Tags: tags,
		Size: fmt.Sprintf("%d GiB", awssdk.ToInt32(s.VolumeSize)), Details: map[string]string{},
	}
	// A snapshot copied from elsewhere names a volume id that is not real.
	if v := awssdk.ToString(s.VolumeId); v != "" && v != "vol-ffffffff" {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindVolume, ID: v})
	}
	set(r.Details, "description", awssdk.ToString(s.Description))
	set(r.Details, "encrypted", yesNo(awssdk.ToBool(s.Encrypted)))
	set(r.Details, "progress", awssdk.ToString(s.Progress))
	set(r.Details, "storageTier", string(s.StorageTier))
	return r
}

func images(ctx context.Context, c *ec2.Client, region string) ([]cloud.Resource, error) {
	var out []cloud.Resource
	pg := ec2.NewDescribeImagesPaginator(c, &ec2.DescribeImagesInput{Owners: []string{"self"}})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, im := range page.Images {
			out = append(out, OwnImage(im, region))
		}
	}
	return out, nil
}

// OwnImage maps one image this account owns.
func OwnImage(im types.Image, region string) cloud.Resource {
	id := awssdk.ToString(im.ImageId)
	tags := tagMap(im.Tags)
	name := awssdk.ToString(im.Name)
	if name == "" {
		name = cloud.Name(tags, id)
	}
	r := cloud.Resource{Kind: cloud.KindImage, ID: id, Name: name, Region: region, State: string(im.State),
		Tags: tags, Details: map[string]string{}}
	if t, err := time.Parse(time.RFC3339, awssdk.ToString(im.CreationDate)); err == nil {
		r.CreatedAt = &t
	}
	var gb int32
	for _, b := range im.BlockDeviceMappings {
		if b.Ebs == nil {
			continue
		}
		gb += awssdk.ToInt32(b.Ebs.VolumeSize)
		if s := awssdk.ToString(b.Ebs.SnapshotId); s != "" {
			r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindSnapshot, ID: s, Name: awssdk.ToString(b.DeviceName)})
		}
	}
	if gb > 0 {
		r.Size = fmt.Sprintf("%d GiB", gb)
	}
	set(r.Details, "description", awssdk.ToString(im.Description))
	set(r.Details, "architecture", string(im.Architecture))
	set(r.Details, "platform", awssdk.ToString(im.PlatformDetails))
	set(r.Details, "rootDevice", string(im.RootDeviceType))
	if awssdk.ToBool(im.Public) {
		r.Details["public"] = "yes"
	}
	if t := tags["argus:from-instance"]; t != "" {
		r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindInstance, ID: t})
	}
	return r
}

func tagMap(tags []types.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
	return m
}

func set(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

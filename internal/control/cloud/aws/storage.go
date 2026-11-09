package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Disks, snapshots, images and Elastic IPs, through the manage role as the
person who asked. New disks are always encrypted with the account's default
key, and everything Argus makes is tagged with who asked for it.
*/

// volumeReadyWait bounds how long a new disk may take to become available
// before Argus attaches it.
const volumeReadyWait = 75 * time.Second

func nameTags(name, actor string, extra map[string]string, rt types.ResourceType) []types.TagSpecification {
	tags := []types.Tag{{Key: awssdk.String("argus:created-by"), Value: awssdk.String(actor)}}
	if name != "" {
		tags = append(tags, types.Tag{Key: awssdk.String("Name"), Value: awssdk.String(name)})
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "Name" && k != "argus:created-by" {
			tags = append(tags, types.Tag{Key: awssdk.String(k), Value: awssdk.String(extra[k])})
		}
	}
	return []types.TagSpecification{{ResourceType: rt, Tags: tags}}
}

// CreateVolume makes an encrypted disk, and attaches it when asked once
// AWS says it is ready.
func (p *Provider) CreateVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, s cloud.VolumeSpec) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	in := &ec2.CreateVolumeInput{
		AvailabilityZone:  awssdk.String(s.Zone),
		VolumeType:        types.VolumeType(s.Type),
		Encrypted:         awssdk.Bool(true),
		TagSpecifications: nameTags(s.Name, string(actor), s.Tags, types.ResourceTypeVolume),
	}
	if s.SizeGB > 0 {
		in.Size = awssdk.Int32(s.SizeGB)
	}
	if s.Snapshot != "" {
		in.SnapshotId = awssdk.String(s.Snapshot)
	}
	if s.IOPS > 0 {
		in.Iops = awssdk.Int32(s.IOPS)
	}
	if s.Throughput > 0 {
		in.Throughput = awssdk.Int32(s.Throughput)
	}
	out, err := c.CreateVolume(ctx, in)
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	id := awssdk.ToString(out.VolumeId)
	r := cloud.Result{ResourceID: id, RequestID: requestID(out.ResultMetadata)}
	if s.AttachTo == "" {
		return r, nil
	}
	w := ec2.NewVolumeAvailableWaiter(c)
	if err := w.Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}}, volumeReadyWait); err != nil {
		return r, fmt.Errorf("made %s, but it was not ready to attach in time; attach it from Disks: %s", id, plain(err))
	}
	at, err := c.AttachVolume(ctx, &ec2.AttachVolumeInput{VolumeId: awssdk.String(id), InstanceId: awssdk.String(s.AttachTo),
		Device: awssdk.String(s.Device)})
	if err != nil {
		return r, fmt.Errorf("made %s, but attaching it to %s failed; attach it from Disks: %s", id, s.AttachTo, plain(err))
	}
	r.Detail = "Attached to " + s.AttachTo + " as " + s.Device + " (AWS request " + requestID(at.ResultMetadata) + ")"
	return r, nil
}

// ModifyVolume grows a disk or changes its type or performance, while it
// stays in use.
func (p *Provider) ModifyVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, ch cloud.VolumeChange) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	in := &ec2.ModifyVolumeInput{VolumeId: awssdk.String(id)}
	if ch.SizeGB > 0 {
		in.Size = awssdk.Int32(ch.SizeGB)
	}
	if ch.Type != "" {
		in.VolumeType = types.VolumeType(ch.Type)
	}
	if ch.IOPS > 0 {
		in.Iops = awssdk.Int32(ch.IOPS)
	}
	if ch.Throughput > 0 {
		in.Throughput = awssdk.Int32(ch.Throughput)
	}
	out, err := c.ModifyVolume(ctx, in)
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: id, RequestID: requestID(out.ResultMetadata)}, nil
}

// AttachVolume attaches a disk to an instance in its zone.
func (p *Provider) AttachVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume, instance, device string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.AttachVolume(ctx, &ec2.AttachVolumeInput{VolumeId: awssdk.String(volume), InstanceId: awssdk.String(instance),
		Device: awssdk.String(device)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: volume, RequestID: requestID(out.ResultMetadata)}, nil
}

// DetachVolume detaches a disk. It is never forced: a forced detach can
// corrupt the file system on it.
func (p *Provider) DetachVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.DetachVolume(ctx, &ec2.DetachVolumeInput{VolumeId: awssdk.String(volume)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: volume, RequestID: requestID(out.ResultMetadata)}, nil
}

// DeleteVolume deletes a disk that is not attached, after a final snapshot
// when asked. AWS refuses an attached disk itself.
func (p *Provider) DeleteVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume string, finalSnapshot bool) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out := cloud.Result{ResourceID: volume}
	if finalSnapshot {
		s, err := c.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: awssdk.String(volume),
			Description:       awssdk.String("Final snapshot of " + volume + " before Argus deleted it"),
			TagSpecifications: nameTags(volume+" final", string(actor), nil, types.ResourceTypeSnapshot)})
		if err != nil {
			return out, fmt.Errorf("nothing was deleted: the final snapshot of %s failed: %s", volume, plain(err))
		}
		out.Snapshots = map[string]string{volume: awssdk.ToString(s.SnapshotId)}
	}
	res, err := c.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: awssdk.String(volume)})
	if err != nil {
		if len(out.Snapshots) > 0 {
			return out, fmt.Errorf("the final snapshot was taken (%s), but the disk was not deleted: %s", out.Snapshots[volume], plain(err))
		}
		return out, errors.New(plain(err))
	}
	out.RequestID = requestID(res.ResultMetadata)
	return out, nil
}

// CreateSnapshot snapshots a disk as it is now. The snapshot finishes in
// the background; the inventory shows its progress.
func (p *Provider) CreateSnapshot(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume, name string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: awssdk.String(volume),
		Description:       awssdk.String(name),
		TagSpecifications: nameTags(name, string(actor), nil, types.ResourceTypeSnapshot)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: awssdk.ToString(out.SnapshotId), RequestID: requestID(out.ResultMetadata)}, nil
}

// DeleteSnapshot deletes a snapshot. AWS refuses one an image uses.
func (p *Provider) DeleteSnapshot(ctx context.Context, a cloud.Account, actor cloud.Actor, region, snapshot string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: awssdk.String(snapshot)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: snapshot, RequestID: requestID(out.ResultMetadata)}, nil
}

// CreateImage makes an image of an instance and its disks. Without reboot
// the image is of disks that may be mid-write.
func (p *Provider) CreateImage(ctx context.Context, a cloud.Account, actor cloud.Actor, region, instance, name string, reboot bool) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	tags := map[string]string{"argus:from-instance": instance}
	out, err := c.CreateImage(ctx, &ec2.CreateImageInput{InstanceId: awssdk.String(instance), Name: awssdk.String(name),
		Description: awssdk.String("Made by Argus from " + instance), NoReboot: awssdk.Bool(!reboot),
		TagSpecifications: append(nameTags(name, string(actor), tags, types.ResourceTypeImage),
			nameTags(name, string(actor), tags, types.ResourceTypeSnapshot)...)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: awssdk.ToString(out.ImageId), RequestID: requestID(out.ResultMetadata)}, nil
}

// DeleteImage deregisters an image and, when asked, deletes the snapshots
// it was made of. Instances already launched from it are not touched.
func (p *Provider) DeleteImage(ctx context.Context, a cloud.Account, actor cloud.Actor, region, image string, withSnapshots bool) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	var snaps []string
	if withSnapshots {
		d, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{image}})
		if err != nil {
			return cloud.Result{}, errors.New(plain(err))
		}
		for _, im := range d.Images {
			for _, b := range im.BlockDeviceMappings {
				if b.Ebs != nil && awssdk.ToString(b.Ebs.SnapshotId) != "" {
					snaps = append(snaps, awssdk.ToString(b.Ebs.SnapshotId))
				}
			}
		}
	}
	res, err := c.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: awssdk.String(image)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	out := cloud.Result{ResourceID: image, RequestID: requestID(res.ResultMetadata)}
	var left []string
	for _, s := range snaps {
		if _, err := c.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: awssdk.String(s)}); err != nil {
			left = append(left, s+" ("+plain(err)+")")
		}
	}
	if len(snaps) > 0 {
		out.Detail = fmt.Sprintf("Deleted %d of its %d snapshots", len(snaps)-len(left), len(snaps))
	}
	if len(left) > 0 {
		return out, fmt.Errorf("the image was removed, but these snapshots were not deleted and are still billed: %s", strings.Join(left, ", "))
	}
	return out, nil
}

// AllocateAddress allocates an Elastic IP from Amazon's pool.
func (p *Provider) AllocateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, name string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: types.DomainTypeVpc,
		TagSpecifications: nameTags(name, string(actor), nil, types.ResourceTypeElasticIp)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: awssdk.ToString(out.AllocationId), RequestID: requestID(out.ResultMetadata),
		Detail: "Address " + awssdk.ToString(out.PublicIp)}, nil
}

// AssociateAddress points an Elastic IP at an instance. It never takes an
// address from another instance: AWS refuses when it is in use.
func (p *Provider) AssociateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address, instance string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.AssociateAddress(ctx, &ec2.AssociateAddressInput{AllocationId: awssdk.String(address),
		InstanceId: awssdk.String(instance), AllowReassociation: awssdk.Bool(false)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: address, RequestID: requestID(out.ResultMetadata)}, nil
}

// DisassociateAddress takes an Elastic IP off whatever it points at.
func (p *Provider) DisassociateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: []string{address}})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	if len(d.Addresses) == 0 || awssdk.ToString(d.Addresses[0].AssociationId) == "" {
		return cloud.Result{}, errors.New("the address is not attached to anything")
	}
	out, err := c.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{AssociationId: d.Addresses[0].AssociationId})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: address, RequestID: requestID(out.ResultMetadata)}, nil
}

// ReleaseAddress gives an Elastic IP back to Amazon. The address is gone
// for good; AWS refuses one that is attached.
func (p *Provider) ReleaseAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address string) (cloud.Result, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	out, err := c.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: awssdk.String(address)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: address, RequestID: requestID(out.ResultMetadata)}, nil
}

// actionNeeds is the manage permissions each storage and network action
// takes.
var actionNeeds = map[cloud.Action][]string{
	cloud.ActionVolumeCreate:        {"ec2:CreateVolume", "ec2:AttachVolume"},
	cloud.ActionVolumeGrow:          {"ec2:ModifyVolume"},
	cloud.ActionVolumeAttach:        {"ec2:AttachVolume"},
	cloud.ActionVolumeDetach:        {"ec2:DetachVolume"},
	cloud.ActionVolumeDelete:        {"ec2:DeleteVolume"},
	cloud.ActionSnapshotCreate:      {"ec2:CreateSnapshot"},
	cloud.ActionSnapshotDelete:      {"ec2:DeleteSnapshot"},
	cloud.ActionImageCreate:         {"ec2:CreateImage"},
	cloud.ActionImageDelete:         {"ec2:DeregisterImage", "ec2:DeleteSnapshot"},
	cloud.ActionAddressAllocate:     {"ec2:AllocateAddress"},
	cloud.ActionAddressAssociate:    {"ec2:AssociateAddress"},
	cloud.ActionAddressDisassociate: {"ec2:DisassociateAddress"},
	cloud.ActionAddressRelease:      {"ec2:ReleaseAddress"},
	cloud.ActionFirewallCreate: {"ec2:CreateSecurityGroup", "ec2:AuthorizeSecurityGroupIngress",
		"ec2:AuthorizeSecurityGroupEgress"},
	cloud.ActionFirewallRules: {"ec2:AuthorizeSecurityGroupIngress", "ec2:AuthorizeSecurityGroupEgress",
		"ec2:RevokeSecurityGroupIngress", "ec2:RevokeSecurityGroupEgress"},
	cloud.ActionFirewallDelete:    {"ec2:DeleteSecurityGroup"},
	cloud.ActionInstanceFirewalls: {"ec2:ModifyInstanceAttribute"},
	cloud.ActionTargetRegister:    {"elasticloadbalancing:RegisterTargets"},
	cloud.ActionTargetDeregister:  {"elasticloadbalancing:DeregisterTargets"},
}

// Needs is the permissions an action takes.
func (p *Provider) Needs(a cloud.Action) []string { return actionNeeds[a] }

// ManageMissing reads the manage role's policy with the read role (which
// may iam:GetRolePolicy) and answers the ManagePolicy actions it lacks. A
// stack deployed before this phase lacks the storage ones.
func (p *Provider) ManageMissing(ctx context.Context, read cloud.Account, manageRoleARN string) ([]string, error) {
	cfg, err := p.assumed(ctx, read, cloud.SyncActor, "us-east-1")
	if err != nil {
		return nil, err
	}
	name := manageRoleARN[strings.LastIndex(manageRoleARN, "/")+1:]
	out, err := iam.NewFromConfig(cfg).GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: awssdk.String(name),
		PolicyName: awssdk.String("argus-manage")})
	if err != nil {
		return nil, errors.New(plain(err))
	}
	return MissingFrom(awssdk.ToString(out.PolicyDocument))
}

// MissingFrom is the ManagePolicy actions a policy document (URL-encoded,
// as IAM returns it) does not allow.
func MissingFrom(doc string) ([]string, error) {
	if d, err := url.QueryUnescape(doc); err == nil {
		doc = d
	}
	var pol struct {
		Statement []struct {
			Effect string          `json:"Effect"`
			Action json.RawMessage `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &pol); err != nil {
		return nil, fmt.Errorf("the manage role's policy is not JSON: %w", err)
	}
	have := map[string]bool{}
	for _, s := range pol.Statement {
		if s.Effect != "Allow" {
			continue
		}
		var list []string
		if err := json.Unmarshal(s.Action, &list); err != nil {
			var one string
			if json.Unmarshal(s.Action, &one) == nil {
				list = []string{one}
			}
		}
		for _, a := range list {
			have[strings.ToLower(a)] = true
		}
	}
	missing := []string{}
	for _, want := range ManagePolicy {
		if !have[strings.ToLower(want)] {
			missing = append(missing, want)
		}
	}
	return missing, nil
}

var (
	_ cloud.Storage       = (*Provider)(nil)
	_ cloud.PolicyChecker = (*Provider)(nil)
)

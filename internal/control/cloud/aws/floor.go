package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
The launch template and Auto Scaling group under a scale group, made through
the manage role as whoever made or changed the group (argus-scale when the
scaler makes one once its image is ready). See cloud/floor.go.
*/

// floorPolicyName is the one target-tracking policy Argus puts on a group.
const floorPolicyName = "argus-track"

// floorWarmup is how long a new server's metrics are left out of the
// group's, and the health check grace: long enough for the agent and the
// application to start.
const floorWarmup = 300

// CreateFloor makes the launch template, then the Auto Scaling group that
// launches from it into the target group.
func (p *Provider) CreateFloor(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, s cloud.FloorSpec) (cloud.Result, error) {
	if err := s.Check(); err != nil {
		return cloud.Result{}, err
	}
	cfg, err := p.assumed(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	c := ec2.NewFromConfig(cfg)
	img, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{s.Image}})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	if len(img.Images) == 0 {
		return cloud.Result{}, fmt.Errorf("image %s is not available in %s", s.Image, region)
	}
	tags := []types.Tag{}
	for _, k := range sortedKeys(s.Tags) {
		tags = append(tags, types.Tag{Key: awssdk.String(k), Value: awssdk.String(s.Tags[k])})
	}
	data := &types.RequestLaunchTemplateData{
		ImageId:      awssdk.String(s.Image),
		InstanceType: types.InstanceType(s.Size),
		BlockDeviceMappings: []types.LaunchTemplateBlockDeviceMappingRequest{{
			DeviceName: img.Images[0].RootDeviceName,
			Ebs: &types.LaunchTemplateEbsBlockDeviceRequest{VolumeSize: awssdk.Int32(s.DiskGB), VolumeType: types.VolumeTypeGp3,
				Encrypted: awssdk.Bool(true), DeleteOnTermination: awssdk.Bool(true)},
		}},
		NetworkInterfaces: []types.LaunchTemplateInstanceNetworkInterfaceSpecificationRequest{{
			DeviceIndex: awssdk.Int32(0), Groups: s.Firewalls,
			AssociatePublicIpAddress: awssdk.Bool(false), DeleteOnTermination: awssdk.Bool(true),
		}},
		MetadataOptions: &types.LaunchTemplateInstanceMetadataOptionsRequest{HttpTokens: types.LaunchTemplateHttpTokensStateRequired,
			HttpEndpoint: types.LaunchTemplateInstanceMetadataEndpointStateEnabled},
		UserData: awssdk.String(base64.StdEncoding.EncodeToString([]byte(s.UserData))),
	}
	if len(tags) > 0 {
		data.TagSpecifications = []types.LaunchTemplateTagSpecificationRequest{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		}
	}
	lt, err := c.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: awssdk.String(s.Template), LaunchTemplateData: data,
		VersionDescription: awssdk.String("Made by Argus for scale group " + strings.TrimPrefix(s.Name, "argus-")),
		TagSpecifications: []types.TagSpecification{{ResourceType: types.ResourceTypeLaunchTemplate,
			Tags: append([]types.Tag{{Key: awssdk.String("argus:made-by"), Value: awssdk.String(string(actor))}}, tags...)}},
	})
	if err != nil {
		return cloud.Result{}, errors.New("making the launch template: " + plain(err))
	}
	tplID := awssdk.ToString(lt.LaunchTemplate.LaunchTemplateId)
	var asTags []astypes.Tag
	for _, k := range sortedKeys(s.Tags) {
		asTags = append(asTags, astypes.Tag{Key: awssdk.String(k), Value: awssdk.String(s.Tags[k]), PropagateAtLaunch: awssdk.Bool(false)})
	}
	out, err := autoscaling.NewFromConfig(cfg).CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
		AutoScalingGroupName: awssdk.String(s.Name),
		LaunchTemplate: &astypes.LaunchTemplateSpecification{LaunchTemplateId: awssdk.String(tplID),
			Version: awssdk.String("$Latest")},
		MinSize: awssdk.Int32(s.Count.Min), MaxSize: awssdk.Int32(s.Count.Max), DesiredCapacity: awssdk.Int32(s.Count.Desired),
		VPCZoneIdentifier:      awssdk.String(strings.Join(s.Subnets, ",")),
		TargetGroupARNs:        []string{s.TargetGroup},
		HealthCheckType:        awssdk.String("ELB"),
		HealthCheckGracePeriod: awssdk.Int32(floorWarmup),
		DefaultInstanceWarmup:  awssdk.Int32(floorWarmup),
		Tags:                   asTags,
	})
	if err != nil {
		return cloud.Result{Detail: tplID}, errors.New("made the launch template " + tplID + ", but not the Auto Scaling group: " + plain(err))
	}
	return cloud.Result{ResourceID: s.Name, Detail: tplID, RequestID: requestID(out.ResultMetadata)}, nil
}

// SetFloorPolicy puts, or replaces, the group's target-tracking policy.
// Off keeps it there without acting, so turning it back on is the same
// call.
func (p *Provider) SetFloorPolicy(ctx context.Context, a cloud.Account, actor cloud.Actor, region, group string, fp cloud.FloorPolicy) (cloud.Result, error) {
	if err := fp.Track.Check(); err != nil {
		return cloud.Result{}, err
	}
	cfg, err := p.assumed(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	metric := &astypes.PredefinedMetricSpecification{PredefinedMetricType: astypes.MetricTypeASGAverageCPUUtilization}
	if fp.Track.Metric == cloud.TrackRequests {
		if fp.Label == "" {
			return cloud.Result{}, errors.New("requests per server need an application load balancer in front of the target group")
		}
		metric = &astypes.PredefinedMetricSpecification{PredefinedMetricType: astypes.MetricTypeALBRequestCountPerTarget,
			ResourceLabel: awssdk.String(fp.Label)}
	}
	out, err := autoscaling.NewFromConfig(cfg).PutScalingPolicy(ctx, &autoscaling.PutScalingPolicyInput{
		AutoScalingGroupName: awssdk.String(group), PolicyName: awssdk.String(floorPolicyName),
		PolicyType: awssdk.String("TargetTrackingScaling"), Enabled: awssdk.Bool(fp.On),
		TargetTrackingConfiguration: &astypes.TargetTrackingConfiguration{
			PredefinedMetricSpecification: metric, TargetValue: awssdk.Float64(fp.Track.Target)},
	})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: group, RequestID: requestID(out.ResultMetadata)}, nil
}

// FloorInstance reads one server's Auto Scaling group, from the tag AWS
// puts on everything a group launches, and when it started.
func (p *Provider) FloorInstance(ctx context.Context, a cloud.Account, actor cloud.Actor, region, instance string) (cloud.FloorInstance, error) {
	c, err := p.client(ctx, a, actor, region)
	if err != nil {
		return cloud.FloorInstance{}, err
	}
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instance}})
	if err != nil {
		return cloud.FloorInstance{}, errors.New(plain(err))
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			f := cloud.FloorInstance{LaunchedAt: awssdk.ToTime(i.LaunchTime)}
			if i.State != nil {
				f.State = string(i.State.Name)
			}
			for _, t := range i.Tags {
				if awssdk.ToString(t.Key) == "aws:autoscaling:groupName" {
					f.Group = awssdk.ToString(t.Value)
				}
			}
			return f, nil
		}
	}
	return cloud.FloorInstance{}, fmt.Errorf("no instance %s in %s", instance, region)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "Name" && !strings.HasPrefix(k, "aws:") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

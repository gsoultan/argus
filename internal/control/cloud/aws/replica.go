package aws

import (
	"context"
	"errors"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
RDS read replicas, made and removed through the manage role. See
cloud/replica.go.
*/

// CreateReplica asks RDS for a read replica of s.Source in the same region,
// with deletion protection on and the primary's other settings (subnet
// group, security groups, storage, encryption key, parameter group) as RDS
// copies them.
func (p *Provider) CreateReplica(ctx context.Context, a cloud.Account, actor cloud.Actor, s cloud.ReplicaSpec) (cloud.Result, error) {
	if err := s.Check(); err != nil {
		return cloud.Result{}, err
	}
	cfg, err := p.assumed(ctx, a, actor, s.Region)
	if err != nil {
		return cloud.Result{}, err
	}
	tags := []rdstypes.Tag{{Key: awssdk.String(cloud.ReplicaTag), Value: awssdk.String(s.Source)}}
	for _, k := range sortedKeys(s.Tags) {
		if k == cloud.ReplicaTag {
			continue
		}
		tags = append(tags, rdstypes.Tag{Key: awssdk.String(k), Value: awssdk.String(s.Tags[k])})
	}
	in := &rds.CreateDBInstanceReadReplicaInput{
		DBInstanceIdentifier:       awssdk.String(s.Name),
		SourceDBInstanceIdentifier: awssdk.String(s.Source),
		DBInstanceClass:            awssdk.String(s.Size),
		DeletionProtection:         awssdk.Bool(true),
		CopyTagsToSnapshot:         awssdk.Bool(true),
		Tags:                       tags,
	}
	if s.Zone != "" {
		in.AvailabilityZone = awssdk.String(s.Zone)
	}
	out, err := rds.NewFromConfig(cfg).CreateDBInstanceReadReplica(ctx, in)
	if err != nil {
		msg := plain(err)
		if strings.Contains(msg, "KMS") || strings.Contains(msg, "kms") {
			msg += " (the primary is encrypted with a key of your own, and the manage role may not use it; let ArgusManage use that key in its key policy)"
		}
		return cloud.Result{}, errors.New(msg)
	}
	res := cloud.Result{ResourceID: s.Name, RequestID: requestID(out.ResultMetadata)}
	if out.DBInstance != nil && out.DBInstance.DBInstanceArn != nil {
		res.ResourceID = awssdk.ToString(out.DBInstance.DBInstanceArn)
	}
	return res, nil
}

// RemoveReplica deletes a replica Argus made. The manage role may only
// change or delete a database tagged argus:replica-of, so this cannot reach
// a primary even if asked to.
func (p *Provider) RemoveReplica(ctx context.Context, a cloud.Account, actor cloud.Actor, region, name string) (cloud.Result, error) {
	if !cloud.ValidDBName(name) {
		return cloud.Result{}, errors.New("name the replica to remove")
	}
	cfg, err := p.assumed(ctx, a, actor, region)
	if err != nil {
		return cloud.Result{}, err
	}
	c := rds.NewFromConfig(cfg)
	d, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: awssdk.String(name)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	if len(d.DBInstances) != 1 || awssdk.ToString(d.DBInstances[0].ReadReplicaSourceDBInstanceIdentifier) == "" {
		return cloud.Result{}, errors.New(name + " is not a read replica; Argus removes only replicas")
	}
	if awssdk.ToBool(d.DBInstances[0].DeletionProtection) {
		if _, err := c.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{DBInstanceIdentifier: awssdk.String(name),
			DeletionProtection: awssdk.Bool(false), ApplyImmediately: awssdk.Bool(true)}); err != nil {
			return cloud.Result{}, errors.New("turning off deletion protection: " + plain(err))
		}
	}
	out, err := c.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{DBInstanceIdentifier: awssdk.String(name),
		SkipFinalSnapshot: awssdk.Bool(true), DeleteAutomatedBackups: awssdk.Bool(true)})
	if err != nil {
		return cloud.Result{}, errors.New(plain(err))
	}
	return cloud.Result{ResourceID: awssdk.ToString(d.DBInstances[0].DBInstanceArn), RequestID: requestID(out.ResultMetadata)}, nil
}

var _ cloud.Replicas = (*Provider)(nil)

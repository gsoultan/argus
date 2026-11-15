package aws

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
The account-wide read behind Risks & waste: the root user and IAM users
from the credential report, buckets and whether public access is blocked,
and RDS databases in every enabled region. All of it is in the read role
already (iam:Get*, iam:List*, iam:GenerateCredentialReport,
s3:ListAllMyBuckets, s3:GetBucketLocation, s3:GetBucketPublicAccessBlock,
rds:Describe*). It reads settings, never data: no object is listed or read.
*/

// AccountInventory reads the account-wide kinds. Each part that fails is
// left out of the snapshot's Kinds and named in the error.
func (p *Provider) AccountInventory(ctx context.Context, a cloud.Account, regions []string) (cloud.Snapshot, error) {
	home := "us-east-1"
	if len(regions) > 0 {
		home = regions[0]
	}
	cfg, err := p.assumed(ctx, a, cloud.SyncActor, home)
	if err != nil {
		return cloud.Snapshot{}, err
	}
	snap := cloud.Snapshot{Region: cloud.GlobalRegion, ReadAt: time.Now().UTC()}
	var fails []string

	if rs, err := credentialReport(ctx, iam.NewFromConfig(cfg), a.AccountID); err != nil {
		fails = append(fails, "people and keys: "+plain(err))
	} else {
		snap.Kinds = append(snap.Kinds, cloud.KindAccount, cloud.KindUser)
		snap.Resources = append(snap.Resources, rs...)
	}

	if rs, err := buckets(ctx, cfg); err != nil {
		fails = append(fails, "buckets: "+plain(err))
	} else {
		snap.Kinds = append(snap.Kinds, cloud.KindBucket)
		snap.Resources = append(snap.Resources, rs...)
	}

	var dbs []cloud.Resource
	dbOK := true
	for _, region := range regions {
		c := cfg.Copy()
		c.Region = region
		rs, err := databases(ctx, rds.NewFromConfig(c), region)
		if err != nil {
			dbOK = false
			fails = append(fails, "databases in "+region+": "+plain(err))
			continue
		}
		dbs = append(dbs, rs...)
	}
	if dbOK {
		snap.Kinds = append(snap.Kinds, cloud.KindDatabase)
		snap.Resources = append(snap.Resources, dbs...)
	}

	if len(fails) > 0 {
		return snap, errors.New(strings.Join(fails, "; "))
	}
	return snap, nil
}

// errReportNotReady is a credential report AWS is still making; the next
// read picks it up.
var errReportNotReady = errors.New("the credential report is being made")

func credentialReport(ctx context.Context, c *iam.Client, accountID string) ([]cloud.Resource, error) {
	if _, err := c.GenerateCredentialReport(ctx, &iam.GenerateCredentialReportInput{}); err != nil {
		return nil, err
	}
	out, err := c.GetCredentialReport(ctx, &iam.GetCredentialReportInput{})
	if err != nil {
		var nr interface{ ErrorCode() string }
		if errors.As(err, &nr) && strings.HasPrefix(nr.ErrorCode(), "ReportInProgress") {
			return nil, errReportNotReady
		}
		return nil, err
	}
	return ParseCredentialReport(out.Content, accountID)
}

// ParseCredentialReport turns IAM's credential report (CSV) into the
// account's root settings and one resource per IAM user.
func ParseCredentialReport(content []byte, accountID string) ([]cloud.Resource, error) {
	r := csv.NewReader(bytes.NewReader(content))
	head, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("credential report: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	get := func(row []string, k string) string {
		if i, ok := col[k]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	var out []cloud.Resource
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("credential report: %w", err)
		}
		user := get(row, "user")
		if user == "<root_account>" {
			keys := 0
			for _, k := range []string{"access_key_1_active", "access_key_2_active"} {
				if get(row, k) == "true" {
					keys++
				}
			}
			out = append(out, cloud.Resource{Kind: cloud.KindAccount, ID: accountID, Name: "Root user", Region: cloud.GlobalRegion,
				Details: map[string]string{"rootMfa": yesNo(get(row, "mfa_active") == "true"), "rootKeys": strconv.Itoa(keys)}})
			continue
		}
		d := map[string]string{
			"arn":      get(row, "arn"),
			"password": yesNo(get(row, "password_enabled") == "true"),
			"mfa":      yesNo(get(row, "mfa_active") == "true"),
		}
		set(d, "passwordLastUsed", reportTime(get(row, "password_last_used")))
		for _, k := range []string{"1", "2"} {
			if get(row, "access_key_"+k+"_active") != "true" {
				continue
			}
			d["key"+k+"Active"] = "yes"
			set(d, "key"+k+"Rotated", reportTime(get(row, "access_key_"+k+"_last_rotated")))
			set(d, "key"+k+"LastUsed", reportTime(get(row, "access_key_"+k+"_last_used_date")))
		}
		res := cloud.Resource{Kind: cloud.KindUser, ID: get(row, "arn"), Name: user, Region: cloud.GlobalRegion, Details: d}
		if t := reportTime(get(row, "user_creation_time")); t != "" {
			if at, err := time.Parse(time.RFC3339, t); err == nil {
				res.CreatedAt = &at
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// reportTime is a credential report time as RFC 3339, "" for N/A and the
// like.
func reportTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func buckets(ctx context.Context, cfg awssdk.Config) ([]cloud.Resource, error) {
	c := s3.NewFromConfig(cfg)
	list, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}
	var out []cloud.Resource
	for _, b := range list.Buckets {
		name := awssdk.ToString(b.Name)
		r := cloud.Resource{Kind: cloud.KindBucket, ID: name, Name: name, Region: cloud.GlobalRegion, CreatedAt: b.CreationDate,
			Details: map[string]string{}}
		region := awssdk.ToString(b.BucketRegion)
		if region == "" {
			if loc, err := c.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: &name}); err == nil {
				region = string(loc.LocationConstraint)
				if region == "" {
					region = "us-east-1"
				}
			}
		}
		if region != "" {
			r.Details["region"] = region
		}
		rc := c
		if region != "" && region != cfg.Region {
			cc := cfg.Copy()
			cc.Region = region
			rc = s3.NewFromConfig(cc)
		}
		pab, err := rc.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: &name})
		var code interface{ ErrorCode() string }
		switch {
		case err == nil && pab.PublicAccessBlockConfiguration != nil:
			r.Details["publicBlock"] = BlockState(pab.PublicAccessBlockConfiguration.BlockPublicAcls, pab.PublicAccessBlockConfiguration.IgnorePublicAcls,
				pab.PublicAccessBlockConfiguration.BlockPublicPolicy, pab.PublicAccessBlockConfiguration.RestrictPublicBuckets)
		case errors.As(err, &code) && code.ErrorCode() == "NoSuchPublicAccessBlockConfiguration":
			r.Details["publicBlock"] = "none"
		}
		// Any other error leaves it unknown, and no finding is made.
		out = append(out, r)
	}
	return out, nil
}

// BlockState says how much of a bucket's public access is blocked: "all",
// "partial" or "none".
func BlockState(flags ...*bool) string {
	n := 0
	for _, f := range flags {
		if awssdk.ToBool(f) {
			n++
		}
	}
	switch n {
	case len(flags):
		return "all"
	case 0:
		return "none"
	}
	return "partial"
}

func databases(ctx context.Context, c *rds.Client, region string) ([]cloud.Resource, error) {
	var out []cloud.Resource
	pg := rds.NewDescribeDBInstancesPaginator(c, &rds.DescribeDBInstancesInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range page.DBInstances {
			id := awssdk.ToString(d.DBInstanceIdentifier)
			r := cloud.Resource{Kind: cloud.KindDatabase, ID: awssdk.ToString(d.DBInstanceArn), Name: id, Region: region,
				State: awssdk.ToString(d.DBInstanceStatus), Size: awssdk.ToString(d.DBInstanceClass), CreatedAt: d.InstanceCreateTime,
				Details: map[string]string{}}
			if r.ID == "" {
				r.ID = id
			}
			set(r.Details, "engine", awssdk.ToString(d.Engine))
			r.Details["public"] = yesNo(awssdk.ToBool(d.PubliclyAccessible))
			r.Details["encrypted"] = yesNo(awssdk.ToBool(d.StorageEncrypted))
			if d.Endpoint != nil {
				set(r.Details, "port", strconv.Itoa(int(awssdk.ToInt32(d.Endpoint.Port))))
				set(r.Details, "address", awssdk.ToString(d.Endpoint.Address))
			}
			for _, g := range d.VpcSecurityGroups {
				r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindFirewall, ID: awssdk.ToString(g.VpcSecurityGroupId)})
			}
			out = append(out, r)
		}
	}
	return out, nil
}

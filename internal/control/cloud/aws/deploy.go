package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Making the roles without the person opening the AWS console. Possible only
in the account argus-control's own identity is in, and only when someone gave
that identity the rights to make roles: Argus never asks for anyone's keys
to do it. The stack is the same argus-access stack the person would make, so
allowing changes later, or deleting it, works the same either way. The
identity that makes it is argus-control's, so the person who asked is put on
the stack as a tag and in the audit log.
*/

// StackName is the stack the template is deployed as, whoever deploys it.
const StackName = "argus-access"

// homeAccount is the account the base identity is in, asked of STS.
func (p *Provider) homeAccount(ctx context.Context) (awssdk.Config, string, error) {
	base, err := p.base(ctx)
	if err != nil {
		return awssdk.Config{}, "", err
	}
	who, err := sts.NewFromConfig(base).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return awssdk.Config{}, "", fmt.Errorf("%w: %s", cloud.ErrNoBase, plain(err))
	}
	return base, awssdk.ToString(who.Account), nil
}

func (p *Provider) stackClient(ctx context.Context, accountID string) (*cloudformation.Client, error) {
	base, home, err := p.homeAccount(ctx)
	if err != nil {
		return nil, err
	}
	if home != accountID {
		return nil, fmt.Errorf("%w (%s, not %s)", cloud.ErrNotHomeAccount, home, accountID)
	}
	return cloudformation.NewFromConfig(base), nil
}

// DeployAccess creates the argus-access stack in the base identity's own
// account, or updates it when it is already there. It returns as soon as
// CloudFormation has taken it; AccessStack follows it from there.
func (p *Provider) DeployAccess(ctx context.Context, accountID, template string, actor cloud.Actor) (cloud.StackState, error) {
	if !actor.Valid() {
		return cloud.StackState{}, fmt.Errorf("%q cannot be named on the stack", actor)
	}
	c, err := p.stackClient(ctx, accountID)
	if err != nil {
		return cloud.StackState{}, err
	}
	tags := []cftypes.Tag{
		{Key: awssdk.String("argus:requested-by"), Value: awssdk.String(string(actor))},
		{Key: awssdk.String("argus:made-by"), Value: awssdk.String("argus-control")},
	}
	caps := []cftypes.Capability{cftypes.CapabilityCapabilityNamedIam}
	cur, err := describeStack(ctx, c)
	if err != nil {
		return cloud.StackState{}, err
	}
	switch {
	case cur == nil:
		_, err = c.CreateStack(ctx, &cloudformation.CreateStackInput{
			StackName: awssdk.String(StackName), TemplateBody: awssdk.String(template),
			Capabilities: caps, Tags: tags, OnFailure: cftypes.OnFailureRollback,
		})
		if err != nil {
			return cloud.StackState{}, deployRefused(err)
		}
		return cloud.StackState{Status: string(cftypes.StackStatusCreateInProgress)}, nil
	case cur.StackStatus == cftypes.StackStatusRollbackComplete || cur.StackStatus == cftypes.StackStatusRollbackFailed ||
		cur.StackStatus == cftypes.StackStatusCreateFailed:
		// A stack that never got made holds nothing, but deleting it is still
		// the person's call, not Argus's.
		return cloud.StackState{}, fmt.Errorf("the %s stack in this account failed when it was first made (%s); delete it in CloudFormation, then try again",
			StackName, cur.StackStatus)
	case strings.HasSuffix(string(cur.StackStatus), "_IN_PROGRESS"):
		return stackState(ctx, c, cur), nil
	}
	_, err = c.UpdateStack(ctx, &cloudformation.UpdateStackInput{
		StackName: awssdk.String(StackName), TemplateBody: awssdk.String(template), Capabilities: caps, Tags: tags,
	})
	if err != nil {
		if strings.Contains(err.Error(), "No updates are to be performed") {
			return cloud.StackState{Status: string(cur.StackStatus), Done: true}, nil
		}
		return cloud.StackState{}, deployRefused(err)
	}
	return cloud.StackState{Status: string(cftypes.StackStatusUpdateInProgress)}, nil
}

// AccessStack reports the argus-access stack in the base identity's own
// account, with the first failure's reason when it failed.
func (p *Provider) AccessStack(ctx context.Context, accountID string) (cloud.StackState, error) {
	c, err := p.stackClient(ctx, accountID)
	if err != nil {
		return cloud.StackState{}, err
	}
	cur, err := describeStack(ctx, c)
	if err != nil || cur == nil {
		return cloud.StackState{}, err
	}
	return stackState(ctx, c, cur), nil
}

func describeStack(ctx context.Context, c *cloudformation.Client) (*cftypes.Stack, error) {
	out, err := c.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: awssdk.String(StackName)})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, deployRefused(err)
	}
	if len(out.Stacks) == 0 {
		return nil, nil
	}
	return &out.Stacks[0], nil
}

func stackState(ctx context.Context, c *cloudformation.Client, s *cftypes.Stack) cloud.StackState {
	status := string(s.StackStatus)
	st := cloud.StackState{Status: status}
	switch {
	case s.StackStatus == cftypes.StackStatusCreateComplete, s.StackStatus == cftypes.StackStatusUpdateComplete:
		st.Done = true
	case strings.HasSuffix(status, "_IN_PROGRESS"):
		// Still going, even when it is rolling back; say why once it is.
		if strings.Contains(status, "ROLLBACK") {
			st.Reason = firstFailure(ctx, c)
		}
	default:
		// Anything else that has stopped did not make the roles as asked:
		// a rollback, a failure, or a deleted stack.
		st.Failed = true
		st.Reason = firstFailure(ctx, c)
	}
	return st
}

// firstFailure is the reason of the earliest failed resource in the
// stack's latest events: the one that made the rest roll back.
func firstFailure(ctx context.Context, c *cloudformation.Client) string {
	out, err := c.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: awssdk.String(StackName)})
	if err != nil {
		return ""
	}
	reason := ""
	// Newest first; the last failure seen walking back is the first one.
	for _, e := range out.StackEvents {
		if strings.HasSuffix(string(e.ResourceStatus), "_FAILED") && awssdk.ToString(e.ResourceStatusReason) != "" &&
			!strings.Contains(awssdk.ToString(e.ResourceStatusReason), "Resource creation cancelled") {
			reason = awssdk.ToString(e.ResourceStatusReason)
		}
		if awssdk.ToString(e.LogicalResourceId) == StackName && strings.HasSuffix(string(e.ResourceStatus), "_IN_PROGRESS") &&
			awssdk.ToString(e.ResourceStatusReason) == "User Initiated" {
			break
		}
	}
	return reason
}

// deployRefused says what the identity lacked, in a sentence the person
// can hand to whoever runs the account.
func deployRefused(err error) error {
	msg := plain(err)
	if strings.Contains(msg, "AccessDenied") || strings.Contains(msg, "not authorized") {
		return errors.New("argus-control's own identity may not make the roles (" + msg + "); make them yourself with the steps below, " +
			"or give that identity cloudformation:CreateStack, UpdateStack, DescribeStacks and DescribeStackEvents and the IAM role permissions the template needs")
	}
	return errors.New(msg)
}

var _ cloud.Deployer = (*Provider)(nil)

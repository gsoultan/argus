package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
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
	c, _, _, err := p.homeClients(ctx, accountID)
	return c, err
}

// homeClients are the clients for the base identity's own account, with
// the identity's ARN as a policy names it.
func (p *Provider) homeClients(ctx context.Context, accountID string) (*cloudformation.Client, awssdk.Config, string, error) {
	base, err := p.base(ctx)
	if err != nil {
		return nil, awssdk.Config{}, "", err
	}
	who, err := sts.NewFromConfig(base).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, awssdk.Config{}, "", fmt.Errorf("%w: %s", cloud.ErrNoBase, plain(err))
	}
	if home := awssdk.ToString(who.Account); home != accountID {
		return nil, awssdk.Config{}, "", fmt.Errorf("%w (%s, not %s)", cloud.ErrNotHomeAccount, home, accountID)
	}
	return cloudformation.NewFromConfig(base), base, TrustPrincipal(awssdk.ToString(who.Arn)), nil
}

// deployIAMActions are what CloudFormation does with the identity's rights
// to make, change and, on a failure, remove the roles. Every one is needed:
// an identity that may create a role but not read it back leaves a stack
// that can neither finish nor roll back.
var deployIAMActions = []string{
	"iam:CreateRole", "iam:GetRole", "iam:DeleteRole", "iam:UpdateRole", "iam:UpdateAssumeRolePolicy",
	"iam:PutRolePolicy", "iam:GetRolePolicy", "iam:DeleteRolePolicy",
	"iam:AttachRolePolicy", "iam:DetachRolePolicy", "iam:TagRole", "iam:UntagRole",
}

// deployStackActions are the stack's own.
var deployStackActions = []string{
	"cloudformation:CreateStack", "cloudformation:UpdateStack", "cloudformation:DeleteStack",
	"cloudformation:DescribeStacks", "cloudformation:DescribeStackEvents",
}

// deployRoles are the roles the template makes.
func deployRoles(manage bool) []string {
	if manage {
		return []string{ReadRoleName, ManageRoleName, ConsoleReadRoleName, ConsoleOperateRoleName}
	}
	return []string{ReadRoleName}
}

// DeployRights is the policy the base identity needs for DeployAccess, as
// JSON an administrator can attach to it. Limiting it to Argus* roles does
// not make it safe to keep: an identity that may make a role and set its
// policy can make one with any permission. The console says to remove it
// once the roles are made.
func DeployRights(accountID string) string {
	return `{"Version":"2012-10-17","Statement":[` +
		`{"Effect":"Allow","Action":["` + strings.Join(deployIAMActions, `","`) + `"],"Resource":"arn:aws:iam::` + accountID + `:role/Argus*"},` +
		`{"Effect":"Allow","Action":["` + strings.Join(deployStackActions, `","`) + `"],"Resource":"arn:aws:cloudformation:*:` + accountID + `:stack/` + StackName + `/*"},` +
		`{"Effect":"Allow","Action":"iam:SimulatePrincipalPolicy","Resource":"*"}]}`
}

// missingRights asks IAM's policy simulator which of the actions the stack
// needs the base identity may not take. Argus checks before it creates
// anything, so a missing right means no stack rather than a stuck one.
func missingRights(ctx context.Context, cfg awssdk.Config, principal, accountID string, manage bool) ([]string, error) {
	c := iam.NewFromConfig(cfg)
	var roles []string
	for _, r := range deployRoles(manage) {
		roles = append(roles, "arn:aws:iam::"+accountID+":role/"+r)
	}
	stack := fmt.Sprintf("arn:aws:cloudformation:%s:%s:stack/%s/00000000-0000-0000-0000-000000000000", cfg.Region, accountID, StackName)
	var missing []string
	for _, q := range []struct {
		actions, resources []string
	}{{deployIAMActions, roles}, {deployStackActions, []string{stack}}} {
		pg := iam.NewSimulatePrincipalPolicyPaginator(c, &iam.SimulatePrincipalPolicyInput{
			PolicySourceArn: awssdk.String(principal), ActionNames: q.actions, ResourceArns: q.resources,
		})
		for pg.HasMorePages() {
			out, err := pg.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, r := range out.EvaluationResults {
				if !allowed(r) {
					missing = append(missing, awssdk.ToString(r.EvalActionName))
				}
			}
		}
	}
	return missing, nil
}

// allowed is an evaluation that allows the action on every resource asked.
func allowed(r iamtypes.EvaluationResult) bool {
	if len(r.ResourceSpecificResults) == 0 {
		return r.EvalDecision == iamtypes.PolicyEvaluationDecisionTypeAllowed
	}
	for _, rr := range r.ResourceSpecificResults {
		if rr.EvalResourceDecision != iamtypes.PolicyEvaluationDecisionTypeAllowed {
			return false
		}
	}
	return true
}

// DeployAccess creates the argus-access stack in the base identity's own
// account, or updates it when it is already there. It returns as soon as
// CloudFormation has taken it; AccessStack follows it from there.
func (p *Provider) DeployAccess(ctx context.Context, accountID, template string, manage bool, actor cloud.Actor) (cloud.StackState, error) {
	if !actor.Valid() {
		return cloud.StackState{}, fmt.Errorf("%q cannot be named on the stack", actor)
	}
	c, cfg, principal, err := p.homeClients(ctx, accountID)
	if err != nil {
		return cloud.StackState{}, err
	}
	missing, err := missingRights(ctx, cfg, principal, accountID, manage)
	if err != nil {
		return cloud.StackState{}, &cloud.DeployRefusal{Reason: "Argus could not check its own permissions first (" + plain(err) +
			"), so it made nothing rather than risk a stack that cannot finish.", Rights: DeployRights(accountID)}
	}
	if len(missing) > 0 {
		return cloud.StackState{}, &cloud.DeployRefusal{Reason: principal + " may not " + strings.Join(missing, ", ") +
			", so Argus made nothing.", Missing: missing, Rights: DeployRights(accountID)}
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
			// The rights to remove the roles were checked above, so a failed
			// create removes itself rather than waiting for someone.
			Capabilities: caps, Tags: tags, OnFailure: cftypes.OnFailureDelete,
		})
		if err != nil {
			return cloud.StackState{}, deployRefused(err)
		}
		return cloud.StackState{Status: string(cftypes.StackStatusCreateInProgress)}, nil
	case StuckStatus(string(cur.StackStatus)):
		// Deleting it is the person's call, not Argus's, and a stack stuck
		// this way usually needs more rights than Argus has.
		return cloud.StackState{}, &cloud.DeployRefusal{Reason: fmt.Sprintf("the %s stack in this account is stuck at %s. "+
			"Delete it as an administrator, or run the CloudShell command below, which deletes a stuck stack first.", StackName, cur.StackStatus)}
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

// StuckStatus is a stack that never finished being made and cannot be
// updated: it has to be deleted before the roles can be made again.
func StuckStatus(s string) bool {
	switch cftypes.StackStatus(s) {
	case cftypes.StackStatusRollbackComplete, cftypes.StackStatusRollbackFailed, cftypes.StackStatusCreateFailed,
		cftypes.StackStatusDeleteFailed:
		return true
	}
	return false
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

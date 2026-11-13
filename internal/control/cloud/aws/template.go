package aws

import (
	"fmt"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// ReadPolicy is what the read role may do: describe and list, never read
// data. No s3:GetObject, no secrets, no parameter values, no logs content.
// It covers the inventory the later phases read too, so connecting an
// account once is enough for every read page.
var ReadPolicy = []string{
	"ec2:Describe*",
	"elasticloadbalancing:Describe*",
	"autoscaling:Describe*",
	"route53:Get*", "route53:List*",
	"s3:ListAllMyBuckets", "s3:GetBucketLocation", "s3:GetBucketTagging", "s3:GetBucketVersioning",
	"s3:GetBucketPublicAccessBlock", "s3:GetEncryptionConfiguration", "s3:GetLifecycleConfiguration",
	"rds:Describe*", "rds:ListTagsForResource",
	"iam:Get*", "iam:List*", "iam:GenerateCredentialReport",
	"cloudtrail:LookupEvents",
	"pricing:GetProducts",
	"ce:GetCostAndUsage", "ce:GetCostForecast",
	"ssm:DescribeInstanceInformation",
}

// ManagePolicy is what the manage role may do: the changes Argus makes to
// instances, disks, snapshots, images, Elastic IPs, security groups and
// load balancer targets, and nothing else. No
// IAM, so a change cannot grant itself more; no iam:PassRole, so an
// instance cannot be launched with a role. New disks use the account's
// default EBS key, whose key policy already lets EBS use it for anyone in
// the account, so no KMS permission is needed either.
var ManagePolicy = []string{
	"ec2:Describe*",
	"ec2:RunInstances",
	"ec2:StartInstances", "ec2:StopInstances", "ec2:RebootInstances",
	"ec2:TerminateInstances",
	"ec2:ModifyInstanceAttribute",
	"ec2:CreateTags", "ec2:DeleteTags",
	"ec2:CreateSnapshot", "ec2:DeleteSnapshot",
	"ec2:CreateVolume", "ec2:ModifyVolume", "ec2:AttachVolume", "ec2:DetachVolume", "ec2:DeleteVolume",
	"ec2:CreateImage", "ec2:DeregisterImage",
	"ec2:AllocateAddress", "ec2:AssociateAddress", "ec2:DisassociateAddress", "ec2:ReleaseAddress",
	"ec2:CreateSecurityGroup", "ec2:DeleteSecurityGroup",
	"ec2:AuthorizeSecurityGroupIngress", "ec2:AuthorizeSecurityGroupEgress",
	"ec2:RevokeSecurityGroupIngress", "ec2:RevokeSecurityGroupEgress",
	"elasticloadbalancing:RegisterTargets", "elasticloadbalancing:DeregisterTargets",
}

// ReadRoleName and ManageRoleName are the roles the template makes.
const (
	ReadRoleName   = "ArgusRead"
	ManageRoleName = "ArgusManage"
)

// Template is the CloudFormation template that makes the read role in one
// account, and with manage the manage role beside it. Everything is baked
// in: the principal they trust and the external id they require, so there
// are no parameters to get wrong. Only AWS resource types, so
// CloudFormation does not charge for it. Deploying the manage template over
// the read one adds the manage role; deploying the read one over the
// manage one removes it.
func Template(principal, externalID string, manage bool) string {
	return TemplateTrusting([]string{principal}, externalID, manage)
}

// TemplateTrusting is Template with roles that trust every one of
// principals: while an account moves from one of argus-control's
// identities to another, its roles trust both, so nothing stops working
// between the stack update and Argus switching over.
func TemplateTrusting(principals []string, externalID string, manage bool) string {
	return TemplateFor("", principals, externalID, manage)
}

// TemplateFor is TemplateTrusting for account accountID. When one of the
// principals is an IAM user or role in that same account, the stack also
// gives it the right to switch into Argus's roles: a role trusting an
// identity is half of it, and the identity's own policy is the other half.
// So the one command or the one button is all it takes, and nobody attaches
// a policy by hand. Roles in other accounts are covered too, since every
// Argus role is named Argus*.
func TemplateFor(accountID string, principals []string, externalID string, manage bool) string {
	principal := principalYAML(principals)
	desc := "Lets Argus read this account's inventory, as the person who asked."
	if manage {
		desc = "Lets Argus read this account's inventory and change its instances, disks, images, addresses, firewalls and load balancer targets, as the person who asked, and open time-limited console sessions for people someone approved."
	}
	var b strings.Builder
	fmt.Fprintf(&b, `AWSTemplateFormatVersion: "2010-09-09"
Description: >-
  %s
  Trusts Argus's identity and requires its external id and a source identity.
Resources:
`, desc)
	b.WriteString(roleYAML("ArgusRead", ReadRoleName, "argus-read", principal, externalID, ReadPolicy))
	if manage {
		b.WriteString(roleYAML("ArgusManage", ManageRoleName, "argus-manage", principal, externalID, ManagePolicy))
		b.WriteString(consoleRoleYAML("ArgusConsoleRead", ConsoleReadRoleName, principal, externalID, consolePolicy[cloud.ConsoleRead]))
		b.WriteString(consoleRoleYAML("ArgusConsoleOperate", ConsoleOperateRoleName, principal, externalID, consolePolicy[cloud.ConsoleOperate]))
	}
	b.WriteString(assumeYAML(accountID, principals))
	b.WriteString(`Outputs:
  ReadRoleArn:
    Description: Argus finds this role by its name; nothing to copy.
    Value: !GetAtt ArgusRead.Arn
`)
	if manage {
		b.WriteString(`  ManageRoleArn:
    Description: Argus finds this role by its name too.
    Value: !GetAtt ArgusManage.Arn
`)
	}
	return b.String()
}

func roleYAML(logical, name, policy, principal, externalID string, actions []string) string {
	var acts strings.Builder
	for _, a := range actions {
		fmt.Fprintf(&acts, "                  - %s\n", a)
	}
	return fmt.Sprintf(`  %s:
    Type: AWS::IAM::Role
    Properties:
      RoleName: %s
      MaxSessionDuration: 3600
      AssumeRolePolicyDocument:
        Version: "2012-10-17"
        Statement:
          - Effect: Allow
            Principal:
              AWS: %s
            Action:
              - sts:AssumeRole
              - sts:SetSourceIdentity
              - sts:TagSession
            Condition:
              StringEquals:
                sts:ExternalId: %s
              "Null":
                sts:SourceIdentity: "false"
      Policies:
        - PolicyName: %s
          PolicyDocument:
            Version: "2012-10-17"
            Statement:
              - Effect: Allow
                Resource: "*"
                Action:
%s`, logical, name, principal, yamlQuote(externalID), policy, acts.String())
}

// DeployCommand is a command that makes the role from AWS CloudShell or any
// shell with the AWS CLI, without that shell having to reach Argus: the
// template travels inside the command.
func DeployCommand(principal, externalID string, manage bool) string {
	return DeployCommandTrusting([]string{principal}, externalID, manage)
}

// DeployCommandTrusting is DeployCommand for TemplateTrusting.
func DeployCommandTrusting(principals []string, externalID string, manage bool) string {
	return DeployCommandFor("", principals, externalID, manage)
}

// DeployCommandFor is DeployCommand for TemplateFor.
func DeployCommandFor(accountID string, principals []string, externalID string, manage bool) string {
	return "cat > argus-access.yaml <<'ARGUS'\n" + TemplateFor(accountID, principals, externalID, manage) + "ARGUS\n" +
		clearStuck +
		"aws cloudformation deploy --stack-name argus-access --template-file argus-access.yaml --capabilities CAPABILITY_NAMED_IAM && \\\n" +
		"aws cloudformation describe-stacks --stack-name argus-access --query \"Stacks[0].Outputs[].[OutputKey,OutputValue]\" --output text\n"
}

// clearStuck deletes an argus-access stack that never finished being made
// (it holds only Argus's roles, if anything), since CloudFormation will not
// deploy over one. A stack that is in use is left alone.
const clearStuck = `case "$(aws cloudformation describe-stacks --stack-name argus-access --query 'Stacks[0].StackStatus' --output text 2>/dev/null)" in
  ROLLBACK_COMPLETE|ROLLBACK_FAILED|CREATE_FAILED|DELETE_FAILED)
    echo "Deleting the argus-access stack that did not finish..."
    aws cloudformation delete-stack --stack-name argus-access &&
    aws cloudformation wait stack-delete-complete --stack-name argus-access ;;
esac
`

// principalYAML is the trust policy's AWS principal: one quoted ARN, or a
// flow list of them.
func principalYAML(principals []string) string {
	if len(principals) == 1 {
		return yamlQuote(principals[0])
	}
	q := make([]string, len(principals))
	for n, p := range principals {
		q[n] = yamlQuote(p)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// yamlQuote double-quotes s for YAML. Both values are ARNs and hex the
// control plane made, but they are quoted anyway.
func yamlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// consoleRoleYAML is a console role: the same trust as the others, an AWS
// managed policy, and sessions of an hour at most. Argus opens a session on
// it only for a person whose grant is in force, and only for what the
// grant has left.
func consoleRoleYAML(logical, name, principal, externalID, policyARN string) string {
	return fmt.Sprintf(`  %s:
    Type: AWS::IAM::Role
    Properties:
      RoleName: %s
      MaxSessionDuration: 3600
      AssumeRolePolicyDocument:
        Version: "2012-10-17"
        Statement:
          - Effect: Allow
            Principal:
              AWS: %s
            Action:
              - sts:AssumeRole
              - sts:SetSourceIdentity
              - sts:TagSession
            Condition:
              StringEquals:
                sts:ExternalId: %s
              "Null":
                sts:SourceIdentity: "false"
      ManagedPolicyArns:
        - %s
`, logical, name, principal, yamlQuote(externalID), policyARN)
}

// AssumeActions are what an identity's own policy must allow for it to use
// Argus's roles.
var AssumeActions = []string{"sts:AssumeRole", "sts:SetSourceIdentity", "sts:TagSession"}

// AssumePolicyName is the inline policy the stack puts on Argus's own
// identity when that identity is in the account.
const AssumePolicyName = "argus-assume-roles"

// LocalPrincipal splits an IAM user or role ARN in accountID into its kind
// ("user" or "role") and name. Anything else, such as an account root or an
// identity in another account, gives "".
func LocalPrincipal(accountID, arn string) (kind, name string) {
	parts := strings.SplitN(arn, ":", 6)
	if accountID == "" || len(parts) != 6 || parts[2] != "iam" || parts[4] != accountID {
		return "", ""
	}
	res := strings.Split(parts[5], "/")
	if len(res) < 2 || (res[0] != "user" && res[0] != "role") || res[len(res)-1] == "" {
		return "", ""
	}
	return res[0], res[len(res)-1]
}

// assumeYAML is the policy that lets the principals in accountID use the
// Argus roles, or nothing when none of them is there.
func assumeYAML(accountID string, principals []string) string {
	var users, roles []string
	for _, p := range principals {
		switch kind, name := LocalPrincipal(accountID, p); kind {
		case "user":
			users = append(users, name)
		case "role":
			roles = append(roles, name)
		}
	}
	if len(users) == 0 && len(roles) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, `  ArgusAssume:
    Type: AWS::IAM::Policy
    Properties:
      PolicyName: %s
`, AssumePolicyName)
	for _, l := range []struct {
		key   string
		names []string
	}{{"Users", users}, {"Roles", roles}} {
		if len(l.names) == 0 {
			continue
		}
		fmt.Fprintf(&b, "      %s:\n", l.key)
		for _, n := range l.names {
			fmt.Fprintf(&b, "        - %s\n", yamlQuote(n))
		}
	}
	b.WriteString(`      PolicyDocument:
        Version: "2012-10-17"
        Statement:
          - Effect: Allow
            Action:
`)
	for _, a := range AssumeActions {
		fmt.Fprintf(&b, "              - %s\n", a)
	}
	b.WriteString(`            Resource: "arn:aws:iam::*:role/Argus*"
`)
	return b.String()
}

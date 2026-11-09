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
	desc := "Lets Argus read this account's inventory, as the person who asked."
	if manage {
		desc = "Lets Argus read this account's inventory and change its instances, disks, images, addresses, firewalls and load balancer targets, as the person who asked, and open time-limited console sessions for people someone approved."
	}
	var b strings.Builder
	fmt.Fprintf(&b, `AWSTemplateFormatVersion: "2010-09-09"
Description: >-
  %s
  Trusts one identity and requires its external id and a source identity.
Resources:
`, desc)
	b.WriteString(roleYAML("ArgusRead", ReadRoleName, "argus-read", principal, externalID, ReadPolicy))
	if manage {
		b.WriteString(roleYAML("ArgusManage", ManageRoleName, "argus-manage", principal, externalID, ManagePolicy))
		b.WriteString(consoleRoleYAML("ArgusConsoleRead", ConsoleReadRoleName, principal, externalID, consolePolicy[cloud.ConsoleRead]))
		b.WriteString(consoleRoleYAML("ArgusConsoleOperate", ConsoleOperateRoleName, principal, externalID, consolePolicy[cloud.ConsoleOperate]))
	}
	b.WriteString(`Outputs:
  ReadRoleArn:
    Description: Paste this into Argus.
    Value: !GetAtt ArgusRead.Arn
`)
	if manage {
		b.WriteString(`  ManageRoleArn:
    Description: Paste this into Argus too.
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
%s`, logical, name, yamlQuote(principal), yamlQuote(externalID), policy, acts.String())
}

// DeployCommand is a command that makes the role from AWS CloudShell or any
// shell with the AWS CLI, without that shell having to reach Argus: the
// template travels inside the command.
func DeployCommand(principal, externalID string, manage bool) string {
	return "cat > argus-access.yaml <<'ARGUS'\n" + Template(principal, externalID, manage) + "ARGUS\n" +
		"aws cloudformation deploy --stack-name argus-access --template-file argus-access.yaml --capabilities CAPABILITY_NAMED_IAM && \\\n" +
		"aws cloudformation describe-stacks --stack-name argus-access --query \"Stacks[0].Outputs[].[OutputKey,OutputValue]\" --output text\n"
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
`, logical, name, yamlQuote(principal), yamlQuote(externalID), policyARN)
}

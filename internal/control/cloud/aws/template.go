package aws

import (
	"fmt"
	"strings"
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

// ReadRoleName is the role the template makes.
const ReadRoleName = "ArgusRead"

// Template is the CloudFormation template that makes the read role in one
// account. Everything is baked in: the principal it trusts and the external
// id it requires, so there are no parameters to get wrong. Only AWS resource
// types, so CloudFormation does not charge for it.
func Template(principal, externalID string) string {
	var acts strings.Builder
	for _, a := range ReadPolicy {
		fmt.Fprintf(&acts, "                  - %s\n", a)
	}
	return fmt.Sprintf(`AWSTemplateFormatVersion: "2010-09-09"
Description: >-
  Lets Argus read this account's inventory, as the person who asked.
  Trusts one identity, requires its external id and a source identity, and
  grants describe and list only.
Resources:
  ArgusRead:
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
        - PolicyName: argus-read
          PolicyDocument:
            Version: "2012-10-17"
            Statement:
              - Effect: Allow
                Resource: "*"
                Action:
%sOutputs:
  ReadRoleArn:
    Description: Paste this into Argus.
    Value: !GetAtt ArgusRead.Arn
`, ReadRoleName, yamlQuote(principal), yamlQuote(externalID), acts.String())
}

// DeployCommand is a command that makes the role from AWS CloudShell or any
// shell with the AWS CLI, without that shell having to reach Argus: the
// template travels inside the command.
func DeployCommand(principal, externalID string) string {
	return "cat > argus-read.yaml <<'ARGUS'\n" + Template(principal, externalID) + "ARGUS\n" +
		"aws cloudformation deploy --stack-name argus-access --template-file argus-read.yaml --capabilities CAPABILITY_NAMED_IAM && \\\n" +
		"aws cloudformation describe-stacks --stack-name argus-access --query \"Stacks[0].Outputs[?OutputKey=='ReadRoleArn'].OutputValue\" --output text\n"
}

// yamlQuote double-quotes s for YAML. Both values are ARNs and hex the
// control plane made, but they are quoted anyway.
func yamlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

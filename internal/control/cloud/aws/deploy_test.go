package aws

import (
	"encoding/json"
	"strings"
	"testing"

	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// The stack that stopped at ROLLBACK_FAILED had an identity that could
// create a role but not read it back. The check refuses anything short of
// every right, on every role.
func TestDeployNeedsEveryRightOnEveryRole(t *testing.T) {
	allow, deny := iamtypes.PolicyEvaluationDecisionTypeAllowed, iamtypes.PolicyEvaluationDecisionTypeImplicitDeny
	if !allowed(iamtypes.EvaluationResult{EvalDecision: allow}) {
		t.Fatal("an allowed action must pass")
	}
	if allowed(iamtypes.EvaluationResult{EvalDecision: allow, ResourceSpecificResults: []iamtypes.ResourceSpecificResult{
		{EvalResourceDecision: allow}, {EvalResourceDecision: deny}}}) {
		t.Fatal("an action denied on one role must fail, whatever the summary says")
	}
	for _, a := range []string{"iam:PutUserPolicy", "iam:GetRole", "iam:DeleteRole", "iam:CreateRole", "cloudformation:DeleteStack"} {
		if !strings.Contains(DeployRights("993686182533", "arn:aws:iam::993686182533:user/argus"), `"`+a+`"`) {
			t.Errorf("the policy to attach lacks %s", a)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(DeployRights("993686182533", "arn:aws:iam::993686182533:user/argus")), &doc); err != nil {
		t.Fatalf("the policy is not JSON: %v", err)
	}
	if len(deployRoles(true)) != 4 || len(deployRoles(false)) != 1 {
		t.Fatal("the check must cover every role the template makes")
	}
}

func TestStuckStacksAreTheOnesToDelete(t *testing.T) {
	for _, s := range []string{"ROLLBACK_FAILED", "ROLLBACK_COMPLETE", "CREATE_FAILED", "DELETE_FAILED"} {
		if !StuckStatus(s) {
			t.Errorf("%s is stuck", s)
		}
	}
	for _, s := range []string{"CREATE_COMPLETE", "UPDATE_COMPLETE", "UPDATE_ROLLBACK_COMPLETE", "CREATE_IN_PROGRESS"} {
		if StuckStatus(s) {
			t.Errorf("%s is in use and must not be deleted", s)
		}
	}
	if !strings.Contains(DeployCommand("p", "e", false), "ROLLBACK_FAILED") {
		t.Fatal("the command must clear a stuck stack before deploying")
	}
}

// A role trusting Argus's identity is half of it: in the account the
// identity is in, the stack also gives it the right to use the roles, so
// nobody attaches a policy by hand. The roles refused it with
// "not authorized to perform: sts:SetSourceIdentity" until this.
func TestTheStackLetsItsOwnIdentityUseTheRoles(t *testing.T) {
	user := "arn:aws:iam::993686182533:user/argus"
	tpl := TemplateFor("993686182533", []string{user}, "argus-x", true)
	for _, want := range []string{"Type: AWS::IAM::Policy", "PolicyName: argus-assume-roles", "Users:", `- "argus"`,
		"- sts:AssumeRole", "- sts:SetSourceIdentity", "- sts:TagSession", `Resource: "arn:aws:iam::*:role/Argus*"`} {
		if !strings.Contains(tpl, want) {
			t.Errorf("the template lacks %q:\n%s", want, tpl)
		}
	}
	if strings.Contains(TemplateFor("111122223333", []string{user}, "argus-x", true), "AWS::IAM::Policy") {
		t.Error("another account's stack must not touch the identity, which is not there")
	}
	role := TemplateFor("993686182533", []string{"arn:aws:iam::993686182533:role/ops/argus-control"}, "argus-x", false)
	if !strings.Contains(role, "Roles:") || !strings.Contains(role, `- "argus-control"`) {
		t.Errorf("a role identity gets the policy by its name, without the path:\n%s", role)
	}
	if strings.Contains(TemplateFor("993686182533", []string{"arn:aws:iam::993686182533:root"}, "argus-x", false), "AWS::IAM::Policy") {
		t.Error("an account root is not a user or a role")
	}
	if !strings.Contains(DeployRights("993686182533", user), `"Resource":"`+user+`"`) {
		t.Error("making the stack itself needs the right to put that policy on the identity")
	}
}

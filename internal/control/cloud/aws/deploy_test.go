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
	for _, a := range []string{"iam:GetRole", "iam:DeleteRole", "iam:CreateRole", "cloudformation:DeleteStack"} {
		if !strings.Contains(DeployRights("993686182533"), `"`+a+`"`) {
			t.Errorf("the policy to attach lacks %s", a)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(DeployRights("993686182533")), &doc); err != nil {
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

package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

/*
Connecting without copying role ARNs back, and with Argus making the stack
itself in the account its own identity is in. Against the fake provider.
*/

func (e cloudEnv) check(t *testing.T, manage bool) CloudCheck {
	t.Helper()
	m := "false"
	if manage {
		m = "true"
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/check", "admin-token",
		`{"provider":"aws","accountId":"`+e.account+`","manage":`+m+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	var c CloudCheck
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	return c
}

// The page waits on check: not ready until the role trusts this Argus with
// its external id, then ready, and connecting needs no ARN at all.
func TestCloudConnectFindsTheRolesByName(t *testing.T) {
	e := cloudAPI(t)
	p := e.prepare(t)
	if p.ReadRoleARN != e.roleARN() {
		t.Fatalf("prepare must say where the read role will be: %q", p.ReadRoleARN)
	}
	if p.CanDeploy {
		t.Fatal("argus-control's identity is in another account; it must not offer to make the roles")
	}
	if c := e.check(t, false); c.Ready || c.Detail == "" {
		t.Fatalf("no role yet must be not ready, with why: %+v", c)
	}
	e.fake.Roles[e.roleARN()] = "argus-someone-elses-id"
	if c := e.check(t, false); c.Ready {
		t.Fatal("a role without this Argus's external id must not count as ready")
	}
	e.fake.Roles[e.roleARN()] = p.ExternalID
	if c := e.check(t, false); !c.Ready {
		t.Fatalf("the role is there: %+v", c)
	}
	if c := e.check(t, true); c.Ready || !strings.Contains(c.Detail, "manage role") {
		t.Fatalf("read and change must wait for the manage role too: %+v", c)
	}
	target := cloudTarget("aws", e.account, "payments-prod")
	if n := auditCount(t, e.s, "cloud.account.refused", target); n != 0 {
		t.Fatal("waiting for the role is not a refused connection")
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"aws","name":"payments-prod","accountId":"`+e.account+`","environment":"production"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect without an ARN: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.RoleARN != e.roleARN() || c.ManageRoleARN != "" {
		t.Fatalf("connected with the wrong roles: %+v", c)
	}
}

// Asking for changes without naming the manage role uses ArgusManage, and
// is refused when that role does not let Argus in.
func TestCloudConnectManageDefaultsToTheTemplatesRole(t *testing.T) {
	e := cloudAPI(t)
	p := e.prepare(t)
	e.fake.Roles[e.roleARN()] = p.ExternalID
	body := `{"provider":"aws","name":"payments-prod","accountId":"` + e.account + `","manage":true,"environment":"production"}`
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token", body); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a missing manage role must be refused: %d %s", w.Code, w.Body)
	}
	e.fake.Roles["arn:aws:iam::"+e.account+":role/ArgusManage"] = p.ExternalID
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.ManageRoleARN != "arn:aws:iam::"+e.account+":role/ArgusManage" {
		t.Fatalf("manage role: %q", c.ManageRoleARN)
	}
}

// Argus makes the stack only in its own identity's account, records who
// asked, and says plainly when the identity may not.
func TestCloudDeployOnlyInTheHomeAccountAndAudited(t *testing.T) {
	e := cloudAPI(t)
	body := `{"provider":"aws","accountId":"` + e.account + `","name":"payments-prod"}`
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/deploy", "admin-token", body); w.Code != http.StatusConflict {
		t.Fatalf("deploy in another account than argus-control's: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/deploy", "auditor-token", body); w.Code != http.StatusForbidden {
		t.Fatalf("an auditor made a stack: %d", w.Code)
	}
	if e.fake.Stack.Template != "" {
		t.Fatal("nothing may have been deployed yet")
	}

	// Now argus-control's identity is in this account.
	e.fake.BaseIdentity.Principal = "arn:aws:iam::" + e.account + ":user/argus"
	e.a.cloud.mu.Lock()
	e.a.cloud.bases = nil
	e.a.cloud.mu.Unlock()
	p := e.prepare(t)
	if !p.CanDeploy {
		t.Fatal("in its own account Argus should offer to make the roles")
	}
	e.fake.Stack.DeployErr = errors.New("argus-control's own identity may not make the roles (AccessDenied)")
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/deploy", "admin-token", body); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), "may not make the roles") {
		t.Fatalf("a refused deploy must say why: %d %s", w.Code, w.Body)
	}
	target := cloudTarget("aws", e.account, "payments-prod")
	if n := auditCount(t, e.s, "cloud.account.stack_refused", target); n != 1 {
		t.Fatalf("refused deploys audited %d times, want 1", n)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/deploy", "admin-token", body); w.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(e.fake.Stack.Template, p.ExternalID) || e.fake.Stack.Actor == "" {
		t.Fatalf("the stack must be this Argus's template, made for the person: %+v", e.fake.Stack)
	}
	if n := auditCount(t, e.s, "cloud.account.stack_made", target); n != 1 {
		t.Fatalf("deploys audited %d times, want 1", n)
	}
	if c := e.check(t, false); c.Ready || c.Stack == nil || c.Stack.Status != "CREATE_IN_PROGRESS" {
		t.Fatalf("check must follow the stack while it is made: %+v", c)
	}
	e.fake.Stack.State.Status, e.fake.Stack.State.Done = "CREATE_COMPLETE", true
	e.fake.Roles[e.roleARN()] = p.ExternalID
	if c := e.check(t, false); !c.Ready || c.Stack == nil || !c.Stack.Done {
		t.Fatalf("done: %+v", c)
	}
}

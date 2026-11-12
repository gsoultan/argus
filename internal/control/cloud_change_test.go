package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 2: changes. Against the fake provider, which edits its own
inventory and records who every call ran for.
*/

func TestCloudEnvironmentFromTags(t *testing.T) {
	cases := map[string]string{"prod": "production", "Staging": "staging", "dev": "development", "uat": "staging", "something-else": "staging"}
	for v, want := range cases {
		got := cloudEnvOf(map[string]string{"Environment": v}, "staging")
		if got != want {
			t.Errorf("Environment=%s: got %s, want %s", v, got, want)
		}
	}
	if cloudEnvOf(nil, "production") != "production" {
		t.Error("no tag must fall back to the account's environment")
	}
	if !cloudNeedsApproval("production", cloud.LevelChange) || cloudNeedsApproval("production", cloud.LevelOperate) ||
		!cloudNeedsApproval("staging", cloud.LevelRemove) || cloudNeedsApproval("staging", cloud.LevelChange) ||
		cloudNeedsApproval("development", cloud.LevelRemove) {
		t.Error("approval rules by environment are wrong")
	}
}

type changeEnv struct {
	cloudEnv
	acct    CloudAccount
	second  string // another admin's email; token "second-token"
	approve string // an approver's email; token "approver-token"
}

func (e cloudEnv) manageARN() string { return "arn:aws:iam::" + e.account + ":role/ArgusManage" }

// changeAPI connects an account that may be changed, with one instance,
// a subnet and a security group read into it.
func changeAPI(t *testing.T) changeEnv {
	t.Helper()
	e := cloudAPI(t)
	p := e.prepare(t)
	e.fake.Roles[e.roleARN()] = p.ExternalID
	e.fake.Roles[e.manageARN()] = p.ExternalID
	region := "ap-southeast-3"
	e.fake.Resources[region] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: e.account + "-i-1", Name: "pay-api-01", Region: region, State: "running",
			Size: "m6i.xlarge", Tags: map[string]string{"Name": "pay-api-01"},
			Links: []cloud.Ref{{Kind: cloud.KindVolume, ID: e.account + "-vol-1", Name: "/dev/xvda"}}},
		{Kind: cloud.KindSubnet, ID: e.account + "-subnet-1", Name: "private-a", Region: region, Zone: region + "a",
			Network: "vpc-1", Size: "10.0.1.0/24"},
		{Kind: cloud.KindFirewall, ID: e.account + "-sg-1", Name: "sg-web", Region: region, Network: "vpc-1"},
	}
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM cloud_changes WHERE account_id IN
			(SELECT id FROM cloud_accounts WHERE account_id = $1)`, e.account)
	})
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"aws","name":"payments-prod","accountId":"`+e.account+`","roleArn":"`+e.roleARN()+
			`","manageRoleArn":"`+e.manageARN()+`","environment":"production"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.Access != "manage" || c.ManageRoleARN != e.manageARN() {
		t.Fatalf("connected without changes: %+v", c)
	}
	waitCloudSync(t, e, c)
	second, _ := newAccount(t, e.s, "admin")
	approver, _ := newAccount(t, e.s, "approver")
	e.a.UserTokens["second-token"] = second
	e.a.UserTokens["approver-token"] = approver
	return changeEnv{cloudEnv: e, acct: c, second: second, approve: approver}
}

func (e changeEnv) ask(t *testing.T, token, body string, want int) CloudChange {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", token, body)
	if w.Code != want {
		t.Fatalf("change: %d %s, want %d", w.Code, w.Body, want)
	}
	var c CloudChange
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	return c
}

func (e changeEnv) instance() string { return e.account + "-i-1" }

func (e changeEnv) body(action, extra string) string {
	return `{"accountId":"` + e.acct.ID + `","region":"ap-southeast-3","action":"` + action + `","resourceId":"` + e.instance() + `"` + extra + `}`
}

func TestCloudChangesNeedAManageRole(t *testing.T) {
	e := cloudAPI(t)
	p := e.prepare(t)
	e.fake.Roles[e.roleARN()] = p.ExternalID
	c := e.connect(t, http.StatusCreated)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		`{"accountId":"`+c.ID+`","region":"ap-southeast-3","action":"stop","resourceId":"x"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "read only") {
		t.Fatalf("a read-only account must refuse changes plainly: %d %s", w.Code, w.Body)
	}
}

// Stopping is an operate change: it runs at once, as the person, and is
// two rows in the audit chain.
func TestCloudStopRunsAtOnceAsThePerson(t *testing.T) {
	e := changeAPI(t)
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "auditor-token", e.body("stop", "")); w.Code != http.StatusForbidden {
		t.Fatalf("an auditor changed something: %d", w.Code)
	}
	c := e.ask(t, "admin-token", e.body("stop", ""), http.StatusOK)
	if c.State != "applied" || c.RequestID == "" {
		t.Fatalf("stop did not apply: %+v", c)
	}
	found := false
	for _, call := range e.fake.CallLog() {
		if call == "stop "+e.instance()+" "+e.admin {
			found = true
		}
	}
	if !found {
		t.Fatalf("stop must run as the person who asked: %v", e.fake.CallLog())
	}
	target := m2target(c)
	if auditCount(t, e.s, "cloud.change.requested", target) != 1 || auditCount(t, e.s, "cloud.change.applied", target) != 1 {
		t.Fatal("a change must be audited as requested and applied")
	}
}

// Removing on Production: the name must be typed, it waits for someone
// else, and it runs as the person who asked once approved.
func TestCloudRemoveOnProductionNeedsSomeoneElse(t *testing.T) {
	e := changeAPI(t)
	e.ask(t, "admin-token", e.body("remove", `,"confirm":"pay-api"`), http.StatusBadRequest)
	c := e.ask(t, "admin-token", e.body("remove", `,"confirm":"pay-api-01","spec":{"finalSnapshots":true}`), http.StatusAccepted)
	if c.State != "awaiting_approval" || c.PlanHash == "" || !strings.Contains(c.Summary, "1 disk") {
		t.Fatalf("remove must wait with its plan: %+v", c)
	}
	e.ask(t, "admin-token", e.body("stop", ""), http.StatusConflict) // one change at a time
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "admin-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("the person who asked approved their own remove: %d", w.Code)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "approver-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	var done CloudChange
	_ = json.Unmarshal(w.Body.Bytes(), &done)
	if done.State != "applied" || done.DecidedBy == nil || *done.DecidedBy != e.approve {
		t.Fatalf("approved remove did not apply: %+v", done)
	}
	if !strings.Contains(string(done.Result), "snap-") {
		t.Fatalf("final snapshots not recorded: %s", done.Result)
	}
	want := "remove " + e.instance() + " snapshots=true " + e.admin
	if calls := e.fake.CallLog(); calls[len(calls)-1] != want && !strings.Contains(strings.Join(calls, "\n"), want) {
		t.Fatalf("remove must run as the person who asked: %v", calls)
	}
	target := m2target(done)
	for _, act := range []string{"requested", "approved", "applied"} {
		if auditCount(t, e.s, "cloud.change."+act, target) != 1 {
			t.Errorf("cloud.change.%s not audited once", act)
		}
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "second-token", ""); w.Code != http.StatusConflict {
		t.Fatalf("a settled change was decided again: %d", w.Code)
	}
}

func TestCloudRemoveRespectsTerminationProtection(t *testing.T) {
	e := changeAPI(t)
	e.fake.Protected = map[string]bool{e.instance(): true}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", e.body("remove", `,"confirm":"pay-api-01"`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "termination protection") {
		t.Fatalf("a protected instance must be refused, not unprotected: %d %s", w.Code, w.Body)
	}
	for _, call := range e.fake.CallLog() {
		if strings.HasPrefix(call, "protect ") || strings.HasPrefix(call, "remove ") {
			t.Fatalf("nothing may change: %s", call)
		}
	}
}

// An approval runs the plan that was approved: if the instance changed in
// between, the change goes back for a decision on the new plan.
func TestCloudRemoveReplansWhenTheInstanceChanged(t *testing.T) {
	e := changeAPI(t)
	c := e.ask(t, "admin-token", e.body("remove", `,"confirm":"pay-api-01"`), http.StatusAccepted)
	r := &e.fake.Resources["ap-southeast-3"][0]
	r.Links = append(r.Links, cloud.Ref{Kind: cloud.KindVolume, ID: e.account + "-vol-2", Name: "/dev/xvdb"})
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "approver-token", "")
	var got CloudChange
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != "awaiting_approval" || got.PlanHash == c.PlanHash || !strings.Contains(got.Summary, "2 disks") {
		t.Fatalf("a changed plan must go back for a decision: %d %+v", w.Code, got)
	}
	for _, call := range e.fake.CallLog() {
		if strings.HasPrefix(call, "remove ") {
			t.Fatal("nothing may be removed on a plan nobody saw")
		}
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/decline", "approver-token", ""); w.Code != http.StatusOK {
		t.Fatalf("decline: %d %s", w.Code, w.Body)
	}
}

// Launching with the agent: the first-boot script names the server and
// enrolls it with a token for that name only, and the token is not stored
// with the change.
func TestCloudLaunchEnrollsTheAgent(t *testing.T) {
	e := changeAPI(t)
	spec := `{"name":"web-02","image":"ami-ubuntu","size":"t3.small","diskGb":20,"subnet":"` + e.account +
		`-subnet-1","firewalls":["` + e.account + `-sg-1"],"agent":true,"tags":{"Environment":"development"}}`
	body := `{"accountId":"` + e.acct.ID + `","region":"ap-southeast-3","action":"launch","spec":` + spec + `}`
	c := e.ask(t, "admin-token", body, http.StatusOK)
	if c.State != "applied" || !strings.HasPrefix(c.ResourceID, "i-fake") || c.Environment != "development" {
		t.Fatalf("launch: %+v", c)
	}
	ud := e.fake.LastUserData
	if !strings.Contains(ud, "hostnamectl set-hostname 'web-02'") || !strings.Contains(ud, "/api/v1/install/agent.sh?") ||
		!strings.Contains(ud, "token=") {
		t.Fatalf("first-boot script wrong:\n%s", ud)
	}
	if strings.Contains(string(c.Spec), "argus_enroll") || strings.Contains(string(c.Spec), "token") {
		t.Fatalf("the enrollment token must not be stored with the change: %s", c.Spec)
	}
	var bound string
	_ = e.s.pool.QueryRow(context.Background(), `SELECT hostname FROM agent_enrollment_tokens
		WHERE created_by = $1 ORDER BY created_at DESC LIMIT 1`, e.admin).Scan(&bound)
	if bound != "web-02" {
		t.Fatalf("the token must be bound to the new server's name, got %q", bound)
	}
	// On Production a launch waits.
	spec2 := strings.Replace(strings.Replace(spec, "development", "production", 1), "web-02", "web-03", 1)
	w := e.ask(t, "admin-token", `{"accountId":"`+e.acct.ID+`","region":"ap-southeast-3","action":"launch","spec":`+spec2+`}`, http.StatusAccepted)
	if w.State != "awaiting_approval" {
		t.Fatalf("a production launch must wait: %+v", w)
	}
	bad := strings.Replace(spec, "web-02", "Web 02!", 1)
	e.ask(t, "admin-token", `{"accountId":"`+e.acct.ID+`","region":"ap-southeast-3","action":"launch","spec":`+bad+`}`, http.StatusBadRequest)
}

func TestCloudResizeNeedsAStoppedInstance(t *testing.T) {
	e := changeAPI(t)
	e.ask(t, "admin-token", e.body("resize", `,"spec":{"size":"m6i.2xlarge"}`), http.StatusConflict)
}

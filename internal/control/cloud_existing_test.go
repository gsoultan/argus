package control

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Instances that were already in the account: Argus reads and operates them
like any other, but never terminates one it did not launch, and
disconnecting the account changes nothing in it.
*/

// launchedByArgus records that Argus launched the fixture's instance, as an
// applied launch would.
func (e changeEnv) launchedByArgus(t *testing.T) { e.madeByArgus(t, "launch", e.instance()) }

// madeByArgus records an applied change of action that made resource id.
func (e changeEnv) madeByArgus(t *testing.T, action, id string) {
	t.Helper()
	_, err := e.s.pool.Exec(context.Background(), `INSERT INTO cloud_changes (account_id, region, action, level,
		environment, resource_id, resource_name, spec, plan, plan_hash, summary, reason, state, requested_by)
		VALUES ($1::uuid, 'ap-southeast-3', $2, 'change', 'production', $3, $3, '{}',
		'{}', '', 'Made ' || $3, '', 'applied', $4)`, e.acct.ID, action, id, e.admin)
	if err != nil {
		t.Fatal(err)
	}
}

func (e changeEnv) noCall(t *testing.T, prefixes ...string) {
	t.Helper()
	for _, call := range e.fake.CallLog() {
		for _, p := range prefixes {
			if strings.HasPrefix(call, p) {
				t.Fatalf("nothing may change in the account: %s", call)
			}
		}
	}
}

// An instance that was in the account before Argus is in its inventory and
// can be operated, but a remove is refused before anything is asked of AWS.
func TestCloudExistingInstanceIsManagedButNeverRemoved(t *testing.T) {
	e := changeAPI(t)
	found := false
	for _, it := range e.items(t, "instance") {
		if it.Resource.ID == e.instance() {
			found = true
		}
	}
	if !found {
		t.Fatal("an instance Argus did not launch must still be in its inventory")
	}
	if c := e.ask(t, "admin-token", e.body("stop", ""), http.StatusOK); c.State != "applied" {
		t.Fatalf("an existing instance must still be operable: %+v", c)
	}

	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", e.body("remove", `,"confirm":"pay-api-01"`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Argus did not launch it") {
		t.Fatalf("removing an instance Argus did not launch must be refused: %d %s", w.Code, w.Body)
	}
	pw := do(t, e.h, http.MethodGet, "/api/v1/cloud/remove-plan?accountId="+e.acct.ID+"&region=ap-southeast-3&resourceId="+e.instance(), "admin-token", "")
	if pw.Code != http.StatusOK || !strings.Contains(pw.Body.String(), "Argus did not launch it") {
		t.Fatalf("the remove dialog must say why: %s", pw.Body)
	}
	e.noCall(t, "remove ")
}

// A tag that claims Argus launched it is not Argus's record.
func TestCloudLaunchedTagIsNotEnoughToRemove(t *testing.T) {
	e := changeAPI(t)
	e.fake.Resources["ap-southeast-3"][0].Tags["argus:launched-by"] = e.admin
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", e.body("remove", `,"confirm":"pay-api-01"`))
	if w.Code != http.StatusConflict {
		t.Fatalf("a tag anyone can write let a remove through: %d %s", w.Code, w.Body)
	}
	e.noCall(t, "remove ")
}

// Even one Argus launched is refused while an Auto Scaling group owns it.
func TestCloudRemoveRefusesAutoScalingInstances(t *testing.T) {
	e := changeAPI(t)
	e.launchedByArgus(t)
	e.fake.Resources["ap-southeast-3"][0].Tags["aws:autoscaling:groupName"] = "web-asg"
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", e.body("remove", `,"confirm":"pay-api-01"`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Auto Scaling group web-asg") {
		t.Fatalf("an instance an Auto Scaling group owns must be refused: %d %s", w.Code, w.Body)
	}
	e.noCall(t, "remove ")
}

func TestCloudManagedByNamesTheOwner(t *testing.T) {
	cases := map[string]string{
		"aws:cloudformation:stack-name":     "CloudFormation stack x",
		"aws:autoscaling:groupName":         "Auto Scaling group x",
		"eks:nodegroup-name":                "EKS node group x",
		"elasticbeanstalk:environment-name": "Elastic Beanstalk environment x",
	}
	for tag, want := range cases {
		if got := cloud.ManagedBy(map[string]string{tag: "x"}); got != want {
			t.Errorf("%s: got %q, want %q", tag, got, want)
		}
	}
	if cloud.ManagedBy(map[string]string{"Name": "web"}) != "" {
		t.Error("an instance nothing owns must have no owner")
	}
}

// Disconnecting is Argus forgetting the account: no call reaches AWS, and a
// change still waiting for a decision is cancelled rather than left to be
// approved.
func TestCloudDisconnectLeavesTheAccountAlone(t *testing.T) {
	e := changeAPI(t)
	e.launchedByArgus(t)
	c := e.ask(t, "admin-token", e.body("remove", `,"confirm":"pay-api-01"`), http.StatusAccepted)
	before := len(e.fake.CallLog())
	if w := do(t, e.h, http.MethodDelete, "/api/v1/cloud/accounts/"+e.acct.ID, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	if calls := e.fake.CallLog()[before:]; len(calls) != 0 {
		t.Fatalf("disconnecting must not call AWS: %v", calls)
	}
	got, err := e.s.CloudChange(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "cancelled" {
		t.Fatalf("a waiting change must be cancelled on disconnect: %s", got.State)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "approver-token", ""); w.Code == http.StatusOK {
		t.Fatalf("a change on a disconnected account was approved: %s", w.Body)
	}
	e.noCall(t, "remove ")
}

// A security group that was in the account is never deleted from Argus,
// and the inventory says which groups and instances Argus made.
func TestCloudExistingFirewallIsNeverDeleted(t *testing.T) {
	e := networkAPI(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", e.storage("firewall-delete", "sg-0000000000000bbb2", `,"confirm":"db"`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Argus did not make it") {
		t.Fatalf("deleting a group Argus did not make must be refused: %d %s", w.Code, w.Body)
	}
	e.noCall(t, "firewall-delete ")
	sg := e.account + "-sg-1"
	made := map[string]bool{}
	for _, it := range e.items(t, "firewall") {
		made[it.Resource.ID] = it.MadeByArgus
	}
	if made[sg] {
		t.Fatalf("a group Argus did not make is marked as made by Argus: %v", made)
	}
	e.madeByArgus(t, "firewall-create", sg)
	for _, it := range e.items(t, "firewall") {
		made[it.Resource.ID] = it.MadeByArgus
	}
	if !made[sg] {
		t.Fatalf("the inventory must say which groups Argus made: %v", made)
	}
	for _, it := range e.items(t, "instance") {
		if it.MadeByArgus {
			t.Fatalf("an instance Argus did not launch is marked as made by Argus: %s", it.Resource.ID)
		}
	}
}

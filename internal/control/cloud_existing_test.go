package control

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Resources that were already in the account: the first read after
connecting takes in every one of them, Argus manages and removes them like
the ones it made (by typed name, with approval where the environment needs
it), and disconnecting the account changes nothing in it.
*/

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

// The first read after connecting takes in everything already in the
// account: every kind, in every region, made by Argus or not.
func TestCloudFirstSyncInventoriesEverythingAlreadyThere(t *testing.T) {
	e := cloudAPI(t)
	p := e.prepare(t)
	e.fake.Roles[e.roleARN()] = p.ExternalID
	want := map[string]bool{}
	for i, region := range []string{"ap-southeast-3", "ap-southeast-1"} {
		var rs []cloud.Resource
		for _, k := range cloud.Kinds {
			id := e.account + "-" + string(k) + "-" + strconv.Itoa(i)
			rs = append(rs, cloud.Resource{Kind: k, ID: id, Name: "existing-" + id, Region: region})
			want[id] = true
		}
		e.fake.Resources[region] = rs
	}
	c := e.connect(t, http.StatusCreated)
	waitCloudSync(t, e, c)
	for _, k := range cloud.Kinds {
		for _, it := range e.items(t, string(k)) {
			if it.MadeByArgus {
				t.Errorf("%s was already in the account but is marked as made by Argus", it.Resource.ID)
			}
			delete(want, it.Resource.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("the first sync left out resources already in the account: %v", want)
	}
}

// An instance that was in the account before Argus is in its inventory, is
// operated, and is removed like any other: by its typed name, waiting for
// someone else on Production, and only then terminated.
func TestCloudExistingInstanceIsManagedAndRemovable(t *testing.T) {
	e := changeAPI(t)
	found := false
	for _, it := range e.items(t, "instance") {
		if it.Resource.ID == e.instance() {
			found = !it.MadeByArgus
		}
	}
	if !found {
		t.Fatal("an instance Argus did not launch must be in its inventory, marked as already there")
	}
	if c := e.ask(t, "admin-token", e.body("stop", ""), http.StatusOK); c.State != "applied" {
		t.Fatalf("an existing instance must be operable: %+v", c)
	}
	pw := do(t, e.h, http.MethodGet, "/api/v1/cloud/remove-plan?accountId="+e.acct.ID+"&region=ap-southeast-3&resourceId="+e.instance(), "admin-token", "")
	if pw.Code != http.StatusOK || strings.Contains(pw.Body.String(), `"refusal":"`) {
		t.Fatalf("an existing instance must be removable: %d %s", pw.Code, pw.Body)
	}
	e.ask(t, "admin-token", e.body("remove", `,"confirm":"pay-api"`), http.StatusBadRequest)
	e.noCall(t, "remove ")
	c := e.ask(t, "admin-token", e.body("remove", `,"confirm":"pay-api-01"`), http.StatusAccepted)
	if c.State != "awaiting_approval" {
		t.Fatalf("removing on Production must wait for someone else: %+v", c)
	}
	e.noCall(t, "remove ")
	if done := e.approveChange(t, c); done.State != "applied" {
		t.Fatalf("remove: %+v", done)
	}
	e.called(t, "remove "+e.instance()+" snapshots=false "+e.admin)
}

// Termination protection still stops a remove, whoever made the instance.
func TestCloudExistingInstanceKeepsTerminationProtection(t *testing.T) {
	e := changeAPI(t)
	e.fake.Protected = map[string]bool{e.instance(): true}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", e.body("remove", `,"confirm":"pay-api-01"`))
	if w.Code != http.StatusConflict {
		t.Fatalf("a protected instance must be refused: %d %s", w.Code, w.Body)
	}
	e.noCall(t, "remove ")
}

// An instance is refused while an Auto Scaling group owns it.
func TestCloudRemoveRefusesAutoScalingInstances(t *testing.T) {
	e := changeAPI(t)
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

// A security group that was in the account is deleted like one Argus made,
// and the inventory says which groups and instances Argus made.
func TestCloudExistingFirewallIsDeletable(t *testing.T) {
	e := networkAPI(t)
	d := e.ask(t, "admin-token", e.storage("firewall-delete", "sg-0000000000000bbb2", `,"confirm":"db"`), http.StatusAccepted)
	if done := e.approveChange(t, d); done.State != "applied" {
		t.Fatalf("an existing group must be deletable: %+v", done)
	}
	e.called(t, "firewall-delete sg-0000000000000bbb2 "+e.admin)
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

package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/control/cloud/gcp"
)

/*
Connecting a Google Cloud project the way an AWS account is connected:
prepare says where argus-read will be and gives one command, check waits
for it, and connecting needs nothing copied back. Changes go through a
second account, argus-manage, made by the same command when asked for.
*/

// gcpFake is the fake as Google Cloud: it makes only the changes Argus
// has built for Google so far.
type gcpFake struct{ *cloud.Fake }

func (gcpFake) Supports(a cloud.Action) bool {
	return slices.Contains(gcp.SupportedActions(), a)
}

// SetTagsOf is Google's way of labelling: by kind, since ids are unique
// only within one.
func (g gcpFake) SetTagsOf(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, kind cloud.Kind, id string, set map[string]string, remove []string) (cloud.Result, error) {
	g.Fake.Labelled = append(g.Fake.Labelled, string(kind)+" "+id)
	// A Google resource's name is its own, not a label's.
	names := map[string]string{}
	for _, r := range g.Fake.Resources[region] {
		names[r.ID] = r.Name
	}
	res, err := g.Fake.SetTags(ctx, a, actor, region, id, set, remove)
	for i, r := range g.Fake.Resources[region] {
		g.Fake.Resources[region][i].Name = names[r.ID]
	}
	return res, err
}

// Sizes are the machine types of the instance's zone.
func (gcpFake) Sizes(context.Context, cloud.Account, cloud.Actor, string, string) ([]string, error) {
	return []string{"e2-medium", "n2-standard-4"}, nil
}

func gcpEnv(t *testing.T) (cloudEnv, *cloud.Fake, string) {
	t.Helper()
	e := cloudAPI(t)
	fake := &cloud.Fake{Name: "gcp",
		BaseIdentity: cloud.Base{Principal: "argus-control@ops-home.iam.gserviceaccount.com",
			ARN: "argus-control@ops-home.iam.gserviceaccount.com", Source: "the service account of the VM argus-control runs on"},
		Roles: map[string]string{}, Regions: []string{"europe-west1", "us-central1", "global"}, Resources: map[string][]cloud.Resource{}}
	e.a.SetCloudProvider(gcpFake{fake})
	project := fmt.Sprintf("argus-test-%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_resources WHERE account_id IN (SELECT id FROM cloud_accounts WHERE account_id = $1)`, project)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_accounts WHERE account_id = $1`, project)
	})
	return e, fake, project
}

func TestCloudConnectGoogleProject(t *testing.T) {
	e, fake, project := gcpEnv(t)
	sa := "argus-read@" + project + ".iam.gserviceaccount.com"

	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", w.Code, w.Body)
	}
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.ReadRoleARN != sa || p.CanDeploy || p.ManageRoleARN != "" {
		t.Fatalf("prepare must point at argus-read and nothing else: %+v", p)
	}
	for _, want := range []string{"argus-external-id=" + p.ExternalID, fake.BaseIdentity.Principal, "roles/compute.viewer", "--project=$P"} {
		if !strings.Contains(p.Command, want) {
			t.Fatalf("the command lacks %q:\n%s", want, p.Command)
		}
	}
	// The AWS account's external id is not this project's.
	if aws := e.prepare(t); aws.ExternalID == p.ExternalID {
		t.Fatal("two accounts must not share an external id")
	}

	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token",
		`{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var pm CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &pm)
	if w.Code != http.StatusOK || pm.ManageRoleARN != "argus-manage@"+project+".iam.gserviceaccount.com" ||
		!strings.Contains(pm.Command, "roles/compute.instanceAdmin.v1") || pm.ExternalID != p.ExternalID {
		t.Fatalf("allowing changes must make argus-manage with the same external id: %d %+v", w.Code, pm)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token",
		`{"provider":"gcp","accountId":"412788931204"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an AWS account id is not a project id: %d %s", w.Code, w.Body)
	}

	check := func() CloudCheck {
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/check", "admin-token", `{"provider":"gcp","accountId":"`+project+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("check: %d %s", w.Code, w.Body)
		}
		var c CloudCheck
		_ = json.Unmarshal(w.Body.Bytes(), &c)
		return c
	}
	if c := check(); c.Ready || !strings.Contains(c.Detail, sa) {
		t.Fatalf("no argus-read yet must be not ready, naming it: %+v", c)
	}
	fake.Roles[sa] = "argus-someone-elses-id"
	if c := check(); c.Ready {
		t.Fatal("argus-read made for another organization must not count")
	}
	fake.Roles[sa] = p.ExternalID
	if c := check(); !c.Ready {
		t.Fatalf("argus-read is there: %+v", c)
	}

	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"gcp","name":"payments-gcp","accountId":"`+project+`","environment":"staging"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.Provider != "gcp" || c.RoleARN != sa || c.ManageRoleARN != "" || c.Identity != DefaultCloudIdentity {
		t.Fatalf("connected wrong: %+v", c)
	}
	if n := auditCount(t, e.s, "cloud.account.connected", cloudTarget("gcp", project, "payments-gcp")); n != 1 {
		t.Fatalf("connecting must be audited once, got %d", n)
	}
	// Allowing changes needs argus-manage, made for this Argus.
	manageSA := "argus-manage@" + project + ".iam.gserviceaccount.com"
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token", `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no argus-manage yet must be refused: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token",
		`{"roleArn":"argus-manage@another-project-1.iam.gserviceaccount.com"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a manage account in another project must be refused: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token", `{"roleArn":"`+sa+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("argus-read cannot be the manage account: %d %s", w.Code, w.Body)
	}
	fake.Roles[manageSA] = p.ExternalID
	w = do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("allow changes: %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.ManageRoleARN != manageSA {
		t.Fatalf("manage account = %q", c.ManageRoleARN)
	}
	if n := auditCount(t, e.s, "cloud.account.manage_allowed", cloudTarget("gcp", project, "payments-gcp")); n != 1 {
		t.Fatalf("allowing changes must be audited once, got %d", n)
	}

	w = do(t, e.h, http.MethodGet, "/api/v1/cloud/accounts", "admin-token", "")
	var view cloudAccountsView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	providers := map[string]int{}
	for _, b := range view.Bases {
		providers[b.Provider]++
	}
	if providers["aws"] != 1 || providers["gcp"] != 1 {
		t.Fatalf("each cloud's identity must be listed once: %+v", view.Bases)
	}
	for _, a := range view.Accounts {
		if a.ID == c.ID && a.IdentityMissing {
			t.Fatal("a project on Google's Main identity must not read as having lost it to AWS's")
		}
	}
}

// A project with changes allowed grows a disk, never shrinks it or changes
// its type, and refuses what Argus has not built for Google yet.
func TestCloudGoogleProjectGrowsADisk(t *testing.T) {
	e, fake, project := gcpEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	fake.Resources["us-central1"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: "101", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "running", Size: "e2-small"},
		{Kind: cloud.KindVolume, ID: "201", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "in-use", Size: "10 GiB",
			Details: map[string]string{"type": "pd-balanced"}, Links: []cloud.Ref{{Kind: cloud.KindInstance, ID: "101", Name: "boot"}}},
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"gcp","name":"web-gcp","accountId":"`+project+`","environment":"development","manage":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect with changes: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.ManageRoleARN != p.ManageRoleARN {
		t.Fatalf("manage account = %q", c.ManageRoleARN)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !e.a.SyncCloudAccount(context.Background(), c) {
		if time.Now().After(deadline) {
			t.Fatal("the project was being read for too long")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ask := func(action, id, spec string, want int) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
			`{"accountId":"`+c.ID+`","region":"us-central1","action":"`+action+`","resourceId":"`+id+`"`+spec+`}`)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", action, w.Code, w.Body, want)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}
	ask("volume-grow", "201", `,"spec":{"sizeGb":5}`, http.StatusBadRequest)
	ask("volume-grow", "201", `,"spec":{"sizeGb":10}`, http.StatusBadRequest)
	ask("volume-grow", "201", `,"spec":{"sizeGb":20,"type":"pd-ssd"}`, http.StatusBadRequest)
	ask("volume-grow", "201", `,"spec":{"sizeGb":70000}`, http.StatusBadRequest)
	ch := ask("volume-grow", "201", `,"spec":{"sizeGb":50}`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != "Grow web-1 from 10 to 50 GB" || !strings.Contains(string(ch.Result), "Google grows the disk") {
		t.Fatalf("grow: %+v %s", ch, ch.Result)
	}
	if ch := ask("reboot", "101", "", http.StatusOK); !strings.Contains(ch.Summary, "Reset web-1") {
		t.Fatalf("a Google reboot is a reset and must say so: %q", ch.Summary)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		`{"accountId":"`+c.ID+`","region":"us-central1","action":"hibernate","resourceId":"101"}`); w.Code != http.StatusNotImplemented {
		t.Fatalf("what is not built for Google must be refused as such: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		`{"accountId":"`+c.ID+`","region":"us-central1","action":"volume-delete","resourceId":"201","confirm":"web-1"}`); w.Code != http.StatusNotImplemented {
		t.Fatalf("deleting a disk is not built for Google yet: %d %s", w.Code, w.Body)
	}
}

func TestCloudGoogleProjectChangesMachineTypeLabelsAndProtection(t *testing.T) {
	e, fake, project := gcpEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	fake.Resources["us-central1"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: "101", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "running", Size: "e2-small"},
		{Kind: cloud.KindVolume, ID: "201", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "in-use", Size: "10 GiB"},
		{Kind: cloud.KindSnapshot, ID: "301", Name: "web-1-snap", Region: "us-central1", State: "completed"},
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"gcp","name":"web-gcp","accountId":"`+project+`","environment":"development","manage":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect with changes: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	deadline := time.Now().Add(5 * time.Second)
	for !e.a.SyncCloudAccount(context.Background(), c) {
		if time.Now().After(deadline) {
			t.Fatal("the project was being read for too long")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ask := func(body string, want int) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
			`{"accountId":"`+c.ID+`","region":"us-central1",`+body+`}`)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", body, w.Code, w.Body, want)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}

	// The console lists the sizes of the instance's own zone.
	w = do(t, e.h, http.MethodGet, "/api/v1/cloud/instance-sizes?accountId="+c.ID+"&region=us-central1&resourceId=101", "admin-token", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "n2-standard-4") || strings.Contains(w.Body.String(), "t3.medium") {
		t.Fatalf("sizes: %d %s", w.Code, w.Body)
	}

	// A machine type changes on a stopped VM only, and only to one.
	ask(`"action":"resize","resourceId":"101","spec":{"size":"t3.medium"}`, http.StatusBadRequest)
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		`{"accountId":"`+c.ID+`","region":"us-central1","action":"resize","resourceId":"101","spec":{"size":"n2-standard-4"}}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stopped VM") {
		t.Fatalf("a running VM: %d %s", w.Code, w.Body)
	}
	fake.Resources["us-central1"][0].State = "stopped"
	if !e.a.SyncCloudAccount(context.Background(), c) {
		t.Fatal("sync")
	}
	ch := ask(`"action":"resize","resourceId":"101","spec":{"size":"n2-standard-4"}`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != "Change web-1 from e2-small to n2-standard-4" {
		t.Fatalf("resize: %+v", ch)
	}

	// Labels: lowercase, on instances and disks, by kind.
	ask(`"action":"tags","resourceId":"101","spec":{"set":{"Team":"payments"}}`, http.StatusBadRequest)
	ask(`"action":"tags","resourceId":"101","spec":{"set":{"goog-x":"y"}}`, http.StatusBadRequest)
	ask(`"action":"tags","resourceId":"301","kind":"snapshot","spec":{"set":{"team":"payments"}}`, http.StatusNotImplemented)
	ch = ask(`"action":"tags","resourceId":"101","spec":{"set":{"team":"payments"}}`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != "Change labels on web-1: team=payments" || ch.Level != cloud.LevelOperate {
		t.Fatalf("labels: %+v", ch)
	}
	ch = ask(`"action":"tags","resourceId":"201","kind":"volume","spec":{"set":{"backup":"daily"}}`, http.StatusOK)
	if ch.State != "applied" {
		t.Fatalf("disk labels: %+v", ch)
	}
	if strings.Join(fake.Labelled, ",") != "instance 101,volume 201" {
		t.Fatalf("labelled = %v; Google needs the kind", fake.Labelled)
	}
	// Deletion protection, in Google's words. Asked before the VM is
	// labelled into production below, which would send it for approval.
	if ch := ask(`"action":"protect","resourceId":"101"`, http.StatusOK); ch.Summary != "Turn on deletion protection for web-1" || !fake.Protected["101"] {
		t.Fatalf("protect: %+v", ch)
	}

	// A lowercase environment label moves the VM between rules, as the
	// Environment tag does in AWS.
	if ch := ask(`"action":"tags","resourceId":"101","spec":{"set":{"environment":"production"}}`, http.StatusOK); ch.Level != cloud.LevelChange {
		t.Fatalf("an environment label is a bigger change: %+v", ch)
	}
}

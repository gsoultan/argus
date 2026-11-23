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

// Zones are the region's zones, for a launch.
func (gcpFake) Zones(context.Context, cloud.Account, cloud.Actor, string) ([]string, error) {
	return []string{"us-central1-a", "us-central1-b"}, nil
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

func TestCloudGoogleProjectMakesAttachesAndDeletesDisksAndSnapshots(t *testing.T) {
	e, fake, project := gcpEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	fake.Resources["us-central1"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: "101", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "running", Size: "e2-small",
			Details: map[string]string{"rootDeviceName": "persistent-disk-0"},
			Links:   []cloud.Ref{{Kind: cloud.KindVolume, ID: "200", Name: "persistent-disk-0"}}},
		{Kind: cloud.KindVolume, ID: "200", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "in-use", Size: "10 GiB",
			Links: []cloud.Ref{{Kind: cloud.KindInstance, ID: "101", Name: "persistent-disk-0"}}},
		{Kind: cloud.KindVolume, ID: "201", Name: "spare", Region: "us-central1", Zone: "us-central1-a", State: "available", Size: "30 GiB"},
	}
	fake.Resources[gcp.GlobalRegion] = []cloud.Resource{
		{Kind: cloud.KindSnapshot, ID: "401", Name: "nightly-1", Region: gcp.GlobalRegion, State: "ready", Size: "40 GiB"},
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
	ask := func(region, body string, want int) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
			`{"accountId":"`+c.ID+`","region":"`+region+`",`+body+`}`)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", body, w.Code, w.Body, want)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}

	// A new disk is named as Google names things, sized as Google sizes
	// them, and from a snapshot is at least the snapshot's size.
	ask("us-central1", `"action":"volume-create","spec":{"zone":"us-central1-a","sizeGb":50,"type":"pd-ssd"}`, http.StatusBadRequest)
	ask("us-central1", `"action":"volume-create","spec":{"name":"Logs","zone":"us-central1-a","sizeGb":50}`, http.StatusBadRequest)
	ask("us-central1", `"action":"volume-create","spec":{"name":"logs","zone":"us-central1-a","sizeGb":50,"type":"gp3"}`, http.StatusBadRequest)
	ask("us-central1", `"action":"volume-create","spec":{"name":"logs","zone":"us-central1-a","sizeGb":5}`, http.StatusBadRequest)
	ask("us-central1", `"action":"volume-create","spec":{"name":"spare","zone":"us-central1-a","sizeGb":50}`, http.StatusConflict)
	ask("us-central1", `"action":"volume-create","spec":{"name":"logs","zone":"us-central1-a","sizeGb":20,"snapshot":"401"}`, http.StatusBadRequest)
	ch := ask("us-central1", `"action":"volume-create","spec":{"name":"logs","zone":"us-central1-a","snapshot":"401","attachTo":"101"}`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != "Make the disk logs: 40 GB pd-balanced in us-central1-a, from nightly-1, attached to web-1" {
		t.Fatalf("create: %+v", ch)
	}
	var made cloudVolumeCreateSpec
	_ = json.Unmarshal(ch.Spec, &made)
	if made.Device != "logs" {
		t.Fatalf("the device is the disk's name in Google Cloud: %+v", made)
	}

	// A final snapshot before a delete is its own change in Google Cloud.
	ask("us-central1", `"action":"volume-delete","resourceId":"201","confirm":"spare","spec":{"finalSnapshot":true}`, http.StatusBadRequest)

	// Attaching names the device after the disk; the boot disk of a
	// running VM stays where it is.
	ch = ask("us-central1", `"action":"volume-attach","resourceId":"201","spec":{"instance":"101"}`, http.StatusOK)
	if ch.Summary != "Attach spare to web-1 as spare" {
		t.Fatalf("attach: %+v", ch)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		`{"accountId":"`+c.ID+`","region":"us-central1","action":"volume-detach","resourceId":"200"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "root disk") {
		t.Fatalf("detaching a running VM's boot disk: %d %s", w.Code, w.Body)
	}

	// A snapshot gets a Compute Engine name when none is given.
	ch = ask("us-central1", `"action":"snapshot-create","resourceId":"200"`, http.StatusOK)
	var named cloudNameSpec
	_ = json.Unmarshal(ch.Spec, &named)
	if !gcp.ValidName(named.Name) || !strings.HasPrefix(named.Name, "web-1-") {
		t.Fatalf("snapshot name = %q", named.Name)
	}
	ask("us-central1", `"action":"snapshot-create","resourceId":"200","spec":{"name":"Web 1 nightly"}`, http.StatusBadRequest)

	// Deleting a snapshot, which belongs to the whole project.
	ch = ask(gcp.GlobalRegion, `"action":"snapshot-delete","resourceId":"401","confirm":"nightly-1"`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != "Delete the snapshot nightly-1 (401, 40 GiB)" {
		t.Fatalf("snapshot delete: %+v", ch)
	}
}

func TestGCPSnapshotNames(t *testing.T) {
	at := time.Date(2026, 11, 17, 4, 5, 0, 0, time.UTC)
	for disk, want := range map[string]string{
		"web-1":                 "web-1-20261117-0405",
		"Web_1 Data":            "web-1-data-20261117-0405",
		"9lives":                "disk-9lives-20261117-0405",
		strings.Repeat("a", 70): strings.Repeat("a", 49) + "-20261117-0405",
	} {
		if got := gcpSnapshotName(disk, at); got != want || !gcp.ValidName(got) {
			t.Errorf("%q: %q, want %q", disk, got, want)
		}
	}
}

func TestCloudGoogleProjectReservesAssignsAndReleasesAddresses(t *testing.T) {
	e, fake, project := gcpEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	fake.Resources["us-central1"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: "101", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "running", PublicIP: "34.1.2.3"},
		{Kind: cloud.KindInstance, ID: "102", Name: "web-2", Region: "us-central1", Zone: "us-central1-a", State: "running"},
		{Kind: cloud.KindAddress, ID: "501", Name: "static-1", Region: "us-central1", State: "unattached", PublicIP: "35.1.1.1"},
		{Kind: cloud.KindAddress, ID: "502", Name: "static-2", Region: "us-central1", State: "attached", PublicIP: "35.2.2.2",
			Links: []cloud.Ref{{Kind: cloud.KindInstance, ID: "102", Name: "web-2"}}},
		{Kind: cloud.KindAddress, ID: "504", Name: "db-internal", Region: "us-central1", State: "reserved", Addresses: []string{"10.128.0.9"}},
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
	ask := func(region, body string, want int) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
			`{"accountId":"`+c.ID+`","region":"`+region+`",`+body+`}`)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", body, w.Code, w.Body, want)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}

	// A static address is named as Compute Engine names things, in a region.
	ask("us-central1", `"action":"address-allocate"`, http.StatusBadRequest)
	ask("us-central1", `"action":"address-allocate","spec":{"name":"Web IP"}`, http.StatusBadRequest)
	ask("us-central1", `"action":"address-allocate","spec":{"name":"static-1"}`, http.StatusConflict)
	ask(gcp.GlobalRegion, `"action":"address-allocate","spec":{"name":"web-ip"}`, http.StatusBadRequest)
	if ch := ask("us-central1", `"action":"address-allocate","spec":{"name":"web-ip"}`, http.StatusOK); ch.Summary != "Reserve the static address web-ip in us-central1" {
		t.Fatalf("reserve: %+v", ch)
	}

	// Assigning replaces the VM's ephemeral address; a VM keeps one static address.
	ask("us-central1", `"action":"address-associate","resourceId":"504","spec":{"instance":"101"}`, http.StatusBadRequest)
	ask("us-central1", `"action":"address-associate","resourceId":"501","spec":{"instance":"102"}`, http.StatusConflict)
	if ch := ask("us-central1", `"action":"address-associate","resourceId":"501","spec":{"instance":"101"}`, http.StatusOK); ch.Summary != "Assign the static address 35.1.1.1 to web-1, replacing its public address 34.1.2.3" {
		t.Fatalf("assign: %+v", ch)
	}

	// Taking it off leaves the VM without a public address, and says so.
	ch := ask("us-central1", `"action":"address-disassociate","resourceId":"502"`, http.StatusOK)
	if ch.Summary != "Take the static address 35.2.2.2 off web-2; it has no public address until it is given one" {
		t.Fatalf("unassign: %+v", ch)
	}
	if ch := ask("us-central1", `"action":"address-release","resourceId":"504","confirm":"db-internal"`, http.StatusOK); ch.Summary != "Release the static address db-internal (10.128.0.9) back to Google; the address is gone for good" {
		t.Fatalf("release: %+v", ch)
	}
}

func TestCloudGoogleProjectRemovesAVMByItsTypedName(t *testing.T) {
	e, fake, project := gcpEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	fake.Resources["us-central1"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: "101", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "running", Size: "e2-small",
			Links: []cloud.Ref{{Kind: cloud.KindVolume, ID: "200", Name: "persistent-disk-0"}}},
		{Kind: cloud.KindInstance, ID: "102", Name: "web-2", Region: "us-central1", Zone: "us-central1-a", State: "running", Size: "e2-small"},
	}
	if fake.Protected == nil {
		fake.Protected = map[string]bool{}
	}
	fake.Protected["102"] = true
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
	ask(`"action":"remove","resourceId":"101","confirm":"web"`, http.StatusBadRequest)
	ask(`"action":"remove","resourceId":"102","confirm":"web-2"`, http.StatusConflict)
	ch := ask(`"action":"remove","resourceId":"101","confirm":"web-1","spec":{"finalSnapshots":true}`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != "Remove web-1 (101) and 1 disk, 8 GB, after a final snapshot of each" {
		t.Fatalf("remove: %+v", ch)
	}
}

func TestCloudGoogleProjectLaunchesVMsInAZone(t *testing.T) {
	e, fake, project := gcpEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	fake.Resources["us-central1"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: "101", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "running", Size: "e2-small"},
		{Kind: cloud.KindSubnet, ID: "801", Name: "default", Region: "us-central1", Network: "default"},
	}
	fake.Resources[gcp.GlobalRegion] = []cloud.Resource{
		{Kind: cloud.KindFirewall, ID: "901", Name: "allow-http", Region: gcp.GlobalRegion, Network: "default"},
		{Kind: cloud.KindFirewall, ID: "903", Name: "other-net", Region: gcp.GlobalRegion, Network: "other"},
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

	// The form offers the region's zones, Google machine types, and the project's firewall rules.
	w = do(t, e.h, http.MethodGet, "/api/v1/cloud/launch-options?accountId="+c.ID+"&region=us-central1", "admin-token", "")
	var opts CloudLaunchOptions
	_ = json.Unmarshal(w.Body.Bytes(), &opts)
	if w.Code != http.StatusOK || strings.Join(opts.Zones, ",") != "us-central1-a,us-central1-b" || !slices.Contains(opts.Sizes, "e2-medium") ||
		len(opts.Subnets) != 1 || len(opts.Firewalls) != 2 {
		t.Fatalf("options: %d %+v", w.Code, opts)
	}

	ask := func(spec string, want int) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
			`{"accountId":"`+c.ID+`","region":"us-central1","action":"launch","spec":`+spec+`}`)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", spec, w.Code, w.Body, want)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}
	good := `"image":"ami-ubuntu","size":"e2-medium","diskGb":30,"subnet":"801","firewalls":["901"]`
	ask(`{"name":"web-2",`+good+`}`, http.StatusBadRequest)                         // no zone
	ask(`{"name":"web-2","zone":"europe-west1-b",`+good+`}`, http.StatusBadRequest) // another region's zone
	ask(`{"name":"web-1","zone":"us-central1-a",`+good+`}`, http.StatusConflict)    // taken in the zone
	ask(`{"name":"web-2","zone":"us-central1-a","image":"ami-ubuntu","size":"t3.medium","subnet":"801"}`, http.StatusBadRequest)
	ask(`{"name":"web-2","zone":"us-central1-a","image":"ami-ubuntu","size":"e2-medium","diskGb":8,"subnet":"801"}`, http.StatusBadRequest)
	ask(`{"name":"web-2","zone":"us-central1-a","image":"ami-ubuntu","size":"e2-medium","subnet":"801","firewalls":["903"]}`, http.StatusBadRequest)
	ask(`{"name":"web-2","zone":"us-central1-a",`+good+`,"tags":{"Team":"web"}}`, http.StatusBadRequest)
	ch := ask(`{"name":"web-2","zone":"us-central1-a",`+good+`,"tags":{"team":"web"},"extraDisks":[100]}`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != "Launch web-2: Ubuntu 24.04 LTS, e2-medium, 30 GB in us-central1-a, 1 extra disk" {
		t.Fatalf("launch: %+v", ch)
	}
}

func TestCloudGoogleProjectMakesAndDeletesImages(t *testing.T) {
	e, fake, project := gcpEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	fake.Resources["us-central1"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: "101", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "running", Size: "e2-small"},
	}
	fake.Resources[gcp.GlobalRegion] = []cloud.Resource{
		{Kind: cloud.KindImage, ID: "9001", Name: "web-golden", Region: gcp.GlobalRegion, State: "ready", Size: "10 GiB"},
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
	ask := func(region, body string, want int) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
			`{"accountId":"`+c.ID+`","region":"`+region+`",`+body+`}`)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", body, w.Code, w.Body, want)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}
	ask("us-central1", `"action":"image-create","resourceId":"101","spec":{"name":"Web Golden"}`, http.StatusBadRequest)
	ask("us-central1", `"action":"image-create","resourceId":"101","spec":{"name":"web-2","reboot":true}`, http.StatusBadRequest)
	ask("us-central1", `"action":"image-create","resourceId":"101","spec":{"name":"web-golden"}`, http.StatusConflict)
	ch := ask("us-central1", `"action":"image-create","resourceId":"101","spec":{"name":"web-golden-2"}`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != `Make the image "web-golden-2" from the boot disk of web-1, copied while it runs` {
		t.Fatalf("image: %+v", ch)
	}
	ask(gcp.GlobalRegion, `"action":"image-delete","resourceId":"9001","confirm":"web-golden","spec":{"withSnapshots":true}`, http.StatusBadRequest)
	ch = ask(gcp.GlobalRegion, `"action":"image-delete","resourceId":"9001","confirm":"web-golden"`, http.StatusOK)
	if ch.State != "applied" || ch.Summary != "Delete the image web-golden (9001)" {
		t.Fatalf("delete: %+v", ch)
	}
}

func TestCloudGoogleProjectChangesFirewallRulesByNetworkTag(t *testing.T) {
	e, fake, project := gcpEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"gcp","accountId":"`+project+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	web := []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 443, ToPort: 443, Source: "10.0.0.0/8"}}
	fake.Resources["us-central1"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: "101", Name: "web-1", Region: "us-central1", Zone: "us-central1-a", State: "running", Size: "e2-small", Network: "700",
			Links: []cloud.Ref{{Kind: cloud.KindFirewall, ID: "901", Name: "web-in"}, {Kind: cloud.KindFirewall, ID: "905", Name: "allow-internal"}}},
	}
	fake.Resources[gcp.GlobalRegion] = []cloud.Resource{
		{Kind: cloud.KindNetwork, ID: "700", Name: "default", Region: gcp.GlobalRegion},
		{Kind: cloud.KindFirewall, ID: "901", Name: "web-in", Region: gcp.GlobalRegion, Network: "700", Rules: web,
			Details: map[string]string{"direction": "ingress", "targetTags": "web-in"}},
		{Kind: cloud.KindFirewall, ID: "902", Name: "ssh-in", Region: gcp.GlobalRegion, Network: "700",
			Rules:   []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "35.235.240.0/20"}},
			Details: map[string]string{"direction": "ingress", "targetTags": "ssh-in"}},
		{Kind: cloud.KindFirewall, ID: "904", Name: "block-all", Region: gcp.GlobalRegion, Network: "700",
			Details: map[string]string{"direction": "ingress", "action": "deny"}},
		{Kind: cloud.KindFirewall, ID: "905", Name: "allow-internal", Region: gcp.GlobalRegion, Network: "700",
			Rules:   []cloud.Rule{{Inbound: true, Protocol: "all", FromPort: 0, ToPort: 65535, Source: "10.128.0.0/9"}},
			Details: map[string]string{"direction": "ingress"}},
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
	ask := func(region, body string, want int) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
			`{"accountId":"`+c.ID+`","region":"`+region+`",`+body+`}`)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", body, w.Code, w.Body, want)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}
	g := gcp.GlobalRegion
	// Making one: Google names, inbound only, every port from every source.
	ask(g, `"action":"firewall-create","spec":{"network":"700","name":"API In","rules":[{"inbound":true,"protocol":"tcp","fromPort":8443,"source":"10.1.0.0/16"}]}`, http.StatusBadRequest)
	ask(g, `"action":"firewall-create","spec":{"network":"700","name":"web-in","rules":[{"inbound":true,"protocol":"tcp","fromPort":8443,"source":"10.1.0.0/16"}]}`, http.StatusConflict)
	ask(g, `"action":"firewall-create","spec":{"network":"700","name":"api-in","rules":[{"inbound":false,"protocol":"tcp","fromPort":8443,"source":"10.1.0.0/16"}]}`, http.StatusBadRequest)
	ask(g, `"action":"firewall-create","spec":{"network":"700","name":"api-in","rules":[{"inbound":true,"protocol":"tcp","fromPort":8443,"source":"10.1.0.0/16"},{"inbound":true,"protocol":"tcp","fromPort":22,"source":"0.0.0.0/0"}]}`, http.StatusBadRequest)
	ch := ask(g, `"action":"firewall-create","spec":{"network":"700","name":"api-in","rules":[{"inbound":true,"protocol":"tcp","fromPort":8443,"source":"10.1.0.0/16"},{"inbound":true,"protocol":"tcp","fromPort":9443,"source":"10.1.0.0/16"}]}`, http.StatusOK)
	if ch.Summary != "Make firewall rule api-in in default: allow tcp 8443, tcp 9443 from 10.1.0.0/16 to VMs tagged api-in" {
		t.Fatalf("create: %+v", ch)
	}

	// Changing one.
	ask(g, `"action":"firewall-rules","resourceId":"904","spec":{"add":[{"inbound":true,"protocol":"tcp","fromPort":80,"source":"10.0.0.0/8"}]}`, http.StatusConflict)
	ask(g, `"action":"firewall-rules","resourceId":"901","spec":{"add":[{"inbound":true,"protocol":"tcp","fromPort":22,"source":"0.0.0.0/0"}]}`, http.StatusBadRequest)
	ask(g, `"action":"firewall-rules","resourceId":"901","spec":{"remove":[{"inbound":true,"protocol":"tcp","fromPort":443,"toPort":443,"source":"10.0.0.0/8"}]}`, http.StatusBadRequest)
	ch = ask(g, `"action":"firewall-rules","resourceId":"901","spec":{"add":[{"inbound":true,"protocol":"tcp","fromPort":22,"source":"0.0.0.0/0"}],"remove":[{"inbound":true,"protocol":"tcp","fromPort":443,"toPort":443,"source":"10.0.0.0/8"}]}`, http.StatusOK)
	if ch.Level != cloud.LevelRemove || !strings.HasPrefix(ch.Summary, "Change firewall rule web-in: add tcp 22 from anywhere; remove tcp 443 from 10.0.0.0/8; opens SSH (22)") ||
		!strings.HasSuffix(ch.Summary, "(reaches 1 VM)") {
		t.Fatalf("rules: %+v", ch)
	}

	// Giving a VM its rules, by tag.
	ask("us-central1", `"action":"instance-firewalls","resourceId":"101","spec":{"firewalls":["901"]}`, http.StatusBadRequest)
	ask("us-central1", `"action":"instance-firewalls","resourceId":"101","spec":{"firewalls":["901","905","904"]}`, http.StatusBadRequest)
	ch = ask("us-central1", `"action":"instance-firewalls","resourceId":"101","spec":{"firewalls":["905","902"]}`, http.StatusOK)
	if ch.Summary != "Set the firewall rules of web-1 to allow-internal, ssh-in, by its network tags" {
		t.Fatalf("instance: %+v", ch)
	}

	// Deleting one. One that reaches a VM by tag is deleted too: it is
	// how a Google firewall rule's last allow is taken away, so the summary
	// names what it reaches rather than refusing.
	ch = ask(g, `"action":"firewall-delete","resourceId":"902","confirm":"ssh-in"`, http.StatusOK)
	if ch.Summary != "Delete firewall rule ssh-in (902)" {
		t.Fatalf("delete: %+v", ch)
	}
	ch = ask(g, `"action":"firewall-delete","resourceId":"901","confirm":"901"`, http.StatusOK)
	if ch.Level != cloud.LevelRemove || ch.Summary != "Delete firewall rule web-in (901); the 1 VM it reaches (web-1) lose what it lets in" {
		t.Fatalf("delete one that reaches a VM: %+v", ch)
	}
}

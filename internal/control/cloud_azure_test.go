package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/control/cloud/azure"
)

/*
Connecting an Azure subscription the way an AWS account and a Google Cloud
project are connected: prepare says which role argus-control must hold and
gives one command, check waits for it, and connecting needs nothing copied
back. Read only unless changes are allowed, which makes one custom role in
the subscription and assigns it the same way.
*/

const azurePrincipal = "33333333-3333-4333-8333-333333333333"

func azureEnv(t *testing.T) (cloudEnv, *cloud.Fake, string) {
	t.Helper()
	e := cloudAPI(t)
	fake := &cloud.Fake{Name: "azure",
		BaseIdentity: cloud.Base{Principal: azurePrincipal, ARN: azurePrincipal, Source: "the managed identity of the VM argus-control runs on"},
		Roles:        map[string]string{}, Regions: []string{"eastus", "westeurope"}, Resources: map[string][]cloud.Resource{}}
	e.a.SetCloudProvider(fake)
	n := time.Now().UnixNano()
	sub := fmt.Sprintf("%08x-%04x-4000-8000-%012x", uint32(n>>32), uint16(n>>16), uint64(n)&0xffffffffffff)
	t.Cleanup(func() {
		ctx := context.Background()
		// Changes asked for here hold the account's row, so they go first.
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_changes WHERE account_id IN (SELECT id FROM cloud_accounts WHERE account_id = $1)`, sub)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_resources WHERE account_id IN (SELECT id FROM cloud_accounts WHERE account_id = $1)`, sub)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM cloud_accounts WHERE account_id = $1`, sub)
	})
	return e, fake, sub
}

func TestCloudConnectAzureSubscription(t *testing.T) {
	e, fake, sub := azureEnv(t)
	reader := azure.RoleID(sub, azure.ReaderRoleID)

	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"azure","accountId":"`+sub+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", w.Code, w.Body)
	}
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.ReadRoleARN != reader || p.RoleName != "Reader" || p.CanDeploy || p.ManageRoleARN != "" || p.Template != "" {
		t.Fatalf("prepare must point at Reader in the subscription and nothing else: %+v", p)
	}
	for _, want := range []string{"argus-external-id=" + p.ExternalID, "grant " + azurePrincipal + " ", azure.ReaderRoleID, "S=" + sub} {
		if !strings.Contains(p.Command, want) {
			t.Fatalf("the command lacks %q:\n%s", want, p.Command)
		}
	}
	// The AWS account's external id is not this subscription's.
	if aws := e.prepare(t); aws.ExternalID == p.ExternalID {
		t.Fatal("two accounts must not share an external id")
	}

	for body, why := range map[string]string{
		`{"provider":"azure","accountId":"412788931204"}`:                                "an AWS account id is not a subscription id",
		`{"provider":"azure","accountId":"` + strings.ToUpper(sub) + `"}`:                "a subscription id is kept in lower case, so one subscription is one account",
		`{"provider":"azure","accountId":"payments-prod-4182"}`:                          "a project id is not a subscription id",
		`{"provider":"gcp","accountId":"` + sub + `"}`:                                   "a subscription id is not a project id",
		`{"provider":"oracle","accountId":"` + sub + `"}`:                                "a cloud Argus does not connect",
		`{"provider":"azure","accountId":"` + sub + `/resourceGroups/x","manage":false}`: "a path is not a subscription id",
	} {
		if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", why, w.Code, w.Body)
		}
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "operator-token", `{"provider":"azure","accountId":"`+sub+`"}`); w.Code == http.StatusOK {
		t.Fatalf("only an administrator prepares a connection: %d", w.Code)
	}

	check := func() CloudCheck {
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/check", "admin-token", `{"provider":"azure","accountId":"`+sub+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("check: %d %s", w.Code, w.Body)
		}
		var c CloudCheck
		_ = json.Unmarshal(w.Body.Bytes(), &c)
		return c
	}
	if c := check(); c.Ready || !strings.Contains(c.Detail, "The subscription does not let Argus in yet") {
		t.Fatalf("no role yet must be not ready, in words: %+v", c)
	}
	fake.Roles[reader] = "argus-someone-elses-id"
	if c := check(); c.Ready {
		t.Fatal("a role given for another organization must not count")
	}
	// Connecting before the role is there is refused, and the refusal is
	// in the audit log.
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"payments-azure","accountId":"`+sub+`"}`); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), "subscription "+sub) {
		t.Fatalf("connecting without the role must be refused, naming the subscription: %d %s", w.Code, w.Body)
	}
	if n := auditCount(t, e.s, "cloud.account.refused", cloudTarget("azure", sub, "payments-azure")); n != 1 {
		t.Fatalf("a refused connection must be audited once, got %d", n)
	}
	fake.Roles[reader] = p.ExternalID
	if c := check(); !c.Ready {
		t.Fatalf("the role is there: %+v", c)
	}

	// Another role cannot be named in Reader's place.
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"payments-azure","accountId":"`+sub+`","roleArn":"`+azure.RoleID(sub, "8e3af657-a8ff-443c-a75c-2fe8c4bcb635")+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("only Reader at the subscription may be the read role: %d %s", w.Code, w.Body)
	}
	// Changes are not allowed on the word of the request: the manage role
	// has to be there, for this organization, or nothing is connected.
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"payments-azure","accountId":"`+sub+`","manage":true}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("connecting with changes and no manage role must be refused: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"payments-azure","accountId":"`+sub+`","manageRoleArn":"`+azure.RoleID(sub, "b24988ac-6180-42a0-ab88-20f7382dd24c")+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("Contributor, or any role but the subscription's Argus manage, must not be the manage role: %d %s", w.Code, w.Body)
	}

	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"payments-azure","accountId":"`+sub+`","environment":"staging"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.Provider != "azure" || c.RoleARN != reader || c.ManageRoleARN != "" || c.Identity != DefaultCloudIdentity || c.Access != "read" {
		t.Fatalf("connected wrong: %+v", c)
	}
	if n := auditCount(t, e.s, "cloud.account.connected", cloudTarget("azure", sub, "payments-azure")); n != 1 {
		t.Fatalf("connecting must be audited once, got %d", n)
	}
	if !contains(fake.CallLog(), "verify "+e.admin) {
		t.Fatalf("the role must be checked as the person connecting: %v", fake.CallLog())
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"again","accountId":"`+sub+`"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "subscription "+sub) {
		t.Fatalf("a subscription connects once: %d %s", w.Code, w.Body)
	}
	// Allowing changes later: refused until the manage role is there for
	// this organization, refused for any other role, and audited both ways.
	manage := azure.RoleID(sub, azure.ManageRoleID(sub))
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token", `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("allowing changes with no manage role must be refused: %d %s", w.Code, w.Body)
	}
	fake.Roles[manage] = "argus-someone-elses-id"
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token", `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a manage role given for another organization must not count: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token",
		`{"roleArn":"`+azure.RoleID(sub, "b24988ac-6180-42a0-ab88-20f7382dd24c")+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("another role must not be named as the manage role: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "operator-token", `{}`); w.Code == http.StatusOK {
		t.Fatalf("only an administrator allows changes: %d", w.Code)
	}
	fake.Roles[manage] = p.ExternalID
	w = do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("allow changes: %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.ManageRoleARN != manage || c.Access != "manage" {
		t.Fatalf("changes allowed wrong: %+v", c)
	}
	if n := auditCount(t, e.s, "cloud.account.manage_allowed", cloudTarget("azure", sub, "payments-azure")); n != 1 {
		t.Fatalf("allowing changes must be audited once, got %d", n)
	}
	if n := auditCount(t, e.s, "cloud.account.refused", cloudTarget("azure", sub, "payments-azure")); n < 3 {
		t.Fatalf("each refusal must be audited, got %d", n)
	}

	w = do(t, e.h, http.MethodGet, "/api/v1/cloud/accounts", "admin-token", "")
	var view cloudAccountsView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	providers := map[string]int{}
	for _, b := range view.Bases {
		providers[b.Provider]++
	}
	if providers["aws"] != 1 || providers["azure"] != 1 {
		t.Fatalf("each cloud's identity must be listed once: %+v", view.Bases)
	}
	for _, a := range view.Accounts {
		if a.ID == c.ID && a.IdentityMissing {
			t.Fatal("a subscription on Azure's Main identity must not read as having lost it to AWS's")
		}
		// The console offers only what Argus does in Azure so far.
		if a.ID == c.ID && (!contains(actionNames(a.Actions), "stop") || contains(actionNames(a.Actions), "remove") || contains(actionNames(a.Actions), "launch")) {
			t.Fatalf("a subscription must list the changes Argus makes there, and no others: %v", a.Actions)
		}
	}

	// Disconnecting says what is left in the subscription.
	if w := do(t, e.h, http.MethodDelete, "/api/v1/cloud/accounts/"+c.ID, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	var detail string
	if err := e.s.pool.QueryRow(context.Background(), `SELECT detail FROM audit_events WHERE action = 'cloud.account.disconnected' AND target = $1`,
		cloudTarget("azure", sub, "payments-azure")).Scan(&detail); err != nil || !strings.Contains(detail, "role assignment") {
		t.Fatalf("the disconnect must be audited, saying the role assignment stays: %q %v", detail, err)
	}
}

// An identity that does not answer says how to give argus-control one, and
// only to the people who can.
func TestCloudAzureIdentityNextStep(t *testing.T) {
	if got := cloudIdentityNextStep(context.Background(), "azure"); !strings.Contains(got, "cloud.azure") || !strings.Contains(got, "managed identity") {
		t.Errorf("default organization: %q", got)
	}
	if got := cloudIdentityNextStep(WithTenant(context.Background(), &Tenant{ID: "a1b2c3d4e5f6"}), "azure"); !strings.Contains(got, "platform's owner") {
		t.Errorf("another organization: %q", got)
	}
	e, fake, sub := azureEnv(t)
	fake.BaseErr = cloud.ErrNoBase
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"azure","accountId":"`+sub+`"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Azure identity") {
		t.Fatalf("no identity: %d %s", w.Code, w.Body)
	}
}

// The inventory an Azure subscription's sync stores is listed like any
// other account's, and a server whose agent names its VM is linked to it.
func TestCloudAzureInventoryAndServerMatch(t *testing.T) {
	e, fake, sub := azureEnv(t)
	reader := azure.RoleID(sub, azure.ReaderRoleID)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"azure","accountId":"`+sub+`"}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	fake.Roles[reader] = p.ExternalID
	const vm = "aaaaaaaa-0000-4000-8000-000000000001"
	fake.Resources["eastus"] = []cloud.Resource{{Kind: cloud.KindInstance, ID: vm, Name: "web-1", Region: "eastus", Zone: "eastus-2",
		State: "running", Size: "Standard_B2s", Addresses: []string{"10.0.1.5"}}}
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token", `{"provider":"azure","name":"payments-azure","accountId":"`+sub+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	waitCloudSync(t, e, c)
	w = do(t, e.h, http.MethodGet, "/api/v1/cloud/resources?kind=instance", "admin-token", "")
	var out struct {
		Items []CloudItem `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	var got *CloudItem
	for i := range out.Items {
		if out.Items[i].Resource.ID == vm && out.Items[i].ProviderAccount == sub {
			got = &out.Items[i]
		}
	}
	if got == nil || got.Provider != "azure" || got.ProviderAccount != sub || got.Resource.Size != "Standard_B2s" {
		t.Fatalf("the VM must be listed under its subscription: %+v", got)
	}
	items := []CloudItem{*got}
	matchServers(items, []Asset{{ID: "s1", Hostname: "web-1.internal"}}, nil,
		map[string]HostCloud{"s1": {Provider: "azure", Account: sub, Region: "eastus", ID: vm}})
	if items[0].Server == nil || items[0].Server.By != "agent" {
		t.Fatalf("a server whose agent names the VM must be linked by that: %+v", items[0].Server)
	}
}

func actionNames(as []cloud.Action) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = string(a)
	}
	return out
}

// Changes in a subscription: the prepared command makes the manage role,
// a stop says it deallocates, a size changes on a stopped VM only, and tags
// follow Azure's rules.
func TestCloudAzureSubscriptionStopsResizesAndTags(t *testing.T) {
	e, fake, sub := azureEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"azure","accountId":"`+sub+`","manage":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("prepare with changes: %d %s", w.Code, w.Body)
	}
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.ManageRoleARN != azure.RoleID(sub, azure.ManageRoleID(sub)) || p.ManageRoleName != azure.ManageRoleName {
		t.Fatalf("prepare must name the subscription's manage role: %+v", p)
	}
	for _, want := range []string{"roleDefinitions/$M", "M=" + azure.ManageRoleID(sub), "Microsoft.Compute/virtualMachines/deallocate/action"} {
		if !strings.Contains(p.Command, want) {
			t.Fatalf("the command lacks %q:\n%s", want, p.Command)
		}
	}
	for _, act := range azure.ManageActions {
		if strings.HasPrefix(act, "Microsoft.Authorization/") || strings.Contains(act, "*") {
			t.Fatalf("the manage role must hold no right to grant access and no wildcard: %s", act)
		}
	}
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	const vm = "aaaaaaaa-0000-4000-8000-000000000001"
	fake.Resources["eastus"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: vm, Name: "web-1", Region: "eastus", Zone: "eastus-2", State: "running", Size: "Standard_B2s"},
		{Kind: cloud.KindVolume, ID: "bbbbbbbb-0000-4000-8000-000000000001", Name: "web-1_OsDisk", Region: "eastus", State: "in-use"},
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"web-azure","accountId":"`+sub+`","environment":"development","manage":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect with changes: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	waitCloudSync(t, e, c)
	ask := func(body string, want int) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", `{"accountId":"`+c.ID+`","region":"eastus",`+body+`}`)
		if w.Code != want {
			t.Fatalf("%s: %d %s, want %d", body, w.Code, w.Body, want)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}

	// A size is Azure's, and changes on a stopped VM only.
	ask(`"action":"resize","resourceId":"`+vm+`","spec":{"size":"t3.medium"}`, http.StatusBadRequest)
	ask(`"action":"resize","resourceId":"`+vm+`","spec":{"size":"Standard_B2s"}`, http.StatusBadRequest)
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		`{"accountId":"`+c.ID+`","region":"eastus","action":"resize","resourceId":"`+vm+`","spec":{"size":"Standard_D2s_v5"}}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stopped VM") {
		t.Fatalf("a running VM: %d %s", w.Code, w.Body)
	}
	// A stop says what Azure does with it, and is asked as the person.
	if ch := ask(`"action":"stop","resourceId":"`+vm+`"`, http.StatusOK); ch.State != "applied" || !strings.Contains(ch.Summary, "deallocate") {
		t.Fatalf("stop: %+v", ch)
	}
	if !contains(fake.CallLog(), "stop "+vm+" "+e.admin) {
		t.Fatalf("the stop must be made as the person who asked: %v", fake.CallLog())
	}
	fake.Resources["eastus"][0].State = "stopped"
	deadline := time.Now().Add(5 * time.Second)
	for !e.a.SyncCloudAccount(context.Background(), c) {
		if time.Now().After(deadline) {
			t.Fatal("the subscription was being read for too long")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ch := ask(`"action":"resize","resourceId":"`+vm+`","spec":{"size":"Standard_D2s_v5"}`, http.StatusOK); ch.State != "applied" ||
		ch.Summary != "Change web-1 from Standard_B2s to Standard_D2s_v5" {
		t.Fatalf("resize: %+v", ch)
	}

	// Tags: Azure's rules, on VMs, disks and snapshots for now.
	ask(`"action":"tags","resourceId":"`+vm+`","spec":{"set":{"a/b":"x"}}`, http.StatusBadRequest)
	ask(`"action":"tags","resourceId":"`+vm+`","spec":{"set":{"Microsoft-owner":"x"}}`, http.StatusBadRequest)
	ask(`"action":"tags","resourceId":"bbbbbbbb-0000-4000-8000-000000000001","kind":"firewall","spec":{"set":{"team":"payments"}}`, http.StatusNotImplemented)
	if ch := ask(`"action":"tags","resourceId":"`+vm+`","spec":{"set":{"Team":"payments"}}`, http.StatusOK); ch.State != "applied" ||
		ch.Summary != "Change tags on web-1: Team=payments" {
		t.Fatalf("tags: %+v", ch)
	}
}

// Disks in a subscription: Azure's own rules are checked before anything
// is asked of it, and each refusal says what to do instead.
func TestCloudAzureSubscriptionDisksAndSnapshots(t *testing.T) {
	e, fake, sub := azureEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"azure","accountId":"`+sub+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	for _, want := range []string{"Microsoft.Compute/disks/write", "Microsoft.Compute/snapshots/delete", "Microsoft.Compute/virtualMachines/attachDetachDataDisks/action"} {
		if !strings.Contains(p.Command, want) {
			t.Fatalf("the command lacks %q:\n%s", want, p.Command)
		}
	}
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	const (
		vm      = "aaaaaaaa-0000-4000-8000-000000000001"
		osDisk  = "bbbbbbbb-0000-4000-8000-000000000001"
		data    = "bbbbbbbb-0000-4000-8000-000000000002"
		left    = "bbbbbbbb-0000-4000-8000-000000000003"
		spare   = "bbbbbbbb-0000-4000-8000-000000000004"
		nightly = "cccccccc-0000-4000-8000-000000000001"
	)
	rg := map[string]string{"resourceGroup": "Web-RG"}
	with := func(extra ...string) map[string]string {
		out := map[string]string{"resourceGroup": "Web-RG"}
		for i := 0; i+1 < len(extra); i += 2 {
			out[extra[i]] = extra[i+1]
		}
		return out
	}
	onVM := []cloud.Ref{{Kind: cloud.KindInstance, ID: vm, Name: "web-1"}}
	fake.Resources["eastus"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: vm, Name: "web-1", Region: "eastus", Zone: "eastus-2", State: "running", Size: "Standard_B2s",
			Details: with("rootDeviceName", "web-1_OsDisk")},
		{Kind: cloud.KindVolume, ID: osDisk, Name: "web-1_OsDisk", Region: "eastus", Zone: "eastus-2", State: "in-use", Size: "30 GiB", Links: onVM,
			Details: with("type", "Premium_LRS", "platform", "linux")},
		{Kind: cloud.KindVolume, ID: data, Name: "web-1-data", Region: "eastus", Zone: "eastus-2", State: "in-use", Size: "128 GiB", Links: onVM,
			Details: with("type", "StandardSSD_LRS")},
		{Kind: cloud.KindVolume, ID: left, Name: "left-over", Region: "eastus", State: "available", Size: "200 GiB", Details: with("type", "Standard_LRS")},
		{Kind: cloud.KindVolume, ID: spare, Name: "spare", Region: "eastus", Zone: "eastus-2", State: "available", Size: "64 GiB", Details: with("type", "Premium_LRS")},
		{Kind: cloud.KindSnapshot, ID: nightly, Name: "nightly", Region: "eastus", State: "completed", Size: "30 GiB", Details: rg},
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"disks-azure","accountId":"`+sub+`","environment":"development","manage":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect with changes: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	waitCloudSync(t, e, c)
	ask := func(body string, want int, says string) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", `{"accountId":"`+c.ID+`","region":"eastus",`+body+`}`)
		if w.Code != want || !strings.Contains(w.Body.String(), says) {
			t.Fatalf("%s: %d %s, want %d saying %q", body, w.Code, w.Body, want, says)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}
	bad, conflict, ok := http.StatusBadRequest, http.StatusConflict, http.StatusOK

	// Grow: size only, upwards, and the disk a VM starts from only while it is stopped.
	ask(`"action":"volume-grow","resourceId":"`+data+`","spec":{"sizeGb":64}`, bad, "only grows")
	ask(`"action":"volume-grow","resourceId":"`+data+`","spec":{"sizeGb":256,"type":"Premium_LRS"}`, bad, "size only")
	ask(`"action":"volume-grow","resourceId":"`+data+`","spec":{"sizeGb":40000}`, bad, "at most 32767")
	ask(`"action":"volume-grow","resourceId":"`+osDisk+`","spec":{"sizeGb":64}`, conflict, "Stop web-1 first")
	if ch := ask(`"action":"volume-grow","resourceId":"`+data+`","spec":{"sizeGb":256}`, ok, ""); ch.State != "applied" || ch.Summary != "Grow web-1-data from 128 to 256 GiB" ||
		!strings.Contains(string(ch.Result), "Azure grows the disk") {
		t.Fatalf("grow: %+v", ch)
	}

	// Attach: a disk without a zone does not go on a VM in one, and Azure picks the LUN.
	ask(`"action":"volume-attach","resourceId":"`+left+`","spec":{"instance":"`+vm+`"}`, bad, "left-over is in no zone and web-1 in eastus-2")
	ask(`"action":"volume-attach","resourceId":"`+data+`","spec":{"instance":"`+vm+`"}`, conflict, "already attached")
	if ch := ask(`"action":"volume-attach","resourceId":"`+spare+`","spec":{"instance":"`+vm+`","device":"/dev/sdf"}`, ok, ""); ch.Summary != "Attach spare to web-1 as a data disk" {
		t.Fatalf("attach: %+v", ch)
	}
	// Detach: never the disk the VM starts from.
	ask(`"action":"volume-detach","resourceId":"`+osDisk+`"`, conflict, "Azure does not detach that one")
	ask(`"action":"volume-detach","resourceId":"`+left+`"`, conflict, "not attached")
	if ch := ask(`"action":"volume-detach","resourceId":"`+data+`"`, ok, ""); ch.Summary != "Detach web-1-data from web-1" {
		t.Fatalf("detach: %+v", ch)
	}

	// Snapshots get a name Azure takes, and never one in use.
	ask(`"action":"snapshot-create","resourceId":"`+left+`","spec":{"name":"a/b"}`, bad, "snapshot's name")
	ask(`"action":"snapshot-create","resourceId":"`+left+`","spec":{"name":"NIGHTLY"}`, conflict, "already has a snapshot named NIGHTLY")
	if ch := ask(`"action":"snapshot-create","resourceId":"`+left+`"`, ok, ""); !strings.HasPrefix(ch.Summary, `Snapshot left-over as "left-over-20`) {
		t.Fatalf("snapshot: %+v", ch)
	}

	// A new disk needs a resource group Argus has read, a type of Azure's and a free name.
	ask(`"action":"volume-create","spec":{"name":"d1","sizeGb":8}`, bad, "pick the resource group")
	ask(`"action":"volume-create","spec":{"name":"d1","group":"elsewhere","sizeGb":8}`, bad, "read nothing in a resource group named elsewhere")
	ask(`"action":"volume-create","spec":{"name":"d1","group":"Web-RG","sizeGb":8,"type":"gp3"}`, bad, "pick a disk type")
	ask(`"action":"volume-create","spec":{"name":"bad name","group":"Web-RG","sizeGb":8}`, bad, "name the disk")
	ask(`"action":"volume-create","spec":{"name":"Left-Over","group":"web-rg","sizeGb":8}`, conflict, "already has a disk named Left-Over")
	ask(`"action":"volume-create","spec":{"name":"d1","group":"Web-RG","sizeGb":8,"zone":"eastus-9"}`, bad, "eastus-1 to eastus-3")
	ask(`"action":"volume-create","spec":{"name":"d1","group":"Web-RG","sizeGb":8,"type":"Premium_ZRS","zone":"eastus-1"}`, bad, "leave the zone out")
	ask(`"action":"volume-create","spec":{"name":"d1","group":"Web-RG","sizeGb":8,"zone":"eastus-1","attachTo":"`+vm+`"}`, bad, "web-1 is in eastus-2")
	ask(`"action":"volume-create","spec":{"name":"d1","group":"Web-RG","sizeGb":8,"snapshot":"`+nightly+`"}`, bad, "at least 30 GiB")
	ask(`"action":"volume-create","spec":{"name":"d1","group":"Web-RG","sizeGb":0}`, bad, "1 to 32767 GiB")
	if ch := ask(`"action":"volume-create","spec":{"name":"d1","group":"web-rg","snapshot":"`+nightly+`"}`, ok, ""); ch.Summary !=
		"Make the disk d1: 30 GiB StandardSSD_LRS in eastus, resource group Web-RG, from nightly" {
		t.Fatalf("new disk: %+v", ch)
	}
	// Attached, it goes where the VM is: its zone and its resource group.
	if ch := ask(`"action":"volume-create","spec":{"name":"d2","sizeGb":16,"type":"Premium_LRS","attachTo":"`+vm+`"}`, ok, ""); ch.Summary !=
		"Make the disk d2: 16 GiB Premium_LRS in eastus-2, resource group Web-RG, attached to web-1" {
		t.Fatalf("new attached disk: %+v", ch)
	}

	// Delete: typed, of a disk nothing uses, and a final snapshot is its own change.
	ask(`"action":"volume-delete","resourceId":"`+left+`","confirm":"left-over","spec":{"finalSnapshot":true}`, bad, "in Azure a final snapshot is its own change")
	ask(`"action":"volume-delete","resourceId":"`+left+`","confirm":"nope"`, bad, "type the disk's name")
	ask(`"action":"volume-delete","resourceId":"`+osDisk+`","confirm":"web-1_OsDisk"`, conflict, "detach it first")
	if ch := ask(`"action":"volume-delete","resourceId":"`+left+`","confirm":"left-over"`, ok, ""); !strings.HasPrefix(ch.Summary, "Delete the disk left-over") {
		t.Fatalf("delete: %+v", ch)
	}
	if ch := ask(`"action":"snapshot-delete","resourceId":"`+nightly+`","confirm":"nightly"`, ok, ""); !strings.HasPrefix(ch.Summary, "Delete the snapshot nightly") {
		t.Fatalf("snapshot delete: %+v", ch)
	}
	// Images are refused by the real provider, whatever the console sends.
	real := azure.New(azure.Options{})
	for _, a := range []cloud.Action{cloud.ActionImageCreate, cloud.ActionImageDelete} {
		if cloud.Supports(real, a) {
			t.Fatalf("%s is not made in Azure yet", a)
		}
	}
}

// Public addresses in a subscription: named, in a resource group, on a VM
// through its network interface, and never taken off a load balancer.
func TestCloudAzureSubscriptionPublicAddresses(t *testing.T) {
	e, fake, sub := azureEnv(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token", `{"provider":"azure","accountId":"`+sub+`","manage":true}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	for _, want := range []string{"Microsoft.Network/publicIPAddresses/write", "Microsoft.Network/networkInterfaces/write"} {
		if !strings.Contains(p.Command, want) {
			t.Fatalf("the command lacks %q:\n%s", want, p.Command)
		}
	}
	fake.Roles[p.ReadRoleARN] = p.ExternalID
	fake.Roles[p.ManageRoleARN] = p.ExternalID
	const (
		web  = "aaaaaaaa-0000-4000-8000-000000000001"
		api  = "aaaaaaaa-0000-4000-8000-000000000002"
		ip1  = "dddddddd-0000-4000-8000-000000000001"
		free = "dddddddd-0000-4000-8000-000000000003"
		lb   = "dddddddd-0000-4000-8000-000000000004"
	)
	rg := map[string]string{"resourceGroup": "Web-RG"}
	fake.Resources["eastus"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: web, Name: "web-1", Region: "eastus", State: "running", PublicIP: "20.1.2.3", Details: rg},
		{Kind: cloud.KindInstance, ID: api, Name: "api-1", Region: "eastus", State: "running", Details: rg},
		{Kind: cloud.KindAddress, ID: ip1, Name: "web-1-ip", Region: "eastus", State: "attached", PublicIP: "20.1.2.3", Details: rg,
			Links: []cloud.Ref{{Kind: cloud.KindInstance, ID: web, Name: "web-1"}}},
		{Kind: cloud.KindAddress, ID: free, Name: "free-east", Region: "eastus", State: "unattached", PublicIP: "20.5.5.5", Details: rg},
		{Kind: cloud.KindAddress, ID: lb, Name: "lb-ip", Region: "eastus", State: "attached", PublicIP: "20.6.6.6", Details: rg},
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"net-azure","accountId":"`+sub+`","environment":"development","manage":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("connect with changes: %d %s", w.Code, w.Body)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	waitCloudSync(t, e, c)
	ask := func(region, body string, want int, says string) CloudChange {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token", `{"accountId":"`+c.ID+`","region":"`+region+`",`+body+`}`)
		if w.Code != want || !strings.Contains(w.Body.String(), says) {
			t.Fatalf("%s: %d %s, want %d saying %q", body, w.Code, w.Body, want, says)
		}
		var ch CloudChange
		_ = json.Unmarshal(w.Body.Bytes(), &ch)
		return ch
	}
	bad, conflict, ok := http.StatusBadRequest, http.StatusConflict, http.StatusOK

	// A new address is named and goes into a resource group Argus has read.
	ask("eastus", `"action":"address-allocate","spec":{"name":"api-ip"}`, bad, "pick the resource group")
	ask("eastus", `"action":"address-allocate","spec":{"name":"bad name","group":"Web-RG"}`, bad, "name the address")
	ask("eastus", `"action":"address-allocate","spec":{"name":"api-ip","group":"elsewhere"}`, bad, "read nothing in a resource group named elsewhere")
	ask("eastus", `"action":"address-allocate","spec":{"name":"FREE-EAST","group":"web-rg"}`, conflict, "already has a public address named FREE-EAST")
	ask("mars", `"action":"address-allocate","spec":{"name":"api-ip","group":"Web-RG"}`, bad, "")
	if ch := ask("eastus", `"action":"address-allocate","spec":{"name":"api-ip","group":"web-rg"}`, ok, ""); ch.State != "applied" ||
		ch.Summary != "Make the public address api-ip in eastus, resource group Web-RG" {
		t.Fatalf("make: %+v", ch)
	}

	// On a VM: one address each, and only an address nothing has.
	ask("eastus", `"action":"address-associate","resourceId":"`+free+`","spec":{"instance":"`+web+`"}`, conflict, "web-1 already has the public address 20.1.2.3")
	ask("eastus", `"action":"address-associate","resourceId":"`+ip1+`","spec":{"instance":"`+api+`"}`, conflict, "20.1.2.3 is attached to something")
	if ch := ask("eastus", `"action":"address-associate","resourceId":"`+free+`","spec":{"instance":"`+api+`"}`, ok, ""); ch.Summary != "Put the public address 20.5.5.5 on api-1" {
		t.Fatalf("attach: %+v", ch)
	}
	// Off a VM, never off a load balancer.
	ask("eastus", `"action":"address-disassociate","resourceId":"`+lb+`"`, conflict, "on a load balancer or a gateway, not a VM")
	if ch := ask("eastus", `"action":"address-disassociate","resourceId":"`+ip1+`"`, ok, ""); ch.Summary !=
		"Take the public address 20.1.2.3 off web-1; it has no public address until it is given one" {
		t.Fatalf("take off: %+v", ch)
	}
	// Deleting takes the address typed, and one nothing uses.
	ask("eastus", `"action":"address-release","resourceId":"`+lb+`","confirm":"20.6.6.6"`, conflict, "take it off first")
	ask("eastus", `"action":"address-release","resourceId":"`+lb+`","confirm":"nope"`, bad, "type the address")
}

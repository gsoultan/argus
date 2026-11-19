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
back. Read only, for now.
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
		`{"provider":"azure","accountId":"` + sub + `","manage":true}`:                   "changes in a subscription are not offered yet",
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
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"azure","name":"payments-azure","accountId":"`+sub+`","manage":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("connecting with changes must be refused: %d %s", w.Code, w.Body)
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
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token", `{}`); w.Code != http.StatusNotImplemented {
		t.Fatalf("allowing changes in a subscription must be refused: %d %s", w.Code, w.Body)
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
		if out.Items[i].Resource.ID == vm {
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

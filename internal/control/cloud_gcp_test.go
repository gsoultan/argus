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
)

/*
Connecting a Google Cloud project the way an AWS account is connected:
prepare says where argus-read will be and gives one command, check waits
for it, and connecting needs nothing copied back. Read only, for now.
*/

func gcpEnv(t *testing.T) (cloudEnv, *cloud.Fake, string) {
	t.Helper()
	e := cloudAPI(t)
	fake := &cloud.Fake{Name: "gcp",
		BaseIdentity: cloud.Base{Principal: "argus-control@ops-home.iam.gserviceaccount.com",
			ARN: "argus-control@ops-home.iam.gserviceaccount.com", Source: "the service account of the VM argus-control runs on"},
		Roles: map[string]string{}, Regions: []string{"europe-west1", "us-central1", "global"}, Resources: map[string][]cloud.Resource{}}
	e.a.SetCloudProvider(fake)
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

	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token",
		`{"provider":"gcp","accountId":"`+project+`","manage":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("changes in a project are not offered yet: %d %s", w.Code, w.Body)
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
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"gcp","name":"payments-gcp","accountId":"`+project+`","manage":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("connecting with changes must be refused: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/cloud/accounts/"+c.ID+"/manage", "admin-token", `{}`); w.Code != http.StatusNotImplemented {
		t.Fatalf("allowing changes in a project must be refused: %d %s", w.Code, w.Body)
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

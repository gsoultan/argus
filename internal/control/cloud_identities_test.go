package control

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
More than one cloud identity: each account keeps the one it was connected
with, an organization only uses those made available to it, a move happens
only once Argus gets in with the new one, and an identity in use cannot go.
Against fake providers, one per identity.
*/

const otherOrg = "a1b2c3d4e5f6"

// withSecond gives the environment a second identity, "second", with its
// own provider and so its own idea of which roles trust it. orgs limits who
// may use it.
func (e cloudEnv) withSecond(def bool, orgs ...string) *cloud.Fake {
	second := &cloud.Fake{
		BaseIdentity: cloud.Base{Principal: "arn:aws:iam::771203458812:user/argus-second",
			ARN: "arn:aws:iam::771203458812:user/argus-second", Source: "the access key"},
		Roles:     map[string]string{},
		Regions:   []string{"ap-southeast-1"},
		Resources: map[string][]cloud.Resource{},
	}
	e.a.SetCloudIdentities("aws", []CloudIdentity{
		{ID: DefaultCloudIdentity, Name: "Main", Default: !def, Provider: e.fake},
		{ID: "second", Name: "Second", Default: def, Organizations: orgs, Provider: second},
	})
	return second
}

func TestCloudIdentityAvailability(t *testing.T) {
	a := &API{}
	main, only, gov := &cloud.Fake{}, &cloud.Fake{}, &cloud.Fake{}
	a.SetCloudIdentities("aws", []CloudIdentity{
		{ID: "main", Name: "Main", Default: true, Organizations: []string{DefaultTenantID}, Provider: main},
		{ID: "only", Name: "Only", Organizations: []string{otherOrg}, Provider: only},
		{ID: "gov", Name: "Gov", Organizations: []string{DefaultTenantID, otherOrg}, Provider: gov},
	})
	def := WithTenant(context.Background(), &Tenant{ID: DefaultTenantID})
	other := WithTenant(context.Background(), &Tenant{ID: otherOrg})
	third := WithTenant(context.Background(), &Tenant{ID: "zzzzzzzzzzzz"})

	if i, err := a.cloudIdentity(def, "aws", ""); err != nil || i.ID != "main" {
		t.Errorf("default organization's default = %+v, %v; want main", i, err)
	}
	// The platform's default is not the other organization's to use, and it
	// has two: it must choose.
	if _, err := a.cloudIdentity(other, "aws", ""); identityStatus(err) != http.StatusBadRequest {
		t.Errorf("other organization with two to choose from: %v", err)
	}
	if _, err := a.cloudIdentity(other, "aws", "main"); identityStatus(err) != http.StatusForbidden {
		t.Errorf("another organization's identity: %v, want refused", err)
	}
	if i, err := a.cloudIdentity(other, "aws", "only"); err != nil || i.ID != "only" {
		t.Errorf("its own identity: %+v %v", i, err)
	}
	if _, err := a.cloudIdentity(third, "aws", ""); identityStatus(err) != http.StatusConflict {
		t.Errorf("an organization with none: %v, want the next step", err)
	}
	if ids := a.cloudIdentitiesFor(other, "aws"); len(ids) != 2 || ids[0].ID != "only" || ids[1].ID != "gov" {
		t.Errorf("listed for the other organization: %+v", ids)
	}
	// One to use is that one, without choosing.
	a.SetCloudIdentities("aws", []CloudIdentity{
		{ID: "main", Name: "Main", Default: true, Organizations: []string{DefaultTenantID}, Provider: main},
		{ID: "only", Name: "Only", Organizations: []string{otherOrg}, Provider: only},
	})
	if i, err := a.cloudIdentity(other, "aws", ""); err != nil || i.ID != "only" {
		t.Errorf("the only one: %+v %v", i, err)
	}
	// An account on an identity the organization lost is never reached
	// with another.
	if _, err := a.cloudProviderFor(other, CloudAccount{Provider: "aws", Name: "x", Identity: "main"}); err == nil ||
		!strings.Contains(err.Error(), "Change identity") {
		t.Errorf("account on another organization's identity: %v", err)
	}
	if _, err := a.cloudIdentity(def, "gcp", ""); identityStatus(err) != http.StatusNotImplemented {
		t.Errorf("an unconfigured provider: %v", err)
	}
}

// Connecting with an identity this organization may not use is refused
// and recorded, not quietly done with the default.
func TestCloudConnectRefusesAnotherOrganizationsIdentity(t *testing.T) {
	e := cloudAPI(t)
	second := e.withSecond(false, otherOrg)
	second.Roles[e.roleARN()] = e.prepare(t).ExternalID
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"aws","name":"payments-prod","accountId":"`+e.account+`","environment":"production","identity":"second"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("connect with another organization's identity: %d %s", w.Code, w.Body)
	}
	if n := auditCount(t, e.s, "cloud.identity.refused", cloudTarget("aws", e.account, "payments-prod")); n != 1 {
		t.Errorf("%d refusals recorded, want 1", n)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token",
		`{"provider":"aws","accountId":"`+e.account+`","identity":"second"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("prepare with another organization's identity: %d", w.Code)
	}
	// Its listing never names it.
	w = do(t, e.h, http.MethodGet, "/api/v1/cloud/accounts", "auditor-token", "")
	var v cloudAccountsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if len(v.Bases) != 1 || v.Bases[0].Identity != "main" {
		t.Errorf("bases listed: %+v", v.Bases)
	}
}

// Each account uses the identity it was connected with. Making another
// the default moves nothing; an account whose identity is gone is not
// read with another one.
func TestCloudAccountKeepsItsIdentity(t *testing.T) {
	e := cloudAPI(t)
	e.fake.Roles[e.roleARN()] = e.prepare(t).ExternalID
	c := e.connect(t, http.StatusCreated)
	if c.Identity != "main" {
		t.Fatalf("connected on %q, want main", c.Identity)
	}
	second := e.withSecond(true)
	got, _ := e.s.CloudAccount(context.Background(), c.ID)
	if got.Identity != "main" {
		t.Fatalf("a new default moved the account to %q", got.Identity)
	}
	// The template for an account already connected trusts its own identity,
	// not the new default.
	if p := e.prepare(t); p.Identity != "main" || !strings.Contains(p.Template, "603914770215:role/argus-control") {
		t.Errorf("template for a connected account trusts %s (%s)", p.Principal, p.Identity)
	}
	syncNow := func() {
		for !e.a.SyncCloudAccount(context.Background(), got) {
			time.Sleep(10 * time.Millisecond) // the read after connecting
		}
	}
	syncNow()
	e.fake.Calls = nil
	if syncNow(); len(e.fake.Calls) == 0 || len(second.Calls) != 0 {
		t.Errorf("sync went through the wrong identity: main %v, second %v", e.fake.Calls, second.Calls)
	}

	// Main is taken out of the config by hand.
	e.a.SetCloudIdentities("aws", []CloudIdentity{{ID: "second", Name: "Second", Default: true, Provider: second}})
	second.Calls = nil
	syncNow()
	if len(second.Calls) != 0 {
		t.Errorf("an account whose identity is gone was read with another: %v", second.Calls)
	}
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/accounts", "auditor-token", "")
	var v cloudAccountsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	i := slices.IndexFunc(v.Accounts, func(a CloudAccount) bool { return a.ID == c.ID })
	if i < 0 || !v.Accounts[i].IdentityMissing {
		t.Errorf("account not marked as having lost its identity: %+v", v.Accounts)
	}
}

// A move happens only once Argus gets into the account's roles with the
// new identity, and is recorded; until then the account stays where it was.
func TestCloudMoveOnlyOnceTheRolesTrustIt(t *testing.T) {
	e := cloudAPI(t)
	ext := e.prepare(t).ExternalID
	e.fake.Roles[e.roleARN()] = ext
	c := e.connect(t, http.StatusCreated)
	second := e.withSecond(false)

	// The template for the move trusts both while the stack updates.
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token",
		`{"provider":"aws","accountId":"`+e.account+`","identity":"second","keep":"main"}`)
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.ExternalID != ext || p.Kept == "" || !strings.Contains(p.Template, "argus-second") ||
		!strings.Contains(p.Template, "role/argus-control") {
		t.Fatalf("move template = %d %+v", w.Code, p)
	}

	path := "/api/v1/cloud/accounts/" + c.ID + "/identity"
	body := `{"identity":"second"}`
	w = do(t, e.h, http.MethodPost, path+"/check", "admin-token", body)
	var chk CloudCheck
	_ = json.Unmarshal(w.Body.Bytes(), &chk)
	if w.Code != http.StatusOK || chk.Ready || chk.Detail == "" {
		t.Fatalf("check before the stack is updated: %d %+v", w.Code, chk)
	}
	if w := do(t, e.h, http.MethodPut, path, "admin-token", body); w.Code != http.StatusConflict {
		t.Fatalf("move before the roles trust it: %d %s", w.Code, w.Body)
	}
	if got, _ := e.s.CloudAccount(context.Background(), c.ID); got.Identity != "main" {
		t.Fatalf("a refused move left the account on %q", got.Identity)
	}
	target := cloudTarget("aws", e.account, "payments-prod")
	if n := auditCount(t, e.s, "cloud.account.refused", target); n != 1 {
		t.Errorf("%d refused moves recorded, want 1", n)
	}
	if w := do(t, e.h, http.MethodPut, path, "operator-token", body); w.Code != http.StatusForbidden {
		t.Errorf("an operator moved an account: %d", w.Code)
	}

	second.Roles[e.roleARN()] = ext
	w = do(t, e.h, http.MethodPost, path+"/check", "admin-token", body)
	_ = json.Unmarshal(w.Body.Bytes(), &chk)
	if !chk.Ready {
		t.Fatalf("check once the role trusts it: %+v", chk)
	}
	w = do(t, e.h, http.MethodPut, path, "admin-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("move: %d %s", w.Code, w.Body)
	}
	var moved CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &moved)
	if moved.Identity != "second" {
		t.Errorf("moved = %+v", moved)
	}
	if n := auditCount(t, e.s, "cloud.account.identity_changed", target); n != 1 {
		t.Errorf("%d moves recorded, want 1", n)
	}
	if w := do(t, e.h, http.MethodPut, path, "admin-token", body); w.Code != http.StatusBadRequest {
		t.Errorf("moving to the identity it already uses: %d", w.Code)
	}
}

// An identity accounts still use cannot be removed, nor taken away from an
// organization whose accounts use it.
func TestCloudIdentityInUseCannotGo(t *testing.T) {
	e := cloudAPI(t)
	e.fake.Roles[e.roleARN()] = e.prepare(t).ExternalID
	e.connect(t, http.StatusCreated)
	ctx := PlatformContext(context.Background())
	main := AWSIdentitySettings{ID: "main", Name: "Main", Source: IdentityDefault, Organizations: []string{}}
	second := AWSIdentitySettings{ID: "second", Name: "Second", Source: IdentityDefault, Organizations: []string{}}
	before := &CloudSettings{AWS: AWSCloudSettings{Default: "main", Identities: []AWSIdentitySettings{main, second}}}

	msg, err := e.a.cloudIdentitiesInUse(ctx, before, &CloudSettings{AWS: AWSCloudSettings{Default: "second",
		Identities: []AWSIdentitySettings{second}}})
	if err != nil || !strings.Contains(msg, "cannot remove Main") || !strings.Contains(msg, "Change identity") {
		t.Errorf("removing Main in use: %q %v", msg, err)
	}
	narrowed := main
	narrowed.Organizations = []string{otherOrg}
	msg, _ = e.a.cloudIdentitiesInUse(ctx, before, &CloudSettings{AWS: AWSCloudSettings{Default: "main",
		Identities: []AWSIdentitySettings{narrowed, second}}})
	if !strings.Contains(msg, "take Main away") {
		t.Errorf("taking Main from the organization that uses it: %q", msg)
	}
	msg, _ = e.a.cloudIdentitiesInUse(ctx, before, &CloudSettings{AWS: AWSCloudSettings{Default: "main",
		Identities: []AWSIdentitySettings{main}}})
	if msg != "" {
		t.Errorf("removing an identity nothing uses was refused: %q", msg)
	}
}

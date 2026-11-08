package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 1: connecting an AWS account and reading it. Against the fake
provider, which admits a role only with the external id its trust policy
would require, and records who every call ran for.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Cloud
*/

/* ── Without a database ──────────────────────────────────────────────────── */

func TestCloudExternalIDIsPerOrganizationAndAccount(t *testing.T) {
	signer, err := auth.NewSigner("a-signing-secret-with-plenty-of-variety-0193")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Close)
	a := &API{signer: signer}
	ctx := context.Background()
	one, _ := a.cloudExternalID(ctx, "aws", "412788931204")
	again, _ := a.cloudExternalID(ctx, "aws", "412788931204")
	other, _ := a.cloudExternalID(ctx, "aws", "229011460871")
	elsewhere, _ := a.cloudExternalID(WithTenant(ctx, &Tenant{ID: "a1b2c3d4e5f6"}), "aws", "412788931204")
	if one != again || !strings.HasPrefix(one, "argus-") || len(one) != len("argus-")+32 {
		t.Fatalf("external id must be stable and argus-<32 hex>: %q %q", one, again)
	}
	if one == other || one == elsewhere {
		t.Fatal("an external id must differ per account and per organization")
	}
	if _, err := (&API{}).cloudExternalID(ctx, "aws", "412788931204"); err == nil {
		t.Fatal("without a signing secret there is no external id, and no guessing one")
	}
}

func TestCloudAccountInputRefusals(t *testing.T) {
	good := func() cloudAccountIn {
		return cloudAccountIn{Provider: "aws", Name: "payments-prod", AccountID: "412788931204",
			RoleARN: "arn:aws:iam::412788931204:role/ArgusRead"}
	}
	in := good()
	if err := in.validate(); err != nil || in.Environment != "production" {
		t.Fatalf("good input refused or environment not defaulted: %v %+v", err, in)
	}
	bad := map[string]func(*cloudAccountIn){
		"another provider":       func(i *cloudAccountIn) { i.Provider = "gcp" },
		"no name":                func(i *cloudAccountIn) { i.Name = " " },
		"a short account id":     func(i *cloudAccountIn) { i.AccountID = "4127889312" },
		"a user, not a role":     func(i *cloudAccountIn) { i.RoleARN = "arn:aws:iam::412788931204:user/x" },
		"a role in another acct": func(i *cloudAccountIn) { i.RoleARN = "arn:aws:iam::229011460871:role/ArgusRead" },
		"an unknown environment": func(i *cloudAccountIn) { i.Environment = "qa" },
	}
	for name, mutate := range bad {
		in := good()
		mutate(&in)
		if err := in.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMatchServersSaysHow(t *testing.T) {
	items := []CloudItem{
		{Resource: cloud.Resource{ID: "i-1", Name: "web-01", Addresses: []string{"10.0.0.5"}}},
		{Resource: cloud.Resource{ID: "i-2", Name: "pay-api-01", Addresses: []string{"10.0.0.9"}}},
		{Resource: cloud.Resource{ID: "i-3", Name: "i-3", Addresses: []string{"10.0.0.7"}}},
	}
	matchServers(items, []Asset{{ID: "a", Hostname: "web", Address: "10.0.0.5"}, {ID: "b", Hostname: "PAY-API-01", Address: "pay.internal"}})
	if items[0].Server == nil || items[0].Server.By != "address" || items[0].Server.ID != "a" {
		t.Errorf("address match: %+v", items[0].Server)
	}
	if items[1].Server == nil || items[1].Server.By != "name" {
		t.Errorf("name match: %+v", items[1].Server)
	}
	if items[2].Server != nil {
		t.Errorf("an unnamed instance must not match anything: %+v", items[2].Server)
	}
}

/* ── With a database ─────────────────────────────────────────────────────── */

type cloudEnv struct {
	a       *API
	s       *Store
	h       http.Handler
	admin   string
	fake    *cloud.Fake
	account string
}

func cloudAPI(t *testing.T) cloudEnv {
	t.Helper()
	a, s, h, admin := enrollAPI(t)
	signer, err := auth.NewSigner("a-signing-secret-with-plenty-of-variety-0193")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Close)
	a.signer = signer
	auditor, _ := newAccount(t, s, "auditor")
	operator, _ := newAccount(t, s, "operator")
	a.UserTokens["auditor-token"] = auditor
	a.UserTokens["operator-token"] = operator
	fake := &cloud.Fake{
		BaseIdentity: cloud.Base{Principal: "arn:aws:iam::603914770215:role/argus-control",
			ARN: "arn:aws:sts::603914770215:assumed-role/argus-control/i-0abc", Source: "the instance profile"},
		Roles:     map[string]string{},
		Regions:   []string{"ap-southeast-1", "ap-southeast-3"},
		Resources: map[string][]cloud.Resource{},
	}
	a.SetCloudProvider(fake)
	acct := fmt.Sprintf("%012d", time.Now().UnixNano()%1_000_000_000_000)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.pool.Exec(ctx, `DELETE FROM cloud_resources WHERE account_id IN (SELECT id FROM cloud_accounts WHERE account_id = $1)`, acct)
		_, _ = s.pool.Exec(ctx, `DELETE FROM cloud_accounts WHERE account_id = $1`, acct)
	})
	return cloudEnv{a: a, s: s, h: h, admin: admin, fake: fake, account: acct}
}

func (e cloudEnv) roleARN() string { return "arn:aws:iam::" + e.account + ":role/ArgusRead" }

func (e cloudEnv) prepare(t *testing.T) CloudPrepared {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token",
		`{"provider":"aws","accountId":"`+e.account+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", w.Code, w.Body)
	}
	var p CloudPrepared
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return p
}

func (e cloudEnv) connect(t *testing.T, want int) CloudAccount {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts", "admin-token",
		`{"provider":"aws","name":"payments-prod","accountId":"`+e.account+`","roleArn":"`+e.roleARN()+`","environment":"production"}`)
	if w.Code != want {
		t.Fatalf("connect: %d %s, want %d", w.Code, w.Body, want)
	}
	var c CloudAccount
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	return c
}

func (e cloudEnv) items(t *testing.T, kind string) []CloudItem {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/resources?kind="+kind, "auditor-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", w.Code, w.Body)
	}
	var out struct{ Items []CloudItem }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	var mine []CloudItem
	for _, it := range out.Items {
		if strings.HasPrefix(it.Resource.ID, e.account) {
			mine = append(mine, it)
		}
	}
	return mine
}

// The refusal first: a role that does not require this Argus's external
// id is not connected, and the attempt is in the audit chain.
func TestCloudConnectRefusesARoleWithoutOurExternalID(t *testing.T) {
	e := cloudAPI(t)
	e.fake.Roles[e.roleARN()] = "argus-someone-elses-id"
	e.connect(t, http.StatusUnprocessableEntity)
	target := cloudTarget("aws", e.account, "payments-prod")
	if n := auditCount(t, e.s, "cloud.account.refused", target); n != 1 {
		t.Fatalf("refused connects audited %d times, want 1", n)
	}
	if n := auditCount(t, e.s, "cloud.account.connected", target); n != 0 {
		t.Fatal("a refused role must not be recorded as connected")
	}
}

func TestCloudOnlyAdminsConnectAndOnlyFleetRolesSee(t *testing.T) {
	e := cloudAPI(t)
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "auditor-token",
		`{"provider":"aws","accountId":"`+e.account+`"}`); w.Code != http.StatusForbidden {
		t.Fatalf("an auditor prepared a connection: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodGet, "/api/v1/cloud/accounts", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator read cloud accounts: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodGet, "/api/v1/cloud/resources?kind=instance", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator read cloud resources: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodGet, "/api/v1/cloud/accounts", "auditor-token", ""); w.Code != http.StatusOK {
		t.Fatalf("an auditor could not read cloud accounts: %d", w.Code)
	}
}

func TestCloudNoIdentityOnTheHostIsSaidPlainly(t *testing.T) {
	e := cloudAPI(t)
	e.fake.BaseErr = fmt.Errorf("%w: no EC2 IMDS role found", cloud.ErrNoBase)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/accounts/prepare", "admin-token",
		`{"provider":"aws","accountId":"`+e.account+`"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "instance profile") {
		t.Fatalf("want 409 naming the fix, got %d %s", w.Code, w.Body)
	}
}

// Connected end to end: the template trusts this host and requires the
// external id, the role is assumed as the person connecting, the sync
// stores what it read, a region that fails keeps its last read, and
// disconnecting is audited and keeps the rows.
func TestCloudConnectSyncAndDisconnect(t *testing.T) {
	e := cloudAPI(t)
	p := e.prepare(t)
	if !strings.Contains(p.Template, p.ExternalID) || !strings.Contains(p.Template, "arn:aws:iam::603914770215:role/argus-control") {
		t.Fatalf("the template must trust this host and require the external id:\n%s", p.Template)
	}
	e.fake.Roles[e.roleARN()] = p.ExternalID
	inst := cloud.Resource{Kind: cloud.KindInstance, ID: e.account + "-i-1", Name: "pay-api-01", Region: "ap-southeast-3",
		State: "running", Size: "m6i.xlarge", Addresses: []string{"198.51.100.33"}}
	sg := cloud.Resource{Kind: cloud.KindFirewall, ID: e.account + "-sg-1", Name: "sg-web-public", Region: "ap-southeast-3",
		Rules: []cloud.Rule{{Inbound: true, Protocol: "tcp", FromPort: 22, ToPort: 22, Source: "0.0.0.0/0"}}}
	e.fake.Resources["ap-southeast-3"] = []cloud.Resource{inst, sg}
	e.fake.Resources["ap-southeast-1"] = []cloud.Resource{{Kind: cloud.KindInstance, ID: e.account + "-i-2", Name: "etl-1", Region: "ap-southeast-1"}}

	host := unique("pay-api") + ".example"
	if _, err := e.s.CreateAsset(context.Background(), AssetInput{Hostname: host, Address: "198.51.100.33",
		Principals: []string{"ops"}, CredentialRef: "injected"}); err != nil {
		t.Fatal(err)
	}

	c := e.connect(t, http.StatusCreated)
	if c.Access != "read" || c.IdentityARN != e.roleARN() || len(c.Regions) != 2 {
		t.Fatalf("connected account wrong: %+v", c)
	}
	calls := e.fake.CallLog()
	if len(calls) == 0 || calls[0] != "verify "+e.admin {
		t.Fatalf("the role must be assumed as the person connecting it, calls: %v", calls)
	}
	target := cloudTarget("aws", e.account, "payments-prod")
	if ev := lastAuditOn(t, e.s, target); ev.Action != "cloud.account.connected" || ev.ActorEmail != e.admin {
		t.Fatalf("connect audit: %+v", ev)
	}
	e.connect(t, http.StatusConflict)

	// The background sync started by connecting may still be running;
	// waiting for it keeps this deterministic.
	waitCloudSync(t, e, c)
	items := e.items(t, "instance")
	if len(items) != 2 {
		t.Fatalf("want 2 instances, got %+v", items)
	}
	var pay CloudItem
	for _, it := range items {
		if it.Resource.Name == "pay-api-01" {
			pay = it
		}
	}
	if pay.Server == nil || pay.Server.Hostname != host || pay.Server.By != "address" {
		t.Fatalf("instance not matched to its server by address: %+v", pay.Server)
	}
	if fw := e.items(t, "firewall"); len(fw) != 1 || !fw[0].Resource.Rules[0].Open() {
		t.Fatalf("firewall not stored with its rules: %+v", fw)
	}

	// One region fails: its last read stays, the other region's change lands.
	e.fake.RegionErr = map[string]error{"ap-southeast-1": errors.New("RequestLimitExceeded")}
	e.fake.Resources["ap-southeast-3"] = []cloud.Resource{sg}
	if !e.a.SyncCloudAccount(context.Background(), c) {
		t.Fatal("sync did not run")
	}
	items = e.items(t, "instance")
	if len(items) != 1 || items[0].Resource.Name != "etl-1" {
		t.Fatalf("a failed region must keep its inventory and a read region must drop what went: %+v", items)
	}
	got, _ := e.s.CloudAccount(context.Background(), c.ID)
	if got.SyncErrors["ap-southeast-1"] == "" || got.SyncedAt == nil {
		t.Fatalf("the failed region must be recorded: %+v", got)
	}
	for _, call := range e.fake.CallLog() {
		if strings.HasPrefix(call, "inventory ") && !strings.HasSuffix(call, " argus-sync") {
			t.Fatalf("background reads must run as argus-sync, not a person: %v", call)
		}
	}

	if w := do(t, e.h, http.MethodDelete, "/api/v1/cloud/accounts/"+c.ID, "auditor-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an auditor disconnected an account: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodDelete, "/api/v1/cloud/accounts/"+c.ID, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	if ev := lastAuditOn(t, e.s, target); ev.Action != "cloud.account.disconnected" || ev.ActorEmail != e.admin {
		t.Fatalf("disconnect audit: %+v", ev)
	}
	if len(e.items(t, "instance")) != 0 {
		t.Fatal("a disconnected account's resources must not be listed as present")
	}
	var rows int
	_ = e.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM cloud_resources r JOIN cloud_accounts a ON a.id = r.account_id
		WHERE a.account_id = $1`, e.account).Scan(&rows)
	if rows == 0 {
		t.Fatal("disconnecting must keep the rows, not delete them")
	}
}

func waitCloudSync(t *testing.T, e cloudEnv, c CloudAccount) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := e.s.CloudAccount(context.Background(), c.ID)
		if err == nil && got.SyncedAt != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the sync started by connecting never finished")
}

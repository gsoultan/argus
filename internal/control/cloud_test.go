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
	matchServers(items, []Asset{{ID: "a", Hostname: "web", Address: "10.0.0.5"}, {ID: "b", Hostname: "PAY-API-01", Address: "pay.internal"}}, nil, nil)
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

// A server added by its public name, or behind NAT, has an Address EC2
// does not list; its agent reports the private one, which EC2 does.
func TestMatchServersByWhatTheAgentReports(t *testing.T) {
	items := []CloudItem{
		{Resource: cloud.Resource{ID: "i-1", Name: "i-1", Addresses: []string{"172.31.5.10"}, PublicIP: "203.0.113.4"}},
		{Resource: cloud.Resource{ID: "i-2", Name: "i-2", Addresses: []string{"10.0.0.8"}}},
		{Resource: cloud.Resource{ID: "i-3", Name: "db-01", Addresses: []string{"10.9.9.9"}}},
	}
	assets := []Asset{
		{ID: "a", Hostname: "ip-172-31-5-10", Address: "app.example.com"},
		{ID: "b", Hostname: "one", Address: "x.example.com"}, {ID: "c", Hostname: "two", Address: "y.example.com"},
		{ID: "d", Hostname: "db-01.prod.example.com", Address: "z.example.com"},
	}
	reported := map[string][]AssetAddress{
		"a": {{Address: "172.31.5.10"}},
		"b": {{Address: "10.0.0.8"}}, "c": {{Address: "10.0.0.8"}},
	}
	matchServers(items, assets, reported, nil)
	if items[0].Server == nil || items[0].Server.ID != "a" || items[0].Server.By != "address" {
		t.Errorf("the agent's private address must match: %+v", items[0].Server)
	}
	if items[1].Server != nil {
		t.Errorf("an address two servers report must match neither: %+v", items[1].Server)
	}
	if items[2].Server == nil || items[2].Server.ID != "d" || items[2].Server.By != "name" {
		t.Errorf("a name must match the hostname's first label: %+v", items[2].Server)
	}
}

// What a server's agent read from the cloud's metadata service links it to
// its instance before any address or name, in the account and region it
// named, and only when no other server claims the same instance.
func TestMatchServersByReportedInstance(t *testing.T) {
	inst := func(id, acct, region, name, addr string) CloudItem {
		return CloudItem{Provider: "aws", ProviderAccount: acct,
			Resource: cloud.Resource{ID: id, Name: name, Region: region, Addresses: []string{addr}}}
	}
	items := []CloudItem{
		inst("i-0a41c27b9e3f1d27d", "412788931204", "ap-southeast-3", "web-01", "10.0.0.5"), // a reports it
		inst("i-0b93f12aa0c4d0e2e", "412788931204", "ap-southeast-3", "web-02", "10.0.0.6"), // b and c both claim it
		inst("i-07c2aa3f0d19e4c91", "999999999999", "ap-southeast-3", "pay-01", "10.0.0.7"), // d names another account
		inst("i-0e11d8a4c2f3b710c", "412788931204", "ap-southeast-3", "batch", "10.0.0.8"),  // e reports another instance
	}
	assets := []Asset{
		// a's own address is another instance's: the report must win.
		{ID: "a", Hostname: "web-01", Address: "10.0.0.8"},
		{ID: "b", Hostname: "b"}, {ID: "c", Hostname: "c"},
		{ID: "d", Hostname: "pay-01"},
		{ID: "e", Hostname: "batch", Address: "10.0.0.8"},
	}
	instances := map[string]HostCloud{
		"a": {Provider: "aws", Account: "412788931204", Region: "ap-southeast-3", ID: "i-0a41c27b9e3f1d27d"},
		"b": {Provider: "aws", Account: "412788931204", Region: "ap-southeast-3", ID: "i-0b93f12aa0c4d0e2e"},
		"c": {Provider: "aws", Account: "412788931204", Region: "ap-southeast-3", ID: "i-0b93f12aa0c4d0e2e"},
		"d": {Provider: "aws", Account: "412788931204", Region: "ap-southeast-3", ID: "i-07c2aa3f0d19e4c91"},
		"e": {Provider: "aws", Account: "412788931204", Region: "ap-southeast-3", ID: "i-00000000000000000"},
	}
	matchServers(items, assets, nil, instances)
	if s := items[0].Server; s == nil || s.ID != "a" || s.By != "agent" {
		t.Errorf("reported instance: %+v", s)
	}
	if s := items[1].Server; s != nil && s.By == "agent" {
		t.Errorf("an instance two servers claim must not link by report: %+v", s)
	}
	if s := items[2].Server; s != nil && s.ID == "d" {
		t.Errorf("a report naming another account must not link, nor fall back to the name: %+v", s)
	}
	if s := items[3].Server; s != nil {
		t.Errorf("a server whose agent named its instance must not match another by address: %+v", s)
	}
}

func TestHostCloudBounded(t *testing.T) {
	good := HostCloud{Provider: "aws", Account: "412788931204", Region: "ap-southeast-3", Zone: "ap-southeast-3a", ID: "i-0a41c27b9e3f1d27d", Type: "m6i.large"}
	az := HostCloud{Provider: "azure", Account: "6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90", Region: "eastus", Zone: "eastus-2",
		ID: "aaaaaaaa-0000-4000-8000-000000000001", Type: "Standard_B2s"}
	if az.bounded() == nil {
		t.Fatal("an Azure VM's identity must be kept")
	}
	az.ID = "web-1"
	if az.bounded() != nil {
		t.Fatal("an Azure VM is named by its vmId, nothing else")
	}
	if good.bounded() == nil {
		t.Fatal("a real identity was dropped")
	}
	for _, mut := range []func(*HostCloud){
		func(c *HostCloud) { c.ID = "i-0a41c27b9e3f1d27d'; DROP" },
		func(c *HostCloud) { c.Account = "4127" },
		func(c *HostCloud) { c.Provider = "azure" },
		func(c *HostCloud) { c.Region = "<b>" },
		func(c *HostCloud) { c.Type = "m6i.large\n" },
	} {
		c := good
		mut(&c)
		if c.bounded() != nil {
			t.Errorf("kept %+v", c)
		}
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

// The Cloud card's link comes from the instance the agent read from the
// metadata service, even when the server's address points elsewhere, and
// only fleet roles see it.
func TestAssetCloudFollowsTheAgentsReport(t *testing.T) {
	e := cloudAPI(t)
	e.fake.Roles[e.roleARN()] = e.prepare(t).ExternalID
	id := "i-0" + e.account
	e.fake.Resources["ap-southeast-3"] = []cloud.Resource{
		{Kind: cloud.KindInstance, ID: id, Name: "web-01", Region: "ap-southeast-3", State: "running", Size: "m6i.large",
			Addresses: []string{"10.20.1.14"}},
		{Kind: cloud.KindInstance, ID: "i-1" + e.account, Name: "decoy", Region: "ap-southeast-3", Addresses: []string{"203.0.113.77"}},
	}
	host := unique("web") + ".example"
	ctx := context.Background()
	asset, err := e.s.CreateAsset(ctx, AssetInput{Hostname: host, Address: "203.0.113.77",
		Principals: []string{"ops"}, CredentialRef: "injected"})
	if err != nil {
		t.Fatal(err)
	}
	sys := `{"cloud":{"provider":"aws","account":"` + e.account + `","region":"ap-southeast-3","zone":"ap-southeast-3a","id":"` + id + `","type":"m6i.large"}}`
	if _, err := e.s.pool.Exec(ctx, `INSERT INTO agents (hostname, version, last_seen_at, updated_at, system)
		VALUES ($1, 'test', now(), now(), $2::jsonb)`, host, sys); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.s.pool.Exec(context.Background(), `DELETE FROM agents WHERE hostname = $1`, host) })

	c := e.connect(t, http.StatusCreated)
	waitCloudSync(t, e, c)

	if w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/cloud", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator must not see the cloud card: %d", w.Code)
	}
	w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/cloud", "auditor-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("asset cloud: %d %s", w.Code, w.Body)
	}
	var out AssetCloud
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Reported == nil || out.Reported.ID != id {
		t.Fatalf("reported: %+v", out.Reported)
	}
	if out.Instance == nil || out.Instance.Resource.ID != id || out.Instance.Server == nil || out.Instance.Server.By != "agent" {
		t.Fatalf("the agent's report must win over the address: %+v", out.Instance)
	}
	if out.Instance.ProviderAccount != e.account || out.Instance.Provider != "aws" {
		t.Fatalf("provider account missing: %+v", out.Instance)
	}
	if w := do(t, e.h, http.MethodGet, "/api/v1/assets/00000000-0000-0000-0000-000000000000/cloud", "auditor-token", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing asset: %d", w.Code)
	}
}

// Termination protection is on unless a launch turns it off.
func TestLaunchSpecProtectsByDefault(t *testing.T) {
	var s cloudLaunchSpec
	if err := json.Unmarshal([]byte(`{"name":"web-03","size":"t3.small"}`), &s); err != nil || !s.Protect {
		t.Fatalf("left out: protect=%v err=%v", s.Protect, err)
	}
	if err := json.Unmarshal([]byte(`{"name":"web-03","protect":false}`), &s); err != nil || s.Protect {
		t.Fatalf("turned off: protect=%v err=%v", s.Protect, err)
	}
}

package gcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Against a stand-in for Google: the metadata server, IAM Credentials, IAM and
Compute Engine, answering the way the real ones do for a project set up by
SetupCommandFor.
*/

type fakeGoogle struct {
	t           *testing.T
	project     string
	description string
	mu          sync.Mutex
	agents      []string
	tokens      int
}

func (g *fakeGoogle) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/email", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("argus-control@ops-home.iam.gserviceaccount.com"))
	})
	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"base-token","expires_in":3599}`))
	})
	mux.HandleFunc("POST /v1/projects/-/serviceAccounts/{sa}", func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		if r.Header.Get("Authorization") != "Bearer base-token" {
			http.Error(w, `{"error":{"message":"bad base token"}}`, http.StatusUnauthorized)
			return
		}
		if r.PathValue("sa") != "argus-read@"+g.project+".iam.gserviceaccount.com:generateAccessToken" {
			http.Error(w, `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.getAccessToken' denied on resource (or it may not exist).","status":"PERMISSION_DENIED"}}`, http.StatusForbidden)
			return
		}
		g.mu.Lock()
		g.tokens++
		g.mu.Unlock()
		_, _ = w.Write([]byte(`{"accessToken":"read-token","expireTime":"2099-01-01T00:00:00Z"}`))
	})
	read := func(f http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			g.record(r)
			if r.Header.Get("Authorization") != "Bearer read-token" {
				http.Error(w, `{"error":{"message":"not argus-read"}}`, http.StatusForbidden)
				return
			}
			f(w, r)
		}
	}
	mux.HandleFunc("GET /v1/projects/{p}/serviceAccounts/{sa}", read(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"email": r.PathValue("sa"), "projectId": g.project, "description": g.description})
	}))
	c := "/compute/v1/projects/{p}"
	mux.HandleFunc("GET "+c+"/regions", read(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"name":"us-central1","status":"UP"},{"name":"europe-west1","status":"UP"},{"name":"old-1","status":"DOWN"}]}`))
	}))
	link := "https://www.googleapis.com/compute/v1/projects/" + g.project
	answers := map[string]string{
		"/aggregated/instances": `{"items":{"zones/us-central1-a":{"instances":[{"id":"101","name":"web-1","zone":"` + link + `/zones/us-central1-a",
			"status":"RUNNING","machineType":"` + link + `/zones/us-central1-a/machineTypes/e2-small","creationTimestamp":"2026-10-01T10:00:00.000-07:00",
			"tags":{"items":["web"]},"labels":{"team":"payments"},
			"networkInterfaces":[{"networkIP":"10.128.0.5","network":"` + link + `/global/networks/default","subnetwork":"` + link + `/regions/us-central1/subnetworks/default",
				"accessConfigs":[{"natIP":"34.1.2.3"}]}],
			"disks":[{"source":"` + link + `/zones/us-central1-a/disks/web-1","deviceName":"boot","boot":true}]}]},
			"zones/europe-west1-b":{"warning":{"code":"NO_RESULTS_ON_PAGE"}}}}`,
		"/aggregated/disks": `{"items":{"zones/us-central1-a":{"disks":[
			{"id":"201","name":"web-1","selfLink":"` + link + `/zones/us-central1-a/disks/web-1","zone":"` + link + `/zones/us-central1-a","sizeGb":"10","users":["` + link + `/zones/us-central1-a/instances/web-1"]},
			{"id":"202","name":"left-over","selfLink":"` + link + `/zones/us-central1-a/disks/left-over","zone":"` + link + `/zones/us-central1-a","sizeGb":"200"}]}}}`,
		"/aggregated/subnetworks": `{"items":{"regions/us-central1":{"subnetworks":[{"id":"301","name":"default","selfLink":"` + link + `/regions/us-central1/subnetworks/default",
			"region":"` + link + `/regions/us-central1","network":"` + link + `/global/networks/default","ipCidrRange":"10.128.0.0/20"}]}}}`,
		"/aggregated/addresses": `{"items":{"regions/europe-west1":{"addresses":[{"id":"401","name":"spare","region":"` + link + `/regions/europe-west1","address":"35.9.9.9","status":"RESERVED","addressType":"EXTERNAL"}]}}}`,
		"/global/addresses":     `{}`,
		"/global/networks":      `{"items":[{"id":"501","name":"default","selfLink":"` + link + `/global/networks/default","autoCreateSubnetworks":true}]}`,
		"/global/firewalls": `{"items":[
			{"id":"601","name":"allow-ssh","network":"` + link + `/global/networks/default","direction":"INGRESS","priority":1000,"sourceRanges":["0.0.0.0/0"],"allowed":[{"IPProtocol":"tcp","ports":["22","8000-8080"]}]},
			{"id":"602","name":"db-only","network":"` + link + `/global/networks/default","direction":"INGRESS","targetTags":["db"],"sourceRanges":["0.0.0.0/0"],"allowed":[{"IPProtocol":"tcp","ports":["5432"]}]},
			{"id":"603","name":"off","network":"` + link + `/global/networks/default","direction":"INGRESS","disabled":true,"allowed":[{"IPProtocol":"all"}]}]}`,
		"/global/snapshots": `{"items":[{"id":"701","name":"nightly","status":"READY","diskSizeGb":"10","sourceDisk":"` + link + `/zones/us-central1-a/disks/web-1"}]}`,
		"/global/images":    `{}`,
	}
	for path, body := range answers {
		mux.HandleFunc("GET "+c+path, read(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	}
	return mux
}

func (g *fakeGoogle) record(r *http.Request) {
	g.mu.Lock()
	g.agents = append(g.agents, r.Header.Get("User-Agent"))
	g.mu.Unlock()
}

func newFake(t *testing.T, ext string) (*Provider, *fakeGoogle, cloud.Account) {
	t.Helper()
	g := &fakeGoogle{t: t, project: "payments-prod-4182", description: Description(ext)}
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	p := New(Options{})
	p.ep = endpoints{metadata: srv.URL, iamCredentials: srv.URL, iam: srv.URL, compute: srv.URL}
	acct := cloud.Account{Provider: "gcp", AccountID: g.project, RoleARN: AccountEmail(g.project, ReadAccountName), ExternalID: ext}
	return p, g, acct
}

func TestVerifyActsAsArgusReadAndNamesThePerson(t *testing.T) {
	p, g, acct := newFake(t, "argus-0123abcd")
	base, err := p.Base(context.Background())
	if err != nil || base.Principal != "argus-control@ops-home.iam.gserviceaccount.com" {
		t.Fatalf("base: %+v %v", base, err)
	}
	id, err := p.Verify(context.Background(), acct, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(id.Regions, []string{"europe-west1", "us-central1", GlobalRegion}) || id.ARN != acct.RoleARN {
		t.Fatalf("verify: %+v", id)
	}
	if !slices.Contains(g.agents, "argus-control (for alice@example.com)") {
		t.Fatalf("Google's log must name the person: %v", g.agents)
	}
	if _, err := p.Verify(context.Background(), acct, "alice@example.com"); err != nil || g.tokens != 1 {
		t.Fatalf("a fresh token is reused, not minted again: %d %v", g.tokens, err)
	}
}

// The external id is what keeps one organization out of a project that
// trusted the same identity for another, so a near miss is a refusal.
func TestVerifyRefusesAnAccountMadeForAnotherArgus(t *testing.T) {
	for name, desc := range map[string]string{
		"another id":   Description("argus-ffff"),
		"a longer one": Description("argus-0123abcd99"),
		"none":         "made by hand",
	} {
		t.Run(name, func(t *testing.T) {
			p, g, acct := newFake(t, "argus-0123abcd")
			g.description = desc
			if _, err := p.Verify(context.Background(), acct, "alice@example.com"); err == nil || !strings.Contains(err.Error(), "not made for this Argus") {
				t.Fatalf("must refuse: %v", err)
			}
			p.mu.Lock()
			n := len(p.sessions)
			p.mu.Unlock()
			if n != 0 {
				t.Fatal("a refused account's token must not be kept")
			}
		})
	}
}

func TestVerifyRefusesAnotherProjectsAccount(t *testing.T) {
	p, _, acct := newFake(t, "argus-0123abcd")
	acct.RoleARN = AccountEmail("someone-else-1", ReadAccountName)
	if _, err := p.Verify(context.Background(), acct, "alice@example.com"); err == nil {
		t.Fatal("a service account in another project must be refused before Google is asked")
	}
}

func TestInventoryCutsTheProjectIntoRegions(t *testing.T) {
	p, _, acct := newFake(t, "argus-0123abcd")
	ctx := context.Background()
	us, err := p.Inventory(ctx, acct, "us-central1", cloud.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]cloud.Resource{}
	for _, r := range us.Resources {
		byName[string(r.Kind)+"/"+r.Name] = r
	}
	vm := byName["instance/web-1"]
	if vm.ID != "101" || vm.State != "running" || vm.Size != "e2-small" || vm.PublicIP != "34.1.2.3" || vm.Zone != "us-central1-a" ||
		vm.Network != "501" || !slices.Equal(vm.Addresses, []string{"10.128.0.5"}) {
		t.Fatalf("instance: %+v", vm)
	}
	has := func(r cloud.Resource, k cloud.Kind, id string) bool {
		return slices.ContainsFunc(r.Links, func(l cloud.Ref) bool { return l.Kind == k && l.ID == id })
	}
	if !has(vm, cloud.KindVolume, "201") || !has(vm, cloud.KindSubnet, "301") || !has(vm, cloud.KindFirewall, "601") ||
		!has(vm, cloud.KindFirewall, "603") || has(vm, cloud.KindFirewall, "602") {
		t.Fatalf("links: %+v", vm.Links)
	}
	if byName["volume/web-1"].State != "in-use" || byName["volume/left-over"].State != "available" || byName["volume/left-over"].Size != "200 GiB" {
		t.Fatal("a disk nothing uses is available, as the console counts it")
	}
	if _, ok := byName["firewall/allow-ssh"]; ok {
		t.Fatal("firewalls are global, not in a region")
	}

	eu, err := p.Inventory(ctx, acct, "europe-west1", cloud.Kinds)
	if err != nil || len(eu.Resources) != 1 || eu.Resources[0].State != "unattached" || eu.Resources[0].PublicIP != "35.9.9.9" {
		t.Fatalf("an external address nothing uses is unattached: %+v %v", eu.Resources, err)
	}

	global, err := p.Inventory(ctx, acct, GlobalRegion, cloud.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	var ssh, off cloud.Resource
	for _, r := range global.Resources {
		switch r.Name {
		case "allow-ssh":
			ssh = r
		case "off":
			off = r
		}
	}
	if len(ssh.Rules) != 2 || !ssh.Rules[0].Open() || !ssh.Rules[0].Covers(22) || ssh.Rules[1].FromPort != 8000 || ssh.Rules[1].ToPort != 8080 {
		t.Fatalf("an open SSH rule must read as open: %+v", ssh.Rules)
	}
	if off.State != "disabled" || len(off.Rules) != 0 {
		t.Fatalf("a disabled rule lets nothing in: %+v", off)
	}
	if slices.Contains(global.Kinds, cloud.KindInstance) || !slices.Contains(us.Kinds, cloud.KindInstance) {
		t.Fatal("each snapshot must say only the kinds it holds, or the other region's are marked gone")
	}
}

func TestBaseFromAKeyFile(t *testing.T) {
	var gotAssertion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotAssertion = r.PostForm.Get("assertion")
		_, _ = w.Write([]byte(`{"access_token":"key-token","expires_in":3600}`))
	}))
	defer srv.Close()
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(rk)
	key, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "argus@ops-home.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": srv.URL})
	path := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(path, key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{CredentialsFile: path}).Base(context.Background()); err == nil {
		t.Fatal("a key anyone on the host can read must be refused")
	}
	_ = os.Chmod(path, 0o600)
	b, err := New(Options{CredentialsFile: path}).Base(context.Background())
	if err != nil || b.Principal != "argus@ops-home.iam.gserviceaccount.com" || strings.Count(gotAssertion, ".") != 2 {
		t.Fatalf("base: %+v %v %q", b, err, gotAssertion)
	}
	if _, err := ParseKey([]byte(`{"type":"authorized_user"}`)); err == nil {
		t.Fatal("a person's credentials are not argus-control's identity")
	}
}

func TestSetupCommandCarriesTheExternalID(t *testing.T) {
	cmd := SetupCommandFor("payments-prod-4182", []string{"argus@ops-home.iam.gserviceaccount.com"}, "argus-0123")
	for _, want := range []string{"P=payments-prod-4182", Description("argus-0123"), "roles/compute.viewer", "roles/iam.serviceAccountViewer",
		"--member=serviceAccount:argus@ops-home.iam.gserviceaccount.com --role=roles/iam.serviceAccountTokenCreator"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("missing %q in\n%s", want, cmd)
		}
	}
	if !hasMarker(Description("argus-0123"), "argus-0123") {
		t.Fatal("the description must carry the marker Argus checks for")
	}
}

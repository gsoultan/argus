package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
A stand-in for Google that answers argus-manage the way a project set up
with changes allowed does, and records every change it is asked for.
*/

type fakeManage struct {
	project string
	desc    string
	mu      sync.Mutex
	posts   []string
	bodies  []string
	agents  []string
	queries []string
}

func (g *fakeManage) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/email", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("argus-control@ops-home.iam.gserviceaccount.com"))
	})
	mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"base-token","expires_in":3599}`))
	})
	mux.HandleFunc("POST /v1/projects/-/serviceAccounts/{sa}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sa") != AccountEmail(g.project, ManageAccountName)+":generateAccessToken" {
			http.Error(w, `{"error":{"message":"denied"}}`, http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"accessToken":"manage-token","expireTime":"2099-01-01T00:00:00Z"}`))
	})
	auth := func(f http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer manage-token" {
				http.Error(w, `{"error":{"message":"not argus-manage"}}`, http.StatusForbidden)
				return
			}
			f(w, r)
		}
	}
	mux.HandleFunc("GET /v1/projects/{p}/serviceAccounts/{sa}", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"email": r.PathValue("sa"), "projectId": g.project, "description": g.desc})
	}))
	link := "https://www.googleapis.com/compute/v1/projects/" + g.project
	mux.HandleFunc("GET /compute/v1/projects/{p}/aggregated/{kind}", auth(func(w http.ResponseWriter, r *http.Request) {
		f := r.URL.Query().Get("filter")
		switch {
		case r.PathValue("kind") == "instances" && f == "id = 101":
			_, _ = w.Write([]byte(`{"items":{"zones/us-central1-a":{"instances":[{"id":"101","name":"web-1","zone":"` + link + `/zones/us-central1-a"}]},
				"zones/europe-west1-b":{"warning":{"code":"NO_RESULTS_ON_PAGE"}}}}`))
		case r.PathValue("kind") == "disks" && f == "id = 201":
			_, _ = w.Write([]byte(`{"items":{"zones/us-central1-a":{"disks":[{"id":"201","name":"web-1","zone":"` + link + `/zones/us-central1-a"}]}}}`))
		case r.PathValue("kind") == "disks" && f == "id = 202":
			_, _ = w.Write([]byte(`{"items":{"regions/us-central1":{"disks":[{"id":"202","name":"shared","region":"` + link + `/regions/us-central1"}]}}}`))
		case r.PathValue("kind") == "addresses" && (f == "id = 501" || f == "id = 502"):
			id := strings.TrimPrefix(f, "id = ")
			_, _ = w.Write([]byte(`{"items":{"regions/us-central1":{"addresses":[{"id":"` + id + `","name":"static-` + id + `","region":"` + link + `/regions/us-central1"}]}}}`))
		case r.PathValue("kind") == "addresses" && f == "id = 503":
			_, _ = w.Write([]byte(`{"items":{"global":{"addresses":[{"id":"503","name":"lb-ip"}]}}}`))
		default:
			_, _ = w.Write([]byte(`{"items":{}}`))
		}
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/zones/{z}/instances/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Query().Get("fields"), "networkInterfaces") {
			ip := "34.1.2.3"
			if r.PathValue("id") == "web-1" {
				ip = "35.2.2.2"
			}
			_, _ = w.Write([]byte(`{"networkInterfaces":[{"name":"nic0","accessConfigs":[{"name":"External NAT","natIP":"` + ip + `"}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"web-1","status":"RUNNING","machineType":"` + link + `/zones/us-central1-a/machineTypes/e2-small",
			"labels":{"team":"payments","environment":"production"},"labelFingerprint":"fp-101",
			"metadata":{"items":[{"key":"startup-script","value":"echo hi"}]},
			"disks":[{"source":"` + link + `/zones/us-central1-a/disks/web-1-boot","deviceName":"persistent-disk-0","boot":true,"autoDelete":true,"diskSizeGb":"10"},
			{"source":"` + link + `/zones/us-central1-a/disks/web-1","deviceName":"data","autoDelete":false,"diskSizeGb":"200"}],
			"networkInterfaces":[{"name":"nic0","accessConfigs":[{"name":"External NAT","natIP":"34.1.2.3"}]}]}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/zones/{z}/disks/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fields") == "id,sizeGb" {
			ids := map[string]string{"web-1-boot": "200", "web-1": "201"}
			_, _ = w.Write([]byte(`{"id":"` + ids[r.PathValue("id")] + `","sizeGb":"` + map[string]string{"web-1-boot": "10", "web-1": "200"}[r.PathValue("id")] + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"users":["` + link + `/zones/us-central1-a/instances/web-1"]}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/global/snapshots/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selfLink":"` + link + `/global/snapshots/nightly-1"}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/global/images/family/{f}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("p") == "windows-cloud" {
			http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"name":"` + r.PathValue("f") + `-v20261101"}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/global/images", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"name":"web-golden","status":"READY","diskSizeGb":"30"},
			{"name":"old-golden","status":"READY","diskSizeGb":"10","deprecated":{"state":"DEPRECATED"}},
			{"name":"baking","status":"PENDING","diskSizeGb":"10"},
			{"name":"win-golden","status":"READY","diskSizeGb":"50","guestOsFeatures":[{"type":"WINDOWS"}]}]}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/regions/{rg}", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"zones":["` + link + `/zones/us-central1-c","` + link + `/zones/us-central1-a"]}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/regions/{rg}/subnetworks/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"selfLink":"` + link + `/regions/us-central1/subnetworks/default","network":"` + link + `/global/networks/default"}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/global/firewalls/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "901":
			_, _ = w.Write([]byte(`{"name":"allow-http","network":"` + link + `/global/networks/default","targetTags":["http-server"]}`))
		case "902":
			_, _ = w.Write([]byte(`{"name":"allow-ssh","network":"` + link + `/global/networks/default","targetTags":["ssh","http-server"]}`))
		default:
			_, _ = w.Write([]byte(`{"name":"other-net","network":"` + link + `/global/networks/other"}`))
		}
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/regions/{rg}/addresses", auth(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("filter"), "35.2.2.2") {
			_, _ = w.Write([]byte(`{"items":[{"address":"35.2.2.2"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/regions/{rg}/addresses/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "501", "static-new":
			_, _ = w.Write([]byte(`{"address":"35.1.1.1"}`))
		case "502":
			_, _ = w.Write([]byte(`{"address":"35.2.2.2","users":["` + link + `/zones/us-central1-a/instances/web-1"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/regions/{rg}/operations/{op}", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"` + r.PathValue("op") + `","status":"DONE"}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/zones/{z}/operations/{op}", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"` + r.PathValue("op") + `","status":"DONE"}`))
	}))
	mux.HandleFunc("DELETE /compute/v1/", auth(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.posts = append(g.posts, "DELETE "+r.URL.Path)
		g.bodies = append(g.bodies, "")
		g.agents = append(g.agents, r.Header.Get("User-Agent"))
		g.queries = append(g.queries, r.URL.RawQuery)
		g.mu.Unlock()
		_, _ = w.Write([]byte(`{"name":"operation-d","status":"RUNNING"}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/regions/{rg}/disks/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"labelFingerprint":"fp-202"}`))
	}))
	mux.HandleFunc("GET /compute/v1/projects/{p}/zones/{z}/machineTypes", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"items":[{"name":"n2-standard-4"},{"name":"n1-highcpu-2","deprecated":{"state":"DEPRECATED"}}],"nextPageToken":"p2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"name":"e2-medium"}]}`))
	}))
	mux.HandleFunc("POST /compute/v1/", auth(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.posts = append(g.posts, r.URL.Path)
		g.bodies = append(g.bodies, string(b))
		g.agents = append(g.agents, r.Header.Get("User-Agent"))
		g.queries = append(g.queries, r.URL.RawQuery)
		g.mu.Unlock()
		if strings.Contains(r.URL.Path, "/zones/us-central1-a/instances/101/reset") {
			_, _ = w.Write([]byte(`{"name":"operation-9","status":"DONE","error":{"errors":[{"message":"The resource is not ready"}]}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/zones/us-central1-a/disks") {
			_, _ = w.Write([]byte(`{"name":"operation-c","status":"RUNNING","targetId":"301",
				"selfLink":"` + link + `/zones/us-central1-a/operations/operation-c"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/global/images") {
			_, _ = w.Write([]byte(`{"name":"operation-i","status":"RUNNING","targetId":"9001"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/zones/us-central1-a/instances") {
			_, _ = w.Write([]byte(`{"name":"operation-l","status":"RUNNING","targetId":"7001"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/regions/us-central1/addresses") {
			_, _ = w.Write([]byte(`{"name":"operation-a","status":"RUNNING","targetId":"601",
				"selfLink":"` + link + `/regions/us-central1/operations/operation-a"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/deleteAccessConfig") || strings.HasSuffix(r.URL.Path, "/disks/web-1-boot/createSnapshot") {
			_, _ = w.Write([]byte(`{"name":"operation-x","status":"DONE"}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"operation-1","status":"RUNNING"}`))
	}))
	return mux
}

func newManageFake(t *testing.T, ext, desc string) (*Provider, *fakeManage, cloud.Account) {
	t.Helper()
	g := &fakeManage{project: "payments-prod-4182", desc: desc}
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	p := New(Options{})
	p.ep = endpoints{metadata: srv.URL, iamCredentials: srv.URL, iam: srv.URL, compute: srv.URL}
	return p, g, cloud.Account{Provider: "gcp", AccountID: g.project, RoleARN: AccountEmail(g.project, ManageAccountName), ExternalID: ext}
}

func TestPowerFindsTheZoneAndNamesThePerson(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	res, err := p.Power(context.Background(), acct, "alice@example.com", "us-central1", "101", cloud.ActionStop)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequestID != "operation-1" || res.ResourceID != "101" {
		t.Fatalf("result = %+v", res)
	}
	if len(g.posts) != 1 || g.posts[0] != "/compute/v1/projects/payments-prod-4182/zones/us-central1-a/instances/101/stop" {
		t.Fatalf("posts = %v", g.posts)
	}
	if g.agents[0] != "argus-control (for alice@example.com)" {
		t.Errorf("user agent = %q", g.agents[0])
	}
}

func TestAnOperationGoogleRefusesIsAnError(t *testing.T) {
	p, _, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	_, err := p.Power(context.Background(), acct, "alice@example.com", "us-central1", "101", cloud.ActionReboot)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnInstanceInAnotherRegionIsRefused(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	if _, err := p.Power(context.Background(), acct, "alice@example.com", "europe-west1", "101", cloud.ActionStart); err == nil {
		t.Fatal("the console named another region than the instance's")
	}
	if _, err := p.Power(context.Background(), acct, "alice@example.com", "us-central1", "../101", cloud.ActionStart); err == nil {
		t.Fatal("an id is digits only")
	}
	if len(g.posts) != 0 {
		t.Fatalf("nothing should be changed: %v", g.posts)
	}
}

func TestADiskGrowsByResizeZonalAndRegional(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	if _, err := p.ModifyVolume(context.Background(), acct, "alice@example.com", "us-central1", "201", cloud.VolumeChange{SizeGB: 50}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ModifyVolume(context.Background(), acct, "alice@example.com", "us-central1", "202", cloud.VolumeChange{SizeGB: 500}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/compute/v1/projects/payments-prod-4182/zones/us-central1-a/disks/201/resize",
		"/compute/v1/projects/payments-prod-4182/regions/us-central1/disks/202/resize"}
	if len(g.posts) != 2 || g.posts[0] != want[0] || g.posts[1] != want[1] {
		t.Fatalf("posts = %v", g.posts)
	}
	if g.bodies[0] != `{"sizeGb":"50"}` {
		t.Errorf("body = %s", g.bodies[0])
	}
	if _, err := p.ModifyVolume(context.Background(), acct, "alice@example.com", "us-central1", "201", cloud.VolumeChange{SizeGB: 60, Type: "pd-ssd"}); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("a type change is not made through a resize: %v", err)
	}
}

func TestArgusManageMustCarryTheExternalID(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("someone-else"))
	if _, err := p.Power(context.Background(), acct, "alice@example.com", "us-central1", "101", cloud.ActionStop); err == nil {
		t.Fatal("argus-manage made for another Argus must not be used")
	}
	if len(g.posts) != 0 {
		t.Fatalf("nothing should be changed: %v", g.posts)
	}
}

func TestOnlyBuiltChangesAreSupported(t *testing.T) {
	p := New(Options{})
	for _, a := range []cloud.Action{cloud.ActionStart, cloud.ActionStop, cloud.ActionReboot, cloud.ActionVolumeGrow,
		cloud.ActionResize, cloud.ActionTags, cloud.ActionProtect, cloud.ActionUnprotect, cloud.ActionAddressAllocate, cloud.ActionAddressRelease,
		cloud.ActionLaunch, cloud.ActionRemove, cloud.ActionImageCreate, cloud.ActionImageDelete} {
		if !cloud.Supports(p, a) {
			t.Errorf("%s should be supported", a)
		}
	}
	for _, a := range []cloud.Action{cloud.ActionHibernate, cloud.ActionFirewallCreate} {
		if cloud.Supports(p, a) {
			t.Errorf("%s is not built for Google Cloud yet", a)
		}
	}
	if _, err := p.Power(context.Background(), cloud.Account{}, "alice@example.com", "us-central1", "101", cloud.ActionHibernate); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("hibernate: %v", err)
	}
}

func TestSetupCommandMakesArgusManageOnlyWhenAsked(t *testing.T) {
	pr := []string{"argus@ops-home.iam.gserviceaccount.com"}
	read := SetupCommandFor("payments-prod-4182", pr, "argus-0123", false)
	if strings.Contains(read, ManageAccountName) {
		t.Fatal("a read-only setup must not make argus-manage")
	}
	cmd := SetupCommandFor("payments-prod-4182", pr, "argus-0123", true)
	for _, want := range []string{" M=argus-manage@payments-prod-4182.iam.gserviceaccount.com", ManageDescription("argus-0123"),
		"--member=serviceAccount:$M --role=roles/compute.instanceAdmin.v1",
		"add-iam-policy-binding $M --project=$P --member=serviceAccount:argus@ops-home.iam.gserviceaccount.com --role=roles/iam.serviceAccountTokenCreator"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("missing %q in\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "serviceAccountUser") || strings.Contains(cmd, "roles/owner") || strings.Contains(cmd, "roles/editor") {
		t.Fatal("argus-manage gets nothing beyond Compute Engine")
	}
	if !hasMarker(ManageDescription("argus-0123"), "argus-0123") {
		t.Fatal("the description must carry the marker Argus checks for")
	}
	// Every step is chained, so one that fails stops the rest.
	lines := strings.Split(strings.TrimSpace(cmd), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "#") && i > 2 {
			t.Fatalf("a comment inside the chain breaks it, line %d: %s", i, l)
		}
	}
}

func TestResizeSetsTheMachineTypeInTheZone(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	if _, err := p.Resize(context.Background(), acct, "alice@example.com", "us-central1", "101", "n2-standard-4"); err != nil {
		t.Fatal(err)
	}
	if len(g.posts) != 1 || g.posts[0] != "/compute/v1/projects/payments-prod-4182/zones/us-central1-a/instances/101/setMachineType" {
		t.Fatalf("posts = %v", g.posts)
	}
	if g.bodies[0] != `{"machineType":"zones/us-central1-a/machineTypes/n2-standard-4"}` {
		t.Errorf("body = %s", g.bodies[0])
	}
	for _, bad := range []string{"t3.medium", "N2-standard-4", "../x", "e2"} {
		if _, err := p.Resize(context.Background(), acct, "alice@example.com", "us-central1", "101", bad); err == nil {
			t.Errorf("%q is not a machine type", bad)
		}
	}
	if len(g.posts) != 1 {
		t.Fatalf("nothing more should be changed: %v", g.posts)
	}
}

func TestSizesAreTheZonesCurrentMachineTypes(t *testing.T) {
	p, _, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	got, err := p.Sizes(context.Background(), acct, "alice@example.com", "us-central1", "101")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "e2-medium,n2-standard-4" {
		t.Fatalf("sizes = %v (every page, sorted, no deprecated ones)", got)
	}
}

func TestLabelsAreMergedAndWrittenWithTheFingerprint(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	if _, err := p.SetTagsOf(context.Background(), acct, "alice@example.com", "us-central1", cloud.KindInstance, "101",
		map[string]string{"owner": "alice"}, []string{"team"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetTagsOf(context.Background(), acct, "alice@example.com", "us-central1", cloud.KindVolume, "202",
		map[string]string{"backup": "daily"}, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"/compute/v1/projects/payments-prod-4182/zones/us-central1-a/instances/101/setLabels",
		"/compute/v1/projects/payments-prod-4182/regions/us-central1/disks/202/setLabels"}
	if len(g.posts) != 2 || g.posts[0] != want[0] || g.posts[1] != want[1] {
		t.Fatalf("posts = %v", g.posts)
	}
	if g.bodies[0] != `{"labelFingerprint":"fp-101","labels":{"environment":"production","owner":"alice"}}` {
		t.Errorf("instance body = %s", g.bodies[0])
	}
	if g.bodies[1] != `{"labelFingerprint":"fp-202","labels":{"backup":"daily"}}` {
		t.Errorf("disk body = %s", g.bodies[1])
	}
	if _, err := p.SetTagsOf(context.Background(), acct, "alice@example.com", "us-central1", cloud.KindInstance, "101",
		map[string]string{"Owner": "alice"}, nil); err == nil {
		t.Fatal("Google keeps labels lowercase")
	}
	if _, err := p.SetTagsOf(context.Background(), acct, "alice@example.com", "us-central1", cloud.KindSnapshot, "101",
		map[string]string{"owner": "alice"}, nil); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("snapshots are not labelled yet: %v", err)
	}
	if len(g.posts) != 2 {
		t.Fatalf("nothing more should be changed: %v", g.posts)
	}
}

func TestLabelProblems(t *testing.T) {
	for k, v := range map[string]string{"env": "prod", "argusschedule": "office-hours", "a_b-1": ""} {
		if why := LabelProblem(k, v); why != "" {
			t.Errorf("%s=%s: %s", k, v, why)
		}
	}
	for _, kv := range [][2]string{{"Env", "prod"}, {"env", "Prod"}, {"1env", "x"}, {"goog-managed", "x"}, {"env", strings.Repeat("x", 64)}} {
		if LabelProblem(kv[0], kv[1]) == "" {
			t.Errorf("%s=%s should be refused", kv[0], kv[1])
		}
	}
}

func TestProtectSetsDeletionProtection(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	if _, err := p.Protect(context.Background(), acct, "alice@example.com", "us-central1", "101", true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Protect(context.Background(), acct, "alice@example.com", "us-central1", "101", false); err != nil {
		t.Fatal(err)
	}
	if len(g.posts) != 2 || g.posts[0] != "/compute/v1/projects/payments-prod-4182/zones/us-central1-a/instances/101/setDeletionProtection" {
		t.Fatalf("posts = %v", g.posts)
	}
	if g.queries[0] != "deletionProtection=true" || g.queries[1] != "deletionProtection=false" {
		t.Fatalf("queries = %v", g.queries)
	}
}

func TestADiskIsMadeFromASnapshotAndAttachedOnceItExists(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	res, err := p.CreateVolume(context.Background(), acct, "alice@example.com", "us-central1", cloud.VolumeSpec{
		Name: "web-1-logs", Zone: "us-central1-a", SizeGB: 50, Type: "pd-ssd", Snapshot: "401", AttachTo: "101", Device: "web-1-logs",
		Tags: map[string]string{"argus:change": "ABC-1", "team": "payments"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ResourceID != "301" || !strings.Contains(res.Detail, "attached it to web-1 as web-1-logs") {
		t.Fatalf("result = %+v", res)
	}
	want := []string{"/compute/v1/projects/payments-prod-4182/zones/us-central1-a/disks",
		"/compute/v1/projects/payments-prod-4182/zones/us-central1-a/instances/101/attachDisk"}
	if len(g.posts) != 2 || g.posts[0] != want[0] || g.posts[1] != want[1] {
		t.Fatalf("posts = %v", g.posts)
	}
	for _, part := range []string{`"name":"web-1-logs"`, `"sizeGb":"50"`, `"type":"zones/us-central1-a/diskTypes/pd-ssd"`,
		`"sourceSnapshot":"/compute/v1/projects/payments-prod-4182/global/snapshots/nightly-1"`, `"argus-change":"abc-1"`, `"team":"payments"`} {
		if !strings.Contains(g.bodies[0], part) {
			t.Errorf("missing %s in %s", part, g.bodies[0])
		}
	}
	if g.bodies[1] != `{"deviceName":"web-1-logs","mode":"READ_WRITE","source":"projects/payments-prod-4182/zones/us-central1-a/disks/web-1-logs"}` {
		t.Errorf("attach body = %s", g.bodies[1])
	}
	for _, bad := range []cloud.VolumeSpec{
		{Name: "Logs", Zone: "us-central1-a", SizeGB: 50, Type: "pd-ssd"},
		{Name: "logs", Zone: "us-central1-a", SizeGB: 50, Type: "gp3"},
		{Name: "logs", Zone: "europe-west1-b", SizeGB: 50, Type: "pd-ssd"},
	} {
		if _, err := p.CreateVolume(context.Background(), acct, "alice@example.com", "us-central1", bad); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
	if len(g.posts) != 2 {
		t.Fatalf("nothing more should be made: %v", g.posts)
	}
}

func TestADiskIsDetachedByTheDeviceItHasOnItsInstance(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	if _, err := p.DetachVolume(context.Background(), acct, "alice@example.com", "us-central1", "201"); err != nil {
		t.Fatal(err)
	}
	if len(g.posts) != 1 || g.posts[0] != "/compute/v1/projects/payments-prod-4182/zones/us-central1-a/instances/web-1/detachDisk" || g.queries[0] != "deviceName=data" {
		t.Fatalf("posts = %v %v", g.posts, g.queries)
	}
}

func TestDisksAndSnapshotsAreDeletedAndSnapshotsTaken(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	if _, err := p.DeleteVolume(context.Background(), acct, "alice@example.com", "us-central1", "202", false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeleteVolume(context.Background(), acct, "alice@example.com", "us-central1", "201", true); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("a final snapshot is its own change in Google Cloud: %v", err)
	}
	if _, err := p.DeleteSnapshot(context.Background(), acct, "alice@example.com", GlobalRegion, "401"); err != nil {
		t.Fatal(err)
	}
	res, err := p.CreateSnapshot(context.Background(), acct, "alice@example.com", "us-central1", "201", "web-1-20261117-0400")
	if err != nil || res.ResourceID != "web-1-20261117-0400" {
		t.Fatalf("snapshot: %+v %v", res, err)
	}
	if _, err := p.CreateSnapshot(context.Background(), acct, "alice@example.com", "us-central1", "201", "web-1 2026-11-17"); err == nil {
		t.Fatal("a snapshot name is a Compute Engine name")
	}
	want := []string{"DELETE /compute/v1/projects/payments-prod-4182/regions/us-central1/disks/202",
		"DELETE /compute/v1/projects/payments-prod-4182/global/snapshots/401",
		"/compute/v1/projects/payments-prod-4182/zones/us-central1-a/disks/201/createSnapshot"}
	if strings.Join(g.posts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %v", g.posts)
	}
	if g.bodies[2] != `{"name":"web-1-20261117-0400"}` {
		t.Errorf("snapshot body = %s", g.bodies[2])
	}
}

func TestAddressesAreReservedAssignedUnassignedAndReleased(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	ctx := context.Background()
	res, err := p.AllocateAddress(ctx, acct, "alice@example.com", "us-central1", "static-new")
	if err != nil || res.ResourceID != "601" || res.Detail != "Address 35.1.1.1" {
		t.Fatalf("reserve: %+v %v", res, err)
	}
	if _, err := p.AllocateAddress(ctx, acct, "alice@example.com", "us-central1", "Static IP"); err == nil {
		t.Fatal("an address name is a Compute Engine name")
	}
	// Assigning replaces the ephemeral address the VM had.
	res, err = p.AssociateAddress(ctx, acct, "alice@example.com", "us-central1", "501", "101")
	if err != nil || res.Detail != "Replaced 34.1.2.3" {
		t.Fatalf("assign: %+v %v", res, err)
	}
	if _, err := p.AssociateAddress(ctx, acct, "alice@example.com", "us-central1", "502", "101"); err == nil || !strings.Contains(err.Error(), "in use by web-1") {
		t.Fatalf("an address in use is not taken from its VM: %v", err)
	}
	if _, err := p.AssociateAddress(ctx, acct, "alice@example.com", "us-central1", "503", "101"); err == nil {
		t.Fatal("a global address is for load balancers")
	}
	if _, err := p.DisassociateAddress(ctx, acct, "alice@example.com", "us-central1", "502"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReleaseAddress(ctx, acct, "alice@example.com", "us-central1", "502"); err == nil {
		t.Fatal("an address in use is not released")
	}
	if _, err := p.ReleaseAddress(ctx, acct, "alice@example.com", "us-central1", "501"); err != nil {
		t.Fatal(err)
	}
	base := "/compute/v1/projects/payments-prod-4182/"
	want := []string{base + "regions/us-central1/addresses",
		base + "zones/us-central1-a/instances/101/deleteAccessConfig",
		base + "zones/us-central1-a/instances/101/addAccessConfig",
		base + "zones/us-central1-a/instances/web-1/deleteAccessConfig",
		"DELETE " + base + "regions/us-central1/addresses/501"}
	if strings.Join(g.posts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %v", g.posts)
	}
	if g.bodies[0] != `{"addressType":"EXTERNAL","name":"static-new"}` {
		t.Errorf("reserve body = %s", g.bodies[0])
	}
	if g.queries[1] != "accessConfig=External+NAT&networkInterface=nic0" || g.queries[2] != "networkInterface=nic0" ||
		g.bodies[2] != `{"name":"external-nat","natIP":"35.1.1.1","type":"ONE_TO_ONE_NAT"}` {
		t.Errorf("assign: %v %v", g.queries, g.bodies)
	}
	if g.queries[3] != "accessConfig=External+NAT&networkInterface=nic0" {
		t.Errorf("unassign: %v", g.queries[3])
	}
}

func TestAVMIsRemovedWithTheDisksGoogleDeletesWithIt(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	ctx := context.Background()
	plan, err := p.PlanRemove(ctx, acct, "alice@example.com", "us-central1", "101")
	if err != nil {
		t.Fatal(err)
	}
	want := cloud.RemovePlan{ID: "101", Name: "web-1", State: "running", Size: "e2-small", PublicIP: "34.1.2.3", Volumes: []cloud.Volume{
		{ID: "200", Device: "persistent-disk-0", SizeGB: 10, Deleted: true, Description: "web-1-boot (boot)"},
		{ID: "201", Device: "data", SizeGB: 200, Description: "web-1"}}}
	if plan.Hash() != want.Hash() {
		t.Fatalf("plan = %+v", plan)
	}
	res, err := p.Remove(ctx, acct, "alice@example.com", "us-central1", "101", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 1 || !strings.HasPrefix(res.Snapshots["200"], "web-1-boot-final-") {
		t.Fatalf("snapshots = %v", res.Snapshots)
	}
	base := "/compute/v1/projects/payments-prod-4182/zones/us-central1-a/"
	if strings.Join(g.posts, "\n") != base+"disks/web-1-boot/createSnapshot\nDELETE "+base+"instances/101" {
		t.Fatalf("calls = %v", g.posts)
	}
}

func TestManagedVMsAndFinalSnapshotNames(t *testing.T) {
	for _, c := range []struct {
		labels, meta map[string]string
		want         string
	}{
		{nil, map[string]string{"created-by": "projects/1/zones/us-central1-a/instanceGroupManagers/web"}, "managed instance group web"},
		{map[string]string{"goog-gke-node": "", "goog-k8s-cluster-name": "prod"}, nil, "GKE cluster prod"},
		{map[string]string{"goog-dataproc-cluster-name": "etl"}, nil, "Dataproc cluster etl"},
		{map[string]string{"team": "web"}, map[string]string{"startup-script": "x"}, ""},
	} {
		if got := managedBy(c.labels, c.meta); got != c.want {
			t.Errorf("%v %v: %q, want %q", c.labels, c.meta, got, c.want)
		}
	}
	at := time.Date(2026, 11, 17, 4, 5, 0, 0, time.UTC)
	if n := finalName(strings.Repeat("a", 70), at); len(n) != 63 || !ValidName(n) {
		t.Errorf("final name %q", n)
	}
	if n := finalName("web-1-boot", at); n != "web-1-boot-final-20261117-0405" {
		t.Errorf("final name %q", n)
	}
}

func TestAVMIsLaunchedFromAnImageFamilyWithNoServiceAccount(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	ctx := context.Background()
	imgs, err := p.Images(ctx, acct, "alice@example.com", "us-central1")
	if err != nil || len(imgs) != len(imageFamilies)-1+2 || imgs[0].ID != "projects/debian-cloud/global/images/family/debian-12" || imgs[0].Name != "debian-12-v20261101" || !imgs[0].Linux {
		t.Fatalf("images = %+v %v", imgs, err)
	}
	zones, err := p.Zones(ctx, acct, "alice@example.com", "us-central1")
	if err != nil || strings.Join(zones, ",") != "us-central1-a,us-central1-c" {
		t.Fatalf("zones = %v %v", zones, err)
	}
	spec := cloud.LaunchSpec{Region: "us-central1", Zone: "us-central1-a", Name: "web-3", Image: imgs[0].ID, Size: "e2-medium", DiskGB: 30,
		Subnet: "801", Firewalls: []string{"901", "902"}, Protect: true, PublicIP: true, ExtraDisks: []int32{100}, Spot: true,
		Tags: map[string]string{"argus:launched-by": "Alice@Example.com", "argus:change": "ABC-1", "team": "web"}, UserData: "#!/bin/sh\necho enroll\n"}
	res, err := p.Launch(ctx, acct, "alice@example.com", spec)
	if err != nil || res.ResourceID != "7001" {
		t.Fatalf("launch: %+v %v", res, err)
	}
	if len(g.posts) != 1 || g.posts[0] != "/compute/v1/projects/payments-prod-4182/zones/us-central1-a/instances" {
		t.Fatalf("posts = %v", g.posts)
	}
	var body struct {
		MachineType string `json:"machineType"`
		Disks       []struct {
			Boot             bool   `json:"boot"`
			AutoDelete       bool   `json:"autoDelete"`
			DeviceName       string `json:"deviceName"`
			InitializeParams struct {
				SourceImage string `json:"sourceImage"`
				DiskSizeGb  string `json:"diskSizeGb"`
			} `json:"initializeParams"`
		} `json:"disks"`
		NetworkInterfaces []struct {
			Subnetwork    string           `json:"subnetwork"`
			AccessConfigs []map[string]any `json:"accessConfigs"`
		} `json:"networkInterfaces"`
		Labels             map[string]string        `json:"labels"`
		DeletionProtection bool                     `json:"deletionProtection"`
		ServiceAccounts    []any                    `json:"serviceAccounts"`
		Tags               struct{ Items []string } `json:"tags"`
		Metadata           struct {
			Items []struct{ Key, Value string }
		} `json:"metadata"`
		Scheduling struct {
			ProvisioningModel string `json:"provisioningModel"`
		} `json:"scheduling"`
	}
	if err := json.Unmarshal([]byte(g.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.MachineType != "zones/us-central1-a/machineTypes/e2-medium" || len(body.Disks) != 2 || !body.Disks[0].Boot || !body.Disks[1].AutoDelete ||
		body.Disks[0].InitializeParams.SourceImage != imgs[0].ID || body.Disks[0].InitializeParams.DiskSizeGb != "30" ||
		body.Disks[1].DeviceName != "web-3-data-1" || body.Disks[1].InitializeParams.DiskSizeGb != "100" {
		t.Errorf("disks: %s", g.bodies[0])
	}
	if body.NetworkInterfaces[0].Subnetwork != "/compute/v1/projects/payments-prod-4182/regions/us-central1/subnetworks/default" || len(body.NetworkInterfaces[0].AccessConfigs) != 1 {
		t.Errorf("network: %s", g.bodies[0])
	}
	if strings.Join(body.Tags.Items, ",") != "http-server,ssh" {
		t.Errorf("network tags = %v", body.Tags.Items)
	}
	if body.Labels["argus-launched-by"] != "alice_example_com" || body.Labels["argus-change"] != "abc-1" || body.Labels["team"] != "web" {
		t.Errorf("labels = %v", body.Labels)
	}
	if !body.DeletionProtection || body.ServiceAccounts == nil || len(body.ServiceAccounts) != 0 || body.Scheduling.ProvisioningModel != "SPOT" {
		t.Errorf("settings: %s", g.bodies[0])
	}
	if len(body.Metadata.Items) != 1 || body.Metadata.Items[0].Key != "startup-script" ||
		!strings.Contains(body.Metadata.Items[0].Value, "[ -e /var/lib/argus-first-boot ] && exit 0") || !strings.HasSuffix(body.Metadata.Items[0].Value, "echo enroll\n") {
		t.Errorf("startup script: %+v", body.Metadata.Items)
	}
	for _, bad := range []cloud.LaunchSpec{
		func() cloud.LaunchSpec { b := spec; b.Zone = "europe-west1-b"; return b }(),
		func() cloud.LaunchSpec { b := spec; b.Image = "projects/evil/global/images/x"; return b }(),
		func() cloud.LaunchSpec { b := spec; b.Firewalls = []string{"903"}; return b }(),
		func() cloud.LaunchSpec { b := spec; b.Name = "Web 3"; return b }(),
	} {
		if _, err := p.Launch(ctx, acct, "alice@example.com", bad); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
	if len(g.posts) != 1 {
		t.Fatalf("nothing more should be made: %v", g.posts)
	}
}

func TestAnImageIsMadeOfTheBootDiskAndLaunchedFrom(t *testing.T) {
	p, g, acct := newManageFake(t, "argus-0123", ManageDescription("argus-0123"))
	ctx := context.Background()
	if _, err := p.CreateImage(ctx, acct, "alice@example.com", "us-central1", "101", "Web Golden", false); err == nil {
		t.Fatal("an uppercase name was taken")
	}
	if _, err := p.CreateImage(ctx, acct, "alice@example.com", "us-central1", "101", "web-golden-2", true); err == nil {
		t.Fatal("a reboot was taken")
	}
	res, err := p.CreateImage(ctx, acct, "alice@example.com", "us-central1", "101", "web-golden-2", false)
	if err != nil || res.ResourceID != "9001" || res.RequestID != "operation-i" {
		t.Fatalf("create: %+v %v", res, err)
	}
	if len(g.posts) != 1 || g.posts[0] != "/compute/v1/projects/payments-prod-4182/global/images" || g.queries[0] != "forceCreate=true" {
		t.Fatalf("posts = %v %v", g.posts, g.queries)
	}
	var body struct {
		Name       string            `json:"name"`
		SourceDisk string            `json:"sourceDisk"`
		Labels     map[string]string `json:"labels"`
	}
	_ = json.Unmarshal([]byte(g.bodies[0]), &body)
	if body.Name != "web-golden-2" || !strings.HasSuffix(body.SourceDisk, "/zones/us-central1-a/disks/web-1-boot") || body.Labels["argus-from-instance"] != "101" {
		t.Fatalf("body = %s", g.bodies[0])
	}

	imgs, err := p.Images(ctx, acct, "alice@example.com", "us-central1")
	if err != nil {
		t.Fatal(err)
	}
	own := map[string]cloud.Image{}
	for _, im := range imgs {
		if im.Family == "own" {
			own[im.Name] = im
		}
	}
	if len(own) != 2 || own["web-golden"].ID != "projects/payments-prod-4182/global/images/web-golden" || own["web-golden"].MinDiskGB != 30 ||
		!own["web-golden"].Linux || own["win-golden"].Linux {
		t.Fatalf("own images = %+v", own)
	}
	spec := cloud.LaunchSpec{Region: "us-central1", Zone: "us-central1-a", Name: "web-4", Image: own["web-golden"].ID, Size: "e2-medium", DiskGB: 30, Subnet: "801"}
	if _, err := p.Launch(ctx, acct, "alice@example.com", spec); err != nil {
		t.Fatalf("launch from an own image: %v", err)
	}
	spec.Image = "projects/someone-else/global/images/web-golden"
	if _, err := p.Launch(ctx, acct, "alice@example.com", spec); err == nil {
		t.Fatal("another project's image was taken")
	}

	if _, err := p.DeleteImage(ctx, acct, "alice@example.com", "global", "9001", true); err == nil {
		t.Fatal("deleting with snapshots was taken")
	}
	g.posts = nil
	if res, err := p.DeleteImage(ctx, acct, "alice@example.com", "global", "9001", false); err != nil || res.ResourceID != "9001" {
		t.Fatalf("delete: %+v %v", res, err)
	}
	if len(g.posts) != 1 || g.posts[0] != "DELETE /compute/v1/projects/payments-prod-4182/global/images/9001" {
		t.Fatalf("posts = %v", g.posts)
	}
}

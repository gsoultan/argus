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
		default:
			_, _ = w.Write([]byte(`{"items":{}}`))
		}
	}))
	mux.HandleFunc("POST /compute/v1/", auth(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.posts = append(g.posts, r.URL.Path)
		g.bodies = append(g.bodies, string(b))
		g.agents = append(g.agents, r.Header.Get("User-Agent"))
		g.mu.Unlock()
		if strings.Contains(r.URL.Path, "/zones/us-central1-a/instances/101/reset") {
			_, _ = w.Write([]byte(`{"name":"operation-9","status":"DONE","error":{"errors":[{"message":"The resource is not ready"}]}}`))
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
	for _, a := range []cloud.Action{cloud.ActionStart, cloud.ActionStop, cloud.ActionReboot, cloud.ActionVolumeGrow} {
		if !cloud.Supports(p, a) {
			t.Errorf("%s should be supported", a)
		}
	}
	for _, a := range []cloud.Action{cloud.ActionLaunch, cloud.ActionRemove, cloud.ActionHibernate, cloud.ActionResize, cloud.ActionVolumeDelete} {
		if cloud.Supports(p, a) {
			t.Errorf("%s is not built for Google Cloud yet", a)
		}
	}
	if _, err := p.Launch(context.Background(), cloud.Account{}, "alice@example.com", cloud.LaunchSpec{}); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("launch: %v", err)
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

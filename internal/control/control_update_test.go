package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/controlupdate"
)

/*
Needs Postgres for the routes:

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control -run ControlUpdate
*/

// manifestServer publishes 0.9.0's migrations: ours plus extra.
func manifestServer(t *testing.T, extra ...MigrationInfo) *httptest.Server {
	ours, err := MigrationManifest()
	if err != nil {
		t.Fatal(err)
	}
	list := append(append([]MigrationInfo{}, ours...), extra...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0.9.0/"+ManifestName("0.9.0") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"migrations": list})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func updatesAPI(t *testing.T, a *API, releases string) {
	a.Version = "0.8.2"
	a.Releases = &ReleaseWatcher{}
	a.Releases.control = &AgentRelease{Version: "0.9.0", PublishedAt: time.Now()}
	a.Updates = &ControlUpdater{Dir: t.TempDir(), ReleasesURL: releases}
}

func TestControlUpdateRollbackIsCleanWhenMigrationsOnlyAdd(t *testing.T) {
	a := &API{}
	updatesAPI(t, a, manifestServer(t, MigrationInfo{Name: "999_new_table.sql"}).URL)
	v := a.controlUpdateView(context.Background())
	if !v.Newer || v.Rollback == nil || !v.Rollback.Known {
		t.Fatalf("view %+v", v)
	}
	if v.Rollback.Added != 1 || len(v.Rollback.Breaking) != 0 {
		t.Fatalf("rollback %+v, want one additive migration", v.Rollback)
	}
}

func TestControlUpdateRollbackNamesWhatItTakesAway(t *testing.T) {
	a := &API{}
	updatesAPI(t, a, manifestServer(t,
		MigrationInfo{Name: "998_add.sql"},
		MigrationInfo{Name: "999_drop.sql", Breaking: true, Why: "drops users.nickname"}).URL)
	v := a.controlUpdateView(context.Background())
	if v.Rollback.Added != 2 || len(v.Rollback.Breaking) != 1 || v.Rollback.Breaking[0].Why != "drops users.nickname" {
		t.Fatalf("rollback %+v", v.Rollback)
	}
}

func TestControlUpdateRollbackUnknownWithoutAManifest(t *testing.T) {
	a := &API{}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	updatesAPI(t, a, srv.URL)
	v := a.controlUpdateView(context.Background())
	if v.Rollback == nil || v.Rollback.Known || !strings.Contains(v.Rollback.Reason, "backup") {
		t.Fatalf("rollback %+v", v.Rollback)
	}
}

func TestControlUpdateNothingNewerOnTheNewest(t *testing.T) {
	a := &API{}
	updatesAPI(t, a, "")
	a.Version = "0.9.0"
	if v := a.controlUpdateView(context.Background()); v.Newer || v.Rollback != nil {
		t.Fatalf("view %+v", v)
	}
}

func TestControlUpdateWaitsForSessionsThenHandsOver(t *testing.T) {
	u := &ControlUpdater{Dir: t.TempDir()}
	open := make(chan int, 1)
	open <- 2
	n := 2
	sessions := func() ControlUpdateSessions {
		select {
		case n = <-open:
		default:
		}
		return ControlUpdateSessions{Ending: n}
	}
	req := controlupdate.Request{ID: "w1", Version: "0.9.0", From: "0.8.2", RequestedBy: "ops@acme.co"}
	u.wait(req, sessions)
	st, _ := controlupdate.ReadStatus(u.Dir)
	if st.State != controlupdate.StateWaiting || !strings.Contains(st.Message, "2 sessions") {
		t.Fatalf("status %+v", st)
	}
	if _, err := controlupdate.ReadRequest(u.Dir); err == nil {
		t.Fatal("handed over while sessions were open")
	}
	open <- 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := controlupdate.ReadRequest(u.Dir); err == nil {
			if r.ID != "w1" || r.Version != "0.9.0" {
				t.Fatalf("request %+v", r)
			}
			st, _ := controlupdate.ReadStatus(u.Dir)
			if st.State != controlupdate.StateRequested {
				t.Fatalf("status %+v", st)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("not handed over once the sessions ended")
}

func TestControlUpdateWaitingCanBeCancelled(t *testing.T) {
	u := &ControlUpdater{Dir: t.TempDir()}
	req := controlupdate.Request{ID: "c1", Version: "0.9.0", From: "0.8.2"}
	u.wait(req, func() ControlUpdateSessions { return ControlUpdateSessions{Ending: 1} })
	st, ok := u.cancel("cancelled by ops@acme.co")
	if !ok || st.State != controlupdate.StateCancelled {
		t.Fatalf("cancel = %+v, %v", st, ok)
	}
	if _, ok := u.cancel("again"); ok {
		t.Fatal("cancelled twice")
	}
	if _, err := controlupdate.ReadRequest(u.Dir); err == nil {
		t.Fatal("a cancelled update was handed over")
	}
}

func TestControlUpdateRoutes(t *testing.T) {
	h := newTenantHarness(t)
	updatesAPI(t, h.api, manifestServer(t).URL)
	tn, orgOwner, orgPw := newTenant(t, h.store)
	ownerEmail, ownerPw := newAccount(t, h.store, "owner")
	opEmail, opPw := newAccount(t, h.store, "operator")

	inOrg := h.signIn(t, tn.Slug, orgOwner, orgPw)
	if code, _ := h.call(t, inOrg, tn.Slug, http.MethodGet, "/api/v1/platform/update", ""); code != http.StatusNotFound {
		t.Errorf("an organization's owner = %d, want 404", code)
	}
	inOp := h.signIn(t, "", opEmail, opPw)
	if code, _ := h.call(t, inOp, "", http.MethodPost, "/api/v1/platform/update",
		`{"version":"0.9.0","when":"now"}`); code != http.StatusNotFound {
		t.Errorf("an operator asking = %d, want 404", code)
	}

	in := h.signIn(t, "", ownerEmail, ownerPw)
	code, body := h.call(t, in, "", http.MethodGet, "/api/v1/platform/update", "")
	if code != http.StatusOK {
		t.Fatalf("view = %d: %s", code, body)
	}
	var v ControlUpdateView
	_ = json.Unmarshal([]byte(body), &v)
	if !v.Newer || v.Newest.Version != "0.9.0" || v.Rollback == nil || !v.Rollback.Known {
		t.Fatalf("view %s", body)
	}

	// Only the newest release, and only when this install can update.
	h.api.Updates.Unsupported = "not run by systemd"
	if code, body := h.call(t, in, "", http.MethodPost, "/api/v1/platform/update",
		`{"version":"0.9.0","when":"now"}`); code != http.StatusConflict || !strings.Contains(body, "systemd") {
		t.Errorf("unsupported = %d %s", code, body)
	}
	h.api.Updates.Unsupported = ""
	if code, _ := h.call(t, in, "", http.MethodPost, "/api/v1/platform/update",
		`{"version":"9.9.9","when":"now"}`); code != http.StatusConflict {
		t.Errorf("a version that is not the newest = %d, want 409", code)
	}

	code, body = h.call(t, in, "", http.MethodPost, "/api/v1/platform/update", `{"version":"0.9.0","when":"idle"}`)
	if code != http.StatusAccepted {
		t.Fatalf("ask = %d: %s", code, body)
	}
	r, err := controlupdate.ReadRequest(h.api.Updates.Dir)
	if err != nil || r.Version != "0.9.0" || r.From != "0.8.2" || r.RequestedBy != ownerEmail {
		t.Fatalf("request %+v, %v", r, err)
	}
	if code, _ := h.call(t, in, "", http.MethodPost, "/api/v1/platform/update",
		`{"version":"0.9.0","when":"now"}`); code != http.StatusConflict {
		t.Errorf("a second update while one runs = %d, want 409", code)
	}

	// Everyone signed in sees that it is under way, and nothing more.
	code, body = h.call(t, inOp, "", http.MethodGet, "/api/v1/control-update", "")
	if code != http.StatusOK || !strings.Contains(body, `"active":true`) || strings.Contains(body, ownerEmail) {
		t.Errorf("banner = %d %s", code, body)
	}

	// The helper finishes; the next start records it once.
	st, _ := controlupdate.ReadStatus(h.api.Updates.Dir)
	st.State, st.Message, st.At = controlupdate.StateDone, "updated from 0.8.2 to 0.9.0; ready in 4s", time.Now()
	if err := controlupdate.WriteStatus(h.api.Updates.Dir, st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.api.ControlUpdateStarted(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, _ := controlupdate.ReadStatus(h.api.Updates.Dir); st.Audited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the outcome was not recorded")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r, ok := controlupdate.ReadReady(h.api.Updates.Dir); !ok || r.Version != "0.8.2" || !r.Listening {
		t.Errorf("ready %+v", r)
	}
	events, err := h.store.AuditEvents(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range events {
		if strings.HasPrefix(e.Action, "control.") {
			actions = append(actions, e.Action)
		}
	}
	if strings.Join(actions, ",") != "control.updated,control.update_requested" &&
		strings.Join(actions, ",") != "control.update_requested,control.updated" {
		t.Errorf("audit %v", actions)
	}
}

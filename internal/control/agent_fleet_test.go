package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

/*
The fleet's agents: finding a newer release, Update all, and automatic
updates.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run 'AgentFleet|AgentRelease|AutomaticAgent'
*/

func TestNewestAgentReleaseSkipsOnesAgentsCannotDownload(t *testing.T) {
	pkgs := func(v string) []struct {
		Name string `json:"name"`
	} {
		return []struct {
			Name string `json:"name"`
		}{{"checksums.txt"}, {"argus-agent_" + v + "_linux_amd64.deb"}}
	}
	list := []ghRelease{
		{Tag: "1.2.0"}, // no "v", and its build left no files
		{Tag: "v1.12.0-rc.1", Prerelease: true, Assets: pkgs("1.12.0-rc.1")},
		{Tag: "v1.11.0", Draft: true, Assets: pkgs("1.11.0")},
		{Tag: "v1.10.0", Assets: pkgs("1.10.0")[:1]}, // checksums but no packages
		{Tag: "v1.9.0", Assets: pkgs("1.9.0")},
		{Tag: "v1.1.0", Assets: pkgs("1.1.0")},
	}
	if r := newestAgentRelease(list); r == nil || r.Version != "1.9.0" {
		t.Fatalf("newest = %+v, want 1.9.0", r)
	}
	for _, c := range []struct {
		a, b string
		less bool
	}{
		{"1.9.0", "1.10.0", true}, {"1.10.0", "1.9.0", false}, {"1.2.0-rc.1", "1.2.0", true},
		{"1.2.0", "1.2.0", false}, {"v1.0.0", "1.0.1", true}, {"dev", "0.0.1", true},
	} {
		if got := versionLess(c.a, c.b); got != c.less {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// releasesAt serves a GitHub-shaped release list naming versions.
func releasesAt(t *testing.T, versions ...string) *ReleaseWatcher {
	t.Helper()
	var list []map[string]any
	for _, v := range versions {
		list = append(list, map[string]any{"tag_name": "v" + v, "html_url": "https://example/" + v,
			"assets": []map[string]string{{"name": "checksums.txt"},
				{"name": "argus-agent_" + v + "_linux_amd64.deb"}}})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)
	w := &ReleaseWatcher{API: srv.URL}
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w
}

func fleetAPI(t *testing.T, console string, releases *ReleaseWatcher) (*Store, http.Handler) {
	t.Helper()
	a, s, h, _ := enrollAPI(t)
	operator, _ := newAccount(t, s, "operator")
	a.UserTokens["operator-token"] = operator
	a.Version = console
	a.Releases = releases
	return s, h
}

func fleetRow(t *testing.T, h http.Handler, host string) (AgentFleetView, AgentFleetRow) {
	t.Helper()
	w := do(t, h, http.MethodGet, "/api/v1/agents/updates", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("fleet: %d %s", w.Code, w.Body)
	}
	var v AgentFleetView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	for _, f := range v.Servers {
		if f.Hostname == host {
			return v, f
		}
	}
	t.Fatalf("%s is not on the fleet view", host)
	return v, AgentFleetRow{}
}

// A release newer than the console is found and offered, on the server page
// and as what an update defaults to.
func TestAgentReleaseNewerThanTheConsoleIsOffered(t *testing.T) {
	s, h := fleetAPI(t, "v1.0.0", releasesAt(t, "1.0.0", "1.1.0"))
	asset, cred := enrolledAsset(t, s, h)
	beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`)

	path := "/api/v1/assets/" + asset.ID + "/agent/update"
	var view AgentUpdateView
	_ = json.Unmarshal(do(t, h, http.MethodGet, path, "admin-token", "").Body.Bytes(), &view)
	if view.Latest != "1.1.0" || view.ConsoleVersion != "1.0.0" {
		t.Fatalf("offered %q (console %q), want 1.1.0 found on the releases", view.Latest, view.ConsoleVersion)
	}
	w := do(t, h, http.MethodPost, path, "admin-token", `{}`)
	var req AgentUpdateRequest
	_ = json.Unmarshal(w.Body.Bytes(), &req)
	if w.Code != http.StatusAccepted || req.Version != "1.1.0" {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}

	// With no release found, the console's own version is what is offered.
	_, h2 := fleetAPI(t, "v1.0.0", nil)
	v, _ := fleetRow(t, h2, asset.Hostname)
	if v.Latest != "1.0.0" || v.Release != nil || v.CheckedAt != nil {
		t.Errorf("without a release check: %+v", v)
	}
}

// Update all asks every agent that is behind and can update, and says why
// it left the others.
func TestAgentFleetUpdateAllAsksOnlyAgentsThatCan(t *testing.T) {
	s, h := fleetAPI(t, "v1.1.0", nil)
	behind, credBehind := enrolledAsset(t, s, h)
	cannot, credCannot := enrolledAsset(t, s, h)
	current, credCurrent := enrolledAsset(t, s, h)
	old, credOld := enrolledAsset(t, s, h)
	beatWith(t, h, credBehind, behind.Hostname, "1.0.0", `{"supported":true}`)
	beatWith(t, h, credCannot, cannot.Hostname, "1.0.0",
		`{"supported":false,"reason":"this server does not run systemd"}`)
	beatWith(t, h, credCurrent, current.Hostname, "1.1.0", `{"supported":true}`)
	heartbeatAs(t, h, credOld, old.Hostname) // older than updating itself

	v, f := fleetRow(t, h, behind.Hostname)
	if f.State != agentBehind || v.Latest != "1.1.0" {
		t.Fatalf("behind: %+v", f)
	}
	for host, want := range map[string]string{cannot.Hostname: agentCannot,
		current.Hostname: agentCurrent, old.Hostname: agentTooOld} {
		if _, f := fleetRow(t, h, host); f.State != want {
			t.Errorf("%s: state %s, want %s", host, f.State, want)
		}
	}

	if w := do(t, h, http.MethodPost, "/api/v1/agents/updates", "operator-token", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("an operator updated the fleet: %d", w.Code)
	}
	w := do(t, h, http.MethodPost, "/api/v1/agents/updates", "admin-token", `{}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("update all: %d %s", w.Code, w.Body)
	}
	var out struct {
		Asked   []string         `json:"asked"`
		Skipped []AgentFleetSkip `json:"skipped"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if !contains(out.Asked, behind.Hostname) || contains(out.Asked, cannot.Hostname) ||
		contains(out.Asked, current.Hostname) || contains(out.Asked, old.Hostname) {
		t.Errorf("asked %v", out.Asked)
	}
	why := map[string]string{}
	for _, sk := range out.Skipped {
		why[sk.Hostname] = sk.Why
	}
	if !strings.Contains(why[cannot.Hostname], "systemd") || !strings.Contains(why[old.Hostname], "too old") {
		t.Errorf("skipped: %+v", out.Skipped)
	}
	if r := beatWith(t, h, credBehind, behind.Hostname, "1.0.0", `{"supported":true}`); r["update"] == nil {
		t.Error("the agent asked by Update all was not told on its heartbeat")
	}
	if e := lastAuditOn(t, s, behind.Hostname); e.Action != "agent.update_requested" ||
		!strings.Contains(e.Detail, "every agent behind") {
		t.Errorf("audit = %s: %s", e.Action, e.Detail)
	}
	if _, f := fleetRow(t, h, behind.Hostname); f.State != agentUpdating {
		t.Errorf("after Update all: %s", f.State)
	}

	// Naming one server asks that one only.
	w = do(t, h, http.MethodPost, "/api/v1/agents/updates", "admin-token",
		`{"hostnames":["`+current.Hostname+`"]}`)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Asked) != 0 || len(out.Skipped) != 1 || !strings.Contains(out.Skipped[0].Why, "already runs 1.1.0") {
		t.Errorf("naming a current server: %s", w.Body)
	}
}

// With automatic updates on, an agent that is behind is asked on its own
// heartbeat; a version that went back on a server is not tried there again.
func TestAutomaticAgentUpdatesAskOnTheHeartbeatButDoNotRetryAFailure(t *testing.T) {
	s, h := fleetAPI(t, "v1.0.0", releasesAt(t, "1.1.0"))
	asset, cred := enrolledAsset(t, s, h)
	t.Cleanup(func() { _, _ = s.SetAgentAutoUpdate(context.Background(), false, "test") })

	if r := beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`); r["update"] != nil {
		t.Fatalf("updated with automatic updates off: %v", r)
	}
	if w := do(t, h, http.MethodPut, "/api/v1/agents/updates/automatic", "operator-token", `{"on":true}`); w.Code != http.StatusForbidden {
		t.Fatalf("an operator turned on automatic updates: %d", w.Code)
	}
	if w := do(t, h, http.MethodPut, "/api/v1/agents/updates/automatic", "admin-token", `{"on":true}`); w.Code != http.StatusOK {
		t.Fatalf("turn on: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, "agents"); e.Action != "agent.auto_update_on" {
		t.Errorf("audit = %s", e.Action)
	}

	r := beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`)
	u, _ := r["update"].(map[string]any)
	if u["version"] != "1.1.0" || u["now"] != false {
		t.Fatalf("an agent behind was not asked to update when idle: %v", r)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "agent.update_requested" ||
		!strings.Contains(e.Detail, "Automatic updates asked") {
		t.Errorf("audit = %s: %s", e.Action, e.Detail)
	}
	v, f := fleetRow(t, h, asset.Hostname)
	if !v.Automatic || f.State != agentUpdating || f.Request.RequestedBy != autoUpdater {
		t.Fatalf("fleet: automatic %v, %+v", v.Automatic, f)
	}

	// It went back. The request is cleared and not made again on its own.
	back := `{"supported":true,"id":"` + f.Request.ID + `","state":"rolled_back","from":"1.0.0",` +
		`"target":"1.1.0","message":"1.1.0 did not reconnect to Argus within 1m30s; went back to 1.0.0"}`
	if r := beatWith(t, h, cred, asset.Hostname, "1.0.0", back); r["update"] != nil {
		t.Fatalf("a version that went back was tried again at once: %v", r)
	}
	if r := beatWith(t, h, cred, asset.Hostname, "1.0.0", back); r["update"] != nil {
		t.Fatalf("a version that went back was tried again on the next heartbeat: %v", r)
	}
	if _, f := fleetRow(t, h, asset.Hostname); f.State != agentHeld || !strings.Contains(f.Why, "went back") {
		t.Errorf("after going back: %+v", f)
	}
	// Update all leaves it too; naming it is a person deciding to retry.
	w := do(t, h, http.MethodPost, "/api/v1/agents/updates", "admin-token", `{}`)
	if strings.Contains(strings.SplitN(w.Body.String(), `"skipped"`, 2)[0], asset.Hostname) {
		t.Errorf("Update all retried a held server: %s", w.Body)
	}
	w = do(t, h, http.MethodPost, "/api/v1/agents/updates", "admin-token", `{"hostnames":["`+asset.Hostname+`"]}`)
	if !strings.Contains(w.Body.String(), `"asked":["`+asset.Hostname+`"]`) {
		t.Errorf("naming a held server did not retry it: %s", w.Body)
	}

	do(t, h, http.MethodPut, "/api/v1/agents/updates/automatic", "admin-token", `{"on":false}`)
	if e := lastAuditOn(t, s, "agents"); e.Action != "agent.auto_update_off" {
		t.Errorf("audit = %s", e.Action)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

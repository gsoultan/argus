package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Installing and removing software from the Apps tab, through a real tunnel to
a real agent loop whose package manager is a fake: who may ask, a preview
kept and gone ahead with, the run sent on the heartbeat with the preview's
hash, how it ends, and undoing it.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Software
*/

/* ── Without a database ──────────────────────────────────────────────────── */

func TestSoftwareUndoOf(t *testing.T) {
	install := PackageRun{Mode: PackagesInstall, State: RunDone,
		Items: []tunnel.SoftwareItem{{Name: "nginx"}, {Name: "curl"}},
		Upgraded: []PackageChange{
			{Name: "nginx", To: "1.24.0-2"},
			{Name: "libnginx-mod-http", To: "1.24.0-2"},
			{Name: "curl", From: "8.5.0-1", To: "8.5.0-2"},
		}}
	action, items, err := undoOf(install)
	if err != nil || action != PackagesRemove || len(items) != 1 || items[0].Name != "nginx" || items[0].Version != "" {
		t.Fatalf("undo of an install = %s %+v %v; want removing only what it added and was asked for", action, items, err)
	}

	remove := PackageRun{Mode: PackagesRemove, State: RunDone, Items: []tunnel.SoftwareItem{{Name: "nginx"}},
		Upgraded: []PackageChange{{Name: "nginx", From: "1.24.0-2"}, {Name: "nginx-common", From: "1.24.0-2"}}}
	action, items, err = undoOf(remove)
	if err != nil || action != PackagesInstall || len(items) != 1 || items[0].Version != "1.24.0-2" {
		t.Fatalf("undo of a removal = %s %+v %v; want reinstalling at the version removed", action, items, err)
	}

	for _, r := range []PackageRun{
		{Mode: PackagesAll, State: RunDone},
		{Mode: PackagesInstall, State: RunFailed, Items: install.Items, Upgraded: install.Upgraded},
		{Mode: PackagesInstall, State: RunDone, Items: []tunnel.SoftwareItem{{Name: "curl"}},
			Upgraded: []PackageChange{{Name: "curl", From: "1", To: "2"}}},
	} {
		if _, _, err := undoOf(r); err == nil {
			t.Errorf("%s %s run could be undone", r.Mode, r.State)
		}
	}
}

func TestDescribeSoftware(t *testing.T) {
	got := describeMode(PackageRun{Mode: PackagesRemove, Purge: true,
		Items: []tunnel.SoftwareItem{{Name: "redis-server"}}})
	if got != "redis-server and its settings" {
		t.Errorf("describe = %q", got)
	}
	got = runPhrase(PackageRun{Mode: PackagesInstall, Items: []tunnel.SoftwareItem{{Name: "nginx", Version: "1.24.0-2"}}})
	if got != "installing nginx 1.24.0-2" {
		t.Errorf("phrase = %q", got)
	}
}

/* ── Through the agent ───────────────────────────────────────────────────── */

// fakeSoftware is a server's package manager as the agent's service sees it.
type fakeSoftware struct {
	mu    sync.Mutex
	asked []tunnel.GrantSoftware
	// changed makes the next preview answer differently, as when a newer
	// version arrived.
	changed bool
}

func (f *fakeSoftware) ask(_ context.Context, q tunnel.GrantSoftware) tunnel.SoftwareResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, q)
	switch q.Op {
	case tunnel.SoftwareInstalled:
		return tunnel.SoftwareResult{OK: true, Manager: "apt", Total: 2, Manual: 1, Installed: []tunnel.InstalledPackage{
			{Name: "openssh-server", Version: "1:9.6p1-3", Manual: true, Protected: "Argus reaches the server through it"},
			{Name: "curl", Version: "8.5.0-2"},
		}}
	case tunnel.SoftwareSearch:
		return tunnel.SoftwareResult{OK: true, Manager: "apt", Found: []tunnel.FoundPackage{
			{Name: "nginx", Version: "1.24.0-2", Origin: "noble", Versions: []string{"1.24.0-2", "1.24.0-1"}}}}
	}
	pv := &tunnel.SoftwarePreviewResult{Action: q.Action, Items: q.Items, Purge: q.Purge, Hash: strings.Repeat("a", 64)}
	switch {
	case q.Action == tunnel.SoftwareRemove && q.Items[0].Name == "systemd":
		pv.Blocked = "systemd keeps the server running; it cannot be removed from the console."
	case q.Action == tunnel.SoftwareRemove:
		pv.Changes = []tunnel.SoftwareChange{{Kind: tunnel.ChangeRemove, Name: q.Items[0].Name, From: "1.24.0-2", Asked: true}}
		pv.Hash = strings.Repeat("b", 64)
	default:
		pv.Changes = []tunnel.SoftwareChange{
			{Kind: tunnel.ChangeAdd, Name: "nginx", To: "1.24.0-2", Asked: true},
			{Kind: tunnel.ChangeAdd, Name: "nginx-common", To: "1.24.0-2"},
		}
		pv.Services = []string{"nginx.service"}
	}
	if f.changed {
		pv.Hash = strings.Repeat("c", 64)
	}
	return tunnel.SoftwareResult{OK: true, Manager: "apt", Preview: pv}
}

func TestSoftwareInstallThroughTheAgent(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	ctx := context.Background()
	admin, _ := newAccount(t, e.store, "admin")
	op, _ := newAccount(t, e.store, "operator")
	auditor, _ := newAccount(t, e.store, "auditor")
	e.api.UserTokens = map[string]string{"admin-token": admin, "op-token": op, "auditor-token": auditor}

	host := unique("software") + ".example"
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.30", Port: 22,
		Protocol: "ssh", Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.store.pool.Exec(context.Background(), `DELETE FROM package_runs WHERE lower(hostname) = lower($1)`, host)
		_ = e.store.ArchiveAsset(context.Background(), asset.ID)
	})
	beat := func(b *PackageBeat) *PackageOrders {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"hostname": host, "version": "1.2.0", "active_sessions": 0, "packages": b})
		w := do(t, e.h, http.MethodPost, "/api/v1/report/heartbeat", cred, string(raw))
		if w.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
		}
		var out struct {
			Packages *PackageOrders `json:"packages"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Packages
	}
	base := "/api/v1/assets/" + asset.ID + "/software"

	// Before the agent says it can manage packages, nothing is asked of it.
	if w := do(t, e.h, http.MethodGet, base, "admin-token", ""); w.Code != http.StatusConflict {
		t.Fatalf("a server that never reported: %d %s", w.Code, w.Body)
	}
	beat(&PackageBeat{Supported: true, Report: pkgReport(time.Now().UTC())})

	// Reported, but offline: an answer that says so.
	var ans SoftwareAnswer
	w := do(t, e.h, http.MethodGet, base, "admin-token", "")
	if _ = json.Unmarshal(w.Body.Bytes(), &ans); w.Code != http.StatusOK || ans.State != "offline" {
		t.Fatalf("offline: %d %s", w.Code, w.Body)
	}

	fake := &fakeSoftware{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.SoftwareService] = agent.Software(agent.SoftwareConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, Log: log, Ask: fake.ask})
	actx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")

	// An auditor sees what is installed; an operator does not.
	w = do(t, e.h, http.MethodGet, base, "auditor-token", "")
	ans = SoftwareAnswer{}
	if _ = json.Unmarshal(w.Body.Bytes(), &ans); w.Code != http.StatusOK || ans.State != "ok" ||
		len(ans.Installed) != 2 || ans.Installed[0].Protected == "" || ans.Manual != 1 {
		t.Fatalf("installed: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodGet, base, "op-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator read the installed list: %d", w.Code)
	}
	// Only admins search and preview; an auditor cannot.
	if w := do(t, e.h, http.MethodGet, base+"/search?q=nginx", "auditor-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an auditor searched: %d", w.Code)
	}
	// What would be an option to the package manager never reaches it.
	fake.mu.Lock()
	before := len(fake.asked)
	fake.mu.Unlock()
	for _, body := range []string{
		`{"action":"install","items":[{"name":"-o APT::Update::Pre-Invoke::=sh"}]}`,
		`{"action":"install","items":[{"name":"nginx","version":"--force"}]}`,
		`{"action":"install","items":[{"name":"nginx"}],"purge":true}`,
		`{"action":"upgrade","items":[{"name":"nginx"}]}`,
		`{"action":"install","items":[]}`,
	} {
		if w := do(t, e.h, http.MethodPost, base+"/preview", "admin-token", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if w := do(t, e.h, http.MethodGet, base+"/search?q=-o%20x", "admin-token", ""); w.Code != http.StatusBadRequest {
		t.Errorf("an option as a search: %d", w.Code)
	}
	fake.mu.Lock()
	if len(fake.asked) != before {
		t.Fatalf("a refused request reached the agent: %+v", fake.asked[before:])
	}
	fake.mu.Unlock()

	w = do(t, e.h, http.MethodGet, base+"/search?q=nginx", "admin-token", "")
	ans = SoftwareAnswer{}
	if _ = json.Unmarshal(w.Body.Bytes(), &ans); ans.State != "ok" || len(ans.Found) != 1 || len(ans.Found[0].Versions) != 2 {
		t.Fatalf("search: %d %s", w.Code, w.Body)
	}

	// A blocked preview is kept, shown, and cannot be gone ahead with.
	preview := func(body string) SoftwarePreview {
		t.Helper()
		w := do(t, e.h, http.MethodPost, base+"/preview", "admin-token", body)
		if w.Code != http.StatusOK {
			t.Fatalf("preview %s: %d %s", body, w.Code, w.Body)
		}
		var pv SoftwarePreview
		_ = json.Unmarshal(w.Body.Bytes(), &pv)
		return pv
	}
	blocked := preview(`{"action":"remove","items":[{"name":"systemd"}]}`)
	if blocked.Blocked == "" || blocked.ID == "" {
		t.Fatalf("blocked = %+v", blocked)
	}
	if w := do(t, e.h, http.MethodPost, base+"/runs", "admin-token", `{"previewId":"`+blocked.ID+`"}`); w.Code != http.StatusConflict {
		t.Fatalf("a blocked preview was gone ahead with: %d %s", w.Code, w.Body)
	}

	// An old preview has to be asked again.
	old := preview(`{"action":"install","items":[{"name":"nginx"}]}`)
	_, _ = e.store.pool.Exec(ctx, `UPDATE software_previews SET created_at = now() - interval '20 minutes' WHERE id = $1`, old.ID)
	if w := do(t, e.h, http.MethodPost, base+"/runs", "admin-token", `{"previewId":"`+old.ID+`"}`); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "fifteen minutes") {
		t.Fatalf("an old preview: %d %s", w.Code, w.Body)
	}

	pv := preview(`{"action":"install","items":[{"name":"nginx"}]}`)
	if pv.Blocked != "" || !pv.StartServices || len(pv.Result.Changes) != 2 || pv.ExpiresAt.Sub(pv.CreatedAt) != previewTTL {
		t.Fatalf("preview = %+v", pv)
	}
	w = do(t, e.h, http.MethodPost, base+"/runs", "admin-token", `{"previewId":"`+pv.ID+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	var run PackageRun
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	if run.Mode != PackagesInstall || run.State != RunQueued || len(run.Items) != 1 || !run.StartServices ||
		run.PreviewID == nil || *run.PreviewID != pv.ID {
		t.Fatalf("run = %+v", run)
	}
	if a := lastAuditOn(t, e.store, host); a.Action != "software.install_requested" ||
		!strings.Contains(a.Detail, "installing nginx") || !strings.Contains(a.Detail, "2 to add") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	// One at a time on a server, updates included.
	again := preview(`{"action":"install","items":[{"name":"nginx"}]}`)
	if w := do(t, e.h, http.MethodPost, base+"/runs", "admin-token", `{"previewId":"`+again.ID+`"}`); w.Code != http.StatusConflict {
		t.Fatalf("a second run on the server: %d %s", w.Code, w.Body)
	}

	// The heartbeat carries it with the hash the agent checks.
	o := beat(&PackageBeat{Supported: true})
	if o == nil || o.Run == nil || o.Run.ID != run.ID || o.Run.Mode != "install" || o.Run.Expect != pv.Result.Hash ||
		!o.Run.StartServices || len(o.Run.Items) != 1 || o.Run.Items[0].Name != "nginx" {
		t.Fatalf("order = %+v", o)
	}
	finished := time.Now().UTC()
	beat(&PackageBeat{Supported: true, Run: &PackageRunStatus{ID: run.ID, State: "done", Message: "installed or changed 2 packages",
		Upgraded: []PackageChange{{Name: "nginx", To: "1.24.0-2"}, {Name: "nginx-common", To: "1.24.0-2"}},
		Started:  []string{"nginx.service"}, StartedAt: &finished, FinishedAt: &finished}})
	a := lastAuditOn(t, e.store, host)
	if a.Action != "packages.run_finished" || !strings.Contains(a.Detail, "installed or changed 2 packages") ||
		!strings.Contains(a.Detail, "started nginx.service") || !strings.Contains(a.Detail, "The install on") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}

	w = do(t, e.h, http.MethodGet, base+"/runs", "auditor-token", "")
	var list struct {
		Runs []PackageRun `json:"runs"`
	}
	if _ = json.Unmarshal(w.Body.Bytes(), &list); w.Code != http.StatusOK || len(list.Runs) != 1 ||
		list.Runs[0].State != RunDone || len(list.Runs[0].Started) != 1 || list.Runs[0].AssetID != asset.ID {
		t.Fatalf("runs: %d %s", w.Code, w.Body)
	}

	// Undo is a preview of removing what it added, gone ahead with like any.
	w = do(t, e.h, http.MethodPost, "/api/v1/package-runs/"+run.ID+"/undo", "admin-token", "")
	var undo SoftwarePreview
	if _ = json.Unmarshal(w.Body.Bytes(), &undo); w.Code != http.StatusOK || undo.Action != PackagesRemove ||
		len(undo.Items) != 1 || undo.Items[0].Name != "nginx" || undo.UndoOf == nil || *undo.UndoOf != run.ID {
		t.Fatalf("undo: %d %s", w.Code, w.Body)
	}
	w = do(t, e.h, http.MethodPost, base+"/runs", "admin-token", `{"previewId":"`+undo.ID+`"}`)
	var removal PackageRun
	if _ = json.Unmarshal(w.Body.Bytes(), &removal); w.Code != http.StatusAccepted || removal.UndoOf == nil ||
		*removal.UndoOf != run.ID || removal.Mode != PackagesRemove {
		t.Fatalf("undo run: %d %s", w.Code, w.Body)
	}
	if a := lastAuditOn(t, e.store, host); a.Action != "software.remove_requested" || !strings.Contains(a.Detail, "undoing") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	// A preview for one server cannot be gone ahead with on another.
	other, _ := e.store.CreateAsset(ctx, AssetInput{Hostname: unique("software-other") + ".example",
		Address: "192.0.2.31", Port: 22, Protocol: "ssh", Principals: []string{"ubuntu"}})
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), other.ID) })
	if w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+other.ID+"/software/runs", "admin-token",
		`{"previewId":"`+again.ID+`"}`); w.Code == http.StatusAccepted {
		t.Fatalf("a preview went ahead on another server")
	}
}

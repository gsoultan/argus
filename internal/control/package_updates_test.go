package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

/*
Operating system updates from the console: what the agent reports, runs
asked for by hand and by a rule's window, approvals, several servers one at
a time, and what happens when a server goes quiet half way.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Package
*/

/* ── Without a database ──────────────────────────────────────────────────── */

func TestPackageWindowStart(t *testing.T) {
	// Sunday and Wednesday, 02:00 for two hours.
	r := PackageRule{Mode: PackagesSecurity, WindowDays: []int{0, 3}, WindowStartMinute: 120, WindowMinutes: 120}
	sun := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // a Sunday
	cases := []struct {
		name   string
		now    time.Time
		offset int
		in     bool
		start  time.Time
	}{
		{"before it opens", sun.Add(time.Hour + 59*time.Minute), 0, false, time.Time{}},
		{"as it opens", sun.Add(2 * time.Hour), 0, true, sun.Add(2 * time.Hour)},
		{"inside", sun.Add(3*time.Hour + 30*time.Minute), 0, true, sun.Add(2 * time.Hour)},
		{"as it closes", sun.Add(4 * time.Hour), 0, false, time.Time{}},
		{"a day without one", sun.AddDate(0, 0, 1).Add(3 * time.Hour), 0, false, time.Time{}},
		{"the other day", sun.AddDate(0, 0, 3).Add(3 * time.Hour), 0, true, sun.AddDate(0, 0, 3).Add(2 * time.Hour)},
		// 02:30 on Sunday in Jakarta (UTC+7) is 19:30 on Saturday in UTC.
		{"by the server's clock", sun.Add(-4*time.Hour - 30*time.Minute), 7 * 60, true, sun.Add(-5 * time.Hour)},
		{"not by the control plane's", sun.Add(3 * time.Hour), 7 * 60, false, time.Time{}},
	}
	for _, c := range cases {
		start, in := r.windowStart(c.now, c.offset)
		if in != c.in || !start.Equal(c.start) {
			t.Errorf("%s: in=%v start=%v, want in=%v start=%v", c.name, in, start, c.in, c.start)
		}
	}
	// A window that runs past midnight is still open the next morning.
	late := PackageRule{Mode: PackagesAll, WindowDays: []int{6}, WindowStartMinute: 23 * 60, WindowMinutes: 180}
	sat := sun.AddDate(0, 0, -1)
	if start, in := late.windowStart(sun.Add(time.Hour), 0); !in || !start.Equal(sat.Add(23*time.Hour)) {
		t.Errorf("past midnight: in=%v start=%v", in, start)
	}
	if _, in := late.windowStart(sun.Add(2*time.Hour), 0); in {
		t.Error("the window should have closed at 02:00")
	}
}

func TestPackageNextWindow(t *testing.T) {
	r := PackageRule{Mode: PackagesSecurity, WindowDays: []int{3}, WindowStartMinute: 120, WindowMinutes: 60}
	sun := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	next := r.nextWindow(sun, 0)
	if next == nil || !next.Equal(time.Date(2026, 11, 4, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %v", next)
	}
	r.Mode = PackagesNotify
	if r.nextWindow(sun, 0) != nil {
		t.Error("a rule that installs nothing on its own has no window")
	}
}

func TestPackagePolicyValidate(t *testing.T) {
	good := func() PackagePolicy {
		return PackagePolicy{Rules: []PackageRule{
			{Tag: "web", Mode: PackagesSecurity, WindowDays: []int{3, 0, 3}, WindowStartMinute: 60, WindowMinutes: 60, Reboot: "if_needed"},
			{Tag: "", Mode: PackagesNotify, WindowDays: []int{}, WindowStartMinute: 120, WindowMinutes: 120},
		}, Holds: []string{" postgresql* ", "", "postgresql*", "linux-image-*"}}
	}
	p := good()
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := p.Rules[0].WindowDays; len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Errorf("days = %v, want sorted and unique", got)
	}
	if p.Rules[1].Reboot != "never" || p.Rules[1].Position != 1 {
		t.Errorf("default rule = %+v", p.Rules[1])
	}
	if len(p.Holds) != 2 {
		t.Errorf("holds = %v", p.Holds)
	}
	bad := map[string]func(*PackagePolicy){
		"no rules":          func(p *PackagePolicy) { p.Rules = nil },
		"no default rule":   func(p *PackagePolicy) { p.Rules = p.Rules[:1] },
		"two default rules": func(p *PackagePolicy) { p.Rules[0].Tag = "" },
		"the same tag twice": func(p *PackagePolicy) {
			p.Rules = append(p.Rules, PackageRule{Tag: "WEB", Mode: PackagesOff, WindowMinutes: 60})
		},
		"a mode that is not":   func(p *PackagePolicy) { p.Rules[0].Mode = "everything" },
		"a reboot that is not": func(p *PackagePolicy) { p.Rules[0].Reboot = "always" },
		"no day for a window":  func(p *PackagePolicy) { p.Rules[0].WindowDays = nil },
		"a day that is not":    func(p *PackagePolicy) { p.Rules[0].WindowDays = []int{7} },
		"a start past 23:59":   func(p *PackagePolicy) { p.Rules[0].WindowStartMinute = 1440 },
		"a window too short":   func(p *PackagePolicy) { p.Rules[0].WindowMinutes = 10 },
		"a window too long":    func(p *PackagePolicy) { p.Rules[0].WindowMinutes = 1441 },
		"a tag with a space":   func(p *PackagePolicy) { p.Rules[0].Tag = "two words" },
		"a hold with a slash":  func(p *PackagePolicy) { p.Holds = []string{"../etc"} },
		"a hold with a space":  func(p *PackagePolicy) { p.Holds = []string{"a; rm -rf /"} },
	}
	for name, mutate := range bad {
		p := good()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPackageRuleFor(t *testing.T) {
	p := PackagePolicy{Rules: []PackageRule{
		{Tag: "db", Mode: PackagesOff},
		{Tag: "web", Mode: PackagesAll},
		{Tag: "", Mode: PackagesNotify},
	}}
	if got := p.ruleFor([]string{"Web", "db"}).Mode; got != PackagesOff {
		t.Errorf("the first rule in order wins: %s", got)
	}
	if got := p.ruleFor([]string{"web"}).Mode; got != PackagesAll {
		t.Errorf("web: %s", got)
	}
	if got := p.ruleFor(nil).Mode; got != PackagesNotify {
		t.Errorf("untagged: %s", got)
	}
	if got := (PackagePolicy{}).ruleFor(nil); got.Mode != PackagesNotify {
		t.Errorf("no rules at all should only tell: %+v", got)
	}
}

func TestPackageWanted(t *testing.T) {
	r := PackageReport{Updates: []PackageUpdate{
		{Name: "openssl", Security: true},
		{Name: "postgresql-16", Security: true},
		{Name: "curl.x86_64"},
		{Name: "linux-image-6.8.0-50", New: true, Security: true},
	}}
	holds := []string{"postgresql*", "curl"}
	if n := wanted(r, PackagesSecurity, holds); n != 1 {
		t.Errorf("security = %d, want 1 (held and new ones left out)", n)
	}
	if n := wanted(r, PackagesAll, holds); n != 1 {
		t.Errorf("all = %d, want 1 (curl.x86_64 is held by its base name)", n)
	}
	if n := wanted(r, PackagesAll, nil); n != 3 {
		t.Errorf("all without holds = %d", n)
	}
	z := PackageReport{Manager: "zypper", SecurityPatches: 2}
	if n := wanted(z, PackagesSecurity, nil); n != 2 {
		t.Errorf("zypper patches = %d", n)
	}
}

func TestPackageRunRequestValidate(t *testing.T) {
	ok := []runRequest{
		{Mode: "all"}, {Mode: "security", Reboot: "if_needed"}, {Mode: "selected", Packages: []string{"openssl", "curl.x86_64"}},
		{Mode: "reboot"}, {Mode: "reboot", Reboot: "never"},
	}
	for _, in := range ok {
		if err := in.validate(); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
	bad := []runRequest{
		{}, {Mode: "everything"}, {Mode: "selected"}, {Mode: "selected", Packages: []string{"-o APT::x"}},
		{Mode: "selected", Packages: []string{"a b"}}, {Mode: "all", Reboot: "always"},
	}
	for _, in := range bad {
		if err := in.validate(); err == nil {
			t.Errorf("%+v: accepted", in)
		}
	}
}

/* ── With a database ─────────────────────────────────────────────────────── */

type pkgEnv struct {
	s *Store
	h http.Handler
	a *API
	// second is another admin, for approvals.
	second string
}

func packagesAPI(t *testing.T) pkgEnv {
	t.Helper()
	a, s, h, _ := enrollAPI(t)
	second, _ := newAccount(t, s, "admin")
	operator, _ := newAccount(t, s, "operator")
	auditor, _ := newAccount(t, s, "auditor")
	a.UserTokens["second-token"] = second
	a.UserTokens["operator-token"] = operator
	a.UserTokens["auditor-token"] = auditor
	// The rules are the organization's: put them back as they were.
	before, err := s.PackagePolicy(context.Background())
	if err != nil {
		t.Fatalf("PackagePolicy: %v", err)
	}
	t.Cleanup(func() {
		if err := s.SavePackagePolicy(context.Background(), before); err != nil {
			t.Errorf("restoring the package policy: %v", err)
		}
	})
	return pkgEnv{s: s, h: h, a: a, second: second}
}

// pkgServer is an enrolled server with an agent, tagged.
func (e pkgEnv) server(t *testing.T, tags ...string) (Asset, string) {
	t.Helper()
	host := unique("pkg") + ".example"
	cred, _ := enrolled(t, e.s, e.h, host)
	asset, err := e.s.CreateAsset(context.Background(), AssetInput{
		Hostname: host, Address: "192.0.2.20", Principals: []string{"ops"}, CredentialRef: "injected", Tags: tags,
	})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM package_runs WHERE lower(hostname) = lower($1)`, host)
	})
	return asset, cred
}

// setRule adds a rule for tag, keeping the others.
func (e pkgEnv) setRule(t *testing.T, r PackageRule) {
	t.Helper()
	p, err := e.s.PackagePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Rules = append([]PackageRule{r}, p.Rules...)
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := e.s.SavePackagePolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func pkgReport(at time.Time, updates ...PackageUpdate) *PackageReport {
	if updates == nil {
		updates = []PackageUpdate{}
	}
	return &PackageReport{CheckedAt: at, Manager: "apt", Supported: true, Updates: updates}
}

// beat sends a heartbeat with a packages part (nil: an agent older than
// package updates) and returns the reply's packages.
func (e pkgEnv) beat(t *testing.T, cred, host string, b *PackageBeat) (map[string]any, *PackageOrders) {
	t.Helper()
	body := map[string]any{"hostname": host, "version": "1.2.0", "active_sessions": 0}
	if b != nil {
		body["packages"] = b
	}
	raw, _ := json.Marshal(body)
	w := do(t, e.h, http.MethodPost, "/api/v1/report/heartbeat", cred, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	all := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &all)
	p, ok := out["packages"]
	if !ok {
		return all, nil
	}
	var o PackageOrders
	if err := json.Unmarshal(p, &o); err != nil {
		t.Fatalf("packages: %v", err)
	}
	return all, &o
}

func (e pkgEnv) view(t *testing.T, asset Asset) PackagesView {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/packages", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("view: %d %s", w.Code, w.Body)
	}
	var v PackagesView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func (e pkgEnv) ask(t *testing.T, asset Asset, token, body string, want int) PackageRun {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/packages/runs", token, body)
	if w.Code != want {
		t.Fatalf("ask: %d %s, want %d", w.Code, w.Body, want)
	}
	var r PackageRun
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

var (
	openssl = PackageUpdate{Name: "openssl", Installed: "3.0.13-0ubuntu3.4", Available: "3.0.13-0ubuntu3.5",
		Security: true, Origin: "noble-security"}
	vim = PackageUpdate{Name: "vim", Installed: "2:9.1.0016-1ubuntu7.5", Available: "2:9.1.0016-1ubuntu7.6",
		Origin: "noble-updates"}
	pg = PackageUpdate{Name: "postgresql-16", Installed: "16.4-0ubuntu0.24.04.1", Available: "16.6-0ubuntu0.24.04.1",
		Security: true, Origin: "noble-security"}
)

// The whole of a run asked for by hand: seen, asked, sent until the agent
// answers, followed, finished and audited once.
func TestPackageRunByHand(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	now := time.Now().UTC()

	_, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(now, openssl, vim, pg)})
	if o == nil || !o.Check || o.Run != nil {
		t.Fatalf("orders = %+v", o)
	}
	v := e.view(t, asset)
	if !v.Enrolled || !v.Reported || !v.Supported || v.Report == nil || len(v.Report.Updates) != 3 ||
		v.Report.securityCount() != 2 || v.Rule.Mode == "" {
		t.Fatalf("view = %+v", v)
	}

	run := e.ask(t, asset, "admin-token", `{"mode":"security","reboot":"if_needed"}`, http.StatusAccepted)
	if run.State != RunQueued || run.Trigger != "manual" || run.Reboot != "if_needed" || len(run.Holds) == 0 {
		t.Fatalf("run = %+v", run)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_requested" ||
		!strings.Contains(a.Detail, "security updates") || !strings.Contains(a.Detail, "reboot if") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	// One at a time on a server.
	e.ask(t, asset, "admin-token", `{"mode":"all"}`, http.StatusConflict)

	_, o = e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true})
	if o == nil || o.Run == nil || o.Run.ID != run.ID || o.Run.Mode != "security" || o.Run.Reboot != "if_needed" ||
		!contains(o.Run.Holds, "postgresql*") {
		t.Fatalf("the reply should carry the run, holds and all: %+v", o)
	}
	// Once sent it cannot be taken back: the agent may be installing.
	w := do(t, e.h, http.MethodPost, "/api/v1/package-runs/"+run.ID+"/cancel", "admin-token", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "half way") {
		t.Errorf("cancel after sending: %d %s", w.Code, w.Body)
	}
	// The reply was lost (the connection dropped): the next one carries the
	// same run, and the agent starts it once.
	_, o = e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true})
	if o.Run == nil || o.Run.ID != run.ID {
		t.Fatalf("a run not yet answered should be sent again: %+v", o)
	}

	started := time.Now().UTC()
	running := &PackageRunStatus{ID: run.ID, State: "running", Step: "install", Message: "installing 1 package",
		Planned: 1, StartedAt: &started}
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Run: running})
	got := e.run(t, run.ID)
	if got.State != RunRunning || got.Step != "install" || got.StartedAt == nil || got.Planned != 1 {
		t.Fatalf("running = %+v", got)
	}

	finished := time.Now().UTC()
	done := &PackageRunStatus{ID: run.ID, State: "done", Step: "done", Message: "upgraded 1 package",
		Upgraded:  []PackageChange{{Name: "openssl", From: openssl.Installed, To: openssl.Available}},
		Restarted: []string{"nginx.service"}, Log: "Setting up openssl ...", StartedAt: &started, FinishedAt: &finished}
	_, o = e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Run: done,
		Report: pkgReport(finished, vim, pg)})
	if o.Run != nil {
		t.Fatalf("a finished run was sent again: %+v", o.Run)
	}
	got = e.run(t, run.ID)
	if got.State != RunDone || len(got.Upgraded) != 1 || got.Upgraded[0].To != openssl.Available ||
		!contains(got.Restarted, "nginx.service") || got.FinishedAt == nil || !strings.Contains(got.Log, "openssl") {
		t.Fatalf("done = %+v", got)
	}
	a := lastAuditOn(t, e.s, asset.Hostname)
	if a.Action != "packages.run_finished" || !strings.Contains(a.Detail, "upgraded 1 package") ||
		!strings.Contains(a.Detail, "nginx.service") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	// The agent repeats the finished run until it hears back: audited once.
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Run: done})
	if n := auditCount(t, e.s, "packages.run_finished", asset.Hostname); n != 1 {
		t.Errorf("run_finished audited %d times", n)
	}
	// And the next run may be asked.
	next := e.ask(t, asset, "admin-token", `{"mode":"selected","packages":["postgresql-16"]}`, http.StatusAccepted)
	if len(next.Holds) != 0 || len(next.Packages) != 1 {
		t.Errorf("a package named by hand is not held back: %+v", next)
	}
}

func (e pkgEnv) run(t *testing.T, id string) PackageRun {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/package-runs/"+id, "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	var r PackageRun
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

// A run taken back before any heartbeat carried it never reaches the
// server.
func TestPackageRunTakenBackBeforeItIsSent(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), vim)})
	run := e.ask(t, asset, "admin-token", `{"mode":"all"}`, http.StatusAccepted)
	w := do(t, e.h, http.MethodPost, "/api/v1/package-runs/"+run.ID+"/cancel", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if _, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true}); o.Run != nil {
		t.Fatalf("a cancelled run was sent: %+v", o.Run)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_cancelled" {
		t.Errorf("audit = %s", a.Action)
	}
	// A status for it now, from a confused agent, changes nothing.
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Run: &PackageRunStatus{ID: run.ID, State: "done"}})
	if got := e.run(t, run.ID); got.State != RunCancelled {
		t.Errorf("state = %s", got.State)
	}
}

// What cannot be asked, and who cannot ask.
func TestPackageRunRefusals(t *testing.T) {
	e := packagesAPI(t)

	// An agent older than package updates sends nothing and hears nothing.
	old, oldCred := e.server(t)
	if all, o := e.beat(t, oldCred, old.Hostname, nil); o != nil || all["packages"] != nil {
		t.Fatalf("an old agent was sent orders: %v", all)
	}
	if v := e.view(t, old); v.Reported || !v.Enrolled {
		t.Errorf("old agent view = %+v", v)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+old.ID+"/packages/runs", "admin-token", `{"mode":"all"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "too old") {
		t.Errorf("old agent: %d %s", w.Code, w.Body)
	}

	// A server the agent cannot update (not root, no systemd, no known
	// package manager) says why, and the console repeats it.
	cannot, cannotCred := e.server(t)
	e.beat(t, cannotCred, cannot.Hostname, &PackageBeat{Supported: false,
		Reason: "no apt, dnf, yum or zypper on this server"})
	w = do(t, e.h, http.MethodPost, "/api/v1/assets/"+cannot.ID+"/packages/runs", "admin-token", `{"mode":"all"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no apt, dnf") {
		t.Errorf("unsupported: %d %s", w.Code, w.Body)
	}

	ok, cred := e.server(t)
	e.beat(t, cred, ok.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), vim)})
	for name, body := range map[string]string{
		"no mode":            `{}`,
		"a mode that is not": `{"mode":"everything"}`,
		"nothing picked":     `{"mode":"selected","packages":[]}`,
		"an option smuggled": `{"mode":"selected","packages":["--allow-downgrades"]}`,
		"an unknown field":   `{"mode":"all","force":true}`,
	} {
		if w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+ok.ID+"/packages/runs", "admin-token", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	for _, token := range []string{"operator-token", "auditor-token"} {
		if w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+ok.ID+"/packages/runs", token, `{"mode":"all"}`); w.Code != http.StatusForbidden {
			t.Errorf("%s may not install: %d", token, w.Code)
		}
		if w := do(t, e.h, http.MethodPut, "/api/v1/settings/package-updates", token, `{"rules":[],"holds":[]}`); w.Code != http.StatusForbidden {
			t.Errorf("%s may not change the rules: %d", token, w.Code)
		}
	}
	if w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+ok.ID+"/packages", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator reads the fleet's updates: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+ok.ID+"/packages", "auditor-token", ""); w.Code != http.StatusOK {
		t.Errorf("an auditor should read them: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodGet, "/api/v1/package-runs/00000000-0000-0000-0000-000000000000", "admin-token", ""); w.Code != http.StatusNotFound {
		t.Errorf("a run that does not exist: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+ok.ID+"/packages/runs", "", `{"mode":"all"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d", w.Code)
	}

	// Off for its rule: nothing is looked for, and a look cannot be asked.
	tag := unique("off")
	off, offCred := e.server(t, tag)
	e.setRule(t, PackageRule{Tag: tag, Mode: PackagesOff, WindowMinutes: 60})
	if _, o := e.beat(t, offCred, off.Hostname, &PackageBeat{Supported: true}); o == nil || o.Check {
		t.Errorf("a server whose rule is off was told to look: %+v", o)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+off.ID+"/packages/check", "admin-token", ""); w.Code != http.StatusConflict {
		t.Errorf("check with updates off: %d %s", w.Code, w.Body)
	}
}

// "Look now" rides the reply until the agent says it looked.
func TestPackageCheckNow(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true})
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/packages/check", "admin-token", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	var out struct{ CheckID string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	// Asking twice before it answers is the same look.
	w = do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/packages/check", "admin-token", "")
	var again struct{ CheckID string }
	_ = json.Unmarshal(w.Body.Bytes(), &again)
	if out.CheckID == "" || again.CheckID != out.CheckID {
		t.Fatalf("check ids %q %q", out.CheckID, again.CheckID)
	}
	if !e.view(t, asset).CheckAsked {
		t.Error("the view should say a look was asked for")
	}
	_, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Checking: true})
	if o.CheckID != out.CheckID {
		t.Fatalf("orders = %+v", o)
	}
	_, o = e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, CheckID: out.CheckID,
		Report: pkgReport(time.Now(), openssl)})
	if o.CheckID != "" {
		t.Errorf("an answered look was asked again: %+v", o)
	}
	if v := e.view(t, asset); v.CheckAsked || v.Report == nil || len(v.Report.Updates) != 1 {
		t.Errorf("view = %+v", v)
	}
}

// A rule can make runs asked by hand wait for a second admin.
func TestPackageRunNeedsApproval(t *testing.T) {
	e := packagesAPI(t)
	tag := unique("prod")
	asset, cred := e.server(t, tag)
	e.setRule(t, PackageRule{Tag: tag, Mode: PackagesNotify, WindowMinutes: 60, NeedsApproval: true})
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), openssl)})

	run := e.ask(t, asset, "admin-token", `{"mode":"all"}`, http.StatusAccepted)
	if run.State != RunAwaitingApproval {
		t.Fatalf("state = %s", run.State)
	}
	if !strings.Contains(lastAuditOn(t, e.s, asset.Hostname).Detail, "waits for another admin") {
		t.Error("the audit should say it waits")
	}
	if _, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true}); o.Run != nil {
		t.Fatal("a run waiting for approval was sent")
	}
	// Not by the one who asked, and not by an operator.
	if w := do(t, e.h, http.MethodPost, "/api/v1/package-runs/"+run.ID+"/approve", "admin-token", ""); w.Code != http.StatusConflict {
		t.Errorf("self-approval: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/package-runs/"+run.ID+"/approve", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("operator approval: %d", w.Code)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/package-runs/"+run.ID+"/approve", "second-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	got := e.run(t, run.ID)
	if got.State != RunQueued || got.DecidedBy == nil || !strings.EqualFold(*got.DecidedBy, e.second) {
		t.Fatalf("approved = %+v", got)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_approved" || !strings.EqualFold(a.ActorEmail, e.second) {
		t.Errorf("audit = %s by %s", a.Action, a.ActorEmail)
	}
	// Twice is refused.
	if w := do(t, e.h, http.MethodPost, "/api/v1/package-runs/"+run.ID+"/deny", "second-token", ""); w.Code != http.StatusConflict {
		t.Errorf("deciding twice: %d", w.Code)
	}
	if _, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true}); o.Run == nil || o.Run.ID != run.ID {
		t.Fatal("an approved run should be sent")
	}
}

func TestPackageRunDenied(t *testing.T) {
	e := packagesAPI(t)
	tag := unique("prod")
	asset, cred := e.server(t, tag)
	e.setRule(t, PackageRule{Tag: tag, Mode: PackagesNotify, WindowMinutes: 60, NeedsApproval: true})
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), openssl)})
	run := e.ask(t, asset, "admin-token", `{"mode":"all"}`, http.StatusAccepted)
	if w := do(t, e.h, http.MethodPost, "/api/v1/package-runs/"+run.ID+"/deny", "second-token", ""); w.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if got := e.run(t, run.ID); got.State != RunDenied || got.FinishedAt == nil {
		t.Fatalf("denied = %+v", got)
	}
	if _, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true}); o.Run != nil {
		t.Fatal("a denied run was sent")
	}
	// The server is free for another.
	e.ask(t, asset, "admin-token", `{"mode":"security"}`, http.StatusAccepted)
}

// Several servers go one at a time, and one that fails stops the rest.
func TestPackageBatchStopsAtAFailure(t *testing.T) {
	e := packagesAPI(t)
	a1, c1 := e.server(t)
	a2, c2 := e.server(t)
	a3, c3 := e.server(t)
	for _, s := range []struct {
		a Asset
		c string
	}{{a1, c1}, {a2, c2}, {a3, c3}} {
		e.beat(t, s.c, s.a.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), openssl)})
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/package-runs", "admin-token",
		`{"assetIds":["`+a1.ID+`","`+a2.ID+`","`+a3.ID+`"],"mode":"security"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch: %d %s", w.Code, w.Body)
	}
	var runs []PackageRun
	_ = json.Unmarshal(w.Body.Bytes(), &runs)
	if len(runs) != 3 || runs[0].BatchID == nil || runs[0].AssetHostname != a1.Hostname ||
		runs[2].AssetHostname != a3.Hostname {
		t.Fatalf("runs = %+v", runs)
	}
	if got := e.run(t, runs[1].ID); got.State != RunWaiting {
		t.Fatalf("the second server should wait its turn: %s", got.State)
	}
	if _, o := e.beat(t, c2, a2.Hostname, &PackageBeat{Supported: true}); o.Run != nil {
		t.Fatal("the second server was sent its run before the first finished")
	}
	_, o := e.beat(t, c1, a1.Hostname, &PackageBeat{Supported: true})
	if o.Run == nil || o.Run.ID != runs[0].ID {
		t.Fatal("the first server should be sent its run")
	}
	e.beat(t, c1, a1.Hostname, &PackageBeat{Supported: true, Run: &PackageRunStatus{ID: runs[0].ID,
		State: "failed", Message: "E: Unable to locate package openssl"}})
	if a := lastAuditOn(t, e.s, a1.Hostname); a.Action != "packages.run_failed" || a.Severity != "warn" {
		t.Errorf("audit = %s/%s", a.Action, a.Severity)
	}
	for _, r := range runs[1:] {
		got := e.run(t, r.ID)
		if got.State != RunCancelled || !strings.Contains(got.Message, a1.Hostname) {
			t.Errorf("%s after a failure: %s %q", got.AssetHostname, got.State, got.Message)
		}
	}
	if _, o := e.beat(t, c3, a3.Hostname, &PackageBeat{Supported: true}); o.Run != nil {
		t.Fatal("a server after the failure was sent its run")
	}
}

func TestPackageBatchMovesOnWhenOneIsDone(t *testing.T) {
	e := packagesAPI(t)
	a1, c1 := e.server(t)
	a2, c2 := e.server(t)
	e.beat(t, c1, a1.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), vim)})
	e.beat(t, c2, a2.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), vim)})
	w := do(t, e.h, http.MethodPost, "/api/v1/package-runs", "admin-token",
		`{"assetIds":["`+a1.ID+`","`+a2.ID+`"],"mode":"all"}`)
	var runs []PackageRun
	_ = json.Unmarshal(w.Body.Bytes(), &runs)
	if len(runs) != 2 {
		t.Fatalf("batch: %d %s", w.Code, w.Body)
	}
	e.beat(t, c1, a1.Hostname, &PackageBeat{Supported: true})
	e.beat(t, c1, a1.Hostname, &PackageBeat{Supported: true, Run: &PackageRunStatus{ID: runs[0].ID, State: "done"}})
	_, o := e.beat(t, c2, a2.Hostname, &PackageBeat{Supported: true})
	if o.Run == nil || o.Run.ID != runs[1].ID {
		t.Fatalf("the second server should go once the first is done: %+v", o)
	}
	// A server already busy refuses the whole batch, and nothing is made.
	w = do(t, e.h, http.MethodPost, "/api/v1/package-runs", "admin-token",
		`{"assetIds":["`+a1.ID+`","`+a2.ID+`"],"mode":"all"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), a2.Hostname) {
		t.Errorf("a busy server: %d %s", w.Code, w.Body)
	}
	if runs, _ := e.s.PackageRunsFor(context.Background(), a1.Hostname, 10); len(runs) != 1 {
		t.Errorf("a refused batch left %d runs on the first server", len(runs))
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/package-runs", "admin-token", `{"assetIds":[],"mode":"all"}`); w.Code != http.StatusBadRequest {
		t.Errorf("no servers: %d", w.Code)
	}
}

// A rule that installs on its own starts one run in its window, for what
// it installs, and no more.
func TestPackageAutomaticRunInTheWindow(t *testing.T) {
	e := packagesAPI(t)
	tag := unique("auto")
	asset, cred := e.server(t, tag)
	now := time.Now().UTC()
	// A window open now, by a clock seven hours ahead of UTC.
	local := now.Add(7 * time.Hour)
	startMin := local.Hour()*60 + local.Minute() - 10
	day := int(local.Weekday())
	if startMin < 0 {
		startMin += 1440
		day = (day + 6) % 7
	}
	e.setRule(t, PackageRule{Tag: tag, Mode: PackagesSecurity, WindowDays: []int{day},
		WindowStartMinute: startMin, WindowMinutes: 60, Reboot: "if_needed"})
	rep := pkgReport(now, vim, pg) // nothing the rule installs: pg is held
	rep.UTCOffsetMinutes = 7 * 60

	if _, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: rep}); o.Run != nil {
		t.Fatalf("a run started for nothing but held and ordinary updates: %+v", o.Run)
	}
	rep = pkgReport(now.Add(time.Second), vim, pg, openssl)
	rep.UTCOffsetMinutes = 7 * 60
	_, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: rep})
	if o.Run == nil || o.Run.Mode != "security" || o.Run.Reboot != "if_needed" || !contains(o.Run.Holds, "postgresql*") {
		t.Fatalf("orders = %+v", o)
	}
	run := e.run(t, o.Run.ID)
	if run.Trigger != "automatic" || run.RequestedBy != "" {
		t.Errorf("run = %+v", run)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_started" || !strings.Contains(a.Detail, "maintenance window") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	// It fails. The window does not try again: an unattended retry loop
	// on a broken server is worse than waiting a week.
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Run: &PackageRunStatus{ID: run.ID, State: "failed",
		Message: "dpkg was interrupted"}})
	for i := 0; i < 2; i++ {
		if _, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true}); o.Run != nil {
			t.Fatalf("the same window started a second run: %+v", o.Run)
		}
	}
	runs, _ := e.s.PackageRunsFor(context.Background(), asset.Hostname, 10)
	if len(runs) != 1 {
		t.Errorf("runs = %d", len(runs))
	}
}

func TestPackageNoAutomaticRunOutsideTheWindow(t *testing.T) {
	e := packagesAPI(t)
	tag := unique("auto")
	asset, cred := e.server(t, tag)
	now := time.Now().UTC()
	// Tomorrow's weekday only.
	e.setRule(t, PackageRule{Tag: tag, Mode: PackagesAll, WindowDays: []int{(int(now.Weekday()) + 2) % 7},
		WindowStartMinute: 0, WindowMinutes: 30})
	_, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(now, openssl, vim)})
	if o.Run != nil {
		t.Fatalf("a run started outside its window: %+v", o.Run)
	}
	if v := e.view(t, asset); v.NextWindow == nil || !v.NextWindow.After(now) {
		t.Errorf("next window = %v", v.NextWindow)
	}
}

// The server goes quiet in the middle of a run: power cut, network gone,
// agent removed. After three hours the run is called interrupted, so the
// server is free; if the agent comes back and says how it really ended,
// that is kept.
func TestPackageRunOnAServerThatWentQuiet(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), openssl)})
	run := e.ask(t, asset, "admin-token", `{"mode":"all","reboot":"if_needed"}`, http.StatusAccepted)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true})
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Run: &PackageRunStatus{ID: run.ID,
		State: "rebooting", Step: "reboot", Message: "rebooting to finish"}})
	if got := e.run(t, run.ID); got.State != RunRebooting {
		t.Fatalf("state = %s", got.State)
	}
	// Two hours of silence is still a reboot taking its time.
	_, _ = e.s.pool.Exec(context.Background(), `UPDATE package_runs SET updated_at = now() - interval '2 hours' WHERE id = $1`, run.ID)
	if v := e.view(t, asset); len(v.Runs) == 0 || v.Runs[0].State != RunRebooting {
		t.Fatalf("expired too soon: %+v", v.Runs)
	}
	_, _ = e.s.pool.Exec(context.Background(), `UPDATE package_runs SET updated_at = now() - interval '4 hours' WHERE id = $1`, run.ID)
	v := e.view(t, asset)
	if len(v.Runs) == 0 || v.Runs[0].State != RunInterrupted || !strings.Contains(v.Runs[0].Message, "stopped reporting") {
		t.Fatalf("after four quiet hours: %+v", v.Runs)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_interrupted" {
		t.Errorf("audit = %s", a.Action)
	}
	// The server is back and says it rebooted and finished.
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Run: &PackageRunStatus{ID: run.ID,
		State: "done", Rebooted: true, Message: "upgraded 1 package; rebooted"}})
	got := e.run(t, run.ID)
	if got.State != RunDone || !got.Rebooted {
		t.Fatalf("a late report should be kept: %+v", got)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_finished" || !strings.Contains(a.Detail, "rebooted") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
}

// The agent settles a run its helper never finished (killed, or the power
// went) and says so; the console alerts on it.
func TestPackageRunInterruptedByTheServer(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), openssl)})
	run := e.ask(t, asset, "admin-token", `{"mode":"all"}`, http.StatusAccepted)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true})
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Run: &PackageRunStatus{ID: run.ID,
		State: "interrupted", Message: "the server restarted before the update finished; the package manager was repaired"}})
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_interrupted" || !strings.Contains(a.Detail, "repaired") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	var n int
	_ = e.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM console_alerts
		WHERE title = $1`, "Updating "+asset.Hostname+" was interrupted").Scan(&n)
	if n != 1 {
		t.Errorf("alerts = %d", n)
	}
	// Free for another try.
	e.ask(t, asset, "admin-token", `{"mode":"all"}`, http.StatusAccepted)
}

// More security updates than at the last look is news; the first look is
// not, nor is the same count again.
func TestPackageSecurityAlert(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	count := func() int {
		var n int
		_ = e.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM console_alerts WHERE title LIKE $1`,
			asset.Hostname+" has % security update%").Scan(&n)
		return n
	}
	now := time.Now().UTC()
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(now, openssl)})
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(now.Add(time.Minute), openssl)})
	if n := count(); n != 0 {
		t.Fatalf("alerts before anything new = %d", n)
	}
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(now.Add(2*time.Minute), openssl, pg)})
	if n := count(); n != 1 {
		t.Errorf("alerts after a new one = %d", n)
	}
}

func TestPackagePolicyRoute(t *testing.T) {
	e := packagesAPI(t)
	w := do(t, e.h, http.MethodGet, "/api/v1/settings/package-updates", "admin-token", "")
	var p PackagePolicy
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != http.StatusOK || len(p.Rules) == 0 {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	tag := unique("web")
	body := `{"rules":[{"tag":"` + tag + `","mode":"all","windowDays":[6],"windowStartMinute":60,"windowMinutes":90,` +
		`"reboot":"if_needed","needsApproval":true},{"tag":"","mode":"notify","windowDays":[],"windowStartMinute":120,` +
		`"windowMinutes":120,"reboot":"never"}],"holds":["mysql-server*"]}`
	w = do(t, e.h, http.MethodPut, "/api/v1/settings/package-updates", "admin-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if len(p.Rules) != 2 || p.Rules[0].Tag != tag || !p.Rules[0].NeedsApproval || len(p.Holds) != 1 {
		t.Fatalf("saved = %+v", p)
	}
	a := lastAuditOn(t, e.s, "package updates")
	if a.Action != "packages.policy_changed" || !strings.Contains(a.Detail, "tag "+tag+": all Sat 01:00") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	for name, bad := range map[string]string{
		"no default": `{"rules":[{"tag":"x","mode":"notify","windowMinutes":60}],"holds":[]}`,
		"bad hold":   `{"rules":[{"tag":"","mode":"notify","windowMinutes":60}],"holds":["$(reboot)"]}`,
		"unknown":    `{"rules":[],"holds":[],"extra":1}`,
	} {
		if w := do(t, e.h, http.MethodPut, "/api/v1/settings/package-updates", "admin-token", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestPackageFleet(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: pkgReport(time.Now(), openssl, vim)})
	e.ask(t, asset, "admin-token", `{"mode":"security"}`, http.StatusAccepted)
	w := do(t, e.h, http.MethodGet, "/api/v1/package-updates", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("fleet: %d %s", w.Code, w.Body)
	}
	var rows []PackageFleetRow
	_ = json.Unmarshal(w.Body.Bytes(), &rows)
	for _, r := range rows {
		if r.AssetID == asset.ID {
			if !r.Supported || r.Updates != 2 || r.Security != 1 || r.Manager != "apt" || r.Run == nil ||
				r.Run.State != RunQueued {
				t.Errorf("row = %+v", r)
			}
			return
		}
	}
	t.Fatalf("the server is missing from the fleet (%d rows)", len(rows))
}

// A report is kept within bounds whatever an agent sends.
func TestPackageReportBounded(t *testing.T) {
	r := PackageReport{UTCOffsetMinutes: 24 * 60, Error: strings.Repeat("x", 5000)}
	for i := 0; i < maxPackageUpdates+10; i++ {
		r.Updates = append(r.Updates, PackageUpdate{Name: strings.Repeat("n", 300)})
	}
	r.bounded()
	if len(r.Updates) != maxPackageUpdates || len(r.Updates[0].Name) != 200 || len(r.Error) != 1000 ||
		r.UTCOffsetMinutes != 0 {
		t.Errorf("bounded: %d updates, name %d, error %d, offset %d", len(r.Updates), len(r.Updates[0].Name),
			len(r.Error), r.UTCOffsetMinutes)
	}
}

// A reboot asked for on its own, start to finish: offered only by an agent
// that can do one, sent as a reboot whatever was said about rebooting,
// audited as a reboot, and the "needs a reboot" alert closed once it is
// done.
func TestPackageRebootByHand(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	now := time.Now().UTC()
	alerts := func(open bool) int {
		var n int
		_ = e.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM console_alerts
			WHERE title = $1 AND (closed_at IS NULL) = $2`, asset.Hostname+" needs a reboot", open).Scan(&n)
		return n
	}

	// An agent that does not say it can reboot is not asked to.
	needs := pkgReport(now, vim)
	needs.RebootRequired = true
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Report: needs})
	if v := e.view(t, asset); v.CanReboot {
		t.Fatalf("an older agent was taken to reboot: %+v", v)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/packages/runs", "admin-token", `{"mode":"reboot"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "too old to reboot") {
		t.Fatalf("reboot on an older agent: %d %s", w.Code, w.Body)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/package-runs", "admin-token",
		`{"assetIds":["`+asset.ID+`"],"mode":"reboot"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "too old to reboot") {
		t.Fatalf("fleet reboot on an older agent: %d %s", w.Code, w.Body)
	}

	// An update run that left the server needing one raises the alert.
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true})
	if v := e.view(t, asset); !v.CanReboot {
		t.Fatalf("view = %+v", v)
	}
	up := e.ask(t, asset, "admin-token", `{"mode":"all"}`, http.StatusAccepted)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true})
	fin := time.Now().UTC()
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, Run: &PackageRunStatus{ID: up.ID,
		State: "done", Upgraded: []PackageChange{{Name: "vim", To: vim.Available}}, RebootRequired: true,
		StartedAt: &fin, FinishedAt: &fin}})
	if n := alerts(true); n != 1 {
		t.Fatalf("open reboot alerts = %d", n)
	}

	// Operators and auditors cannot reboot.
	e.ask(t, asset, "operator-token", `{"mode":"reboot"}`, http.StatusForbidden)
	e.ask(t, asset, "auditor-token", `{"mode":"reboot"}`, http.StatusForbidden)

	run := e.ask(t, asset, "admin-token", `{"mode":"reboot","reboot":"never","packages":["vim"]}`, http.StatusAccepted)
	if run.Mode != PackagesReboot || run.Reboot != "if_needed" || len(run.Packages) != 0 {
		t.Fatalf("run = %+v", run)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_requested" ||
		a.Detail != "Asked to reboot "+asset.Hostname+"." {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	// One at a time, as for any run.
	e.ask(t, asset, "admin-token", `{"mode":"reboot"}`, http.StatusConflict)
	_, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true})
	if o == nil || o.Run == nil || o.Run.ID != run.ID || o.Run.Mode != PackagesReboot {
		t.Fatalf("orders = %+v", o)
	}

	started := time.Now().UTC()
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, Run: &PackageRunStatus{ID: run.ID,
		State: "rebooting", Step: "reboot", Message: "rebooting, as asked from the console", StartedAt: &started}})
	if got := e.run(t, run.ID); got.State != RunRebooting {
		t.Fatalf("rebooting = %+v", got)
	}
	back := time.Now().UTC()
	after := pkgReport(back, vim)
	_, o = e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, Report: after,
		Run: &PackageRunStatus{ID: run.ID, State: "done", Rebooted: true, Message: "the server rebooted",
			StartedAt: &started, FinishedAt: &back}})
	if o.Run != nil {
		t.Fatalf("a finished reboot was sent again: %+v", o.Run)
	}
	if got := e.run(t, run.ID); got.State != RunDone || !got.Rebooted {
		t.Fatalf("done = %+v", got)
	}
	a := lastAuditOn(t, e.s, asset.Hostname)
	if a.Action != "packages.run_finished" || !strings.HasPrefix(a.Detail, "The reboot of "+asset.Hostname) ||
		strings.Contains(a.Detail, "package") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	if n := alerts(true); n != 0 {
		t.Errorf("the reboot alert is still open after the reboot (%d)", n)
	}
	if v := e.view(t, asset); v.Report == nil || v.Report.RebootRequired {
		t.Errorf("the server still shows a reboot needed: %+v", v.Report)
	}
}

// A reboot that failed is said plainly, and the server still needs one.
func TestPackageRebootFails(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, Report: pkgReport(time.Now())})
	run := e.ask(t, asset, "admin-token", `{"mode":"reboot"}`, http.StatusAccepted)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true})
	now := time.Now().UTC()
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, Run: &PackageRunStatus{ID: run.ID,
		State: "failed", Message: "the reboot failed: Failed to connect to bus", RebootRequired: true,
		StartedAt: &now, FinishedAt: &now}})
	if got := e.run(t, run.ID); got.State != RunFailed || !got.RebootRequired {
		t.Fatalf("run = %+v", got)
	}
	var title string
	_ = e.s.pool.QueryRow(context.Background(), `SELECT title FROM console_alerts WHERE title LIKE $1
		ORDER BY seq DESC LIMIT 1`, "Rebooting "+asset.Hostname+"%").Scan(&title)
	if title != "Rebooting "+asset.Hostname+" failed" {
		t.Errorf("alert = %q", title)
	}
	// Supported false (the agent lost root) means no reboot either, even
	// if it says it can.
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: false, Reason: "not root", Reboot: true})
	if v := e.view(t, asset); v.CanReboot {
		t.Errorf("a server that cannot be updated offers a reboot")
	}
}

// A release upgrade, start to finish: offered only by an agent that can do
// one and only for the release the server offers, refused while the server
// is not ready, sent with the release a person said yes to, never for a
// fleet, and audited as a release upgrade.
func TestPackageReleaseUpgrade(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	now := time.Now().UTC()
	jammy := func(blockers ...string) *PackageReport {
		r := pkgReport(now)
		r.Release = &ReleaseInfo{Distro: "ubuntu", Current: "Ubuntu 22.04.5 LTS", Version: "22.04", Next: "24.04",
			NextName: "Ubuntu 24.04.1 LTS", Tool: "do-release-upgrade", Blockers: blockers}
		return r
	}

	// An agent that does not say it can is not asked to.
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, Report: jammy()})
	if v := e.view(t, asset); v.CanUpgradeRelease || v.Report == nil || v.Report.Release == nil ||
		v.Report.Release.Next != "24.04" {
		t.Fatalf("view = %+v", v)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/packages/runs", "admin-token",
		`{"mode":"release_upgrade","release":"24.04"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "too old to upgrade") {
		t.Fatalf("older agent: %d %s", w.Code, w.Body)
	}

	// Not ready: the agent's own reasons, word for word.
	now = now.Add(time.Second)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, ReleaseUpgrade: true,
		Report: jammy("reboot it first: an update already installed waits for a reboot")})
	w = do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/packages/runs", "admin-token",
		`{"mode":"release_upgrade","release":"24.04"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "reboot it first") {
		t.Fatalf("not ready: %d %s", w.Code, w.Body)
	}

	now = now.Add(time.Second)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, ReleaseUpgrade: true, Report: jammy()})
	// A release the server does not offer, or none at all.
	w = do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/packages/runs", "admin-token",
		`{"mode":"release_upgrade","release":"24.10"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "offers 24.04, not 24.10") {
		t.Fatalf("other release: %d %s", w.Code, w.Body)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/packages/runs", "admin-token",
		`{"mode":"release_upgrade"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no release: %d %s", w.Code, w.Body)
	}
	// Never several servers at once.
	w = do(t, e.h, http.MethodPost, "/api/v1/package-runs", "admin-token",
		`{"assetIds":["`+asset.ID+`"],"mode":"release_upgrade"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "one server at a time") {
		t.Fatalf("fleet: %d %s", w.Code, w.Body)
	}
	e.ask(t, asset, "operator-token", `{"mode":"release_upgrade","release":"24.04"}`, http.StatusForbidden)

	run := e.ask(t, asset, "admin-token", `{"mode":"release_upgrade","release":"24.04","reboot":"never","packages":["vim"]}`,
		http.StatusAccepted)
	if run.Mode != PackagesRelease || run.Release != "24.04" || run.Reboot != "if_needed" || len(run.Packages) != 0 ||
		len(run.Holds) != 0 {
		t.Fatalf("run = %+v", run)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "packages.run_requested" ||
		a.Detail != "Asked to upgrade "+asset.Hostname+" to release 24.04 and reboot into it." {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	_, o := e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, ReleaseUpgrade: true})
	if o == nil || o.Run == nil || o.Run.ID != run.ID || o.Run.Mode != PackagesRelease || o.Run.Expect != "24.04" {
		t.Fatalf("orders = %+v", o.Run)
	}

	started := time.Now().UTC()
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, ReleaseUpgrade: true,
		Run: &PackageRunStatus{ID: run.ID, State: "rebooting", Step: "reboot",
			Message: "upgraded to Ubuntu 24.04.1 LTS; rebooting into it", StartedAt: &started}})
	back := time.Now().UTC()
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, Reboot: true, ReleaseUpgrade: true,
		Run: &PackageRunStatus{ID: run.ID, State: "done", Rebooted: true, StartedAt: &started, FinishedAt: &back,
			Message: "upgraded from Ubuntu 22.04.5 LTS to Ubuntu 24.04.1 LTS, and the server rebooted into it"}})
	got := e.run(t, run.ID)
	if got.State != RunDone || !got.Rebooted || got.Release != "24.04" {
		t.Fatalf("done = %+v", got)
	}
	a := lastAuditOn(t, e.s, asset.Hostname)
	if a.Action != "packages.run_finished" ||
		!strings.HasPrefix(a.Detail, "The release upgrade of "+asset.Hostname+" to 24.04 (asked by ") ||
		!strings.Contains(a.Detail, "to Ubuntu 24.04.1 LTS") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
}

// A release upgrade that did not take is an alert that says so.
func TestPackageReleaseUpgradeFails(t *testing.T) {
	e := packagesAPI(t)
	asset, cred := e.server(t)
	r := pkgReport(time.Now().UTC())
	r.Release = &ReleaseInfo{Distro: "debian", Current: "Debian GNU/Linux 12 (bookworm)", Version: "12", Next: "13",
		NextName: "Debian 13 (trixie)", Tool: "apt"}
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, ReleaseUpgrade: true, Report: r})
	run := e.ask(t, asset, "admin-token", `{"mode":"release_upgrade","release":"13"}`, http.StatusAccepted)
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, ReleaseUpgrade: true})
	now := time.Now().UTC()
	e.beat(t, cred, asset.Hostname, &PackageBeat{Supported: true, ReleaseUpgrade: true, Run: &PackageRunStatus{
		ID: run.ID, State: "failed", StartedAt: &now, FinishedAt: &now,
		Message: "the server rebooted, but it is still on 12: the upgrade to Debian 13 (trixie) did not happen"}})
	var title string
	_ = e.s.pool.QueryRow(context.Background(), `SELECT title FROM console_alerts WHERE title LIKE $1
		ORDER BY seq DESC LIMIT 1`, "Upgrading the release of "+asset.Hostname+"%").Scan(&title)
	if title != "Upgrading the release of "+asset.Hostname+" failed" {
		t.Errorf("alert = %q", title)
	}
}

func TestPackageReleaseRequestValidate(t *testing.T) {
	for _, in := range []runRequest{{Mode: "release_upgrade", Release: "24.04"}, {Mode: "release_upgrade", Release: "9"}} {
		if err := in.validate(); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
	for _, in := range []runRequest{{Mode: "release_upgrade"}, {Mode: "release_upgrade", Release: "noble"},
		{Mode: "release_upgrade", Release: "24.04 && reboot"}} {
		if err := in.validate(); err == nil {
			t.Errorf("%+v was taken", in)
		}
	}
	in := runRequest{Mode: "all", Release: "24.04"}
	if err := in.validate(); err != nil || in.Release != "" {
		t.Errorf("an update run kept a release: %+v %v", in, err)
	}
}

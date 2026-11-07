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
Cleaning log files from the console: what the agent reports, what a
cleanup may be planned from, approvals, the typed confirm, automatic
cleanups when a disk fills, and the audit of what was removed.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Log
*/

/* ── Without a database ──────────────────────────────────────────────────── */

func TestLogCleanupPolicyValidate(t *testing.T) {
	good := func() LogCleanupPolicy {
		r := defaultLogRule()
		web := defaultLogRule()
		web.Tag, web.Mode, web.ActiveOverMB = "web", LogCleanDisk, 2048
		return LogCleanupPolicy{Rules: []LogCleanupRule{web, r},
			Paths: []string{" /srv/*/logs/*.log ", "", "/srv/*/logs/*.log"}, Protect: []string{"/var/log/pgbackrest/*"}}
	}
	p := good()
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(p.Paths) != 1 || p.Rules[1].Position != 1 {
		t.Errorf("policy = %+v", p)
	}
	bad := map[string]func(*LogCleanupPolicy){
		"no default rule":          func(p *LogCleanupPolicy) { p.Rules = p.Rules[:1] },
		"a mode that is not":       func(p *LogCleanupPolicy) { p.Rules[0].Mode = "always" },
		"a disk mark too low":      func(p *LogCleanupPolicy) { p.Rules[0].DiskPercent = 10 },
		"less than a week journal": func(p *LogCleanupPolicy) { p.Rules[0].JournalKeepDays = 3 },
		"a tiny journal":           func(p *LogCleanupPolicy) { p.Rules[0].JournalMaxMB = 10 },
		"live logs over 5 MB":      func(p *LogCleanupPolicy) { p.Rules[0].ActiveOverMB = 5 },
		"keeping a gigabyte":       func(p *LogCleanupPolicy) { p.Rules[0].ActiveKeepMB = 1024 },
		"cleaning /etc":            func(p *LogCleanupPolicy) { p.Paths = []string{"/etc/log/*"} },
		"cleaning a database":      func(p *LogCleanupPolicy) { p.Paths = []string{"/srv/db/data/*"} },
		"a relative path":          func(p *LogCleanupPolicy) { p.Paths = []string{"logs/*"} },
		"going up":                 func(p *LogCleanupPolicy) { p.Paths = []string{"/srv/logs/../../etc/*"} },
		"the whole disk":           func(p *LogCleanupPolicy) { p.Paths = []string{"/log*"} },
		"windows system32":         func(p *LogCleanupPolicy) { p.Paths = []string{`C:\Windows\System32\LogFiles\*`} },
	}
	for name, mutate := range bad {
		p := good()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLogRuleWindow(t *testing.T) {
	r := defaultLogRule() // 03:00 for two hours
	day := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	if _, in := r.inWindow(day.Add(2*time.Hour), 0); in {
		t.Error("02:00 is before the window")
	}
	if d, in := r.inWindow(day.Add(4*time.Hour), 0); !in || d != "2026-11-07" {
		t.Errorf("04:00: %s %v", d, in)
	}
	// 03:30 in Jakarta (UTC+7) is 20:30 the day before in UTC.
	if d, in := r.inWindow(day.Add(-3*time.Hour-30*time.Minute), 7*60); !in || d != "2026-11-07" {
		t.Errorf("by the server's clock: %s %v", d, in)
	}
}

func logTestReport(now time.Time) LogReport {
	old := func(days int) time.Time { return now.Add(-time.Duration(days) * 24 * time.Hour) }
	o20, o2 := old(20), old(2)
	return LogReport{ScannedAt: now, OS: "linux", Supported: true,
		Disks: []LogDisk{{Mount: "/", Total: 100 << 30, Used: 93 << 30, Avail: 7 << 30}},
		Items: []LogItem{
			{Key: "journal", Kind: "journal", Title: "Journal", Root: "/var/log/journal", Bytes: 4 << 30},
			{Key: "rotated", Kind: "rotated", Title: "Rotated logs", Root: "/var/log", Bytes: 3000, Count: 2,
				Oldest: &o20, Files: []LogFile{
					{Path: "/var/log/syslog.2.gz", Bytes: 2000, Modified: o20},
					{Path: "/var/log/syslog.1", Bytes: 1000, Modified: o2}}},
			{Key: "active:/var/log/nginx/access.log", Kind: "active", Title: "nginx/access.log",
				Root: "/var/log/nginx/access.log", Bytes: 3 << 30},
			{Key: "dumps", Kind: "files", Title: "Crash dumps", Root: `C:\Windows\Minidump`, Bytes: 500,
				Files: []LogFile{{Path: `C:\Windows\Minidump\a.dmp`, Bytes: 500, Modified: o20}}},
		},
		Open: []OpenDeleted{{PID: 2381, FD: 7, Process: "java", Unit: "tomcat.service",
			Path: "/opt/tomcat/logs/catalina.out", Bytes: 2 << 30}},
		Protected: []ProtectedLog{}, UTCOffsetMinutes: 0}
}

func TestPlanLogCleanupUsesOnlyTheScansFiles(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	r := logTestReport(now)
	rule := defaultLogRule()
	plan, err := planLogCleanup(r, rule, LogPick{Items: []string{"rotated", "journal", "active:/var/log/nginx/access.log"},
		Restart: []string{"tomcat.service"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]LogAction{}
	for _, a := range plan.Actions {
		kinds[a.Kind] = a
	}
	if d := kinds["delete"]; len(d.Files) != 1 || d.Files[0].Path != "/var/log/syslog.2.gz" || d.KeepDays != 14 {
		t.Errorf("delete = %+v (only files older than 14 days)", d)
	}
	if j := kinds["journal"]; j.KeepDays != 28 || j.MaxBytes != 2048<<20 {
		t.Errorf("journal = %+v", j)
	}
	if tr := kinds["truncate"]; tr.Path != "/var/log/nginx/access.log" || tr.KeepBytes != 100<<20 {
		t.Errorf("truncate = %+v", tr)
	}
	if rs := kinds["restart_unit"]; rs.Unit != "tomcat.service" {
		t.Errorf("restart = %+v", rs)
	}
	want := int64(2000) + (4<<30 - 2048<<20) + (3<<30 - 100<<20) + 2<<30
	if plan.Bytes != want {
		t.Errorf("planned %d, want %d", plan.Bytes, want)
	}
	for name, pick := range map[string]LogPick{
		"an item the scan did not report": {Items: []string{"path:/etc/*"}},
		"a service holding nothing":       {Restart: []string{"sshd.service"}},
		"nothing at all":                  {},
	} {
		if _, err := planLogCleanup(r, rule, pick, now); err == nil {
			t.Errorf("%s: planned", name)
		}
	}
	open := LogPick{}
	open.Open = append(open.Open, struct {
		PID int `json:"pid"`
		FD  int `json:"fd"`
	}{PID: 2381, FD: 9})
	if _, err := planLogCleanup(r, rule, open, now); err == nil {
		t.Error("an open file the scan did not report was planned")
	}
}

func TestAutomaticPickLeavesDumpsAndLiveLogs(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	r := logTestReport(now)
	rule := defaultLogRule()
	pick := automaticPick(r, rule, now)
	if strings.Join(pick.Items, ",") != "journal,rotated" {
		t.Errorf("picked %v", pick.Items)
	}
	rule.ActiveOverMB = 1024
	if pick := automaticPick(r, rule, now); !contains(pick.Items, "active:/var/log/nginx/access.log") {
		t.Errorf("a live log over the rule's size should go: %v", pick.Items)
	}
	if d, ok := r.fullDisk(85); !ok || d.percent() != 93 {
		t.Errorf("full disk = %+v %v", d, ok)
	}
}

/* ── With a database ─────────────────────────────────────────────────────── */

type logEnv struct{ pkgEnv }

func logsAPI(t *testing.T) logEnv {
	t.Helper()
	e := packagesAPI(t)
	before, err := e.s.LogCleanupPolicy(context.Background())
	if err != nil {
		t.Fatalf("LogCleanupPolicy: %v", err)
	}
	t.Cleanup(func() {
		if err := e.s.SaveLogCleanupPolicy(context.Background(), before); err != nil {
			t.Errorf("restoring the log cleanup policy: %v", err)
		}
	})
	return logEnv{e}
}

func (e logEnv) server(t *testing.T, tags ...string) (Asset, string) {
	asset, cred := e.pkgEnv.server(t, tags...)
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM log_cleanup_runs WHERE lower(asset_hostname) = lower($1)`,
			asset.Hostname)
	})
	return asset, cred
}

func (e logEnv) beat(t *testing.T, cred, host string, b *LogBeat) *LogOrders {
	t.Helper()
	body := map[string]any{"hostname": host, "version": "1.2.0", "active_sessions": 0}
	if b != nil {
		body["logs"] = b
	}
	raw, _ := json.Marshal(body)
	w := do(t, e.h, http.MethodPost, "/api/v1/report/heartbeat", cred, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	p, ok := out["logs"]
	if !ok {
		return nil
	}
	var o LogOrders
	if err := json.Unmarshal(p, &o); err != nil {
		t.Fatalf("logs: %v", err)
	}
	return &o
}

func (e logEnv) view(t *testing.T, asset Asset) LogsView {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/logs", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("view: %d %s", w.Code, w.Body)
	}
	var v LogsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func (e logEnv) ask(t *testing.T, asset Asset, token, body string, want int) LogCleanupRun {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/logs/runs", token, body)
	if w.Code != want {
		t.Fatalf("ask: %d %s, want %d", w.Code, w.Body, want)
	}
	var r LogCleanupRun
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

func (e logEnv) run(t *testing.T, id string) LogCleanupRun {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/log-runs/"+id, "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	var r LogCleanupRun
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

// A cleanup by hand from start to end: reported, confirmed by name, sent
// until the agent answers, and audited once with what it removed.
func TestLogCleanupByHand(t *testing.T) {
	e := logsAPI(t)
	asset, cred := e.server(t)
	now := time.Now().UTC()
	rep := logTestReport(now)

	o := e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true, Report: &rep})
	if o == nil || !o.Scan || o.Run != nil {
		t.Fatalf("orders = %+v", o)
	}
	if n := alertCountTitled(t, e.s, asset.Hostname+": / is 93% full"); n != 1 {
		t.Errorf("full disk alerts = %d", n)
	}
	// The same disk on the next scan is not news.
	e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true, Report: &rep})
	if n := alertCountTitled(t, e.s, asset.Hostname+": / is 93% full"); n != 1 {
		t.Errorf("full disk alerted again: %d", n)
	}
	v := e.view(t, asset)
	if !v.Supported || v.Report == nil || len(v.Previews) != 4 || v.Confirm == "" {
		t.Fatalf("view = %+v", v)
	}
	// Auditors see, they do not clean; nobody cleans without typing the name.
	e.ask(t, asset, "auditor-token", `{"items":["rotated"],"confirm":"`+v.Confirm+`"}`, http.StatusForbidden)
	e.ask(t, asset, "admin-token", `{"items":["rotated"],"confirm":"yes"}`, http.StatusBadRequest)
	e.ask(t, asset, "admin-token", `{"items":["path:/etc/*"],"confirm":"`+v.Confirm+`"}`, http.StatusBadRequest)

	run := e.ask(t, asset, "admin-token", `{"items":["rotated","journal"],"confirm":"`+strings.ToUpper(v.Confirm)+`"}`,
		http.StatusAccepted)
	if run.State != RunQueued || run.PlannedBytes == 0 || !strings.Contains(run.Summary, "Rotated logs (1 file)") {
		t.Fatalf("run = %+v", run)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "logs.clean_requested" || !strings.Contains(a.Detail, "Journal") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	e.ask(t, asset, "admin-token", `{"items":["journal"],"confirm":"`+v.Confirm+`"}`, http.StatusConflict)

	o = e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true})
	if o.Run == nil || o.Run.ID != run.ID || len(o.Run.Actions) != 2 {
		t.Fatalf("the reply should carry the cleanup: %+v", o.Run)
	}
	for _, a := range o.Run.Actions {
		if a.Kind == "delete" && (len(a.Files) != 1 || a.Files[0].Path != "/var/log/syslog.2.gz") {
			t.Errorf("delete names %+v", a.Files)
		}
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/log-runs/"+run.ID+"/cancel", "admin-token", "")
	if w.Code != http.StatusConflict {
		t.Errorf("cancel after sending: %d %s", w.Code, w.Body)
	}

	done := &LogRunStatus{ID: run.ID, State: "done", Message: "freed 2 GB", Freed: 2 << 30,
		Removed: []LogRemoved{{Path: "/var/log/syslog.2.gz", Action: "deleted", Bytes: 2000, Modified: now},
			{Path: "journal", Action: "vacuumed", Bytes: 4 << 30, After: 2 << 30}},
		Skipped: []LogSkipped{{Path: "/var/log/syslog.1", Why: "it changed since the scan"}}}
	o = e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true, Run: done})
	if o.Run != nil {
		t.Fatalf("a finished cleanup was sent again")
	}
	got := e.run(t, run.ID)
	if got.State != RunDone || got.Freed != 2<<30 || len(got.Removed) != 2 || len(got.Skipped) != 1 {
		t.Fatalf("done = %+v", got)
	}
	a := lastAuditOn(t, e.s, asset.Hostname)
	if a.Action != "logs.clean_finished" || !strings.Contains(a.Detail, "removed or emptied 2 files") ||
		!strings.Contains(a.Detail, run.ID) {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true, Run: done})
	if n := auditCount(t, e.s, "logs.clean_finished", asset.Hostname); n != 1 {
		t.Errorf("clean_finished audited %d times", n)
	}
}

// A tag that needs approval: the asker cannot approve their own.
func TestLogCleanupNeedsAnotherAdmin(t *testing.T) {
	e := logsAPI(t)
	p, _ := e.s.LogCleanupPolicy(context.Background())
	r := defaultLogRule()
	r.Tag, r.NeedsApproval = "prod", true
	p.Rules = append([]LogCleanupRule{r}, p.Rules...)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SaveLogCleanupPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	asset, cred := e.server(t, "prod")
	rep := logTestReport(time.Now().UTC())
	e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true, Report: &rep})
	v := e.view(t, asset)
	run := e.ask(t, asset, "admin-token", `{"items":["journal"],"confirm":"`+v.Confirm+`"}`, http.StatusAccepted)
	if run.State != RunAwaitingApproval {
		t.Fatalf("state = %s", run.State)
	}
	if o := e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true}); o.Run != nil {
		t.Fatal("a cleanup waiting for approval was sent")
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/log-runs/"+run.ID+"/approve", "admin-token", "")
	if w.Code != http.StatusConflict {
		t.Errorf("self-approval: %d %s", w.Code, w.Body)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/log-runs/"+run.ID+"/approve", "second-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	if o := e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true}); o.Run == nil || o.Run.ID != run.ID {
		t.Fatal("an approved cleanup was not sent")
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "logs.clean_approved" {
		t.Errorf("audit = %s", a.Action)
	}
}

// A rule that cleans when a disk fills starts one cleanup a day, of what
// the rule allows, and never an open file or a dump.
func TestLogCleanupWhenADiskFills(t *testing.T) {
	e := logsAPI(t)
	p, _ := e.s.LogCleanupPolicy(context.Background())
	r := defaultLogRule()
	r.Tag, r.Mode, r.DiskPercent = "full", LogCleanDisk, 90
	p.Rules = append([]LogCleanupRule{r}, p.Rules...)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SaveLogCleanupPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	asset, cred := e.server(t, "full")
	rep := logTestReport(time.Now().UTC())
	o := e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true, Report: &rep})
	if o.Run == nil {
		t.Fatal("no automatic cleanup at 93%")
	}
	for _, a := range o.Run.Actions {
		if a.Kind != "delete" && a.Kind != "journal" {
			t.Errorf("an automatic cleanup took %s", a.Kind)
		}
	}
	a := lastAuditOn(t, e.s, asset.Hostname)
	if a.Action != "logs.clean_started" || !strings.Contains(a.Detail, "93% full") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	runID := o.Run.ID
	done := &LogRunStatus{ID: runID, State: "done", Freed: 1}
	e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true, Run: done, Report: &rep})
	// Still full: but one a day.
	if o := e.beat(t, cred, asset.Hostname, &LogBeat{Supported: true}); o.Run != nil {
		t.Errorf("a second automatic cleanup on the same day: %+v", o.Run.ID)
	}
}

// An agent older than log cleanup gets nothing to read.
func TestLogCleanupOlderAgent(t *testing.T) {
	e := logsAPI(t)
	asset, cred := e.server(t)
	if o := e.beat(t, cred, asset.Hostname, nil); o != nil {
		t.Errorf("orders for an older agent: %+v", o)
	}
	v := e.view(t, asset)
	if v.Reported {
		t.Error("an agent that said nothing is shown as reporting")
	}
	e.ask(t, asset, "admin-token", `{"items":["journal"],"confirm":"`+v.Confirm+`"}`, http.StatusConflict)
}

func alertCountTitled(t *testing.T, s *Store, title string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM console_alerts WHERE title = $1`,
		title).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

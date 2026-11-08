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
Growing disks from the console: what may be planned from the agent's
report, approvals, the heartbeat orders, the audit of what ran, and the
automatic rule.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Disk
*/

const testGiB = int64(1) << 30

func diskTestReport() DiskReport {
	root := DiskFS{Key: "uuid:aaaa", Mount: "/", FS: "ext4", Total: uint64(20 * testGiB),
		Used: uint64(19 * testGiB), Avail: uint64(1 * testGiB), Answer: DiskGrow, Room: 80 * testGiB,
		Layers: []DiskLayer{{Kind: "disk", Name: "nvme0n1", Size: 100 * testGiB},
			{Kind: "partition", Name: "nvme0n1p1", Size: 20 * testGiB, Room: 80 * testGiB},
			{Kind: "fs", Name: "ext4", Size: 20 * testGiB}},
		Path: &GrowPath{Disk: "nvme0n1", DiskSize: 100 * testGiB, Part: "nvme0n1p1", PartNum: 1, PartStart: 1 << 20,
			PartSize: 20 * testGiB, PartRoom: 80 * testGiB, Table: "gpt", FSDevice: "nvme0n1p1", FSType: "ext4",
			FSSize: 20 * testGiB, Mount: "/"}}
	data := DiskFS{Key: "uuid:bbbb", Mount: "/data", FS: "xfs", Total: uint64(50 * testGiB),
		Used: uint64(10 * testGiB), Avail: uint64(40 * testGiB), Answer: DiskGrow, Room: 30 * testGiB, LVM: true,
		Layers: []DiskLayer{{Kind: "vg", Name: "vg0", Size: 80 * testGiB, Room: 30 * testGiB},
			{Kind: "lv", Name: "data", Size: 50 * testGiB}, {Kind: "fs", Name: "xfs", Size: 50 * testGiB}},
		Path: &GrowPath{PV: "sdb", VG: "vg0", VGUUID: "vg-uuid", VGFree: 30 * testGiB, LV: "data", LVUUID: "lv-uuid",
			LVSize: 50 * testGiB, FSDevice: "dm-0", FSType: "xfs", FSSize: 50 * testGiB, Mount: "/data"}}
	boot := DiskFS{Key: "uuid:cccc", Mount: "/boot/efi", FS: "vfat", Answer: DiskBlocked,
		Why: "vfat cannot grow while mounted.", Path: &GrowPath{Mount: "/boot/efi"}}
	return DiskReport{ScannedAt: time.Now().UTC(), OS: "linux", Supported: true, Hypervisor: "KVM",
		Filesystems: []DiskFS{root, data, boot}, Disks: []BlockDisk{{Name: "nvme0n1", Size: 100 * testGiB, Bus: "nvme"}}}
}

/* ── Without a database ──────────────────────────────────────────────────── */

func TestPlanDiskGrowOnlyFromTheReport(t *testing.T) {
	r := diskTestReport()
	r.bounded()
	if r.Filesystems[2].Path != nil {
		t.Error("a filesystem that cannot grow kept a path")
	}
	p, err := planDiskGrow(r, "uuid:aaaa", 0)
	if err != nil || p.Planned != 80*testGiB || p.Bytes != 0 || !strings.Contains(p.Summary, "grow / (ext4) by 80") {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	if p, err := planDiskGrow(r, "uuid:bbbb", 10*testGiB); err != nil || p.Bytes != 10*testGiB || p.Planned != 10*testGiB {
		t.Errorf("part of a group = %+v, %v", p, err)
	}
	if p, err := planDiskGrow(r, "uuid:bbbb", 30*testGiB); err != nil || p.Bytes != 0 {
		t.Errorf("all of a group's room is all = %+v, %v", p, err)
	}
	for name, c := range map[string]struct {
		key   string
		bytes int64
	}{
		"a filesystem the scan did not see": {"uuid:zzzz", 0},
		"one that cannot grow":              {"uuid:cccc", 0},
		"part of a partition":               {"uuid:aaaa", testGiB},
		"more than the group has":           {"uuid:bbbb", 31 * testGiB},
		"a negative size":                   {"uuid:bbbb", -1},
	} {
		if _, err := planDiskGrow(r, c.key, c.bytes); err == nil {
			t.Errorf("%s: planned", name)
		}
	}
}

func TestDiskGrowPolicyValidate(t *testing.T) {
	good := func() DiskGrowPolicy {
		return DiskGrowPolicy{Rules: []DiskGrowRule{{Tag: " production ", NeedsApproval: true}, {AutoPercent: 90}}}
	}
	p := good()
	if err := p.Validate(); err != nil || p.Rules[0].Tag != "production" || p.Rules[1].Position != 1 {
		t.Fatalf("Validate: %+v %v", p, err)
	}
	bad := map[string]func(*DiskGrowPolicy){
		"no default rule":  func(p *DiskGrowPolicy) { p.Rules = p.Rules[:1] },
		"two defaults":     func(p *DiskGrowPolicy) { p.Rules[0].Tag = "" },
		"a mark too low":   func(p *DiskGrowPolicy) { p.Rules[1].AutoPercent = 20 },
		"a mark of 100":    func(p *DiskGrowPolicy) { p.Rules[1].AutoPercent = 100 },
		"a tag with a ;":   func(p *DiskGrowPolicy) { p.Rules[0].Tag = "a;b" },
		"the same tag x 2": func(p *DiskGrowPolicy) { p.Rules = append(p.Rules, DiskGrowRule{Tag: "PRODUCTION"}) },
	}
	for name, mutate := range bad {
		p := good()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if r := (DiskGrowPolicy{}).ruleFor(nil); !r.NeedsApproval {
		t.Error("with no rules, a grow should wait for approval")
	}
	if r := p.ruleFor([]string{"Production"}); !r.NeedsApproval {
		t.Error("tags match without case")
	}
}

/* ── With a database ─────────────────────────────────────────────────────── */

type diskEnv struct{ pkgEnv }

func disksAPI(t *testing.T) diskEnv {
	t.Helper()
	e := packagesAPI(t)
	before, err := e.s.DiskGrowPolicy(context.Background())
	if err != nil {
		t.Fatalf("DiskGrowPolicy: %v", err)
	}
	t.Cleanup(func() {
		if err := e.s.SaveDiskGrowPolicy(context.Background(), before); err != nil {
			t.Errorf("restoring the disk grow rules: %v", err)
		}
	})
	return diskEnv{e}
}

func (e diskEnv) server(t *testing.T, tags ...string) (Asset, string) {
	asset, cred := e.pkgEnv.server(t, tags...)
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM disk_grow_runs WHERE lower(asset_hostname) = lower($1)`,
			asset.Hostname)
	})
	return asset, cred
}

func (e diskEnv) beat(t *testing.T, cred, host string, b *DiskBeat) *DiskOrders {
	t.Helper()
	body := map[string]any{"hostname": host, "version": "1.2.0", "active_sessions": 0}
	if b != nil {
		body["disks"] = b
	}
	raw, _ := json.Marshal(body)
	w := do(t, e.h, http.MethodPost, "/api/v1/report/heartbeat", cred, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	p, ok := out["disks"]
	if !ok {
		return nil
	}
	var o DiskOrders
	if err := json.Unmarshal(p, &o); err != nil {
		t.Fatalf("disks: %v", err)
	}
	return &o
}

func (e diskEnv) view(t *testing.T, asset Asset) DisksView {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/disks", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("view: %d %s", w.Code, w.Body)
	}
	var v DisksView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func (e diskEnv) grow(t *testing.T, asset Asset, token, body string, want int) DiskGrowRun {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/disks/grow", token, body)
	if w.Code != want {
		t.Fatalf("grow: %d %s, want %d", w.Code, w.Body, want)
	}
	var r DiskGrowRun
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

func (e diskEnv) run(t *testing.T, id string) DiskGrowRun {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/disk-runs/"+id, "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	var r DiskGrowRun
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

func (e diskEnv) noApproval(t *testing.T) {
	t.Helper()
	p := DiskGrowPolicy{Rules: []DiskGrowRule{{Tag: "production", NeedsApproval: true}, {}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SaveDiskGrowPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// A grow by hand from start to end: planned from the report, sent until
// the agent answers, not taken back once sent, and audited once with what
// it ran.
func TestDiskGrowByHand(t *testing.T) {
	e := disksAPI(t)
	e.noApproval(t)
	asset, cred := e.server(t)
	rep := diskTestReport()

	o := e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, Report: &rep})
	if o == nil || !o.Scan || o.Run != nil || o.ScanID != "" {
		t.Fatalf("orders = %+v", o)
	}
	v := e.view(t, asset)
	if !v.Supported || v.Report == nil || len(v.Report.Filesystems) != 3 || v.Rule.NeedsApproval {
		t.Fatalf("view = %+v", v)
	}

	// A scan asked for, with a rescan, is on the next reply until the agent
	// says it scanned.
	w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/disks/scan", "admin-token", `{"rescan":true}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("scan: %d %s", w.Code, w.Body)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "disks.rescan_requested" {
		t.Errorf("audit = %s", a.Action)
	}
	o = e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true})
	if o.ScanID == "" || !o.Rescan {
		t.Fatalf("scan orders = %+v", o)
	}
	o = e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, ScanID: o.ScanID, Report: &rep})
	if o.ScanID != "" || o.Rescan {
		t.Fatalf("a finished scan is asked again: %+v", o)
	}

	// Auditors see, they do not grow; nothing the scan did not offer.
	e.grow(t, asset, "auditor-token", `{"key":"uuid:aaaa"}`, http.StatusForbidden)
	e.grow(t, asset, "admin-token", `{"key":"uuid:cccc"}`, http.StatusBadRequest)
	e.grow(t, asset, "admin-token", `{"key":"/dev/sda"}`, http.StatusBadRequest)

	run := e.grow(t, asset, "admin-token", `{"key":"uuid:aaaa"}`, http.StatusAccepted)
	if run.State != RunQueued || run.PlannedBytes != 80*testGiB || run.Path.Part != "nvme0n1p1" {
		t.Fatalf("run = %+v", run)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "disks.grow_requested" || !strings.Contains(a.Detail, "grow /") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	e.grow(t, asset, "admin-token", `{"key":"uuid:bbbb"}`, http.StatusConflict)

	o = e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true})
	if o.Run == nil || o.Run.ID != run.ID || o.Run.Key != "uuid:aaaa" || o.Run.Path.PartStart != 1<<20 {
		t.Fatalf("the reply should carry the grow: %+v", o.Run)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/disk-runs/"+run.ID+"/cancel", "admin-token", "")
	if w.Code != http.StatusConflict {
		t.Errorf("cancel after sending: %d %s", w.Code, w.Body)
	}

	running := &DiskRunStatus{ID: run.ID, State: "running", Step: "Grow partition 1",
		Steps: []DiskStep{{Title: "Check the layout", State: "done"}}}
	e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, Run: running})
	if got := e.run(t, run.ID); got.State != RunRunning || got.StartedAt == nil || len(got.Steps) != 1 {
		t.Fatalf("running = %+v", got)
	}
	done := &DiskRunStatus{ID: run.ID, State: "done", Message: "grew / from 20 GiB to 100 GiB",
		TableBefore: "label: gpt\n", Before: 20 * testGiB, After: 100 * testGiB,
		Steps: []DiskStep{{Title: "Check the layout", State: "done"},
			{Title: "Grow partition 1", Command: "growpart /dev/nvme0n1 1", State: "done"},
			{Title: "Grow the filesystem", Command: "resize2fs /dev/nvme0n1p1", State: "done"}}}
	o = e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, Run: done})
	if o.Run != nil {
		t.Fatalf("a finished grow was sent again")
	}
	got := e.run(t, run.ID)
	if got.State != RunDone || got.SizeAfter != 100*testGiB || got.TableBefore != "label: gpt\n" || len(got.Steps) != 3 {
		t.Fatalf("done = %+v", got)
	}
	a := lastAuditOn(t, e.s, asset.Hostname)
	if a.Action != "disks.grow_finished" || !strings.Contains(a.Detail, "growpart /dev/nvme0n1 1 (done)") ||
		!strings.Contains(a.Detail, run.ID) {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, Run: done})
	if n := auditCount(t, e.s, "disks.grow_finished", asset.Hostname); n != 1 {
		t.Errorf("grow_finished audited %d times", n)
	}
	if v := e.view(t, asset); len(v.Runs) != 1 || v.Runs[0].AssetID != asset.ID {
		t.Errorf("runs = %+v", v.Runs)
	}
}

// Production waits for a second admin, and a failure rings the bell.
func TestDiskGrowNeedsAnotherAdmin(t *testing.T) {
	e := disksAPI(t)
	e.noApproval(t)
	asset, cred := e.server(t, "production")
	rep := diskTestReport()
	e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, Report: &rep})
	run := e.grow(t, asset, "admin-token", `{"key":"uuid:bbbb","bytes":`+jsonInt(10*testGiB)+`}`, http.StatusAccepted)
	if run.State != RunAwaitingApproval || run.GrowBytes != 10*testGiB {
		t.Fatalf("run = %+v", run)
	}
	if o := e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true}); o.Run != nil {
		t.Fatal("a grow waiting for approval was sent")
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/disk-runs/"+run.ID+"/approve", "admin-token", "")
	if w.Code != http.StatusConflict {
		t.Errorf("self-approval: %d %s", w.Code, w.Body)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/disk-runs/"+run.ID+"/approve", "second-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	o := e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true})
	if o.Run == nil || o.Run.ID != run.ID || o.Run.Bytes != 10*testGiB {
		t.Fatalf("an approved grow was not sent: %+v", o.Run)
	}
	failed := &DiskRunStatus{ID: run.ID, State: "failed", Message: "nothing was changed: the LVM layout changed since the scan"}
	e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, Run: failed})
	if n := alertCountTitled(t, e.s, "Growing /data on "+asset.Hostname+" failed"); n != 1 {
		t.Errorf("failure alerts = %d", n)
	}
	if a := lastAuditOn(t, e.s, asset.Hostname); a.Action != "disks.grow_failed" {
		t.Errorf("audit = %s", a.Action)
	}
}

// A rule that grows on its own grows the fullest filesystem with room, once
// a day.
func TestDiskGrowOnItsOwn(t *testing.T) {
	e := disksAPI(t)
	p := DiskGrowPolicy{Rules: []DiskGrowRule{{Tag: "auto", AutoPercent: 90}, {}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SaveDiskGrowPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	asset, cred := e.server(t, "auto")
	rep := diskTestReport()
	o := e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, Report: &rep})
	if o.Run == nil || o.Run.Key != "uuid:aaaa" {
		t.Fatalf("no automatic grow at 95%%: %+v", o.Run)
	}
	a := lastAuditOn(t, e.s, asset.Hostname)
	if a.Action != "disks.grow_started" || !strings.Contains(a.Detail, "95% full") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true, Report: &rep,
		Run: &DiskRunStatus{ID: o.Run.ID, State: "done"}})
	if o := e.beat(t, cred, asset.Hostname, &DiskBeat{Supported: true}); o.Run != nil {
		t.Errorf("a second automatic grow on the same day: %+v", o.Run.ID)
	}
}

// An agent older than disk growing gets nothing to read, and nothing can be
// asked of it.
func TestDiskGrowOlderAgent(t *testing.T) {
	e := disksAPI(t)
	asset, cred := e.server(t)
	if o := e.beat(t, cred, asset.Hostname, nil); o != nil {
		t.Errorf("orders for an older agent: %+v", o)
	}
	if v := e.view(t, asset); v.Reported {
		t.Error("an agent that said nothing is shown as reporting")
	}
	e.grow(t, asset, "admin-token", `{"key":"uuid:aaaa"}`, http.StatusConflict)
}

func TestDiskGrowPolicyRoute(t *testing.T) {
	e := disksAPI(t)
	w := do(t, e.h, http.MethodPut, "/api/v1/settings/disk-grow", "admin-token",
		`{"rules":[{"tag":"db","needsApproval":true,"autoPercent":0},{"tag":"","autoPercent":85}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	var p DiskGrowPolicy
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if len(p.Rules) != 2 || p.Rules[0].Tag != "db" || p.Rules[1].AutoPercent != 85 {
		t.Errorf("rules = %+v", p.Rules)
	}
	w = do(t, e.h, http.MethodPut, "/api/v1/settings/disk-grow", "auditor-token", `{"rules":[{"tag":""}]}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("auditor put: %d", w.Code)
	}
	w = do(t, e.h, http.MethodPut, "/api/v1/settings/disk-grow", "admin-token", `{"rules":[{"tag":"x"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("no default: %d", w.Code)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

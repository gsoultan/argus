package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

/* ── The helper: checking ────────────────────────────────────────────────── */

// PackageChecker looks for updates with whatever package manager the server
// has. It is what `argus-agent packages-check` runs.
type PackageChecker struct {
	Run  cmdRunner
	Look lookPathFunc
	Now  func() time.Time
}

func (c PackageChecker) defaults() PackageChecker {
	if c.Run == nil {
		c.Run = execRunner
	}
	if c.Look == nil {
		c.Look = exec.LookPath
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Check refreshes the package lists and says what can be upgraded.
func (c PackageChecker) Check(ctx context.Context) (PackageReport, error) {
	c = c.defaults()
	_, off := c.Now().Zone()
	r := PackageReport{CheckedAt: c.Now().UTC(), UTCOffsetMinutes: off / 60, Updates: []PackageUpdate{}}
	m := detectManager(c.Look)
	if m == nil {
		r.Reason = errNoManager.Error()
		return r, nil
	}
	r.Manager, r.Supported = m.name(), true
	if err := m.refresh(ctx, c.Run, nil); err != nil {
		return r, err
	}
	return c.listInto(ctx, m, r)
}

func (c PackageChecker) listInto(ctx context.Context, m pkgManager, r PackageReport) (PackageReport, error) {
	ups, patches, err := m.list(ctx, c.Run)
	if err != nil {
		return r, err
	}
	if len(ups) > maxReportedUpdates {
		ups = ups[:maxReportedUpdates]
	}
	if ups == nil {
		ups = []PackageUpdate{}
	}
	r.Updates, r.SecurityPatches = ups, patches
	r.RebootRequired = m.rebootWanted(ctx, c.Run)
	return r, nil
}

// RunPackagesCheck is `argus-agent packages-check`: the report on stdout.
func RunPackagesCheck(stdout io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), packageCheckTimeout)
	defer cancel()
	r, err := PackageChecker{}.Check(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(r); err != nil {
		return 1
	}
	return 0
}

/* ── The helper: installing ──────────────────────────────────────────────── */

// PackageApplier carries out a run. It is what `argus-agent packages-apply`
// runs, as root, in a unit of its own.
type PackageApplier struct {
	Plan PackagePlan
	Log  *slog.Logger
	PackageChecker
	// Restart restarts a service; Reboot reboots. Replaced in tests.
	Restart func(ctx context.Context, svc string) error
	Reboot  func() error
	BootID  func() string
	// Windows runs the plan the Windows way (winsoft.go), with
	// WinInventory reading the installed programs.
	Windows      bool
	WinInventory winInventory

	mu sync.Mutex
	st PackageRunStatus
	lw *runLog
}

// RunPackagesApply is `argus-agent packages-apply --plan <path>`.
func RunPackagesApply(planPath string) int {
	b, err := os.ReadFile(planPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "packages-apply:", err)
		return 2
	}
	var plan PackagePlan
	if err := json.Unmarshal(b, &plan); err != nil || plan.StateDir == "" || plan.ID == "" {
		fmt.Fprintln(os.Stderr, "packages-apply: the plan cannot be read")
		return 2
	}
	a := &PackageApplier{Plan: plan, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Windows: runtime.GOOS == "windows", WinInventory: hostWinInventory}
	timeout := packageRunTimeout
	if plan.Script != nil && plan.Script.timeout()+10*time.Minute > timeout {
		timeout = plan.Script.timeout() + 10*time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st := a.Apply(ctx)
	_ = os.Remove(planPath)
	if st.State == RunFailed {
		return 1
	}
	return 0
}

func (a *PackageApplier) defaults() {
	a.PackageChecker = a.PackageChecker.defaults()
	if a.Log == nil {
		a.Log = slog.Default()
	}
	if a.Restart == nil {
		a.Restart = func(ctx context.Context, svc string) error {
			code, err := a.Run(ctx, nil, nil, "systemctl", "try-restart", svc)
			if err == nil && code != 0 {
				err = fmt.Errorf("systemctl try-restart %s: exit %d", svc, code)
			}
			return err
		}
	}
	if a.Reboot == nil {
		a.Reboot = func() error {
			_, err := a.Run(context.Background(), nil, nil, "systemctl", "reboot")
			return err
		}
	}
	if a.BootID == nil {
		a.BootID = readBootID
	}
}

// Apply runs the plan and returns the state it ended in, which it has also
// written to packages-run.json.
func (a *PackageApplier) Apply(ctx context.Context) PackageRunStatus {
	a.defaults()
	now := a.Now().UTC()
	a.st = PackageRunStatus{ID: a.Plan.ID, State: RunRunning, Step: "refresh",
		Message: "refreshing the package lists", Unit: a.Plan.Unit, BootID: a.BootID(),
		StartedAt: now, At: now}
	a.lw = &runLog{limit: maxRunLog, flush: a.flushLog}
	a.save()
	if a.Windows {
		return a.applyWindows(ctx)
	}
	if a.Plan.Mode == PackagesInstall || a.Plan.Mode == PackagesRemove {
		return a.applySoftware(ctx)
	}
	if a.Plan.Mode == PackagesScript {
		return a.applyScript(ctx)
	}

	m := detectManager(a.Look)
	if m == nil {
		return a.fail(errNoManager.Error())
	}
	if err := m.refresh(ctx, a.Run, a.lw); err != nil {
		return a.fail(err.Error())
	}
	ups, _, err := m.list(ctx, a.Run)
	if err != nil {
		return a.fail(err.Error())
	}
	names, all, why := a.targets(ups)
	if why != "" {
		return a.fail(why)
	}
	before := map[string]string{}
	for _, u := range ups {
		before[u.Name] = u.Installed
	}
	planned := len(names)
	if all {
		planned = 0
		for _, u := range ups {
			if !u.New && !held(u.Name, a.Plan.Holds) {
				planned++
			}
		}
	}
	zy, zypperSecurity := m.(zypperManager)
	zypperSecurity = zypperSecurity && a.Plan.Mode == PackagesSecurity
	if planned == 0 && !zypperSecurity {
		a.writeReport(ctx, m)
		return a.finish(RunDone, "nothing to install: the server is up to date for what was asked", nil)
	}

	a.update(func(s *PackageRunStatus) {
		s.Step, s.Planned = "install", planned
		s.Message = fmt.Sprintf("installing %d package%s", planned, plural(planned))
		if zypperSecurity {
			s.Message = "installing the security patches"
		}
	})
	if zypperSecurity {
		err = zy.installSecurityPatches(ctx, a.Run, a.lw)
	} else {
		err = m.install(ctx, a.Run, a.lw, names, all, a.Plan.Holds)
	}
	a.lw.Flush()
	// What did change is worth reporting even when the install failed half
	// way: some packages may be on the new version.
	changed := a.changes(ctx, m, ups, before)
	if err != nil {
		a.update(func(s *PackageRunStatus) { s.Upgraded = changed })
		a.writeReport(ctx, m)
		return a.fail(err.Error())
	}

	a.update(func(s *PackageRunStatus) {
		s.Step, s.Upgraded, s.Message = "services", changed, "restarting services that use replaced files"
	})
	var restarted, notRestarted []string
	for _, svc := range sortedUnique(m.staleServices(ctx, a.Run)) {
		if protectedService(svc) {
			notRestarted = append(notRestarted, svc)
			continue
		}
		if err := a.Restart(ctx, svc); err != nil {
			fmt.Fprintf(a.lw, "argus: could not restart %s: %v\n", svc, err)
			notRestarted = append(notRestarted, svc)
			continue
		}
		restarted = append(restarted, svc)
	}
	a.update(func(s *PackageRunStatus) { s.Restarted = restarted })
	if len(notRestarted) > 0 {
		fmt.Fprintf(a.lw, "argus: left for a person or a reboot: %s\n", strings.Join(notRestarted, ", "))
	}

	reboot := m.rebootWanted(ctx, a.Run)
	for _, c := range changed {
		if kernelPackage(strings.SplitN(c.Name, ".", 2)[0]) {
			reboot = true
		}
	}
	a.writeReport(ctx, m)
	msg := fmt.Sprintf("upgraded %d package%s", len(changed), plural(len(changed)))
	if len(restarted) > 0 {
		msg += ", restarted " + strings.Join(restarted, ", ")
	}
	if reboot && a.Plan.Reboot == "if_needed" {
		a.update(func(s *PackageRunStatus) {
			s.State, s.Step, s.Message = RunRebooting, "reboot", msg+"; rebooting"
		})
		a.lw.Flush()
		if err := a.Reboot(); err != nil {
			a.update(func(s *PackageRunStatus) { s.RebootRequired = true })
			return a.finish(RunDone, msg+"; the reboot failed ("+err.Error()+"), so the server still needs one", nil)
		}
		// systemd stops this unit on the way down; the agent that comes
		// back reads "rebooting" and a new boot, and calls it done.
		return a.snapshot()
	}
	return a.finish(RunDone, msg, func(s *PackageRunStatus) {
		if reboot {
			s.RebootRequired = true
			s.Message += "; the server needs a reboot for all of it to take effect"
		}
	})
}

// targets is what to upgrade: names, or all with holds left out.
func (a *PackageApplier) targets(ups []PackageUpdate) (names []string, all bool, why string) {
	switch a.Plan.Mode {
	case PackagesAll:
		return nil, true, ""
	case PackagesSecurity:
		for _, u := range ups {
			if u.Security && !u.New && !held(u.Name, a.Plan.Holds) {
				names = append(names, u.Name)
			}
		}
		return names, false, ""
	case PackagesSelected:
		waiting := map[string]bool{}
		for _, u := range ups {
			waiting[u.Name] = true
		}
		for _, n := range a.Plan.Packages {
			if held(n, a.Plan.Holds) {
				continue
			}
			// A name no longer waiting was upgraded since the console
			// looked, or never could be: skip it rather than fail.
			if waiting[n] {
				names = append(names, n)
			}
		}
		return names, false, ""
	}
	return nil, false, fmt.Sprintf("%q is not a way to update", a.Plan.Mode)
}

// changes compares installed versions now with before.
func (a *PackageApplier) changes(ctx context.Context, m pkgManager, ups []PackageUpdate,
	before map[string]string) []PackageChange {
	names := make([]string, 0, len(ups))
	for _, u := range ups {
		names = append(names, u.Name)
	}
	after := m.installed(ctx, a.Run, names)
	var out []PackageChange
	for _, u := range ups {
		now, ok := after[u.Name]
		if !ok || now == "" || now == before[u.Name] {
			continue
		}
		out = append(out, PackageChange{Name: u.Name, From: before[u.Name], To: now})
	}
	return out
}

// writeReport lists what is still waiting, so the console is not left
// showing what the run just installed.
func (a *PackageApplier) writeReport(ctx context.Context, m pkgManager) {
	_, off := a.Now().Zone()
	r := PackageReport{CheckedAt: a.Now().UTC(), Manager: m.name(), Supported: true,
		UTCOffsetMinutes: off / 60}
	r, err := a.listInto(ctx, m, r)
	if err != nil {
		r.Error = err.Error()
	}
	if err := writeFileAtomic(filepath.Join(a.Plan.StateDir, packagesReportFile), mustJSON(r), 0o600); err != nil {
		a.Log.Error("packages: cannot keep the report", "error", err)
	}
}

func (a *PackageApplier) fail(msg string) PackageRunStatus {
	return a.finish(RunFailed, msg, nil)
}

func (a *PackageApplier) finish(state, msg string, more func(*PackageRunStatus)) PackageRunStatus {
	a.lw.Flush()
	a.update(func(s *PackageRunStatus) {
		s.State, s.Step, s.Message, s.FinishedAt = state, "", msg, a.Now().UTC()
		if more != nil {
			more(s)
		}
	})
	return a.snapshot()
}

func (a *PackageApplier) snapshot() PackageRunStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

func (a *PackageApplier) update(f func(*PackageRunStatus)) {
	a.mu.Lock()
	f(&a.st)
	a.st.At = a.Now().UTC()
	a.mu.Unlock()
	a.save()
}

func (a *PackageApplier) flushLog(s string) {
	a.mu.Lock()
	a.st.Log = s
	a.st.At = a.Now().UTC()
	a.mu.Unlock()
	a.save()
}

func (a *PackageApplier) save() {
	a.mu.Lock()
	b := mustJSON(a.st)
	a.mu.Unlock()
	if err := writeFileAtomic(filepath.Join(a.Plan.StateDir, packagesRunFile), b, 0o600); err != nil {
		a.Log.Error("packages: cannot write the run's state", "error", err)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// runLog keeps the end of what the package manager prints, and hands it to
// flush at most once a second so the state file is not rewritten per line.
type runLog struct {
	mu    sync.Mutex
	buf   []byte
	limit int
	last  time.Time
	flush func(string)
}

func (l *runLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > l.limit {
		cut := len(l.buf) - l.limit
		if i := bytes.IndexByte(l.buf[cut:], '\n'); i >= 0 && i < 1024 {
			cut += i + 1
		}
		l.buf = append([]byte(nil), l.buf[cut:]...)
	}
	due := time.Since(l.last) >= time.Second
	var s string
	if due {
		l.last, s = time.Now(), string(l.buf)
	}
	l.mu.Unlock()
	if due && l.flush != nil {
		l.flush(s)
	}
	return len(p), nil
}

// Flush hands over what is held now.
func (l *runLog) Flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	s := string(l.buf)
	l.last = time.Now()
	l.mu.Unlock()
	if l.flush != nil {
		l.flush(s)
	}
}

// readBootID names this boot; "" where the kernel does not say.
func readBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// RunPackagesRepair is `argus-agent packages-repair`: finish what an
// interrupted run left half done.
func RunPackagesRepair(stdout io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	m := detectManager(exec.LookPath)
	if m == nil {
		return 0
	}
	if err := m.repair(ctx, execRunner, stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

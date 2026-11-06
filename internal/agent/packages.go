package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

/*
Operating system updates, from the console.

The agent says which package updates are waiting on its server, which of
them fix a security issue, and whether the server wants a reboot; the console
asks it to install them, by hand or on a schedule an admin set.

Two halves, like the agent's own updates (update.go):

  - The daemon decides. It checks for updates every six hours while the
    console asks it to look, and when the console asks for a run it writes
    a plan and starts the helper. It reports both on the heartbeat.

  - The helper does the work: `argus-agent packages-check` and
    `argus-agent packages-apply`, as root, in a transient systemd unit of
    their own (packages_linux.go). Outside the agent's sandbox, which cannot
    write /var or /usr; and outside its cgroup, so a run goes on when the
    agent restarts, is updated, or loses its connection to the control plane.

What happens when something goes wrong half way:

  - The control plane is unreachable: nothing. The helper does not talk to
    it. The run's state is in packages-run.json in the state directory, and
    the agent reports it on the first heartbeat that gets through.
  - The agent restarts or is updated: the helper's unit is not the agent's,
    so it keeps going; the new agent reads the state file and goes on
    reporting it.
  - The server goes down (power, a person rebooting it): the helper dies
    with it. The agent finds a run that says "running" whose unit is gone,
    calls it interrupted, finishes whatever the package manager left half
    done (dpkg --configure -a), and looks again for what is still waiting.
  - The helper asked for the reboot itself: the state says "rebooting"
    before it does, and the agent that comes back after it calls the run
    done.
  - Another apt or dnf holds the lock: the helper waits two minutes for it
    and fails without changing anything.

A run is identified by the console's id, and the agent acts on one id once:
a heartbeat reply repeating a request the agent already finished, or is
working on, changes nothing.
*/

// PackageUpdate is one package that can be upgraded.
type PackageUpdate struct {
	Name      string `json:"name"`
	Installed string `json:"installed,omitempty"`
	Available string `json:"available"`
	// Security is whether the new version fixes a security issue, as the
	// distribution says.
	Security bool `json:"security,omitempty"`
	// Origin is where the new version comes from: the archive, or the
	// security advisory that names it.
	Origin string `json:"origin,omitempty"`
	// New is a package the upgrade installs that is not installed now,
	// usually a new kernel another package depends on.
	New bool `json:"new,omitempty"`
	// Reboot is a kernel: it runs only after a reboot.
	Reboot bool `json:"reboot,omitempty"`

	arch string // zypper only, to look up the installed version
}

// PackageReport is what the agent found waiting.
type PackageReport struct {
	CheckedAt time.Time `json:"checked_at"`
	// Manager is apt, dnf, yum or zypper; "" when the server has none Argus
	// knows.
	Manager string `json:"manager,omitempty"`
	// Supported is whether this server can be updated from the console;
	// Reason says why not.
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Error is why the last check failed. Updates then hold what the check
	// before it found, and CheckedAt when that was.
	Error   string          `json:"error,omitempty"`
	Updates []PackageUpdate `json:"updates"`
	// SecurityPatches is zypper's count of security patches, which it
	// names by patch rather than by package.
	SecurityPatches int `json:"security_patches,omitempty"`
	// RebootRequired is the server asking for a reboot now, for what is
	// already installed.
	RebootRequired bool `json:"reboot_required,omitempty"`
	// UTCOffsetMinutes is the server's clock against UTC, so a maintenance
	// window is the server's own 02:00.
	UTCOffsetMinutes int `json:"utc_offset_minutes"`
}

// maxReportedUpdates bounds a report. A server years behind can have
// thousands; past this the report says how many were left out.
const maxReportedUpdates = 3000

// PackageOrders is what the control plane says about packages on a
// heartbeat reply.
type PackageOrders struct {
	// Check is whether to look for updates every six hours. Off when the
	// console's policy for this server is off, and absent from a control
	// plane older than package updates, which leaves the agent not looking.
	Check bool `json:"check"`
	// CheckID asks for a check now; the agent reports it back once done.
	CheckID string `json:"check_id,omitempty"`
	// Run is an install the console asked for and the agent has not
	// finished.
	Run *PackageRunRequest `json:"run,omitempty"`
}

// Package run modes.
const (
	PackagesAll      = "all"
	PackagesSecurity = "security"
	PackagesSelected = "selected"
)

// PackageRunRequest is an install the console asked for.
type PackageRunRequest struct {
	ID string `json:"id"`
	// Mode is all, security or selected.
	Mode string `json:"mode"`
	// Packages are the names to upgrade, for selected.
	Packages []string `json:"packages,omitempty"`
	// Holds are globs never upgraded, whatever the mode.
	Holds []string `json:"holds,omitempty"`
	// Reboot is "if_needed" to reboot when the server asks for one after
	// the install, else "never".
	Reboot string `json:"reboot,omitempty"`
}

// Package run states.
const (
	RunStarting    = "starting"
	RunRunning     = "running"
	RunRebooting   = "rebooting"
	RunDone        = "done"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"
)

// PackageChange is one package a run upgraded.
type PackageChange struct {
	Name string `json:"name"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

// PackageRunStatus is what the agent says about a run, on every heartbeat
// until the console has it.
type PackageRunStatus struct {
	ID    string `json:"id"`
	State string `json:"state"`
	// Step is what the helper is doing: refresh, install, services, reboot.
	Step    string `json:"step,omitempty"`
	Message string `json:"message,omitempty"`
	// Planned is how many packages the run means to upgrade.
	Planned   int             `json:"planned,omitempty"`
	Upgraded  []PackageChange `json:"upgraded,omitempty"`
	Restarted []string        `json:"restarted,omitempty"`
	// RebootRequired is the server still wanting a reboot when the run
	// ended without one.
	RebootRequired bool `json:"reboot_required,omitempty"`
	Rebooted       bool `json:"rebooted,omitempty"`
	// Log is the end of what the package manager printed.
	Log        string    `json:"log,omitempty"`
	Unit       string    `json:"unit,omitempty"`
	BootID     string    `json:"boot_id,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	At         time.Time `json:"at"`
}

// Finished reports whether the run has reached an end.
func (s PackageRunStatus) Finished() bool {
	switch s.State {
	case RunDone, RunFailed, RunInterrupted:
		return true
	}
	return false
}

// PackagePlan is what the helper is handed.
type PackagePlan struct {
	PackageRunRequest
	StateDir string `json:"state_dir"`
	Unit     string `json:"unit"`
}

const (
	packagesDir        = "packages"
	packagesReportFile = "packages.json"
	packagesRunFile    = "packages-run.json"
	// packageCheckEvery is how often the agent looks for updates.
	packageCheckEvery = 6 * time.Hour
	// packageCheckRetry is how soon a failed check is tried again.
	packageCheckRetry = 30 * time.Minute
	// maxRunLog bounds the output kept with a run.
	maxRunLog = 48 << 10
	// packageCheckTimeout and packageRunTimeout bound the helper.
	packageCheckTimeout = 15 * time.Minute
	packageRunTimeout   = 2 * time.Hour
)

var reRunID = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// packagePlatform is the part of packages that depends on the host.
type packagePlatform interface {
	// supported says why this server cannot be updated from the console,
	// or "" when it can.
	supported() string
	// check runs the helper's check and returns its report.
	check(ctx context.Context) (PackageReport, error)
	// launch starts the helper for a run, in a unit named unit, and
	// returns without waiting for it.
	launch(ctx context.Context, planPath, unit string) error
	// active reports whether the helper's unit is still running.
	active(unit string) bool
	// repair finishes what an interrupted run left half done.
	repair(ctx context.Context) error
	// bootID names this boot.
	bootID() string
}

// Packages is the daemon's half of package updates.
type Packages struct {
	StateDir string
	Log      *slog.Logger
	// Changed is called when there is something new to report, so it
	// reaches the console without waiting for the next heartbeat.
	Changed func()
	// Busy is whether the agent itself is updating: no run starts then.
	Busy func() bool

	platform packagePlatform
	now      func() time.Time

	mu         sync.Mutex
	enabled    bool
	report     *PackageReport
	sent       time.Time // CheckedAt of the report last delivered
	checking   bool
	lastTry    time.Time
	checkID    string // asked by the console, not yet answered
	checkDone  string // answered, to report
	answered   string // the last check id answered, never run again
	run        PackageRunStatus
	runLoaded  bool
	repairing  bool
	lastChange time.Time
}

// NewPackages builds the daemon's half for this host. executable is the
// agent binary the helper runs from.
func NewPackages(stateDir, executable string, log *slog.Logger) *Packages {
	if log == nil {
		log = slog.Default()
	}
	return &Packages{StateDir: stateDir, Log: log,
		platform: hostPackagePlatform(executable, stateDir), now: time.Now}
}

// PackageBeat is what rides the heartbeat.
type PackageBeat struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Report is sent when it is newer than the last one delivered.
	Report *PackageReport `json:"report,omitempty"`
	// CheckID is a check the console asked for that has finished.
	CheckID string `json:"check_id,omitempty"`
	// Checking is a check under way.
	Checking bool              `json:"checking,omitempty"`
	Run      *PackageRunStatus `json:"run,omitempty"`
}

// Beat is what to put on the next heartbeat.
func (p *Packages) Beat() PackageBeat {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadLocked()
	b := PackageBeat{Checking: p.checking, CheckID: p.checkDone}
	if reason := p.platform.supported(); reason != "" {
		b.Reason = reason
	} else {
		b.Supported = true
	}
	if p.report != nil && p.report.CheckedAt.After(p.sent) {
		r := *p.report
		b.Report = &r
	}
	if p.run.ID != "" {
		r := p.run
		b.Run = &r
	}
	return b
}

// Delivered says a heartbeat carrying b reached the control plane.
func (p *Packages) Delivered(b PackageBeat) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.Report != nil && b.Report.CheckedAt.After(p.sent) {
		p.sent = b.Report.CheckedAt
	}
	if b.CheckID != "" && b.CheckID == p.checkDone {
		p.checkDone = ""
	}
}

// Handle acts on what the control plane said on a heartbeat reply. o is
// nil from a control plane older than package updates.
func (p *Packages) Handle(ctx context.Context, o *PackageOrders) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadLocked()
	if o == nil {
		p.enabled = false
		return
	}
	p.enabled = o.Check
	if o.CheckID != "" && o.CheckID != p.checkID && o.CheckID != p.checkDone && o.CheckID != p.answered {
		p.checkID = o.CheckID
	}
	if o.Run != nil {
		p.startLocked(ctx, *o.Run)
	}
}

// startLocked starts a run the console asked for, once.
func (p *Packages) startLocked(ctx context.Context, req PackageRunRequest) {
	if req.ID == p.run.ID {
		return // working on it, or finished and waiting to be heard
	}
	now := p.now().UTC()
	st := PackageRunStatus{ID: req.ID, StartedAt: now, At: now}
	fail := func(msg string) {
		st.State, st.Message, st.FinishedAt = RunFailed, msg, now
		p.setRunLocked(st)
	}
	if !reRunID.MatchString(req.ID) {
		p.Log.Error("packages: refusing a run with a malformed id")
		return
	}
	if p.run.ID != "" && !p.run.Finished() {
		// One run at a time. The console sends the next only once this
		// one is reported; this is a second line of defence. The new one
		// is left unanswered rather than failed, which would forget the
		// run still going: it is started on a later heartbeat, once this
		// one has ended.
		p.Log.Warn("packages: a run was asked for while another is going; it waits",
			"going", p.run.ID, "asked", req.ID)
		return
	}
	if reason := p.platform.supported(); reason != "" {
		fail(reason)
		return
	}
	switch req.Mode {
	case PackagesAll, PackagesSecurity:
	case PackagesSelected:
		if len(req.Packages) == 0 {
			fail("no packages were named")
			return
		}
		for _, n := range req.Packages {
			if !rePackageName.MatchString(n) {
				fail(fmt.Sprintf("%q is not a package name", n))
				return
			}
		}
	default:
		fail(fmt.Sprintf("%q is not a way to update", req.Mode))
		return
	}
	for _, h := range req.Holds {
		if !reHoldGlob.MatchString(h) {
			fail(fmt.Sprintf("%q is not a package name or pattern", h))
			return
		}
	}
	if p.Busy != nil && p.Busy() {
		fail("the agent is updating itself; ask again once it is back")
		return
	}
	dir := filepath.Join(p.StateDir, packagesDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail("cannot prepare the run: " + err.Error())
		return
	}
	short := strings.ReplaceAll(req.ID, "-", "")
	if len(short) > 12 {
		short = short[:12]
	}
	unit := "argus-packages-" + short
	plan := PackagePlan{PackageRunRequest: req, StateDir: p.StateDir, Unit: unit}
	planPath := filepath.Join(dir, "plan-"+req.ID+".json")
	if err := writeFileAtomic(planPath, mustJSON(plan), 0o600); err != nil {
		fail("cannot prepare the run: " + err.Error())
		return
	}
	st.State, st.Step, st.Message, st.Unit, st.BootID = RunStarting, "starting", "starting", unit, p.platform.bootID()
	p.setRunLocked(st)
	if err := p.platform.launch(ctx, planPath, unit); err != nil {
		st.Unit = ""
		fail("cannot start the update: " + err.Error())
		return
	}
	p.Log.Info("packages: run started", "id", req.ID, "mode", req.Mode, "unit", unit)
}

// rePackageName is a package name as apt, dnf and zypper spell them, with
// an optional architecture.
var rePackageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:~-]{0,199}$`)

// reHoldGlob is a package name with * and ? allowed.
var reHoldGlob = regexp.MustCompile(`^[A-Za-z0-9*?][A-Za-z0-9+._:~*?-]{0,199}$`)

func (p *Packages) setRunLocked(st PackageRunStatus) {
	p.run = st
	if err := writeFileAtomic(filepath.Join(p.StateDir, packagesRunFile), mustJSON(st), 0o600); err != nil {
		p.Log.Error("packages: cannot keep the run's state", "error", err)
	}
	p.lastChange = p.now()
}

// loadLocked reads what an earlier agent left: the last report and the
// last run, so neither is lost to a restart.
func (p *Packages) loadLocked() {
	if p.runLoaded {
		return
	}
	p.runLoaded = true
	if b, err := os.ReadFile(filepath.Join(p.StateDir, packagesRunFile)); err == nil {
		var st PackageRunStatus
		if json.Unmarshal(b, &st) == nil {
			p.run = st
		}
	}
	if b, err := os.ReadFile(filepath.Join(p.StateDir, packagesReportFile)); err == nil {
		var r PackageReport
		if json.Unmarshal(b, &r) == nil {
			p.report = &r
		}
	}
}

// Watch follows a run the helper is doing and checks for updates when it
// is time. It returns when ctx ends.
func (p *Packages) Watch(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		p.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick is one look: at the run, then at whether to check.
func (p *Packages) tick(ctx context.Context) {
	if p.followRun(ctx) {
		p.changed()
	}
	p.maybeCheck(ctx)
}

func (p *Packages) changed() {
	if p.Changed != nil {
		p.Changed()
	}
}

// followRun reads the run's state from the helper's file, and settles a
// run whose helper is gone. It reports whether anything changed.
func (p *Packages) followRun(ctx context.Context) bool {
	p.mu.Lock()
	p.loadLocked()
	cur := p.run
	p.mu.Unlock()
	if cur.ID == "" || cur.Finished() {
		return false
	}
	b, err := os.ReadFile(filepath.Join(p.StateDir, packagesRunFile))
	if err != nil {
		return false
	}
	var disk PackageRunStatus
	if json.Unmarshal(b, &disk) != nil || disk.ID != cur.ID {
		return false
	}
	if !disk.Finished() && disk.Unit != "" && !p.platform.active(disk.Unit) &&
		p.now().Sub(disk.StartedAt) > 10*time.Second {
		disk = p.settle(ctx, disk)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if disk.At.Equal(p.run.At) && disk.State == p.run.State && len(disk.Log) == len(p.run.Log) {
		return false
	}
	// Report progress at most every three seconds; an end at once.
	if !disk.Finished() && p.now().Sub(p.lastChange) < 3*time.Second {
		p.run = disk
		return false
	}
	p.run = disk
	p.lastChange = p.now()
	if disk.Finished() || disk.State == RunRebooting {
		// The helper listed what is still waiting once it was done.
		if b, err := os.ReadFile(filepath.Join(p.StateDir, packagesReportFile)); err == nil {
			var r PackageReport
			if json.Unmarshal(b, &r) == nil && (p.report == nil || r.CheckedAt.After(p.report.CheckedAt)) {
				p.report = &r
			}
		}
	}
	return true
}

// settle ends a run whose helper is no longer running without having said
// how it ended.
func (p *Packages) settle(ctx context.Context, st PackageRunStatus) PackageRunStatus {
	now := p.now().UTC()
	st.At, st.FinishedAt = now, now
	rebooted := st.BootID != "" && st.BootID != p.platform.bootID()
	switch {
	case st.State == RunRebooting && rebooted:
		st.State, st.Rebooted, st.RebootRequired, st.Step = RunDone, true, false, ""
		st.Message = "installed, and the server rebooted"
	case st.State == RunRebooting:
		st.State, st.Step = RunDone, ""
		st.RebootRequired = true
		st.Message = "installed; the reboot did not happen, so the server still needs one"
	default:
		st.State, st.Step = RunInterrupted, ""
		why := "the update stopped before it finished"
		if rebooted {
			why = "the server restarted while it was updating"
		}
		if err := p.platform.repair(ctx); err != nil {
			st.Message = why + "; finishing what the package manager left half done failed: " + err.Error() +
				". Run `dpkg --configure -a` (or your distribution's equivalent) on the server"
		} else {
			st.Message = why + ". What the package manager left half done was finished; " +
				"the packages still waiting are listed again"
		}
		p.mu.Lock()
		if p.checkID == "" {
			p.checkID = "after-" + st.ID
		}
		p.mu.Unlock()
	}
	if err := writeFileAtomic(filepath.Join(p.StateDir, packagesRunFile), mustJSON(st), 0o600); err != nil {
		p.Log.Error("packages: cannot keep the run's state", "error", err)
	}
	p.Log.Warn("packages: run settled without its helper", "id", st.ID, "state", st.State, "message", st.Message)
	return st
}

// maybeCheck looks for updates when the console asked for it now, or when
// it is time and the console wants the agent looking.
func (p *Packages) maybeCheck(ctx context.Context) {
	p.mu.Lock()
	running := p.run.ID != "" && !p.run.Finished()
	due := p.enabled && (p.report == nil || p.now().Sub(p.report.CheckedAt) >= packageCheckEvery)
	if p.report != nil && p.report.Error != "" && p.enabled {
		due = p.now().Sub(p.lastTry) >= packageCheckRetry
	} else if due && p.now().Sub(p.lastTry) < packageCheckRetry {
		due = false
	}
	asked := p.checkID
	if p.checking || running || (!due && asked == "") || p.platform.supported() != "" {
		if asked != "" && p.platform.supported() != "" {
			// Nothing to look with: answer so the console stops asking.
			p.checkDone, p.checkID, p.answered = asked, "", asked
		}
		p.mu.Unlock()
		return
	}
	p.checking, p.lastTry = true, p.now()
	p.mu.Unlock()
	p.changed()

	cctx, cancel := context.WithTimeout(ctx, packageCheckTimeout)
	r, err := p.platform.check(cctx)
	cancel()

	p.mu.Lock()
	p.checking = false
	if err != nil {
		// Keep what the last good check found, and say this one failed.
		prev := PackageReport{Supported: true}
		if p.report != nil {
			prev = *p.report
		}
		prev.Error = err.Error()
		prev.CheckedAt = p.now().UTC()
		r = prev
		p.Log.Warn("packages: check failed", "error", err)
	}
	p.report = &r
	if err := writeFileAtomic(filepath.Join(p.StateDir, packagesReportFile), mustJSON(r), 0o600); err != nil {
		p.Log.Error("packages: cannot keep the report", "error", err)
	}
	if asked != "" && p.checkID == asked {
		p.checkDone, p.checkID, p.answered = asked, "", asked
	}
	p.mu.Unlock()
	p.changed()
}

// errNoManager is a server with no package manager Argus knows.
var errNoManager = errors.New("this server has no package manager Argus knows (apt, dnf, yum or zypper)")

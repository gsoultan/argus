//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type linuxUpdates struct {
	executable string
	// run and lookPath are replaced in tests.
	run      func(name string, args ...string) error
	lookPath func(string) (string, error)
}

func hostUpdatePlatform(executable string) updatePlatform {
	return &linuxUpdates{
		executable: executable,
		run:        func(name string, args ...string) error { return exec.Command(name, args...).Run() },
		lookPath:   exec.LookPath,
	}
}

func (p *linuxUpdates) check() string {
	switch {
	case runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64":
		return "there is no agent package for this processor (" + runtime.GOARCH + ")"
	case os.Geteuid() != 0:
		return "the agent is not running as root, so it cannot install a package"
	case !systemdRunning():
		return "this server does not run systemd, which the update needs to restart the agent; " +
			"upgrade the package by hand"
	case p.format() == "":
		return "the agent was not installed from the argus-agent package, so there is no " +
			"package to upgrade; install it from the package, then updates work from here"
	}
	return ""
}

// restartCheck needs what the helper needs to restart the unit: root, to
// start it, and systemd, to restart it. No package is involved.
func (p *linuxUpdates) restartCheck() string {
	switch {
	case os.Geteuid() != 0:
		return "the agent is not running as root, so it cannot restart itself"
	case !systemdRunning():
		return "this server does not run systemd, which restarting the agent needs; " +
			"restart it by hand"
	}
	return ""
}

// format is the package manager that owns argus-agent here.
func (p *linuxUpdates) format() string {
	checks := []struct {
		format, tool string
		args         []string
	}{
		{"deb", "dpkg-query", []string{"-W", "-f=${Status}", "argus-agent"}},
		{"rpm", "rpm", []string{"-q", "argus-agent"}},
		{"apk", "apk", []string{"info", "-e", "argus-agent"}},
	}
	for _, c := range checks {
		if _, err := p.lookPath(c.tool); err != nil {
			continue
		}
		if c.format == "deb" {
			out, err := exec.Command(c.tool, c.args...).Output()
			if err == nil && strings.Contains(string(out), "ok installed") {
				return "deb"
			}
			continue
		}
		if p.run(c.tool, c.args...) == nil {
			return c.format
		}
	}
	return ""
}

// packageBusy names the process holding dpkg's lock, if one does. The
// update waits for it rather than failing on it.
func (p *linuxUpdates) packageBusy(format string) string {
	if format != "deb" {
		return ""
	}
	return dpkgLockHolder()
}

// dpkgLocks are the locks dpkg and its frontends (apt, unattended-upgrades)
// take, frontend first.
var dpkgLocks = []string{"/var/lib/dpkg/lock-frontend", "/var/lib/dpkg/lock"}

// dpkgLockHolder names the process holding one of dpkg's locks, like
// "process 812, unattended-upgr", or "" when neither is held. It only asks
// who holds them: it never takes them, and never removes them.
func dpkgLockHolder() string {
	for _, path := range dpkgLocks {
		if who := lockHolder(path); who != "" {
			return who
		}
	}
	return ""
}

func lockHolder(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil || lk.Type == syscall.F_UNLCK {
		return ""
	}
	if lk.Pid <= 0 {
		return "another process"
	}
	who := "process " + strconv.Itoa(int(lk.Pid))
	if comm, err := os.ReadFile("/proc/" + strconv.Itoa(int(lk.Pid)) + "/comm"); err == nil {
		if name := strings.TrimSpace(string(comm)); name != "" {
			who += ", " + name
		}
	}
	return who
}

// isPackageLocked reports whether a failed install failed only because
// another package install held the lock: nothing was installed then.
func isPackageLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "lock was locked") || strings.Contains(msg, "lock is locked") ||
		strings.Contains(msg, "unable to lock") || strings.Contains(msg, "status database is locked")
}

func (p *linuxUpdates) fileName(version string) string {
	// The names goreleaser gives the packages; see the install script.
	return fmt.Sprintf("argus-agent_%s_linux_%s.%s", version, runtime.GOARCH, p.format())
}

// launch runs the helper in a transient unit of its own: outside the
// agent's sandbox, which cannot write /usr, and outside its cgroup, which
// the restart is about to stop. It runs from a copy of this binary, because
// the package it installs replaces the one at /usr/bin.
func (p *linuxUpdates) launch(ctx context.Context, planPath string) error {
	helper := planPath[:strings.LastIndex(planPath, "/")] + "/argus-agent-update"
	if err := copyFile(p.executable, helper, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "systemd-run", "--quiet", "--collect",
		"--unit=argus-agent-update", "--service-type=exec",
		"--description=Argus agent update", helper, "update-apply", "--plan", planPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

/* ── The helper ──────────────────────────────────────────────────────────── */

// Applier carries out an update plan. It is what `argus-agent update-apply`
// runs, outside the agent, as root.
type Applier struct {
	Plan UpdatePlan
	Log  *slog.Logger
	// Run runs a command; nil is the real one. Replaced in tests.
	Run func(name string, args ...string) error
	// ReadyWait overrides readyWait in tests.
	ReadyWait time.Duration
	// Restarts reads how many times systemd has restarted the unit; nil is
	// the real one, and a test that sets Run without it skips the check.
	Restarts func() int
	// LockWait and LockPoll override lockWait and lockPoll in tests.
	LockWait, LockPoll time.Duration
	// LockHolder names who holds the package lock; nil is the real look.
	LockHolder func() string
}

const (
	// lockWait is how long the helper keeps trying an install that another
	// package install is in the way of. The agent already waited for the
	// lock before it began, so this covers one that started in between;
	// logins wait all the while, so it is short.
	lockWait = 2 * time.Minute
	lockPoll = 5 * time.Second
)

func (a *Applier) lockHolder() string {
	if a.LockHolder != nil {
		return a.LockHolder()
	}
	if a.Run != nil {
		return ""
	}
	return dpkgLockHolder()
}

func (a *Applier) restarts() int {
	if a.Restarts != nil {
		return a.Restarts()
	}
	if a.Run != nil {
		return 0
	}
	out, err := exec.Command("systemctl", "show", "-p", "NRestarts", "--value", a.Plan.Unit).Output()
	if err != nil {
		return 0
	}
	n := 0
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

// RunUpdateApply is `argus-agent update-apply --plan <path>`.
func RunUpdateApply(planPath string, stdout io.Writer) int {
	log := slog.New(slog.NewTextHandler(stdout, nil))
	b, err := os.ReadFile(planPath)
	if err != nil {
		log.Error("update: cannot read the plan", "error", err)
		return 1
	}
	var plan UpdatePlan
	if err := json.Unmarshal(b, &plan); err != nil {
		log.Error("update: cannot read the plan", "error", err)
		return 1
	}
	a := &Applier{Plan: plan, Log: log}
	if a.Apply() != UpdateDone {
		return 1
	}
	return 0
}

// Apply installs the new version, restarts the agent, and checks it came
// back; if it did not, it puts the old version back. It returns the state it
// ended in, which it has also written to update.json.
func (a *Applier) Apply() string {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	p := a.Plan
	if p.Restart {
		return a.applyRestart()
	}
	status := UpdateStatus{ID: p.ID, From: p.From, Target: p.Target, State: UpdateRestarting,
		Message: "installing " + p.Target}
	a.write(status)

	started := time.Now()
	reason := ""
	if err := a.install(p.Package); isPackageLocked(err) {
		return a.locked(status, err)
	} else if err != nil {
		reason = "the package for " + p.Target + " would not install: " + err.Error()
	} else if err := a.restart(); err != nil {
		reason = "the agent would not restart on " + p.Target + ": " + err.Error()
	} else if err := a.awaitReady(p.Target, started); err != nil {
		reason = p.Target + " " + err.Error()
	}
	if reason == "" {
		took := time.Since(started).Round(time.Second)
		status.State = UpdateDone
		status.Message = fmt.Sprintf("updated from %s to %s; back in %s", p.From, p.Target, took)
		a.finish(status)
		return UpdateDone
	}

	a.Log.Error("update: "+reason+"; going back", "to", p.From)
	status.Message = reason + "; going back to " + p.From
	a.write(status)
	back := time.Now()
	err := a.goBack()
	if err == nil {
		err = a.restart()
	}
	if err == nil {
		err = a.awaitReady(p.From, back)
	}
	if err != nil {
		status.State = UpdateFailed
		status.Message = reason + ", and going back to " + p.From + " did not work either: " +
			err.Error() + ". Log in with a break-glass account and run: sudo systemctl status argus-agent"
		a.finish(status)
		return UpdateFailed
	}
	status.State = UpdateRolledBack
	status.Message = reason + "; went back to " + p.From
	a.finish(status)
	return UpdateRolledBack
}

// locked ends an update whose package never installed because another
// package install held dpkg's lock the whole time. Nothing changed, so there
// is nothing to go back to: the agent is restarted on the version it runs,
// which lets go of the logins it was holding, and the update is marked as
// one to ask again.
func (a *Applier) locked(status UpdateStatus, err error) string {
	p := a.Plan
	holder := a.lockHolder()
	if holder == "" {
		holder = "it has finished since"
	}
	what := "another package install was running on this server (" + holder + "), so " +
		p.Target + " was not installed and nothing changed"
	a.Log.Warn("update: "+what, "error", err)
	back := time.Now()
	rerr := a.restart()
	if rerr == nil {
		rerr = a.awaitReady(p.From, back)
	}
	status.Retry = true
	if rerr != nil {
		status.State = UpdateFailed
		status.Message = what + ", but the agent did not come back on " + p.From + ": " + rerr.Error() +
			". Log in with a break-glass account and run: sudo systemctl status argus-agent"
		a.finish(status)
		return UpdateFailed
	}
	status.State = UpdateFailed
	status.Message = what + "; it is still on " + p.From + ". Update it again once that install " +
		"has finished: automatic updates try again on their own"
	a.finish(status)
	return UpdateFailed
}

// applyRestart restarts the agent on the version it runs and checks it came
// back. There is nothing to go back to, so one that does not come back is a
// failure, with the way in to look.
func (a *Applier) applyRestart() string {
	p := a.Plan
	status := UpdateStatus{ID: p.ID, From: p.From, Target: p.Target, Restart: true,
		State: UpdateRestarting, Message: "restarting"}
	a.write(status)

	started := time.Now()
	err := a.restart()
	if err == nil {
		err = a.awaitReady(p.Target, started)
	} else {
		err = fmt.Errorf("would not restart: %w", err)
	}
	if err != nil {
		status.State = UpdateFailed
		status.Message = "the agent " + err.Error() + ". Log in with a break-glass account and " +
			"run: sudo systemctl status argus-agent"
		a.finish(status)
		return UpdateFailed
	}
	status.State = UpdateDone
	status.Message = fmt.Sprintf("restarted; back in %s", time.Since(started).Round(time.Second))
	a.finish(status)
	return UpdateDone
}

func (a *Applier) run(name string, args ...string) error {
	if a.Run != nil {
		return a.Run(name, args...)
	}
	cmd := exec.Command(name, args...)
	// The new package's postinstall restarts the agent itself unless it is
	// told the update is doing that.
	cmd.Env = append(os.Environ(), "ARGUS_AGENT_UPDATING=1", "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return fmt.Errorf("%s: %v: %s", name, err, msg)
	}
	return nil
}

func (a *Applier) install(pkg string) error {
	switch a.Plan.Format {
	case "deb":
		return a.installDeb(pkg)
	case "rpm":
		return a.run("rpm", "-U", "--replacepkgs", "--oldpackage", pkg)
	case "apk":
		return a.run("apk", "add", "--allow-untrusted", pkg)
	}
	return fmt.Errorf("no way to install a %q package", a.Plan.Format)
}

// installDeb runs dpkg, trying again for a while when another package
// install holds the lock. dpkg fails at once on a held lock, before it
// changes anything, so trying again is safe. The lock is never removed:
// that would break the install holding it.
func (a *Applier) installDeb(pkg string) error {
	wait, poll := a.LockWait, a.LockPoll
	if wait == 0 {
		wait = lockWait
	}
	if poll == 0 {
		poll = lockPoll
	}
	deadline := time.Now().Add(wait)
	said := false
	for {
		err := a.run("dpkg", "-i", "--force-confold", pkg)
		if !isPackageLocked(err) || time.Now().After(deadline) {
			return err
		}
		if !said {
			said = true
			holder := a.lockHolder()
			if holder == "" {
				holder = "another process"
			}
			a.Log.Warn("update: another package install holds dpkg's lock; waiting for it", "holder", holder)
			// Also keeps the status fresh, so the agent does not give up
			// on this helper while it waits.
			a.write(UpdateStatus{ID: a.Plan.ID, From: a.Plan.From, Target: a.Plan.Target,
				State: UpdateRestarting, Message: "waiting for another package install on this " +
					"server to finish (" + holder + ")"})
		}
		time.Sleep(poll)
	}
}

// goBack reinstalls the version that was running, or puts its binary back
// when there was no package to be had.
func (a *Applier) goBack() error {
	if a.Plan.Rollback != "" {
		if err := a.install(a.Plan.Rollback); err == nil {
			return nil
		} else {
			a.Log.Error("update: the previous package would not install; restoring the binary", "error", err)
		}
	}
	if a.Plan.Previous == "" {
		return errors.New("there is nothing to go back to")
	}
	if a.Run != nil {
		return a.Run("restore", a.Plan.Previous, a.Plan.Executable)
	}
	tmp := a.Plan.Executable + ".argus-rollback"
	if err := copyFile(a.Plan.Previous, tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, a.Plan.Executable)
}

func (a *Applier) restart() error {
	_ = a.run("systemctl", "daemon-reload")
	// A version that kept exiting may have hit systemd's start limit, which
	// would refuse the restart that puts the old one back.
	_ = a.run("systemctl", "reset-failed", a.Plan.Unit)
	// Logins arriving now wait for the agent rather than being refused.
	if a.Plan.Marker != "" {
		_ = os.MkdirAll(a.Plan.Marker[:strings.LastIndex(a.Plan.Marker, "/")], 0o755)
		_ = os.WriteFile(a.Plan.Marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	}
	return a.run("systemctl", "restart", a.Plan.Unit)
}

// awaitReady waits for version to say it is serving, after since.
func (a *Applier) awaitReady(version string, since time.Time) error {
	wait := a.ReadyWait
	if wait == 0 {
		wait = readyWait
	}
	base := a.restarts()
	return awaitAgentReady(a.Plan, version, since, wait, func() int { return a.restarts() - base })
}

func (a *Applier) write(s UpdateStatus) {
	// Dated, because the agent gives up on a restart that has said nothing
	// for helperLost.
	s.At = time.Now().UTC()
	if err := WriteUpdateStatus(a.Plan.StateDir, s); err != nil {
		a.Log.Error("update: cannot write the update's state", "error", err)
	}
}

func (a *Applier) finish(s UpdateStatus) {
	a.write(s)
	if a.Plan.Marker != "" {
		_ = os.Remove(a.Plan.Marker)
	}
	// The packages and the copies; update.json and the log stay.
	for _, f := range []string{a.Plan.Package, a.Plan.Rollback, a.Plan.Previous} {
		if f != "" {
			_ = os.Remove(f)
		}
	}
	if s.State == UpdateFailed {
		a.Log.Error("update: " + s.Message)
	} else {
		a.Log.Info("update: " + s.Message)
	}
}

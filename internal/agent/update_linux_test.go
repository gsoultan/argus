//go:build linux

package agent

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The helper, with commands faked: a new version that never reconnects is
// replaced by the one that was running, and the outcome says so.
func TestAnUpdateThatDoesNotReconnectGoesBack(t *testing.T) {
	dir := t.TempDir()
	plan := UpdatePlan{ID: "r1", From: "1.0.0", Target: "1.1.0", Format: "deb",
		Package: dir + "/new.deb", Rollback: dir + "/old.deb", Previous: dir + "/prev",
		Executable: dir + "/argus-agent", Unit: "argus-agent", StateDir: dir,
		Marker: dir + "/run/updating", NeedTunnel: true}
	var ran []string
	installed := ""
	a := &Applier{Plan: plan, ReadyWait: 700 * time.Millisecond, Run: func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		switch {
		case name == "dpkg":
			installed = args[len(args)-1]
		case name == "systemctl" && args[0] == "restart":
			// The new version answers SSH but never reaches the control
			// plane; the old one does both.
			if installed == plan.Package {
				WriteReady(dir, "1.1.0", true, false)
			} else {
				WriteReady(dir, "1.0.0", true, true)
			}
		}
		return nil
	}}
	if got := a.Apply(); got != UpdateRolledBack {
		t.Fatalf("Apply = %s; ran %v", got, ran)
	}
	s, _ := ReadUpdateStatus(dir)
	if s.State != UpdateRolledBack || !strings.Contains(s.Message, "did not reconnect to Argus") ||
		!strings.Contains(s.Message, "went back to 1.0.0") {
		t.Errorf("status = %+v", s)
	}
	if installed != plan.Rollback {
		t.Errorf("the old package was not reinstalled: %v", ran)
	}
}

func TestAnUpdateThatReconnectsIsDone(t *testing.T) {
	dir := t.TempDir()
	plan := UpdatePlan{ID: "r1", From: "1.0.0", Target: "1.1.0", Format: "rpm",
		Package: dir + "/new.rpm", StateDir: dir, Unit: "argus-agent", NeedTunnel: true}
	var ran []string
	var midway UpdateStatus
	a := &Applier{Plan: plan, ReadyWait: 2 * time.Second, Run: func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if name == "systemctl" && args[0] == "restart" {
			// The new agent, starting while the helper still checks on it,
			// must see a restart in progress -- not one it gives up on. A
			// helper that wrote no time made it declare the update failed
			// and overwrite the helper's own result (found in the drill).
			fresh := NewUpdater(dir, "1.1.0", dir+"/argus-agent", nil)
			fresh.platform = &fakeUpdates{}
			midway = fresh.Status()
			WriteReady(dir, "1.1.0", true, true)
		}
		return nil
	}}
	if got := a.Apply(); got != UpdateDone {
		t.Fatalf("Apply = %s; ran %v", got, ran)
	}
	if midway.State != UpdateRestarting {
		t.Errorf("the new agent read the restart as %+v", midway)
	}
	if !strings.Contains(strings.Join(ran, "\n"), "rpm -U --replacepkgs --oldpackage "+plan.Package) {
		t.Errorf("ran %v", ran)
	}
	if s, _ := ReadUpdateStatus(dir); s.State != UpdateDone || !strings.Contains(s.Message, "updated from 1.0.0 to 1.1.0") {
		t.Errorf("status = %+v", s)
	}
}

// A new version that keeps exiting is given up on after a few tries, not
// after the whole wait: logins wait for the agent all the while.
func TestAnUpdateThatKeepsCrashingGoesBackSoon(t *testing.T) {
	dir := t.TempDir()
	plan := UpdatePlan{ID: "r1", From: "1.0.0", Target: "1.1.0", Format: "deb",
		Package: dir + "/new.deb", Rollback: dir + "/old.deb", StateDir: dir, Unit: "argus-agent"}
	installed, crashes := "", 0
	a := &Applier{Plan: plan, ReadyWait: time.Minute,
		Restarts: func() int {
			if installed == plan.Package {
				crashes++ // systemd restarting a binary that exits at once
			}
			return crashes
		},
		Run: func(name string, args ...string) error {
			switch {
			case name == "dpkg":
				installed = args[len(args)-1]
			case name == "systemctl" && args[0] == "restart" && installed == plan.Rollback:
				WriteReady(dir, "1.0.0", true, true)
			}
			return nil
		}}
	began := time.Now()
	if got := a.Apply(); got != UpdateRolledBack {
		t.Fatalf("Apply = %s", got)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("waited %s for a version that kept exiting", took)
	}
	if s, _ := ReadUpdateStatus(dir); !strings.Contains(s.Message, "exited 3 times") {
		t.Errorf("status = %+v", s)
	}
}

// A restart installs nothing and is done once the agent is back and
// reconnected.
func TestARestartThatComesBackIsDone(t *testing.T) {
	dir := t.TempDir()
	plan := UpdatePlan{ID: "r1", From: "1.0.0", Target: "1.0.0", Restart: true, StateDir: dir,
		Unit: "argus-agent", Marker: dir + "/run/updating", NeedTunnel: true}
	var ran []string
	a := &Applier{Plan: plan, ReadyWait: 2 * time.Second, Run: func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if name == "systemctl" && args[0] == "restart" {
			WriteReady(dir, "1.0.0", true, true)
		}
		return nil
	}}
	if got := a.Apply(); got != UpdateDone {
		t.Fatalf("Apply = %s; ran %v", got, ran)
	}
	for _, r := range ran {
		if strings.HasPrefix(r, "dpkg") || strings.HasPrefix(r, "rpm") || strings.HasPrefix(r, "apk") {
			t.Errorf("a restart installed something: %v", ran)
		}
	}
	if !strings.Contains(strings.Join(ran, "\n"), "systemctl restart argus-agent") {
		t.Errorf("ran %v", ran)
	}
	s, _ := ReadUpdateStatus(dir)
	if s.State != UpdateDone || !s.Restart || !strings.HasPrefix(s.Message, "restarted; back in") {
		t.Errorf("status = %+v", s)
	}
}

// A restart that does not reconnect has nothing to go back to: it fails and
// says how to look.
func TestARestartThatDoesNotReconnectFails(t *testing.T) {
	dir := t.TempDir()
	plan := UpdatePlan{ID: "r1", From: "1.0.0", Target: "1.0.0", Restart: true, StateDir: dir,
		Unit: "argus-agent", NeedTunnel: true}
	a := &Applier{Plan: plan, ReadyWait: 700 * time.Millisecond, Run: func(name string, args ...string) error {
		if name == "systemctl" && args[0] == "restart" {
			WriteReady(dir, "1.0.0", true, false)
		}
		return nil
	}}
	if got := a.Apply(); got != UpdateFailed {
		t.Fatalf("Apply = %s", got)
	}
	s, _ := ReadUpdateStatus(dir)
	if !strings.Contains(s.Message, "did not reconnect to Argus") ||
		!strings.Contains(s.Message, "systemctl status argus-agent") {
		t.Errorf("status = %+v", s)
	}
}

// dpkg refuses to run while unattended-upgrades holds its lock. The helper
// tries again, and installs once the lock is free.
func TestAnUpdateWaitsForDpkgsLock(t *testing.T) {
	dir := t.TempDir()
	plan := UpdatePlan{ID: "r1", From: "1.0.0", Target: "1.1.0", Format: "deb",
		Package: dir + "/new.deb", Rollback: dir + "/old.deb", StateDir: dir, Unit: "argus-agent"}
	tries := 0
	a := &Applier{Plan: plan, ReadyWait: 2 * time.Second, LockWait: 5 * time.Second,
		LockPoll: 10 * time.Millisecond, LockHolder: func() string { return "process 812, unattended-upgr" },
		Run: func(name string, args ...string) error {
			switch {
			case name == "dpkg":
				tries++
				if tries < 3 {
					return dpkgLockedErr
				}
			case name == "systemctl" && args[0] == "restart":
				WriteReady(dir, "1.1.0", true, true)
			}
			return nil
		}}
	if got := a.Apply(); got != UpdateDone {
		t.Fatalf("Apply = %s after %d tries", got, tries)
	}
	if tries != 3 {
		t.Errorf("dpkg ran %d times", tries)
	}
}

var dpkgLockedErr = errors.New("dpkg: exit status 2: dpkg: error: dpkg frontend lock was locked by " +
	"/usr/bin/python3.14 process with pid 485091")

// A lock that stays held changes nothing: no package goes back in, the agent
// restarts on the version it runs, and the outcome says plainly what was in
// the way and that it can be asked again.
func TestAnUpdateTheLockStaysOnChangesNothing(t *testing.T) {
	dir := t.TempDir()
	plan := UpdatePlan{ID: "r1", From: "1.0.0", Target: "1.1.0", Format: "deb",
		Package: dir + "/new.deb", Rollback: dir + "/old.deb", StateDir: dir, Unit: "argus-agent"}
	var ran []string
	a := &Applier{Plan: plan, ReadyWait: 2 * time.Second, LockWait: 50 * time.Millisecond,
		LockPoll: 10 * time.Millisecond, LockHolder: func() string { return "process 812, unattended-upgr" },
		Run: func(name string, args ...string) error {
			ran = append(ran, name+" "+strings.Join(args, " "))
			switch {
			case name == "dpkg":
				return dpkgLockedErr
			case name == "systemctl" && args[0] == "restart":
				WriteReady(dir, "1.0.0", true, true)
			}
			return nil
		}}
	if got := a.Apply(); got != UpdateFailed {
		t.Fatalf("Apply = %s", got)
	}
	for _, r := range ran {
		if strings.Contains(r, plan.Rollback) {
			t.Errorf("reinstalled the old package though nothing changed: %v", ran)
		}
	}
	if !strings.Contains(strings.Join(ran, "\n"), "systemctl restart argus-agent") {
		t.Errorf("the agent holding logins was not restarted: %v", ran)
	}
	s, _ := ReadUpdateStatus(dir)
	if !s.Retry || !strings.Contains(s.Message, "another package install was running on this server "+
		"(process 812, unattended-upgr)") || !strings.Contains(s.Message, "nothing changed") ||
		!strings.Contains(s.Message, "still on 1.0.0") {
		t.Errorf("status = %+v", s)
	}
}

// Another process holding a lock on dpkg's lock file is found, by pid and
// name; a free one is not.
func TestLockHolderFindsTheProcess(t *testing.T) {
	path := t.TempDir() + "/lock-frontend"
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if who := lockHolder(path); who != "" {
		t.Fatalf("a free lock is held by %q", who)
	}
	// fcntl locks belong to a process, so the holder must be another one.
	hold := exec.Command("python3", "-c", `import fcntl,sys,time
f=open(sys.argv[1],"w"); fcntl.lockf(f, fcntl.LOCK_EX); print("held", flush=True); time.sleep(30)`, path)
	out, _ := hold.StdoutPipe()
	if err := hold.Start(); err != nil {
		t.Skip("no python3 here")
	}
	defer func() { _ = hold.Process.Kill(); _ = hold.Wait() }()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(out, buf); err != nil {
		t.Skip("the holder did not start")
	}
	who := lockHolder(path)
	if !strings.HasPrefix(who, "process "+strconv.Itoa(hold.Process.Pid)) {
		t.Errorf("holder = %q, want process %d", who, hold.Process.Pid)
	}
}

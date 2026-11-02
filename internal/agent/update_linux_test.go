//go:build linux

package agent

import (
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

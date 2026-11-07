package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Installing and removing software: the run.

A run is a package run (packages_apply.go) with mode install or remove, so it
travels the same way an update does: on the heartbeat, in a unit of its own,
its state in a file the agent follows, settled by the agent when its helper
vanishes. What differs is the work:

 1. Simulate again, exactly as the preview did, and compare the hash with
    the one the console sent. A person said yes to that list of changes;
    if the server would now do something else (a newer version arrived, a
    dependency went, someone installed something by hand), stop before
    anything changes.
 2. Run the package manager on the simulation's own targets: the packages
    asked for, and for a removal the ones only they needed.
 3. For an install, start the services the new packages ship, or keep
    them stopped, as asked, the same way on every distribution: Debian
    starts services as it installs them, so "keep stopped" sets a
    policy-rc.d for the run; RHEL and SUSE never start them, so "start"
    enables them.
 4. Say exactly what changed, from the package database before and after.
*/

// policyRCD is Debian's hook for "may a package start its service now".
// Tests move it.
var policyRCD = "/usr/sbin/policy-rc.d"

func (a *PackageApplier) applySoftware(ctx context.Context) PackageRunStatus {
	m := asSW(detectManager(a.Look))
	if m == nil {
		return a.fail(errNoManager.Error())
	}
	action := a.Plan.Mode
	if action == PackagesInstall {
		a.update(func(s *PackageRunStatus) {
			s.Step, s.Message = "refresh", "refreshing the package lists"
		})
		refreshIfOld(ctx, a.Run, m)
	}
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "simulate", "checking the server still answers what was approved"
	})
	pv, targets, err := previewSoftware(ctx, a.Run, m, action, a.Plan.Items, a.Plan.Purge)
	if err != nil {
		return a.fail(err.Error())
	}
	if pv.Hash != a.Plan.Expect {
		return a.fail("the server has changed since this was previewed: installing or removing it now " +
			"would not do what was approved. Nothing was changed; preview it again.")
	}
	if pv.Blocked != "" {
		return a.fail(pv.Blocked)
	}

	before := map[string]string{}
	for _, c := range pv.Changes {
		before[c.Name] = c.From
	}
	verb := "installing"
	if action == PackagesRemove {
		verb = "removing"
	}
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Planned = action, len(pv.Changes)
		s.Message = fmt.Sprintf("%s %s", verb, strings.Join(itemNames(a.Plan.Items), ", "))
	})

	var env []string
	restore := func() {}
	if action == PackagesInstall && !a.Plan.StartServices {
		if _, ok := m.(aptManager); ok {
			restore = a.holdServices()
		}
	}
	err = m.apply(ctx, a.Run, a.lw, action, targets, a.Plan.Purge, env)
	restore()
	a.lw.Flush()
	changed := a.softwareChanges(ctx, m, pv.Changes, before)
	a.update(func(s *PackageRunStatus) { s.Upgraded = changed })
	if err != nil {
		a.writeReport(ctx, m)
		return a.fail(err.Error())
	}

	var started []string
	if action == PackagesInstall {
		var added []string
		for _, c := range changed {
			if c.From == "" {
				added = append(added, c.Name)
			}
		}
		units := m.unitsOf(ctx, a.Run, added)
		if a.Plan.StartServices && len(units) > 0 {
			a.update(func(s *PackageRunStatus) {
				s.Step, s.Message = "services", "starting "+strings.Join(units, ", ")
			})
			for _, u := range units {
				if protectedService(u) {
					continue
				}
				code, err := a.Run(ctx, a.lw, nil, "systemctl", "enable", "--now", u)
				if err != nil || code != 0 {
					fmt.Fprintf(a.lw, "argus: could not start %s\n", u)
					continue
				}
				started = append(started, u)
			}
		} else if len(units) > 0 {
			fmt.Fprintf(a.lw, "argus: left stopped, as asked: %s\n", strings.Join(units, ", "))
			if _, ok := m.(aptManager); ok {
				// Debian enabled them as it installed them; asked to keep
				// them stopped, keep them from starting at boot too.
				for _, u := range units {
					_, _ = a.Run(ctx, a.lw, nil, "systemctl", "disable", "--now", u)
				}
			}
		}
	}
	a.writeReport(ctx, m)
	msg := fmt.Sprintf("%s %d package%s", map[string]string{PackagesInstall: "installed or changed",
		PackagesRemove: "removed"}[action], len(changed), plural(len(changed)))
	if len(started) > 0 {
		msg += ", started " + strings.Join(started, ", ")
	}
	return a.finish(RunDone, msg, func(s *PackageRunStatus) { s.Started = started })
}

// holdServices keeps Debian from starting services while packages are
// installed, by a policy-rc.d that says no, and returns how to undo it. A
// policy-rc.d someone already has is theirs: it is left alone and decides.
func (a *PackageApplier) holdServices() func() {
	if _, err := os.Lstat(policyRCD); err == nil {
		fmt.Fprintf(a.lw, "argus: %s already exists; it decides whether services start\n", policyRCD)
		return func() {}
	}
	if err := os.WriteFile(policyRCD, []byte("#!/bin/sh\n# Argus: keep services stopped while installing.\nexit 101\n"), 0o755); err != nil {
		fmt.Fprintf(a.lw, "argus: cannot keep services stopped: %v\n", err)
		return func() {}
	}
	return func() { _ = os.Remove(policyRCD) }
}

// softwareChanges is what the package database says changed, for the
// packages the simulation named.
func (a *PackageApplier) softwareChanges(ctx context.Context, m swManager, planned []tunnel.SoftwareChange,
	before map[string]string) []PackageChange {
	after := map[string]string{}
	if pkgs, err := m.installedList(ctx, a.Run); err == nil {
		for _, p := range pkgs {
			after[p.Name] = p.Version
			after[p.Name+":"+p.Arch] = p.Version
		}
	}
	var out []PackageChange
	for _, c := range planned {
		now := after[c.Name]
		if now == before[c.Name] {
			continue
		}
		out = append(out, PackageChange{Name: c.Name, From: before[c.Name], To: now})
	}
	return out
}

func itemNames(items []tunnel.SoftwareItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

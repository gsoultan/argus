package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

/* ── The helper: a release upgrade ───────────────────────────────────────── */

// ubuntuOverrideName is Argus's setting for do-release-upgrade, there only
// while a run is going.
const ubuntuOverrideName = "argus.cfg"

// ubuntuOverride keeps everything the server had: by default
// do-release-upgrade removes what the new release no longer ships, which on a
// server Argus manages can be the database client its backups run, or
// anything a person installed on purpose.
const ubuntuOverride = `# Written by Argus for a release upgrade asked for from the console, and
# removed when it ends. Nothing installed is removed because the new release
# no longer ships it; that clean-up is left to a person.
[Distro]
RemoveObsoletes=no
`

// releaseEnv is aptEnv plus keeping a configuration file managed by ucf, as
// dpkg's --force-confold keeps one dpkg manages: sshd_config, with the
// ForceCommand that sends logins through Argus, is one of those.
// PYTHONUNBUFFERED makes do-release-upgrade and leapp, both Python, print
// each line as it happens rather than in blocks once a pipe's buffer fills,
// which on a quiet upgrade can be many minutes.
var releaseEnv = append(append([]string{}, aptEnv...), "UCF_FORCE_CONFFOLD=1", "RELEASE_UPGRADER_NO_SCREEN=1",
	"PYTHONUNBUFFERED=1")

// applyRelease moves the server to the release the run was approved for,
// then reboots into it. The agent that comes back checks it got there.
func (a *PackageApplier) applyRelease(ctx context.Context) PackageRunStatus {
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "preflight", "checking the server is ready for a new release"
	})
	m := detectManager(a.Look)
	if m == nil {
		return a.unchanged(errNoManager.Error())
	}
	if err := m.refresh(ctx, a.Run, a.lw); err != nil {
		return a.unchanged(err.Error())
	}
	_, off := a.Now().Zone()
	r := PackageReport{CheckedAt: a.Now().UTC(), Manager: m.name(), Supported: true, UTCOffsetMinutes: off / 60}
	r, err := a.listInto(ctx, m, r)
	if err != nil {
		return a.unchanged(err.Error())
	}
	info := r.Release
	switch {
	case info == nil:
		return a.unchanged("the server does not say which release it runs (no /etc/os-release)")
	case info.Reason != "":
		return a.unchanged(info.Reason)
	case info.Next == "":
		return a.unchanged("no newer release is offered for " + info.Current + " now")
	case info.Next != a.Plan.Expect:
		return a.unchanged(fmt.Sprintf("the server now offers %s, not the %s this run was approved for; nothing was "+
			"changed. Look for updates again and ask again", info.Next, a.Plan.Expect),
			"Press Check for updates, so Argus sees what the server offers now.",
			"Ask for the upgrade to "+info.Next+" if that is the one you want.")
	case len(info.Blockers) > 0:
		return a.unchanged("not upgraded, nothing was changed: " + strings.Join(info.Blockers, "; "))
	}
	if info.Tool == releaseToolUbuntu || info.Tool == releaseToolDebian {
		if err := a.answerGrub(ctx); err != nil {
			return a.unchanged(err.Error())
		}
	}
	change := &ReleaseChange{Distro: info.Distro, From: info.Version, FromName: info.Current, To: info.Next,
		ToName: info.NextName, Tool: info.Tool}
	deps := snapshotDeps(ctx, a.Run, a.Look, managerFamily(m))
	if err := writeFileAtomic(filepath.Join(a.Plan.StateDir, packagesDir, releaseDepsFile), mustJSON(deps), 0o600); err != nil {
		return a.unchanged("cannot keep the list of what the agent uses on the server, so nothing was changed: " + err.Error())
	}
	fmt.Fprintf(a.lw, "argus: upgrading %s to %s with %s\n", info.Current, info.NextName, info.Tool)
	a.update(func(s *PackageRunStatus) {
		s.Release, s.Step = change, "upgrade"
		s.Message = "upgrading to " + info.NextName + "; this takes a while, and the server reboots at the end"
	})

	switch info.Tool {
	case releaseToolUbuntu:
		err = a.upgradeUbuntu(ctx, info)
	case releaseToolDebian:
		err = a.upgradeDebian(ctx, m, info)
	case releaseToolLeapp:
		err = a.upgradeLeapp(ctx)
	default:
		err = errors.New(releaseUnsupported)
	}
	a.lw.Flush()
	if err != nil {
		a.writeReport(ctx, m)
		return a.failErr(err, &RunOutcome{Changed: ChangedUnknown})
	}
	if info.Tool != releaseToolLeapp {
		// The upgrade has rewritten os-release by now. A tool that exited 0
		// without moving is not an upgrade.
		if now := osRelease()["VERSION_ID"]; !versionIs(now, info.Next) {
			a.writeReport(ctx, m)
			return a.fail(fmt.Sprintf("%s finished, but the server says it is on %s, not %s; look at %s",
				info.Tool, orUnknown(now), info.Next, change.logFile()))
		}
	}

	// An upgrader that exited 0 can still leave a package unconfigured (a
	// maintainer script that failed after it moved on). Finish it, and do
	// not reboot into a release dpkg has not finished.
	if _, refused := a.rebootRefusal(ctx); len(refused) > 0 {
		fmt.Fprintln(a.lw, "argus: dpkg left "+strings.Join(refused, ", ")+" unfinished; finishing them")
		_ = aptManager{}.repair(ctx, a.Run, a.lw)
		a.lw.Flush()
		if why, left := a.rebootRefusal(ctx); why != "" {
			a.writeReport(ctx, m)
			msg := fmt.Sprintf("%s moved the server to %s, but dpkg could not finish configuring %s, so the server "+
				"was not rebooted: it may not start again until they are finished (`sudo dpkg --configure -a`, "+
				"`sudo apt-get -f install`). Its log is %s", info.Tool, info.NextName, strings.Join(left, ", "),
				change.logFile())
			return a.failWith(msg, RunOutcome{Changed: ChangedYes, Why: msg, Next: unfinishedNext})
		}
	}

	msg := "upgraded to " + info.NextName + "; rebooting into it"
	if info.Tool == releaseToolLeapp {
		msg = "leapp is ready; rebooting into the upgrade, which runs before the server comes back"
	}
	a.update(func(s *PackageRunStatus) { s.State, s.Step, s.Message = RunRebooting, "reboot", msg })
	fmt.Fprintln(a.lw, "argus: "+msg)
	a.lw.Flush()
	if err := a.Reboot(); err != nil {
		return a.finish(RunDone, change.notRebooted()+" (the reboot failed: "+err.Error()+")",
			func(s *PackageRunStatus) { s.RebootRequired = true })
	}
	return a.snapshot()
}

// unchanged ends a release upgrade that stopped before it changed anything,
// with what to do next: what is given, or what fits most stops.
func (a *PackageApplier) unchanged(msg string, next ...string) PackageRunStatus {
	if len(next) == 0 {
		next = againSteps
	}
	return a.failWith(msg, RunOutcome{Changed: ChangedNo, Why: msg, Next: next})
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown release"
	}
	return s
}

// aptPartWay is what to say when an apt upgrade itself failed: first
// whether dpkg changed any package since the run started, read from its log.
// Nothing changed is said as that, with the tool's reason, and nothing is
// repaired. Otherwise Argus tries to finish what dpkg left half done and
// says whether that worked and what a person does next. The server is not
// rebooted: a boot loader left unconfigured may not boot.
func (a *PackageApplier) aptPartWay(ctx context.Context, tool string, code int, out, term []byte, log string) error {
	why := tail(out, 400)
	failed := dpkgFailed(term)
	changed := dpkgChangedSince(a.snapshot().StartedAt)
	if changed == ChangedNo {
		fmt.Fprintln(a.lw, "argus: "+tool+" stopped before dpkg changed any package; nothing was changed")
		a.lw.Flush()
		return &outcomeError{
			msg: fmt.Sprintf("%s failed (exit %d) before changing any package, so nothing was changed and the server "+
				"is still on its release. It said: %s. Its log is %s", tool, code, why, log),
			outcome: RunOutcome{Changed: ChangedNo, Why: why, Next: againSteps},
		}
	}
	fmt.Fprintln(a.lw, "argus: the upgrade stopped part way; finishing what dpkg left half done")
	repaired := aptManager{}.repair(ctx, a.Run, a.lw) == nil
	left := dpkgUnfinished(ctx, a.Run)
	repaired = repaired && len(left) == 0
	a.lw.Flush()
	failedOn := ""
	if len(failed) > 0 {
		failedOn = " dpkg failed on " + strings.Join(failed, ", ") + "."
	}
	if len(left) > 0 {
		failedOn += " Still half configured: " + strings.Join(left, ", ") + "."
	}
	if hasString(failed, "grub-pc") || hasString(left, "grub-pc") {
		failedOn += " grub-pc is among them, so the boot loader may not be installed: the server may not start " +
			"again if it reboots now."
	}
	next := []string{
		"Do not reboot yet.",
		"Run `sudo dpkg --configure -a` and `sudo apt-get -f install` until both finish without errors. If grub-pc " +
			"is what fails, run `sudo dpkg-reconfigure grub-pc` and tick the disk the server boots from.",
		"Run `sudo apt full-upgrade` to finish the release.",
		"Check that /etc/os-release names the new release, and only then reboot.",
	}
	said := "Argus tried to finish configuring the packages and could not. Do not reboot until " +
		"`sudo dpkg --configure -a` and `sudo apt-get -f install` finish without errors (if grub-pc is what " +
		"fails, `sudo dpkg-reconfigure grub-pc` and tick the disk the server boots from), then run " +
		"`sudo apt full-upgrade` to finish the release, and only then reboot."
	if repaired {
		next = append(next[:1:1], next[2:]...)
		said = "Argus then finished configuring the packages (dpkg --configure -a, apt-get -f install). Do not " +
			"reboot yet: run `sudo apt full-upgrade` to finish the release, check /etc/os-release names it, " +
			"and only then reboot."
	}
	where := "so the server is part way between releases"
	if changed == ChangedUnknown {
		where = "and dpkg's log could not be read, so the server may be part way between releases"
	}
	return &outcomeError{
		msg:     fmt.Sprintf("%s failed (exit %d), %s.%s %s Its log is %s. %s", tool, code, where, failedOn, said, log, why),
		outcome: RunOutcome{Changed: changed, Why: why, Next: next},
	}
}

func (a *PackageApplier) upgradeUbuntu(ctx context.Context, info *ReleaseInfo) error {
	cfg := filepath.Join(ubuntuUpgradeOverrides, ubuntuOverrideName)
	if err := os.MkdirAll(ubuntuUpgradeOverrides, 0o755); err != nil {
		return fmt.Errorf("cannot write the upgrade's settings: %w", err)
	}
	if err := writeFileAtomic(cfg, []byte(ubuntuOverride), 0o644); err != nil {
		return fmt.Errorf("cannot write the upgrade's settings: %w", err)
	}
	defer os.Remove(cfg)
	// What the upgrader writes to its own log this run, kept whole: its
	// output says little, and why it stopped is only there.
	mainLog := &tailer{files: ubuntuUpgradeLogs[:1], offsets: map[string]int64{}}
	mainLog.start()
	// Which package dpkg failed on is only in the terminal log.
	termLog := &tailer{files: ubuntuUpgradeLogs[1:], offsets: map[string]int64{}}
	termLog.start()
	var b bytes.Buffer
	stop := a.follow(ctx, "upgrading", ubuntuUpgradeLogs)
	code, err := a.Run(ctx, io.MultiWriter(&b, a.lw), releaseEnv, "do-release-upgrade",
		"-f", "DistUpgradeViewNonInteractive")
	stop()
	if err != nil {
		return err
	}
	if code == 0 {
		return nil
	}
	var logged bytes.Buffer
	mainLog.read(&logged)
	why, restored := distUpgradeFailure(logged.Bytes())
	if why == "" {
		why = tail(b.Bytes(), 400)
	}
	if restored && versionIs(osRelease()["VERSION_ID"], info.Version) {
		return &outcomeError{
			msg: fmt.Sprintf("do-release-upgrade stopped before upgrading anything and put the server back the way "+
				"it was, so it is still on %s and nothing was changed. It said: %s. Fix that, look for updates again, "+
				"and ask again; the whole log is %s", info.Current, why, ubuntuUpgradeLogs[0]),
			outcome: RunOutcome{Changed: ChangedNo, Why: why, Next: againSteps},
		}
	}
	var term bytes.Buffer
	termLog.read(&term)
	term.Write(b.Bytes())
	return a.aptPartWay(ctx, "do-release-upgrade", code, []byte(why), term.Bytes(),
		"/var/log/dist-upgrade/main.log (what dpkg printed is in apt-term.log beside it)")
}

// reDistUpgradeError is a line the upgrader logs when something is wrong:
//
//	2026-11-13 13:30:02,117 ERROR Dist-upgrade failed: 'E:Unable to correct problems, you have held broken packages.'
var reDistUpgradeError = regexp.MustCompile(`^(?:\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d+ )?(?:ERROR|CRITICAL) (.+)$`)

// Lines in the upgrader's log that name a package it wants gone but will
// not remove, and a package no source offers any more:
//
//	ERROR Dist-upgrade failed: 'The package postgresql-18-mobilitydb is marked for removal but it is in the removal deny list.'
//	ERROR Package libgdal39 has no priority set
var (
	reDistUpgradeDenied   = regexp.MustCompile(`The package (\S+) is marked for removal but it is in the removal deny list`)
	reDistUpgradeNoSource = regexp.MustCompile(`^Package (\S+) has no priority set$`)
)

// distUpgradeNoise are ERROR lines the upgrader logs on runs that go on fine.
var distUpgradeNoise = []string{"failed to import AptClone", "view.abort called"}

// distUpgradeFailure reads the upgrader's log for why it stopped: its last
// few errors, and whether it gave up before upgrading anything. It gave up
// early when it put the system back ("Restoring original system state") or
// could not work out the upgrade at all ("Could not calculate the upgrade"),
// which it does before it changes a package.
func distUpgradeFailure(log []byte) (why string, restored bool) {
	var errs, noSource []string
	denied := ""
	for _, line := range strings.Split(string(log), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Restoring original system state") ||
			strings.Contains(line, "Could not calculate the upgrade") {
			restored = true
		}
		m := reDistUpgradeError.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		e := strings.TrimSpace(m[1])
		if d := reDistUpgradeDenied.FindStringSubmatch(e); d != nil {
			denied = d[1]
		}
		if n := reDistUpgradeNoSource.FindStringSubmatch(e); n != nil {
			if !hasString(noSource, n[1]) {
				noSource = append(noSource, n[1])
			}
			continue
		}
		noise := false
		for _, s := range distUpgradeNoise {
			noise = noise || strings.Contains(e, s)
		}
		if noise {
			continue
		}
		if len(e) > 300 {
			e = e[:300] + "…"
		}
		if !hasString(errs, e) {
			errs = append(errs, e)
		}
	}
	if denied != "" {
		why = "the new release has no version of " + denied + ", so upgrading would remove it, and the upgrader " +
			"refuses to remove it (its deny list protects database server packages such as postgresql-*)"
		if len(noSource) > 0 {
			why += ". No source offers these installed packages for the new release either: " + strings.Join(noSource, ", ")
		}
		why += ". They usually come from a third-party source, such as apt.postgresql.org, which the upgrader " +
			"turns off while it runs. Remove the ones you do not use, or make sure that source has packages for " +
			"the new release, then try again"
		return why, restored
	}
	if len(errs) > 3 {
		errs = errs[len(errs)-3:]
	}
	return strings.Join(errs, "; "), restored
}

func (a *PackageApplier) upgradeDebian(ctx context.Context, m pkgManager, info *ReleaseInfo) error {
	major, _, _ := strings.Cut(info.Version, ".")
	n, ok := debianNext[major]
	if !ok {
		return fmt.Errorf("no upgrade is known from Debian %s", info.Version)
	}
	from, to := n[1], n[2]
	backup := filepath.Join(a.Plan.StateDir, packagesDir, "release-"+a.Plan.ID, "apt")
	ch, err := pointSourcesAt(aptSourcesRoot, backup, from, to)
	if err != nil {
		_ = restoreSources(aptSourcesRoot, backup)
		return fmt.Errorf("cannot point apt at %s; nothing was changed: %w", to, err)
	}
	if len(ch.Rewritten) == 0 {
		_ = restoreSources(aptSourcesRoot, backup)
		return fmt.Errorf("no apt source names Debian's %s archive, so there is nothing to point at %s; "+
			"nothing was changed", from, to)
	}
	fmt.Fprintf(a.lw, "argus: apt now reads %s from %s\n", to, strings.Join(ch.Rewritten, ", "))
	for _, d := range ch.Disabled {
		fmt.Fprintf(a.lw, "argus: turned off for the upgrade, not Debian's: %s\n", d)
	}
	if err := m.refresh(ctx, a.Run, a.lw); err != nil {
		// Nothing is installed yet: put the old release back.
		if rerr := restoreSources(aptSourcesRoot, backup); rerr != nil {
			return fmt.Errorf("the %s package lists could not be downloaded (%v), and the old sources could not be "+
				"put back (%v); fix /etc/apt by hand", to, err, rerr)
		}
		_ = m.refresh(ctx, a.Run, a.lw)
		return fmt.Errorf("the %s package lists could not be downloaded, so nothing was upgraded and apt is back on %s: %v",
			to, from, err)
	}
	opts := append(append([]string{}, aptLock...), "-y", "-q", "-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold", "-o", "Dpkg::Use-Pty=0")
	for _, step := range [][]string{{"upgrade", "--without-new-pkgs"}, {"full-upgrade"}} {
		var b bytes.Buffer
		stop := a.follow(ctx, "apt-get "+step[0], nil)
		code, err := a.Run(ctx, io.MultiWriter(&b, a.lw), releaseEnv, "apt-get", append(append([]string{}, opts...), step...)...)
		stop()
		if err != nil {
			return err
		}
		if code != 0 {
			err := a.aptPartWay(ctx, "apt-get "+step[0], code, b.Bytes(), b.Bytes(), "/var/log/apt/term.log")
			var oe *outcomeError
			if errors.As(err, &oe) && oe.outcome.Changed == ChangedNo {
				// Nothing was installed: apt goes back on the old release,
				// as when the new lists cannot be downloaded.
				if rerr := restoreSources(aptSourcesRoot, backup); rerr != nil {
					oe.msg += fmt.Sprintf(". The old sources could not be put back (%v); fix /etc/apt by hand", rerr)
					oe.outcome.Next = append([]string{"Point /etc/apt back at " + from + " by hand."}, oe.outcome.Next...)
				} else {
					_ = m.refresh(ctx, a.Run, a.lw)
					oe.msg += ". apt is back on " + from
				}
			}
			return err
		}
	}
	return nil
}

func (a *PackageApplier) upgradeLeapp(ctx context.Context) error {
	a.update(func(s *PackageRunStatus) {
		s.Message = "leapp is checking the server (preupgrade); this can take twenty minutes"
	})
	var b bytes.Buffer
	stop := a.follow(ctx, "leapp preupgrade", leappUpgradeLogs[:1])
	code, err := a.Run(ctx, io.MultiWriter(&b, a.lw), releaseEnv, "leapp", "preupgrade")
	stop()
	if err != nil {
		return err
	}
	if rep, rerr := os.ReadFile(leappReportFile); rerr == nil {
		if inh := leappInhibitors(rep); len(inh) > 0 {
			return fmt.Errorf("leapp found what stops the upgrade, and nothing was changed: %s. The full report "+
				"is %s", strings.Join(inh, "; "), leappReportFile)
		}
	}
	if code != 0 {
		return fmt.Errorf("leapp preupgrade failed (exit %d), and nothing was changed: %s", code, tail(b.Bytes(), 400))
	}
	a.update(func(s *PackageRunStatus) {
		s.Message = "leapp is downloading the new release and preparing its upgrade boot"
	})
	b.Reset()
	stop = a.follow(ctx, "leapp upgrade", leappUpgradeLogs[1:])
	code, err = a.Run(ctx, io.MultiWriter(&b, a.lw), releaseEnv, "leapp", "upgrade")
	stop()
	if err != nil {
		return err
	}
	if code != 0 {
		if rep, rerr := os.ReadFile(leappReportFile); rerr == nil {
			if inh := leappInhibitors(rep); len(inh) > 0 {
				return fmt.Errorf("leapp stopped the upgrade, and nothing was changed: %s", strings.Join(inh, "; "))
			}
		}
		return fmt.Errorf("leapp upgrade failed (exit %d); nothing is changed until the reboot, which did not "+
			"happen: %s", code, tail(b.Bytes(), 400))
	}
	return nil
}

/* ── The daemon: after the reboot ────────────────────────────────────────── */

// releaseFinisher is a platform that can finish a release upgrade after its
// reboot.
type releaseFinisher interface {
	finishRelease(ctx context.Context) (ReleaseFinish, error)
}

// settleRelease ends a release upgrade whose reboot happened: done if the
// server is on the release it was asked for, failed if not.
// settleReleaseUnrebooted ends a release upgrade whose helper stopped
// before it reported, on a server that is on the new release but has not
// rebooted into it yet.
func (p *Packages) settleReleaseUnrebooted(ctx context.Context, st *PackageRunStatus) {
	c := st.Release
	st.State, st.Step, st.RebootRequired = RunDone, "", true
	st.Message = "upgraded from " + c.FromName + " to " + c.ToName + ", but the agent's helper stopped before it " +
		"said so, and the server has not rebooted into it yet: reboot it when you are ready"
	if err := p.platform.repair(ctx); err != nil {
		st.Message += ". Finishing what the package manager left failed (" + err.Error() + "); run `dpkg " +
			"--configure -a` on the server before rebooting"
	}
}

func (p *Packages) settleRelease(ctx context.Context, st *PackageRunStatus) {
	c := st.Release
	st.Rebooted, st.RebootRequired, st.Step = true, false, ""
	now := osRelease()["VERSION_ID"]
	var (
		fin ReleaseFinish
		err error
	)
	if f, ok := p.platform.(releaseFinisher); ok {
		fin, err = f.finishRelease(ctx)
	} else {
		err = errors.New("this agent cannot finish a release upgrade on this server")
	}
	if !versionIs(now, c.To) {
		st.State = RunFailed
		st.Message = fmt.Sprintf("the server rebooted, but it is still on %s: the upgrade to %s did not happen. "+
			"Look at %s", orUnknown(now), c.ToName, c.logFile())
		return
	}
	st.State = RunDone
	msg := "upgraded from " + c.FromName + " to " + c.ToName + ", and the server rebooted into it"
	var more []string
	if err != nil {
		more = append(more, "checking what the agent uses on the server failed ("+err.Error()+"); look at it by hand")
	}
	if fin.Repair != "" {
		more = append(more, "finishing what the package manager left failed ("+fin.Repair+
			"); run `dpkg --configure -a` (or your distribution's equivalent) on the server")
	}
	if len(fin.Restored) > 0 {
		more = append(more, "installed "+strings.Join(fin.Restored, ", ")+
			" again, which the upgrade took away and the agent uses")
	}
	if len(fin.Missing) > 0 {
		more = append(more, "still missing, and the agent uses it: "+strings.Join(fin.Missing, ", ")+
			"; install it from the new release")
	}
	more = append(more, fin.Notes...)
	if len(more) > 0 {
		msg += ". " + strings.Join(more, ". ")
	}
	st.Message = msg
}

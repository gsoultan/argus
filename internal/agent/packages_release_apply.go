package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
var releaseEnv = append(append([]string{}, aptEnv...), "UCF_FORCE_CONFFOLD=1", "RELEASE_UPGRADER_NO_SCREEN=1")

// applyRelease moves the server to the release the run was approved for,
// then reboots into it. The agent that comes back checks it got there.
func (a *PackageApplier) applyRelease(ctx context.Context) PackageRunStatus {
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "preflight", "checking the server is ready for a new release"
	})
	m := detectManager(a.Look)
	if m == nil {
		return a.fail(errNoManager.Error())
	}
	if err := m.refresh(ctx, a.Run, a.lw); err != nil {
		return a.fail(err.Error())
	}
	_, off := a.Now().Zone()
	r := PackageReport{CheckedAt: a.Now().UTC(), Manager: m.name(), Supported: true, UTCOffsetMinutes: off / 60}
	r, err := a.listInto(ctx, m, r)
	if err != nil {
		return a.fail(err.Error())
	}
	info := r.Release
	switch {
	case info == nil:
		return a.fail("the server does not say which release it runs (no /etc/os-release)")
	case info.Reason != "":
		return a.fail(info.Reason)
	case info.Next == "":
		return a.fail("no newer release is offered for " + info.Current + " now")
	case info.Next != a.Plan.Expect:
		return a.fail(fmt.Sprintf("the server now offers %s, not the %s this run was approved for; nothing was "+
			"changed. Look for updates again and ask again", info.Next, a.Plan.Expect))
	case len(info.Blockers) > 0:
		return a.fail("not upgraded, nothing was changed: " + strings.Join(info.Blockers, "; "))
	}
	change := &ReleaseChange{Distro: info.Distro, From: info.Version, FromName: info.Current, To: info.Next,
		ToName: info.NextName, Tool: info.Tool}
	deps := snapshotDeps(ctx, a.Run, a.Look, managerFamily(m))
	if err := writeFileAtomic(filepath.Join(a.Plan.StateDir, packagesDir, releaseDepsFile), mustJSON(deps), 0o600); err != nil {
		return a.fail("cannot keep the list of what the agent uses on the server: " + err.Error())
	}
	fmt.Fprintf(a.lw, "argus: upgrading %s to %s with %s\n", info.Current, info.NextName, info.Tool)
	a.update(func(s *PackageRunStatus) {
		s.Release, s.Step = change, "upgrade"
		s.Message = "upgrading to " + info.NextName + "; this takes a while, and the server reboots at the end"
	})

	switch info.Tool {
	case releaseToolUbuntu:
		err = a.upgradeUbuntu(ctx)
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
		return a.fail(err.Error())
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

func orUnknown(s string) string {
	if s == "" {
		return "an unknown release"
	}
	return s
}

// partWay is what to say when the upgrade itself failed: by then the
// server may be on neither release.
func partWay(tool string, code int, out []byte, log string) error {
	return fmt.Errorf("%s failed (exit %d), so the server may be part way between releases: look at %s "+
		"before anything else. %s", tool, code, log, tail(out, 400))
}

func (a *PackageApplier) upgradeUbuntu(ctx context.Context) error {
	cfg := filepath.Join(ubuntuUpgradeOverrides, ubuntuOverrideName)
	if err := os.MkdirAll(ubuntuUpgradeOverrides, 0o755); err != nil {
		return fmt.Errorf("cannot write the upgrade's settings: %w", err)
	}
	if err := writeFileAtomic(cfg, []byte(ubuntuOverride), 0o644); err != nil {
		return fmt.Errorf("cannot write the upgrade's settings: %w", err)
	}
	defer os.Remove(cfg)
	var b bytes.Buffer
	code, err := a.Run(ctx, io.MultiWriter(&b, a.lw), releaseEnv, "do-release-upgrade",
		"-f", "DistUpgradeViewNonInteractive")
	if err != nil {
		return err
	}
	if code != 0 {
		return partWay("do-release-upgrade", code, b.Bytes(), "/var/log/dist-upgrade/main.log")
	}
	return nil
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
		code, err := a.Run(ctx, io.MultiWriter(&b, a.lw), releaseEnv, "apt-get", append(append([]string{}, opts...), step...)...)
		if err != nil {
			return err
		}
		if code != 0 {
			return partWay("apt-get "+step[0], code, b.Bytes(), "/var/log/apt/term.log")
		}
	}
	return nil
}

func (a *PackageApplier) upgradeLeapp(ctx context.Context) error {
	a.update(func(s *PackageRunStatus) {
		s.Message = "leapp is checking the server (preupgrade); this can take twenty minutes"
	})
	var b bytes.Buffer
	code, err := a.Run(ctx, io.MultiWriter(&b, a.lw), nil, "leapp", "preupgrade")
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
	code, err = a.Run(ctx, io.MultiWriter(&b, a.lw), nil, "leapp", "upgrade")
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

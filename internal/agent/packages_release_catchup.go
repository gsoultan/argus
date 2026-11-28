package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

/*
Bringing the current release up to date before a release upgrade.

do-release-upgrade refuses to start while the release it leaves has updates
waiting ("Please install all available updates for your release before
upgrading."). It asks apt for a full upgrade to decide, which is more than
the console's list of updates shows: that list is `apt-get upgrade
--with-new-pkgs`, which keeps back phased updates (Ubuntu hands a new
version to a growing share of servers over days) and anything that needs a
package removed or replaced. So a server the console shows as up to date can
still be refused.

Before it calls the upgrader, the helper therefore runs the same full
upgrade of the current release itself, with phased updates included. It
first simulates it, and stops without changing anything when apt would
remove a package: that is a person's call. Held packages stay held. What is
still waiting afterwards is named, with where it comes from, so that if the
upgrader refuses anyway the run says which package and what to do about it.
*/

// aptAllPhased has apt take phased updates now rather than when this
// server's turn comes, as do-release-upgrade expects.
var aptAllPhased = []string{"-o", "APT::Get::Always-Include-Phased-Updates=true"}

// distUpgradeNotUpToDate is what do-release-upgrade says when the release it
// leaves still has updates waiting.
const distUpgradeNotUpToDate = "Please install all available updates for your release"

// aptSimulation is what a simulated full upgrade would do.
type aptSimulation struct {
	Install  []PackageUpdate
	Remove   []string
	KeptBack []string
}

func parseAptFullSimulate(b []byte) aptSimulation {
	sim := aptSimulation{Install: parseAptSimulate(b)}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	kept := false
	for sc.Scan() {
		line := sc.Text()
		if m := reAptRemv.FindStringSubmatch(line); m != nil {
			sim.Remove = append(sim.Remove, m[1])
		}
		switch {
		case strings.HasPrefix(line, "The following packages have been kept back:"),
			strings.HasPrefix(line, "The following held packages will be changed:"):
			kept = true
			continue
		case kept && strings.HasPrefix(line, " "):
			sim.KeptBack = append(sim.KeptBack, strings.Fields(line)...)
			continue
		}
		kept = false
	}
	sim.Remove = sortedUnique(sim.Remove)
	sim.KeptBack = sortedUnique(sim.KeptBack)
	return sim
}

// simulateFullUpgrade asks apt what a full upgrade of the current release,
// phased updates included, would do. It changes nothing.
func simulateFullUpgrade(ctx context.Context, run cmdRunner) (aptSimulation, []byte, error) {
	args := append([]string{"-s", "-q", "-o", "Debug::NoLocking=1"}, aptAllPhased...)
	b, code, err := output(ctx, run, aptEnv, "apt-get", append(args, "full-upgrade")...)
	if err != nil {
		return aptSimulation{}, b, err
	}
	if code != 0 {
		return aptSimulation{}, b, fmt.Errorf("apt-get could not work out a full upgrade of the current release "+
			"(exit %d): %s", code, tail(b, 400))
	}
	return parseAptFullSimulate(b), b, nil
}

// aptHolds is what `apt-mark showhold` lists.
func aptHolds(ctx context.Context, run cmdRunner) []string {
	b, code, err := output(ctx, run, nil, "apt-mark", "showhold")
	if err != nil || code != 0 {
		return nil
	}
	return sortedUnique(strings.Fields(string(b)))
}

// aptCandidateHosts reads `apt-cache policy` for where each package's
// candidate version comes from: the host of the first source that offers it.
func aptCandidateHosts(ctx context.Context, run cmdRunner, names []string) map[string]string {
	out := map[string]string{}
	if len(names) == 0 {
		return out
	}
	b, _, err := output(ctx, run, aptEnv, "apt-cache", append([]string{"policy"}, names...)...)
	if err != nil {
		return out
	}
	return parseAptPolicyHosts(b)
}

// parseAptPolicyHosts reads apt-cache policy:
//
//	postgresql-16:
//	  Installed: 16.4-1.pgdg24.04+1
//	  Candidate: 16.6-1.pgdg24.04+1
//	  Version table:
//	     16.6-1.pgdg24.04+1 500
//	        500 http://apt.postgresql.org/pub/repos/apt noble-pgdg/main arm64 Packages
//	 *** 16.4-1.pgdg24.04+1 100
//	        100 /var/lib/dpkg/status
func parseAptPolicyHosts(b []byte) map[string]string {
	out := map[string]string{}
	pkg, cand, inCand := "", "", false
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			continue
		case !strings.HasPrefix(line, " ") && strings.HasSuffix(t, ":"):
			pkg, cand, inCand = strings.TrimSuffix(t, ":"), "", false
		case strings.HasPrefix(t, "Candidate:"):
			cand = strings.TrimSpace(strings.TrimPrefix(t, "Candidate:"))
		case strings.HasPrefix(t, "Version table:"):
		default:
			f := strings.Fields(strings.TrimPrefix(t, "*** "))
			if len(f) == 2 && !strings.Contains(f[1], "/") && !strings.Contains(f[1], ":") {
				// A version line: "16.6-1.pgdg24.04+1 500".
				inCand = cand != "" && f[0] == cand
				continue
			}
			if inCand && pkg != "" && out[pkg] == "" && len(f) >= 2 {
				if u, err := url.Parse(f[1]); err == nil && u.Host != "" {
					out[pkg] = u.Host
				}
			}
		}
	}
	return out
}

// ubuntuOrDebianHost reports whether host is the distribution's own archive
// or one of its mirrors.
func ubuntuOrDebianHost(host string) bool {
	h := strings.ToLower(host)
	return h == "ubuntu.com" || strings.HasSuffix(h, ".ubuntu.com") || h == "debian.org" ||
		strings.HasSuffix(h, ".debian.org")
}

// catchUp brings the current release fully up to date, so the release
// upgrader finds nothing waiting. An error carries the run's outcome.
func (a *PackageApplier) catchUp(ctx context.Context, info *ReleaseInfo) error {
	a.update(func(s *PackageRunStatus) {
		s.Step = "catchup"
		s.Message = "installing what is still waiting for " + info.Current + ", phased updates included"
	})
	fmt.Fprintf(a.lw, "argus: bringing %s fully up to date first (apt-get full-upgrade, phased updates included)\n",
		info.Current)
	sim, _, err := simulateFullUpgrade(ctx, a.Run)
	if err != nil {
		why := err.Error()
		return &outcomeError{
			msg: fmt.Sprintf("not upgraded, nothing was changed: %s cannot be brought up to date first, which the "+
				"upgrade needs. %s", info.Current, why),
			outcome: RunOutcome{Changed: ChangedNo, Why: why, Next: []string{
				"On the server, run `sudo apt update` and `sudo apt -o APT::Get::Always-Include-Phased-Updates=true " +
					"full-upgrade`, and fix what apt says (often a third-party source, such as apt.postgresql.org, " +
					"that is out of step with the release).",
				"Run `apt list --upgradable`: it should list nothing.",
				"Press Check for updates, then ask for the upgrade again.",
			}},
		}
	}
	if len(sim.Remove) > 0 {
		why := "bringing " + info.Current + " up to date would remove " + strings.Join(sim.Remove, ", ")
		return &outcomeError{
			msg: "not upgraded, nothing was changed: " + why + ", and Argus does not remove packages without a " +
				"person deciding. The upgrade needs the current release up to date first",
			outcome: RunOutcome{Changed: ChangedNo, Why: why, Next: []string{
				"On the server, run `sudo apt -o APT::Get::Always-Include-Phased-Updates=true full-upgrade` and read " +
					"what it would remove (" + strings.Join(sim.Remove, ", ") + "). Answer yes if that is fine; if " +
					"not, find what wants it gone with `apt-cache policy <package>`.",
				"Run `apt list --upgradable`: it should list nothing.",
				"Press Check for updates, then ask for the upgrade again.",
			}},
		}
	}
	if len(sim.Install) == 0 {
		fmt.Fprintln(a.lw, "argus: "+info.Current+" is up to date")
	} else {
		names := make([]string, 0, len(sim.Install))
		for _, u := range sim.Install {
			names = append(names, u.Name)
		}
		fmt.Fprintf(a.lw, "argus: installing %d update%s first: %s\n", len(names), plural(len(names)),
			strings.Join(names, ", "))
		a.lw.Flush()
		started := a.Now()
		opts := append(append(append([]string{}, aptLock...), aptAllPhased...), "-y", "-q",
			"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "-o", "Dpkg::Use-Pty=0",
			"full-upgrade")
		var b bytes.Buffer
		stop := a.follow(ctx, "apt-get full-upgrade", nil)
		code, err := a.Run(ctx, io.MultiWriter(&b, a.lw), releaseEnv, "apt-get", opts...)
		stop()
		if err != nil {
			return err
		}
		if code != 0 {
			return a.catchUpFailed(ctx, info, code, b.Bytes(), started)
		}
		a.caughtUp = len(names)
		// What the release upgrade itself changes is read from dpkg's log
		// from here on, not what was just installed for the current release.
		a.releaseSince = a.Now()
		if sim, _, err = simulateFullUpgrade(ctx, a.Run); err != nil {
			sim = aptSimulation{}
		}
	}
	if l := a.leftover(ctx, sim); len(l.Said) > 0 {
		fmt.Fprintln(a.lw, "argus: still waiting after the full upgrade: "+strings.Join(l.Said, ", "))
	}
	a.lw.Flush()
	return nil
}

// catchUpFailed is the outcome of a full upgrade of the current release
// that failed: nothing changed, or what dpkg left half done finished (or
// not), and in every case the server is still on its release.
func (a *PackageApplier) catchUpFailed(ctx context.Context, info *ReleaseInfo, code int, out []byte,
	started time.Time) error {
	why := tail(out, 400)
	failed := dpkgFailed(out)
	on := ""
	if len(failed) > 0 {
		on = " dpkg failed on " + strings.Join(failed, ", ") + "."
	}
	retry := "On the server, run `sudo apt -o APT::Get::Always-Include-Phased-Updates=true full-upgrade` and fix " +
		"what it says" + forPackages(failed) + "."
	if dpkgChangedSince(started) == ChangedNo {
		return &outcomeError{
			msg: fmt.Sprintf("not upgraded, nothing was changed: installing the updates waiting for %s first "+
				"failed (apt-get full-upgrade, exit %d) before any package changed.%s The server is still on %s. "+
				"It said: %s", info.Current, code, on, info.Current, why),
			outcome: RunOutcome{Changed: ChangedNo, Why: why, Next: []string{retry,
				"Run `apt list --upgradable`: it should list nothing.",
				"Press Check for updates, then ask for the upgrade again."}},
		}
	}
	repaired := aptManager{}.repair(ctx, a.Run, a.lw) == nil
	left := dpkgUnfinished(ctx, a.Run)
	a.lw.Flush()
	state := "Argus then finished configuring what dpkg left half done."
	next := []string{retry}
	if !repaired || len(left) > 0 {
		state = "Argus could not finish what dpkg left half done" + forPackages(left) + "."
		next = []string{"Run `sudo dpkg --configure -a` and `sudo apt-get -f install` until both finish without errors.",
			retry}
	}
	next = append(next, "Run `apt list --upgradable`: it should list nothing.",
		"Press Check for updates, then ask for the upgrade again.")
	return &outcomeError{
		msg: fmt.Sprintf("not upgraded: installing the updates waiting for %s first failed part way (apt-get "+
			"full-upgrade, exit %d), so some of them are installed. The server is still on %s; the release "+
			"upgrade did not start.%s %s It said: %s", info.Current, code, info.Current, on, state, why),
		outcome: RunOutcome{Changed: ChangedYes, Why: why, Next: next},
	}
}

func forPackages(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " (" + strings.Join(names, ", ") + ")"
}

// leftover is what a full upgrade of the current release left waiting,
// and why for each package as a person reads it.
type leftover struct {
	Names []string
	Held  []string
	// Third are packages whose new version comes from a source that is not
	// the distribution's, and Hosts those sources.
	Third []string
	Hosts []string
	// Said is each name with its why: "postgresql-16 (from apt.postgresql.org)".
	Said []string
}

func (a *PackageApplier) leftover(ctx context.Context, sim aptSimulation) leftover {
	var l leftover
	names := append([]string{}, sim.KeptBack...)
	for _, u := range sim.Install {
		names = append(names, u.Name)
	}
	l.Names = sortedUnique(names)
	if len(l.Names) == 0 {
		return l
	}
	holds := aptHolds(ctx, a.Run)
	hosts := aptCandidateHosts(ctx, a.Run, l.Names)
	for _, n := range l.Names {
		var why []string
		if hasString(holds, n) {
			why = append(why, "held")
			l.Held = append(l.Held, n)
		}
		if h := hosts[n]; h != "" && !ubuntuOrDebianHost(h) {
			why = append(why, "from "+h)
			l.Third = append(l.Third, n)
			l.Hosts = append(l.Hosts, h)
		}
		if len(why) == 0 && hasString(sim.KeptBack, n) {
			why = append(why, "kept back")
		}
		if len(why) > 0 {
			n += " (" + strings.Join(why, ", ") + ")"
		}
		l.Said = append(l.Said, n)
	}
	l.Hosts = sortedUnique(l.Hosts)
	return l
}

// notUpToDate is the outcome when do-release-upgrade refused because the
// current release still has updates: which packages, why each, and what to
// run about them. It looks again now, since what it finds is what is left.
func (a *PackageApplier) notUpToDate(ctx context.Context, info *ReleaseInfo) error {
	var l leftover
	if sim, _, err := simulateFullUpgrade(ctx, a.Run); err == nil {
		l = a.leftover(ctx, sim)
	}
	why := "do-release-upgrade refused to start: " + info.Current + " still has updates it wants installed first " +
		"(\"Please install all available updates for your release before upgrading.\")"
	var fix []string
	if len(l.Held) > 0 {
		fix = append(fix, "These are held, so apt does not upgrade them: "+strings.Join(l.Held, ", ")+". If they "+
			"no longer need pinning, run `sudo apt-mark unhold "+strings.Join(l.Held, " ")+"`; while they are held "+
			"the release upgrade cannot start.")
	}
	if len(l.Third) > 0 {
		fix = append(fix, "These come from "+strings.Join(l.Hosts, ", ")+", not the release's own archive: "+
			strings.Join(l.Third, ", ")+". Run `sudo apt install --only-upgrade "+strings.Join(l.Third, " ")+
			"` and fix what apt says; if one cannot be upgraded, remove it if nothing uses it, or turn that "+
			"source off until the release upgrade is done.")
	}
	general := "On the server, run `sudo apt update` and `sudo apt -o APT::Get::Always-Include-Phased-Updates=true " +
		"full-upgrade`, then `apt list --upgradable`, which should list nothing."
	if len(l.Said) > 0 {
		why += ". Still waiting: " + strings.Join(l.Said, ", ")
	} else {
		fix = append(fix, "apt now lists nothing waiting, phased updates included, so what the upgrader still "+
			"counts is in "+ubuntuUpgradeLogs[0]+".")
	}
	fix = append(fix, general)
	next := append(append([]string{}, fix...), "Press Check for updates, then ask for the upgrade again.")
	return &outcomeError{
		msg:     "not upgraded, nothing was changed by the release upgrade: " + why + ". " + strings.Join(fix, " "),
		outcome: RunOutcome{Changed: ChangedNo, Why: why, Next: next},
	}
}

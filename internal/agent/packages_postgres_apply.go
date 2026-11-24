package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

/* ── The helper: PostgreSQL's major version, and apt's sources ───────────── */

var (
	rePgUpgradeExpect = regexp.MustCompile(`^([0-9]{1,3})/([A-Za-z0-9_.-]{1,64}):([0-9]{1,3})$`)
	rePgClusterExpect = regexp.MustCompile(`^([0-9]{1,3})/([A-Za-z0-9_.-]{1,64})$`)
)

// validPostgresExpect says whether a run's Expect names what its mode needs.
func validPostgresExpect(mode, expect string) bool {
	switch mode {
	case PackagesPostgresUpgrade:
		return rePgUpgradeExpect.MatchString(expect)
	case PackagesPostgresDropOld:
		return rePgClusterExpect.MatchString(expect)
	case PackagesAptSourcesFix:
		return reHash.MatchString(expect)
	}
	return false
}

// pgBin is a program of one PostgreSQL major version.
func pgBin(ver, prog string) string { return "/usr/lib/postgresql/" + ver + "/bin/" + prog }

// nothingChanged ends a run that stopped before changing anything.
func (a *PackageApplier) nothingChanged(msg string, next ...string) PackageRunStatus {
	if len(next) == 0 {
		next = []string{"Press Check for updates, and look at the PostgreSQL card again."}
	}
	return a.failWith(msg, RunOutcome{Changed: ChangedNo, Why: msg, Next: next})
}

// applyPostgresUpgrade moves one cluster to a newer major version the way
// postgresql-common documents it: a pg_dumpall first, the new server and its
// extensions, the empty cluster the package made dropped, pg_upgradecluster,
// then a check and an analyze. The old cluster is kept, stopped, on another
// port: going back is stopping the new one and starting it.
func (a *PackageApplier) applyPostgresUpgrade(ctx context.Context) PackageRunStatus {
	m := rePgUpgradeExpect.FindStringSubmatch(a.Plan.Expect)
	if m == nil {
		return a.nothingChanged("the run does not say which cluster to upgrade; nothing was changed")
	}
	from, name, to := m[1], m[2], m[3]
	c := PackageChecker{Run: a.Run, Look: a.Look, Now: a.Now}
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "preflight", fmt.Sprintf("checking PostgreSQL %s %s can move to %s", from, name, to)
	})
	if code, err := a.Run(ctx, a.lw, aptEnv, "apt-get", append(append([]string{}, aptLock...), "-q", "update")...); err != nil || code != 0 {
		return a.nothingChanged(fmt.Sprintf("apt-get update failed (exit %d%s), so the new packages cannot be "+
			"installed; nothing was changed", code, errSuffix(err)))
	}
	installed, _ := c.pgInstalled(ctx)
	pu := c.postgres(ctx, installed)
	switch {
	case pu == nil || pu.Upgrade == nil:
		return a.nothingChanged(fmt.Sprintf("the server no longer offers to move PostgreSQL %s %s to %s; nothing was "+
			"changed", from, name, to))
	case pu.Upgrade.Expect() != a.Plan.Expect:
		return a.nothingChanged(fmt.Sprintf("the server now offers PostgreSQL %s %s to %s, not what was approved "+
			"(%s); nothing was changed", pu.Upgrade.From, pu.Upgrade.Cluster, pu.Upgrade.To, a.Plan.Expect))
	case len(pu.Upgrade.Blockers) > 0:
		return a.nothingChanged("not upgraded, nothing was changed: " + strings.Join(pu.Upgrade.Blockers, "; "))
	}
	up := *pu.Upgrade
	port := strconv.Itoa(up.Port)
	asPostgres := func(w io.Writer, prog string, args ...string) (int, error) {
		return a.Run(ctx, w, nil, "runuser", append([]string{"-u", "postgres", "--", prog}, args...)...)
	}

	// 1. A dump of everything, roles included, that 17 or 19 can restore.
	backup := pgBackupPath(from, name, a.Now().UTC().Format("20060102-150405"))
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "backup", fmt.Sprintf("backing up PostgreSQL %s %s to %s", from, name, backup)
	})
	if code, err := a.Run(ctx, a.lw, nil, "install", "-d", "-m", "0700", "-o", "postgres", "-g", "postgres",
		pgBackupDir); err != nil || code != 0 {
		return a.nothingChanged(fmt.Sprintf("could not make %s for the backup (exit %d%s); nothing was changed",
			pgBackupDir, code, errSuffix(err)))
	}
	if code, err := asPostgres(a.lw, pgBin(from, "pg_dumpall"), "-p", port, "-f", backup); err != nil || code != 0 {
		_ = os.Remove(backup)
		return a.nothingChanged(fmt.Sprintf("pg_dumpall of PostgreSQL %s %s failed (exit %d%s); nothing was changed",
			from, name, code, errSuffix(err)), "Read the log below for why the backup failed.",
			"Press Check for updates, then try again.")
	}
	fmt.Fprintln(a.lw, "argus: backed up PostgreSQL "+from+" "+name+" to "+backup)

	// 2. The new server and its extensions. The package creates and starts
	// an empty "<to> main" when the server has none.
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "install", "installing "+strings.Join(up.Packages, ", ")
	})
	args := append(append([]string{"install", "-y", "-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold"}, aptLock...), up.Packages...)
	if code, err := a.Run(ctx, a.lw, aptEnv, "apt-get", args...); err != nil || code != 0 {
		msg := fmt.Sprintf("installing PostgreSQL %s failed (exit %d%s); PostgreSQL %s %s was not touched", to, code,
			errSuffix(err), from, name)
		return a.failWith(msg, RunOutcome{Changed: ChangedUnknown, Why: msg, Next: []string{
			"Run `sudo apt-get -f install` on the server.",
			"PostgreSQL " + from + " " + name + " still runs as before; its backup is " + backup + ".",
			"Press Check for updates, then try again."}})
	}

	// 3. The empty cluster of the new version, so pg_upgradecluster can use
	// the name. Only when it is really empty.
	back := func(extra ...string) []string {
		return append(extra, "Its backup is "+backup+"; `sudo -u postgres psql -p "+port+" -f "+backup+
			"` restores it into an empty cluster.")
	}
	if pu2 := c.postgres(ctx, installed); pu2 != nil {
		for _, cl := range pu2.Clusters {
			if cl.Version != to || cl.Name != name {
				continue
			}
			again := c.pgUpgrade(ctx, pu2.Clusters, installed, to)
			if again == nil || !again.DropEmpty {
				msg := fmt.Sprintf("PostgreSQL %s was installed, but its cluster %s is not empty, so Argus did not "+
					"drop it; PostgreSQL %s %s was not touched", to, name, from, name)
				return a.failWith(msg, RunOutcome{Changed: ChangedYes, Why: msg, Next: back(
					"Look at it with `sudo pg_lsclusters`; drop it with `sudo pg_dropcluster " + to + " " + name +
						" --stop` if it is not needed, then upgrade again.")})
			}
			a.update(func(s *PackageRunStatus) {
				s.Step, s.Message = "drop_empty", fmt.Sprintf("dropping the empty PostgreSQL %s %s the package made", to, name)
			})
			if code, err := a.Run(ctx, a.lw, nil, "pg_dropcluster", "--stop", to, name); err != nil || code != 0 {
				msg := fmt.Sprintf("could not drop the empty PostgreSQL %s %s (exit %d%s); PostgreSQL %s %s was not "+
					"touched", to, name, code, errSuffix(err), from, name)
				return a.failWith(msg, RunOutcome{Changed: ChangedYes, Why: msg, Next: back(
					"Run `sudo pg_dropcluster " + to + " " + name + " --stop`, then upgrade again.")})
			}
		}
	}

	// 4. The upgrade itself. The old cluster stops here.
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "upgrade", fmt.Sprintf("pg_upgradecluster -m %s: PostgreSQL %s %s to %s; it is down "+
			"until this ends", up.Method, from, name, to)
	})
	if code, err := a.Run(ctx, a.lw, nil, "pg_upgradecluster", "-v", to, "-m", up.Method, from, name); err != nil || code != 0 {
		msg := fmt.Sprintf("pg_upgradecluster failed (exit %d%s)", code, errSuffix(err))
		return a.failWith(msg, RunOutcome{Changed: ChangedUnknown, Why: msg, Next: back(
			"Look at `sudo pg_lsclusters`.",
			"If "+to+" "+name+" is there but broken, drop it (`sudo pg_dropcluster "+to+" "+name+" --stop`) and "+
				"start the old one again (`sudo pg_ctlcluster "+from+" "+name+" start`): it was not changed.")})
	}

	// 5. Check: the new cluster up on the old port, the old one down.
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "verify", "checking the new cluster answers" })
	out, _, _ := output(ctx, a.Run, nil, "pg_lsclusters", "--no-header")
	var newOK, oldDown bool
	oldPort := 0
	for _, cl := range parsePgLsclusters(out) {
		if cl.Name != name {
			continue
		}
		if cl.Version == to {
			newOK = cl.Online && cl.Port == up.Port
		}
		if cl.Version == from {
			oldDown, oldPort = !cl.Online, cl.Port
		}
	}
	ver, code, err := output(ctx, a.Run, nil, "runuser", "-u", "postgres", "--", pgBin(to, "psql"), "-X", "-A",
		"-t", "-q", "-p", port, "-c", "select version()")
	if !newOK || err != nil || code != 0 {
		msg := fmt.Sprintf("pg_upgradecluster ended, but PostgreSQL %s %s does not answer on port %s", to, name, port)
		return a.failWith(msg, RunOutcome{Changed: ChangedYes, Why: msg, Next: back(
			"Look at `sudo pg_lsclusters` and `sudo journalctl -u postgresql@"+to+"-"+name+"`.",
			"To go back: `sudo pg_ctlcluster "+to+" "+name+" stop`, then `sudo pg_ctlcluster "+from+" "+name+
				" start` (set its port back to "+port+" in /etc/postgresql/"+from+"/"+name+"/postgresql.conf first).")})
	}
	fmt.Fprintln(a.lw, "argus: "+strings.TrimSpace(string(ver)))

	// 6. Statistics, which an upgrade does not carry.
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "analyze", "vacuumdb --all --analyze-in-stages, so queries are planned well again"
	})
	analyzed := ""
	if code, err := a.Run(ctx, a.lw, nil, "runuser", "-u", "postgres", "--", pgBin(to, "vacuumdb"), "-p", port,
		"--all", "--analyze-in-stages"); err != nil || code != 0 {
		analyzed = fmt.Sprintf(" The analyze afterwards failed (exit %d%s); run `sudo -u postgres vacuumdb -p %s "+
			"--all --analyze-in-stages`.", code, errSuffix(err), port)
	}
	a.writeReportNow(ctx)
	kept := fmt.Sprintf("PostgreSQL %s %s is kept, stopped", from, name)
	if oldDown && oldPort > 0 {
		kept += fmt.Sprintf(", on port %d", oldPort)
	}
	return a.finish(RunDone, fmt.Sprintf("PostgreSQL %s is now %s on port %s (%s). %s, and its backup is %s. Once "+
		"your applications work, remove it from the Updates tab; until then, going back is stopping %s and "+
		"starting %s.%s", name, to, port, pgVersionLine(string(ver)), kept, backup, to, from, analyzed), nil)
}

func pgVersionLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, " on "); i > 0 {
		s = s[:i]
	}
	return s
}

// applyPostgresDropOld drops the stopped cluster an upgrade left, and
// removes its version's server packages when nothing else uses them and
// apt would remove nothing else with them.
func (a *PackageApplier) applyPostgresDropOld(ctx context.Context) PackageRunStatus {
	m := rePgClusterExpect.FindStringSubmatch(a.Plan.Expect)
	if m == nil {
		return a.nothingChanged("the run does not say which cluster to remove; nothing was changed")
	}
	ver, name := m[1], m[2]
	c := PackageChecker{Run: a.Run, Look: a.Look, Now: a.Now}
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "preflight", fmt.Sprintf("checking PostgreSQL %s %s is stopped and replaced", ver, name)
	})
	installed, _ := c.pgInstalled(ctx)
	pu := c.postgres(ctx, installed)
	if pu == nil || pu.OldCluster == nil || pu.OldCluster.Expect() != a.Plan.Expect {
		return a.nothingChanged(fmt.Sprintf("PostgreSQL %s %s is not a stopped cluster with a newer %s running; "+
			"nothing was changed", ver, name, name))
	}
	old := *pu.OldCluster
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "drop", fmt.Sprintf("dropping PostgreSQL %s %s and its data", ver, name)
	})
	if code, err := a.Run(ctx, a.lw, nil, "pg_dropcluster", ver, name); err != nil || code != 0 {
		return a.nothingChanged(fmt.Sprintf("pg_dropcluster %s %s failed (exit %d%s); nothing was changed", ver, name,
			code, errSuffix(err)))
	}
	msg := fmt.Sprintf("Dropped PostgreSQL %s %s; %s %s runs as before.", ver, name, old.Current, name)
	if len(old.Packages) > 0 {
		a.update(func(s *PackageRunStatus) {
			s.Step, s.Message = "remove", "removing "+strings.Join(old.Packages, ", ")
		})
		sim, code, err := output(ctx, a.Run, aptEnv, "apt-get", append([]string{"-s", "remove"}, old.Packages...)...)
		extra := []string{}
		for _, l := range strings.Split(string(sim), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "Remv" && !hasString(old.Packages, f[1]) {
				extra = append(extra, f[1])
			}
		}
		switch {
		case err != nil || code != 0:
			msg += fmt.Sprintf(" Its packages were left installed: apt could not plan removing them (exit %d%s).",
				code, errSuffix(err))
		case len(extra) > 0:
			msg += " Its packages were left installed: removing them would also remove " + strings.Join(extra, ", ") +
				". Remove them yourself if that is fine: `sudo apt remove " + strings.Join(old.Packages, " ") + "`."
		default:
			args := append(append([]string{"remove", "-y"}, aptLock...), old.Packages...)
			if code, err := a.Run(ctx, a.lw, aptEnv, "apt-get", args...); err != nil || code != 0 {
				msg += fmt.Sprintf(" Removing its packages failed (exit %d%s); run `sudo apt remove %s`.", code,
					errSuffix(err), strings.Join(old.Packages, " "))
			} else {
				msg += " Removed " + strings.Join(old.Packages, ", ") + "."
			}
		}
	}
	if len(old.Backups) > 0 {
		msg += " The backup taken before the upgrade is still in " + strings.Join(old.Backups, ", ") +
			"; delete it once you no longer need it."
	}
	a.writeReportNow(ctx)
	return a.finish(RunDone, msg, nil)
}

// applyAptSourcesFix moves aside the files apt ignores and points
// apt.postgresql.org at this release, as the console showed them. If apt
// cannot read PGDG's suite for this release, its file is put back.
func (a *PackageApplier) applyAptSourcesFix(ctx context.Context) PackageRunStatus {
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "preflight", "reading apt's sources" })
	c := PackageChecker{Run: a.Run, Look: a.Look, Now: a.Now}
	_, pgdg := c.pgInstalled(ctx)
	osr := osRelease()
	f := aptSourcesFix(osr, pgdg)
	if f == nil {
		a.writeReportNow(ctx)
		return a.finish(RunDone, "apt's sources need nothing any more; nothing was changed", nil)
	}
	if f.Fingerprint != a.Plan.Expect {
		return a.nothingChanged("apt's sources changed since they were shown; nothing was changed")
	}
	dir := filepath.Join(aptSourcesRoot, "sources.list.d")
	keep := filepath.Join(aptBackupDir, a.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(keep, 0o700); err != nil {
		return a.nothingChanged("could not make " + keep + ": " + err.Error() + "; nothing was changed")
	}
	var done []string
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "move", "moving aside the files apt ignores to "+keep })
	for _, l := range f.Leftovers {
		if !l.Movable {
			continue
		}
		if err := moveFile(filepath.Join(dir, l.Name), filepath.Join(keep, l.Name)); err != nil {
			fmt.Fprintf(a.lw, "argus: could not move %s: %v\n", l.Name, err)
			continue
		}
		fmt.Fprintf(a.lw, "argus: moved %s to %s\n", l.Name, keep)
		done = append(done, "moved "+l.Name+" aside")
	}
	if p := f.PGDG; p != nil {
		a.update(func(s *PackageRunStatus) {
			s.Step, s.Message = "pgdg", "pointing apt.postgresql.org at "+p.Want+" in /etc/apt/"+p.File
		})
		path := filepath.Join(aptSourcesRoot, p.File)
		orig, err := os.ReadFile(path)
		if err != nil {
			return a.failWith("could not read /etc/apt/"+p.File+": "+err.Error(), RunOutcome{Changed: changedIf(done),
				Why: err.Error()})
		}
		_ = os.WriteFile(filepath.Join(keep, filepath.Base(p.File)), orig, 0o600)
		codename := strings.TrimSuffix(p.Want, "-pgdg")
		fixed := fixPGDGOneLine(orig, codename)
		if strings.HasSuffix(p.File, ".sources") {
			fixed = fixPGDGDeb822(orig, codename)
		}
		st, _ := os.Stat(path)
		mode := os.FileMode(0o644)
		if st != nil {
			mode = st.Mode().Perm()
		}
		if err := writeFileAtomic(path, fixed, mode); err != nil {
			return a.failWith("could not write /etc/apt/"+p.File+": "+err.Error(), RunOutcome{Changed: changedIf(done),
				Why: err.Error()})
		}
		var b strings.Builder
		code, err := a.Run(ctx, io.MultiWriter(&b, a.lw), aptEnv, "apt-get", append(append([]string{}, aptLock...),
			"-q", "update")...)
		if err != nil || code != 0 || pgdgRefused(b.String()) {
			_ = writeFileAtomic(path, orig, mode)
			msg := fmt.Sprintf("apt.postgresql.org did not answer for %s, so /etc/apt/%s was put back as it was",
				p.Want, p.File)
			return a.failWith(msg, RunOutcome{Changed: changedIf(done), Why: msg, Next: []string{
				"apt.postgresql.org may not serve " + p.Want + " yet; see https://wiki.postgresql.org/wiki/Apt.",
				"Press Check for updates later, and fix it again once it does."}})
		}
		done = append(done, "pointed apt.postgresql.org at "+p.Want+" and turned it on")
	}
	if len(done) == 0 {
		return a.nothingChanged("nothing could be moved; see the log below")
	}
	if _, err := a.Run(ctx, a.lw, aptEnv, "apt-get", append(append([]string{}, aptLock...), "-q", "update")...); err != nil {
		fmt.Fprintln(a.lw, "argus: apt-get update: "+err.Error())
	}
	a.writeReportNow(ctx)
	msg := strings.Join(done, "; ")
	return a.finish(RunDone, strings.ToUpper(msg[:1])+msg[1:]+". The files as they were are in "+keep+
		"; to undo, copy them back to /etc/apt/sources.list.d.", nil)
}

func changedIf(done []string) string {
	if len(done) > 0 {
		return ChangedYes
	}
	return ChangedNo
}

// pgdgRefused is apt saying apt.postgresql.org has no such suite.
func pgdgRefused(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "postgresql.org") && (strings.Contains(l, "Release file") ||
			strings.HasPrefix(strings.TrimSpace(l), "Err:") || strings.Contains(l, "404")) {
			return true
		}
	}
	return false
}

// moveFile renames, or copies and removes across filesystems.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		return err
	}
	return os.Remove(from)
}

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Installing and removing software with the server's own package manager.

The same three as updates (packages_managers.go): apt, dnf/yum and zypper,
from the repositories the server already trusts. Argus never adds a
repository and never hands the package manager a file: what can be
installed is what the server's administrator already decided it may.

Every change is simulated first, by the package manager itself (apt-get -s,
dnf --assumeno, zypper --dry-run), and the simulation is what the console
shows and what a person approves. The run simulates again and stops when the
answer differs (software_apply.go), so the server cannot have moved between
the yes and the doing.

Like the rest of the helper this runs as root, outside the agent's sandbox,
and never asks a question: names and versions are checked against
tunnel.ValidPackageName before they get here, and `--` ends the options
wherever a tool accepts it.
*/

// swManager is what a package manager does for installing and removing.
type swManager interface {
	pkgManager
	// installedList is every installed package.
	installedList(ctx context.Context, run cmdRunner) ([]tunnel.InstalledPackage, error)
	// search looks in the repositories for q.
	search(ctx context.Context, run cmdRunner, q string) ([]tunnel.FoundPackage, error)
	// simulate works out what installing or removing items would change.
	// targets is what the real command is to be given: the items, plus for
	// a removal the packages only they needed.
	simulate(ctx context.Context, run cmdRunner, action string, items []tunnel.SoftwareItem,
		purge bool) (sim softwareSim, err error)
	// apply installs or removes targets, as simulate named them.
	apply(ctx context.Context, run cmdRunner, log io.Writer, action string, targets []string, purge bool,
		env []string) error
	// unitsOf lists the systemd services the installed packages ship.
	unitsOf(ctx context.Context, run cmdRunner, names []string) []string
	// purges says whether this manager can delete configuration on removal.
	purges() bool
}

// softwareSim is a simulation's answer.
type softwareSim struct {
	changes    []tunnel.SoftwareChange
	targets    []string
	downloadKB int64
	diskKB     int64
}

func asSW(m pkgManager) swManager {
	s, _ := m.(swManager)
	return s
}

/* ── apt ─────────────────────────────────────────────────────────────────── */

func (aptManager) purges() bool { return true }

func (aptManager) installedList(ctx context.Context, run cmdRunner) ([]tunnel.InstalledPackage, error) {
	b, code, err := output(ctx, run, nil, "dpkg-query", "-W",
		"-f=${db:Status-Abbrev}\t${Package}\t${Version}\t${Architecture}\t${Installed-Size}\t${binary:Summary}\n")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("dpkg-query failed (exit %d): %s", code, tail(b, 300))
	}
	pkgs := parseDpkgQuery(b)
	manual := map[string]bool{}
	if mb, mcode, merr := output(ctx, run, nil, "apt-mark", "showmanual"); merr == nil && mcode == 0 {
		for _, n := range strings.Fields(string(mb)) {
			manual[n] = true
		}
	}
	for i := range pkgs {
		p := &pkgs[i]
		p.Manual = manual[p.Name] || len(manual) == 0
		p.InstalledAt = dpkgInstalledAt(p.Name, p.Arch)
	}
	return pkgs, nil
}

// parseDpkgQuery reads the dpkg-query line above, keeping installed
// packages only ("ii", or "hi" for one on hold).
func parseDpkgQuery(b []byte) []tunnel.InstalledPackage {
	var out []tunnel.InstalledPackage
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(line, "\t", 6)
		if len(f) < 5 {
			continue
		}
		st := strings.TrimSpace(f[0])
		if len(st) < 2 || st[1] != 'i' {
			continue
		}
		p := tunnel.InstalledPackage{Name: f[1], Version: f[2], Arch: f[3]}
		p.SizeKB, _ = strconv.ParseInt(strings.TrimSpace(f[4]), 10, 64)
		if len(f) == 6 {
			p.Summary = strings.TrimSpace(f[5])
		}
		out = append(out, p)
	}
	return out
}

// dpkgInfoDir is where dpkg keeps a package's file list, whose time is when
// it was installed or last upgraded. Tests move it.
var dpkgInfoDir = "/var/lib/dpkg/info"

func dpkgInstalledAt(name, arch string) int64 {
	for _, f := range []string{name + ".list", name + ":" + arch + ".list"} {
		if st, err := os.Stat(filepath.Join(dpkgInfoDir, f)); err == nil {
			return st.ModTime().Unix()
		}
	}
	return 0
}

func (aptManager) search(ctx context.Context, run cmdRunner, q string) ([]tunnel.FoundPackage, error) {
	b, code, err := output(ctx, run, nil, "apt-cache", "search", "--names-only", "--", q)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("apt-cache search failed (exit %d): %s", code, tail(b, 300))
	}
	found := parseAptSearch(b)
	if len(found) < 10 {
		// Few names match: say what the descriptions find too.
		if more, mcode, merr := output(ctx, run, nil, "apt-cache", "search", "--", q); merr == nil && mcode == 0 {
			seen := map[string]bool{}
			for _, f := range found {
				seen[f.Name] = true
			}
			for _, f := range parseAptSearch(more) {
				if !seen[f.Name] {
					found = append(found, f)
				}
			}
		}
	}
	found = rankFound(found, q)
	if len(found) == 0 {
		return found, nil
	}
	names := make([]string, 0, len(found))
	for _, f := range found {
		names = append(names, f.Name)
	}
	pb, _, err := output(ctx, run, nil, "apt-cache", append([]string{"policy", "--"}, names...)...)
	if err != nil {
		return found, nil
	}
	pol := parseAptPolicy(pb)
	out := found[:0]
	for _, f := range found {
		p, ok := pol[f.Name]
		if !ok || p.candidate == "" || p.candidate == "(none)" {
			continue // a virtual package, or nothing installable
		}
		f.Version, f.Origin, f.Versions = p.candidate, p.origin, p.versions
		if p.installed != "" && p.installed != "(none)" {
			f.Installed = p.installed
		}
		out = append(out, f)
	}
	return out, nil
}

// parseAptSearch reads "name - summary" lines.
func parseAptSearch(b []byte) []tunnel.FoundPackage {
	var out []tunnel.FoundPackage
	for _, line := range strings.Split(string(b), "\n") {
		name, sum, ok := strings.Cut(line, " - ")
		if !ok || !tunnel.ValidPackageName(name) {
			continue
		}
		out = append(out, tunnel.FoundPackage{Name: name, Summary: strings.TrimSpace(sum)})
	}
	return out
}

// rankFound puts the exact name first, then names starting with q, then the
// rest, and keeps the first MaxSoftwareFound.
func rankFound(in []tunnel.FoundPackage, q string) []tunnel.FoundPackage {
	q = strings.ToLower(strings.TrimSpace(q))
	rank := func(n string) int {
		n = strings.ToLower(n)
		switch {
		case n == q:
			return 0
		case strings.HasPrefix(n, q):
			return 1
		case strings.Contains(n, q):
			return 2
		}
		return 3
	}
	sort.SliceStable(in, func(i, j int) bool {
		ri, rj := rank(in[i].Name), rank(in[j].Name)
		if ri != rj {
			return ri < rj
		}
		if len(in[i].Name) != len(in[j].Name) {
			return len(in[i].Name) < len(in[j].Name)
		}
		return in[i].Name < in[j].Name
	})
	if len(in) > tunnel.MaxSoftwareFound {
		in = in[:tunnel.MaxSoftwareFound]
	}
	return in
}

type aptPolicy struct {
	installed, candidate, origin string
	versions                     []string
}

// parseAptPolicy reads `apt-cache policy a b c`:
//
//	nginx:
//	  Installed: (none)
//	  Candidate: 1.24.0-2ubuntu7.2
//	  Version table:
//	     1.24.0-2ubuntu7.2 500
//	        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
//	 *** 1.24.0-2ubuntu7 500
//	        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
//	        100 /var/lib/dpkg/status
func parseAptPolicy(b []byte) map[string]aptPolicy {
	out := map[string]aptPolicy{}
	var (
		name    string
		cur     aptPolicy
		lastVer string
	)
	flush := func() {
		if name != "" {
			out[name] = cur
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":") {
			flush()
			name, cur, lastVer = strings.TrimSuffix(line, ":"), aptPolicy{}, ""
			continue
		}
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Installed:"):
			cur.installed = strings.TrimSpace(strings.TrimPrefix(t, "Installed:"))
		case strings.HasPrefix(t, "Candidate:"):
			cur.candidate = strings.TrimSpace(strings.TrimPrefix(t, "Candidate:"))
		case t == "Version table:":
		default:
			f := strings.Fields(strings.TrimPrefix(t, "*** "))
			if len(f) == 2 && !strings.Contains(f[0], "/") {
				if _, err := strconv.Atoi(f[1]); err == nil && tunnel.ValidPackageVersion(f[0]) {
					lastVer = f[0]
					if len(cur.versions) < tunnel.MaxSoftwareVersions && !hasString(cur.versions, f[0]) {
						cur.versions = append(cur.versions, f[0])
					}
					continue
				}
			}
			// "500 http://host/path suite/component arch Packages"
			if len(f) >= 3 && lastVer != "" && lastVer == cur.candidate && cur.origin == "" &&
				!strings.HasPrefix(f[1], "/var/lib/dpkg") {
				cur.origin = f[2]
				if u := f[1]; !strings.Contains(u, "ubuntu.com") && !strings.Contains(u, "debian.org") {
					if host := hostOf(u); host != "" {
						cur.origin = host + " " + f[2]
					}
				}
			}
		}
	}
	flush()
	return out
}

func hostOf(u string) string {
	_, rest, ok := strings.Cut(u, "://")
	if !ok {
		return ""
	}
	h, _, _ := strings.Cut(rest, "/")
	return h
}

// aptSpec is name or name=version, as apt-get install takes them.
func aptSpec(it tunnel.SoftwareItem) string {
	if it.Version != "" {
		return it.Name + "=" + it.Version
	}
	return it.Name
}

func (m aptManager) simulate(ctx context.Context, run cmdRunner, action string, items []tunnel.SoftwareItem,
	purge bool) (softwareSim, error) {
	sim := softwareSim{}
	base := []string{"-s", "-q", "-o", "Debug::NoLocking=1"}
	if action == tunnel.SoftwareInstall {
		args := append(append([]string{}, base...), "install", "--")
		for _, it := range items {
			args = append(args, aptSpec(it))
			sim.targets = append(sim.targets, aptSpec(it))
		}
		b, code, err := output(ctx, run, aptEnv, "apt-get", args...)
		if err != nil {
			return sim, err
		}
		if code != 0 {
			return sim, fmt.Errorf("apt-get cannot install that: %s", aptError(b))
		}
		sim.changes = parseAptChanges(b)
		sim.downloadKB, sim.diskKB = parseAptSizes(b)
		return sim, nil
	}
	// A removal takes the packages nothing else needs once these are gone,
	// and only those: what was already left over stays for its owner.
	before := map[string]bool{}
	if b, code, err := output(ctx, run, aptEnv, "apt-get", append(append([]string{}, base...), "autoremove")...); err == nil && code == 0 {
		for _, c := range parseAptChanges(b) {
			before[c.Name] = true
		}
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name)
	}
	b, code, err := output(ctx, run, aptEnv, "apt-get", append(append(append([]string{}, base...), "remove", "--"), names...)...)
	if err != nil {
		return sim, err
	}
	if code != 0 {
		return sim, fmt.Errorf("apt-get cannot remove that: %s", aptError(b))
	}
	var orphans []string
	for _, n := range parseAptNoLongerRequired(b) {
		if !before[n] && !hasString(names, n) {
			orphans = append(orphans, n)
		}
	}
	sim.targets = append(append([]string{}, names...), orphans...)
	args := append(append([]string{}, base...), "remove")
	if purge {
		args = append(args, "--purge")
	}
	args = append(append(args, "--"), sim.targets...)
	b, code, err = output(ctx, run, aptEnv, "apt-get", args...)
	if err != nil {
		return sim, err
	}
	if code != 0 {
		return sim, fmt.Errorf("apt-get cannot remove that: %s", aptError(b))
	}
	sim.changes = parseAptChanges(b)
	sim.downloadKB, sim.diskKB = parseAptSizes(b)
	return sim, nil
}

// aptError is the E: lines of apt's output, or its end.
func aptError(b []byte) string {
	var es []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "E: ") {
			es = append(es, strings.TrimPrefix(line, "E: "))
		}
	}
	if len(es) > 0 {
		return tail([]byte(strings.Join(es, "; ")), 400)
	}
	return tail(b, 400)
}

// reAptRemv reads "Remv nginx [1.24.0-2ubuntu7.2]" and "Purg nginx [...]".
var reAptRemv = regexp.MustCompile(`^(?:Remv|Purg) (\S+) \[([^\]]*)\]`)

// parseAptChanges reads a simulation's Inst and Remv lines into changes.
func parseAptChanges(b []byte) []tunnel.SoftwareChange {
	var out []tunnel.SoftwareChange
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := reAptInst.FindStringSubmatch(line); m != nil {
			c := tunnel.SoftwareChange{Name: m[1], From: m[2], To: m[3], Kind: tunnel.ChangeAdd}
			if c.From != "" {
				c.Kind = tunnel.ChangeUpgrade
				if aptVersionLess(c.To, c.From) {
					c.Kind = tunnel.ChangeDowngrade
				}
			}
			seen[c.Name] = len(out)
			out = append(out, c)
			continue
		}
		if m := reAptRemv.FindStringSubmatch(line); m != nil {
			if _, dup := seen[m[1]]; dup {
				continue // Purg after Remv: one change
			}
			seen[m[1]] = len(out)
			out = append(out, tunnel.SoftwareChange{Kind: tunnel.ChangeRemove, Name: m[1], From: m[2]})
		}
	}
	return out
}

// aptVersionLess is a rough a < b, good enough to tell a downgrade: it is
// only a label, the package manager decides.
func aptVersionLess(a, b string) bool {
	return compareVersions(a, b) < 0
}

// compareVersions compares numeric runs numerically and the rest as text.
func compareVersions(a, b string) int {
	ta, tb := versionTokens(a), versionTokens(b)
	for i := 0; i < len(ta) && i < len(tb); i++ {
		x, y := ta[i], tb[i]
		nx, ex := strconv.ParseInt(x, 10, 64)
		ny, ey := strconv.ParseInt(y, 10, 64)
		switch {
		case ex == nil && ey == nil:
			if nx != ny {
				if nx < ny {
					return -1
				}
				return 1
			}
		case x != y:
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ta) < len(tb):
		return -1
	case len(ta) > len(tb):
		return 1
	}
	return 0
}

var reVerTok = regexp.MustCompile(`[0-9]+|[A-Za-z]+`)

func versionTokens(v string) []string { return reVerTok.FindAllString(v, -1) }

// parseAptNoLongerRequired reads the list after "The following packages
// were automatically installed and are no longer required:".
func parseAptNoLongerRequired(b []byte) []string {
	var out []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "The following packages were automatically installed and are no longer required") ||
			strings.HasPrefix(line, "The following package was automatically installed and is no longer required") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break
		}
		for _, n := range strings.Fields(line) {
			if tunnel.ValidPackageName(n) {
				out = append(out, n)
			}
		}
	}
	return out
}

var (
	reAptGet  = regexp.MustCompile(`Need to get ([0-9.,]+) ?([kMG]?B)`)
	reAptDisk = regexp.MustCompile(`After this operation, ([0-9.,]+) ?([kMG]?B) (?:of additional )?disk space will be (used|freed)`)
)

// parseAptSizes reads "Need to get 1,580 kB" and "After this operation,
// 6,234 kB of additional disk space will be used" (or freed).
func parseAptSizes(b []byte) (download, disk int64) {
	if m := reAptGet.FindSubmatch(b); m != nil {
		download = sizeKB(string(m[1]), string(m[2]))
	}
	if m := reAptDisk.FindSubmatch(b); m != nil {
		disk = sizeKB(string(m[1]), string(m[2]))
		if string(m[3]) == "freed" {
			disk = -disk
		}
	}
	return download, disk
}

// sizeKB turns "1,580" "kB" or "1.3" "M" or "5" "MiB" into kilobytes.
func sizeKB(num, unit string) int64 {
	num = strings.ReplaceAll(num, ",", "")
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "b":
		f /= 1024
	case "k", "kb", "kib":
	case "m", "mb", "mib":
		f *= 1024
	case "g", "gb", "gib":
		f *= 1024 * 1024
	}
	return int64(f + 0.5)
}

func (aptManager) apply(ctx context.Context, run cmdRunner, log io.Writer, action string, targets []string,
	purge bool, env []string) error {
	args := append(append([]string{}, aptLock...), "-y", "-q",
		"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold",
		"-o", "Dpkg::Use-Pty=0")
	verb := "install"
	if action == tunnel.SoftwareRemove {
		verb = "remove"
		args = append(args, "remove")
		if purge {
			args = append(args, "--purge")
		}
	} else {
		args = append(args, "install")
	}
	args = append(append(args, "--"), targets...)
	var b bytes.Buffer
	code, err := run(ctx, io.MultiWriter(&b, log), append(append([]string{}, aptEnv...), env...), "apt-get", args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("apt-get %s failed (exit %d): %s", verb, code, aptError(b.Bytes()))
	}
	return nil
}

func (aptManager) unitsOf(ctx context.Context, run cmdRunner, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	b, _, err := output(ctx, run, nil, "dpkg", append([]string{"-L", "--"}, names...)...)
	if err != nil {
		return nil
	}
	return serviceUnits(b)
}

// serviceUnits picks the systemd services out of a file list: plain ones,
// not templates, not ones only other units start.
func serviceUnits(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		p := strings.TrimSpace(line)
		dir, file := filepath.Split(p)
		if !strings.HasSuffix(file, ".service") || strings.Contains(file, "@") {
			continue
		}
		switch strings.TrimSuffix(dir, "/") {
		case "/lib/systemd/system", "/usr/lib/systemd/system":
			out = append(out, file)
		}
	}
	return sortedUnique(out)
}

/* ── dnf and yum ─────────────────────────────────────────────────────────── */

func (dnfManager) purges() bool { return false }

func (m dnfManager) installedList(ctx context.Context, run cmdRunner) ([]tunnel.InstalledPackage, error) {
	pkgs, err := rpmList(ctx, run)
	if err != nil {
		return nil, err
	}
	manual := map[string]bool{}
	if b, code, err := output(ctx, run, nil, m.tool, "-q", "repoquery", "--userinstalled",
		"--qf", "%{name}\n"); err == nil && code == 0 {
		for _, n := range strings.Fields(string(b)) {
			manual[n] = true
		}
	}
	for i := range pkgs {
		pkgs[i].Manual = manual[pkgs[i].Name] || len(manual) == 0
	}
	return pkgs, nil
}

// rpmList is every installed package from the rpm database.
func rpmList(ctx context.Context, run cmdRunner) ([]tunnel.InstalledPackage, error) {
	b, code, err := output(ctx, run, nil, "rpm", "-qa", "--qf",
		`%{NAME}\t%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}\t%{ARCH}\t%{SIZE}\t%{INSTALLTIME}\t%{SUMMARY}\n`)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("rpm -qa failed (exit %d): %s", code, tail(b, 300))
	}
	return parseRPMList(b), nil
}

func parseRPMList(b []byte) []tunnel.InstalledPackage {
	var out []tunnel.InstalledPackage
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(line, "\t", 6)
		if len(f) < 5 || f[0] == "gpg-pubkey" {
			continue
		}
		p := tunnel.InstalledPackage{Name: f[0], Version: f[1], Arch: f[2]}
		if n, err := strconv.ParseInt(f[3], 10, 64); err == nil {
			p.SizeKB = (n + 1023) / 1024
		}
		p.InstalledAt, _ = strconv.ParseInt(f[4], 10, 64)
		if len(f) == 6 {
			p.Summary = strings.TrimSpace(f[5])
		}
		out = append(out, p)
	}
	return out
}

func (m dnfManager) search(ctx context.Context, run cmdRunner, q string) ([]tunnel.FoundPackage, error) {
	glob := "*" + strings.ReplaceAll(strings.TrimSpace(q), " ", "*") + "*"
	b, code, err := output(ctx, run, nil, m.tool, "-q", "repoquery", "--available",
		"--qf", "%{name}\t%{evr}\t%{repoid}\t%{summary}\n", "--", glob)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%s repoquery failed (exit %d): %s", m.tool, code, tail(b, 300))
	}
	found := parseRepoquery(b)
	installed := map[string]string{}
	if pkgs, err := rpmList(ctx, run); err == nil {
		for _, p := range pkgs {
			installed[p.Name] = p.Version
		}
	}
	for i := range found {
		found[i].Installed = installed[found[i].Name]
	}
	return rankFound(found, q), nil
}

// parseRepoquery groups "name\tevr\trepo\tsummary" lines by name, newest
// version first.
func parseRepoquery(b []byte) []tunnel.FoundPackage {
	by := map[string]*tunnel.FoundPackage{}
	var order []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 3 || !tunnel.ValidPackageName(f[0]) || !tunnel.ValidPackageVersion(f[1]) {
			continue
		}
		p, ok := by[f[0]]
		if !ok {
			p = &tunnel.FoundPackage{Name: f[0]}
			if len(f) == 4 {
				p.Summary = strings.TrimSpace(f[3])
			}
			by[f[0]] = p
			order = append(order, f[0])
		}
		if !hasString(p.Versions, f[1]) {
			p.Versions = append(p.Versions, f[1])
			if p.Version == "" || compareVersions(f[1], p.Version) > 0 {
				p.Version, p.Origin = f[1], f[2]
			}
		}
	}
	out := make([]tunnel.FoundPackage, 0, len(order))
	for _, n := range order {
		p := by[n]
		sort.SliceStable(p.Versions, func(i, j int) bool { return compareVersions(p.Versions[i], p.Versions[j]) > 0 })
		if len(p.Versions) > tunnel.MaxSoftwareVersions {
			p.Versions = p.Versions[:tunnel.MaxSoftwareVersions]
		}
		out = append(out, *p)
	}
	return out
}

// dnfSpec is name or name-version, as dnf install takes them.
func dnfSpec(it tunnel.SoftwareItem) string {
	if it.Version != "" {
		return it.Name + "-" + it.Version
	}
	return it.Name
}

func (m dnfManager) simulate(ctx context.Context, run cmdRunner, action string, items []tunnel.SoftwareItem,
	purge bool) (softwareSim, error) {
	sim := softwareSim{}
	if purge {
		return sim, fmt.Errorf("%s keeps configuration changed by hand as .rpmsave files; there is nothing to purge", m.tool)
	}
	verb := "install"
	for _, it := range items {
		if action == tunnel.SoftwareRemove {
			sim.targets = append(sim.targets, it.Name)
		} else {
			sim.targets = append(sim.targets, dnfSpec(it))
		}
	}
	if action == tunnel.SoftwareRemove {
		verb = "remove"
	}
	args := append([]string{"--assumeno", verb, "--"}, sim.targets...)
	b, code, err := output(ctx, run, []string{"LC_ALL=C"}, m.tool, args...)
	if err != nil {
		return sim, err
	}
	changes := parseDnfTransaction(b)
	// --assumeno answers no, which dnf reports as an error; a failure to
	// resolve prints no transaction at all.
	if len(changes) == 0 && code != 0 && code != 1 {
		return sim, fmt.Errorf("%s cannot %s that: %s", m.tool, verb, dnfError(b))
	}
	if len(changes) == 0 && bytes.Contains(b, []byte("Error")) {
		return sim, fmt.Errorf("%s cannot %s that: %s", m.tool, verb, dnfError(b))
	}
	installed := map[string]string{}
	if pkgs, err := rpmList(ctx, run); err == nil {
		for _, p := range pkgs {
			installed[p.Name] = p.Version
		}
	}
	for i := range changes {
		c := &changes[i]
		switch c.Kind {
		case tunnel.ChangeUpgrade, tunnel.ChangeDowngrade:
			c.From = installed[c.Name]
		case tunnel.ChangeRemove:
			if c.From == "" {
				c.From = installed[c.Name]
			}
		}
	}
	sim.changes = changes
	sim.downloadKB, sim.diskKB = parseDnfSizes(b)
	return sim, nil
}

func dnfError(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Error") || strings.HasPrefix(t, "No match for argument") ||
			strings.HasPrefix(t, "Failed to resolve") || strings.HasPrefix(t, "No packages to remove") {
			return tail([]byte(t), 400)
		}
	}
	return tail(b, 400)
}

// parseDnfTransaction reads the transaction table dnf 4 and dnf 5 print
// before asking, section by section:
//
//	Installing:
//	 redis      x86_64   6.2.7-1.el9   appstream   1.3 M
//	Installing dependencies:
//	 ...
//	Removing:
//	 nginx      x86_64   1:1.20.1-14.el9   @appstream   1.2 M
//
// A long name wraps onto a line of its own in dnf 4.
func parseDnfTransaction(b []byte) []tunnel.SoftwareChange {
	var out []tunnel.SoftwareChange
	kind := ""
	pending := ""
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimRight(line, " \r")
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, " ") && strings.HasSuffix(t, ":") {
			kind = dnfSectionKind(strings.TrimSuffix(t, ":"))
			pending = ""
			continue
		}
		if !strings.HasPrefix(t, " ") {
			if strings.HasPrefix(t, "Transaction Summary") || strings.HasPrefix(t, "Total") {
				kind = ""
			}
			continue
		}
		if kind == "" {
			continue
		}
		f := strings.Fields(t)
		if len(f) == 1 && tunnel.ValidPackageName(f[0]) {
			pending = f[0]
			continue
		}
		if pending != "" {
			f = append([]string{pending}, f...)
			pending = ""
		}
		if len(f) < 4 || f[0] == "replacing" {
			continue
		}
		name, ver := f[0], f[2]
		if !tunnel.ValidPackageName(name) || seen[name] {
			continue
		}
		seen[name] = true
		c := tunnel.SoftwareChange{Kind: kind, Name: name}
		if kind == tunnel.ChangeRemove {
			c.From = ver
		} else {
			c.To = ver
		}
		out = append(out, c)
	}
	return out
}

func dnfSectionKind(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	switch {
	case strings.HasPrefix(h, "installing"), strings.HasPrefix(h, "reinstalling"):
		return tunnel.ChangeAdd
	case strings.HasPrefix(h, "upgrading"):
		return tunnel.ChangeUpgrade
	case strings.HasPrefix(h, "downgrading"):
		return tunnel.ChangeDowngrade
	case strings.HasPrefix(h, "removing"):
		return tunnel.ChangeRemove
	}
	return ""
}

var (
	reDnfDownload  = regexp.MustCompile(`(?:Total download size|Need to download): ?([0-9.]+) ?([kMG]i?B?|[kMG])`)
	reDnf5Download = regexp.MustCompile(`Need to download ([0-9.]+) ?([kMG]?i?B)`)
	reDnfInstalled = regexp.MustCompile(`(Installed size|Freed space): ([0-9.]+) ?([kMG]i?B?|[kMG])`)
	reDnf5After    = regexp.MustCompile(`After this operation, ([0-9.]+) ?([kMG]?i?B) (?:extra )?will be (used|freed)`)
)

func parseDnfSizes(b []byte) (download, disk int64) {
	if m := reDnfDownload.FindSubmatch(b); m != nil {
		download = sizeKB(string(m[1]), string(m[2]))
	} else if m := reDnf5Download.FindSubmatch(b); m != nil {
		download = sizeKB(string(m[1]), string(m[2]))
	}
	if m := reDnfInstalled.FindSubmatch(b); m != nil {
		disk = sizeKB(string(m[2]), string(m[3]))
		if string(m[1]) == "Freed space" {
			disk = -disk
		}
	} else if m := reDnf5After.FindSubmatch(b); m != nil {
		disk = sizeKB(string(m[1]), string(m[2]))
		if string(m[3]) == "freed" {
			disk = -disk
		}
	}
	return download, disk
}

func (m dnfManager) apply(ctx context.Context, run cmdRunner, log io.Writer, action string, targets []string,
	_ bool, env []string) error {
	verb := "install"
	if action == tunnel.SoftwareRemove {
		verb = "remove"
	}
	var b bytes.Buffer
	code, err := run(ctx, io.MultiWriter(&b, log), env, m.tool, append([]string{"-y", verb, "--"}, targets...)...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s %s failed (exit %d): %s", m.tool, verb, code, dnfError(b.Bytes()))
	}
	return nil
}

func (dnfManager) unitsOf(ctx context.Context, run cmdRunner, names []string) []string {
	return rpmUnits(ctx, run, names)
}

func rpmUnits(ctx context.Context, run cmdRunner, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	b, _, err := output(ctx, run, nil, "rpm", append([]string{"-ql", "--"}, names...)...)
	if err != nil {
		return nil
	}
	return serviceUnits(b)
}

/* ── zypper ──────────────────────────────────────────────────────────────── */

func (zypperManager) purges() bool { return false }

func (zypperManager) installedList(ctx context.Context, run cmdRunner) ([]tunnel.InstalledPackage, error) {
	pkgs, err := rpmList(ctx, run)
	if err != nil {
		return nil, err
	}
	// zypper does not keep who asked for what in a form worth reading here:
	// everything counts as asked for.
	for i := range pkgs {
		pkgs[i].Manual = true
	}
	return pkgs, nil
}

type zypperSearch struct {
	Solvables []struct {
		Status     string `xml:"status,attr"`
		Name       string `xml:"name,attr"`
		Summary    string `xml:"summary,attr"`
		Kind       string `xml:"kind,attr"`
		Edition    string `xml:"edition,attr"`
		Arch       string `xml:"arch,attr"`
		Repository string `xml:"repository,attr"`
	} `xml:"search-result>solvable-list>solvable"`
}

func (zypperManager) search(ctx context.Context, run cmdRunner, q string) ([]tunnel.FoundPackage, error) {
	b, code, err := output(ctx, run, zypperEnv, "zypper", "-n", "--xmlout", "search", "-s", "-t", "package", "--", q)
	if err != nil {
		return nil, err
	}
	// 104 is "nothing found".
	if code == 104 {
		return nil, nil
	}
	if code != 0 {
		return nil, fmt.Errorf("zypper search failed (exit %d): %s", code, tail(b, 300))
	}
	found, err := parseZypperSearch(b)
	if err != nil {
		return nil, err
	}
	return rankFound(found, q), nil
}

func parseZypperSearch(b []byte) ([]tunnel.FoundPackage, error) {
	var s zypperSearch
	if err := xml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("cannot read zypper's answer: %w", err)
	}
	by := map[string]*tunnel.FoundPackage{}
	var order []string
	for _, v := range s.Solvables {
		if (v.Kind != "" && v.Kind != "package") || !tunnel.ValidPackageName(v.Name) {
			continue
		}
		p, ok := by[v.Name]
		if !ok {
			p = &tunnel.FoundPackage{Name: v.Name, Summary: v.Summary}
			by[v.Name] = p
			order = append(order, v.Name)
		}
		if v.Status == "installed" {
			p.Installed = v.Edition
			if v.Repository == "(System Packages)" || v.Repository == "" {
				continue
			}
		}
		if !tunnel.ValidPackageVersion(v.Edition) || hasString(p.Versions, v.Edition) {
			continue
		}
		p.Versions = append(p.Versions, v.Edition)
		if p.Version == "" || compareVersions(v.Edition, p.Version) > 0 {
			p.Version, p.Origin = v.Edition, v.Repository
		}
	}
	out := make([]tunnel.FoundPackage, 0, len(order))
	for _, n := range order {
		p := by[n]
		if p.Version == "" {
			continue
		}
		sort.SliceStable(p.Versions, func(i, j int) bool { return compareVersions(p.Versions[i], p.Versions[j]) > 0 })
		if len(p.Versions) > tunnel.MaxSoftwareVersions {
			p.Versions = p.Versions[:tunnel.MaxSoftwareVersions]
		}
		out = append(out, *p)
	}
	return out, nil
}

type zypperSolvable struct {
	Name       string `xml:"name,attr"`
	Edition    string `xml:"edition,attr"`
	EditionOld string `xml:"edition-old,attr"`
	Kind       string `xml:"kind,attr"`
}

type zypperSummary struct {
	Summary struct {
		DownloadSize int64            `xml:"download-size,attr"`
		SpaceDiff    int64            `xml:"space-usage-diff,attr"`
		Install      []zypperSolvable `xml:"to-install>solvable"`
		Reinstall    []zypperSolvable `xml:"to-reinstall>solvable"`
		Upgrade      []zypperSolvable `xml:"to-upgrade>solvable"`
		Downgrade    []zypperSolvable `xml:"to-downgrade>solvable"`
		Remove       []zypperSolvable `xml:"to-remove>solvable"`
	} `xml:"install-summary"`
	Messages []struct {
		Type string `xml:"type,attr"`
		Text string `xml:",chardata"`
	} `xml:"message"`
}

// zypperSpec is name or name=version.
func zypperSpec(it tunnel.SoftwareItem) string {
	if it.Version != "" {
		return it.Name + "=" + it.Version
	}
	return it.Name
}

func (zypperManager) simulate(ctx context.Context, run cmdRunner, action string, items []tunnel.SoftwareItem,
	purge bool) (softwareSim, error) {
	sim := softwareSim{}
	if purge {
		return sim, fmt.Errorf("zypper keeps configuration changed by hand as .rpmsave files; there is nothing to purge")
	}
	args := []string{"-n", "--xmlout"}
	if action == tunnel.SoftwareRemove {
		args = append(args, "remove", "--dry-run", "--clean-deps", "--")
		for _, it := range items {
			sim.targets = append(sim.targets, it.Name)
		}
	} else {
		args = append(args, "install", "--dry-run", "--auto-agree-with-licenses", "--")
		for _, it := range items {
			sim.targets = append(sim.targets, zypperSpec(it))
		}
	}
	b, code, err := output(ctx, run, zypperEnv, "zypper", append(args, sim.targets...)...)
	if err != nil {
		return sim, err
	}
	changes, download, disk, msg, perr := parseZypperSummary(b)
	if perr != nil {
		return sim, perr
	}
	if code != 0 && (code < 100 || code > 106) {
		if msg == "" {
			msg = tail(b, 400)
		}
		return sim, fmt.Errorf("zypper cannot do that: %s", msg)
	}
	sim.changes, sim.downloadKB, sim.diskKB = changes, download, disk
	return sim, nil
}

func parseZypperSummary(b []byte) ([]tunnel.SoftwareChange, int64, int64, string, error) {
	var s zypperSummary
	if err := xml.Unmarshal(b, &s); err != nil {
		return nil, 0, 0, "", fmt.Errorf("cannot read zypper's answer: %w", err)
	}
	var out []tunnel.SoftwareChange
	add := func(kind string, list []zypperSolvable) {
		for _, v := range list {
			if v.Kind != "" && v.Kind != "package" {
				continue
			}
			c := tunnel.SoftwareChange{Kind: kind, Name: v.Name}
			switch kind {
			case tunnel.ChangeRemove:
				c.From = v.Edition
			default:
				c.To, c.From = v.Edition, v.EditionOld
			}
			out = append(out, c)
		}
	}
	add(tunnel.ChangeAdd, s.Summary.Install)
	add(tunnel.ChangeAdd, s.Summary.Reinstall)
	add(tunnel.ChangeUpgrade, s.Summary.Upgrade)
	add(tunnel.ChangeDowngrade, s.Summary.Downgrade)
	add(tunnel.ChangeRemove, s.Summary.Remove)
	msg := ""
	for _, m := range s.Messages {
		if m.Type == "error" {
			msg = strings.TrimSpace(m.Text)
		}
	}
	return out, (s.Summary.DownloadSize + 1023) / 1024, s.Summary.SpaceDiff / 1024, msg, nil
}

func (zypperManager) apply(ctx context.Context, run cmdRunner, log io.Writer, action string, targets []string,
	_ bool, env []string) error {
	args := []string{"-n", "install", "--auto-agree-with-licenses", "--"}
	if action == tunnel.SoftwareRemove {
		args = []string{"-n", "remove", "--clean-deps", "--"}
	}
	var b bytes.Buffer
	code, err := run(ctx, io.MultiWriter(&b, log), append(append([]string{}, zypperEnv...), env...), "zypper",
		append(args, targets...)...)
	if err != nil {
		return err
	}
	if code != 0 && (code < 100 || code > 106) {
		return fmt.Errorf("zypper %s failed (exit %d): %s", args[1], code, tail(b.Bytes(), 400))
	}
	return nil
}

func (zypperManager) unitsOf(ctx context.Context, run cmdRunner, names []string) []string {
	return rpmUnits(ctx, run, names)
}

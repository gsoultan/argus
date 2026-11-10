package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

/*
Release upgrades: moving a server to the next major release of its
operating system, from the console. Ubuntu 22.04 to 24.04, Debian 12 to 13,
RHEL, AlmaLinux, Rocky or Oracle Linux 8 to 9.

Each distribution has one way it supports, and Argus uses that way rather
than its own:

  - Ubuntu: do-release-upgrade, non-interactive. It is what Canonical tests,
    and it knows each release's quirks (the transitions, the packages that
    have to go first). Argus adds one setting for the run,
    RemoveObsoletes=no in /etc/update-manager/release-upgrades.d/argus.cfg:
    nothing the server had is removed because the new release no longer
    ships it. Cleaning those up is a person's decision, made afterwards.
  - Debian: the release notes' procedure. Point the Debian entries in the
    apt sources at the new codename, then apt-get upgrade
    --without-new-pkgs and apt-get full-upgrade. Entries from anyone else
    that name the old codename are turned off, as do-release-upgrade does,
    and listed: whether the vendor publishes for the new release is theirs
    to say. No autoremove.
  - RHEL and its rebuilds: leapp (ELevate on AlmaLinux, Rocky and Oracle
    Linux). Its preupgrade report is the check, its inhibitors are what the
    run stops on, and the upgrade itself happens in leapp's own boot after
    the reboot.

The server must be ready first: nothing waiting for the current release, no
reboot pending, enough disk. Those are what the distributions' own tools
refuse on, and saying so in the console beats a run that fails at once.
argus-control on the same server is refused outright: an upgrade takes the
console down part way through and nobody could follow it (docs/os-upgrades.md
is the procedure by hand).

The run is the same helper and unit as any update (packages_apply.go), so it
goes on whatever the agent does, and its last step is always a reboot. The
agent that comes back on the new boot does not take the reboot as success:
it reads /etc/os-release, and the run is done only when the server says it
is on the release it was asked for. It also checks that what the agent
itself uses on the server (sshd and its sftp-server, the PostgreSQL and MySQL
clients it runs backups and databases with) is still there, and installs it
again from the new release when an upgrade took it away.

The agent itself needs nothing from the release: it is one static binary
from a package no distribution ships, which neither do-release-upgrade nor
leapp removes, and its unit only asks for systemd.
*/

// ReleaseInfo is the server's release and the one it can be upgraded to.
type ReleaseInfo struct {
	// Distro is os-release's ID: ubuntu, debian, rhel, almalinux...
	Distro string `json:"distro"`
	// Current is os-release's PRETTY_NAME, Version its VERSION_ID.
	Current string `json:"current"`
	Version string `json:"version"`
	// Next is the VERSION_ID the upgrade ends on, NextName how to say it.
	// Empty when there is no newer release to go to.
	Next     string `json:"next,omitempty"`
	NextName string `json:"next_name,omitempty"`
	// Tool is what does the upgrade: do-release-upgrade, apt or leapp.
	Tool string `json:"tool,omitempty"`
	// Reason is why this server cannot be upgraded from the console at
	// all; Blockers why not now, each something a person can fix.
	Reason   string   `json:"reason,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
}

// ReleaseChange is the upgrade a run is making.
type ReleaseChange struct {
	Distro   string `json:"distro"`
	From     string `json:"from"`
	FromName string `json:"from_name"`
	To       string `json:"to"`
	ToName   string `json:"to_name"`
	Tool     string `json:"tool"`
}

// logFile is where the tool that did the upgrade says what went wrong.
func (c ReleaseChange) logFile() string {
	switch c.Tool {
	case releaseToolUbuntu:
		return "/var/log/dist-upgrade/main.log"
	case releaseToolLeapp:
		return "/var/log/leapp/leapp-upgrade.log"
	}
	return "/var/log/apt/term.log"
}

// notRebooted is what to say when the run asked for its reboot and the
// server is still on the same boot.
func (c ReleaseChange) notRebooted() string {
	if c.Tool == releaseToolLeapp {
		return "leapp prepared the upgrade to " + c.ToName + ", but the server did not reboot, and the upgrade " +
			"itself happens during that reboot; reboot it to carry it out"
	}
	return "upgraded to " + c.ToName + ", but the reboot did not happen; reboot it so it runs the new release"
}

const (
	releaseToolUbuntu = "do-release-upgrade"
	releaseToolDebian = "apt"
	releaseToolLeapp  = "leapp"
	// releaseRunTimeout bounds a release upgrade's helper. A big server
	// takes an hour or two; leapp's preupgrade alone can take twenty
	// minutes.
	releaseRunTimeout = 6 * time.Hour
	// releaseMinFree is the free space / and /var need. Ubuntu's upgrader
	// and leapp check for themselves and stop before changing anything;
	// this says it in the console, and is the only check Debian gets.
	releaseMinFree = 3 << 30
	releaseDepsFile = "release-deps.json"
)

// reOSVersion is a VERSION_ID a run can be approved for: 24.04, 13, 9.
var reOSVersion = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3})?$`)

// Paths, replaced in tests.
var (
	osReleaseFiles = []string{"/etc/os-release", "/usr/lib/os-release"}
	// ubuntuUpgradeOverrides is read by do-release-upgrade after its own
	// configuration.
	ubuntuUpgradeOverrides = "/etc/update-manager/release-upgrades.d"
	aptSourcesRoot         = "/etc/apt"
	leappReportFile        = "/var/log/leapp/leapp-report.json"
	// argusControlBinary is the control plane, when it is installed here.
	argusControlBinary = "/usr/bin/argus-control"
	releaseFree        = freeBytes
	toolGlob           = filepath.Glob
)

// osRelease reads /etc/os-release into its keys.
func osRelease() map[string]string {
	for _, f := range osReleaseFiles {
		if b, err := os.ReadFile(f); err == nil {
			return parseOSRelease(b)
		}
	}
	return nil
}

func parseOSRelease(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `"'`)
		}
		out[k] = v
	}
	return out
}

// versionIs says whether VERSION_ID now is the release want: 24.04 is
// 24.04, 9.4 is 9.
func versionIs(now, want string) bool {
	return now != "" && (now == want || strings.HasPrefix(now, want+"."))
}

// debianNext is each Debian release's next, by major version, with
// codenames. A release goes in only once it is stable: the Debian path has
// no tool that would know.
var debianNext = map[string][4]string{
	"10": {"11", "buster", "bullseye", "Debian 11 (bullseye)"},
	"11": {"12", "bullseye", "bookworm", "Debian 12 (bookworm)"},
	"12": {"13", "bookworm", "trixie", "Debian 13 (trixie)"},
}

// rhelFamily is what leapp upgrades.
var rhelFamily = map[string]string{"rhel": "Red Hat Enterprise Linux", "almalinux": "AlmaLinux",
	"rocky": "Rocky Linux", "ol": "Oracle Linux", "centos": "CentOS Linux"}

const releaseUnsupported = "release upgrades from the console are for Ubuntu, Debian, Red Hat Enterprise Linux, " +
	"AlmaLinux, Rocky Linux and Oracle Linux"

// release works out the server's release and its next one. r is what the
// same check found waiting, which a release upgrade has to wait for.
func (c PackageChecker) release(ctx context.Context, r PackageReport) *ReleaseInfo {
	osr := osRelease()
	if osr == nil {
		return nil
	}
	info := &ReleaseInfo{Distro: osr["ID"], Current: osr["PRETTY_NAME"], Version: osr["VERSION_ID"]}
	if info.Current == "" {
		info.Current = strings.TrimSpace(osr["NAME"] + " " + info.Version)
	}
	major, _, _ := strings.Cut(info.Version, ".")
	switch {
	case info.Distro == "ubuntu":
		info.Tool = releaseToolUbuntu
		if _, err := c.Look("do-release-upgrade"); err != nil {
			info.Reason = "do-release-upgrade is not installed; install ubuntu-release-upgrader-core " +
				"(apt-get install ubuntu-release-upgrader-core) and the next Ubuntu release is offered here"
			return info
		}
		out, _, err := output(ctx, c.Run, aptEnv, "do-release-upgrade", "-c")
		if err != nil {
			info.Reason = "do-release-upgrade could not be run: " + err.Error()
			return info
		}
		next, why := parseReleaseCheck(out)
		if next == "" {
			info.Reason = why
			return info
		}
		info.Next, info.NextName = ubuntuVersionID(next), "Ubuntu "+next
	case info.Distro == "debian":
		info.Tool = releaseToolDebian
		n, ok := debianNext[major]
		if !ok {
			return info // the newest release, or one too old for this path
		}
		info.Next, info.NextName = n[0], n[3]
	case rhelFamily[info.Distro] != "":
		info.Tool = releaseToolLeapp
		if _, err := c.Look("leapp"); err != nil {
			info.Reason = "leapp is not installed; install leapp-upgrade (on AlmaLinux, Rocky Linux and Oracle " +
				"Linux, the ELevate packages) and the next major release is offered here"
			return info
		}
		m, err := strconv.Atoi(major)
		if err != nil || m < 7 || m > 9 {
			return info
		}
		info.Next = strconv.Itoa(m + 1)
		info.NextName = rhelFamily[info.Distro] + " " + info.Next
	default:
		info.Reason = releaseUnsupported
		return info
	}
	if info.Next == "" {
		return info
	}
	info.Blockers = releaseBlockers(r)
	return info
}

// releaseBlockers is what has to be fixed before the server can move to a
// new release.
func releaseBlockers(r PackageReport) []string {
	var out []string
	if _, err := os.Stat(argusControlBinary); err == nil {
		out = append(out, "argus-control runs on this server, and upgrading it from the console would take the "+
			"console down part way through; upgrade it by hand, as docs/os-upgrades.md describes")
	}
	if n := len(r.Updates); n > 0 {
		out = append(out, fmt.Sprintf("install the %d update%s waiting for the current release first", n, plural(n)))
	}
	if r.RebootRequired {
		out = append(out, "reboot it first: an update already installed waits for a reboot")
	}
	for _, dir := range []string{"/", "/var"} {
		if free := releaseFree(dir); free > 0 && free < releaseMinFree {
			out = append(out, fmt.Sprintf("%s has %.1f GB free and an upgrade needs at least %d GB", dir,
				float64(free)/(1<<30), releaseMinFree>>30))
		}
	}
	return sortedUnique(out)
}

// reNewRelease reads do-release-upgrade -c: "New release '24.04.1 LTS' available."
var reNewRelease = regexp.MustCompile(`New release '([^']+)' available`)

// parseReleaseCheck reads do-release-upgrade -c's output: the next release,
// or why there is none to offer.
func parseReleaseCheck(b []byte) (next, why string) {
	if m := reNewRelease.FindSubmatch(b); m != nil {
		return strings.TrimSpace(string(m[1])), ""
	}
	s := string(b)
	switch {
	case strings.Contains(s, "Prompt") && strings.Contains(s, "never"):
		return "", "release upgrades are turned off on this server (Prompt=never in /etc/update-manager/release-upgrades)"
	case strings.Contains(s, "not possible right now"):
		return "", "Ubuntu says upgrading from this release is not possible right now; " + tail(b, 300)
	}
	return "", ""
}

var reUbuntuVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+`)

// ubuntuVersionID is "24.04" from "24.04.1 LTS".
func ubuntuVersionID(v string) string {
	if m := reUbuntuVersion.FindString(v); m != "" {
		return m
	}
	return v
}

/* ── Debian's sources ────────────────────────────────────────────────────── */

// sourcesChange is what pointing apt at a new release did.
type sourcesChange struct {
	// Rewritten are the files whose Debian entries now name the new
	// release; Disabled the entries from anyone else that were turned off.
	Rewritten []string `json:"rewritten,omitempty"`
	Disabled  []string `json:"disabled,omitempty"`
}

// debianSuite moves one suite to the new codename, and says whether it named
// the old one: bookworm, bookworm-updates, bookworm-security. Debian 10
// spelled its security suite buster/updates.
func debianSuite(suite, from, to string) (string, bool) {
	switch {
	case suite == from:
		return to, true
	case suite == from+"/updates":
		return to + "-security", true
	case strings.HasPrefix(suite, from+"-"):
		return to + suite[len(from):], true
	}
	return suite, false
}

// debianArchive is a URI Debian itself serves, or a mirror of it.
func debianArchive(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "debian.org" || strings.HasSuffix(host, ".debian.org") {
		return true
	}
	p := strings.TrimSuffix(u.Path, "/")
	return p == "/debian" || p == "/debian-security"
}

// disabledMark starts a line Argus turned off.
const disabledMark = "# disabled by Argus for the upgrade to "

// rewriteSourcesList moves a one-line-style sources file to the new release.
func rewriteSourcesList(b []byte, from, to string) (out []byte, rewritten bool, disabled []string) {
	var buf bytes.Buffer
	lines := strings.SplitAfter(string(b), "\n")
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\n")
		f := strings.Fields(line)
		if len(f) < 3 || (f[0] != "deb" && f[0] != "deb-src") {
			buf.WriteString(raw)
			continue
		}
		// deb [ options ] uri suite components...
		i := 1
		if strings.HasPrefix(f[1], "[") {
			for i < len(f) && !strings.HasSuffix(f[i], "]") {
				i++
			}
			i++
		}
		if i+1 >= len(f) {
			buf.WriteString(raw)
			continue
		}
		uri, suite := f[i], f[i+1]
		next, named := debianSuite(suite, from, to)
		if !named {
			buf.WriteString(raw)
			continue
		}
		nl := strings.TrimPrefix(raw, line)
		if debianArchive(uri) {
			f[i+1] = next
			// Fields loses the spacing; an entry is still one line.
			buf.WriteString(strings.Join(f, " ") + nl)
			rewritten = true
			continue
		}
		buf.WriteString(disabledMark + to + ": " + line + nl)
		disabled = append(disabled, uri+" "+suite)
	}
	return buf.Bytes(), rewritten, disabled
}

// rewriteSourcesDeb822 moves a deb822 .sources file to the new release, one
// stanza at a time.
func rewriteSourcesDeb822(b []byte, from, to string) (out []byte, rewritten bool, disabled []string) {
	var buf bytes.Buffer
	stanzas := regexp.MustCompile(`\n[ \t]*\n`).Split(string(b), -1)
	for si, st := range stanzas {
		if si > 0 {
			buf.WriteString("\n\n")
		}
		lines := strings.Split(st, "\n")
		var uris []string
		suitesAt, enabledAt := -1, -1
		for li, l := range lines {
			k, v, ok := strings.Cut(l, ":")
			if !ok || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "#") {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "uris":
				uris = strings.Fields(v)
			case "suites":
				suitesAt = li
			case "enabled":
				enabledAt = li
			}
		}
		if suitesAt < 0 || len(uris) == 0 {
			buf.WriteString(st)
			continue
		}
		_, v, _ := strings.Cut(lines[suitesAt], ":")
		suites := strings.Fields(v)
		named := false
		for i, s := range suites {
			n, ok := debianSuite(s, from, to)
			named = named || ok
			suites[i] = n
		}
		if !named {
			buf.WriteString(st)
			continue
		}
		ours := true
		for _, u := range uris {
			ours = ours && debianArchive(u)
		}
		if ours {
			lines[suitesAt] = "Suites: " + strings.Join(suites, " ")
			rewritten = true
		} else {
			if enabledAt >= 0 {
				lines[enabledAt] = "Enabled: no"
				lines = append([]string{disabledMark + to}, lines...)
			} else {
				lines = append([]string{disabledMark + to, "Enabled: no"}, lines...)
			}
			disabled = append(disabled, strings.Join(uris, " ")+" "+strings.TrimSpace(v))
		}
		buf.WriteString(strings.Join(lines, "\n"))
	}
	return buf.Bytes(), rewritten, disabled
}

// sourceFiles are apt's sources under root: sources.list, and the .list and
// .sources files beside it.
func sourceFiles(root string) []string {
	var out []string
	if _, err := os.Stat(filepath.Join(root, "sources.list")); err == nil {
		out = append(out, filepath.Join(root, "sources.list"))
	}
	for _, pat := range []string{"*.list", "*.sources"} {
		m, _ := filepath.Glob(filepath.Join(root, "sources.list.d", pat))
		out = append(out, m...)
	}
	sort.Strings(out)
	return out
}

// pointSourcesAt rewrites apt's sources for the new release, keeping a copy
// of every file it changes under backup to put back.
func pointSourcesAt(root, backup, from, to string) (sourcesChange, error) {
	var ch sourcesChange
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return ch, err
	}
	for _, f := range sourceFiles(root) {
		b, err := os.ReadFile(f)
		if err != nil {
			return ch, err
		}
		var (
			out       []byte
			rewritten bool
			disabled  []string
		)
		if strings.HasSuffix(f, ".sources") {
			out, rewritten, disabled = rewriteSourcesDeb822(b, from, to)
		} else {
			out, rewritten, disabled = rewriteSourcesList(b, from, to)
		}
		if bytes.Equal(out, b) {
			continue
		}
		rel, _ := filepath.Rel(root, f)
		keep := filepath.Join(backup, rel)
		if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
			return ch, err
		}
		if err := os.WriteFile(keep, b, 0o600); err != nil {
			return ch, err
		}
		fi, err := os.Stat(f)
		if err != nil {
			return ch, err
		}
		if err := writeFileAtomic(f, out, fi.Mode().Perm()); err != nil {
			return ch, err
		}
		if rewritten {
			ch.Rewritten = append(ch.Rewritten, f)
		}
		ch.Disabled = append(ch.Disabled, disabled...)
	}
	return ch, nil
}

// restoreSources puts back what pointSourcesAt changed.
func restoreSources(root, backup string) error {
	return filepath.WalkDir(backup, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(backup, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return writeFileAtomic(filepath.Join(root, rel), b, 0o644)
	})
}

/* ── leapp ───────────────────────────────────────────────────────────────── */

// leappInhibitors reads leapp's preupgrade report for what stops the
// upgrade, each with leapp's own hint when it gives one.
func leappInhibitors(b []byte) []string {
	var rep struct {
		Entries []struct {
			Title  string   `json:"title"`
			Groups []string `json:"groups"`
			Flags  []string `json:"flags"`
			Detail struct {
				Remediations []struct {
					Type    string          `json:"type"`
					Context json.RawMessage `json:"context"`
				} `json:"remediations"`
			} `json:"detail"`
		} `json:"entries"`
	}
	if json.Unmarshal(b, &rep) != nil {
		return nil
	}
	var out []string
	for _, e := range rep.Entries {
		inhibits := false
		for _, g := range append(append([]string{}, e.Groups...), e.Flags...) {
			inhibits = inhibits || g == "inhibitor"
		}
		if !inhibits {
			continue
		}
		s := strings.TrimSpace(e.Title)
		for _, r := range e.Detail.Remediations {
			var hint string
			if r.Type == "hint" && json.Unmarshal(r.Context, &hint) == nil && hint != "" {
				s += " (" + strings.TrimSpace(hint) + ")"
				break
			}
		}
		out = append(out, s)
	}
	return out
}

/* ── What the agent needs on the server ──────────────────────────────────── */

// releaseDep is a program the agent runs on its server, where it was before
// the upgrade, and the package it came from.
type releaseDep struct {
	Tool    string `json:"tool"`
	Path    string `json:"path"`
	Package string `json:"package,omitempty"`
}

// agentTools are what the agent runs on a server: sshd and its sftp-server
// for sessions and file transfers, the database clients for backups and
// database access. Only those present before an upgrade are looked for
// after it.
var agentTools = []string{"sshd", "sftp-server", "pg_dump", "pg_restore", "psql", "mysql", "mysqldump",
	"mariadb", "mariadb-dump", "mongosh", "mongodump", "mongorestore"}

// toolFallbacks are packages that bring a tool back when the one it came
// from is not in the new release (postgresql-client-14 on Ubuntu 24.04).
var toolFallbacks = map[string]map[string][]string{
	"apt": {
		"sshd": {"openssh-server"}, "sftp-server": {"openssh-sftp-server", "openssh-server"},
		"pg_dump": {"postgresql-client"}, "pg_restore": {"postgresql-client"}, "psql": {"postgresql-client"},
		"mysql": {"default-mysql-client", "mysql-client", "mariadb-client"},
		"mysqldump": {"default-mysql-client", "mysql-client", "mariadb-client"},
		"mariadb": {"mariadb-client"}, "mariadb-dump": {"mariadb-client"},
	},
	"dnf": {
		"sshd": {"openssh-server"}, "sftp-server": {"openssh-server"},
		"pg_dump": {"postgresql"}, "pg_restore": {"postgresql"}, "psql": {"postgresql"},
		"mysql": {"mysql", "mariadb"}, "mysqldump": {"mysql", "mariadb"},
		"mariadb": {"mariadb"}, "mariadb-dump": {"mariadb"},
	},
}

// findTool finds an agent tool where the agent itself would look.
func findTool(look lookPathFunc, tool string) string {
	if p, err := look(tool); err == nil {
		return p
	}
	var pats []string
	switch tool {
	case "sshd":
		pats = []string{"/usr/sbin/sshd"}
	case "sftp-server":
		pats = []string{"/usr/lib/openssh/sftp-server", "/usr/libexec/openssh/sftp-server",
			"/usr/lib/ssh/sftp-server"}
	case "pg_dump", "pg_restore", "psql":
		pats = []string{"/usr/lib/postgresql/*/bin/" + tool, "/usr/pgsql-*/bin/" + tool}
	}
	for _, pat := range pats {
		if m, _ := toolGlob(pat); len(m) > 0 {
			sort.Strings(m)
			return m[len(m)-1]
		}
	}
	return ""
}

// ownerOf is the package a file came from, "" when none says.
func ownerOf(ctx context.Context, run cmdRunner, manager, path string) string {
	switch manager {
	case "apt":
		b, code, err := output(ctx, run, nil, "dpkg-query", "-S", path)
		if err != nil || code != 0 {
			return ""
		}
		// "openssh-server: /usr/sbin/sshd", or "a, b: /path" for a shared one.
		name, _, _ := strings.Cut(strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]), ":")
		name, _, _ = strings.Cut(name, ",")
		if rePackageName.MatchString(strings.TrimSpace(name)) {
			return strings.TrimSpace(name)
		}
	case "dnf", "yum":
		b, code, err := output(ctx, run, nil, "rpm", "-qf", "--qf", "%{NAME}\n", path)
		if err != nil || code != 0 {
			return ""
		}
		name := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		if rePackageName.MatchString(name) {
			return name
		}
	}
	return ""
}

// snapshotDeps lists the agent's tools on the server now.
func snapshotDeps(ctx context.Context, run cmdRunner, look lookPathFunc, manager string) []releaseDep {
	var out []releaseDep
	for _, t := range agentTools {
		p := findTool(look, t)
		if p == "" {
			continue
		}
		out = append(out, releaseDep{Tool: t, Path: p, Package: ownerOf(ctx, run, manager, p)})
	}
	return out
}

// ReleaseFinish is what the helper found after the reboot.
type ReleaseFinish struct {
	// Restored are packages installed again because a tool the agent uses
	// was gone; Missing the tools still gone.
	Restored []string `json:"restored,omitempty"`
	Missing  []string `json:"missing,omitempty"`
	// Notes are what a person should look at next.
	Notes []string `json:"notes,omitempty"`
	// Repair is why finishing what the package manager left failed.
	Repair string `json:"repair,omitempty"`
}

// managerFamily is the key into toolFallbacks.
func managerFamily(m pkgManager) string {
	if m == nil {
		return ""
	}
	if m.name() == "yum" {
		return "dnf"
	}
	return m.name()
}

// installOne installs one package, saying whether it worked.
func installOne(ctx context.Context, run cmdRunner, m pkgManager, log io.Writer, pkg string) bool {
	if !rePackageName.MatchString(pkg) {
		return false
	}
	var code int
	var err error
	switch m.(type) {
	case aptManager:
		args := append(append([]string{}, aptLock...), "-y", "-q", "-o", "Dpkg::Options::=--force-confdef",
			"-o", "Dpkg::Options::=--force-confold", "-o", "Dpkg::Use-Pty=0", "install", pkg)
		code, err = run(ctx, log, aptEnv, "apt-get", args...)
	case dnfManager:
		code, err = run(ctx, log, nil, m.name(), "-y", "install", pkg)
	default:
		return false
	}
	return err == nil && code == 0
}

// finishRelease runs after the reboot that ends a release upgrade, as root:
// it finishes what the package manager left, brings back the agent's tools,
// and takes away Argus's setting for the upgrade.
func finishRelease(ctx context.Context, run cmdRunner, look lookPathFunc, stateDir string, log io.Writer) ReleaseFinish {
	var res ReleaseFinish
	_ = os.Remove(filepath.Join(ubuntuUpgradeOverrides, ubuntuOverrideName))
	m := detectManager(look)
	if m == nil {
		return res
	}
	if err := m.repair(ctx, run, log); err != nil {
		res.Repair = err.Error()
	}
	var deps []releaseDep
	if b, err := os.ReadFile(filepath.Join(stateDir, packagesDir, releaseDepsFile)); err == nil {
		_ = json.Unmarshal(b, &deps)
	}
	fam := managerFamily(m)
	refreshed := false
	for _, d := range deps {
		if findTool(look, d.Tool) != "" {
			continue
		}
		if !refreshed {
			_ = m.refresh(ctx, run, log)
			refreshed = true
		}
		back := ""
		for _, pkg := range append([]string{d.Package}, toolFallbacks[fam][d.Tool]...) {
			if pkg == "" {
				continue
			}
			if installOne(ctx, run, m, log, pkg) && findTool(look, d.Tool) != "" {
				back = pkg
				break
			}
		}
		if back != "" {
			res.Restored = append(res.Restored, back)
		} else {
			res.Missing = append(res.Missing, d.Tool)
		}
	}
	res.Restored, res.Missing = sortedUnique(res.Restored), sortedUnique(res.Missing)
	res.Notes = disabledSourceNotes(aptSourcesRoot)
	return res
}

// disabledSourceNotes names the apt sources an upgrade turned off: by Argus
// on Debian, by do-release-upgrade on Ubuntu.
func disabledSourceNotes(root string) []string {
	var off []string
	for _, f := range sourceFiles(root) {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		s := string(b)
		if strings.Contains(s, strings.TrimSpace(disabledMark)) || strings.Contains(s, "disabled on upgrade to") {
			off = append(off, filepath.Base(f))
		}
	}
	if len(off) == 0 {
		return nil
	}
	return []string{"apt sources from outside the distribution were turned off for the upgrade (" +
		strings.Join(off, ", ") + "); turn each back on once its vendor publishes for the new release"}
}

// RunPackagesReleaseFinish is `argus-agent packages-release-finish --state
// <dir>`: what finishRelease found, as JSON on stdout.
func RunPackagesReleaseFinish(stateDir string, stdout io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	res := finishRelease(ctx, execRunner, exec.LookPath, stateDir, os.Stderr)
	if err := json.NewEncoder(stdout).Encode(res); err != nil {
		return 1
	}
	return 0
}

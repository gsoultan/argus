package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
)

/*
The package managers Argus updates a Linux server with: apt, dnf, yum and
zypper. Each says what can be upgraded, which of that fixes a security issue,
installs a chosen set, and says afterwards whether a reboot is wanted.

Everything here runs in the helper (packages_apply.go), as root and outside
the agent's sandbox, because refreshing the package lists already writes to
/var. The commands never prompt: a question nobody can answer would hang a
run until its timeout, so each tool is told to take the default answer and
to keep a configuration file someone changed by hand.
*/

// cmdRunner runs a command and returns its exit code. out, when not nil,
// receives stdout and stderr together. err is set only when the command
// could not be run at all; a non-zero exit is a code, not an error.
type cmdRunner func(ctx context.Context, out io.Writer, env []string, name string, args ...string) (int, error)

// execRunner is the real cmdRunner.
func execRunner(ctx context.Context, out io.Writer, env []string, name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	if out != nil {
		cmd.Stdout, cmd.Stderr = out, out
	}
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// output runs a command and returns what it printed.
func output(ctx context.Context, run cmdRunner, env []string, name string, args ...string) ([]byte, int, error) {
	var b bytes.Buffer
	code, err := run(ctx, &b, env, name, args...)
	return b.Bytes(), code, err
}

// pkgManager is one package manager's commands.
type pkgManager interface {
	name() string
	// refresh downloads the package lists.
	refresh(ctx context.Context, run cmdRunner, log io.Writer) error
	// list says what can be upgraded now, from the lists last refreshed.
	list(ctx context.Context, run cmdRunner) ([]PackageUpdate, int, error)
	// install upgrades names, or everything when all is set. holds are
	// globs never upgraded.
	install(ctx context.Context, run cmdRunner, log io.Writer, names []string, all bool, holds []string) error
	// installed reads the installed version of each name.
	installed(ctx context.Context, run cmdRunner, names []string) map[string]string
	// rebootWanted says whether the server should be rebooted for what is
	// installed now to be what runs.
	rebootWanted(ctx context.Context, run cmdRunner) bool
	// staleServices names the running services still using a replaced
	// library, when the server can say.
	staleServices(ctx context.Context, run cmdRunner) []string
	// repair finishes a transaction an interruption left half done.
	repair(ctx context.Context, run cmdRunner, log io.Writer) error
}

// lookPathFunc is exec.LookPath, replaced in tests.
type lookPathFunc func(string) (string, error)

// detectManager finds the package manager this server uses, nil when it has
// none Argus knows.
func detectManager(look lookPathFunc) pkgManager {
	if _, err := look("apt-get"); err == nil {
		if _, err := look("dpkg-query"); err == nil {
			return aptManager{}
		}
	}
	if _, err := look("zypper"); err == nil {
		return zypperManager{}
	}
	if _, err := look("dnf"); err == nil {
		return dnfManager{tool: "dnf"}
	}
	if _, err := look("yum"); err == nil {
		return dnfManager{tool: "yum"}
	}
	return nil
}

// tail is the last n bytes of b, for an error message.
func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

/* ── apt ─────────────────────────────────────────────────────────────────── */

type aptManager struct{}

func (aptManager) name() string { return "apt" }

// aptEnv keeps apt, debconf and needrestart from asking anything. needrestart
// only lists: the helper restarts what it names itself, so it can leave the
// agent and the session services alone.
var aptEnv = []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l", "NEEDRESTART_SUSPEND=1",
	"APT_LISTCHANGES_FRONTEND=none"}

// aptLock waits for another apt (unattended-upgrades, a person) to finish
// rather than failing at once, for up to two minutes.
var aptLock = []string{"-o", "DPkg::Lock::Timeout=120"}

func (aptManager) refresh(ctx context.Context, run cmdRunner, log io.Writer) error {
	var b bytes.Buffer
	w := io.Writer(&b)
	if log != nil {
		w = io.MultiWriter(&b, log)
	}
	code, err := run(ctx, w, aptEnv, "apt-get", append(append([]string{}, aptLock...), "-q", "update")...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("apt-get update failed (exit %d): %s", code, tail(b.Bytes(), 400))
	}
	return aptFetchFailed(b.Bytes())
}

// aptFetchFailed reads apt-get update's output for lists it could not
// download. apt says so with a warning and exits 0, keeping the old lists:
// taken at its word, a server that cannot reach its mirrors would look up to
// date for ever.
func aptFetchFailed(out []byte) error {
	var failed []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "W: Failed to fetch") || strings.HasPrefix(line, "E: Failed to fetch") {
			failed = append(failed, line)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("could not download %d package list%s, so what is waiting is not known: %s",
		len(failed), plural(len(failed)), tail([]byte(failed[0]), 300))
}

func (aptManager) list(ctx context.Context, run cmdRunner) ([]PackageUpdate, int, error) {
	// A simulated upgrade lists exactly what a real one would install, with
	// the archive each version comes from, which is how a security fix is
	// told from the rest. --with-new-pkgs is what the install uses too: a
	// kernel update pulls in a new package, and upgrade alone holds it back.
	b, code, err := output(ctx, run, aptEnv, "apt-get",
		"-s", "-q", "-o", "Debug::NoLocking=1", "--with-new-pkgs", "upgrade")
	if err != nil {
		return nil, 0, err
	}
	if code != 0 {
		return nil, 0, fmt.Errorf("apt-get could not work out the upgrade (exit %d): %s", code, tail(b, 400))
	}
	return parseAptSimulate(b), 0, nil
}

// reAptInst reads one line of a simulated upgrade:
//
//	Inst openssl [3.0.13-0ubuntu3.4] (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
//	Inst linux-image-6.8.0-48-generic (6.8.0-48.48 Ubuntu:24.04/noble-security [amd64])
var reAptInst = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+) (.*?)(?: \[[^\]]*\])?\)`)

func parseAptSimulate(b []byte) []PackageUpdate {
	var out []PackageUpdate
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		m := reAptInst.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		u := PackageUpdate{Name: m[1], Installed: m[2], Available: m[3], New: m[2] == ""}
		for _, origin := range strings.Split(m[4], ",") {
			origin = strings.TrimSpace(origin)
			lo := strings.ToLower(origin)
			if strings.Contains(lo, "-security") || strings.Contains(lo, "debian-security") {
				u.Security = true
				u.Origin = origin
			}
		}
		if u.Origin == "" {
			u.Origin = strings.TrimSpace(strings.Split(m[4], ",")[0])
		}
		u.Reboot = kernelPackage(u.Name)
		out = append(out, u)
	}
	return out
}

func (aptManager) install(ctx context.Context, run cmdRunner, log io.Writer, names []string, all bool,
	holds []string) error {
	// Keep a configuration file changed by hand, take the package's for one
	// that was not: never ask.
	args := append(append([]string{}, aptLock...), "-y", "-q",
		"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold",
		// No progress bar: it is one line rewritten in place, which a log
		// shows as a wall of percentages.
		"-o", "Dpkg::Use-Pty=0")
	if all {
		// apt has no exclude flag. Holding a package here would outlive the
		// run, so a held package is left out by naming what to upgrade:
		// everything the simulation lists that no hold matches.
		ups, _, err := aptManager{}.list(ctx, run)
		if err != nil {
			return err
		}
		names = names[:0]
		for _, u := range ups {
			if !u.New && !held(u.Name, holds) {
				names = append(names, u.Name)
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	args = append(args, "install", "--only-upgrade")
	args = append(args, names...)
	var b bytes.Buffer
	code, err := run(ctx, io.MultiWriter(&b, log), aptEnv, "apt-get", args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("apt-get install failed (exit %d): %s", code, tail(b.Bytes(), 400))
	}
	return nil
}

func (aptManager) installed(ctx context.Context, run cmdRunner, names []string) map[string]string {
	out := map[string]string{}
	if len(names) == 0 {
		return out
	}
	b, _, err := output(ctx, run, nil, "dpkg-query", append([]string{"-W", "-f=${Package}\t${Version}\n"}, names...)...)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if name, ver, ok := strings.Cut(line, "\t"); ok && ver != "" {
			out[name] = ver
		}
	}
	return out
}

func (aptManager) rebootWanted(context.Context, cmdRunner) bool {
	_, err := os.Stat(rebootRequiredFile)
	return err == nil
}

// rebootRequiredFile is where Debian and Ubuntu say a reboot is wanted.
var rebootRequiredFile = "/var/run/reboot-required"

func (aptManager) staleServices(ctx context.Context, run cmdRunner) []string {
	// Absent without needrestart installed: the run then restarts nothing
	// and says a reboot is the way to be sure.
	b, _, err := output(ctx, run, []string{"NEEDRESTART_SUSPEND=1"}, "needrestart", "-b", "-r", "l")
	if err != nil {
		return nil
	}
	return parseNeedrestart(b)
}

// parseNeedrestart reads needrestart's batch output: NEEDRESTART-SVC: nginx.service
func parseNeedrestart(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if svc, ok := strings.CutPrefix(strings.TrimSpace(line), "NEEDRESTART-SVC:"); ok {
			if svc = strings.TrimSpace(svc); svc != "" {
				out = append(out, svc)
			}
		}
	}
	return out
}

func (aptManager) repair(ctx context.Context, run cmdRunner, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	// Three steps, each for what the one before cannot fix: configure what
	// was unpacked, let apt finish what it planned, and reinstall a package
	// dpkg was cut off in the middle of unpacking, which configure cannot
	// touch ("in a mess ... must be reinstalled").
	if _, err := run(ctx, log, aptEnv, "dpkg", "--force-confdef", "--force-confold", "--configure", "-a"); err != nil {
		return err
	}
	fix := append(append([]string{}, aptLock...), "-y", "-q", "-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold", "-f", "install")
	if _, err := run(ctx, log, aptEnv, "apt-get", fix...); err != nil {
		return err
	}
	audit, _, err := output(ctx, run, nil, "dpkg", "--audit")
	if err != nil {
		return err
	}
	if broken := parseDpkgAudit(audit); len(broken) > 0 {
		args := append(append([]string{}, aptLock...), "-y", "-q", "-o", "Dpkg::Options::=--force-confdef",
			"-o", "Dpkg::Options::=--force-confold", "install", "--reinstall")
		code, err := run(ctx, log, aptEnv, "apt-get", append(args, broken...)...)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("reinstalling %s failed (exit %d)", strings.Join(broken, ", "), code)
		}
	}
	audit, _, err = output(ctx, run, nil, "dpkg", "--audit")
	if err != nil {
		return err
	}
	if left := parseDpkgAudit(audit); len(left) > 0 || strings.TrimSpace(string(audit)) != "" {
		return fmt.Errorf("dpkg still reports problems: %s", tail(audit, 300))
	}
	return nil
}

// parseDpkgAudit names the packages `dpkg --audit` lists, one per indented
// line: " libapt-pkg6.0t64:amd64 package management runtime library".
func parseDpkgAudit(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, " ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		name, _, _ := strings.Cut(f[0], ":")
		if rePackageName.MatchString(name) {
			out = append(out, name)
		}
	}
	return sortedUnique(out)
}

/* ── dnf and yum ─────────────────────────────────────────────────────────── */

type dnfManager struct{ tool string }

func (m dnfManager) name() string { return m.tool }

func (m dnfManager) refresh(ctx context.Context, run cmdRunner, log io.Writer) error {
	var b bytes.Buffer
	w := io.Writer(&b)
	if log != nil {
		w = io.MultiWriter(&b, log)
	}
	code, err := run(ctx, w, nil, m.tool, "-q", "-y", "makecache")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s makecache failed (exit %d): %s", m.tool, code, tail(b.Bytes(), 400))
	}
	return nil
}

func (m dnfManager) list(ctx context.Context, run cmdRunner) ([]PackageUpdate, int, error) {
	b, code, err := output(ctx, run, nil, m.tool, "-q", "check-update")
	if err != nil {
		return nil, 0, err
	}
	// 100 is "there are updates", 0 is "none"; anything else failed.
	if code != 0 && code != 100 {
		return nil, 0, fmt.Errorf("%s check-update failed (exit %d): %s", m.tool, code, tail(b, 400))
	}
	ups := parseDnfCheckUpdate(b)
	if len(ups) == 0 {
		return nil, 0, nil
	}
	installed := rpmInstalled(ctx, run)
	secArgs := []string{"-q", "updateinfo", "list", "--security", "--available"}
	if m.tool == "yum" {
		secArgs = []string{"-q", "updateinfo", "list", "security"}
	}
	sec := map[string]string{}
	if sb, scode, serr := output(ctx, run, nil, m.tool, secArgs...); serr == nil && scode == 0 {
		sec = parseDnfSecurity(sb)
	}
	for i := range ups {
		u := &ups[i]
		u.Installed = installed[u.Name]
		if adv, ok := sec[u.Name]; ok {
			u.Security, u.Origin = true, adv
		}
		base, _, _ := strings.Cut(u.Name, ".")
		u.Reboot = kernelPackage(base)
	}
	return ups, 0, nil
}

// parseDnfCheckUpdate reads `dnf -q check-update`: one "name.arch version
// repo" per line, then perhaps an "Obsoleting Packages" section, which is
// not upgrades.
func parseDnfCheckUpdate(b []byte) []PackageUpdate {
	var out []PackageUpdate
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Obsoleting") {
			break
		}
		f := strings.Fields(t)
		if len(f) != 3 || !strings.Contains(f[0], ".") || strings.HasPrefix(t, "Last metadata") {
			continue
		}
		out = append(out, PackageUpdate{Name: f[0], Available: f[1], Origin: f[2]})
	}
	return out
}

// parseDnfSecurity reads `dnf updateinfo list --security`:
//
//	RLSA-2024:1234 Important/Sec. openssl-1:3.0.7-27.el9.x86_64
//
// into name.arch -> advisory.
func parseDnfSecurity(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.Contains(f[1], "Sec") {
			continue
		}
		if na := nevraNameArch(f[len(f)-1]); na != "" {
			adv := f[0] + " · " + strings.TrimSuffix(strings.ToLower(f[1]), "/sec.")
			if _, seen := out[na]; !seen {
				out[na] = adv
			}
		}
	}
	return out
}

// nevraNameArch turns name-[epoch:]version-release.arch into name.arch.
func nevraNameArch(nevra string) string {
	dot := strings.LastIndex(nevra, ".")
	if dot < 0 {
		return ""
	}
	arch, rest := nevra[dot+1:], nevra[:dot]
	i := strings.LastIndex(rest, "-")
	if i < 0 {
		return ""
	}
	j := strings.LastIndex(rest[:i], "-")
	if j <= 0 {
		return ""
	}
	return rest[:j] + "." + arch
}

// rpmInstalled is every installed package as name.arch -> version-release.
func rpmInstalled(ctx context.Context, run cmdRunner) map[string]string {
	out := map[string]string{}
	b, _, err := output(ctx, run, nil, "rpm", "-qa", "--qf", `%{NAME}.%{ARCH}\t%{VERSION}-%{RELEASE}\n`)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if na, ver, ok := strings.Cut(line, "\t"); ok {
			out[na] = ver
		}
	}
	return out
}

func (m dnfManager) install(ctx context.Context, run cmdRunner, log io.Writer, names []string, all bool,
	holds []string) error {
	args := []string{"-y", "upgrade"}
	for _, h := range holds {
		args = append(args, "--exclude="+h)
	}
	if !all {
		if len(names) == 0 {
			return nil
		}
		args = append(args, names...)
	}
	var b bytes.Buffer
	code, err := run(ctx, io.MultiWriter(&b, log), nil, m.tool, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s upgrade failed (exit %d): %s", m.tool, code, tail(b.Bytes(), 400))
	}
	return nil
}

func (m dnfManager) installed(ctx context.Context, run cmdRunner, names []string) map[string]string {
	all := rpmInstalled(ctx, run)
	out := map[string]string{}
	for _, n := range names {
		if v, ok := all[n]; ok {
			out[n] = v
		}
	}
	return out
}

func (m dnfManager) rebootWanted(ctx context.Context, run cmdRunner) bool {
	// needs-restarting -r: exit 1 when a reboot is wanted. Absent without
	// dnf-utils, in which case the kernel check in the helper decides.
	_, code, err := output(ctx, run, nil, m.tool, "-q", "needs-restarting", "-r")
	return err == nil && code == 1
}

func (m dnfManager) staleServices(ctx context.Context, run cmdRunner) []string {
	b, code, err := output(ctx, run, nil, m.tool, "-q", "needs-restarting", "-s")
	if err != nil || code != 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); strings.HasSuffix(s, ".service") {
			out = append(out, s)
		}
	}
	return out
}

func (m dnfManager) repair(ctx context.Context, run cmdRunner, log io.Writer) error {
	// rpm transactions are atomic; what an interruption leaves is a stale
	// lock and, rarely, duplicate packages, which this cleans.
	if m.tool == "yum" {
		_, _ = run(ctx, log, nil, "yum-complete-transaction", "-y", "--cleanup-only")
		return nil
	}
	_, _ = run(ctx, log, nil, "rpm", "--rebuilddb")
	return nil
}

/* ── zypper ──────────────────────────────────────────────────────────────── */

type zypperManager struct{}

func (zypperManager) name() string { return "zypper" }

// zypperEnv waits for another zypper rather than failing at once.
var zypperEnv = []string{"ZYPP_LOCK_TIMEOUT=120"}

func (zypperManager) refresh(ctx context.Context, run cmdRunner, log io.Writer) error {
	var b bytes.Buffer
	w := io.Writer(&b)
	if log != nil {
		w = io.MultiWriter(&b, log)
	}
	code, err := run(ctx, w, zypperEnv, "zypper", "-n", "-q", "refresh")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("zypper refresh failed (exit %d): %s", code, tail(b.Bytes(), 400))
	}
	return nil
}

type zypperStream struct {
	Updates []struct {
		Kind     string `xml:"kind,attr"`
		Name     string `xml:"name,attr"`
		Edition  string `xml:"edition,attr"`
		Arch     string `xml:"arch,attr"`
		Category string `xml:"category,attr"`
		Status   string `xml:"status,attr"`
		Source   struct {
			Alias string `xml:"alias,attr"`
		} `xml:"source"`
	} `xml:"update-status>update-list>update"`
}

func (zypperManager) list(ctx context.Context, run cmdRunner) ([]PackageUpdate, int, error) {
	b, code, err := output(ctx, run, zypperEnv, "zypper", "-n", "--xmlout", "list-updates")
	if err != nil {
		return nil, 0, err
	}
	if code != 0 {
		return nil, 0, fmt.Errorf("zypper list-updates failed (exit %d): %s", code, tail(b, 400))
	}
	ups, err := parseZypperUpdates(b)
	if err != nil {
		return nil, 0, err
	}
	installed := rpmInstalled(ctx, run)
	for i := range ups {
		ups[i].Installed = installed[ups[i].Name+"."+ups[i].arch]
	}
	// zypper marks security on patches, not packages: the count is what
	// "Security only" installs.
	patches := 0
	if pb, pcode, perr := output(ctx, run, zypperEnv, "zypper", "-n", "--xmlout",
		"list-patches", "--category", "security"); perr == nil && pcode == 0 {
		patches = countZypperPatches(pb)
	}
	return ups, patches, nil
}

func parseZypperUpdates(b []byte) ([]PackageUpdate, error) {
	var s zypperStream
	if err := xml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("cannot read zypper's answer: %w", err)
	}
	var out []PackageUpdate
	for _, u := range s.Updates {
		if u.Kind != "" && u.Kind != "package" {
			continue
		}
		out = append(out, PackageUpdate{Name: u.Name, Available: u.Edition, Origin: u.Source.Alias,
			Reboot: kernelPackage(u.Name), arch: u.Arch})
	}
	return out, nil
}

func countZypperPatches(b []byte) int {
	var s zypperStream
	if xml.Unmarshal(b, &s) != nil {
		return 0
	}
	n := 0
	for _, u := range s.Updates {
		if u.Kind == "patch" && (u.Status == "" || u.Status == "needed") {
			n++
		}
	}
	return n
}

func (zypperManager) install(ctx context.Context, run cmdRunner, log io.Writer, names []string, all bool,
	holds []string) error {
	// A hold outlives the run in zypper too (addlock), so a held package is
	// left out by naming what to upgrade, as for apt.
	if all {
		ups, _, err := zypperManager{}.list(ctx, run)
		if err != nil {
			return err
		}
		names = names[:0]
		for _, u := range ups {
			if !held(u.Name, holds) {
				names = append(names, u.Name)
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"-n", "update", "--no-confirm", "--auto-agree-with-licenses"}, names...)
	var b bytes.Buffer
	code, err := run(ctx, io.MultiWriter(&b, log), zypperEnv, "zypper", args...)
	if err != nil {
		return err
	}
	// 100-106 are zypper's "done, but": a reboot or restart is wanted.
	if code != 0 && (code < 100 || code > 106) {
		return fmt.Errorf("zypper update failed (exit %d): %s", code, tail(b.Bytes(), 400))
	}
	return nil
}

// installSecurityPatches is zypper's security-only install: patches, which
// carry the category, rather than packages.
func (zypperManager) installSecurityPatches(ctx context.Context, run cmdRunner, log io.Writer) error {
	var b bytes.Buffer
	code, err := run(ctx, io.MultiWriter(&b, log), zypperEnv, "zypper", "-n", "patch",
		"--category", "security", "--auto-agree-with-licenses")
	if err != nil {
		return err
	}
	if code != 0 && (code < 100 || code > 106) {
		return fmt.Errorf("zypper patch failed (exit %d): %s", code, tail(b.Bytes(), 400))
	}
	return nil
}

func (zypperManager) installed(ctx context.Context, run cmdRunner, names []string) map[string]string {
	all := rpmInstalled(ctx, run)
	out := map[string]string{}
	for na, v := range all {
		base, _, _ := strings.Cut(na, ".")
		for _, n := range names {
			if n == base {
				out[n] = v
			}
		}
	}
	return out
}

func (zypperManager) rebootWanted(ctx context.Context, run cmdRunner) bool {
	// needs-rebooting: exit 102 when a reboot is wanted.
	_, code, err := output(ctx, run, zypperEnv, "zypper", "needs-rebooting")
	return err == nil && code == 102
}

func (zypperManager) staleServices(context.Context, cmdRunner) []string { return nil }

func (zypperManager) repair(ctx context.Context, run cmdRunner, log io.Writer) error {
	_, _ = run(ctx, log, nil, "rpm", "--rebuilddb")
	return nil
}

/* ── shared ──────────────────────────────────────────────────────────────── */

// reKernel names the packages whose upgrade means a reboot before it runs.
var reKernel = regexp.MustCompile(`^(linux-image|linux-modules|linux-generic|linux-signed|linux-headers-generic|` +
	`kernel|kernel-core|kernel-modules|kernel-default|kernel-uek)($|[-.])`)

func kernelPackage(name string) bool { return reKernel.MatchString(name) }

// held reports whether name matches one of the hold globs. A name carrying
// an architecture (openssl.x86_64) matches on the name alone too.
func held(name string, holds []string) bool {
	base, _, _ := strings.Cut(name, ".")
	for _, h := range holds {
		if ok, _ := path.Match(h, name); ok {
			return true
		}
		if ok, _ := path.Match(h, base); ok {
			return true
		}
	}
	return false
}

// protectedService is a service the helper never restarts on its own: the
// agent (which a restart would cut off mid-report), argus-control and its
// helper on the control plane's own server (the same, and the console with
// it), and what people's sessions run in.
func protectedService(svc string) bool {
	for _, p := range []string{"argus-agent", "argus-control", "user@", "getty@", "serial-getty@", "systemd-", "dbus",
		"display-manager", "gdm", "lightdm", "sddm", "network", "NetworkManager", "wpa_supplicant"} {
		if strings.HasPrefix(svc, p) {
			return true
		}
	}
	return false
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

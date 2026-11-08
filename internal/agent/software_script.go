package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Install scripts: the run.

A script is what someone would otherwise paste into a root shell over SSH.
Argus runs it through the agent instead, so nobody needs a key, and every run
is reviewed, approved and recorded. The helper (packages-apply, already root
and outside the agent's sandbox) does it:

 1. Checks the approval's signature against the control plane key this agent
    pinned (grant-key.pub in its state directory), and the text it was sent
    against the hashes in it. A script nobody reviewed, or that differs by a
    byte from what was reviewed, never runs.
 2. Takes a "before" picture: the package database, systemd services,
    listening ports, and the folders in /opt and /usr/local.
 3. Runs the script in a transient unit of its own: bash -euo pipefail (sh
    -eu where bash is missing), stdin closed so an installer that waits for
    a key fails at once, a hard timeout, the parameters in an environment
    file only root can read, and the agent's state directory hidden and its
    binary and unit read-only, so even a root script cannot quietly change
    Argus.
 4. Runs the version's check script, if it has one, and takes the "after"
    picture. The difference is the report: Argus cannot know in advance what
    a script will do, so it says what it did.

Secret parameter values are masked in the log before it leaves the server.
*/

// ScriptOrder is a reviewed script, as a run carries it.
type ScriptOrder struct {
	// Action is install or uninstall: which part runs.
	Action string `json:"action"`
	// Text is that part; Check is the version's check script, if any.
	Text  string `json:"text"`
	Check string `json:"check,omitempty"`
	// Approval is the reviewer's signed approval (tunnel.ScriptApproval).
	Approval string `json:"approval"`
	// Params are parameter values by name; Secret names the ones masked.
	Params map[string]string `json:"params,omitempty"`
	Secret []string          `json:"secret,omitempty"`
	// TimeoutSec bounds the run; the approval's is the upper limit.
	TimeoutSec int `json:"timeout_sec"`
}

func (o *ScriptOrder) validate() error {
	switch {
	case o == nil:
		return errors.New("the run carries no script")
	case o.Action != tunnel.ScriptInstall && o.Action != tunnel.ScriptUninstall:
		return fmt.Errorf("%q is not a script action", o.Action)
	case o.Text == "" || len(o.Text) > tunnel.MaxScriptBytes || len(o.Check) > tunnel.MaxScriptBytes:
		return errors.New("the script is empty or too long")
	case o.Approval == "":
		return errors.New("the script carries no approval")
	case len(o.Params) > tunnel.MaxScriptParams:
		return errors.New("the script has too many parameters")
	}
	for k, v := range o.Params {
		if !tunnel.ValidScriptParam(k) {
			return fmt.Errorf("%q is not a parameter name", k)
		}
		if err := tunnel.ValidateScriptValue(k, v); err != nil {
			return err
		}
	}
	return nil
}

func (o *ScriptOrder) timeout() time.Duration {
	t := time.Duration(o.TimeoutSec) * time.Second
	if t <= 0 || t > tunnel.MaxScriptTimeout {
		t = 30 * time.Minute
	}
	return t
}

// Where scripts are written and where the report looks for new folders.
// Tests move them.
var (
	scriptRunDir     = "/run"
	scriptFolders    = []string{"/opt", "/usr/local"}
	scriptCheckLimit = 2 * time.Minute
)

func (a *PackageApplier) applyScript(ctx context.Context) PackageRunStatus {
	o := a.Plan.Script
	if err := o.validate(); err != nil {
		return a.fail(err.Error())
	}
	// Mask secrets in everything the log sends.
	var secrets []string
	for _, n := range o.Secret {
		if v := o.Params[n]; len(v) >= 3 {
			secrets = append(secrets, v)
		}
	}
	mask := func(s string) string {
		for _, v := range secrets {
			s = strings.ReplaceAll(s, v, "••••••")
		}
		return s
	}
	a.lw.flush = func(s string) { a.flushLog(mask(s)) }

	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "verify", "checking the script is the one reviewed" })
	appr, err := a.verifyScript(o)
	if err != nil {
		return a.fail(err.Error() + " Nothing was run.")
	}
	timeout := o.timeout()
	if max := time.Duration(appr.TimeoutSec) * time.Second; max > 0 && timeout > max {
		timeout = max
	}

	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "before", "noting what is on the server before" })
	m := asSW(detectManager(a.Look))
	before := a.picture(ctx, m)

	dir := filepath.Join(scriptRunDir, "argus-script-"+a.Plan.ID)
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return a.fail("cannot prepare the script: " + err.Error())
	}
	defer os.RemoveAll(dir)
	verb := map[string]string{tunnel.ScriptInstall: "installing", tunnel.ScriptUninstall: "uninstalling"}[o.Action]
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "script", fmt.Sprintf("%s with %s v%d", verb, scriptName(appr), appr.Version)
	})
	code, err := a.runScriptUnit(ctx, dir, "run", o.Text, o.Params, appr.RunAs, timeout, a.lw)
	a.lw.Flush()
	report := &tunnel.ScriptReport{Exit: code}

	if o.Check != "" {
		a.update(func(s *PackageRunStatus) { s.Step, s.Message = "check", "running the check script" })
		var out bytes.Buffer
		cc, cerr := a.runScriptUnit(ctx, dir, "check", o.Check, o.Params, appr.RunAs, scriptCheckLimit, &out)
		report.Checked = true
		switch {
		case cerr != nil:
			report.CheckError = cerr.Error()
		case cc == 0:
			report.Installed = true
			report.CheckVersion = scriptFirstLine(mask(out.String()), 100)
		}
	}
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "after", "noting what changed" })
	after := a.picture(ctx, m)
	before.diff(after, report)
	if m != nil {
		a.writeReport(ctx, m)
	}
	changed := []PackageChange{}
	for _, c := range report.Packages {
		changed = append(changed, PackageChange{Name: c.Name, From: c.From, To: c.To})
	}
	_, reboot := os.Stat(rebootRequiredFile)
	more := func(s *PackageRunStatus) {
		s.Script, s.Upgraded, s.RebootRequired = report, changed, reboot == nil
	}
	switch {
	case err != nil:
		return a.finish(RunFailed, "the script could not be started: "+err.Error(), more)
	case code != 0:
		return a.finish(RunFailed, fmt.Sprintf("the script ended with exit code %d", code), more)
	case report.Checked && o.Action == tunnel.ScriptInstall && !report.Installed:
		return a.finish(RunFailed, "the script finished, but its check says it is not installed", more)
	case report.Checked && o.Action == tunnel.ScriptUninstall && report.Installed:
		return a.finish(RunFailed, "the script finished, but its check says it is still installed", more)
	}
	return a.finish(RunDone, fmt.Sprintf("%s v%d finished: %s", scriptName(appr), appr.Version, scriptSummary(report)), more)
}

func scriptName(a tunnel.ScriptApproval) string {
	if a.Name != "" {
		return a.Name
	}
	return "the script"
}

// verifyScript checks the approval against the pinned key and the texts
// against it.
func (a *PackageApplier) verifyScript(o *ScriptOrder) (tunnel.ScriptApproval, error) {
	b, err := os.ReadFile(filepath.Join(a.Plan.StateDir, GrantKeyFile))
	if err != nil {
		return tunnel.ScriptApproval{}, errors.New("this agent has not pinned the control plane's key yet, " +
			"so it cannot check the script; it does once its tunnel connects.")
	}
	key, err := tunnel.DecodeGrantKey(string(b))
	if err != nil {
		return tunnel.ScriptApproval{}, errors.New("this agent's pinned control plane key cannot be read.")
	}
	appr, err := tunnel.VerifyScript(key, o.Approval)
	if err != nil {
		return appr, errors.New(strings.TrimSuffix(err.Error(), ".") + ".")
	}
	want, os := tunnel.ScriptLinux, "Linux"
	if a.Windows {
		want, os = tunnel.ScriptWindows, "Windows"
	}
	if appr.Platform != want {
		return appr, errors.New("this script is for " + appr.Platform + ", not " + os + ".")
	}
	want, ok := appr.SHAFor(o.Action)
	if !ok {
		return appr, fmt.Errorf("the reviewed version has no %s script.", o.Action)
	}
	if tunnel.ScriptHash(o.Text) != want {
		return appr, errors.New("the script sent is not the one reviewed.")
	}
	if o.Check != "" {
		if want, ok := appr.SHAFor(tunnel.ScriptCheck); !ok || tunnel.ScriptHash(o.Check) != want {
			return appr, errors.New("the check script sent is not the one reviewed.")
		}
	}
	allowed := map[string]bool{}
	for _, p := range appr.Params {
		allowed[p] = true
	}
	for k := range o.Params {
		if !allowed[k] {
			return appr, fmt.Errorf("the run sets %s, which the reviewed version does not take.", k)
		}
	}
	if !tunnel.ValidRunAs(appr.RunAs) {
		return appr, errors.New("the reviewed version names an account that cannot be one.")
	}
	return appr, nil
}

// runScriptUnit runs text in a transient unit and returns its exit code.
func (a *PackageApplier) runScriptUnit(ctx context.Context, dir, name, text string, params map[string]string,
	runAs string, timeout time.Duration, out interface{ Write([]byte) (int, error) }) (int, error) {
	file := filepath.Join(dir, name+".sh")
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		return -1, err
	}
	var env strings.Builder
	for k, v := range params {
		// systemd's EnvironmentFile: double quotes, backslash escapes.
		v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`).Replace(v)
		fmt.Fprintf(&env, "%s=\"%s\"\n", tunnel.ScriptEnv(k), v)
	}
	envFile := filepath.Join(dir, name+".env")
	if err := os.WriteFile(envFile, []byte(env.String()), 0o600); err != nil {
		return -1, err
	}
	shell := []string{"/bin/sh", "-eu"}
	if p, err := a.Look("bash"); err == nil {
		shell = []string{p, "-euo", "pipefail"}
	}
	exe, _ := os.Executable()
	args := []string{"--pipe", "--wait", "--quiet", "--collect", "--service-type=exec",
		"--unit=argus-script-" + name + "-" + shortID(a.Plan.ID),
		"-p", fmt.Sprintf("RuntimeMaxSec=%d", int(timeout.Seconds())),
		"-p", "StandardInput=null",
		"-p", "EnvironmentFile=" + envFile,
		"-p", "InaccessiblePaths=-" + a.Plan.StateDir,
		"-p", "ReadOnlyPaths=-/etc/systemd/system/argus-agent.service",
	}
	if exe != "" {
		args = append(args, "-p", "ReadOnlyPaths=-"+exe)
	}
	if runAs != "" {
		args = append(args, "-p", "User="+runAs, "--working-directory=/tmp")
	} else {
		args = append(args, "--working-directory=/root")
	}
	args = append(args, "--setenv=ARGUS_RUN_ID="+a.Plan.ID)
	args = append(args, shell...)
	args = append(args, file)
	rctx, cancel := context.WithTimeout(ctx, timeout+time.Minute)
	defer cancel()
	return a.Run(rctx, out, nil, "systemd-run", args...)
}

func shortID(id string) string {
	s := strings.ReplaceAll(id, "-", "")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

func scriptFirstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max]
	}
	return strings.TrimSpace(s)
}

/* ── Before and after ─────────────────────────────────────────────────────── */

type serverPicture struct {
	packages map[string]tunnel.InstalledPackage
	units    map[string]bool
	ports    map[int]bool
	folders  map[string]bool
}

func (a *PackageApplier) picture(ctx context.Context, m swManager) serverPicture {
	p := serverPicture{packages: map[string]tunnel.InstalledPackage{}, units: map[string]bool{},
		ports: map[int]bool{}, folders: map[string]bool{}}
	if m != nil {
		if list, err := m.installedList(ctx, a.Run); err == nil {
			for _, i := range list {
				p.packages[i.Name] = i
			}
		}
	}
	if out, code, err := output(ctx, a.Run, nil, "systemctl", "list-unit-files", "--type=service",
		"--no-legend", "--no-pager"); err == nil && code == 0 {
		for _, u := range parseUnitFiles(out) {
			p.units[u] = true
		}
	}
	if out, code, err := output(ctx, a.Run, nil, "ss", "-Htln"); err == nil && code == 0 {
		for _, port := range parseListenPorts(out) {
			p.ports[port] = true
		}
	}
	for _, d := range scriptFolders {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				p.folders[filepath.Join(d, e.Name())] = true
			}
		}
	}
	return p
}

func (b serverPicture) diff(a serverPicture, r *tunnel.ScriptReport) {
	for name, after := range a.packages {
		before, had := b.packages[name]
		switch {
		case !had:
			r.Packages = append(r.Packages, tunnel.SoftwareChange{Kind: tunnel.ChangeAdd, Name: name, To: after.Version})
		case before.Version != after.Version:
			kind := tunnel.ChangeUpgrade
			if compareVersions(after.Version, before.Version) < 0 {
				kind = tunnel.ChangeDowngrade
			}
			r.Packages = append(r.Packages, tunnel.SoftwareChange{Kind: kind, Name: name, From: before.Version, To: after.Version})
		}
	}
	for name, before := range b.packages {
		if _, ok := a.packages[name]; !ok {
			r.Packages = append(r.Packages, tunnel.SoftwareChange{Kind: tunnel.ChangeRemove, Name: name, From: before.Version})
		}
	}
	sort.Slice(r.Packages, func(i, j int) bool { return r.Packages[i].Name < r.Packages[j].Name })
	r.ServicesAdded, r.ServicesRemoved = setDiff(a.units, b.units), setDiff(b.units, a.units)
	r.FoldersAdded, r.FoldersRemoved = setDiff(a.folders, b.folders), setDiff(b.folders, a.folders)
	r.PortsOpened, r.PortsClosed = intDiff(a.ports, b.ports), intDiff(b.ports, a.ports)
}

func setDiff(x, y map[string]bool) []string {
	var out []string
	for k := range x {
		if !y[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func intDiff(x, y map[int]bool) []int {
	var out []int
	for k := range x {
		if !y[k] {
			out = append(out, k)
		}
	}
	sort.Ints(out)
	return out
}

// scriptSummary is a report in a few words, for the run's message.
func scriptSummary(r *tunnel.ScriptReport) string {
	var parts []string
	add, rm := 0, 0
	for _, c := range r.Packages {
		switch c.Kind {
		case tunnel.ChangeAdd:
			add++
		case tunnel.ChangeRemove:
			rm++
		}
	}
	if add > 0 {
		parts = append(parts, fmt.Sprintf("%d package%s added", add, plural(add)))
	}
	if rm > 0 {
		parts = append(parts, fmt.Sprintf("%d package%s removed", rm, plural(rm)))
	}
	if n := len(r.ServicesAdded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d service%s added", n, plural(n)))
	}
	if n := len(r.PortsOpened); n > 0 {
		parts = append(parts, fmt.Sprintf("%d new listening port%s", n, plural(n)))
	}
	if n := len(r.FoldersAdded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d folder%s added", n, plural(n)))
	}
	if r.Checked && r.Installed {
		v := ""
		if r.CheckVersion != "" {
			v = " (" + r.CheckVersion + ")"
		}
		parts = append(parts, "the check says it is installed"+v)
	}
	if len(parts) == 0 {
		return "nothing Argus watches changed"
	}
	return strings.Join(parts, ", ")
}

// parseUnitFiles reads `systemctl list-unit-files --type=service --no-legend`.
func parseUnitFiles(out []byte) []string {
	var units []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) > 0 && strings.HasSuffix(f[0], ".service") && !strings.Contains(f[0], "@") {
			units = append(units, f[0])
		}
	}
	return units
}

var reListen = regexp.MustCompile(`:(\d+)$`)

// parseListenPorts reads `ss -Htln`: State Recv-Q Send-Q Local Peer.
func parseListenPorts(out []byte) []int {
	seen := map[int]bool{}
	var ports []int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		m := reListen.FindStringSubmatch(f[3])
		if m == nil {
			continue
		}
		if p, err := strconv.Atoi(m[1]); err == nil && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports
}

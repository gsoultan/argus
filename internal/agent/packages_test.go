package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* ── Parsers ─────────────────────────────────────────────────────────────── */

const aptSimulate = `NOTE: This is only a simulation!
Reading package lists...
Building dependency tree...
The following NEW packages will be installed:
  linux-image-6.8.0-48-generic
The following packages will be upgraded:
  curl linux-image-generic openssl
Inst linux-image-6.8.0-48-generic (6.8.0-48.48 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst openssl [3.0.13-0ubuntu3.4] (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst curl [8.5.0-2ubuntu10.4] (8.5.0-2ubuntu10.5 Ubuntu:24.04/noble-updates [amd64]) []
Inst linux-image-generic [6.8.0-45.45] (6.8.0-48.48 Ubuntu:24.04/noble-updates [amd64])
Inst tzdata [2024a-1] (2024b-0+deb12u1 Debian-Security:12/stable-security [all])
Conf openssl (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates [amd64])
`

func TestParseAptSimulate(t *testing.T) {
	ups := parseAptSimulate([]byte(aptSimulate))
	if len(ups) != 5 {
		t.Fatalf("got %d updates, want 5: %+v", len(ups), ups)
	}
	by := map[string]PackageUpdate{}
	for _, u := range ups {
		by[u.Name] = u
	}
	if u := by["openssl"]; u.Installed != "3.0.13-0ubuntu3.4" || u.Available != "3.0.13-0ubuntu3.5" ||
		!u.Security || u.Origin != "Ubuntu:24.04/noble-security" {
		t.Errorf("openssl = %+v", u)
	}
	if u := by["curl"]; u.Security || u.Installed == "" || u.Origin != "Ubuntu:24.04/noble-updates" {
		t.Errorf("curl should be a plain bug fix: %+v", u)
	}
	if u := by["linux-image-6.8.0-48-generic"]; !u.New || !u.Reboot || !u.Security {
		t.Errorf("new kernel = %+v", u)
	}
	if u := by["linux-image-generic"]; !u.Reboot || u.New {
		t.Errorf("kernel meta = %+v", u)
	}
	if u := by["tzdata"]; !u.Security {
		t.Errorf("Debian security archive not recognised: %+v", u)
	}
}

func TestAptFetchFailedIsAnError(t *testing.T) {
	out := "Ign:1 https://deb.debian.org/debian bookworm InRelease\n" +
		"Err:1 https://deb.debian.org/debian bookworm InRelease\n  403  Forbidden\n" +
		"Reading package lists...\n" +
		"W: Failed to fetch https://deb.debian.org/debian/dists/bookworm/InRelease  403  Forbidden\n" +
		"W: Some index files failed to download. They have been ignored, or old ones used instead.\n"
	err := aptFetchFailed([]byte(out))
	if err == nil || !strings.Contains(err.Error(), "could not download 1 package list") {
		t.Fatalf("err = %v", err)
	}
	if aptFetchFailed([]byte("Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease\nReading package lists...\n")) != nil {
		t.Fatal("a clean refresh was taken for a failure")
	}
}

func TestParseAptSimulateNothing(t *testing.T) {
	if ups := parseAptSimulate([]byte("Reading package lists...\n0 upgraded, 0 newly installed\n")); len(ups) != 0 {
		t.Fatalf("got %+v", ups)
	}
}

func TestParseDnf(t *testing.T) {
	check := `
openssl.x86_64                1:3.0.7-27.el9_4           baseos
kernel.x86_64                 5.14.0-427.42.1.el9_4      baseos
tzdata.noarch                 2024b-2.el9                appstream
Obsoleting Packages
grub2-tools.x86_64            1:2.06-80.el9              baseos
`
	ups := parseDnfCheckUpdate([]byte(check))
	if len(ups) != 3 {
		t.Fatalf("got %+v", ups)
	}
	if ups[0].Name != "openssl.x86_64" || ups[0].Available != "1:3.0.7-27.el9_4" || ups[0].Origin != "baseos" {
		t.Errorf("first = %+v", ups[0])
	}
	sec := parseDnfSecurity([]byte(
		"RLSA-2024:1234 Important/Sec. openssl-1:3.0.7-27.el9_4.x86_64\n" +
			"RLSA-2024:9999 Moderate/Sec.  kernel-5.14.0-427.42.1.el9_4.x86_64\n" +
			"RLBA-2024:1111 bugfix         tzdata-2024b-2.el9.noarch\n"))
	if sec["openssl.x86_64"] != "RLSA-2024:1234 · important" || sec["kernel.x86_64"] == "" {
		t.Errorf("security = %v", sec)
	}
	if _, ok := sec["tzdata.noarch"]; ok {
		t.Error("a bug fix advisory was taken for a security one")
	}
}

func TestNevraNameArch(t *testing.T) {
	for in, want := range map[string]string{
		"openssl-1:3.0.7-27.el9_4.x86_64":     "openssl.x86_64",
		"python3-libs-3.9.18-3.el9.x86_64":    "python3-libs.x86_64",
		"kernel-5.14.0-427.42.1.el9_4.x86_64": "kernel.x86_64",
		"garbage":                             "",
	} {
		if got := nevraNameArch(in); got != want {
			t.Errorf("nevraNameArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseZypper(t *testing.T) {
	x := `<?xml version='1.0'?>
<stream>
<update-status version="0.6">
<update-list>
<update kind="package" name="openssl-3" edition="3.1.4-150600.5.15.1" arch="x86_64" edition-old="3.1.4-150600.5.10.1"><summary>x</summary><source url="http://x" alias="repo-sle-update"/></update>
<update kind="package" name="kernel-default" edition="6.4.0-150600.23.25.1" arch="x86_64"><source alias="repo-sle-update"/></update>
</update-list>
</update-status>
</stream>`
	ups, err := parseZypperUpdates([]byte(x))
	if err != nil || len(ups) != 2 {
		t.Fatalf("ups=%+v err=%v", ups, err)
	}
	if ups[0].Name != "openssl-3" || ups[0].Origin != "repo-sle-update" || ups[0].arch != "x86_64" {
		t.Errorf("first = %+v", ups[0])
	}
	if !ups[1].Reboot {
		t.Error("kernel-default should want a reboot")
	}
	if _, err := parseZypperUpdates([]byte("not xml <")); err == nil {
		t.Error("garbage from zypper was read as no updates")
	}
	patches := `<stream><update-status><update-list>
<update kind="patch" name="SUSE-2024-1" status="needed" category="security"/>
<update kind="patch" name="SUSE-2024-2" status="needed" category="security"/>
<update kind="patch" name="SUSE-2024-3" status="applied" category="security"/>
</update-list></update-status></stream>`
	if n := countZypperPatches([]byte(patches)); n != 2 {
		t.Errorf("security patches = %d, want 2", n)
	}
}

func TestParseDpkgAudit(t *testing.T) {
	out := "The following packages are in a mess due to serious problems during\n" +
		"installation.  They must be reinstalled for them (and any packages\n" +
		"that depend on them) to function properly:\n" +
		" libpcre2-8-0:amd64   New Perl Compatible Regular Expression Library- 8 bit run\n" +
		" apt                  commandline package manager\n\n"
	if got := strings.Join(parseDpkgAudit([]byte(out)), ","); got != "apt,libpcre2-8-0" {
		t.Fatalf("got %q", got)
	}
	if len(parseDpkgAudit(nil)) != 0 {
		t.Fatal("a clean audit named packages")
	}
}

func TestParseNeedrestart(t *testing.T) {
	got := parseNeedrestart([]byte("NEEDRESTART-VER: 3.6\nNEEDRESTART-KCUR: 6.8\nNEEDRESTART-SVC: nginx.service\nNEEDRESTART-SVC: ssh.service\n"))
	if strings.Join(got, ",") != "nginx.service,ssh.service" {
		t.Fatalf("got %v", got)
	}
}

func TestHeldAndProtected(t *testing.T) {
	holds := []string{"postgresql*", "docker-ce", "kernel?"}
	for name, want := range map[string]bool{
		"postgresql-16": true, "postgresql-16.x86_64": true, "docker-ce": true, "docker-ce-cli": false,
		"openssl": false, "kernel1": true,
	} {
		if got := held(name, holds); got != want {
			t.Errorf("held(%q) = %v, want %v", name, got, want)
		}
	}
	for svc, want := range map[string]bool{
		"argus-agent.service": true, "user@1000.service": true, "dbus.service": true,
		"nginx.service": false, "ssh.service": false, "systemd-logind.service": true,
	} {
		if got := protectedService(svc); got != want {
			t.Errorf("protectedService(%q) = %v", svc, got)
		}
	}
}

func TestDetectManager(t *testing.T) {
	has := func(tools ...string) lookPathFunc {
		return func(n string) (string, error) {
			for _, t := range tools {
				if t == n {
					return "/usr/bin/" + n, nil
				}
			}
			return "", errors.New("no")
		}
	}
	for _, c := range []struct {
		tools []string
		want  string
	}{
		{[]string{"apt-get", "dpkg-query"}, "apt"},
		{[]string{"dnf", "yum"}, "dnf"},
		{[]string{"yum"}, "yum"},
		{[]string{"zypper"}, "zypper"},
		{[]string{"apt-get"}, ""}, // apt-get without dpkg is not a Debian system
		{nil, ""},
	} {
		m := detectManager(has(c.tools...))
		got := ""
		if m != nil {
			got = m.name()
		}
		if got != c.want {
			t.Errorf("tools %v: got %q, want %q", c.tools, got, c.want)
		}
	}
}

/* ── The helper, against a fake apt ──────────────────────────────────────── */

// fakeApt answers like apt-get, dpkg-query and needrestart on a server
// whose installed versions it keeps, so an install changes what is listed.
type fakeApt struct {
	mu        sync.Mutex
	installed map[string]string
	candidate map[string]string
	security  map[string]bool
	calls     []string
	// failRefresh, failInstall and lockHeld make those steps fail.
	failRefresh, failInstall, lockHeld bool
	// mirrorsDown is apt-get update failing every fetch and exiting 0.
	mirrorsDown bool
	// partial installs only the first package before failing.
	partial  bool
	stale    []string
	rebootIf string // a package whose upgrade makes reboot-required
	reboot   bool
}

func (f *fakeApt) look(n string) (string, error) {
	switch n {
	case "apt-get", "dpkg-query":
		return "/usr/bin/" + n, nil
	}
	return "", errors.New("not here")
}

func (f *fakeApt) run(_ context.Context, out io.Writer, _ []string, name string, args ...string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if out == nil {
		out = io.Discard
	}
	joined := strings.Join(args, " ")
	switch {
	case name == "apt-get" && strings.HasSuffix(joined, " update"):
		if f.mirrorsDown {
			fmt.Fprintln(out, "W: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble/InRelease  Temporary failure resolving 'archive.ubuntu.com'")
			fmt.Fprintln(out, "W: Some index files failed to download. They have been ignored, or old ones used instead.")
			return 0, nil
		}
		if f.failRefresh {
			fmt.Fprintln(out, "E: Failed to fetch http://archive.ubuntu.com/  Temporary failure resolving")
			return 100, nil
		}
		fmt.Fprintln(out, "Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease")
		return 0, nil
	case name == "apt-get" && strings.Contains(joined, "-s "):
		for n, c := range f.candidate {
			if f.installed[n] == c {
				continue
			}
			origin := "Ubuntu:24.04/noble-updates"
			if f.security[n] {
				origin += ", Ubuntu:24.04/noble-security"
			}
			fmt.Fprintf(out, "Inst %s [%s] (%s %s [amd64])\n", n, f.installed[n], c, origin)
		}
		return 0, nil
	case name == "apt-get" && strings.Contains(joined, "install --only-upgrade"):
		if f.lockHeld {
			fmt.Fprintln(out, "E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 812 (unattended-upgr)")
			return 100, nil
		}
		i := 0
		for _, a := range args {
			if strings.HasPrefix(a, "-") || a == "install" || strings.Contains(a, "=") {
				continue
			}
			if f.partial && i > 0 {
				fmt.Fprintln(out, "dpkg: error processing package "+a+" (--configure)")
				return 100, nil
			}
			if c, ok := f.candidate[a]; ok {
				fmt.Fprintf(out, "Setting up %s (%s) ...\n", a, c)
				f.installed[a] = c
				if a == f.rebootIf {
					f.reboot = true
				}
				i++
			}
		}
		if f.failInstall {
			return 100, nil
		}
		return 0, nil
	case name == "dpkg-query":
		for _, a := range args[2:] {
			if v, ok := f.installed[a]; ok {
				fmt.Fprintf(out, "%s\t%s\n", a, v)
			}
		}
		return 0, nil
	case name == "dpkg":
		return 0, nil
	case name == "needrestart":
		for _, svc := range f.stale {
			fmt.Fprintf(out, "NEEDRESTART-SVC: %s\n", svc)
		}
		return 0, nil
	}
	return 0, nil
}

func newFakeApt() *fakeApt {
	return &fakeApt{
		installed: map[string]string{"openssl": "3.0.13-1", "curl": "8.5.0-1", "postgresql-16": "16.4-1",
			"linux-image-generic": "6.8.0-45"},
		candidate: map[string]string{"openssl": "3.0.13-2", "curl": "8.5.0-2", "postgresql-16": "16.5-1",
			"linux-image-generic": "6.8.0-48"},
		security: map[string]bool{"openssl": true, "linux-image-generic": true},
	}
}

func applier(t *testing.T, f *fakeApt, req PackageRunRequest) (*PackageApplier, string) {
	t.Helper()
	dir := t.TempDir()
	old := rebootRequiredFile
	rebootRequiredFile = filepath.Join(dir, "reboot-required")
	t.Cleanup(func() { rebootRequiredFile = old })
	a := &PackageApplier{
		Plan:           PackagePlan{PackageRunRequest: req, StateDir: dir, Unit: "argus-packages-test"},
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		PackageChecker: PackageChecker{Run: f.run, Look: f.look},
		Restart:        func(context.Context, string) error { return nil },
		Reboot:         func() error { return nil },
		BootID:         func() string { return "boot-1" },
	}
	return a, dir
}

func readRunFile(t *testing.T, dir string) PackageRunStatus {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, packagesRunFile))
	if err != nil {
		t.Fatal(err)
	}
	var st PackageRunStatus
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestApplySecurityOnly(t *testing.T) {
	f := newFakeApt()
	a, dir := applier(t, f, PackageRunRequest{ID: "run-security-1", Mode: PackagesSecurity, Reboot: "never"})
	st := a.Apply(context.Background())
	if st.State != RunDone {
		t.Fatalf("state = %s (%s)", st.State, st.Message)
	}
	if f.installed["openssl"] != "3.0.13-2" || f.installed["curl"] != "8.5.0-1" {
		t.Errorf("security only touched the wrong packages: %v", f.installed)
	}
	if len(st.Upgraded) != 2 {
		t.Errorf("upgraded = %+v, want openssl and the kernel", st.Upgraded)
	}
	if !st.RebootRequired {
		t.Error("a kernel was upgraded and the run does not say the server needs a reboot")
	}
	if disk := readRunFile(t, dir); disk.State != RunDone || disk.ID != "run-security-1" {
		t.Errorf("state file = %+v", disk)
	}
	// The report written afterwards no longer lists what was installed.
	b, err := os.ReadFile(filepath.Join(dir, packagesReportFile))
	if err != nil {
		t.Fatal(err)
	}
	var r PackageReport
	_ = json.Unmarshal(b, &r)
	for _, u := range r.Updates {
		if u.Name == "openssl" {
			t.Error("the report after the run still lists openssl as waiting")
		}
	}
	if len(r.Updates) != 2 {
		t.Errorf("still waiting = %+v, want curl and postgresql-16", r.Updates)
	}
}

func TestApplyAllKeepsHolds(t *testing.T) {
	f := newFakeApt()
	a, _ := applier(t, f, PackageRunRequest{ID: "run-all-0001", Mode: PackagesAll, Holds: []string{"postgresql*"}})
	st := a.Apply(context.Background())
	if st.State != RunDone {
		t.Fatalf("state = %s (%s)", st.State, st.Message)
	}
	if f.installed["postgresql-16"] != "16.4-1" {
		t.Error("a held package was upgraded")
	}
	if f.installed["curl"] != "8.5.0-2" {
		t.Error("an unheld package was not upgraded")
	}
	if st.Planned != 3 {
		t.Errorf("planned = %d, want 3", st.Planned)
	}
}

func TestApplySelectedSkipsGoneAndHeld(t *testing.T) {
	f := newFakeApt()
	f.installed["curl"] = f.candidate["curl"] // upgraded by someone since the console looked
	a, _ := applier(t, f, PackageRunRequest{ID: "run-selected-1", Mode: PackagesSelected,
		Packages: []string{"curl", "openssl", "postgresql-16"}, Holds: []string{"postgresql*"}})
	st := a.Apply(context.Background())
	if st.State != RunDone || len(st.Upgraded) != 1 || st.Upgraded[0].Name != "openssl" {
		t.Fatalf("got %+v", st)
	}
}

func TestApplyNothingToDo(t *testing.T) {
	f := newFakeApt()
	for n, c := range f.candidate {
		f.installed[n] = c
	}
	a, _ := applier(t, f, PackageRunRequest{ID: "run-nothing-1", Mode: PackagesAll})
	st := a.Apply(context.Background())
	if st.State != RunDone || !strings.Contains(st.Message, "nothing to install") {
		t.Fatalf("got %+v", st)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "--only-upgrade") {
			t.Error("an install ran with nothing to install")
		}
	}
}

func TestApplyRefreshFails(t *testing.T) {
	f := newFakeApt()
	f.failRefresh = true
	a, _ := applier(t, f, PackageRunRequest{ID: "run-offline-1", Mode: PackagesAll})
	st := a.Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "apt-get update failed") {
		t.Fatalf("got %+v", st)
	}
	if f.installed["openssl"] != "3.0.13-1" {
		t.Error("something was installed after the lists could not be refreshed")
	}
	if !strings.Contains(st.Log, "Temporary failure resolving") {
		t.Errorf("the log does not carry apt's reason: %q", st.Log)
	}
}

func TestApplyMirrorsUnreachable(t *testing.T) {
	f := newFakeApt()
	f.mirrorsDown = true
	a, _ := applier(t, f, PackageRunRequest{ID: "run-mirrors-1", Mode: PackagesAll})
	st := a.Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "could not download") {
		t.Fatalf("got %+v", st)
	}
	r, err := PackageChecker{Run: f.run, Look: f.look}.Check(context.Background())
	if err == nil {
		t.Fatalf("a check with every mirror unreachable reported %d updates and no error", len(r.Updates))
	}
}

func TestApplyLockHeld(t *testing.T) {
	f := newFakeApt()
	f.lockHeld = true
	a, _ := applier(t, f, PackageRunRequest{ID: "run-locked-1", Mode: PackagesAll})
	st := a.Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "Could not get lock") {
		t.Fatalf("got %+v", st)
	}
	if len(st.Upgraded) != 0 {
		t.Errorf("a locked install reported upgrades: %+v", st.Upgraded)
	}
	waited := false
	for _, c := range f.calls {
		if strings.Contains(c, "DPkg::Lock::Timeout=120") && strings.Contains(c, "install") {
			waited = true
		}
	}
	if !waited {
		t.Error("the install does not wait for another apt to let go of the lock")
	}
}

func TestApplyPartialFailureSaysWhatChanged(t *testing.T) {
	f := newFakeApt()
	f.partial = true
	a, _ := applier(t, f, PackageRunRequest{ID: "run-partial-1", Mode: PackagesSelected,
		Packages: []string{"curl", "openssl"}})
	st := a.Apply(context.Background())
	if st.State != RunFailed {
		t.Fatalf("got %+v", st)
	}
	if len(st.Upgraded) != 1 {
		t.Errorf("a half-done install should say what did change: %+v", st.Upgraded)
	}
}

func TestApplyRebootsOnlyWhenAsked(t *testing.T) {
	for _, policy := range []string{"never", "if_needed"} {
		f := newFakeApt()
		rebooted := false
		a, dir := applier(t, f, PackageRunRequest{ID: "run-reboot-" + policy, Mode: PackagesAll, Reboot: policy})
		a.Reboot = func() error { rebooted = true; return nil }
		if err := os.WriteFile(rebootRequiredFile, []byte("*** System restart required ***\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		st := a.Apply(context.Background())
		_ = dir
		switch policy {
		case "never":
			if rebooted || st.State != RunDone || !st.RebootRequired {
				t.Errorf("never: rebooted=%v state=%+v", rebooted, st)
			}
		case "if_needed":
			if !rebooted || st.State != RunRebooting {
				t.Errorf("if_needed: rebooted=%v state=%+v", rebooted, st)
			}
		}
	}
}

func TestApplyRestartsStaleServicesButNotProtected(t *testing.T) {
	f := newFakeApt()
	f.stale = []string{"nginx.service", "argus-agent.service", "user@1000.service", "broken.service"}
	a, _ := applier(t, f, PackageRunRequest{ID: "run-services-1", Mode: PackagesAll})
	var got []string
	a.Restart = func(_ context.Context, svc string) error {
		got = append(got, svc)
		if svc == "broken.service" {
			return errors.New("exit 1")
		}
		return nil
	}
	st := a.Apply(context.Background())
	if strings.Join(got, ",") != "broken.service,nginx.service" {
		t.Fatalf("restarted %v: the agent and session services must be left alone", got)
	}
	if st.State != RunDone || strings.Join(st.Restarted, ",") != "nginx.service" {
		t.Fatalf("run = %+v", st)
	}
	if !strings.Contains(st.Log, "could not restart broken.service") ||
		!strings.Contains(st.Log, "argus-agent.service") {
		t.Errorf("log does not say what was left: %q", st.Log)
	}
}

/* ── The daemon's half ───────────────────────────────────────────────────── */

type fakePlatform struct {
	mu        sync.Mutex
	reason    string
	report    PackageReport
	checkErr  error
	checks    int
	launched  []string
	launchErr error
	isActive  bool
	repairs   int
	repairErr error
	boot      string
}

func (f *fakePlatform) supported() string { return f.reason }
func (f *fakePlatform) check(context.Context) (PackageReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.report, f.checkErr
}
func (f *fakePlatform) launch(_ context.Context, plan, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = append(f.launched, unit)
	return f.launchErr
}
func (f *fakePlatform) active(string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.isActive }
func (f *fakePlatform) repair(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repairs++
	return f.repairErr
}
func (f *fakePlatform) bootID() string { f.mu.Lock(); defer f.mu.Unlock(); return f.boot }

func daemon(t *testing.T, fp *fakePlatform) (*Packages, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	p := &Packages{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		platform: fp, now: func() time.Time { return clock }}
	return p, &clock
}

func TestDaemonStartsARunOnce(t *testing.T) {
	fp := &fakePlatform{boot: "b1", isActive: true}
	p, _ := daemon(t, fp)
	req := &PackageRunRequest{ID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", Mode: PackagesSecurity}
	p.Handle(context.Background(), &PackageOrders{Check: true, Run: req})
	p.Handle(context.Background(), &PackageOrders{Check: true, Run: req})
	if len(fp.launched) != 1 {
		t.Fatalf("launched %d times for one request", len(fp.launched))
	}
	b := p.Beat()
	if b.Run == nil || b.Run.State != RunStarting || b.Run.Unit != "argus-packages-0f1e2d3c4b5a" {
		t.Fatalf("beat = %+v", b.Run)
	}
	// The plan is on disk for the helper, readable by root only.
	fi, err := os.Stat(filepath.Join(p.StateDir, packagesDir, "plan-"+req.ID+".json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("plan: %v %v", fi, err)
	}
}

func TestDaemonRefusesBadRequests(t *testing.T) {
	for name, req := range map[string]PackageRunRequest{
		"mode":    {ID: "aaaaaaaa-1", Mode: "everything"},
		"empty":   {ID: "aaaaaaaa-2", Mode: PackagesSelected},
		"name":    {ID: "aaaaaaaa-3", Mode: PackagesSelected, Packages: []string{"openssl; rm -rf /"}},
		"option":  {ID: "aaaaaaaa-4", Mode: PackagesSelected, Packages: []string{"--allow-downgrades"}},
		"hold":    {ID: "aaaaaaaa-5", Mode: PackagesAll, Holds: []string{"-x"}},
		"default": {ID: "aaaaaaaa-6", Mode: ""},
	} {
		fp := &fakePlatform{}
		p, _ := daemon(t, fp)
		p.Handle(context.Background(), &PackageOrders{Run: &req})
		if len(fp.launched) != 0 {
			t.Errorf("%s: launched a run that should be refused", name)
		}
		if st := p.Beat().Run; st == nil || st.State != RunFailed {
			t.Errorf("%s: run = %+v, want failed", name, st)
		}
	}
	// A malformed id is not even answered: it cannot be reported back.
	fp := &fakePlatform{}
	p, _ := daemon(t, fp)
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "../../x", Mode: PackagesAll}})
	if len(fp.launched) != 0 || p.Beat().Run != nil {
		t.Error("a run with a malformed id was acted on")
	}
}

func TestDaemonUnsupportedServerFailsTheRun(t *testing.T) {
	fp := &fakePlatform{reason: "the agent is not running as root, so it cannot install packages"}
	p, _ := daemon(t, fp)
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "bbbbbbbb-1", Mode: PackagesAll}})
	st := p.Beat()
	if st.Supported || st.Run == nil || st.Run.State != RunFailed || !strings.Contains(st.Run.Message, "root") {
		t.Fatalf("beat = %+v", st)
	}
	if len(fp.launched) != 0 {
		t.Error("launched on a server that cannot be updated")
	}
}

func TestDaemonLaunchFailure(t *testing.T) {
	fp := &fakePlatform{launchErr: errors.New("systemd-run: Failed to start transient service unit")}
	p, _ := daemon(t, fp)
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "cccccccc-1", Mode: PackagesAll}})
	if st := p.Beat().Run; st == nil || st.State != RunFailed || !strings.Contains(st.Message, "cannot start") {
		t.Fatalf("run = %+v", st)
	}
}

func TestDaemonOneRunAtATime(t *testing.T) {
	fp := &fakePlatform{isActive: true}
	p, _ := daemon(t, fp)
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "dddddddd-1", Mode: PackagesAll}})
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "dddddddd-2", Mode: PackagesAll}})
	if len(fp.launched) != 1 {
		t.Fatalf("launched %d", len(fp.launched))
	}
	// The second is not failed in place of the first: the agent still
	// follows, and reports, the run that is going.
	if st := p.Beat().Run; st == nil || st.ID != "dddddddd-1" {
		t.Fatalf("run = %+v", st)
	}
}

// The helper is gone and the run never said it finished, on the same boot:
// killed, or the unit stopped. The run is interrupted, the package manager
// repaired, and a fresh check asked for.
func TestDaemonSettlesARunWhoseHelperDied(t *testing.T) {
	fp := &fakePlatform{boot: "b1", isActive: true}
	p, clock := daemon(t, fp)
	p.Handle(context.Background(), &PackageOrders{Check: true, Run: &PackageRunRequest{ID: "eeeeeeee-1", Mode: PackagesAll}})
	writeFileAtomicT(t, filepath.Join(p.StateDir, packagesRunFile), PackageRunStatus{ID: "eeeeeeee-1",
		State: RunRunning, Step: "install", Unit: "argus-packages-eeeeeeee1", BootID: "b1",
		StartedAt: *clock, At: *clock})
	*clock = clock.Add(time.Minute)
	fp.isActive = false
	p.tick(context.Background())
	st := p.Beat().Run
	if st == nil || st.State != RunInterrupted || fp.repairs != 1 {
		t.Fatalf("run = %+v repairs = %d", st, fp.repairs)
	}
	if !strings.Contains(st.Message, "before it finished") {
		t.Errorf("message = %q", st.Message)
	}
	if fp.checks != 1 {
		t.Errorf("checks = %d: an interrupted run should be followed by a fresh look", fp.checks)
	}
	// And it is settled once: another tick changes nothing.
	p.tick(context.Background())
	if fp.repairs != 1 {
		t.Error("repaired twice")
	}
}

// The server lost power mid-install: a new boot, a run still "running".
func TestDaemonSettlesARunCutByARestart(t *testing.T) {
	fp := &fakePlatform{boot: "b2"}
	p, clock := daemon(t, fp)
	writeFileAtomicT(t, filepath.Join(p.StateDir, packagesRunFile), PackageRunStatus{ID: "ffffffff-1",
		State: RunRunning, Unit: "argus-packages-ffffffff1", BootID: "b1", StartedAt: clock.Add(-time.Hour)})
	p.tick(context.Background())
	st := p.Beat().Run
	if st == nil || st.State != RunInterrupted || !strings.Contains(st.Message, "restarted while it was updating") {
		t.Fatalf("run = %+v", st)
	}
	if fp.repairs != 1 {
		t.Error("no repair after power loss")
	}
}

func TestDaemonRepairFailureSaysWhatToDo(t *testing.T) {
	fp := &fakePlatform{boot: "b2", repairErr: errors.New("dpkg: status database area is locked")}
	p, clock := daemon(t, fp)
	writeFileAtomicT(t, filepath.Join(p.StateDir, packagesRunFile), PackageRunStatus{ID: "abababab-1",
		State: RunRunning, Unit: "u", BootID: "b1", StartedAt: clock.Add(-time.Hour)})
	p.tick(context.Background())
	if st := p.Beat().Run; st == nil || !strings.Contains(st.Message, "dpkg --configure -a") {
		t.Fatalf("run = %+v", st)
	}
}

// The helper asked for the reboot itself: a new boot means it happened.
func TestDaemonFinishesARunAfterItsReboot(t *testing.T) {
	fp := &fakePlatform{boot: "b2"}
	p, clock := daemon(t, fp)
	writeFileAtomicT(t, filepath.Join(p.StateDir, packagesRunFile), PackageRunStatus{ID: "cdcdcdcd-1",
		State: RunRebooting, Unit: "u", BootID: "b1", StartedAt: clock.Add(-time.Hour)})
	p.tick(context.Background())
	st := p.Beat().Run
	if st == nil || st.State != RunDone || !st.Rebooted || st.RebootRequired {
		t.Fatalf("run = %+v", st)
	}
	if fp.repairs != 0 {
		t.Error("a clean reboot was repaired")
	}
}

func TestDaemonRebootThatNeverHappened(t *testing.T) {
	fp := &fakePlatform{boot: "b1"}
	p, clock := daemon(t, fp)
	writeFileAtomicT(t, filepath.Join(p.StateDir, packagesRunFile), PackageRunStatus{ID: "efefefef-1",
		State: RunRebooting, Unit: "u", BootID: "b1", StartedAt: clock.Add(-time.Hour)})
	p.tick(context.Background())
	st := p.Beat().Run
	if st == nil || st.State != RunDone || !st.RebootRequired || st.Rebooted {
		t.Fatalf("run = %+v", st)
	}
}

// A run that is still going after the agent restarts is followed, not
// started again and not settled.
func TestDaemonFollowsARunAcrossItsOwnRestart(t *testing.T) {
	fp := &fakePlatform{boot: "b1", isActive: true}
	p, clock := daemon(t, fp)
	req := PackageRunRequest{ID: "12121212-1", Mode: PackagesAll}
	writeFileAtomicT(t, filepath.Join(p.StateDir, packagesRunFile), PackageRunStatus{ID: req.ID,
		State: RunRunning, Step: "install", Unit: "u", BootID: "b1", StartedAt: clock.Add(-time.Minute), At: *clock})
	p.Handle(context.Background(), &PackageOrders{Run: &req})
	p.tick(context.Background())
	if len(fp.launched) != 0 || fp.repairs != 0 {
		t.Fatalf("launched=%v repairs=%d", fp.launched, fp.repairs)
	}
	if st := p.Beat().Run; st == nil || st.State != RunRunning {
		t.Fatalf("run = %+v", st)
	}
}

func TestDaemonChecksOnScheduleAndWhenAsked(t *testing.T) {
	fp := &fakePlatform{report: PackageReport{Supported: true, Manager: "apt"}}
	p, clock := daemon(t, fp)
	fp.report.CheckedAt = *clock

	// An older control plane says nothing: no looking.
	p.Handle(context.Background(), nil)
	p.tick(context.Background())
	if fp.checks != 0 {
		t.Fatal("checked with no orders from the console")
	}
	p.Handle(context.Background(), &PackageOrders{Check: true})
	p.tick(context.Background())
	if fp.checks != 1 {
		t.Fatalf("checks = %d", fp.checks)
	}
	b := p.Beat()
	if b.Report == nil {
		t.Fatal("a new report was not put on the heartbeat")
	}
	p.Delivered(b)
	if p.Beat().Report != nil {
		t.Error("a delivered report is sent again")
	}
	// Not again until six hours have passed.
	*clock = clock.Add(time.Hour)
	p.tick(context.Background())
	if fp.checks != 1 {
		t.Error("checked again too soon")
	}
	// Unless the console asks.
	p.Handle(context.Background(), &PackageOrders{Check: true, CheckID: "check-1"})
	fp.report.CheckedAt = *clock
	p.tick(context.Background())
	if fp.checks != 2 {
		t.Fatalf("an asked-for check did not run")
	}
	b = p.Beat()
	if b.CheckID != "check-1" {
		t.Errorf("check id = %q", b.CheckID)
	}
	p.Delivered(b)
	if p.Beat().CheckID != "" {
		t.Error("the answer to a check is repeated after it was delivered")
	}
	// The same id asked again is not a new check.
	p.Handle(context.Background(), &PackageOrders{Check: true, CheckID: "check-1"})
	p.tick(context.Background())
	if fp.checks != 2 {
		t.Error("the same check id ran twice")
	}
	*clock = clock.Add(7 * time.Hour)
	fp.report.CheckedAt = *clock
	p.tick(context.Background())
	if fp.checks != 3 {
		t.Error("no check after six hours")
	}
}

func TestDaemonCheckFailureKeepsWhatWasKnown(t *testing.T) {
	fp := &fakePlatform{report: PackageReport{Supported: true, Manager: "apt",
		Updates: []PackageUpdate{{Name: "openssl", Available: "2", Security: true}}}}
	p, clock := daemon(t, fp)
	fp.report.CheckedAt = *clock
	p.Handle(context.Background(), &PackageOrders{Check: true})
	p.tick(context.Background())
	p.Delivered(p.Beat())

	fp.checkErr = errors.New("apt-get update failed (exit 100): Temporary failure resolving")
	*clock = clock.Add(7 * time.Hour)
	p.tick(context.Background())
	b := p.Beat()
	if b.Report == nil || b.Report.Error == "" || len(b.Report.Updates) != 1 {
		t.Fatalf("report = %+v", b.Report)
	}
	// Tried again after half an hour, not at once.
	*clock = clock.Add(time.Minute)
	p.tick(context.Background())
	if fp.checks != 2 {
		t.Errorf("checks = %d, want 2 (retry waits)", fp.checks)
	}
	*clock = clock.Add(31 * time.Minute)
	p.tick(context.Background())
	if fp.checks != 3 {
		t.Errorf("checks = %d, want 3", fp.checks)
	}
}

func TestDaemonRememberAcrossRestart(t *testing.T) {
	fp := &fakePlatform{report: PackageReport{Supported: true, Manager: "dnf"}}
	p, clock := daemon(t, fp)
	fp.report.CheckedAt = *clock
	p.Handle(context.Background(), &PackageOrders{Check: true})
	p.tick(context.Background())

	// A new daemon on the same state directory knows the last report and
	// does not look again before it is due.
	p2 := &Packages{StateDir: p.StateDir, Log: p.Log, platform: fp, now: p.now}
	p2.Handle(context.Background(), &PackageOrders{Check: true})
	p2.tick(context.Background())
	if fp.checks != 1 {
		t.Errorf("checks = %d: a restart checked again", fp.checks)
	}
	if b := p2.Beat(); b.Report == nil || b.Report.Manager != "dnf" {
		t.Errorf("report after restart = %+v", b.Report)
	}
}

func writeFileAtomicT(t *testing.T, path string, v any) {
	t.Helper()
	if err := writeFileAtomic(path, mustJSON(v), 0o600); err != nil {
		t.Fatal(err)
	}
}

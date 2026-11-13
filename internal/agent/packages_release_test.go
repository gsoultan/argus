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

/* ── Reading the release ─────────────────────────────────────────────────── */

func TestParseOSRelease(t *testing.T) {
	m := parseOSRelease([]byte(`PRETTY_NAME="Ubuntu 22.04.5 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
# a comment
ID=ubuntu
ID_LIKE=debian
VERSION_CODENAME=jammy
`))
	if m["ID"] != "ubuntu" || m["VERSION_ID"] != "22.04" || m["PRETTY_NAME"] != "Ubuntu 22.04.5 LTS" {
		t.Fatalf("got %v", m)
	}
}

func TestVersionIs(t *testing.T) {
	for _, c := range []struct {
		now, want string
		ok        bool
	}{
		{"24.04", "24.04", true}, {"9.4", "9", true}, {"13", "13", true},
		{"22.04", "24.04", false}, {"19", "9", false}, {"", "9", false}, {"9", "9.4", false},
	} {
		if got := versionIs(c.now, c.want); got != c.ok {
			t.Errorf("versionIs(%q, %q) = %v", c.now, c.want, got)
		}
	}
}

func TestParseReleaseCheck(t *testing.T) {
	next, why := parseReleaseCheck([]byte("Checking for a new Ubuntu release\nNew release '24.04.1 LTS' available.\n" +
		"Run 'do-release-upgrade' to upgrade to it.\n"))
	if next != "24.04.1 LTS" || why != "" || ubuntuVersionID(next) != "24.04" {
		t.Fatalf("next=%q why=%q", next, why)
	}
	if next, why := parseReleaseCheck([]byte("Checking for a new Ubuntu release\nNo new release found.\n")); next != "" || why != "" {
		t.Errorf("none: %q %q", next, why)
	}
	_, why = parseReleaseCheck([]byte("In /etc/update-manager/release-upgrades Prompt \nis set to never so upgrading is not possible.\n"))
	if !strings.Contains(why, "Prompt=never") {
		t.Errorf("never: %q", why)
	}
}

/* ── Debian's sources ────────────────────────────────────────────────────── */

func TestRewriteSourcesList(t *testing.T) {
	in := `# the archive
deb http://deb.debian.org/debian bookworm main contrib non-free-firmware
deb-src http://deb.debian.org/debian bookworm main
deb http://deb.debian.org/debian bookworm-updates main
deb [signed-by=/usr/share/keyrings/debian.gpg] http://security.debian.org/debian-security bookworm-security main
deb http://mirror.example.internal/debian/ bookworm-backports main
deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable
deb http://apt.postgresql.org/pub/repos/apt bookworm-pgdg main
deb https://repo.example.com/apt stable main
deb http://deb.debian.org/debian sid main
`
	out, rewritten, disabled := rewriteSourcesList([]byte(in), "bookworm", "trixie")
	got := string(out)
	for _, want := range []string{
		"# the archive\n",
		"deb http://deb.debian.org/debian trixie main contrib non-free-firmware\n",
		"deb-src http://deb.debian.org/debian trixie main\n",
		"deb http://deb.debian.org/debian trixie-updates main\n",
		"deb [signed-by=/usr/share/keyrings/debian.gpg] http://security.debian.org/debian-security trixie-security main\n",
		"deb http://mirror.example.internal/debian/ trixie-backports main\n",
		disabledMark + "trixie: deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable\n",
		disabledMark + "trixie: deb http://apt.postgresql.org/pub/repos/apt bookworm-pgdg main\n",
		"deb https://repo.example.com/apt stable main\n",
		"deb http://deb.debian.org/debian sid main\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "bookworm main") && !strings.Contains(got, "# disabled") {
		t.Error("a Debian line still names bookworm")
	}
	if !rewritten || len(disabled) != 2 {
		t.Errorf("rewritten=%v disabled=%v", rewritten, disabled)
	}
}

func TestRewriteSourcesListBusterSecurity(t *testing.T) {
	out, _, _ := rewriteSourcesList([]byte("deb http://security.debian.org/debian-security buster/updates main\n"),
		"buster", "bullseye")
	if string(out) != "deb http://security.debian.org/debian-security bullseye-security main\n" {
		t.Fatalf("got %q", out)
	}
}

func TestRewriteSourcesDeb822(t *testing.T) {
	in := `Types: deb
URIs: http://deb.debian.org/debian
Suites: bookworm bookworm-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://deb.debian.org/debian-security
Suites: bookworm-security
Components: main
`
	out, rewritten, disabled := rewriteSourcesDeb822([]byte(in), "bookworm", "trixie")
	want := strings.ReplaceAll(in, "Suites: bookworm bookworm-updates", "Suites: trixie trixie-updates")
	want = strings.ReplaceAll(want, "Suites: bookworm-security", "Suites: trixie-security")
	if string(out) != want || !rewritten || len(disabled) != 0 {
		t.Fatalf("got:\n%s\nrewritten=%v disabled=%v", out, rewritten, disabled)
	}

	third := "Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: bookworm\nComponents: stable\n"
	out, rewritten, disabled = rewriteSourcesDeb822([]byte(third), "bookworm", "trixie")
	if rewritten || len(disabled) != 1 || !strings.Contains(string(out), "Enabled: no\n") ||
		!strings.Contains(string(out), "Suites: bookworm\n") {
		t.Fatalf("third party:\n%s\nrewritten=%v disabled=%v", out, rewritten, disabled)
	}
	off := "Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: bookworm\nEnabled: yes\n"
	out, _, _ = rewriteSourcesDeb822([]byte(off), "bookworm", "trixie")
	if strings.Contains(string(out), "Enabled: yes") || strings.Count(string(out), "Enabled:") != 1 {
		t.Fatalf("enabled kept:\n%s", out)
	}
}

func TestPointSourcesAtAndBack(t *testing.T) {
	root, backup := t.TempDir(), filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(filepath.Join(root, "sources.list.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	list := "deb http://deb.debian.org/debian bookworm main\n"
	docker := "deb https://download.docker.com/linux/debian bookworm stable\n"
	other := "deb https://repo.example.com/apt stable main\n"
	write := func(rel, s string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sources.list", list)
	write("sources.list.d/docker.list", docker)
	write("sources.list.d/other.list", other)
	ch, err := pointSourcesAt(root, backup, "bookworm", "trixie")
	if err != nil || len(ch.Rewritten) != 1 || len(ch.Disabled) != 1 {
		t.Fatalf("change = %+v, %v", ch, err)
	}
	read := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(root, rel))
		return string(b)
	}
	if !strings.Contains(read("sources.list"), "trixie") || !strings.HasPrefix(read("sources.list.d/docker.list"), "# disabled") {
		t.Fatalf("not rewritten: %q %q", read("sources.list"), read("sources.list.d/docker.list"))
	}
	if _, err := os.Stat(filepath.Join(backup, "sources.list.d", "other.list")); err == nil {
		t.Error("an untouched file was backed up")
	}
	if notes := disabledSourceNotes(root); len(notes) != 1 || !strings.Contains(notes[0], "docker.list") {
		t.Errorf("notes = %v", notes)
	}
	if err := restoreSources(root, backup); err != nil {
		t.Fatal(err)
	}
	if read("sources.list") != list || read("sources.list.d/docker.list") != docker || read("sources.list.d/other.list") != other {
		t.Fatal("restore did not put the old sources back")
	}
}

/* ── leapp ───────────────────────────────────────────────────────────────── */

func TestLeappInhibitors(t *testing.T) {
	rep := `{"entries":[
 {"title":"Possible problems with remote login using root account","groups":["inhibitor","authentication"],
  "detail":{"remediations":[{"type":"hint","context":"Set PermitRootLogin explicitly in sshd_config"}]}},
 {"title":"Missing required answers in the answer file","flags":["inhibitor"]},
 {"title":"Packages not signed by Red Hat found","groups":["sanity"]}
]}`
	got := leappInhibitors([]byte(rep))
	if len(got) != 2 || got[0] != "Possible problems with remote login using root account (Set PermitRootLogin explicitly in sshd_config)" ||
		got[1] != "Missing required answers in the answer file" {
		t.Fatalf("got %q", got)
	}
	if leappInhibitors([]byte("not json")) != nil {
		t.Error("garbage read as a report")
	}
}

/* ── The check ───────────────────────────────────────────────────────────── */

// releaseHost points the release code at a fake server: os-release, apt's
// sources, the override directory, leapp's report, the control plane's
// binary, and free space.
type releaseHost struct {
	dir     string
	osr     string
	sources string
	free    int64
}

func newReleaseHost(t *testing.T, osr string) *releaseHost {
	t.Helper()
	h := &releaseHost{dir: t.TempDir(), free: 50 << 30}
	h.osr = filepath.Join(h.dir, "os-release")
	h.sources = filepath.Join(h.dir, "apt")
	h.setOS(t, osr)
	old := []any{osReleaseFiles, ubuntuUpgradeOverrides, aptSourcesRoot, leappReportFile, argusControlBinary, releaseFree,
		toolGlob, ubuntuUpgradeLogs, leappUpgradeLogs, efiFirmwareDir, deviceExists}
	toolGlob = func(string) ([]string, error) { return nil, nil }
	efiFirmwareDir = filepath.Join(h.dir, "no-efi")
	deviceExists = func(string) bool { return false }
	ubuntuUpgradeLogs = []string{filepath.Join(h.dir, "main.log"), filepath.Join(h.dir, "apt-term.log")}
	leappUpgradeLogs = []string{filepath.Join(h.dir, "leapp-preupgrade.log"), filepath.Join(h.dir, "leapp-upgrade.log")}
	osReleaseFiles = []string{h.osr}
	ubuntuUpgradeOverrides = filepath.Join(h.dir, "release-upgrades.d")
	aptSourcesRoot = h.sources
	leappReportFile = filepath.Join(h.dir, "leapp-report.json")
	argusControlBinary = filepath.Join(h.dir, "argus-control")
	releaseFree = func(string) int64 { return h.free }
	t.Cleanup(func() {
		osReleaseFiles = old[0].([]string)
		ubuntuUpgradeOverrides = old[1].(string)
		aptSourcesRoot = old[2].(string)
		leappReportFile = old[3].(string)
		argusControlBinary = old[4].(string)
		releaseFree = old[5].(func(string) int64)
		toolGlob = old[6].(func(string) ([]string, error))
		ubuntuUpgradeLogs = old[7].([]string)
		leappUpgradeLogs = old[8].([]string)
		efiFirmwareDir = old[9].(string)
		deviceExists = old[10].(func(string) bool)
	})
	return h
}

func (h *releaseHost) setOS(t *testing.T, osr string) {
	t.Helper()
	if err := os.WriteFile(h.osr, []byte(osr), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	jammy    = "ID=ubuntu\nVERSION_ID=\"22.04\"\nPRETTY_NAME=\"Ubuntu 22.04.5 LTS\"\n"
	noble    = "ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"
	bookworm = "ID=debian\nVERSION_ID=\"12\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n"
	trixie   = "ID=debian\nVERSION_ID=\"13\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n"
	alma8    = "ID=\"almalinux\"\nVERSION_ID=\"8.10\"\nPRETTY_NAME=\"AlmaLinux 8.10 (Cerulean Leopard)\"\n"
)

// scriptRun answers commands by their first words, recording each.
type scriptRun struct {
	mu    sync.Mutex
	calls []string
	on    map[string]func(out io.Writer, env []string) int
}

func (s *scriptRun) run(_ context.Context, out io.Writer, env []string, name string, args ...string) (int, error) {
	s.mu.Lock()
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	s.calls = append(s.calls, cmd)
	s.mu.Unlock()
	if out == nil {
		out = io.Discard
	}
	best := ""
	for k := range s.on {
		if strings.HasPrefix(cmd, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return 0, nil
	}
	return s.on[best](out, env), nil
}

func (s *scriptRun) ran(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func looks(tools ...string) lookPathFunc {
	return func(n string) (string, error) {
		for _, t := range tools {
			if t == n {
				return "/usr/bin/" + n, nil
			}
		}
		return "", errors.New("not here")
	}
}

func TestReleaseCheckUbuntu(t *testing.T) {
	newReleaseHost(t, jammy)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
	}}
	c := PackageChecker{Run: sr.run, Look: looks("apt-get", "dpkg-query", "do-release-upgrade")}
	info := c.release(context.Background(), PackageReport{})
	if info == nil || info.Distro != "ubuntu" || info.Version != "22.04" || info.Next != "24.04" ||
		info.NextName != "Ubuntu 24.04.1 LTS" || info.Tool != releaseToolUbuntu || len(info.Blockers) != 0 {
		t.Fatalf("info = %+v", info)
	}
	// Not ready: updates waiting, a reboot pending, argus-control here, a
	// full disk. Each is said, and none stops the offer being shown.
	h := newReleaseHost(t, jammy)
	h.free = 1 << 30
	if err := os.WriteFile(argusControlBinary, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	info = c.release(context.Background(), PackageReport{RebootRequired: true,
		Updates: []PackageUpdate{{Name: "openssl"}, {Name: "curl"}}})
	if info.Next != "24.04" || len(info.Blockers) != 5 {
		t.Fatalf("blockers = %q", info.Blockers)
	}
	joined := strings.Join(info.Blockers, " | ")
	for _, want := range []string{"argus-control runs on this server", "install the 2 updates", "reboot it first",
		"/ has 1.0 GB free", "/var has 1.0 GB free"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in %s", want, joined)
		}
	}
	// Without the upgrader, the reason says what to install.
	c.Look = looks("apt-get", "dpkg-query")
	if info := c.release(context.Background(), PackageReport{}); info.Next != "" ||
		!strings.Contains(info.Reason, "ubuntu-release-upgrader-core") {
		t.Errorf("no upgrader: %+v", info)
	}
}

func TestReleaseCheckOthers(t *testing.T) {
	c := PackageChecker{Run: (&scriptRun{}).run, Look: looks("dnf", "leapp")}
	newReleaseHost(t, bookworm)
	if info := c.release(context.Background(), PackageReport{}); info.Next != "13" || info.NextName != "Debian 13 (trixie)" ||
		info.Tool != releaseToolDebian {
		t.Errorf("bookworm: %+v", info)
	}
	newReleaseHost(t, trixie)
	if info := c.release(context.Background(), PackageReport{}); info.Next != "" || info.Reason != "" {
		t.Errorf("trixie has no stable next yet: %+v", info)
	}
	newReleaseHost(t, alma8)
	if info := c.release(context.Background(), PackageReport{}); info.Next != "9" || info.NextName != "AlmaLinux 9" ||
		info.Tool != releaseToolLeapp {
		t.Errorf("alma 8: %+v", info)
	}
	c.Look = looks("dnf")
	if info := c.release(context.Background(), PackageReport{}); info.Next != "" || !strings.Contains(info.Reason, "ELevate") {
		t.Errorf("alma without leapp: %+v", info)
	}
	newReleaseHost(t, "ID=fedora\nVERSION_ID=40\n")
	if info := c.release(context.Background(), PackageReport{}); info.Reason != releaseUnsupported {
		t.Errorf("fedora: %+v", info)
	}
	osReleaseFiles = []string{filepath.Join(t.TempDir(), "none")}
	if c.release(context.Background(), PackageReport{}) != nil {
		t.Error("a server with no os-release was given a release")
	}
}

/* ── The helper ──────────────────────────────────────────────────────────── */

func releaseApplier(t *testing.T, sr *scriptRun, look lookPathFunc, expect string) (*PackageApplier, *bool) {
	t.Helper()
	dir := t.TempDir()
	old := rebootRequiredFile
	rebootRequiredFile = filepath.Join(dir, "reboot-required")
	t.Cleanup(func() { rebootRequiredFile = old })
	rebooted := false
	a := &PackageApplier{
		Plan: PackagePlan{PackageRunRequest: PackageRunRequest{ID: "release-run-1", Mode: PackagesRelease,
			Expect: expect, Reboot: "if_needed"}, StateDir: dir, Unit: "argus-packages-test"},
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		PackageChecker: PackageChecker{Run: sr.run, Look: look},
		Restart:        func(context.Context, string) error { return nil },
		Reboot:         func() error { rebooted = true; return nil },
		BootID:         func() string { return "boot-1" },
	}
	return a, &rebooted
}

func TestApplyReleaseUbuntu(t *testing.T) {
	h := newReleaseHost(t, jammy)
	sawOverride := false
	var upgradeEnv []string
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"do-release-upgrade -f DistUpgradeViewNonInteractive": func(w io.Writer, env []string) int {
			b, err := os.ReadFile(filepath.Join(ubuntuUpgradeOverrides, ubuntuOverrideName))
			sawOverride = err == nil && strings.Contains(string(b), "RemoveObsoletes=no")
			upgradeEnv = env
			fmt.Fprintln(w, "System upgrade is complete.")
			h.setOS(t, noble)
			return 0
		},
		"dpkg-query -S /usr/bin/sshd": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "openssh-server: /usr/sbin/sshd")
			return 0
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade", "sshd"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunRebooting || st.Step != "reboot" || !*rebooted {
		t.Fatalf("run = %+v rebooted=%v", st, *rebooted)
	}
	if st.Release == nil || st.Release.From != "22.04" || st.Release.To != "24.04" || st.Release.ToName != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("release = %+v", st.Release)
	}
	if !sawOverride {
		t.Error("do-release-upgrade ran without RemoveObsoletes=no")
	}
	if _, err := os.Stat(filepath.Join(ubuntuUpgradeOverrides, ubuntuOverrideName)); err == nil {
		t.Error("the override outlived the run")
	}
	if !strings.Contains(strings.Join(upgradeEnv, " "), "UCF_FORCE_CONFFOLD=1") {
		t.Errorf("sshd_config could be replaced: env %v", upgradeEnv)
	}
	var deps []releaseDep
	b, err := os.ReadFile(filepath.Join(a.Plan.StateDir, packagesDir, releaseDepsFile))
	if err != nil || json.Unmarshal(b, &deps) != nil || len(deps) != 1 || deps[0].Package != "openssh-server" {
		t.Errorf("deps = %s, %v", b, err)
	}
	if disk := readRunFile(t, a.Plan.StateDir); disk.State != RunRebooting || disk.Release == nil {
		t.Errorf("on disk: %+v", disk)
	}
}

// The release on offer moved since a person said yes, or the server is not
// ready: nothing runs.
func TestApplyReleaseRefusesBeforeChangingAnything(t *testing.T) {
	newReleaseHost(t, jammy)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.10' available.")
			return 0
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "now offers 24.10, not the 24.04") || *rebooted {
		t.Fatalf("run = %+v", st)
	}
	if sr.ran("do-release-upgrade -f") {
		t.Error("upgraded anyway")
	}

	h := newReleaseHost(t, jammy)
	h.free = 1 << 30
	sr = &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
	}}
	a, _ = releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	if st := a.Apply(context.Background()); st.State != RunFailed || !strings.Contains(st.Message, "GB free") ||
		sr.ran("do-release-upgrade -f") {
		t.Fatalf("full disk: %+v", st)
	}
}

func TestApplyReleaseUbuntuFails(t *testing.T) {
	newReleaseHost(t, jammy)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"do-release-upgrade -f": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "E:Could not calculate the upgrade")
			return 1
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || *rebooted || !strings.Contains(st.Message, "part way between releases") ||
		!strings.Contains(st.Message, "/var/log/dist-upgrade/main.log") || !strings.Contains(st.Message, "Do not reboot") {
		t.Fatalf("run = %+v", st)
	}
	if !sr.ran("dpkg --force-confdef --force-confold --configure -a") {
		t.Error("what dpkg left half done was not finished")
	}
}

func TestApplyReleaseDebian(t *testing.T) {
	h := newReleaseHost(t, bookworm)
	if err := os.MkdirAll(filepath.Join(h.sources, "sources.list.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.sources, "sources.list"),
		[]byte("deb http://deb.debian.org/debian bookworm main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var order []string
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"apt-get -o DPkg::Lock::Timeout=120 -q update": func(w io.Writer, _ []string) int {
			b, _ := os.ReadFile(filepath.Join(h.sources, "sources.list"))
			order = append(order, "update:"+strings.Fields(string(b))[2])
			return 0
		},
		"apt-get -o DPkg::Lock::Timeout=120 -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dpkg::Use-Pty=0 upgrade --without-new-pkgs": func(io.Writer, []string) int {
			order = append(order, "upgrade")
			return 0
		},
		"apt-get -o DPkg::Lock::Timeout=120 -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dpkg::Use-Pty=0 full-upgrade": func(io.Writer, []string) int {
			order = append(order, "full-upgrade")
			h.setOS(t, trixie)
			return 0
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query"), "13")
	st := a.Apply(context.Background())
	if st.State != RunRebooting || !*rebooted || st.Release == nil || st.Release.To != "13" {
		t.Fatalf("run = %+v", st)
	}
	if got := strings.Join(order, ","); got != "update:bookworm,update:trixie,upgrade,full-upgrade" {
		t.Fatalf("order = %s", got)
	}
}

// The new release's lists cannot be downloaded: apt is put back on the old
// release, and nothing was upgraded.
func TestApplyReleaseDebianPutsSourcesBack(t *testing.T) {
	h := newReleaseHost(t, bookworm)
	list := "deb http://deb.debian.org/debian bookworm main\n"
	if err := os.MkdirAll(h.sources, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.sources, "sources.list"), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"apt-get -o DPkg::Lock::Timeout=120 -q update": func(w io.Writer, _ []string) int {
			b, _ := os.ReadFile(filepath.Join(h.sources, "sources.list"))
			if strings.Contains(string(b), "trixie") {
				fmt.Fprintln(w, "E: The repository 'http://deb.debian.org/debian trixie Release' does not have a Release file.")
				return 100
			}
			return 0
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query"), "13")
	st := a.Apply(context.Background())
	if st.State != RunFailed || *rebooted || !strings.Contains(st.Message, "apt is back on bookworm") {
		t.Fatalf("run = %+v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(h.sources, "sources.list")); string(b) != list {
		t.Errorf("sources = %q", b)
	}
	if sr.ran("apt-get -o DPkg::Lock::Timeout=120 -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dpkg::Use-Pty=0 full-upgrade") {
		t.Error("upgraded with lists that failed")
	}
}

func TestApplyReleaseLeappInhibited(t *testing.T) {
	newReleaseHost(t, alma8)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"leapp preupgrade": func(io.Writer, []string) int {
			_ = os.WriteFile(leappReportFile, []byte(`{"entries":[{"title":"Detected loaded kernel drivers which have been removed","groups":["inhibitor"]}]}`), 0o644)
			return 1
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("dnf", "leapp"), "9")
	st := a.Apply(context.Background())
	if st.State != RunFailed || *rebooted || !strings.Contains(st.Message, "Detected loaded kernel drivers") ||
		!strings.Contains(st.Message, "nothing was changed") {
		t.Fatalf("run = %+v", st)
	}
	if sr.ran("leapp upgrade") {
		t.Error("upgraded past an inhibitor")
	}
}

func TestApplyReleaseLeapp(t *testing.T) {
	newReleaseHost(t, alma8)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"leapp preupgrade": func(io.Writer, []string) int {
			_ = os.WriteFile(leappReportFile, []byte(`{"entries":[{"title":"x","groups":["sanity"]}]}`), 0o644)
			return 0
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("dnf", "leapp"), "9")
	st := a.Apply(context.Background())
	// The upgrade happens in leapp's boot: the run ends rebooting, still
	// on 8, and the agent that comes back says whether it took.
	if st.State != RunRebooting || !*rebooted || !sr.ran("leapp upgrade") || st.Release.Tool != releaseToolLeapp {
		t.Fatalf("run = %+v", st)
	}
}

/* ── After the reboot ────────────────────────────────────────────────────── */

func TestFinishReleaseBringsBackTheAgentsTools(t *testing.T) {
	newReleaseHost(t, noble)
	state := t.TempDir()
	deps := []releaseDep{{Tool: "sshd", Path: "/usr/sbin/sshd", Package: "openssh-server"},
		{Tool: "pg_dump", Path: "/usr/lib/postgresql/14/bin/pg_dump", Package: "postgresql-client-14"},
		{Tool: "mongosh", Path: "/usr/bin/mongosh", Package: "mongodb-mongosh"}}
	if err := os.MkdirAll(filepath.Join(state, packagesDir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFileAtomicT(t, filepath.Join(state, packagesDir, releaseDepsFile), deps)
	if err := os.MkdirAll(ubuntuUpgradeOverrides, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(ubuntuUpgradeOverrides, ubuntuOverrideName), []byte(ubuntuOverride), 0o644)

	present := map[string]bool{"apt-get": true, "dpkg-query": true, "sshd": true}
	var mu sync.Mutex
	look := func(n string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if present[n] {
			return "/usr/bin/" + n, nil
		}
		return "", errors.New("no")
	}
	installs := func(pkg string, brings string, ok bool) func(io.Writer, []string) int {
		return func(io.Writer, []string) int {
			if !ok {
				return 100
			}
			mu.Lock()
			present[brings] = true
			mu.Unlock()
			return 0
		}
	}
	prefix := "apt-get -o DPkg::Lock::Timeout=120 -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dpkg::Use-Pty=0 install "
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		prefix + "postgresql-client-14": installs("postgresql-client-14", "", false),
		prefix + "postgresql-client":    installs("postgresql-client", "pg_dump", true),
		prefix + "mongodb-mongosh":      installs("mongodb-mongosh", "", false),
		"dpkg --audit":                  func(io.Writer, []string) int { return 0 },
	}}
	res := finishRelease(context.Background(), sr.run, look, state, io.Discard)
	if strings.Join(res.Restored, ",") != "postgresql-client" || strings.Join(res.Missing, ",") != "mongosh" {
		t.Fatalf("res = %+v", res)
	}
	if sr.ran(prefix + "openssh-server") {
		t.Error("reinstalled a tool that was still there")
	}
	if _, err := os.Stat(filepath.Join(ubuntuUpgradeOverrides, ubuntuOverrideName)); err == nil {
		t.Error("Argus's upgrade setting was left behind")
	}
	if !sr.ran("dpkg --force-confdef --force-confold --configure -a") {
		t.Error("the package manager was not finished")
	}
}

type finishingPlatform struct {
	*fakePlatform
	res   ReleaseFinish
	err   error
	calls int
}

func (f *finishingPlatform) finishRelease(context.Context) (ReleaseFinish, error) {
	f.calls++
	return f.res, f.err
}

func TestDaemonReleaseRun(t *testing.T) {
	fp := &fakePlatform{boot: "b1", isActive: true}
	p, _ := daemon(t, fp)
	if !p.Beat().ReleaseUpgrade {
		t.Error("a server that can be updated should say it can upgrade its release")
	}
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "78787878-1", Mode: PackagesRelease}})
	if st := p.Beat().Run; st == nil || st.State != RunFailed || len(fp.launched) != 0 {
		t.Fatalf("a release run with no release said yes to: %+v", st)
	}
	p, _ = daemon(t, fp)
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "78787878-2", Mode: PackagesRelease,
		Expect: "24.04; rm -rf /"}})
	if len(fp.launched) != 0 {
		t.Fatal("launched with a malformed release")
	}
	p, _ = daemon(t, fp)
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "78787878-3", Mode: PackagesRelease,
		Expect: "24.04"}})
	if len(fp.launched) != 1 {
		t.Fatalf("launched %d", len(fp.launched))
	}
}

func rebootingRelease(p *Packages, clock time.Time, id string) {
	_ = writeFileAtomic(filepath.Join(p.StateDir, packagesRunFile), mustJSON(PackageRunStatus{ID: id,
		State: RunRebooting, Step: "reboot", Unit: "u", BootID: "b1", StartedAt: clock.Add(-time.Hour),
		Release: &ReleaseChange{Distro: "ubuntu", From: "22.04", FromName: "Ubuntu 22.04.5 LTS", To: "24.04",
			ToName: "Ubuntu 24.04.1 LTS", Tool: releaseToolUbuntu}}), 0o600)
}

// Back on the new boot, on the new release: done, with what was put back.
func TestDaemonSettlesAReleaseUpgrade(t *testing.T) {
	newReleaseHost(t, noble)
	fp := &finishingPlatform{fakePlatform: &fakePlatform{boot: "b2"},
		res: ReleaseFinish{Restored: []string{"postgresql-client"}, Missing: []string{"mongosh"}}}
	p, clock := daemon(t, fp.fakePlatform)
	p.platform = fp
	rebootingRelease(p, *clock, "89898989-1")
	p.tick(context.Background())
	st := p.Beat().Run
	if st == nil || st.State != RunDone || !st.Rebooted || fp.calls != 1 {
		t.Fatalf("run = %+v calls=%d", st, fp.calls)
	}
	for _, want := range []string{"upgraded from Ubuntu 22.04.5 LTS to Ubuntu 24.04.1 LTS",
		"installed postgresql-client again", "still missing, and the agent uses it: mongosh"} {
		if !strings.Contains(st.Message, want) {
			t.Errorf("no %q in %q", want, st.Message)
		}
	}
	if fp.checks != 1 {
		t.Errorf("checks = %d: the new release's updates should be looked for", fp.checks)
	}
}

// A reboot is not an upgrade: back on the old release, the run failed.
func TestDaemonReleaseUpgradeThatDidNotTake(t *testing.T) {
	newReleaseHost(t, jammy)
	fp := &finishingPlatform{fakePlatform: &fakePlatform{boot: "b2"}}
	p, clock := daemon(t, fp.fakePlatform)
	p.platform = fp
	rebootingRelease(p, *clock, "9a9a9a9a-1")
	p.tick(context.Background())
	st := p.Beat().Run
	if st == nil || st.State != RunFailed || !strings.Contains(st.Message, "still on 22.04") ||
		!strings.Contains(st.Message, "/var/log/dist-upgrade/main.log") {
		t.Fatalf("run = %+v", st)
	}
}

func TestDaemonReleaseUpgradeNotRebooted(t *testing.T) {
	newReleaseHost(t, noble)
	fp := &fakePlatform{boot: "b1"}
	p, clock := daemon(t, fp)
	rebootingRelease(p, *clock, "abababab-9")
	p.tick(context.Background())
	st := p.Beat().Run
	if st == nil || st.State != RunDone || !st.RebootRequired || st.Rebooted ||
		!strings.Contains(st.Message, "reboot it so it runs the new release") {
		t.Fatalf("run = %+v", st)
	}
}

// Cut off part way: interrupted, and said that the server may be between
// releases.
func TestDaemonReleaseUpgradeInterrupted(t *testing.T) {
	newReleaseHost(t, jammy)
	fp := &fakePlatform{boot: "b2"}
	p, clock := daemon(t, fp)
	_ = writeFileAtomic(filepath.Join(p.StateDir, packagesRunFile), mustJSON(PackageRunStatus{ID: "bcbcbcbc-1",
		State: RunRunning, Step: "upgrade", Unit: "u", BootID: "b1", StartedAt: clock.Add(-time.Hour),
		Release: &ReleaseChange{To: "24.04", ToName: "Ubuntu 24.04.1 LTS", Tool: releaseToolUbuntu}}), 0o600)
	p.tick(context.Background())
	st := p.Beat().Run
	if st == nil || st.State != RunInterrupted || !strings.Contains(st.Message, "part way between releases") ||
		fp.repairs != 1 {
		t.Fatalf("run = %+v", st)
	}
}

// The upgrader writes to its own logs, not to its output: the console sees
// them while it runs, and the step says the newest line.
func TestApplyReleaseFollowsTheUpgradersLogs(t *testing.T) {
	h := newReleaseHost(t, jammy)
	oldEvery := followEvery
	followEvery = 10 * time.Millisecond
	t.Cleanup(func() { followEvery = oldEvery })
	if err := os.WriteFile(ubuntuUpgradeLogs[0], []byte("from an upgrade last year\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var during PackageRunStatus
	var env []string
	var a *PackageApplier
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"do-release-upgrade -f DistUpgradeViewNonInteractive": func(w io.Writer, e []string) int {
			env = e
			f, err := os.OpenFile(ubuntuUpgradeLogs[1], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(f, "Unpacking libc6:amd64 (2.39-0ubuntu8) over (2.35-0ubuntu3.8) ...")
			f.Close()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				// The follower replaces the file while this polls; on
				// Windows a read can meet the rename and fail, so a failed
				// read is just another poll.
				if b, err := os.ReadFile(filepath.Join(a.Plan.StateDir, packagesRunFile)); err == nil {
					_ = json.Unmarshal(b, &during)
				}
				if strings.Contains(during.Message, "Unpacking libc6") {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			h.setOS(t, noble)
			return 0
		},
	}}
	a, _ = releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunRebooting {
		t.Fatalf("run = %+v", st)
	}
	if during.Message != "upgrading: Unpacking libc6:amd64 (2.39-0ubuntu8) over (2.35-0ubuntu3.8) ..." {
		t.Errorf("while upgrading the step said %q", during.Message)
	}
	if !strings.Contains(during.Log, "Unpacking libc6") || strings.Contains(st.Log, "last year") {
		t.Errorf("log while upgrading %q, at the end %q", during.Log, st.Log)
	}
	if !strings.Contains(strings.Join(env, " "), "PYTHONUNBUFFERED=1") {
		t.Errorf("do-release-upgrade's output is block-buffered: env %v", env)
	}
}

func TestRunLogLastLine(t *testing.T) {
	l := &runLog{limit: 1 << 10}
	fmt.Fprint(l, "first\nprogress 10%\rprogress 55%\r\n\n")
	if got := l.lastLine(); got != "progress 55%" {
		t.Errorf("lastLine = %q", got)
	}
	l = &runLog{limit: 1 << 10}
	fmt.Fprint(l, strings.Repeat("x", 300))
	if got := l.lastLine(); len(got) > maxStatusLine+len("…") {
		t.Errorf("lastLine is %d long", len(got))
	}
}

func TestParseGrubDevices(t *testing.T) {
	b := []byte("  grub-pc/timeout: 0\n* grub-pc/install_devices: /dev/disk/by-id/ata-QEMU_HARDDISK_QM00001, /dev/sdb\n" +
		"  grub-pc/install_devices_empty: false\n")
	if got := parseGrubDevices(b); strings.Join(got, "|") != "/dev/disk/by-id/ata-QEMU_HARDDISK_QM00001|/dev/sdb" {
		t.Errorf("devices = %q", got)
	}
	if got := parseGrubDevices([]byte("* grub-pc/install_devices:\n")); len(got) != 0 {
		t.Errorf("empty answer read as %q", got)
	}
}

// grubHost is a BIOS server with grub-pc whose disk on record is gone, as on
// a cloud image: / on an LVM volume on vda2.
func grubHost(t *testing.T, onRecord string, lsblk map[string]string) (*scriptRun, *string) {
	t.Helper()
	newReleaseHost(t, noble)
	var answered string
	on := map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '26.04.1 LTS' available.")
			return 0
		},
		"dpkg-query -W -f=${Status} grub-pc": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, "install ok installed")
			return 0
		},
		"debconf-show grub-pc": func(w io.Writer, _ []string) int {
			fmt.Fprintf(w, "* grub-pc/install_devices: %s\n", onRecord)
			return 0
		},
		"findmnt -n -o SOURCE /boot": func(io.Writer, []string) int { return 1 },
		"findmnt -n -o SOURCE /": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "/dev/mapper/ubuntu--vg-ubuntu--lv")
			return 0
		},
		"debconf-set-selections": func(io.Writer, []string) int { return 0 },
	}
	for dev, parents := range lsblk {
		on["lsblk -n -o PKNAME "+dev] = func(w io.Writer, _ []string) int {
			fmt.Fprint(w, parents)
			return 0
		}
	}
	sr := &scriptRun{on: on}
	sr.on["debconf-set-selections"] = func(io.Writer, []string) int {
		sr.mu.Lock()
		last := sr.calls[len(sr.calls)-1]
		sr.mu.Unlock()
		b, _ := os.ReadFile(strings.TrimPrefix(last, "debconf-set-selections "))
		answered = string(b)
		return 0
	}
	return sr, &answered
}

func TestApplyReleaseAnswersGrubsDiskFirst(t *testing.T) {
	sr, answered := grubHost(t, "/dev/disk/by-id/ata-gone", map[string]string{
		"/dev/mapper/ubuntu--vg-ubuntu--lv": "vda3\n",
		"/dev/vda3":                         "vda\n",
		"/dev/vda":                          "\n",
	})
	upgraded := false
	sr.on["do-release-upgrade -f DistUpgradeViewNonInteractive"] = func(io.Writer, []string) int {
		upgraded = *answered != ""
		return 1
	}
	a, _ := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "26.04")
	a.Apply(context.Background())
	if *answered != "grub-pc grub-pc/install_devices multiselect /dev/vda\n" {
		t.Errorf("GRUB was told %q", *answered)
	}
	if !upgraded {
		t.Error("the upgrade ran before GRUB had its disk")
	}
}

func TestApplyReleaseStopsWhenGrubsDiskCannotBeTold(t *testing.T) {
	sr, answered := grubHost(t, "", map[string]string{
		"/dev/mapper/ubuntu--vg-ubuntu--lv": "md0\n",
		"/dev/md0":                          "sda2\nsdb2\n",
	})
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "26.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || *rebooted || !strings.Contains(st.Message, "dpkg-reconfigure grub-pc") ||
		!strings.Contains(st.Message, "nothing was changed") {
		t.Fatalf("run = %+v", st)
	}
	if sr.ran("do-release-upgrade -f") || *answered != "" {
		t.Error("the upgrade started without a disk for GRUB")
	}
}

func TestApplyReleaseKeepsGrubsDiskWhenStillThere(t *testing.T) {
	sr, answered := grubHost(t, "/dev/disk/by-id/ata-here", nil)
	deviceExists = func(p string) bool { return p == "/dev/disk/by-id/ata-here" }
	a, _ := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "26.04")
	a.Apply(context.Background())
	if *answered != "" || sr.ran("findmnt") {
		t.Errorf("a disk still there was replaced: %q", *answered)
	}
	if !sr.ran("do-release-upgrade -f") {
		t.Error("the upgrade did not run")
	}
}

const distUpgradeAborted = `2026-11-13 13:30:00,001 INFO Using config files '['./DistUpgrade.cfg']'
2026-11-13 13:30:01,500 DEBUG checking for held packages
2026-11-13 13:30:02,117 ERROR Dist-upgrade failed: 'E:Unable to correct problems, you have held broken packages.'
2026-11-13 13:30:02,118 ERROR Dist-upgrade failed: 'E:Unable to correct problems, you have held broken packages.'
2026-11-13 13:30:02,200 DEBUG abort called
2026-11-13 13:30:02,201 DEBUG Restoring original system state
`

func TestDistUpgradeFailure(t *testing.T) {
	why, restored := distUpgradeFailure([]byte(distUpgradeAborted))
	if !restored || why != "Dist-upgrade failed: 'E:Unable to correct problems, you have held broken packages.'" {
		t.Errorf("why %q restored %v", why, restored)
	}
	if why, restored := distUpgradeFailure([]byte("2026-11-13 13:30:00,001 INFO fine\n")); why != "" || restored {
		t.Errorf("why %q restored %v", why, restored)
	}
}

// The upgrader gave up before touching anything: Argus says so, with its
// reason from main.log, and does not call the server half upgraded.
func TestApplyReleaseUbuntuRolledBack(t *testing.T) {
	newReleaseHost(t, jammy)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"do-release-upgrade -f DistUpgradeViewNonInteractive": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "extracting 'noble.tar.gz'")
			if err := os.WriteFile(ubuntuUpgradeLogs[0], []byte(distUpgradeAborted), 0o644); err != nil {
				t.Fatal(err)
			}
			return 1
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || *rebooted || !strings.Contains(st.Message, "nothing was changed") ||
		!strings.Contains(st.Message, "held broken packages") || strings.Contains(st.Message, "part way") ||
		strings.Contains(st.Message, "extracting") {
		t.Fatalf("run = %+v", st)
	}
	if sr.ran("dpkg --force-confdef --force-confold --configure -a") {
		t.Error("repaired a server nothing was done to")
	}
}

// Part way: the reason is the upgrader's error, not its download chatter.
func TestApplyReleaseUbuntuPartWayGivesTheLogsReason(t *testing.T) {
	h := newReleaseHost(t, jammy)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"do-release-upgrade -f DistUpgradeViewNonInteractive": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "authenticate 'noble.tar.gz' against 'noble.tar.gz.gpg'")
			h.setOS(t, noble)
			if err := os.WriteFile(ubuntuUpgradeLogs[0],
				[]byte("2026-11-13 13:40:00,000 ERROR got an error from dpkg for pkg: 'grub-pc'\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return 1
		},
	}}
	a, _ := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "part way") ||
		!strings.Contains(st.Message, "error from dpkg for pkg: 'grub-pc'") || strings.Contains(st.Message, "authenticate") {
		t.Fatalf("run = %+v", st)
	}
}

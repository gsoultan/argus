package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/* ── Parsers ─────────────────────────────────────────────────────────────── */

const aptInstallSim = `NOTE: This is only a simulation!
Reading package lists...
The following additional packages will be installed:
  liblzf1 redis-tools
The following NEW packages will be installed:
  liblzf1 redis-server redis-tools
The following packages will be upgraded:
  libjemalloc2
1 upgraded, 3 newly installed, 0 to remove and 13 not upgraded.
Need to get 1,580 kB of archives.
After this operation, 6,234 kB of additional disk space will be used.
Inst libjemalloc2 [5.3.0-2] (5.3.0-2build1 Ubuntu:24.04/noble [amd64])
Inst liblzf1 (3.6-4 Ubuntu:24.04/noble [amd64])
Inst redis-tools (5:7.0.15-1build2 Ubuntu:24.04/noble [amd64])
Inst redis-server (5:7.0.15-1build2 Ubuntu:24.04/noble [amd64])
Conf liblzf1 (3.6-4 Ubuntu:24.04/noble [amd64])
`

const aptRemoveSim = `NOTE: This is only a simulation!
The following packages were automatically installed and are no longer required:
  libnginx-mod-stream nginx-common old-leftover
Use 'apt autoremove' to remove them.
The following packages will be REMOVED:
  nginx
0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.
After this operation, 1,433 kB disk space will be freed.
Remv nginx [1.24.0-2ubuntu7.2]
`

func TestParseAptChangesInstall(t *testing.T) {
	cs := parseAptChanges([]byte(aptInstallSim))
	if len(cs) != 4 {
		t.Fatalf("got %d changes: %+v", len(cs), cs)
	}
	by := map[string]tunnel.SoftwareChange{}
	for _, c := range cs {
		by[c.Name] = c
	}
	if c := by["libjemalloc2"]; c.Kind != tunnel.ChangeUpgrade || c.From != "5.3.0-2" || c.To != "5.3.0-2build1" {
		t.Errorf("libjemalloc2 = %+v", c)
	}
	if c := by["redis-server"]; c.Kind != tunnel.ChangeAdd || c.From != "" || c.To != "5:7.0.15-1build2" {
		t.Errorf("redis-server = %+v", c)
	}
	dl, disk := parseAptSizes([]byte(aptInstallSim))
	if dl != 1580 || disk != 6234 {
		t.Errorf("sizes = %d %d", dl, disk)
	}
}

func TestParseAptRemove(t *testing.T) {
	cs := parseAptChanges([]byte(aptRemoveSim))
	if len(cs) != 1 || cs[0].Kind != tunnel.ChangeRemove || cs[0].From != "1.24.0-2ubuntu7.2" {
		t.Fatalf("got %+v", cs)
	}
	if got := parseAptNoLongerRequired([]byte(aptRemoveSim)); strings.Join(got, ",") != "libnginx-mod-stream,nginx-common,old-leftover" {
		t.Errorf("no longer required = %v", got)
	}
	if _, disk := parseAptSizes([]byte(aptRemoveSim)); disk != -1433 {
		t.Errorf("freed = %d", disk)
	}
	// A purge prints Purg after Remv for the same package: one change.
	cs = parseAptChanges([]byte("Remv nginx [1.24]\nPurg nginx [1.24]\n"))
	if len(cs) != 1 {
		t.Errorf("purge counted twice: %+v", cs)
	}
}

func TestParseAptPolicy(t *testing.T) {
	out := `nginx:
  Installed: (none)
  Candidate: 1.24.0-2ubuntu7.2
  Version table:
     1.24.0-2ubuntu7.2 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
     1.24.0-2ubuntu7 500
        500 http://archive.ubuntu.com/ubuntu noble/main amd64 Packages
postgresql-16:
  Installed: 16.4-1.pgdg24.04+1
  Candidate: 16.5-1.pgdg24.04+1
  Version table:
     16.5-1.pgdg24.04+1 500
        500 http://apt.postgresql.org/pub/repos/apt noble-pgdg/main amd64 Packages
 *** 16.4-1.pgdg24.04+1 100
        100 /var/lib/dpkg/status
`
	pol := parseAptPolicy([]byte(out))
	if p := pol["nginx"]; p.candidate != "1.24.0-2ubuntu7.2" || p.origin != "noble-updates/main" ||
		len(p.versions) != 2 || p.installed != "(none)" {
		t.Errorf("nginx = %+v", p)
	}
	if p := pol["postgresql-16"]; p.origin != "apt.postgresql.org noble-pgdg/main" || p.installed != "16.4-1.pgdg24.04+1" ||
		len(p.versions) != 2 {
		t.Errorf("postgresql-16 = %+v", p)
	}
}

const dnf4Install = `Last metadata expiration check: 0:12:03 ago.
Dependencies resolved.
================================================================================
 Package                  Architecture  Version              Repository    Size
================================================================================
Installing:
 redis                    x86_64        6.2.7-1.el9          appstream    1.3 M
Upgrading:
 openssl-libs             x86_64        1:3.0.7-28.el9       baseos       2.1 M
Installing dependencies:
 a-very-long-package-name-that-wraps-onto-its-own-line
                          noarch        1.0-1.el9            appstream     20 k

Transaction Summary
================================================================================
Install  2 Packages
Upgrade  1 Package

Total download size: 3.4 M
Installed size: 6.2 M
Operation aborted.
`

const dnf5Remove = `Package                Arch    Version                  Repository      Size
Removing:
 nginx                 x86_64  2:1.24.0-1.fc40          updates      1.2 MiB
Removing unused dependencies:
 nginx-filesystem      noarch  2:1.24.0-1.fc40          updates      0.0   B

Transaction Summary:
 Removing:           2 packages

After this operation, 1 MiB will be freed (install 0 B, remove 1 MiB).
Operation aborted by the user.
`

func TestParseDnfTransaction(t *testing.T) {
	cs := parseDnfTransaction([]byte(dnf4Install))
	if len(cs) != 3 {
		t.Fatalf("got %+v", cs)
	}
	if cs[0].Name != "redis" || cs[0].Kind != tunnel.ChangeAdd || cs[0].To != "6.2.7-1.el9" {
		t.Errorf("redis = %+v", cs[0])
	}
	if cs[1].Name != "openssl-libs" || cs[1].Kind != tunnel.ChangeUpgrade {
		t.Errorf("openssl-libs = %+v", cs[1])
	}
	if cs[2].Name != "a-very-long-package-name-that-wraps-onto-its-own-line" || cs[2].To != "1.0-1.el9" {
		t.Errorf("wrapped = %+v", cs[2])
	}
	if dl, disk := parseDnfSizes([]byte(dnf4Install)); dl != 3482 || disk != 6349 {
		t.Errorf("sizes = %d %d", dl, disk)
	}
	cs = parseDnfTransaction([]byte(dnf5Remove))
	if len(cs) != 2 || cs[0].Kind != tunnel.ChangeRemove || cs[0].From != "2:1.24.0-1.fc40" || cs[1].Name != "nginx-filesystem" {
		t.Errorf("dnf5 remove = %+v", cs)
	}
	if _, disk := parseDnfSizes([]byte(dnf5Remove)); disk != -1024 {
		t.Errorf("dnf5 freed = %d", disk)
	}
}

func TestParseZypperSoftware(t *testing.T) {
	sum := `<?xml version='1.0'?>
<stream>
<message type="info">Loading repository data...</message>
<install-summary download-size="1638400" space-usage-diff="6348800" packages-to-change="2">
<to-install>
<solvable type="package" name="redis" edition="7.2.4-1.1" arch="x86_64" repository="repo-oss"/>
</to-install>
<to-upgrade>
<solvable type="package" name="libopenssl3" edition="3.1.4-9.1" edition-old="3.1.4-8.1" arch="x86_64"/>
</to-upgrade>
</install-summary>
</stream>`
	cs, dl, disk, _, err := parseZypperSummary([]byte(sum))
	if err != nil || len(cs) != 2 || dl != 1600 || disk != 6200 {
		t.Fatalf("got %+v %d %d %v", cs, dl, disk, err)
	}
	if cs[1].Kind != tunnel.ChangeUpgrade || cs[1].From != "3.1.4-8.1" || cs[1].To != "3.1.4-9.1" {
		t.Errorf("upgrade = %+v", cs[1])
	}
	search := `<?xml version='1.0'?>
<stream><search-result version="0.0"><solvable-list>
<solvable status="not-installed" name="redis" summary="Persistent key-value store" kind="package" edition="7.2.4-1.1" arch="x86_64" repository="repo-oss"/>
<solvable status="not-installed" name="redis" summary="Persistent key-value store" kind="package" edition="7.2.5-1.1" arch="x86_64" repository="repo-update"/>
<solvable status="installed" name="htop" summary="htop" kind="package" edition="3.3.0-1.1" arch="x86_64" repository="(System Packages)"/>
</solvable-list></search-result></stream>`
	found, err := parseZypperSearch([]byte(search))
	if err != nil || len(found) != 1 || found[0].Version != "7.2.5-1.1" || found[0].Origin != "repo-update" ||
		len(found[0].Versions) != 2 {
		t.Fatalf("found = %+v %v", found, err)
	}
}

func TestParseRepoqueryAndUnits(t *testing.T) {
	rq := "redis\t6.2.7-1.el9\tappstream\tA key-value store\nredis\t7.2.5-1.el9\tappstream\tA key-value store\n" +
		"redis-doc\t6.2.7-1.el9\tappstream\tDocs\n"
	f := parseRepoquery([]byte(rq))
	if len(f) != 2 || f[0].Version != "7.2.5-1.el9" || f[0].Versions[0] != "7.2.5-1.el9" {
		t.Fatalf("repoquery = %+v", f)
	}
	units := serviceUnits([]byte("/usr/lib/systemd/system/redis.service\n/usr/lib/systemd/system/redis@.service\n" +
		"/etc/redis/redis.conf\n/lib/systemd/system/redis-sentinel.service\n/usr/share/doc/x.service\n"))
	if strings.Join(units, ",") != "redis-sentinel.service,redis.service" {
		t.Errorf("units = %v", units)
	}
}

/* ── Validation ──────────────────────────────────────────────────────────── */

func TestSoftwareChangeRefusesOptions(t *testing.T) {
	for _, it := range []tunnel.SoftwareItem{
		{Name: "-o"}, {Name: "--purge"}, {Name: "nginx; rm -rf /"}, {Name: "nginx", Version: "1.0 -y"},
		{Name: "nginx", Version: "--force"}, {Name: ""},
	} {
		if err := tunnel.ValidateSoftwareChange(tunnel.SoftwareInstall, []tunnel.SoftwareItem{it}, false); err == nil {
			t.Errorf("%+v was accepted", it)
		}
	}
	if err := tunnel.ValidateSoftwareChange(tunnel.SoftwareRemove, []tunnel.SoftwareItem{{Name: "nginx", Version: "1"}}, false); err == nil {
		t.Error("a removal took a version")
	}
	if err := tunnel.ValidateSoftwareChange(tunnel.SoftwareInstall, []tunnel.SoftwareItem{{Name: "nginx"}}, true); err == nil {
		t.Error("an install took purge")
	}
	q := tunnel.GrantSoftware{Op: tunnel.SoftwareSearch, Query: "--help"}
	if q.Validate() == nil {
		t.Error("a search for an option was accepted")
	}
}

/* ── Previews against a scripted apt ─────────────────────────────────────── */

// scriptedRun answers commands from a table of "command prefix" -> output,
// and records what ran.
type scriptedRun struct {
	mu    sync.Mutex
	table []scripted
	calls []string
}

type scripted struct {
	prefix string
	out    string
	code   int
}

func (s *scriptedRun) on(prefix, out string, code int) *scriptedRun {
	s.table = append(s.table, scripted{prefix, out, code})
	return s
}

func (s *scriptedRun) run(_ context.Context, out io.Writer, _ []string, name string, args ...string) (int, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	s.mu.Lock()
	s.calls = append(s.calls, cmd)
	s.mu.Unlock()
	for _, e := range s.table {
		if strings.HasPrefix(cmd, e.prefix) {
			if out != nil {
				_, _ = io.WriteString(out, e.out)
			}
			return e.code, nil
		}
	}
	return 0, nil
}

func (s *scriptedRun) ran(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func aptLook(name string) (string, error) {
	switch name {
	case "apt-get", "dpkg-query":
		return "/usr/bin/" + name, nil
	}
	return "", fmt.Errorf("no %s", name)
}

func TestPreviewInstallThatRemovesIsBlocked(t *testing.T) {
	r := (&scriptedRun{}).on("apt-get -s -q -o Debug::NoLocking=1 install -- exim4",
		"Remv postfix [3.8.6-1]\nInst exim4 (4.97-4 Ubuntu:24.04/noble [amd64])\n", 0)
	pv, _, err := previewSoftware(context.Background(), r.run, aptManager{}, tunnel.SoftwareInstall,
		[]tunnel.SoftwareItem{{Name: "exim4"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pv.Blocked, "would remove postfix") {
		t.Fatalf("blocked = %q", pv.Blocked)
	}
}

func TestPreviewRemoveProtected(t *testing.T) {
	r := &scriptedRun{}
	for _, name := range []string{"openssh-server", "argus-agent", "sudo", "systemd-resolved"} {
		pv, _, err := previewSoftware(context.Background(), r.run, aptManager{}, tunnel.SoftwareRemove,
			[]tunnel.SoftwareItem{{Name: name}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(pv.Blocked, "does not remove "+name) {
			t.Errorf("%s: blocked = %q", name, pv.Blocked)
		}
	}
	if r.ran("apt-get") {
		t.Error("a protected removal was simulated at all")
	}
	// A removal that drags a protected package with it is blocked too.
	r = (&scriptedRun{}).
		on("apt-get -s -q -o Debug::NoLocking=1 autoremove", "", 0).
		on("apt-get -s -q -o Debug::NoLocking=1 remove", "Remv foo [1]\nRemv sudo [1.9]\n", 0)
	pv, _, err := previewSoftware(context.Background(), r.run, aptManager{}, tunnel.SoftwareRemove,
		[]tunnel.SoftwareItem{{Name: "foo"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pv.Blocked, "would also remove sudo") {
		t.Errorf("blocked = %q", pv.Blocked)
	}
}

func TestPreviewRemoveTakesOnlyNewOrphans(t *testing.T) {
	r := (&scriptedRun{}).
		on("apt-get -s -q -o Debug::NoLocking=1 autoremove", "Remv old-leftover [1.0]\n", 0).
		on("apt-get -s -q -o Debug::NoLocking=1 remove -- nginx nginx-common libnginx-mod-stream",
			"Remv nginx [1.24]\nRemv nginx-common [1.24]\nRemv libnginx-mod-stream [1.24]\n", 0).
		on("apt-get -s -q -o Debug::NoLocking=1 remove -- nginx", aptRemoveSim, 0)
	pv, targets, err := previewSoftware(context.Background(), r.run, aptManager{}, tunnel.SoftwareRemove,
		[]tunnel.SoftwareItem{{Name: "nginx"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(targets, " ") != "nginx libnginx-mod-stream nginx-common" &&
		strings.Join(targets, " ") != "nginx nginx-common libnginx-mod-stream" {
		t.Errorf("targets = %v", targets)
	}
	for _, tg := range targets {
		if tg == "old-leftover" {
			t.Error("an orphan from before was taken along")
		}
	}
	if pv.Blocked != "" || !pv.PurgeAllowed || len(pv.Hash) != 64 {
		t.Errorf("preview = %+v", pv)
	}
	if !pv.Changes[0].Asked {
		t.Errorf("nginx not marked asked: %+v", pv.Changes)
	}
}

func TestPreviewPurgeRefusedForData(t *testing.T) {
	r := (&scriptedRun{}).
		on("apt-get -s -q -o Debug::NoLocking=1 autoremove", "", 0).
		on("apt-get -s -q -o Debug::NoLocking=1 remove", "Remv postgresql-16 [16.4]\n", 0)
	pv, _, err := previewSoftware(context.Background(), r.run, aptManager{}, tunnel.SoftwareRemove,
		[]tunnel.SoftwareItem{{Name: "postgresql-16"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if pv.PurgeAllowed || !strings.Contains(pv.Blocked, "Purging is not offered") {
		t.Errorf("preview = %+v", pv)
	}
}

func TestPreviewHashFollowsTheAnswer(t *testing.T) {
	a := tunnel.SoftwarePreviewResult{Action: "install", Changes: []tunnel.SoftwareChange{{Kind: "add", Name: "x", To: "1"}}}
	b := a
	b.Changes = []tunnel.SoftwareChange{{Kind: "add", Name: "x", To: "2"}}
	if changesHash(a) == changesHash(b) {
		t.Error("a different version hashes the same")
	}
	c := a
	c.Changes = []tunnel.SoftwareChange{{Kind: "add", Name: "x", To: "1", Asked: true}}
	if changesHash(a) != changesHash(c) {
		t.Error("the hash depends on presentation")
	}
}

/* ── Runs ────────────────────────────────────────────────────────────────── */

func softwareApplier(t *testing.T, r *scriptedRun, req PackageRunRequest) *PackageApplier {
	t.Helper()
	dir := t.TempDir()
	lists := filepath.Join(dir, "lists")
	if err := os.MkdirAll(lists, 0o755); err != nil {
		t.Fatal(err)
	}
	old := aptListsDir
	aptListsDir = lists
	t.Cleanup(func() { aptListsDir = old })
	rcd := policyRCD
	policyRCD = filepath.Join(dir, "policy-rc.d")
	t.Cleanup(func() { policyRCD = rcd })
	req.ID = "run-00000001"
	return &PackageApplier{
		Plan:           PackagePlan{PackageRunRequest: req, StateDir: dir, Unit: "argus-packages-test"},
		PackageChecker: PackageChecker{Run: r.run, Look: aptLook, Now: time.Now},
		Restart:        func(context.Context, string) error { return nil },
		Reboot:         func() error { return nil },
		BootID:         func() string { return "boot" },
	}
}

func TestSoftwareRunStopsWhenTheServerChanged(t *testing.T) {
	r := (&scriptedRun{}).on("apt-get -s -q -o Debug::NoLocking=1 install -- redis-server", aptInstallSim, 0)
	a := softwareApplier(t, r, PackageRunRequest{Mode: PackagesInstall,
		Items: []tunnel.SoftwareItem{{Name: "redis-server"}}, Expect: strings.Repeat("0", 64), StartServices: true})
	st := a.Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "changed since this was previewed") {
		t.Fatalf("state = %s %q", st.State, st.Message)
	}
	if r.ran("apt-get -o DPkg::Lock::Timeout=120 -y") || r.ran("apt-get install") {
		t.Fatal("the package manager ran after the preview no longer matched")
	}
}

func TestSoftwareRunInstallsWhatWasApproved(t *testing.T) {
	r := (&scriptedRun{}).on("apt-get -s -q -o Debug::NoLocking=1 install -- redis-server", aptInstallSim, 0)
	pv, _, err := previewSoftware(context.Background(), r.run, aptManager{}, tunnel.SoftwareInstall,
		[]tunnel.SoftwareItem{{Name: "redis-server"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	r.on("dpkg-query -W -f=${db:Status-Abbrev}", "ii \tredis-server\t5:7.0.15-1build2\tamd64\t1000\tredis\n"+
		"ii \tredis-tools\t5:7.0.15-1build2\tamd64\t1000\ttools\nii \tliblzf1\t3.6-4\tamd64\t10\tlzf\n"+
		"ii \tlibjemalloc2\t5.3.0-2build1\tamd64\t10\tjemalloc\n", 0).
		on("dpkg -L -- liblzf1 redis-tools redis-server", "/lib/systemd/system/redis-server.service\n", 0)
	a := softwareApplier(t, r, PackageRunRequest{Mode: PackagesInstall,
		Items: []tunnel.SoftwareItem{{Name: "redis-server"}}, Expect: pv.Hash, StartServices: false})
	st := a.Apply(context.Background())
	if st.State != RunDone {
		t.Fatalf("state = %s %q\n%s", st.State, st.Message, strings.Join(r.calls, "\n"))
	}
	if !r.ran("apt-get -o DPkg::Lock::Timeout=120 -y -q") {
		t.Fatalf("apt-get install never ran:\n%s", strings.Join(r.calls, "\n"))
	}
	if len(st.Upgraded) != 4 {
		t.Errorf("changes = %+v", st.Upgraded)
	}
	if !r.ran("systemctl disable --now redis-server.service") || r.ran("systemctl enable") {
		t.Errorf("asked to keep services stopped:\n%s", strings.Join(r.calls, "\n"))
	}
	if _, err := os.Stat(policyRCD); !os.IsNotExist(err) {
		t.Error("policy-rc.d was left behind")
	}
}

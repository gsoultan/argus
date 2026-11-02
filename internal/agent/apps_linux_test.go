//go:build linux

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDecodeProcAddr(t *testing.T) {
	for in, want := range map[string]string{
		"0100007F:1538":                         "127.0.0.1:5432",
		"00000000:0016":                         "0.0.0.0:22",
		"00000000000000000000000000000000:1538": ":::5432",
		"00000000000000000000000001000000:18EB": "::1:6379",
		"0000000000000000FFFF00000100007F:1538": "127.0.0.1:5432",
	} {
		addr, port, ok := decodeProcAddr(in)
		got := addr + ":" + itoa(port)
		if !ok || got != want {
			t.Errorf("%s = %s, want %s", in, got, want)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestUnitOfCgroup(t *testing.T) {
	for in, want := range map[string]string{
		"0::/system.slice/postgresql@16-main.service\n":           "postgresql@16-main.service",
		"12:pids:/\n1:name=systemd:/system.slice/nginx.service\n": "nginx.service",
		"0::/user.slice/user-1000.slice/session-3.scope\n":        "",
		"0::/system.slice/docker-0123.scope\n":                    "",
	} {
		if got := unitOfCgroup(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

// A scan of a fake /proc names the process behind a listening socket, from
// its fd table, without reading its command line.
func TestListeningSocketsNamesTheProcess(t *testing.T) {
	proc := t.TempDir()
	write := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(proc, p)), 0o755)
		if err := os.WriteFile(filepath.Join(proc, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("net/tcp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   0: 00000000:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000   112        0 4242 1 0 100 0 0 10 0\n"+
		"   1: 0100007F:1538 0100007F:9C40 01 00000000:00000000 00:00000000 00000000   112        0 4343 1 0 100 0 0 10 0\n")
	write("77/comm", "redis-server\n")
	write("77/cmdline", "redis-server\x00--requirepass\x00hunter2\x00")
	write("77/cgroup", "0::/system.slice/redis-server.service\n")
	_ = os.MkdirAll(filepath.Join(proc, "77/fd"), 0o755)
	if err := os.Symlink("socket:[4242]", filepath.Join(proc, "77/fd/5")); err != nil {
		t.Fatal(err)
	}

	ls, ok := listeningSockets(proc)
	if !ok || len(ls) != 1 {
		t.Fatalf("listeners = %+v", ls)
	}
	l := ls[0]
	if l.Address != "0.0.0.0" || l.Port != 6379 || l.Process != "redis-server" || l.Unit != "redis-server.service" {
		t.Errorf("listener = %+v", l)
	}
	if b, _ := json.Marshal(ls); strings.Contains(string(b), "hunter2") {
		t.Errorf("the scan carried a password from the command line: %s", b)
	}
}

func TestParseDpkgStatusKeepsInstalledServers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status")
	_ = os.WriteFile(p, []byte(`Package: postgresql-16
Status: install ok installed
Version: 16.4-1

Package: nginx
Status: deinstall ok config-files
Version: 1.24

Package: libc6
Status: install ok installed
Version: 2.39

Package: redis-server
Status: install ok installed
Description: Persistent key-value database
 with network interface
Version: 5:7.0.15
`), 0o644)
	pkgs, err := parseStanzas(p, "Package", "Version", "Status")
	if err != nil || len(pkgs) != 2 || pkgs[0] != (installedPackage{"postgresql-16", "16.4-1"}) ||
		pkgs[1] != (installedPackage{"redis-server", "5:7.0.15"}) {
		t.Fatalf("%v %+v", err, pkgs)
	}
}

func TestRunningServicesSkipsEmptyCgroups(t *testing.T) {
	cg := t.TempDir()
	for name, events := range map[string]string{
		"nginx.service": "populated 1\nfrozen 0\n", "setup.service": "populated 0\nfrozen 0\n",
	} {
		d := filepath.Join(cg, "system.slice", name)
		_ = os.MkdirAll(d, 0o755)
		_ = os.WriteFile(filepath.Join(d, "cgroup.events"), []byte(events), 0o644)
	}
	got, ok := runningServices(cg)
	if !ok || len(got) != 1 || got[0] != "nginx.service" {
		t.Fatalf("%v %v", ok, got)
	}
}

// A daemon with no TCP port (dockerd) is found running from its name, and a
// container's Postgres, visible in the host's /proc, is not taken for the
// host's own.
func TestCatalogueProcessesSkipsContainers(t *testing.T) {
	proc := t.TempDir()
	for pid, files := range map[string][2]string{
		"10": {"dockerd\n", "0::/system.slice/docker.service\n"},
		"20": {"postgres\n", "0::/docker/ec11946e17d1\n"},
		"30": {"bash\n", "0::/user.slice\n"},
	} {
		_ = os.MkdirAll(filepath.Join(proc, pid), 0o755)
		_ = os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(files[0]), 0o644)
		_ = os.WriteFile(filepath.Join(proc, pid, "cgroup"), []byte(files[1]), 0o644)
	}
	got := catalogueProcesses(proc)
	if len(got) != 1 || got[0] != "dockerd" {
		t.Fatalf("got %v", got)
	}
}

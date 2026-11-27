//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fixtureRoots(t *testing.T, files map[string]string, sizes map[string][3]uint64, markers ...string) sysRoots {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, files)
	return sysRoots{
		proc: filepath.Join(root, "proc"), sys: filepath.Join(root, "sys"), etc: filepath.Join(root, "etc"),
		statfs: func(p string) (uint64, uint64, uint64, bool) {
			s, ok := sizes[p]
			return s[0], s[1], s[2], ok
		},
		exists: func(p string) bool {
			for _, m := range markers {
				if m == p {
					return true
				}
			}
			return false
		},
	}
}

const cpuinfoXeon = `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) Gold 6248R CPU @ 3.00GHz
flags		: fpu vme sse2 hypervisor avx2

processor	: 1
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) Gold 6248R CPU @ 3.00GHz
flags		: fpu vme sse2 hypervisor avx2
`

func TestReadSystemOnAVirtualMachine(t *testing.T) {
	r := fixtureRoots(t, map[string]string{
		"proc/sys/kernel/osrelease":     "6.8.0-45-generic\n",
		"proc/cpuinfo":                  cpuinfoXeon,
		"proc/meminfo":                  "MemTotal:       32768000 kB\nMemFree: 1 kB\n",
		"proc/stat":                     "cpu  1 2 3 4 5 6 7 8 0 0\nbtime 1728638040\n",
		"sys/class/dmi/id/sys_vendor":   "QEMU\n",
		"sys/class/dmi/id/product_name": "Standard PC (Q35 + ICH9, 2009)\n",
		"etc/timezone":                  "Asia/Jakarta\n",
	}, nil)
	s := readSystem(r)
	if s.Kernel != "6.8.0-45-generic" {
		t.Errorf("kernel = %q", s.Kernel)
	}
	if s.CPUModel != "Intel Xeon Gold 6248R" || s.CPUs != 2 {
		t.Errorf("cpu = %q × %d", s.CPUModel, s.CPUs)
	}
	if s.MemoryBytes != 32768000*1024 {
		t.Errorf("memory = %d", s.MemoryBytes)
	}
	if s.Machine != "virtual" || s.Hypervisor != "KVM" {
		t.Errorf("machine = %q %q", s.Machine, s.Hypervisor)
	}
	if s.BootedAt == nil || !s.BootedAt.Equal(time.Unix(1728638040, 0)) {
		t.Errorf("booted = %v", s.BootedAt)
	}
	if s.TimeZone != "Asia/Jakarta" {
		t.Errorf("zone = %q", s.TimeZone)
	}
}

func TestReadSystemDoesNotGuessPhysical(t *testing.T) {
	// No DMI and no hypervisor flag: an ARM board, or a sandbox hiding
	// /sys. Unknown is not physical.
	r := fixtureRoots(t, map[string]string{"proc/cpuinfo": "processor : 0\n"}, nil)
	if s := readSystem(r); s.Machine != "" {
		t.Errorf("machine = %q, want unknown", s.Machine)
	}
	r = fixtureRoots(t, map[string]string{
		"proc/cpuinfo":                "processor : 0\nflags : fpu sse2\n",
		"sys/class/dmi/id/sys_vendor": "Dell Inc.\n",
	}, nil)
	if s := readSystem(r); s.Machine != "physical" {
		t.Errorf("machine = %q, want physical", s.Machine)
	}
	r = fixtureRoots(t, map[string]string{"proc/cpuinfo": "processor : 0\n"}, nil, "/.dockerenv")
	if s := readSystem(r); s.Machine != "container" {
		t.Errorf("machine = %q, want container", s.Machine)
	}
}

func TestReadUsage(t *testing.T) {
	mountinfo := strings.Join([]string{
		`22 1 252:1 / / rw,relatime - ext4 /dev/vda1 rw`,
		`23 22 0:21 / /proc rw - proc proc rw`,
		`24 22 0:5 / /dev rw - devtmpfs udev rw`,
		`25 22 252:17 / /var/lib/postgresql rw - xfs /dev/vdb1 rw`,
		`26 22 7:0 / /snap/core/1 ro - squashfs /dev/loop0 ro`,
		`27 22 0:30 / /var/lib/docker/overlay2/x/merged rw - overlay overlay rw`,
		`28 22 252:1 /home /home rw - ext4 /dev/vda1 rw`,
		`29 22 252:33 / /mnt/my\040data rw - ext4 /dev/vdc1 rw`,
	}, "\n") + "\n"
	r := fixtureRoots(t, map[string]string{
		"proc/loadavg":        "1.42 1.30 1.18 2/345 6789\n",
		"proc/stat":           "cpu  100 0 100 700 100 0 0 0 0 0\n",
		"proc/meminfo":        "MemTotal: 1000 kB\nMemAvailable: 400 kB\nSwapTotal: 100 kB\nSwapFree: 90 kB\n",
		"proc/self/mountinfo": mountinfo,
	}, map[string][3]uint64{
		"/":                   {100, 53, 48},
		"/home":               {100, 53, 48},
		"/var/lib/postgresql": {500, 45, 45},
		"/mnt/my data":        {10, 9, 9},
	})

	u, first := readUsage(r, cpuTimes{})
	if u.CPUBusy != nil {
		t.Errorf("first sample claims %v%% busy with nothing to compare", *u.CPUBusy)
	}
	if u.Load == nil || *u.Load != [3]float64{1.42, 1.30, 1.18} {
		t.Errorf("load = %v", u.Load)
	}
	if u.MemTotal != 1000*1024 || u.MemAvailable != 400*1024 || u.SwapFree != 90*1024 {
		t.Errorf("memory = %+v", u)
	}
	var mounts []string
	for _, d := range u.Disks {
		mounts = append(mounts, d.Mount)
	}
	if got := strings.Join(mounts, ","); got != "/,/mnt/my data,/var/lib/postgresql" {
		t.Errorf("disks = %s", got)
	}
	if pg := u.Disks[2]; pg.Used != 455 || pg.Avail != 45 || pg.FS != "xfs" {
		t.Errorf("postgres disk = %+v", pg)
	}

	// 200 more jiffies, 50 of them idle: 75% busy.
	r2 := r
	writeTree(t, filepath.Dir(r.proc), map[string]string{"proc/stat": "cpu  200 0 150 750 100 0 0 0 0 0\n"})
	u2, _ := readUsage(r2, first)
	if u2.CPUBusy == nil || *u2.CPUBusy != 75 {
		t.Errorf("busy = %v, want 75", u2.CPUBusy)
	}
}

func TestReadNetDev(t *testing.T) {
	dev := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo: 9000 10 0 0 0 0 0 0 9000 10 0 0 0 0 0 0\n" +
		"  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0\n" +
		"  eth1: 50 1 0 0 0 0 0 0 60 1 0 0 0 0 0 0\n" +
		"  wg0: 7 1 0 0 0 0 0 0 8 1 0 0 0 0 0 0\n"
	r := fixtureRoots(t, map[string]string{
		"proc/net/dev":                 dev,
		"sys/class/net/eth0/operstate": "up\n",
		"sys/class/net/eth0/speed":     "1000\n",
		"sys/class/net/eth1/operstate": "down\n",
		"sys/class/net/wg0/operstate":  "unknown\n",
		"sys/class/net/wg0/speed":      "-1\n",
	}, nil)
	got := readNetDev(r)
	if len(got) != 2 || got[0].name != "eth0" || got[1].name != "wg0" {
		t.Fatalf("interfaces = %+v, want eth0 and wg0 (lo and the down eth1 left out)", got)
	}
	if got[0].rx != 1000 || got[0].tx != 2000 || got[0].speedMbps != 1000 {
		t.Errorf("eth0 = %+v", got[0])
	}
	if got[1].speedMbps != 0 {
		t.Errorf("wg0 speed = %d, want 0 for a link that states none", got[1].speedMbps)
	}
}

func TestCleanCPUModel(t *testing.T) {
	for in, want := range map[string]string{
		"Intel(R) Xeon(R) Gold 6248R CPU @ 3.00GHz": "Intel Xeon Gold 6248R",
		"Intel(R) Core(TM) i7-8700 CPU @ 3.20GHz":   "Intel Core i7-8700",
		"AMD EPYC 7B13 64-Core Processor":           "AMD EPYC 7B13 64-Core Processor",
		"QEMU Virtual CPU version 2.5+":             "QEMU Virtual CPU version 2.5+",
	} {
		if got := cleanCPUModel(in); got != want {
			t.Errorf("cleanCPUModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadUsageStealAndConnections(t *testing.T) {
	r := fixtureRoots(t, map[string]string{
		"proc/stat":          "cpu  100 0 100 700 0 0 0 100 0 0\n",
		"proc/meminfo":       "MemTotal: 1024 kB\nMemAvailable: 512 kB\n",
		"proc/loadavg":       "0.50 0.40 0.30 1/100 123\n",
		"proc/net/sockstat":  "sockets: used 73\nTCP: inuse 26 orphan 0 tw 4 alloc 26 mem 1\nUDP: inuse 3 mem 0\n",
		"proc/net/sockstat6": "TCP6: inuse 4\nUDP6: inuse 1\n",
	}, nil)
	_, prev := readUsage(r, cpuTimes{})
	writeTree(t, filepath.Dir(r.proc), map[string]string{"proc/stat": "cpu  150 0 150 800 0 0 0 200 0 0\n"})
	u, _ := readUsage(r, prev)
	// 300 ticks passed, 100 of them stolen.
	if u.Steal == nil || *u.Steal != 33.3 {
		t.Errorf("steal = %v, want 33.3", u.Steal)
	}
	if u.TCPConns == nil || *u.TCPConns != 30 {
		t.Errorf("tcp = %v, want 26 + 4", u.TCPConns)
	}

	// No sockstat at all is not reported, never zero.
	bare := fixtureRoots(t, map[string]string{"proc/stat": "cpu  1 0 1 1 0 0 0 0\n"}, nil)
	if u, _ := readUsage(bare, cpuTimes{}); u.TCPConns != nil || u.Steal != nil {
		t.Errorf("bare = tcp %v steal %v, want neither", u.TCPConns, u.Steal)
	}
}

func TestReadDiskStatsWholeDisksOnly(t *testing.T) {
	r := fixtureRoots(t, map[string]string{
		"proc/diskstats": `   7       0 loop0 10 0 80 0 0 0 0 0 0 0 0
 259       0 nvme0n1 1000 5 20000 300 400 8 8000 90 0 400 390
 259       1 nvme0n1p1 900 5 18000 280 380 8 7600 80 0 380 360
 253       0 dm-0 900 0 18000 280 380 0 7600 80 0 380 360
   8       0 sda 7 0 bad 0 0 0 0 0 0 0 0
`,
	}, nil)
	// sysfs lists the whole disks (and dm-0 and loop0, which are still left out).
	sys := r.sys
	r.exists = func(p string) bool {
		switch p {
		case filepath.Join(sys, "block/nvme0n1"), filepath.Join(sys, "block/dm-0"), filepath.Join(sys, "block/loop0"), filepath.Join(sys, "block/sda"):
			return true
		}
		return false
	}
	got := readDiskStats(r)
	if len(got) != 1 {
		t.Fatalf("disks = %+v, want nvme0n1 alone (no partition, mapper, loop or unreadable line)", got)
	}
	d := got[0]
	if d.name != "nvme0n1" || d.reads != 1000 || d.readBytes != 20000*512 || d.writes != 400 || d.wroteBytes != 8000*512 {
		t.Errorf("nvme0n1 = %+v", d)
	}
}

func TestParseProcStat(t *testing.T) {
	// A name with a space and a bracket in it, which is why fields are
	// counted from the last closing bracket.
	line := "4242 (my (odd) prog) S 1 4242 4242 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 3 0 98765 123456789 2048 18446744073709551615\n"
	c, ok := parseProcStat(4242, line, 4096)
	if !ok {
		t.Fatal("not parsed")
	}
	if c.name != "my (odd) prog" || c.cpu != 300 || c.key.start != 98765 || c.memory != 2048*4096 {
		t.Errorf("parsed %+v", c)
	}
	if _, ok := parseProcStat(1, "1 (init) S 1 2", 4096); ok {
		t.Error("a cut-off line parsed")
	}
}

func TestReadProcsSkipsWhatIsNotAProcess(t *testing.T) {
	r := fixtureRoots(t, map[string]string{
		"proc/12/stat":   "12 (postgres) S 1 12 12 0 -1 0 0 0 0 0 10 5 0 0 20 0 1 0 500 0 100 0\n",
		"proc/self/stat": "12 (postgres) S 1 12 12 0 -1 0 0 0 0 0 10 5 0 0 20 0 1 0 500 0 100 0\n",
		"proc/12/io":     "rchar: 9000\nwchar: 8000\nsyscr: 1\nsyscw: 2\nread_bytes: 4096\nwrite_bytes: 8192\ncancelled_write_bytes: 0\n",
		"proc/13/stat":   "garbage",
		"proc/14/stat":   "14 (sshd) S 1 14 14 0 -1 0 0 0 0 0 1 1 0 0 20 0 1 0 600 0 10 0\n",
		"proc/stat":      "cpu 1 1 1 1\n",
	}, nil)
	got := readProcs(r, 4096)
	sort.Slice(got, func(i, j int) bool { return got[i].key.pid < got[j].key.pid })
	if len(got) != 2 || got[0].key.pid != 12 || got[0].name != "postgres" {
		t.Fatalf("procs = %+v", got)
	}
	if !got[0].io || got[0].read != 4096 || got[0].wrote != 8192 {
		t.Errorf("postgres io = %+v, want storage bytes, not rchar/wchar", got[0])
	}
	if got[1].io || got[1].denied {
		t.Errorf("a process with no io file = %+v, want unread, not refused", got[1])
	}
}

func TestParseProcIO(t *testing.T) {
	r, w, ok := parseProcIO([]byte("rchar: 5\nread_bytes: 12\nwrite_bytes: 34\n"))
	if !ok || r != 12 || w != 34 {
		t.Errorf("parsed %d %d %v", r, w, ok)
	}
	if _, _, ok := parseProcIO([]byte("rchar: 5\nread_bytes: 12\n")); ok {
		t.Error("a file without write_bytes parsed")
	}
}

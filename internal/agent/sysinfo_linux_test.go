//go:build linux

package agent

import (
	"os"
	"path/filepath"
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
	if u.Load != [3]float64{1.42, 1.30, 1.18} {
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

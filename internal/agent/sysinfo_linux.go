//go:build linux

package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sysRoots is where the Linux readers look, so a test can point them at a
// fixture tree instead of the running kernel.
type sysRoots struct {
	proc, sys, etc string
	// statfs reads a filesystem's size; syscall.Statfs outside tests.
	statfs func(path string) (total, free, avail uint64, ok bool)
	// exists reports a file's presence, for the container markers.
	exists func(path string) bool
}

var liveRoots = sysRoots{proc: "/proc", sys: "/sys", etc: "/etc", statfs: statfsLive, exists: fileExists}

func platformSystem() SystemInfo { return readSystem(liveRoots) }

func (u *UsageSampler) platformSample() (Usage, bool) {
	out, now := readUsage(liveRoots, u.prev)
	u.prev = now
	out.Net = u.netRates(readNetDev(liveRoots), out.At)
	return out, true
}

func readSystem(r sysRoots) SystemInfo {
	var s SystemInfo
	s.Kernel = fileFirstLine(filepath.Join(r.proc, "sys/kernel/osrelease"))
	if a := fileFirstLine(filepath.Join(r.proc, "sys/kernel/arch")); a != "" {
		s.Arch = a
	}

	model, cpus, hvFlag := readCPUInfo(filepath.Join(r.proc, "cpuinfo"))
	s.CPUModel, s.CPUs = model, cpus

	mem := readMeminfo(filepath.Join(r.proc, "meminfo"))
	s.MemoryBytes = mem["MemTotal"]

	if bt := statField(filepath.Join(r.proc, "stat"), "btime"); bt > 0 {
		t := time.Unix(int64(bt), 0).UTC()
		s.BootedAt = &t
	}

	vendor := fileFirstLine(filepath.Join(r.sys, "class/dmi/id/sys_vendor"))
	product := fileFirstLine(filepath.Join(r.sys, "class/dmi/id/product_name"))
	switch {
	case r.exists("/.dockerenv") || r.exists("/run/.containerenv"):
		s.Machine = "container"
	case hypervisorFromDMI(vendor, product) != "":
		s.Machine, s.Hypervisor = "virtual", hypervisorFromDMI(vendor, product)
	case hvFlag:
		s.Machine = "virtual"
	case vendor != "":
		// DMI names real hardware and the processor does not report a
		// hypervisor. Only said where both were readable.
		s.Machine = "physical"
	}

	s.TimeZone = timeZoneName(r.etc)
	return s
}

// readCPUInfo returns the processor's model name, the logical processor
// count, and whether the processor flags a hypervisor.
func readCPUInfo(path string) (model string, cpus int, hypervisor bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			cpus++
		case "model name", "Model", "cpu model":
			if model == "" {
				model = v
			}
		case "flags":
			if !hypervisor {
				for _, fl := range strings.Fields(v) {
					if fl == "hypervisor" {
						hypervisor = true
						break
					}
				}
			}
		}
	}
	return cleanCPUModel(model), cpus, hypervisor
}

// readMeminfo reads /proc/meminfo into bytes.
func readMeminfo(path string) map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			n *= 1024
		}
		out[k] = n
	}
	return out
}

// statField reads one single-number line of /proc/stat, such as btime.
func statField(path, name string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == name {
			n, _ := strconv.ParseUint(fields[1], 10, 64)
			return n
		}
	}
	return 0
}

// readCPUTimes reads the aggregate "cpu" line of /proc/stat. Idle counts
// iowait too: a processor waiting on a disk is not doing work.
func readCPUTimes(path string) cpuTimes {
	f, err := os.Open(path)
	if err != nil {
		return cpuTimes{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuTimes{}
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}
	}
	var t cpuTimes
	// user nice system idle iowait irq softirq steal; guest time is already
	// counted in user, so the fields after steal are left out.
	for i, f := range fields[1:] {
		if i >= 8 {
			break
		}
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return cpuTimes{}
		}
		t.total += n
		if i == 3 || i == 4 {
			t.idle += n
		}
	}
	return t
}

func readUsage(r sysRoots, prev cpuTimes) (Usage, cpuTimes) {
	u := Usage{At: time.Now().UTC(), Disks: []Disk{}}
	if b, err := os.ReadFile(filepath.Join(r.proc, "loadavg")); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 3 {
			var load [3]float64
			for i := range load {
				load[i], _ = strconv.ParseFloat(fields[i], 64)
			}
			u.Load = &load
		}
	}
	now := readCPUTimes(filepath.Join(r.proc, "stat"))
	u.CPUBusy = busySince(prev, now)

	mem := readMeminfo(filepath.Join(r.proc, "meminfo"))
	u.MemTotal = mem["MemTotal"]
	if v, ok := mem["MemAvailable"]; ok {
		u.MemAvailable = v
	} else {
		// Kernels before 3.14 have no MemAvailable; free plus page cache is
		// the usual stand-in.
		u.MemAvailable = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}
	u.SwapTotal, u.SwapFree = mem["SwapTotal"], mem["SwapFree"]
	u.Disks = readDisks(r)
	return u, now
}

// readNetDev reads each interface's byte counters from /proc/net/dev, for
// the interfaces that are up and not loopback, with the link speed sysfs
// states. A virtual interface states none, or -1, which is reported as 0.
func readNetDev(r sysRoots) []netCounters {
	f, err := os.Open(filepath.Join(r.proc, "net/dev"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []netCounters
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// "  eth0: rxbytes rxpackets ... (8 rx fields) txbytes ..."
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		// The name goes into a sysfs path; the kernel never writes one with a
		// slash, and one that did would not be read.
		if name == "" || name == "lo" || name == "." || name == ".." || strings.Contains(name, "/") || len(fields) < 9 {
			continue
		}
		if !netUp(r, name) {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		c := netCounters{name: name, rx: rx, tx: tx}
		if v, err := strconv.ParseInt(fileFirstLine(filepath.Join(r.sys, "class/net", name, "speed")), 10, 64); err == nil && v > 0 {
			c.speedMbps = uint64(v)
		}
		out = append(out, c)
		if len(out) == maxNet {
			break
		}
	}
	return out
}

// netUp is whether sysfs says the interface is up. "unknown" counts as up:
// tunnels and some virtual NICs never report anything else while carrying
// traffic. No sysfs entry at all (a test tree, an odd kernel) counts too.
func netUp(r sysRoots, name string) bool {
	switch fileFirstLine(filepath.Join(r.sys, "class/net", name, "operstate")) {
	case "down", "lowerlayerdown", "notpresent", "dormant":
		return false
	}
	return true
}

// realFS are filesystem types that hold data on a device. Everything else in
// the mount table (proc, tmpfs, overlay, cgroup, squashfs for snaps...) is
// either virtual or read-only packaging, and would bury the disks that can
// fill up.
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "nfs": true, "nfs4": true,
	"cifs": true, "smb3": true, "ceph": true, "glusterfs": true, "fuseblk": true,
}

// skippedMounts are trees whose mounts belong to something else: a
// container runtime's layers, a snap's loop devices, kubelet's volumes.
var skippedMounts = []string{"/var/lib/docker/", "/var/lib/containers/", "/var/lib/kubelet/",
	"/snap/", "/run/", "/proc/", "/sys/", "/dev/"}

// readDisks lists real filesystems from the mount table, one per device
// (a btrfs volume mounted at several subvolumes is one disk), fullest
// first, bounded.
func readDisks(r sysRoots) []Disk {
	f, err := os.Open(filepath.Join(r.proc, "self/mountinfo"))
	if err != nil {
		return []Disk{}
	}
	defer f.Close()

	byDev := map[string]Disk{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// id parent major:minor root mount opts... - fstype source superopts
		left, right, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		lf, rf := strings.Fields(left), strings.Fields(right)
		if len(lf) < 5 || len(rf) < 1 {
			continue
		}
		dev, mount, fs := lf[2], unescapeMount(lf[4]), rf[0]
		if !realFS[fs] || skipMount(mount) {
			continue
		}
		total, free, avail, ok := r.statfs(mount)
		if !ok || total == 0 {
			continue
		}
		d := Disk{Mount: mount, FS: fs, Total: total, Used: total - free, Avail: avail}
		if prev, seen := byDev[dev]; seen && len(prev.Mount) <= len(mount) {
			continue
		}
		byDev[dev] = d
	}
	out := make([]Disk, 0, len(byDev))
	for _, d := range byDev {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mount == "/" || out[j].Mount == "/" {
			return out[i].Mount == "/"
		}
		return out[i].Mount < out[j].Mount
	})
	if len(out) > maxDisks {
		out = out[:maxDisks]
	}
	return out
}

func skipMount(m string) bool {
	for _, p := range skippedMounts {
		if strings.HasPrefix(m+"/", p) {
			return true
		}
	}
	return false
}

// unescapeMount undoes mountinfo's octal escapes for space, tab, newline
// and backslash.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func statfsLive(path string) (total, free, avail uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, false
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bfree * bs, st.Bavail * bs, true
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// timeZoneName is the host's zone: /etc/timezone where Debian keeps it,
// else the zoneinfo path /etc/localtime links to.
func timeZoneName(etc string) string {
	if tz := fileFirstLine(filepath.Join(etc, "timezone")); tz != "" {
		return tz
	}
	if target, err := os.Readlink(filepath.Join(etc, "localtime")); err == nil {
		if _, after, ok := strings.Cut(target, "zoneinfo/"); ok {
			return after
		}
	}
	return ""
}

func fileFirstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

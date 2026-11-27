package agent

import (
	"math"
	"runtime"
	"sort"
	"strings"
	"time"
)

/*
What a server is and how busy it is, for the System card on its page.

Two halves with different lifetimes. SystemInfo (kernel, processor, memory
size, how the machine is hosted, when it booted, its time zone) changes on a
reboot or an upgrade, so it rides the host facts report. Usage (load, memory
in use, swap, how full each disk is) changes all the time, so it rides the
heartbeat, about once a minute.

Read-only and nothing a local user could not already see: /proc, /sys's DMI
names, the mount table, statfs and the interface table. One exception: each
process's /proc/<pid>/io, which the kernel shows only to its owner or to a
reader holding CAP_SYS_PTRACE, for disk activity by program. Its two byte
counters and nothing else: no command line, environment or memory of any
process, no file content, no configuration. A mount point is a path, not
what is stored under it.
*/

// SystemInfo is what kind of machine this is.
type SystemInfo struct {
	// Kernel is the running kernel's release, as `uname -r` prints it.
	Kernel string `json:"kernel,omitempty"`
	// Arch is the machine architecture, as `uname -m` prints it.
	Arch string `json:"arch,omitempty"`
	// CPUModel is the processor's name, where the kernel states one.
	CPUModel string `json:"cpu_model,omitempty"`
	// CPUs is the number of logical processors the kernel schedules on.
	CPUs int `json:"cpus,omitempty"`
	// MemoryBytes is the memory the kernel manages.
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
	// Machine is how the host runs: "physical", "virtual" or "container".
	// Empty when the agent cannot tell, which is not the same as physical.
	Machine string `json:"machine,omitempty"`
	// Hypervisor names the virtualisation, when there is one and it says.
	Hypervisor string `json:"hypervisor,omitempty"`
	// BootedAt is when the kernel started.
	BootedAt *time.Time `json:"booted_at,omitempty"`
	// TimeZone is the host's zone name, and OffsetSeconds its offset from UTC
	// at the moment of the report.
	TimeZone      string `json:"time_zone,omitempty"`
	OffsetSeconds int    `json:"utc_offset_seconds"`
	// Interfaces are the network interfaces that are up and the addresses
	// each holds (netinfo.go).
	Interfaces []NetInterface `json:"interfaces,omitempty"`
	// Cloud is the cloud instance this machine runs as, from the cloud's
	// metadata service (cloudid.go). Nil off a cloud, or when it did not
	// answer.
	Cloud *CloudInstance `json:"cloud,omitempty"`
}

// Usage is how busy the machine is now.
type Usage struct {
	At time.Time `json:"at"`
	// Load is the 1, 5 and 15 minute load averages. Nil where the system
	// keeps none (Windows), which is not the same as an idle machine.
	Load *[3]float64 `json:"load,omitempty"`
	// CPUBusy is the share of processor time not idle since the previous
	// sample, 0 to 100. Nil on the first sample after the agent starts,
	// which has nothing to compare against.
	CPUBusy *float64 `json:"cpu_busy,omitempty"`
	// Memory: total, and what applications could still be given without
	// swapping (MemAvailable), which is the honest "free".
	MemTotal     uint64 `json:"mem_total"`
	MemAvailable uint64 `json:"mem_available"`
	SwapTotal    uint64 `json:"swap_total"`
	SwapFree     uint64 `json:"swap_free"`
	Disks        []Disk `json:"disks"`
	// Net is the traffic through each interface that is up, since the
	// previous sample. Nil where the platform reports none.
	Net []NetRate `json:"net,omitempty"`
	// Steal is the share of time the hypervisor ran something else while
	// this machine had work, 0 to 100, since the previous sample: a noisy
	// host, or a burstable instance out of credits. Nil where the platform
	// keeps no such counter (Windows) and on the first sample.
	Steal *float64 `json:"steal,omitempty"`
	// TCPConns is the TCP connections open now, IPv4 and IPv6. Nil where
	// it could not be read.
	TCPConns *uint64 `json:"tcp_conns,omitempty"`
	// DiskIO is each physical disk's operations and bytes per second since
	// the previous sample. Nil where the platform reports none.
	DiskIO []DiskIO `json:"disk_io,omitempty"`
	// Top is what is using the machine now, by program: the processes
	// sharing a name added together (twenty postgres workers are one row),
	// the five programs using the most processor since the previous sample,
	// the five using the most memory and the five reading and writing the
	// disk most, at most fifteen. Names only, never command lines. Nil
	// where the platform reports none.
	Top []Process `json:"top,omitempty"`
}

// Process is one running program in Top: every process with its name.
type Process struct {
	// Name is the program's name: the kernel's 15-character comm on
	// Linux (a kernel thread's without its "/n" suffix), the executable's
	// file name on Windows.
	Name string `json:"name"`
	// PID is its busiest process's, so a single process can be found.
	PID int `json:"pid"`
	// Count is how many processes run under the name; omitted for one.
	Count int `json:"count,omitempty"`
	// CPU is its share of the whole machine's processor time since the
	// previous sample, 0 to 100; nil when none of its processes had an
	// earlier reading.
	CPU *float64 `json:"cpu,omitempty"`
	// Memory is its processes' resident memory added up (working set on
	// Windows), in bytes. Memory two processes share is counted in each.
	Memory uint64 `json:"memory"`
	// ReadBps and WriteBps are bytes per second it read and wrote since
	// the previous sample: what reached the disk on Linux, every file,
	// device and network transfer on Windows. Nil on the first sample and
	// when any of its processes could not be read (IOHidden).
	ReadBps  *float64 `json:"read_bps,omitempty"`
	WriteBps *float64 `json:"write_bps,omitempty"`
	// IOHidden is true when the system refused the agent its processes'
	// counters: a Linux agent without CAP_SYS_PTRACE reading another
	// user's processes.
	IOHidden bool `json:"io_hidden,omitempty"`
}

// DiskIO is one physical disk's activity, averaged since the previous
// sample. A rate is nil on a disk's first sighting, or when its counter
// went backwards, which has no honest rate.
type DiskIO struct {
	Name     string   `json:"name"`
	ReadOps  *float64 `json:"read_ops,omitempty"`
	WriteOps *float64 `json:"write_ops,omitempty"`
	ReadBps  *float64 `json:"read_bps,omitempty"`
	WriteBps *float64 `json:"write_bps,omitempty"`
}

// NetRate is one interface's traffic, averaged since the previous sample.
type NetRate struct {
	Name string `json:"name"`
	// RxBps and TxBps are bytes per second in and out. Nil on the first
	// sample, or when a counter went backwards (the interface was reset or
	// re-created), which has no honest rate.
	RxBps *float64 `json:"rx_bps,omitempty"`
	TxBps *float64 `json:"tx_bps,omitempty"`
	// SpeedMbps is the link speed the system states, 0 when it states none
	// (virtual interfaces, Wi-Fi on some drivers).
	SpeedMbps uint64 `json:"speed_mbps,omitempty"`
}

// Disk is one mounted filesystem.
type Disk struct {
	Mount string `json:"mount"`
	FS    string `json:"fs"`
	Total uint64 `json:"total"`
	// Used is what is in use; Avail is what an unprivileged writer can still
	// use, which is less than total minus used by the root reserve.
	Used  uint64 `json:"used"`
	Avail uint64 `json:"avail"`
}

// maxNet bounds the interfaces in one report; a host with more is a
// container host whose veths are not what anyone is asking about.
const maxNet = 32

// topN is how many programs Top keeps by each measure.
const topN = 5

// maxProcScan bounds the processes read for one sample, so a machine
// running a fork bomb costs a bounded read.
const maxProcScan = 8192

// maxDiskIO bounds the physical disks in one report.
const maxDiskIO = 16

// maxDisks bounds the report. A host with more real filesystems than this is
// unusual, and the console lists the fullest first anyway.
const maxDisks = 32

// DiscoverSystem reads SystemInfo. Every part is best-effort.
func DiscoverSystem() SystemInfo {
	s := safePlatformSystem()
	if s.Arch == "" {
		s.Arch = goArchName(runtime.GOARCH)
	}
	if s.CPUs == 0 {
		s.CPUs = runtime.NumCPU()
	}
	name, off := time.Now().Zone()
	if s.TimeZone == "" {
		if loc := time.Local.String(); loc != "" && loc != "Local" {
			s.TimeZone = loc
		} else {
			s.TimeZone = name
		}
	}
	s.OffsetSeconds = off
	s.Interfaces = DiscoverInterfaces()
	s.Cloud = discoverCloud(s.Hypervisor)
	return s
}

// safePlatformSystem is platformSystem, with a failure costing the facts
// rather than the host report they ride.
func safePlatformSystem() (s SystemInfo) {
	defer func() {
		if recover() != nil {
			s = SystemInfo{}
		}
	}()
	return platformSystem()
}

// UsageSampler takes Usage samples, keeping the previous processor counters
// so each sample can say how busy the processor was since the last one.
type UsageSampler struct {
	prev cpuTimes
	// net is each interface's byte counters at the previous sample.
	net   map[string]netCounters
	netAt time.Time
	// disk is each physical disk's counters at the previous sample.
	disk   map[string]diskCounters
	diskAt time.Time
	// procs is each process's counters at the previous sample, with the
	// machine's processor total and the time, so its shares can be told.
	procs     map[procKey]procPrev
	procTotal uint64
	procAt    time.Time
}

// procKey names a process across samples: a pid can be reused, its start
// time with it cannot.
type procKey struct {
	pid   int
	start uint64
}

// procPrev is what a process's next sample is measured against.
type procPrev struct {
	cpu, read, wrote uint64
	io               bool
}

// procCounters is one process as the platform reads it.
type procCounters struct {
	key    procKey
	name   string
	cpu    uint64 // processor time, in the units total is in
	memory uint64
	// read and wrote are bytes since it started; io is false when they
	// could not be read, and denied says the system refused them.
	read, wrote uint64
	io, denied  bool
}

// programName is the name processes are added up under. A kernel thread
// names its processor or queue after a slash (kworker/3:1H, ksoftirqd/0):
// those are one program.
func programName(name string) string {
	if i := strings.IndexByte(name, '/'); i > 0 {
		return name[:i]
	}
	return name
}

// topProcesses turns two readings of the process table into Top, keeping
// the new reading for the next call. total is the machine's processor time
// in the same units as each process's, at is when the table was read. Nil
// when no process was read.
func (u *UsageSampler) topProcesses(now []procCounters, total uint64, at time.Time) []Process {
	if len(now) == 0 {
		return nil
	}
	dt := float64(0)
	if u.procTotal > 0 && total > u.procTotal {
		dt = float64(total - u.procTotal)
	}
	secs := float64(0)
	if !u.procAt.IsZero() && at.After(u.procAt) {
		secs = at.Sub(u.procAt).Seconds()
	}
	type acc struct {
		p                 Process
		lead              float64 // the busiest member's share, for PID
		cpu, read, wrote  float64
		hasCPU, hasIO     bool
		hidden, ioUnknown bool
	}
	next := make(map[procKey]procPrev, len(now))
	byName := make(map[string]*acc, 64)
	order := make([]*acc, 0, 64)
	for _, c := range now {
		next[c.key] = procPrev{cpu: c.cpu, read: c.read, wrote: c.wrote, io: c.io}
		name := programName(c.name)
		a := byName[name]
		if a == nil {
			a = &acc{p: Process{Name: name, PID: c.key.pid}, lead: -1}
			byName[name] = a
			order = append(order, a)
		}
		a.p.Count++
		a.p.Memory += c.memory
		share := -1.0
		prev, seen := u.procs[c.key]
		if seen && dt > 0 && c.cpu >= prev.cpu {
			share = float64(c.cpu-prev.cpu) / dt * 100
			a.cpu += share
			a.hasCPU = true
		}
		if share > a.lead || (share == a.lead && a.p.Count == 1) {
			a.lead, a.p.PID = share, c.key.pid
		}
		switch {
		case c.denied:
			a.hidden = true
		case !c.io:
			a.ioUnknown = true
		case seen && prev.io && secs > 0 && c.read >= prev.read && c.wrote >= prev.wrote:
			a.read += float64(c.read-prev.read) / secs
			a.wrote += float64(c.wrote-prev.wrote) / secs
			a.hasIO = true
		}
	}
	u.procs, u.procTotal, u.procAt = next, total, at
	all := make([]Process, 0, len(order))
	for _, a := range order {
		p := a.p
		if p.Count == 1 {
			p.Count = 0
		}
		if a.hasCPU {
			v := math.Min(100, math.Round(a.cpu*10)/10)
			p.CPU = &v
		}
		switch {
		case a.hidden:
			p.IOHidden = true
		case a.hasIO && !a.ioUnknown:
			r, w := math.Round(a.read), math.Round(a.wrote)
			p.ReadBps, p.WriteBps = &r, &w
		}
		all = append(all, p)
	}
	return pickTop(all)
}

// pickTop keeps the topN by processor, then the topN by memory, then the
// topN by disk, each skipping a program already kept.
func pickTop(all []Process) []Process {
	cpuOf := func(p Process) float64 {
		if p.CPU == nil {
			return -1
		}
		return *p.CPU
	}
	ioOf := func(p Process) float64 {
		if p.ReadBps == nil {
			return -1
		}
		return *p.ReadBps + *p.WriteBps
	}
	out := make([]Process, 0, 3*topN)
	seen := make(map[string]bool, 3*topN)
	take := func(less func(a, b Process) bool, ok func(Process) bool) {
		sort.SliceStable(all, func(i, j int) bool { return less(all[i], all[j]) })
		added := 0
		for _, p := range all {
			if added == topN || !ok(p) {
				break
			}
			added++
			if !seen[p.Name] {
				out = append(out, p)
				seen[p.Name] = true
			}
		}
	}
	take(func(a, b Process) bool {
		if cpuOf(a) != cpuOf(b) {
			return cpuOf(a) > cpuOf(b)
		}
		return a.Memory > b.Memory
	}, func(p Process) bool { return p.CPU != nil })
	take(func(a, b Process) bool { return a.Memory > b.Memory }, func(Process) bool { return true })
	take(func(a, b Process) bool { return ioOf(a) > ioOf(b) }, func(p Process) bool { return ioOf(p) > 0 })
	return out
}

// diskCounters is one physical disk as the platform reads it: completed
// operations and bytes since boot.
type diskCounters struct {
	name                  string
	reads, writes         uint64
	readBytes, wroteBytes uint64
}

// diskRates turns two readings of the disk counters into per-second rates,
// keeping the new reading for the next call. Like netRates, a disk seen for
// the first time has no rate.
func (u *UsageSampler) diskRates(now []diskCounters, at time.Time) []DiskIO {
	if now == nil {
		return nil
	}
	out := make([]DiskIO, 0, min(len(now), maxDiskIO))
	secs := at.Sub(u.diskAt).Seconds()
	next := make(map[string]diskCounters, len(now))
	for _, c := range now {
		if len(out) == maxDiskIO {
			break
		}
		next[c.name] = c
		d := DiskIO{Name: c.name}
		if p, ok := u.disk[c.name]; ok && secs >= 1 {
			d.ReadOps = rateSince(p.reads, c.reads, secs)
			d.WriteOps = rateSince(p.writes, c.writes, secs)
			d.ReadBps = rateSince(p.readBytes, c.readBytes, secs)
			d.WriteBps = rateSince(p.wroteBytes, c.wroteBytes, secs)
		}
		out = append(out, d)
	}
	u.disk, u.diskAt = next, at
	return out
}

// netCounters is one interface as the platform reads it.
type netCounters struct {
	name      string
	rx, tx    uint64
	speedMbps uint64
}

// netRates turns two readings of the interface counters into per-second
// rates, keeping the new reading for the next call. Interfaces are kept in
// the order read and bounded.
func (u *UsageSampler) netRates(now []netCounters, at time.Time) []NetRate {
	out := make([]NetRate, 0, min(len(now), maxNet))
	secs := at.Sub(u.netAt).Seconds()
	next := make(map[string]netCounters, len(now))
	for _, c := range now {
		if len(out) == maxNet {
			break
		}
		next[c.name] = c
		r := NetRate{Name: c.name, SpeedMbps: c.speedMbps}
		if p, ok := u.net[c.name]; ok && secs >= 1 {
			r.RxBps = rateSince(p.rx, c.rx, secs)
			r.TxBps = rateSince(p.tx, c.tx, secs)
		}
		out = append(out, r)
	}
	u.net, u.netAt = next, at
	return out
}

// rateSince is bytes per second between two counter readings, or nil when
// the counter went backwards.
func rateSince(prev, now uint64, secs float64) *float64 {
	if now < prev || secs <= 0 {
		return nil
	}
	v := float64(now-prev) / secs
	v = float64(int64(v*10+0.5)) / 10
	return &v
}

// Sample reads Usage now. ok is false where the platform reports none, or
// where reading it failed in a way that would otherwise have taken the
// heartbeat down with it: a figure is never worth the agent.
func (u *UsageSampler) Sample() (out Usage, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = Usage{}, false
		}
	}()
	return u.platformSample()
}

// cpuTimes is the aggregate processor counters from /proc/stat.
type cpuTimes struct {
	idle, total uint64
	// steal is the hypervisor's share, counted in total; 0 where the
	// platform keeps none.
	steal uint64
}

// busySince is the non-idle share between two samples, or nil when there
// is no earlier sample or the counters did not move forward.
func busySince(prev, now cpuTimes) *float64 {
	if prev.total == 0 || now.total <= prev.total || now.idle < prev.idle {
		return nil
	}
	dt := float64(now.total - prev.total)
	di := float64(now.idle - prev.idle)
	v := (dt - di) / dt * 100
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	v = float64(int(v*10+0.5)) / 10
	return &v
}

// stealSince is the stolen share between two samples, or nil when there is
// no earlier sample or the counters did not move forward.
func stealSince(prev, now cpuTimes) *float64 {
	if prev.total == 0 || now.total <= prev.total || now.steal < prev.steal {
		return nil
	}
	v := float64(now.steal-prev.steal) / float64(now.total-prev.total) * 100
	v = math.Min(100, float64(int(v*10+0.5))/10)
	return &v
}

// cleanCPUModel drops the trademark noise and runs of spaces vendors pad
// model names with.
func cleanCPUModel(s string) string {
	if i := strings.Index(s, "@"); i > 0 {
		s = s[:i] // the clock speed, which is the base, not what it runs at
	}
	for _, junk := range []string{"(R)", "(r)", "(TM)", "(tm)"} {
		s = strings.ReplaceAll(s, junk, " ")
	}
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(strings.TrimSuffix(s, " CPU"))
}

// goArchName spells Go's architecture the way uname does.
func goArchName(a string) string {
	switch a {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	}
	return a
}

// hypervisorFromDMI names a hypervisor from the DMI vendor and product, or
// "" when they name real hardware or nothing.
func hypervisorFromDMI(vendor, product string) string {
	v, p := strings.ToLower(vendor), strings.ToLower(product)
	switch {
	case strings.Contains(p, "kvm") || strings.Contains(v, "qemu") || strings.Contains(p, "qemu"):
		return "KVM"
	case strings.Contains(v, "vmware") || strings.Contains(p, "vmware"):
		return "VMware"
	case strings.Contains(v, "microsoft") && strings.Contains(p, "virtual"):
		return "Hyper-V"
	case strings.Contains(v, "xen") || strings.Contains(p, "hvm domu"):
		return "Xen"
	case strings.Contains(p, "virtualbox") || strings.Contains(v, "innotek"):
		return "VirtualBox"
	case strings.Contains(v, "amazon ec2") || strings.Contains(p, "amazon ec2"):
		return "Amazon EC2"
	case strings.Contains(v, "google") && strings.Contains(p, "compute engine"):
		return "Google Compute Engine"
	case strings.Contains(v, "digitalocean"):
		return "DigitalOcean"
	case strings.Contains(v, "hetzner"):
		return "Hetzner Cloud"
	case strings.Contains(p, "openstack") || strings.Contains(v, "openstack"):
		return "OpenStack"
	case strings.Contains(v, "parallels") || strings.Contains(p, "parallels"):
		return "Parallels"
	case strings.Contains(v, "bochs") || strings.Contains(p, "bochs"):
		return "Bochs"
	}
	return ""
}

package agent

import (
	"runtime"
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
names, the mount table, statfs and the interface table. No process, no file content, no
configuration. A mount point is a path, not what is stored under it.
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
}

// Usage is how busy the machine is now.
type Usage struct {
	At time.Time `json:"at"`
	// Load is the 1, 5 and 15 minute load averages.
	Load [3]float64 `json:"load"`
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

// maxDisks bounds the report. A host with more real filesystems than this is
// unusual, and the console lists the fullest first anyway.
const maxDisks = 32

// DiscoverSystem reads SystemInfo. Every part is best-effort.
func DiscoverSystem() SystemInfo {
	s := platformSystem()
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
	return s
}

// UsageSampler takes Usage samples, keeping the previous processor counters
// so each sample can say how busy the processor was since the last one.
type UsageSampler struct {
	prev cpuTimes
}

// Sample reads Usage now. ok is false where the platform reports none.
func (u *UsageSampler) Sample() (Usage, bool) {
	return u.platformSample()
}

// cpuTimes is the aggregate processor counters from /proc/stat.
type cpuTimes struct {
	idle, total uint64
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

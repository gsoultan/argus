//go:build windows

package agent

import (
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

/*
The System card's figures on Windows, from the same kind of sources as on
Linux: what any local user could read. GetSystemTimes for how busy the
processor is, GlobalMemoryStatusEx for memory, the fixed drives for disks and
the interface table for traffic. Windows keeps no load average, so Load is
left nil rather than reported as an idle machine, and no swap: the page file
is not swap, and the commit limit would read as a second, larger memory.
*/

var (
	modKernel32Sys           = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = modKernel32Sys.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = modKernel32Sys.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64       = modKernel32Sys.NewProc("GetTickCount64")

	modIphlpapiSys         = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetTcpStatisticsEx = modIphlpapiSys.NewProc("GetTcpStatisticsEx")
)

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func globalMemory() (memoryStatusEx, bool) {
	m := memoryStatusEx{}
	m.Length = uint32(unsafe.Sizeof(m))
	if err := procGlobalMemoryStatusEx.Find(); err != nil {
		return m, false
	}
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	return m, r != 0
}

func platformSystem() SystemInfo {
	var s SystemInfo
	if v := windows.RtlGetVersion(); v != nil {
		s.Kernel = fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if name, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			s.CPUModel = cleanCPUModel(name)
		}
		k.Close()
	}
	if m, ok := globalMemory(); ok {
		s.MemoryBytes = m.TotalPhys
	}
	if procGetTickCount64.Find() == nil {
		ms, _, _ := procGetTickCount64.Call()
		if ms > 0 {
			t := time.Now().Add(-time.Duration(ms) * time.Millisecond).UTC().Truncate(time.Second)
			s.BootedAt = &t
		}
	}
	// The firmware's names, which Windows keeps where Linux keeps DMI. Only
	// a virtual machine is said: without the processor's hypervisor flag,
	// a vendor name alone is not proof of real hardware.
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE); err == nil {
		vendor, _, _ := k.GetStringValue("SystemManufacturer")
		product, _, _ := k.GetStringValue("SystemProductName")
		k.Close()
		if hv := hypervisorFromDMI(vendor, product); hv != "" {
			s.Machine, s.Hypervisor = "virtual", hv
		}
	}
	return s
}

func (u *UsageSampler) platformSample() (Usage, bool) {
	out := Usage{At: time.Now().UTC(), Disks: []Disk{}}
	if now, ok := systemTimes(); ok {
		out.CPUBusy = busySince(u.prev, now)
		u.prev = now
	}
	if m, ok := globalMemory(); ok {
		out.MemTotal, out.MemAvailable = m.TotalPhys, m.AvailPhys
	}
	out.Disks = fixedDrives()
	out.Net = u.netRates(interfaceCounters(), out.At)
	out.TCPConns = tcpConnections()
	out.DiskIO = u.diskRates(physicalDiskCounters(), out.At)
	return out, true
}

// systemTimes reads the processor counters. Kernel time includes idle time,
// so total is kernel plus user and idle is counted once.
func systemTimes() (cpuTimes, bool) {
	if procGetSystemTimes.Find() != nil {
		return cpuTimes{}, false
	}
	var idle, kernel, user windows.Filetime
	r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return cpuTimes{}, false
	}
	ft := func(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	t := cpuTimes{idle: ft(idle), total: ft(kernel) + ft(user)}
	if t.idle > t.total {
		return cpuTimes{}, false
	}
	return t, true
}

// fixedDrives is each fixed drive with a size: not removable, network,
// optical or RAM drives, the way Linux leaves out virtual filesystems. A
// drive that cannot be read (BitLocker-locked) is left out.
func fixedDrives() []Disk {
	out := []Disk{}
	buf := make([]uint16, 512)
	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil || n == 0 || int(n) > len(buf) {
		return out
	}
	// The list is roots separated by NULs, ended by two.
	for i := 0; i < int(n) && len(out) < maxDisks; {
		j := i
		for j < int(n) && buf[j] != 0 {
			j++
		}
		root := windows.UTF16ToString(buf[i:j])
		i = j + 1
		if root == "" {
			continue
		}
		p, err := windows.UTF16PtrFromString(root)
		if err != nil || windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		var avail, total, free uint64
		if windows.GetDiskFreeSpaceEx(p, &avail, &total, &free) != nil || total == 0 || free > total || avail > total {
			continue
		}
		fsName := make([]uint16, windows.MAX_PATH+1)
		fs := ""
		if windows.GetVolumeInformation(p, nil, 0, nil, nil, nil, &fsName[0], uint32(len(fsName))) == nil {
			fs = strings.ToLower(windows.UTF16ToString(fsName))
		}
		out = append(out, Disk{Mount: strings.TrimSuffix(root, `\`), FS: fs, Total: total, Used: total - free, Avail: avail})
	}
	return out
}

// Interface flags in MIB_IF_ROW2's InterfaceAndOperStatusFlags.
const (
	ifFlagHardware  = 0x01
	ifFlagFilter    = 0x02
	ifFlagConnector = 0x04
)

// interfaceCounters reads the interface table: interfaces that are up, not
// loopback and not one of the filter layers Windows lists once per
// installed driver (each a copy of a real adapter's counters).
func interfaceCounters() []netCounters {
	var table *windows.MibIfTable2
	if windows.GetIfTable2Ex(windows.MibIfTableNormal, &table) != nil || table == nil {
		return nil
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	rows := unsafe.Slice(&table.Table[0], table.NumEntries)
	var out []netCounters
	for i := range rows {
		r := &rows[i]
		if r.Type == windows.IF_TYPE_SOFTWARE_LOOPBACK || r.OperStatus != windows.IfOperStatusUp {
			continue
		}
		flags := r.InterfaceAndOperStatusFlags
		if flags&ifFlagFilter != 0 || flags&(ifFlagHardware|ifFlagConnector) == 0 {
			continue
		}
		name := windows.UTF16ToString(r.Alias[:])
		if name == "" {
			continue
		}
		c := netCounters{name: name, rx: r.InOctets, tx: r.OutOctets}
		// Bits per second; an unknown speed is reported as all ones.
		if s := r.ReceiveLinkSpeed; s > 0 && s < 1<<50 {
			c.speedMbps = s / 1_000_000
		}
		out = append(out, c)
		if len(out) == maxNet {
			break
		}
	}
	return out
}

// mibTCPStats is MIB_TCPSTATS: fifteen DWORDs.
type mibTCPStats struct {
	RtoAlgorithm, RtoMin, RtoMax, MaxConn   uint32
	ActiveOpens, PassiveOpens, AttemptFails uint32
	EstabResets, CurrEstab, InSegs, OutSegs uint32
	RetransSegs, InErrs, OutRsts, NumConns  uint32
}

// tcpConnections is the TCP connection table's size for IPv4 and IPv6:
// every connection and listener, as Linux's sockstat counts them. Nil when
// neither family can be read.
func tcpConnections() *uint64 {
	if procGetTcpStatisticsEx.Find() != nil {
		return nil
	}
	var total uint64
	var any bool
	for _, family := range []uintptr{windows.AF_INET, windows.AF_INET6} {
		var st mibTCPStats
		if r, _, _ := procGetTcpStatisticsEx.Call(uintptr(unsafe.Pointer(&st)), family); r == 0 {
			total += uint64(st.NumConns)
			any = true
		}
	}
	if !any {
		return nil
	}
	return &total
}

// ioctlDiskPerformance is IOCTL_DISK_PERFORMANCE.
const ioctlDiskPerformance = 0x00070020

// diskPerformance is DISK_PERFORMANCE.
type diskPerformance struct {
	BytesRead, BytesWritten int64
	ReadTime, WriteTime     int64
	IdleTime                int64
	ReadCount, WriteCount   uint32
	QueueDepth, SplitCount  uint32
	QueryTime               int64
	StorageDeviceNumber     uint32
	StorageManagerName      [8]uint16
}

// physicalDiskCounters reads each physical disk's counters, the ones
// Performance Monitor's PhysicalDisk shows. Opening a disk only to query it
// needs no read access; a number with no disk behind it is skipped, since
// numbers can be sparse after a disk is removed. Nil when no disk answers.
func physicalDiskCounters() []diskCounters {
	var out []diskCounters
	for i := 0; i < maxDiskIO; i++ {
		name := fmt.Sprintf(`\\.\PhysicalDrive%d`, i)
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		var dp diskPerformance
		var n uint32
		err = windows.DeviceIoControl(h, ioctlDiskPerformance, nil, 0, (*byte)(unsafe.Pointer(&dp)), uint32(unsafe.Sizeof(dp)), &n, nil)
		windows.CloseHandle(h)
		if err != nil || dp.BytesRead < 0 || dp.BytesWritten < 0 {
			continue
		}
		out = append(out, diskCounters{name: fmt.Sprintf("disk%d", i), reads: uint64(dp.ReadCount), writes: uint64(dp.WriteCount),
			readBytes: uint64(dp.BytesRead), wroteBytes: uint64(dp.BytesWritten)})
	}
	return out
}

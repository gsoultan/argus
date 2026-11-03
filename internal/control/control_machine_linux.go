//go:build linux

package control

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func platformMachine(cpu *cpuSampler, now time.Time) MachineStatus {
	return readMachine("/proc", "/etc/os-release", cpu, now)
}

func readMachine(proc, osRelease string, cpu *cpuSampler, now time.Time) MachineStatus {
	var m MachineStatus
	m.OS = prettyName(osRelease)
	if b, err := os.ReadFile(proc + "/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				m.Load1 = &v
			}
		}
	}
	m.CPUBusy = cpu.busy(func() cpuTimes { return readCPUTimes(proc + "/stat") }, now)
	mem := readMeminfo(proc + "/meminfo")
	m.MemTotal = mem["MemTotal"]
	if v, ok := mem["MemAvailable"]; ok {
		m.MemAvailable = v
	} else {
		m.MemAvailable = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}
	return m
}

func prettyName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

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

// readCPUTimes reads the aggregate "cpu" line. Idle counts iowait too: a
// processor waiting on a disk is not doing work.
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

func statfs(path string) (total, avail uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, true
}

// deviceOf names the filesystem path is on. The filesystem id from statfs
// rather than stat's device number: on overlay and some network filesystems
// two directories of one filesystem report different devices.
func deviceOf(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(uint32(st.Fsid.X__val[0]))<<32 | uint64(uint32(st.Fsid.X__val[1])), true
}

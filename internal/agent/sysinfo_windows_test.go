//go:build windows

package agent

import (
	"testing"
	"time"
)

// The Windows readers on a real Windows machine: CI's runner. Each figure is
// checked for being plausible, not exact, since the runner is a live host.
func TestWindowsReportsSystemAndUsage(t *testing.T) {
	s := DiscoverSystem()
	if s.Kernel == "" || s.MemoryBytes == 0 || s.CPUs == 0 || s.BootedAt == nil {
		t.Errorf("system = %+v, want kernel, memory, CPUs and boot time", s)
	}
	if s.BootedAt != nil && s.BootedAt.After(time.Now()) {
		t.Errorf("booted in the future: %v", s.BootedAt)
	}

	var u UsageSampler
	first, ok := u.Sample()
	if !ok {
		t.Fatal("no usage reported on Windows")
	}
	if first.Load != nil {
		t.Errorf("load = %v, want none: Windows keeps no load average", *first.Load)
	}
	if first.MemTotal == 0 || first.MemAvailable > first.MemTotal {
		t.Errorf("memory = %d of %d", first.MemAvailable, first.MemTotal)
	}
	if len(first.Disks) == 0 {
		t.Fatal("no fixed drives")
	}
	for _, d := range first.Disks {
		if d.Total == 0 || d.Used > d.Total || d.Avail > d.Total || len(d.Mount) != 2 || d.Mount[1] != ':' {
			t.Errorf("drive = %+v", d)
		}
	}
	time.Sleep(1100 * time.Millisecond)
	second, _ := u.Sample()
	if second.CPUBusy == nil || *second.CPUBusy < 0 || *second.CPUBusy > 100 {
		t.Errorf("busy = %v, want a share on the second sample", second.CPUBusy)
	}
	if len(second.Net) == 0 {
		t.Log("no interface reported traffic; acceptable on a runner with only filtered adapters")
	}
	for _, n := range second.Net {
		if n.Name == "" || (n.RxBps != nil && *n.RxBps < 0) {
			t.Errorf("interface = %+v", n)
		}
	}
}

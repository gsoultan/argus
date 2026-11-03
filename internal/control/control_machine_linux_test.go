//go:build linux

package control

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadMachineFromProc(t *testing.T) {
	dir := t.TempDir()
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	must("loadavg", "0.48 0.52 0.60 1/234 5678\n")
	must("stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4\n")
	must("meminfo", "MemTotal:        8000 kB\nMemFree:  1000 kB\nMemAvailable:    5000 kB\n")
	must("os-release", "NAME=Ubuntu\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n")

	var cpu cpuSampler
	now := time.Now()
	m := readMachine(dir, filepath.Join(dir, "os-release"), &cpu, now)
	if m.OS != "Ubuntu 24.04.1 LTS" || m.Load1 == nil || *m.Load1 != 0.48 {
		t.Errorf("machine = %+v", m)
	}
	if m.MemTotal != 8000*1024 || m.MemAvailable != 5000*1024 {
		t.Errorf("memory = %d / %d", m.MemAvailable, m.MemTotal)
	}

	// The second look is measured against the first.
	must("stat", "cpu  150 0 150 750 150 0 0 0 0 0\n")
	m = readMachine(dir, filepath.Join(dir, "os-release"), &cpu, now.Add(15*time.Second))
	if m.CPUBusy == nil || *m.CPUBusy != 50 {
		t.Errorf("busy = %v, want 50", m.CPUBusy)
	}
}

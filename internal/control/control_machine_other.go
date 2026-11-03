//go:build !linux

package control

import (
	"runtime"
	"time"
)

// platformMachine reports only what Go knows on its own: the rest is read
// from /proc, which only Linux has.
func platformMachine(_ *cpuSampler, _ time.Time) MachineStatus {
	return MachineStatus{OS: runtime.GOOS}
}

func statfs(string) (total, avail uint64, ok bool) { return 0, 0, false }

func deviceOf(string) (uint64, bool) { return 0, false }

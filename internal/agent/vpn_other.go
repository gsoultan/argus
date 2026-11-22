//go:build !linux

package agent

// WireGuard's state is read on Linux only so far.
func wgSysfsNames() []string { return nil }

func wgCounters(string) (rx, tx uint64) { return 0, 0 }

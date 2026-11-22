//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const sysClassNet = "/sys/class/net"

// wgSysfsNames lists WireGuard interfaces from the kernel, for a host
// without the wg tool. Nil when /sys/class/net cannot be read.
func wgSysfsNames() []string {
	return wgNamesIn(sysClassNet)
}

func wgNamesIn(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "uevent"))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "DEVTYPE=wireguard") && validIfName(e.Name()) && len(out) < maxWGInterfaces {
			out = append(out, e.Name())
		}
	}
	return out
}

// wgCounters are the bytes the kernel counted through an interface.
func wgCounters(name string) (rx, tx uint64) {
	read := func(f string) uint64 {
		b, err := os.ReadFile(filepath.Join(sysClassNet, name, "statistics", f))
		if err != nil {
			return 0
		}
		v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return v
	}
	return read("rx_bytes"), read("tx_bytes")
}

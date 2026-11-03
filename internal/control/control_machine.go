package control

import (
	"runtime"
	"strings"
	"sync"
	"time"
)

/*
The machine argus-control runs on, for the Status page.

A small reader of its own rather than the agent's: agent code stays under
internal/agent and the control plane links none of it (see
cmd/argus-agent/layering_test.go for the other direction). Read-only and
nothing a local user could not already see: /proc and statfs. Linux only; on
anything else the fields are left empty and the page says they are not
reported, rather than showing zeros as if they were measured.
*/

// MachineStatus is how busy the host is.
type MachineStatus struct {
	OS    string `json:"os,omitempty"`
	Cores int    `json:"cores"`
	// Load1 is the one-minute load average.
	Load1 *float64 `json:"load1,omitempty"`
	// CPUBusy is the share of processor time not idle since the previous
	// look, 0 to 100. Nil when there is nothing to compare against yet.
	CPUBusy *float64 `json:"cpuBusy,omitempty"`
	// MemTotal and MemAvailable are bytes; MemAvailable is what applications
	// could still be given without swapping, the honest "free".
	MemTotal     uint64       `json:"memTotal,omitempty"`
	MemAvailable uint64       `json:"memAvailable,omitempty"`
	Disks        []DiskStatus `json:"disks"`
}

// DiskStatus is one filesystem the control plane writes to.
type DiskStatus struct {
	Path string `json:"path"`
	// Use says what argus-control keeps there, in words.
	Use   string `json:"use"`
	Total uint64 `json:"total"`
	Avail uint64 `json:"avail"`
}

// cpuTimes is the aggregate line of /proc/stat, in clock ticks.
type cpuTimes struct{ total, idle uint64 }

// cpuSampler remembers the last /proc/stat reading so busy-ness can be
// measured between two looks, the way top does.
type cpuSampler struct {
	mu   sync.Mutex
	prev cpuTimes
	at   time.Time
}

// busy returns the busy share since the previous call, sampling twice a
// short while apart when there is no previous reading or it is stale.
func (c *cpuSampler) busy(read func() cpuTimes, now time.Time) *float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := read()
	if cur.total == 0 {
		return nil
	}
	prev := c.prev
	if c.at.IsZero() || now.Sub(c.at) > 5*time.Minute {
		time.Sleep(250 * time.Millisecond)
		prev, cur = cur, read()
	}
	c.prev, c.at = cur, now
	return busyBetween(prev, cur)
}

func busyBetween(prev, cur cpuTimes) *float64 {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return nil
	}
	dt, di := cur.total-prev.total, cur.idle-prev.idle
	if di > dt {
		return nil
	}
	v := 100 * float64(dt-di) / float64(dt)
	return &v
}

func (a *API) machineStatus(now time.Time, disks []DiskStatus) MachineStatus {
	m := platformMachine(&a.cpu, now)
	m.Cores = runtime.NumCPU()
	m.Disks = []DiskStatus{}
	// Two paths on one filesystem are one disk filling up, not two: shown
	// once, with both uses.
	seen := map[uint64]int{}
	for _, d := range disks {
		total, avail, ok := statfs(d.Path)
		if !ok {
			continue
		}
		if dev, ok := deviceOf(d.Path); ok {
			if i, dup := seen[dev]; dup {
				m.Disks[i].Use += "; " + lowerFirst(d.Use)
				continue
			}
			seen[dev] = len(m.Disks)
		}
		d.Total, d.Avail = total, avail
		m.Disks = append(m.Disks, d)
	}
	return m
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

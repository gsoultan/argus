package control

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
What kind of machine a server is and how busy it is, as its agent reported.

The agent reads these from /proc, /sys and statfs (internal/agent/sysinfo.go).
Here they are bounded on the way in -- an agent is a credentialed caller, but
it is still input -- and served to the System card on the server page, which
says how old they are rather than presenting a sample as the present.
*/

// HostSystem mirrors agent.SystemInfo, in the agent's spelling.
type HostSystem struct {
	Kernel        string     `json:"kernel,omitempty"`
	Arch          string     `json:"arch,omitempty"`
	CPUModel      string     `json:"cpu_model,omitempty"`
	CPUs          int        `json:"cpus,omitempty"`
	MemoryBytes   uint64     `json:"memory_bytes,omitempty"`
	Machine       string     `json:"machine,omitempty"`
	Hypervisor    string     `json:"hypervisor,omitempty"`
	BootedAt      *time.Time `json:"booted_at,omitempty"`
	TimeZone      string     `json:"time_zone,omitempty"`
	OffsetSeconds int        `json:"utc_offset_seconds"`
	// Interfaces is nil from an agent older than per-interface addresses;
	// the card then falls back to the flat list on the agents row.
	Interfaces []HostInterface `json:"interfaces,omitempty"`
	// Engine is how an Android device is driven: "attended" (someone accepts
	// each session) or "unattended" (the helper over local ADB, nobody
	// there). Empty from a server, or an app older than the unattended
	// engine.
	Engine string `json:"engine,omitempty"`
	// WirelessDebugging is whether wireless debugging is on, which the
	// unattended engine needs and which listens on every interface on
	// Android 11+. The console shows it so nobody finds it by surprise.
	WirelessDebugging bool `json:"wireless_debugging,omitempty"`
}

// HostInterface mirrors agent.NetInterface.
type HostInterface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses"`
}

// HostUsage mirrors agent.Usage, in the agent's spelling.
type HostUsage struct {
	At           time.Time  `json:"at"`
	Load         [3]float64 `json:"load"`
	CPUBusy      *float64   `json:"cpu_busy,omitempty"`
	MemTotal     uint64     `json:"mem_total"`
	MemAvailable uint64     `json:"mem_available"`
	SwapTotal    uint64     `json:"swap_total"`
	SwapFree     uint64     `json:"swap_free"`
	Disks        []HostDisk `json:"disks"`
}

// HostDisk is one filesystem in a usage report.
type HostDisk struct {
	Mount string `json:"mount"`
	FS    string `json:"fs"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Avail uint64 `json:"avail"`
}

const (
	maxSystemDisks = 32
	maxSystemText  = 200
	maxMountText   = 512
	maxInterfaces  = 32
	maxIfaceAddrs  = 16
)

// bounded cuts what an agent sent to sizes the console can show, and
// drops what cannot be true. Nil stays nil: "not reported".
func (s *HostSystem) bounded() *HostSystem {
	if s == nil {
		return nil
	}
	c := *s
	c.Kernel = clipText(c.Kernel, maxSystemText)
	c.Arch = clipText(c.Arch, 40)
	c.CPUModel = clipText(c.CPUModel, maxSystemText)
	c.Hypervisor = clipText(c.Hypervisor, 80)
	c.TimeZone = clipText(c.TimeZone, 80)
	if c.CPUs < 0 || c.CPUs > 1<<16 {
		c.CPUs = 0
	}
	switch c.Machine {
	case "physical", "virtual", "container":
	default:
		c.Machine = ""
	}
	switch c.Engine {
	case "attended", "unattended":
	default:
		c.Engine = ""
	}
	if c.OffsetSeconds < -14*3600 || c.OffsetSeconds > 14*3600 {
		c.OffsetSeconds = 0
	}
	c.Interfaces = boundedInterfaces(c.Interfaces)
	return &c
}

// boundedInterfaces keeps what parses: an address that is not an address,
// or a MAC that is not one, is dropped rather than shown. Each address is
// stored in its canonical spelling.
func boundedInterfaces(in []HostInterface) []HostInterface {
	if in == nil {
		return nil
	}
	out := make([]HostInterface, 0, min(len(in), maxInterfaces))
	for _, i := range in {
		if len(out) == maxInterfaces {
			break
		}
		name := clipText(i.Name, 64)
		if name == "" {
			continue
		}
		mac := ""
		if hw, err := net.ParseMAC(i.MAC); err == nil {
			mac = hw.String()
		}
		addrs := make([]string, 0, min(len(i.Addresses), maxIfaceAddrs))
		for _, a := range i.Addresses {
			if len(addrs) == maxIfaceAddrs {
				break
			}
			if p, err := netip.ParsePrefix(a); err == nil {
				addrs = append(addrs, p.String())
			}
		}
		if len(addrs) == 0 {
			continue
		}
		out = append(out, HostInterface{Name: name, MAC: mac, Addresses: addrs})
	}
	return out
}

func (u *HostUsage) bounded() *HostUsage {
	if u == nil {
		return nil
	}
	c := *u
	for i, l := range c.Load {
		if math.IsNaN(l) || math.IsInf(l, 0) || l < 0 || l > 1e6 {
			c.Load[i] = 0
		}
	}
	if c.CPUBusy != nil && (math.IsNaN(*c.CPUBusy) || *c.CPUBusy < 0 || *c.CPUBusy > 100) {
		c.CPUBusy = nil
	}
	if c.MemAvailable > c.MemTotal {
		c.MemAvailable = c.MemTotal
	}
	if c.SwapFree > c.SwapTotal {
		c.SwapFree = c.SwapTotal
	}
	disks := make([]HostDisk, 0, min(len(c.Disks), maxSystemDisks))
	for _, d := range c.Disks {
		if len(disks) == maxSystemDisks {
			break
		}
		if d.Total == 0 || d.Used > d.Total || d.Avail > d.Total {
			continue
		}
		d.Mount, d.FS = clipText(d.Mount, maxMountText), clipText(d.FS, 40)
		disks = append(disks, d)
	}
	c.Disks = disks
	return &c
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back off to a rune boundary so a cut never leaves invalid UTF-8.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// ServerSystem is the System card's data.
type ServerSystem struct {
	// Reported is false when no agent has sent system details: it is older
	// than them, or there is no agent.
	Reported bool `json:"reported"`
	// OS is the distribution's own name for itself, from os-release.
	OS     string       `json:"os"`
	System *SystemFacts `json:"system"`
	Usage  *SystemUsage `json:"usage"`
	// UsageAt is when Usage was sampled; nil when no usage was reported.
	UsageAt *time.Time `json:"usageAt"`
	// Network is the addresses the server's interfaces hold. From an agent
	// older than per-interface reporting it is one entry with no name,
	// holding the flat list that agent sends. Empty when none reported.
	Network []SystemInterface `json:"network"`
}

// SystemInterface is one network interface on the System card.
type SystemInterface struct {
	// Name is "" for the flat list of an older agent.
	Name      string          `json:"name"`
	MAC       string          `json:"mac"`
	Addresses []SystemAddress `json:"addresses"`
}

// SystemAddress is one address and what kind it is.
type SystemAddress struct {
	Address string `json:"address"`
	// Prefix is the network's length in bits, or -1 when the agent did not
	// say (an older agent's flat list).
	Prefix int `json:"prefix"`
	// Family is "ipv4" or "ipv6".
	Family string `json:"family"`
	// Public is true for a globally routable address: not private (RFC 1918
	// or IPv6 unique local), not shared address space (RFC 6598, carrier NAT)
	// and not reserved. It says what the address is, not that it can be
	// reached from the internet; a firewall may still stand in the way.
	Public bool `json:"public"`
}

// nonPublic are the ranges netip has no method for that are still not on
// the internet.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),  // shared address space, carrier NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
}

// isPublicAddr is whether a is globally routable.
func isPublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// systemAddress reads one reported address, with or without a prefix.
func systemAddress(s string) (SystemAddress, bool) {
	var (
		a      netip.Addr
		prefix = -1
	)
	if p, err := netip.ParsePrefix(s); err == nil {
		a, prefix = p.Addr(), p.Bits()
	} else if v, err := netip.ParseAddr(s); err == nil {
		a = v
	} else {
		return SystemAddress{}, false
	}
	fam := "ipv6"
	if a.Unmap().Is4() {
		fam, a = "ipv4", a.Unmap()
	}
	return SystemAddress{Address: a.String(), Prefix: prefix, Family: fam, Public: isPublicAddr(a)}, true
}

// systemNetwork is the card's network section: the interfaces when the agent
// reported them, else the flat list on the agents row.
func systemNetwork(ifs []HostInterface, flat []string) []SystemInterface {
	out := []SystemInterface{}
	for _, i := range ifs {
		si := SystemInterface{Name: i.Name, MAC: i.MAC, Addresses: []SystemAddress{}}
		for _, s := range i.Addresses {
			if a, ok := systemAddress(s); ok {
				si.Addresses = append(si.Addresses, a)
			}
		}
		if len(si.Addresses) > 0 {
			out = append(out, si)
		}
	}
	if len(out) > 0 {
		return out
	}
	si := SystemInterface{Addresses: []SystemAddress{}}
	for _, s := range flat {
		if len(si.Addresses) == maxIfaceAddrs {
			break
		}
		if a, ok := systemAddress(s); ok {
			si.Addresses = append(si.Addresses, a)
		}
	}
	if len(si.Addresses) > 0 {
		out = append(out, si)
	}
	return out
}

// SystemFacts is HostSystem as the console spells it.
type SystemFacts struct {
	Kernel        string     `json:"kernel"`
	Arch          string     `json:"arch"`
	CPUModel      string     `json:"cpuModel"`
	CPUs          int        `json:"cpus"`
	MemoryBytes   uint64     `json:"memoryBytes"`
	Machine       string     `json:"machine"`
	Hypervisor    string     `json:"hypervisor"`
	BootedAt      *time.Time `json:"bootedAt"`
	TimeZone      string     `json:"timeZone"`
	OffsetSeconds int        `json:"utcOffsetSeconds"`
	// Engine and WirelessDebugging are an Android device's; empty and false
	// for a server. See HostSystem.
	Engine            string `json:"engine,omitempty"`
	WirelessDebugging bool   `json:"wirelessDebugging,omitempty"`
}

// SystemUsage is HostUsage as the console spells it.
type SystemUsage struct {
	Load         [3]float64   `json:"load"`
	CPUBusy      *float64     `json:"cpuBusy"`
	MemTotal     uint64       `json:"memTotal"`
	MemAvailable uint64       `json:"memAvailable"`
	SwapTotal    uint64       `json:"swapTotal"`
	SwapFree     uint64       `json:"swapFree"`
	Disks        []SystemDisk `json:"disks"`
}

// SystemDisk is one filesystem on the System card.
type SystemDisk struct {
	Mount string `json:"mount"`
	FS    string `json:"fs"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Avail uint64 `json:"avail"`
}

// SystemFor reads a server's system details from the agent that reports for
// it, matched the way the heartbeat matches.
func (s *Store) SystemFor(ctx context.Context, id string) (ServerSystem, error) {
	var (
		osName       *string
		sysRaw, uRaw []byte
		usageAt      *time.Time
		flat         []string
	)
	err := s.db(ctx).QueryRow(ctx, `
		SELECT ag.os, ag.system, ag.usage, ag.usage_at, ag.addresses
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT os, system, usage, usage_at, addresses FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.id::text = $1 AND a.archived_at IS NULL`, id).Scan(&osName, &sysRaw, &uRaw, &usageAt, &flat)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerSystem{}, ErrAssetNotFound
	}
	if err != nil {
		return ServerSystem{}, err
	}
	out := ServerSystem{Network: []SystemInterface{}}
	if osName != nil {
		out.OS = *osName
	}
	var hs HostSystem
	if len(sysRaw) > 0 && string(sysRaw) != "null" && json.Unmarshal(sysRaw, &hs) == nil {
		out.Reported = true
		out.System = &SystemFacts{
			Kernel: hs.Kernel, Arch: hs.Arch, CPUModel: hs.CPUModel, CPUs: hs.CPUs,
			MemoryBytes: hs.MemoryBytes, Machine: hs.Machine, Hypervisor: hs.Hypervisor,
			BootedAt: hs.BootedAt, TimeZone: hs.TimeZone, OffsetSeconds: hs.OffsetSeconds,
			Engine: hs.Engine, WirelessDebugging: hs.WirelessDebugging,
		}
	}
	out.Network = systemNetwork(hs.Interfaces, flat)
	var hu HostUsage
	if len(uRaw) > 0 && string(uRaw) != "null" && json.Unmarshal(uRaw, &hu) == nil {
		out.Reported = true
		disks := make([]SystemDisk, 0, len(hu.Disks))
		for _, d := range hu.Disks {
			disks = append(disks, SystemDisk(d))
		}
		out.Usage = &SystemUsage{
			Load: hu.Load, CPUBusy: hu.CPUBusy, MemTotal: hu.MemTotal, MemAvailable: hu.MemAvailable,
			SwapTotal: hu.SwapTotal, SwapFree: hu.SwapFree, Disks: disks,
		}
		out.UsageAt = usageAt
	}
	return out, nil
}

// getSystem serves GET /api/v1/assets/{id}/system. Fleet roles only, like
// what runs on a server: a kernel version is a reconnaissance fact.
func (a *API) getSystem(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing a server's system details requires the admin, owner or auditor role")
		return
	}
	out, err := a.store.SystemFor(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "system", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

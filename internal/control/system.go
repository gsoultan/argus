package control

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// CanInstallApps is whether the Argus app may install apps: Android's
	// "Install unknown apps" for it, which the person allows once. Nil from
	// a server, or an app older than installing.
	CanInstallApps *bool `json:"can_install_apps,omitempty"`
	// Cloud is the cloud instance the agent says this machine is, from the
	// cloud's metadata service. Nil off a cloud, or from an older agent.
	Cloud *HostCloud `json:"cloud,omitempty"`
}

// HostCloud mirrors agent.CloudInstance.
type HostCloud struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Region   string `json:"region"`
	Zone     string `json:"zone,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
}

var (
	hostAWSInstanceRE = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	hostAWSAccountRE  = regexp.MustCompile(`^[0-9]{12}$`)
	hostGCPInstanceRE = regexp.MustCompile(`^[0-9]{1,20}$`)
	hostGCPProjectRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	hostCloudPlaceRE  = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)
	hostCloudTypeRE   = regexp.MustCompile(`^[a-z0-9.-]{1,60}$`)
)

// bounded keeps a cloud identity only when every part looks like one: it is
// what links a server to an instance, so a malformed one is dropped whole
// rather than half-kept.
func (c *HostCloud) bounded() *HostCloud {
	if c == nil {
		return nil
	}
	if !hostCloudPlaceRE.MatchString(c.Region) || (c.Zone != "" && !hostCloudPlaceRE.MatchString(c.Zone)) ||
		(c.Type != "" && !hostCloudTypeRE.MatchString(c.Type)) {
		return nil
	}
	switch c.Provider {
	case "aws":
		if !hostAWSInstanceRE.MatchString(c.ID) || !hostAWSAccountRE.MatchString(c.Account) {
			return nil
		}
	case "gcp":
		if !hostGCPInstanceRE.MatchString(c.ID) || !hostGCPProjectRE.MatchString(c.Account) {
			return nil
		}
	default:
		return nil
	}
	out := *c
	return &out
}

// HostInterface mirrors agent.NetInterface.
type HostInterface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses"`
}

// HostUsage mirrors agent.Usage, in the agent's spelling.
type HostUsage struct {
	At time.Time `json:"at"`
	// Load is nil where the system keeps no load average (Windows).
	Load         *[3]float64 `json:"load,omitempty"`
	CPUBusy      *float64    `json:"cpu_busy,omitempty"`
	MemTotal     uint64      `json:"mem_total"`
	MemAvailable uint64      `json:"mem_available"`
	SwapTotal    uint64      `json:"swap_total"`
	SwapFree     uint64      `json:"swap_free"`
	Disks        []HostDisk  `json:"disks"`
	// Net is each interface's traffic since the agent's previous sample;
	// nil from an agent older than traffic reporting.
	Net []HostNetRate `json:"net,omitempty"`
	// Steal, TCPConns and DiskIO are nil from an agent older than them, and
	// where the platform keeps no such figure (steal on Windows).
	// TCPConns is a count, read as a number so a figure that cannot be one
	// is dropped by bounded rather than failing the heartbeat it rides.
	Steal    *float64     `json:"steal,omitempty"`
	TCPConns *float64     `json:"tcp_conns,omitempty"`
	DiskIO   []HostDiskIO `json:"disk_io,omitempty"`
	// Top is what is using the machine now, at most maxTopProcs; nil from
	// an agent older than it.
	Top []HostProcess `json:"top,omitempty"`
}

// HostProcess mirrors agent.Process: a program's name (never its command
// line), pid, share of the machine's processor and resident memory.
type HostProcess struct {
	Name   string   `json:"name"`
	PID    int      `json:"pid"`
	CPU    *float64 `json:"cpu,omitempty"`
	Memory float64  `json:"memory"`
}

// HostDiskIO mirrors agent.DiskIO: one physical disk's operations and bytes
// per second since the agent's previous sample.
type HostDiskIO struct {
	Name     string   `json:"name"`
	ReadOps  *float64 `json:"read_ops,omitempty"`
	WriteOps *float64 `json:"write_ops,omitempty"`
	ReadBps  *float64 `json:"read_bps,omitempty"`
	WriteBps *float64 `json:"write_bps,omitempty"`
}

// HostNetRate mirrors agent.NetRate.
type HostNetRate struct {
	Name      string   `json:"name"`
	RxBps     *float64 `json:"rx_bps,omitempty"`
	TxBps     *float64 `json:"tx_bps,omitempty"`
	SpeedMbps uint64   `json:"speed_mbps,omitempty"`
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
	maxNetRates    = 32
	maxDiskIO      = 16
	maxTopProcs    = 10
	// maxProcMemory is 64 TiB, past any machine's memory.
	maxProcMemory = 1 << 46
	// maxDiskOps is ten million operations a second, past any one disk
	// made; more is a broken counter.
	maxDiskOps = 1e7
	// maxTCPConns is past what any one kernel will hold open.
	maxTCPConns = 1 << 26
	// maxRateBps is 1 Tbit/s in bytes: no interface carries more, so a
	// larger rate is a broken counter, not traffic.
	maxRateBps = 125e9
	// maxLinkMbps is 1.6 Tbit/s, past the fastest Ethernet there is.
	maxLinkMbps = 1_600_000
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
	c.Cloud = c.Cloud.bounded()
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
	if c.Load != nil {
		l := *c.Load
		for i, v := range l {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e6 {
				l[i] = 0
			}
		}
		c.Load = &l
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
	c.Net = boundedNetRates(c.Net)
	if c.Steal != nil && (math.IsNaN(*c.Steal) || *c.Steal < 0 || *c.Steal > 100) {
		c.Steal = nil
	}
	if c.TCPConns != nil {
		if v := *c.TCPConns; math.IsNaN(v) || v < 0 || v > maxTCPConns {
			c.TCPConns = nil
		} else {
			v = math.Round(v)
			c.TCPConns = &v
		}
	}
	c.DiskIO = boundedDiskIO(c.DiskIO)
	c.Top = boundedTop(c.Top)
	return &c
}

// boundedTop keeps at most maxTopProcs processes with a printable name and
// figures that can be true. A name is the program's own, so it is shown as
// text, never markup, and cut to what a row holds.
func boundedTop(in []HostProcess) []HostProcess {
	if in == nil {
		return nil
	}
	out := make([]HostProcess, 0, min(len(in), maxTopProcs))
	for _, p := range in {
		if len(out) == maxTopProcs {
			break
		}
		name := clipText(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || r == utf8.RuneError {
				return -1
			}
			return r
		}, p.Name), 64)
		if name == "" || p.PID <= 0 || math.IsNaN(p.Memory) || p.Memory < 0 || p.Memory > maxProcMemory {
			continue
		}
		q := HostProcess{Name: name, PID: p.PID, Memory: math.Round(p.Memory)}
		if p.CPU != nil && !math.IsNaN(*p.CPU) && *p.CPU >= 0 && *p.CPU <= 100 {
			v := *p.CPU
			q.CPU = &v
		}
		out = append(out, q)
	}
	return out
}

// boundedDiskIO keeps at most maxDiskIO named disks, dropping a rate no
// disk does.
func boundedDiskIO(in []HostDiskIO) []HostDiskIO {
	if in == nil {
		return nil
	}
	within := func(p *float64, limit float64) *float64 {
		if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || *p > limit {
			return nil
		}
		v := *p
		return &v
	}
	out := make([]HostDiskIO, 0, min(len(in), maxDiskIO))
	for _, d := range in {
		if len(out) == maxDiskIO {
			break
		}
		name := clipText(d.Name, 64)
		if name == "" {
			continue
		}
		out = append(out, HostDiskIO{Name: name,
			ReadOps: within(d.ReadOps, maxDiskOps), WriteOps: within(d.WriteOps, maxDiskOps),
			ReadBps: within(d.ReadBps, maxRateBps), WriteBps: within(d.WriteBps, maxRateBps)})
	}
	return out
}

// boundedNetRates keeps at most maxNetRates named interfaces, dropping a
// rate that cannot be traffic and a link speed no link has.
func boundedNetRates(in []HostNetRate) []HostNetRate {
	if in == nil {
		return nil
	}
	rate := func(p *float64) *float64 {
		if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || *p > maxRateBps {
			return nil
		}
		v := *p
		return &v
	}
	out := make([]HostNetRate, 0, min(len(in), maxNetRates))
	for _, n := range in {
		if len(out) == maxNetRates {
			break
		}
		name := clipText(n.Name, 64)
		if name == "" {
			continue
		}
		speed := n.SpeedMbps
		if speed > maxLinkMbps {
			speed = 0
		}
		out = append(out, HostNetRate{Name: name, RxBps: rate(n.RxBps), TxBps: rate(n.TxBps), SpeedMbps: speed})
	}
	return out
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
	// Warnings is each resource running low in Usage, by the one rule in
	// resources.go. Empty when nothing is low or nothing was reported.
	Warnings []ResourceWarning `json:"warnings"`
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
	CanInstallApps    *bool  `json:"canInstallApps,omitempty"`
}

// SystemUsage is HostUsage as the console spells it.
type SystemUsage struct {
	// Load is null where the system keeps no load average (Windows).
	Load         *[3]float64  `json:"load"`
	CPUBusy      *float64     `json:"cpuBusy"`
	MemTotal     uint64       `json:"memTotal"`
	MemAvailable uint64       `json:"memAvailable"`
	SwapTotal    uint64       `json:"swapTotal"`
	SwapFree     uint64       `json:"swapFree"`
	Disks        []SystemDisk `json:"disks"`
	// Net is each interface's traffic over the last sample; empty from an
	// agent that does not report it.
	Net []SystemNetRate `json:"net"`
	// Steal and TCPConns are null where the agent does not report them.
	Steal    *float64 `json:"steal"`
	TCPConns *float64 `json:"tcpConns"`
	// Top is what is using the machine now; empty where not reported.
	Top []SystemProcess `json:"top"`
}

// SystemProcess is one row of what is using a server now.
type SystemProcess struct {
	Name   string   `json:"name"`
	PID    int      `json:"pid"`
	CPU    *float64 `json:"cpu"`
	Memory float64  `json:"memory"`
}

// SystemNetRate is one interface's traffic on the System card.
type SystemNetRate struct {
	Name string `json:"name"`
	// RxBps and TxBps are bytes per second; null when the agent had no
	// earlier reading to measure against.
	RxBps     *float64 `json:"rxBps"`
	TxBps     *float64 `json:"txBps"`
	SpeedMbps uint64   `json:"speedMbps"`
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
	out := ServerSystem{Network: []SystemInterface{}, Warnings: []ResourceWarning{}}
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
			Engine: hs.Engine, WirelessDebugging: hs.WirelessDebugging, CanInstallApps: hs.CanInstallApps,
		}
	}
	out.Network = systemNetwork(hs.Interfaces, flat)
	var hu HostUsage
	if len(uRaw) > 0 && string(uRaw) != "null" && json.Unmarshal(uRaw, &hu) == nil {
		out.Reported = true
		out.Usage = systemUsageOf(hu)
		out.UsageAt = usageAt
		out.Warnings = LowResources(hu)
	}
	return out, nil
}

// systemUsageOf is a usage report as the System card shows it.
func systemUsageOf(hu HostUsage) *SystemUsage {
	disks := make([]SystemDisk, 0, len(hu.Disks))
	for _, d := range hu.Disks {
		disks = append(disks, SystemDisk(d))
	}
	net := make([]SystemNetRate, 0, len(hu.Net))
	for _, n := range hu.Net {
		net = append(net, SystemNetRate{Name: n.Name, RxBps: n.RxBps, TxBps: n.TxBps, SpeedMbps: n.SpeedMbps})
	}
	u := &SystemUsage{
		Load: hu.Load, CPUBusy: hu.CPUBusy, MemTotal: hu.MemTotal, MemAvailable: hu.MemAvailable,
		SwapTotal: hu.SwapTotal, SwapFree: hu.SwapFree, Disks: disks, Net: net,
		Steal: hu.Steal, TCPConns: hu.TCPConns, Top: make([]SystemProcess, 0, len(hu.Top)),
	}
	for _, p := range boundedTop(hu.Top) {
		u.Top = append(u.Top, SystemProcess(p))
	}
	return u
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

// AssetAddress is one address a server's agent reported, for the assets list.
type AssetAddress struct {
	Address string `json:"address"`
	Family  string `json:"family"`
	Public  bool   `json:"public"`
	// Interface is "" for an older agent's flat list.
	Interface string `json:"interface,omitempty"`
}

// flatAddresses is systemNetwork as one list, each address once.
func flatAddresses(ifs []SystemInterface) []AssetAddress {
	var out []AssetAddress
	seen := map[string]bool{}
	for _, i := range ifs {
		for _, a := range i.Addresses {
			if seen[a.Address] {
				continue
			}
			seen[a.Address] = true
			out = append(out, AssetAddress{Address: a.Address, Family: a.Family, Public: a.Public, Interface: i.Name})
		}
	}
	return out
}

// AgentInstances is the cloud instance each live asset's agent reported,
// keyed by asset id, matched to its agent the way AgentAddresses matches.
func (s *Store) AgentInstances(ctx context.Context) (map[string]HostCloud, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, ag.system->'cloud'
		  FROM assets a
		  JOIN LATERAL (
		       SELECT system FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL AND ag.system ? 'cloud'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]HostCloud{}
	for rows.Next() {
		var (
			id  string
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var c HostCloud
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		// Checked again on the way out: rows written before the check, or by
		// hand, are input too.
		if b := c.bounded(); b != nil {
			out[id] = *b
		}
	}
	return out, rows.Err()
}

// AgentAddresses is every live asset's reported addresses, keyed by asset
// id, in one query: the agent matched the way SystemFor matches it.
func (s *Store) AgentAddresses(ctx context.Context) (map[string][]AssetAddress, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, ag.system->'interfaces', ag.addresses
		  FROM assets a
		  JOIN LATERAL (
		       SELECT system, addresses FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]AssetAddress{}
	for rows.Next() {
		var (
			id   string
			raw  []byte
			flat []string
			ifs  []HostInterface
		)
		if err := rows.Scan(&id, &raw, &flat); err != nil {
			return nil, err
		}
		if len(raw) > 0 && string(raw) != "null" {
			// A malformed report reads as none and falls back to the flat list.
			_ = json.Unmarshal(raw, &ifs)
		}
		if addrs := flatAddresses(systemNetwork(ifs, flat)); len(addrs) > 0 {
			out[id] = addrs
		}
	}
	return out, rows.Err()
}

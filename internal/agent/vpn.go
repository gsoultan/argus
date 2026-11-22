package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

/*
How the VPNs and tunnels on this host are doing.

apps.go already says which VPN software is installed and running. This reads
the state of their links, which changes by the minute:

  - WireGuard: every interface and its peers, from `wg show <iface> <field>`,
    one field at a time. Never `wg show all dump` or `showconf`: those print
    the interface's private key and any preshared keys. Only public keys,
    endpoints, allowed IPs, the last handshake and byte counters leave here.
  - Cloudflare Tunnel: each cloudflared's own metrics server, which it binds
    to loopback (20241-20245 by default). GET /ready says how many
    connections to Cloudflare are up and the connector's ID, which the
    console matches to the tunnel in the Cloudflare account; /metrics carries
    its version. This is the one place the agent asks a program it found
    anything, and it is deliberate: loopback only, two read-only paths that
    exist for health checks, short timeouts, bounded bodies. cloudflared's
    config and credentials file are never read.

Read-only throughout. Nothing here changes a link.
*/

// VPNScan is what the agent reports about its VPN links.
type VPNScan struct {
	CheckedAt time.Time `json:"checked_at"`
	// WireGuard is every WireGuard interface on the host.
	WireGuard []WGInterface `json:"wireguard"`
	// Cloudflared is every cloudflared whose metrics answered, or failed to.
	Cloudflared []CFConnector `json:"cloudflared"`
	// Sources says what could be read: "wg" when the wg tool answered,
	// "sysfs" when only the kernel's interface list could be read.
	Sources []string `json:"sources"`
}

// WGInterface is one WireGuard interface.
type WGInterface struct {
	Name       string   `json:"name"`
	PublicKey  string   `json:"public_key,omitempty"`
	ListenPort int      `json:"listen_port,omitempty"`
	Addresses  []string `json:"addresses,omitempty"`
	Up         bool     `json:"up"`
	RxBytes    uint64   `json:"rx_bytes"`
	TxBytes    uint64   `json:"tx_bytes"`
	Peers      []WGPeer `json:"peers"`
}

// WGPeer is one peer of a WireGuard interface.
type WGPeer struct {
	PublicKey  string   `json:"public_key"`
	Endpoint   string   `json:"endpoint,omitempty"`
	AllowedIPs []string `json:"allowed_ips,omitempty"`
	// LatestHandshake is when the last handshake completed; zero is never.
	LatestHandshake time.Time `json:"latest_handshake,omitempty"`
	// Keepalive is the persistent keepalive in seconds, 0 when off. With it
	// off, a healthy link with no traffic stops handshaking, so a stale
	// handshake is "idle" rather than "down".
	Keepalive int    `json:"keepalive,omitempty"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
}

// CFConnector is one cloudflared on the host.
type CFConnector struct {
	// Metrics is the loopback address its metrics server answered on.
	Metrics          string `json:"metrics"`
	Ready            bool   `json:"ready"`
	ReadyConnections int    `json:"ready_connections"`
	ConnectorID      string `json:"connector_id,omitempty"`
	Version          string `json:"version,omitempty"`
	Error            string `json:"error,omitempty"`
}

// HandshakeFresh is how recent a WireGuard handshake must be for a link to
// count as up. WireGuard re-handshakes every two minutes while traffic
// flows; a peer with persistent keepalive never goes quiet for longer.
const HandshakeFresh = 3 * time.Minute

// Bounds on a scan, so a host with an unusual setup still reports in one
// sane-sized message.
const (
	maxWGInterfaces = 16
	maxWGPeers      = 256
	maxConnectors   = 8
	maxMetricsBody  = 1 << 20
)

// runner runs a command and returns its standard output. Swapped in tests.
type runner func(ctx context.Context, name string, args ...string) (string, error)

// DiscoverVPN reads the host's VPN links. listeners is the last application
// scan's listening sockets, which name cloudflared's metrics port.
func DiscoverVPN(ctx context.Context, listeners []Listener) VPNScan {
	return discoverVPN(ctx, platformRun, wgSysfsNames, listeners, http.DefaultTransport, time.Now())
}

func discoverVPN(ctx context.Context, run runner, sysfs func() []string, listeners []Listener, rt http.RoundTripper, now time.Time) VPNScan {
	out := VPNScan{CheckedAt: now.UTC(), WireGuard: []WGInterface{}, Cloudflared: []CFConnector{}, Sources: []string{}}
	if ifs, ok := wireguardInterfaces(ctx, run); ok {
		out.WireGuard = ifs
		out.Sources = append(out.Sources, "wg")
	} else if names := sysfs(); names != nil {
		// No wg tool (or not allowed to run it): the kernel still lists the
		// interfaces, so the console can say a link exists without its peers.
		for _, n := range names {
			out.WireGuard = append(out.WireGuard, WGInterface{Name: n, Peers: []WGPeer{}})
		}
		out.Sources = append(out.Sources, "sysfs")
	}
	for i := range out.WireGuard {
		fillInterface(&out.WireGuard[i])
	}
	out.Cloudflared = cloudflaredConnectors(ctx, listeners, rt)
	return out
}

// wireguardInterfaces asks wg for each interface, one public field at a time.
func wireguardInterfaces(ctx context.Context, run runner) ([]WGInterface, bool) {
	names, err := run(ctx, "wg", "show", "interfaces")
	if err != nil {
		return nil, false
	}
	out := []WGInterface{}
	for _, name := range strings.Fields(names) {
		if len(out) == maxWGInterfaces {
			break
		}
		if !validIfName(name) {
			continue
		}
		wi := WGInterface{Name: name, Peers: []WGPeer{}}
		if s, err := run(ctx, "wg", "show", name, "public-key"); err == nil {
			wi.PublicKey = strings.TrimSpace(s)
		}
		if s, err := run(ctx, "wg", "show", name, "listen-port"); err == nil {
			wi.ListenPort, _ = strconv.Atoi(strings.TrimSpace(s))
		}
		peers := map[string]*WGPeer{}
		order := []string{}
		peer := func(k string) *WGPeer {
			if p, ok := peers[k]; ok {
				return p
			}
			if len(order) == maxWGPeers {
				return nil
			}
			peers[k] = &WGPeer{PublicKey: k}
			order = append(order, k)
			return peers[k]
		}
		each := func(field string, f func(p *WGPeer, rest []string)) {
			s, err := run(ctx, "wg", "show", name, field)
			if err != nil {
				return
			}
			for _, line := range strings.Split(s, "\n") {
				parts := strings.Fields(line)
				if len(parts) < 2 {
					continue
				}
				if p := peer(parts[0]); p != nil {
					f(p, parts[1:])
				}
			}
		}
		each("endpoints", func(p *WGPeer, r []string) {
			if r[0] != "(none)" {
				p.Endpoint = r[0]
			}
		})
		each("allowed-ips", func(p *WGPeer, r []string) {
			for _, c := range r {
				if c != "(none)" {
					p.AllowedIPs = append(p.AllowedIPs, c)
				}
			}
		})
		each("latest-handshakes", func(p *WGPeer, r []string) {
			if sec, err := strconv.ParseInt(r[0], 10, 64); err == nil && sec > 0 {
				p.LatestHandshake = time.Unix(sec, 0).UTC()
			}
		})
		each("persistent-keepalive", func(p *WGPeer, r []string) {
			p.Keepalive, _ = strconv.Atoi(r[0]) // "off" is 0
		})
		each("transfer", func(p *WGPeer, r []string) {
			if len(r) >= 2 {
				p.RxBytes, _ = strconv.ParseUint(r[0], 10, 64)
				p.TxBytes, _ = strconv.ParseUint(r[1], 10, 64)
			}
		})
		for _, k := range order {
			wi.Peers = append(wi.Peers, *peers[k])
		}
		out = append(out, wi)
	}
	return out, true
}

// validIfName is a Linux interface name: at most 15 characters, none of
// them a separator. wg prints them; this keeps a surprise out of argv.
func validIfName(s string) bool {
	if s == "" || len(s) > 15 || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if r == '/' || r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// fillInterface adds the interface's own addresses, state and counters.
func fillInterface(wi *WGInterface) {
	ifc, err := net.InterfaceByName(wi.Name)
	if err != nil {
		return
	}
	wi.Up = ifc.Flags&net.FlagUp != 0
	if addrs, err := ifc.Addrs(); err == nil {
		for _, a := range addrs {
			wi.Addresses = append(wi.Addresses, a.String())
		}
	}
	wi.RxBytes, wi.TxBytes = wgCounters(wi.Name)
}

// cloudflaredConnectors asks each cloudflared's loopback metrics server how
// its connections are.
func cloudflaredConnectors(ctx context.Context, listeners []Listener, rt http.RoundTripper) []CFConnector {
	out := []CFConnector{}
	seen := map[string]bool{}
	for _, l := range listeners {
		if l.Process != "cloudflared" || len(out) == maxConnectors {
			continue
		}
		ip := net.ParseIP(l.Address)
		if ip == nil || !ip.IsLoopback() {
			continue // only ever loopback
		}
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(l.Port))
		if seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, readConnector(ctx, rt, addr))
	}
	return out
}

func readConnector(ctx context.Context, rt http.RoundTripper, addr string) CFConnector {
	c := CFConnector{Metrics: addr}
	client := &http.Client{Transport: rt, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			return nil, 0, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer res.Body.Close()
		b, err := io.ReadAll(io.LimitReader(res.Body, maxMetricsBody))
		return b, res.StatusCode, err
	}
	b, code, err := get("/ready")
	if err != nil {
		c.Error = "metrics did not answer"
		return c
	}
	var ready struct {
		Status           int    `json:"status"`
		ReadyConnections int    `json:"readyConnections"`
		ConnectorID      string `json:"connectorId"`
	}
	if json.Unmarshal(b, &ready) != nil {
		// Some other program on a port cloudflared once held, or a very old
		// cloudflared without /ready.
		c.Error = fmt.Sprintf("no readiness answer (HTTP %d)", code)
		return c
	}
	c.ReadyConnections = ready.ReadyConnections
	c.ConnectorID = cutTo(ready.ConnectorID, 64)
	c.Ready = code == http.StatusOK && ready.ReadyConnections > 0
	if mb, _, err := get("/metrics"); err == nil {
		c.Version = cloudflaredVersion(string(mb))
	}
	return c
}

// cloudflaredVersion finds version="..." on the build_info line.
func cloudflaredVersion(metrics string) string {
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, "build_info{") {
			continue
		}
		// goversion="..." comes first; the label wanted follows { or ,.
		i := strings.Index(line, `,version="`)
		if i < 0 {
			i = strings.Index(line, `{version="`)
		}
		if i < 0 {
			return ""
		}
		rest := line[i+len(`,version="`):]
		v, _, _ := strings.Cut(rest, `"`)
		return cutTo(v, 32)
	}
	return ""
}

func cutTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// VPNState is the part of a scan that says up or down, for deciding when to
// report early: a link going down should reach the console in a minute, not
// at the next ten-minute facts pass. Counters and handshake times move all
// the time and are left out.
func (v VPNScan) State(now time.Time) string {
	var parts []string
	for _, wi := range v.WireGuard {
		parts = append(parts, fmt.Sprintf("wg:%s:%t:%d", wi.Name, wi.Up, len(wi.Peers)))
		for _, p := range wi.Peers {
			fresh := !p.LatestHandshake.IsZero() && now.Sub(p.LatestHandshake) < HandshakeFresh
			parts = append(parts, fmt.Sprintf("p:%s:%t", p.PublicKey, fresh))
		}
	}
	for _, c := range v.Cloudflared {
		parts = append(parts, fmt.Sprintf("cf:%s:%t:%s", c.Metrics, c.Ready, c.ConnectorID))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

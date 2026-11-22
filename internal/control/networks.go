package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

/*
Networks: the VPNs and tunnels on the servers Argus manages, as their agents
report them (internal/agent/vpn.go and the VPN entries of the application
catalogue).

Read-only. What is said here comes from the agents alone, so the page states
when it was read and says "not known" for a server whose agent has gone
quiet, rather than repeating a link's last state as if it were current.

A link's state is judged at the time the agent read it, not now: the agent
reports within about a minute whenever a link goes up or down, and otherwise
hourly, so a report an hour old is still current while the agent keeps
checking in.
*/

// VPNScan mirrors agent.VPNScan, in the agent's spelling.
type VPNScan struct {
	CheckedAt   time.Time     `json:"checked_at"`
	WireGuard   []WGInterface `json:"wireguard"`
	Cloudflared []CFConnector `json:"cloudflared"`
	Sources     []string      `json:"sources"`
}

// WGInterface mirrors agent.WGInterface.
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

// WGPeer mirrors agent.WGPeer.
type WGPeer struct {
	PublicKey       string    `json:"public_key"`
	Endpoint        string    `json:"endpoint,omitempty"`
	AllowedIPs      []string  `json:"allowed_ips,omitempty"`
	LatestHandshake time.Time `json:"latest_handshake,omitempty"`
	Keepalive       int       `json:"keepalive,omitempty"`
	RxBytes         uint64    `json:"rx_bytes"`
	TxBytes         uint64    `json:"tx_bytes"`
}

// CFConnector mirrors agent.CFConnector.
type CFConnector struct {
	Metrics          string `json:"metrics"`
	Ready            bool   `json:"ready"`
	ReadyConnections int    `json:"ready_connections"`
	ConnectorID      string `json:"connector_id,omitempty"`
	Version          string `json:"version,omitempty"`
	Error            string `json:"error,omitempty"`
}

const (
	maxVPNInterfaces = 16
	maxVPNPeers      = 256
	maxVPNConnectors = 8
	maxVPNAddrs      = 32
	// handshakeFresh matches agent.HandshakeFresh.
	handshakeFresh = 3 * time.Minute
	// agentQuiet is how long without a heartbeat before what an agent last
	// said about its links is no longer stated as current.
	agentQuiet = 5 * time.Minute
)

// bounded trims a report to what a row holds. Nil stays nil: an agent older
// than VPN discovery leaves the row alone.
func (v *VPNScan) bounded() *VPNScan {
	if v == nil {
		return nil
	}
	out := &VPNScan{CheckedAt: v.CheckedAt, WireGuard: []WGInterface{}, Cloudflared: []CFConnector{},
		Sources: cutStrings(v.Sources, 8)}
	for i, wi := range v.WireGuard {
		if i == maxVPNInterfaces {
			break
		}
		w := WGInterface{Name: cut(wi.Name), PublicKey: cut(wi.PublicKey), ListenPort: wi.ListenPort,
			Addresses: cutStrings(wi.Addresses, maxVPNAddrs), Up: wi.Up, RxBytes: wi.RxBytes, TxBytes: wi.TxBytes, Peers: []WGPeer{}}
		for j, p := range wi.Peers {
			if j == maxVPNPeers {
				break
			}
			w.Peers = append(w.Peers, WGPeer{PublicKey: cut(p.PublicKey), Endpoint: cut(p.Endpoint),
				AllowedIPs: cutStrings(p.AllowedIPs, maxVPNAddrs), LatestHandshake: p.LatestHandshake,
				Keepalive: p.Keepalive, RxBytes: p.RxBytes, TxBytes: p.TxBytes})
		}
		out.WireGuard = append(out.WireGuard, w)
	}
	for i, c := range v.Cloudflared {
		if i == maxVPNConnectors {
			break
		}
		out.Cloudflared = append(out.Cloudflared, CFConnector{Metrics: cut(c.Metrics), Ready: c.Ready,
			ReadyConnections: c.ReadyConnections, ConnectorID: cut(c.ConnectorID), Version: cut(c.Version), Error: cut(c.Error)})
	}
	return out
}

// vpnKinds are the catalogue kinds that are VPNs or tunnels, and what the
// page calls each.
var vpnKinds = map[string]string{
	"wireguard": "WireGuard", "cloudflared": "Cloudflare Tunnel", "warp": "Cloudflare WARP",
	"openvpn": "OpenVPN", "strongswan": "strongSwan (IPsec)", "libreswan": "Libreswan (IPsec)",
	"tailscale": "Tailscale", "zerotier": "ZeroTier",
}

// NetworkServer is one server whose agent can report VPN links.
type NetworkServer struct {
	AssetID  string `json:"assetId"`
	Hostname string `json:"hostname"`
	// Reported is whether its agent has sent a VPN scan at all; an older
	// agent has not.
	Reported  bool       `json:"reported"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	// Quiet is an agent that has not checked in lately: what it last said
	// is shown, but not as current.
	Quiet    bool       `json:"quiet"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
	// WGTool is false when only the kernel's interface list could be read.
	WGTool bool `json:"wgTool"`
}

// NetworkSoftware is VPN or tunnel software found on a server.
type NetworkSoftware struct {
	AssetID   string `json:"assetId"`
	Hostname  string `json:"hostname"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Running   bool   `json:"running"`
	Unit      string `json:"unit,omitempty"`
	Container string `json:"container,omitempty"`
}

// NetworkPeer is one WireGuard peer, judged.
type NetworkPeer struct {
	PublicKey       string     `json:"publicKey"`
	Endpoint        string     `json:"endpoint,omitempty"`
	AllowedIPs      []string   `json:"allowedIps"`
	LatestHandshake *time.Time `json:"latestHandshake,omitempty"`
	Keepalive       int        `json:"keepalive,omitempty"`
	RxBytes         uint64     `json:"rxBytes"`
	TxBytes         uint64     `json:"txBytes"`
	// State is up, down, idle (no recent handshake on a link that only
	// handshakes when there is traffic, or that waits to be dialled),
	// never (configured to dial, never connected), or unknown (agent quiet).
	State string `json:"state"`
	// The other end, when it is an interface on another Argus server.
	PeerAssetID   string `json:"peerAssetId,omitempty"`
	PeerHostname  string `json:"peerHostname,omitempty"`
	PeerInterface string `json:"peerInterface,omitempty"`
}

// NetworkInterface is one WireGuard interface on a server.
type NetworkInterface struct {
	AssetID    string        `json:"assetId"`
	Hostname   string        `json:"hostname"`
	Name       string        `json:"name"`
	PublicKey  string        `json:"publicKey,omitempty"`
	ListenPort int           `json:"listenPort,omitempty"`
	Addresses  []string      `json:"addresses"`
	Up         bool          `json:"up"`
	RxBytes    uint64        `json:"rxBytes"`
	TxBytes    uint64        `json:"txBytes"`
	Peers      []NetworkPeer `json:"peers"`
	// PeersKnown is false when the agent could not run wg, so the peers
	// are not known rather than absent.
	PeersKnown bool `json:"peersKnown"`
}

// NetworkTunnel is one cloudflared on a server.
type NetworkTunnel struct {
	AssetID          string `json:"assetId"`
	Hostname         string `json:"hostname"`
	Metrics          string `json:"metrics"`
	State            string `json:"state"` // up, down, unknown
	ReadyConnections int    `json:"readyConnections"`
	ConnectorID      string `json:"connectorId,omitempty"`
	Version          string `json:"version,omitempty"`
	Error            string `json:"error,omitempty"`
}

// NetworkProblem is something on the page worth a look, most serious first.
type NetworkProblem struct {
	Severity string `json:"severity"` // down, warn
	Kind     string `json:"kind"`
	AssetID  string `json:"assetId"`
	Hostname string `json:"hostname"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// NetworksView is the Networks page.
type NetworksView struct {
	Servers    []NetworkServer    `json:"servers"`
	Software   []NetworkSoftware  `json:"software"`
	Interfaces []NetworkInterface `json:"interfaces"`
	Tunnels    []NetworkTunnel    `json:"tunnels"`
	Problems   []NetworkProblem   `json:"problems"`
}

// networkRow is one server's reports, as read from the database.
type networkRow struct {
	assetID, hostname string
	lastSeen          *time.Time
	apps, vpn         []byte
}

// Networks reads every server's VPN links.
func (s *Store) Networks(ctx context.Context, now time.Time) (NetworksView, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, a.hostname, ag.last_seen_at, ag.applications, ag.vpn
		  FROM assets a
		  JOIN LATERAL (
		       SELECT last_seen_at, applications, vpn FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL
		 ORDER BY a.hostname`)
	if err != nil {
		return NetworksView{}, err
	}
	defer rows.Close()
	var in []networkRow
	for rows.Next() {
		var r networkRow
		if err := rows.Scan(&r.assetID, &r.hostname, &r.lastSeen, &r.apps, &r.vpn); err != nil {
			return NetworksView{}, err
		}
		in = append(in, r)
	}
	if err := rows.Err(); err != nil {
		return NetworksView{}, err
	}
	return buildNetworks(in, now), nil
}

// buildNetworks judges every link. Separate from the query so it is tested
// without a database.
func buildNetworks(in []networkRow, now time.Time) NetworksView {
	v := NetworksView{Servers: []NetworkServer{}, Software: []NetworkSoftware{}, Interfaces: []NetworkInterface{},
		Tunnels: []NetworkTunnel{}, Problems: []NetworkProblem{}}

	type end struct{ assetID, hostname, iface string }
	byKey := map[string]end{}
	scans := make([]*VPNScan, len(in))
	for i, r := range in {
		var scan VPNScan
		if len(r.vpn) > 0 && string(r.vpn) != "null" && json.Unmarshal(r.vpn, &scan) == nil {
			scans[i] = &scan
			for _, wi := range scan.WireGuard {
				if wi.PublicKey != "" {
					byKey[wi.PublicKey] = end{r.assetID, r.hostname, wi.Name}
				}
			}
		}
	}

	for i, r := range in {
		quiet := r.lastSeen == nil || now.Sub(*r.lastSeen) > agentQuiet
		srv := NetworkServer{AssetID: r.assetID, Hostname: r.hostname, Quiet: quiet, LastSeen: r.lastSeen}
		scan := scans[i]

		var inv AppInventory
		if len(r.apps) > 0 && json.Unmarshal(r.apps, &inv) == nil {
			for _, a := range inv.Applications {
				name, ok := vpnKinds[a.Kind]
				if !ok {
					continue
				}
				v.Software = append(v.Software, NetworkSoftware{AssetID: r.assetID, Hostname: r.hostname,
					Kind: a.Kind, Name: name, Version: a.Version, Running: a.Running, Unit: a.Unit, Container: a.Container})
				// WireGuard runs in the kernel; wg-quick is a oneshot that
				// exits once the interface is up, so "not running" means
				// nothing for it.
				if !a.Running && a.Kind != "wireguard" && !quiet {
					v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "stopped",
						AssetID: r.assetID, Hostname: r.hostname,
						Title:  fmt.Sprintf("%s is installed but not running on %s", name, r.hostname),
						Detail: "Installed, with no process or service running. If it should be connected, start it; if not, remove it."})
				}
			}
		}

		if scan == nil {
			v.Servers = append(v.Servers, srv)
			continue
		}
		srv.Reported = true
		at := scan.CheckedAt
		srv.CheckedAt = &at
		srv.WGTool = containsStr(scan.Sources, "wg")
		v.Servers = append(v.Servers, srv)

		for _, wi := range scan.WireGuard {
			ni := NetworkInterface{AssetID: r.assetID, Hostname: r.hostname, Name: wi.Name, PublicKey: wi.PublicKey,
				ListenPort: wi.ListenPort, Addresses: nonNilStrings(wi.Addresses), Up: wi.Up, RxBytes: wi.RxBytes,
				TxBytes: wi.TxBytes, Peers: []NetworkPeer{}, PeersKnown: srv.WGTool}
			if !wi.Up && !quiet {
				v.Problems = append(v.Problems, NetworkProblem{Severity: "down", Kind: "interface-down",
					AssetID: r.assetID, Hostname: r.hostname,
					Title:  fmt.Sprintf("WireGuard %s is down on %s", wi.Name, r.hostname),
					Detail: "The interface exists but is not up, so none of its peers can be reached."})
			}
			for _, p := range wi.Peers {
				np := NetworkPeer{PublicKey: p.PublicKey, Endpoint: p.Endpoint, AllowedIPs: nonNilStrings(p.AllowedIPs),
					Keepalive: p.Keepalive, RxBytes: p.RxBytes, TxBytes: p.TxBytes}
				if !p.LatestHandshake.IsZero() {
					hs := p.LatestHandshake
					np.LatestHandshake = &hs
				}
				if e, ok := byKey[p.PublicKey]; ok && e.assetID != r.assetID {
					np.PeerAssetID, np.PeerHostname, np.PeerInterface = e.assetID, e.hostname, e.iface
				}
				np.State = peerState(p, scan.CheckedAt, quiet)
				if np.State == "down" || np.State == "never" {
					other := p.Endpoint
					if np.PeerHostname != "" {
						other = np.PeerHostname
					}
					if other == "" {
						other = shortKey(p.PublicKey)
					}
					detail := "No handshake in the last 3 minutes, although this side keeps the link alive."
					if np.State == "never" {
						detail = "This side is set to connect but has never completed a handshake. Check the other end is up, the keys match, and UDP reaches it."
					} else if np.LatestHandshake != nil {
						detail = fmt.Sprintf("Last handshake %s before the agent checked, although this side keeps the link alive.",
							roughDuration(scan.CheckedAt.Sub(*np.LatestHandshake)))
					}
					v.Problems = append(v.Problems, NetworkProblem{Severity: "down", Kind: "peer-down",
						AssetID: r.assetID, Hostname: r.hostname,
						Title:  fmt.Sprintf("%s on %s cannot reach %s", wi.Name, r.hostname, other),
						Detail: detail})
				}
				ni.Peers = append(ni.Peers, np)
			}
			v.Interfaces = append(v.Interfaces, ni)
		}

		for _, c := range scan.Cloudflared {
			t := NetworkTunnel{AssetID: r.assetID, Hostname: r.hostname, Metrics: c.Metrics,
				ReadyConnections: c.ReadyConnections, ConnectorID: c.ConnectorID, Version: c.Version, Error: c.Error}
			switch {
			case quiet:
				t.State = "unknown"
			case c.Ready:
				t.State = "up"
			default:
				t.State = "down"
				detail := "cloudflared is running but has no connection to Cloudflare, so what it publishes is unreachable."
				if c.Error != "" {
					detail = "cloudflared's health check: " + c.Error + "."
				}
				v.Problems = append(v.Problems, NetworkProblem{Severity: "down", Kind: "tunnel-down",
					AssetID: r.assetID, Hostname: r.hostname,
					Title: fmt.Sprintf("Cloudflare Tunnel on %s is not connected", r.hostname), Detail: detail})
			}
			if t.State == "up" && c.ReadyConnections < 2 {
				v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "tunnel-thin",
					AssetID: r.assetID, Hostname: r.hostname,
					Title:  fmt.Sprintf("Cloudflare Tunnel on %s has one connection", r.hostname),
					Detail: "cloudflared normally keeps four connections to two Cloudflare locations. With one, a single failure takes it offline."})
			}
			v.Tunnels = append(v.Tunnels, t)
		}

		if quiet && (len(scan.WireGuard) > 0 || len(scan.Cloudflared) > 0) {
			v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "agent-quiet",
				AssetID: r.assetID, Hostname: r.hostname,
				Title:  fmt.Sprintf("%s's links are not known right now", r.hostname),
				Detail: "Its agent has not checked in for over 5 minutes, so its VPN links are shown as last reported."})
		}
	}
	sort.SliceStable(v.Problems, func(i, j int) bool {
		return v.Problems[i].Severity == "down" && v.Problems[j].Severity != "down"
	})
	return v
}

// peerState judges a WireGuard peer at the time its agent read it.
func peerState(p WGPeer, at time.Time, quiet bool) string {
	if quiet {
		return "unknown"
	}
	if !p.LatestHandshake.IsZero() && at.Sub(p.LatestHandshake) < handshakeFresh {
		return "up"
	}
	// Only a side that both knows where to dial and keeps the link alive is
	// expected to have a fresh handshake at all times. Without keepalive a
	// healthy link with no traffic goes quiet; without an endpoint this side
	// waits to be dialled (a laptop, or the far end of a link).
	if p.Endpoint == "" || p.Keepalive == 0 {
		return "idle"
	}
	if p.LatestHandshake.IsZero() {
		return "never"
	}
	return "down"
}

func shortKey(k string) string {
	if len(k) > 8 {
		return "peer " + k[:8] + "…"
	}
	return "peer " + k
}

func roughDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// getNetworks serves the Networks page. Fleet roles only, like what runs on
// a host: link endpoints and allowed IPs map the private network.
func (a *API) getNetworks(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing the networks requires the admin, owner or auditor role")
		return
	}
	out, err := a.store.Networks(r.Context(), time.Now())
	if err != nil {
		a.fail(w, "networks", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

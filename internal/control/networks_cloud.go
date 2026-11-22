package control

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cfapi"
	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
The parts of Networks that come from outside the servers: the site-to-site
VPNs in the connected cloud accounts (read by the cloud sync, cloud/vpn.go)
and the connected Cloudflare accounts (cloudflare_accounts.go). Both are
matched to what the agents report, so a Cloudflare connector is shown
against the server that runs it, and one that no server reports stands out.
*/

// cfStale is how old a Cloudflare read may be before the page says so.
const cfStale = 20 * time.Minute

// cfOldVersion is how old a cloudflared release may be before it is
// called old: Cloudflare supports a release for a year.
const cfOldVersion = 365 * 24 * time.Hour

// NetworkCloudVPN is one site-to-site VPN in a cloud account.
type NetworkCloudVPN struct {
	AccountID      string         `json:"accountId"`
	AccountName    string         `json:"accountName"`
	Provider       string         `json:"provider"`
	Region         string         `json:"region"`
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	State          string         `json:"state"`
	TunnelsUp      int            `json:"tunnelsUp"`
	TunnelsTotal   int            `json:"tunnelsTotal"`
	Tunnels        []cloud.Tunnel `json:"tunnels"`
	RemoteAddress  string         `json:"remoteAddress,omitempty"`
	RemoteNetworks []string       `json:"remoteNetworks"`
	Gateway        string         `json:"gateway,omitempty"`
	Type           string         `json:"type,omitempty"`
	Routing        string         `json:"routing,omitempty"`
	SeenAt         time.Time      `json:"seenAt"`
}

// NetworkCFAccount is one Cloudflare account as last read.
type NetworkCFAccount struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	AccountID    string              `json:"accountId"`
	SyncedAt     *time.Time          `json:"syncedAt,omitempty"`
	Stale        bool                `json:"stale"`
	LastError    string              `json:"lastError,omitempty"`
	Read         []string            `json:"read"`
	Missing      map[string]string   `json:"missing,omitempty"`
	Tunnels      []NetworkCFTunnel   `json:"tunnels"`
	Routes       []cfapi.Route       `json:"routes"`
	Devices      []cfapi.Device      `json:"devices"`
	DevicesTotal int                 `json:"devicesTotal"`
	Magic        []cfapi.MagicTunnel `json:"magicWan"`
}

// NetworkCFTunnel is a Cloudflare Tunnel or WARP Connector with its
// connectors.
type NetworkCFTunnel struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Kind            string               `json:"kind"` // tunnel, warp-connector
	Status          string               `json:"status"`
	ConnsActiveAt   *time.Time           `json:"connsActiveAt,omitempty"`
	ConnsInactiveAt *time.Time           `json:"connsInactiveAt,omitempty"`
	Connectors      []NetworkCFConnector `json:"connectors"`
	Routes          []string             `json:"routes"`
}

// NetworkCFConnector is one running cloudflared, as Cloudflare sees it.
type NetworkCFConnector struct {
	ClientID    string   `json:"clientId"`
	Version     string   `json:"version,omitempty"`
	OriginIP    string   `json:"originIp,omitempty"`
	Locations   []string `json:"locations"`
	Connections int      `json:"connections"`
	// AssetID and Hostname are the Argus server whose agent reports this
	// connector; empty when none does.
	AssetID  string `json:"assetId,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Old      bool   `json:"old"`
}

// addCloudNetworks adds the cloud VPNs and Cloudflare accounts to v, with
// their problems.
func addCloudNetworks(v *NetworksView, vpns []CloudItem, accts []CloudflareAccount, now time.Time) {
	for _, it := range vpns {
		v.CloudVPNs = append(v.CloudVPNs, cloudVPN(v, it))
	}

	// Connector id → the server whose agent reports it.
	byConnector := map[string]int{}
	for i, t := range v.Tunnels {
		if t.ConnectorID != "" {
			byConnector[strings.ToLower(t.ConnectorID)] = i
		}
	}
	for _, c := range accts {
		v.Cloudflare = append(v.Cloudflare, cfAccount(v, c, byConnector, now))
	}
	sortProblems(v.Problems)
}

func cloudVPN(v *NetworksView, it CloudItem) NetworkCloudVPN {
	r := it.Resource
	d := r.Details
	up, _ := strconv.Atoi(d[cloud.DetailTunnelsUp])
	total, _ := strconv.Atoi(d[cloud.DetailTunnelsTotal])
	n := NetworkCloudVPN{AccountID: it.AccountID, AccountName: it.AccountName, Provider: it.Provider, Region: r.Region,
		ID: r.ID, Name: r.Name, State: r.State, TunnelsUp: up, TunnelsTotal: total, Tunnels: parseTunnels(d[cloud.DetailTunnels]),
		RemoteAddress: d[cloud.DetailRemoteAddress], RemoteNetworks: splitList(d[cloud.DetailRemoteNetworks]),
		Gateway: d[cloud.DetailGateway], Type: d[cloud.DetailVPNType], Routing: d[cloud.DetailRouting], SeenAt: it.SeenAt}
	if n.Name == "" {
		n.Name = n.ID
	}
	where := fmt.Sprintf("%s %s in %s", providerName(it.Provider), it.AccountName, r.Region)
	key := problemKey("cloud-vpn", "", it.AccountID+"/"+r.Region+"/"+r.ID)
	switch r.State {
	case cloud.VPNDown:
		v.Problems = append(v.Problems, NetworkProblem{Severity: "down", Kind: "cloud-vpn-down", Key: key,
			Title:  fmt.Sprintf("Cloud VPN %s is down (%s)", n.Name, where),
			Detail: "No tunnel is up, so nothing crosses it. " + tunnelReasons(n.Tunnels)})
	case cloud.VPNPartial:
		if r.Tags[cloud.VPNLinkTag] != "" {
			// Argus made it for one IPsec link, which uses one tunnel.
			break
		}
		v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "cloud-vpn-partial", Key: key + "/partial",
			Title: fmt.Sprintf("Cloud VPN %s has %d of %d tunnels up (%s)", n.Name, up, total, where),
			Detail: "Traffic still flows, but there is no spare: if the tunnel that is up fails, the link is down. " +
				tunnelReasons(n.Tunnels)})
	}
	return n
}

func cfAccount(v *NetworksView, c CloudflareAccount, byConnector map[string]int, now time.Time) NetworkCFAccount {
	a := NetworkCFAccount{ID: c.ID, Name: c.Name, AccountID: c.AccountID, SyncedAt: c.SyncedAt, LastError: c.LastError,
		Read: c.Read, Missing: c.Missing, Tunnels: []NetworkCFTunnel{}, Routes: []cfapi.Route{}, Devices: []cfapi.Device{},
		Magic: []cfapi.MagicTunnel{}}
	a.Stale = c.SyncedAt == nil || now.Sub(*c.SyncedAt) > cfStale
	if c.LastError != "" {
		v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "cf-read-failed",
			Key:    problemKey("cf-read-failed", "", c.ID),
			Title:  fmt.Sprintf("Argus cannot read the Cloudflare account %s", c.Name),
			Detail: c.LastError + ". What is shown is from the last read that worked."})
	}
	s := c.snap
	if s == nil {
		return a
	}
	a.Routes = append(a.Routes, s.Routes...)
	a.DevicesTotal = s.DevicesTotal
	a.Devices = append(a.Devices, s.Devices...)
	sort.SliceStable(a.Devices, func(i, j int) bool {
		li, lj := a.Devices[i].LastSeen, a.Devices[j].LastSeen
		return li != nil && (lj == nil || li.After(*lj))
	})
	if len(a.Devices) > 50 {
		a.Devices = a.Devices[:50]
	}
	a.Magic = append(a.Magic, s.Magic...)
	routes := map[string][]string{}
	for _, r := range s.Routes {
		routes[r.TunnelID] = append(routes[r.TunnelID], r.Network)
	}
	// A failed read is judged at the time of the last one that worked.
	at := now
	if c.SyncedAt != nil {
		at = *c.SyncedAt
	}
	for kind, list := range map[string][]cfapi.Tunnel{"tunnel": s.Tunnels, "warp-connector": s.Connectors} {
		for _, t := range list {
			a.Tunnels = append(a.Tunnels, cfTunnel(v, c, kind, t, routes[t.ID], byConnector, at))
		}
	}
	sort.SliceStable(a.Tunnels, func(i, j int) bool {
		return strings.ToLower(a.Tunnels[i].Name) < strings.ToLower(a.Tunnels[j].Name)
	})
	return a
}

func cfTunnel(v *NetworksView, c CloudflareAccount, kind string, t cfapi.Tunnel, routes []string, byConnector map[string]int, at time.Time) NetworkCFTunnel {
	nt := NetworkCFTunnel{ID: t.ID, Name: t.Name, Kind: kind, Status: t.Status, ConnsActiveAt: t.ConnsActiveAt,
		ConnsInactiveAt: t.ConnsInactiveAt, Connectors: []NetworkCFConnector{}, Routes: nonNilStrings(routes)}
	what := "Cloudflare Tunnel"
	if kind == "warp-connector" {
		what = "WARP Connector"
	}
	idx := map[string]int{}
	for _, conn := range t.Connections {
		id := strings.ToLower(conn.ClientID)
		i, ok := idx[id]
		if !ok {
			nc := NetworkCFConnector{ClientID: conn.ClientID, Version: conn.ClientVersion, OriginIP: conn.OriginIP,
				Locations: []string{}, Old: cloudflaredOld(conn.ClientVersion, at)}
			if ti, ok := byConnector[id]; ok {
				nc.AssetID, nc.Hostname = v.Tunnels[ti].AssetID, v.Tunnels[ti].Hostname
				v.Tunnels[ti].TunnelID, v.Tunnels[ti].TunnelName = t.ID, t.Name
			}
			nt.Connectors = append(nt.Connectors, nc)
			i = len(nt.Connectors) - 1
			idx[id] = i
		}
		nt.Connectors[i].Connections++
		if conn.ColoName != "" && !containsStr(nt.Connectors[i].Locations, conn.ColoName) {
			nt.Connectors[i].Locations = append(nt.Connectors[i].Locations, conn.ColoName)
		}
	}
	key := func(kind string) string { return problemKey(kind, "", c.AccountID+"/"+t.ID) }
	switch t.Status {
	case "down":
		v.Problems = append(v.Problems, NetworkProblem{Severity: "down", Kind: "cf-tunnel-down", Key: key("cf-tunnel-down"),
			Title:  fmt.Sprintf("Cloudflare says %s %s is down", what, t.Name),
			Detail: "No connector is connected, so its public hostnames and private routes reach nothing. Start cloudflared where it runs."})
	case "degraded":
		v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "cf-tunnel-degraded", Key: key("cf-tunnel-degraded"),
			Title:  fmt.Sprintf("Cloudflare says %s %s is degraded", what, t.Name),
			Detail: "At least one connector has fewer connections than it should. It still works; one more failure may take it down."})
	case "inactive":
		if len(nt.Routes) > 0 {
			v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "cf-inactive", Key: key("cf-inactive"),
				Title: fmt.Sprintf("%s %s has not run in a long time but still has routes", what, t.Name),
				Detail: "Traffic to " + strings.Join(nt.Routes, ", ") + " is sent to it and goes nowhere. " +
					"Start it, or delete it in Cloudflare so its routes and hostnames are not left pointing at nothing."})
		}
	}
	if t.Status == "healthy" && len(nt.Connectors) == 1 {
		v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "cf-single-connector", Key: key("cf-single-connector"),
			Title: fmt.Sprintf("%s %s runs on one machine only", what, t.Name),
			Detail: "If that machine or its cloudflared stops, the tunnel is down. Run the same tunnel on a second server " +
				"for a spare (cloudflared tunnel run with the same token)."})
	}
	for _, nc := range nt.Connectors {
		if nc.AssetID == "" {
			where := nc.OriginIP
			if where == "" {
				where = "an unknown address"
			}
			v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "cf-unknown-machine",
				Key:   key("cf-unknown-machine") + "/" + nc.ClientID,
				Title: fmt.Sprintf("%s %s has a connector no Argus server reports, from %s", what, t.Name, where),
				Detail: "It may be a machine without an Argus agent, such as someone's laptop publishing a company hostname, " +
					"or a server whose cloudflared metrics Argus cannot read. Check who runs it."})
		}
		if nc.Old {
			on := nc.Hostname
			if on == "" {
				on = nc.OriginIP
			}
			v.Problems = append(v.Problems, NetworkProblem{Severity: "warn", Kind: "cf-old-version",
				AssetID: nc.AssetID, Hostname: nc.Hostname,
				Key:   problemKey("cf-old-version", nc.AssetID, c.AccountID+"/"+t.ID+"/"+nc.ClientID),
				Title: fmt.Sprintf("cloudflared %s on %s is over a year old", nc.Version, on),
				Detail: "Cloudflare supports each release for a year; older ones can stop connecting without warning. " +
					"Update cloudflared."})
		}
	}
	return nt
}

// cloudflaredOld reports whether a release (year.month.patch) is more than
// cfOldVersion before at. An unreadable version is not called old.
func cloudflaredOld(version string, at time.Time) bool {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return false
	}
	y, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || y < 2017 || m < 1 || m > 12 {
		return false
	}
	released := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
	return at.Sub(released) > cfOldVersion
}

// parseTunnels reads cloud.DetailTunnels back.
func parseTunnels(s string) []cloud.Tunnel {
	out := []cloud.Tunnel{}
	if s == "" {
		return out
	}
	for _, part := range strings.Split(s, "; ") {
		addr, state, _ := strings.Cut(part, "=")
		t := cloud.Tunnel{Address: addr, Up: state == "UP"}
		if !t.Up {
			_, t.Why, _ = strings.Cut(state, ": ")
		}
		out = append(out, t)
	}
	return out
}

func tunnelReasons(ts []cloud.Tunnel) string {
	var why []string
	for _, t := range ts {
		if !t.Up && t.Why != "" {
			why = append(why, t.Address+": "+t.Why)
		}
	}
	if len(why) == 0 {
		return "Check the device on the other end is up and its settings match."
	}
	return "The cloud says " + strings.Join(why, "; ") + "."
}

func splitList(s string) []string {
	out := []string{}
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func providerName(p string) string {
	switch p {
	case "aws":
		return "AWS"
	case "azure":
		return "Azure"
	case "gcp":
		return "Google Cloud"
	}
	return p
}

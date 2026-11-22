package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

/*
The Networks page: links between Argus servers are matched by public key,
each peer is judged at the time its agent read it, a quiet agent's links are
"not known" rather than repeated as current, and only the fleet roles see it.
*/

func netRow(id, host string, seen time.Time, scan VPNScan, apps string) networkRow {
	b, _ := json.Marshal(scan)
	return networkRow{assetID: id, hostname: host, lastSeen: &seen, vpn: b, apps: []byte(apps)}
}

func TestNetworksMatchLinksAndJudgePeers(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	at := now.Add(-20 * time.Minute) // reported 20 min ago, agent still checking in
	db := VPNScan{CheckedAt: at, Sources: []string{"wg"}, WireGuard: []WGInterface{{Name: "wg0", PublicKey: "KEYdb", Up: true,
		Peers: []WGPeer{
			{PublicKey: "KEYapp", Endpoint: "10.0.0.2:51820", Keepalive: 25, LatestHandshake: at.Add(-time.Minute)}, // up at read time
			{PublicKey: "KEYoffice", Endpoint: "203.0.113.9:51820", Keepalive: 25, LatestHandshake: at.Add(-10 * time.Minute)},
			{PublicKey: "KEYlaptop", LatestHandshake: at.Add(-time.Hour)}, // waits to be dialled
			{PublicKey: "KEYnew", Endpoint: "198.51.100.1:51820", Keepalive: 25},
		}}},
		Cloudflared: []CFConnector{{Metrics: "127.0.0.1:20241", Ready: true, ReadyConnections: 4, ConnectorID: "c1", Version: "2025.11.1"}}}
	app := VPNScan{CheckedAt: at, Sources: []string{"wg"}, WireGuard: []WGInterface{{Name: "wg0", PublicKey: "KEYapp", Up: true,
		Peers: []WGPeer{{PublicKey: "KEYdb", LatestHandshake: at.Add(-time.Minute)}}}},
		Cloudflared: []CFConnector{{Metrics: "127.0.0.1:20241", Error: "metrics did not answer"}}}
	quiet := VPNScan{CheckedAt: at, WireGuard: []WGInterface{{Name: "wg1", PublicKey: "KEYq", Up: false}}}
	apps := `{"applications":[{"kind":"openvpn","name":"OpenVPN","running":false},{"kind":"wireguard","name":"WireGuard","running":false},{"kind":"postgres","name":"PostgreSQL","running":true}]}`

	v := buildNetworks([]networkRow{
		netRow("a1", "db-01", now.Add(-30*time.Second), db, apps),
		netRow("a2", "app-01", now.Add(-30*time.Second), app, "{}"),
		netRow("a3", "old-01", now.Add(-time.Hour), quiet, "{}"),
		{assetID: "a4", hostname: "legacy-01"}, // agent older than VPN discovery
	}, now)

	if len(v.Servers) != 4 || v.Servers[3].Reported || !v.Servers[2].Quiet || !v.Servers[0].WGTool {
		t.Fatalf("servers = %+v", v.Servers)
	}
	states := map[string]string{}
	for _, p := range v.Interfaces[0].Peers {
		states[p.PublicKey] = p.State
	}
	want := map[string]string{"KEYapp": "up", "KEYoffice": "down", "KEYlaptop": "idle", "KEYnew": "never"}
	for k, s := range want {
		if states[k] != s {
			t.Errorf("peer %s = %s, want %s", k, states[k], s)
		}
	}
	if p := v.Interfaces[0].Peers[0]; p.PeerHostname != "app-01" || p.PeerAssetID != "a2" || p.PeerInterface != "wg0" {
		t.Errorf("link not matched: %+v", p)
	}
	if v.Interfaces[2].Peers == nil || v.Interfaces[2].PeersKnown {
		t.Errorf("a scan without wg claims to know its peers: %+v", v.Interfaces[2])
	}
	if len(v.Software) != 2 {
		t.Errorf("software = %+v (postgres is not a VPN)", v.Software)
	}
	if v.Tunnels[0].State != "up" || v.Tunnels[1].State != "down" {
		t.Errorf("tunnels = %+v", v.Tunnels)
	}

	kinds := []string{}
	for _, p := range v.Problems {
		kinds = append(kinds, p.Severity+":"+p.Kind+":"+p.Hostname)
	}
	got := strings.Join(kinds, " ")
	for _, k := range []string{"down:peer-down:db-01", "down:tunnel-down:app-01", "warn:stopped:db-01", "warn:agent-quiet:old-01"} {
		if !strings.Contains(got, k) {
			t.Errorf("problems %q lack %s", got, k)
		}
	}
	// A quiet agent's interface is not called down, and WireGuard is never
	// "installed but not running".
	for _, bad := range []string{"interface-down:old-01", "stopped:db-01:WireGuard"} {
		if strings.Contains(got, bad) {
			t.Errorf("problems %q include %s", got, bad)
		}
	}
	if strings.Count(got, "stopped") != 1 {
		t.Errorf("problems %q", got)
	}
	if v.Problems[0].Severity != "down" || v.Problems[len(v.Problems)-1].Severity != "warn" {
		t.Errorf("not most serious first: %q", got)
	}
}

func TestVPNReportIsBounded(t *testing.T) {
	in := &VPNScan{}
	for i := 0; i < maxVPNInterfaces+5; i++ {
		wi := WGInterface{Name: strings.Repeat("w", 5000)}
		for j := 0; j < maxVPNPeers+5; j++ {
			wi.Peers = append(wi.Peers, WGPeer{PublicKey: "k"})
		}
		in.WireGuard = append(in.WireGuard, wi)
	}
	out := in.bounded()
	if len(out.WireGuard) != maxVPNInterfaces || len(out.WireGuard[0].Peers) != maxVPNPeers || len(out.WireGuard[0].Name) != maxAppField {
		t.Fatalf("not bounded: %d interfaces, %d peers", len(out.WireGuard), len(out.WireGuard[0].Peers))
	}
	if (*VPNScan)(nil).bounded() != nil {
		t.Fatal("nil report became a value")
	}
}

func TestNetworksReachTheFleetRolesOnly(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	facts := `{"hostname":"` + asset.Hostname + `","vpn":{"checked_at":"2026-11-22T09:00:00Z","sources":["wg"],` +
		`"wireguard":[{"name":"wg0","public_key":"PUB","up":true,"rx_bytes":1,"tx_bytes":2,` +
		`"peers":[{"public_key":"PEER","endpoint":"203.0.113.1:51820","allowed_ips":["10.88.0.2/32"],"keepalive":25,"rx_bytes":0,"tx_bytes":0}]}],` +
		`"cloudflared":[]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, facts); w.Code != http.StatusOK {
		t.Fatalf("facts: %d %s", w.Code, w.Body)
	}
	w := do(t, h, http.MethodGet, "/api/v1/networks", "admin-token", "")
	var got NetworksView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("networks: %d %s", w.Code, w.Body)
	}
	found := false
	for _, i := range got.Interfaces {
		if i.AssetID == asset.ID && i.Name == "wg0" && len(i.Peers) == 1 && i.Peers[0].AllowedIPs[0] == "10.88.0.2/32" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the reported interface is missing: %s", w.Body)
	}

	// An older agent's facts leave the links alone.
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, `{"hostname":"`+asset.Hostname+`"}`); w.Code != http.StatusOK {
		t.Fatalf("old facts: %d", w.Code)
	}
	w = do(t, h, http.MethodGet, "/api/v1/networks", "admin-token", "")
	if !strings.Contains(w.Body.String(), `"PEER"`) {
		t.Errorf("an old agent's facts erased the links: %s", w.Body)
	}

	if w := do(t, h, http.MethodGet, "/api/v1/networks", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator read the networks: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/networks", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous read the networks: %d", w.Code)
	}
}

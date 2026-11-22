package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func wgTestKey(b byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestCheckWGDraft(t *testing.T) {
	hosts := map[string]string{"a": "db1", "b": "db2", "c": "edge"}
	good := func() WGNetworkDraft {
		return WGNetworkDraft{Name: "db mesh", Shape: "mesh", CIDR: "10.88.0.0/24", Members: []WGMemberDraft{
			{AssetID: "a", Endpoint: "203.0.113.1"}, {AssetID: "b", Address: "10.88.0.1"}, {AssetID: "c", Endpoint: "edge.example.com"}}}
	}
	d := good()
	if err := checkWGDraft(&d, hosts, wgTaken{}); err != nil {
		t.Fatal(err)
	}
	if d.Port != 51820 || d.Members[0].Address != "10.88.0.2" || d.Members[2].Address != "10.88.0.3" || d.Members[1].Hostname != "db2" {
		t.Errorf("not filled in: %+v", d)
	}

	taken := wgTaken{
		ranges: []wgRange{{netip.MustParsePrefix("10.88.0.0/16"), "subnet app in prod"}},
		ports:  map[string]map[int]string{"a": {51821: "other"}},
		own:    map[string][]netip.Prefix{"b": {netip.MustParsePrefix("172.20.0.5/32")}},
	}
	for name, c := range map[string]struct {
		change func(*WGNetworkDraft)
		taken  wgTaken
		want   string
	}{
		"overlap":        {func(*WGNetworkDraft) {}, taken, "overlaps subnet app in prod"},
		"public range":   {func(d *WGNetworkDraft) { d.CIDR = "8.8.8.0/24" }, wgTaken{}, "private range"},
		"not the start":  {func(d *WGNetworkDraft) { d.CIDR = "10.88.0.5/24" }, wgTaken{}, "did you mean 10.88.0.0/24"},
		"too wide":       {func(d *WGNetworkDraft) { d.CIDR = "10.0.0.0/8" }, wgTaken{}, "/16 and a /29"},
		"port in use":    {func(d *WGNetworkDraft) { d.CIDR, d.Port = "10.99.0.0/24", 51821 }, taken, "already uses UDP port 51821"},
		"own address":    {func(d *WGNetworkDraft) { d.CIDR = "172.20.0.0/24" }, taken, "db2's own address"},
		"two quiet":      {func(d *WGNetworkDraft) { d.Members[0].Endpoint = "" }, wgTaken{}, "both lack an address"},
		"no hub":         {func(d *WGNetworkDraft) { d.Shape = "hub" }, wgTaken{}, "exactly one hub"},
		"quiet hub":      {func(d *WGNetworkDraft) { d.Shape, d.Members[1].Hub = "hub", true }, wgTaken{}, "the hub, db2, needs"},
		"twice":          {func(d *WGNetworkDraft) { d.Members[1].AssetID = "a" }, wgTaken{}, "twice"},
		"outside":        {func(d *WGNetworkDraft) { d.Members[1].Address = "10.89.0.1" }, wgTaken{}, "not a usable address"},
		"broadcast":      {func(d *WGNetworkDraft) { d.Members[1].Address = "10.88.0.255" }, wgTaken{}, "not a usable address"},
		"unknown server": {func(d *WGNetworkDraft) { d.Members[1].AssetID = "zzz" }, wgTaken{}, "not in the inventory"},
		"port in host":   {func(d *WGNetworkDraft) { d.Members[0].Endpoint = "1.2.3.4:51820" }, wgTaken{}, "without the port"},
		"newline name":   {func(d *WGNetworkDraft) { d.Name = "x\nPostUp = id" }, wgTaken{}, "name"},
	} {
		d := good()
		c.change(&d)
		if err := checkWGDraft(&d, hosts, c.taken); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A hub network: a spoke may only dial out.
	d = good()
	d.Shape, d.Members[0].Hub, d.Members[2].Endpoint = "hub", true, ""
	if err := checkWGDraft(&d, hosts, wgTaken{}); err != nil {
		t.Errorf("hub: %v", err)
	}
}

func TestBuildWGSpecs(t *testing.T) {
	ka, kb, kc := wgTestKey(1), wgTestKey(2), wgTestKey(3)
	row := func(shape, asset, addr, ep, key string, hub bool) wgMemberRow {
		return wgMemberRow{network: "n1", name: "net", shape: shape, cidr: "10.88.0.0/24", iface: "argusabcdef", port: 51820,
			asset: asset, address: addr, endpoint: ep, key: key, hub: hub}
	}
	mesh := []wgMemberRow{row("mesh", "a", "10.88.0.1", "203.0.113.1", ka, false), row("mesh", "b", "10.88.0.2", "", kb, false),
		row("mesh", "c", "10.88.0.3", "198.51.100.3", "", false)}
	s := buildWGSpecs("b", mesh)
	if len(s) != 1 || s[0].Address != "10.88.0.2/24" || s[0].OpenPort || s[0].Forward {
		t.Fatalf("mesh: %+v", s)
	}
	// c has no key yet, so only a.
	if len(s[0].Peers) != 1 || s[0].Peers[0].PublicKey != ka || s[0].Peers[0].Endpoint != "203.0.113.1:51820" ||
		s[0].Peers[0].AllowedIPs[0] != "10.88.0.1/32" || s[0].Peers[0].Keepalive != 25 {
		t.Errorf("mesh peers: %+v", s[0].Peers)
	}
	if s := buildWGSpecs("a", mesh); !s[0].OpenPort || s[0].Peers[0].Endpoint != "" || s[0].Peers[0].Keepalive != 0 {
		t.Errorf("a: %+v", s)
	}

	hub := []wgMemberRow{row("hub", "h", "10.88.0.1", "hub.example.com", ka, true), row("hub", "s1", "10.88.0.2", "", kb, false),
		row("hub", "s2", "10.88.0.3", "", kc, false)}
	s = buildWGSpecs("s1", hub)
	if len(s[0].Peers) != 1 || s[0].Peers[0].AllowedIPs[0] != "10.88.0.0/24" || s[0].Peers[0].Endpoint != "hub.example.com:51820" {
		t.Errorf("spoke: %+v", s[0].Peers)
	}
	s = buildWGSpecs("h", hub)
	if !s[0].Forward || !s[0].OpenPort || len(s[0].Peers) != 2 || s[0].Peers[1].AllowedIPs[0] != "10.88.0.3/32" {
		t.Errorf("hub: %+v", s[0])
	}
	if s := buildWGSpecs("nobody", hub); len(s) != 0 {
		t.Errorf("a server outside the network got %+v", s)
	}
}

func TestLastAddr(t *testing.T) {
	for in, want := range map[string]string{"10.88.0.0/24": "10.88.0.254", "10.0.0.0/16": "10.0.255.254", "192.168.1.8/29": "192.168.1.14"} {
		if got := lastAddr(netip.MustParsePrefix(in)).String(); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

type wgEnv struct{ pkgEnv }

func (e wgEnv) beat(t *testing.T, cred, host string, b *WGNetBeat) *WGNetOrders {
	t.Helper()
	body := map[string]any{"hostname": host, "version": "1.2.0", "active_sessions": 0}
	if b != nil {
		body["wgnet"] = b
	}
	raw, _ := json.Marshal(body)
	w := do(t, e.h, http.MethodPost, "/api/v1/report/heartbeat", cred, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	p, ok := out["wgnet"]
	if !ok {
		return nil
	}
	var o WGNetOrders
	if err := json.Unmarshal(p, &o); err != nil {
		t.Fatalf("wgnet: %v", err)
	}
	return &o
}

func (e wgEnv) list(t *testing.T, token string) []WGNetwork {
	t.Helper()
	w := do(t, e.h, http.MethodGet, "/api/v1/networks/wireguard", token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var v wgNetworksView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v.Networks
}

func TestWGNetworkRoutes(t *testing.T) {
	e := wgEnv{packagesAPI(t)}
	ctx := context.Background()
	_, _ = e.s.pool.Exec(ctx, `DELETE FROM wg_networks`)
	t.Cleanup(func() { _, _ = e.s.pool.Exec(ctx, `DELETE FROM wg_networks`) })
	a, credA := e.server(t)
	b, credB := e.server(t)
	var auditsBefore int
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'wireguard.network.%' AND target = 'wireguard network db mesh'`).Scan(&auditsBefore)

	body := `{"name":"db mesh","shape":"mesh","cidr":"10.77.0.0/24","members":[{"assetId":"` + a.ID +
		`","endpoint":"203.0.113.7"},{"assetId":"` + b.ID + `"}]}`
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard", "auditor-token", body); w.Code != http.StatusForbidden {
		t.Errorf("an auditor asked for a network: %d", w.Code)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard", "admin-token", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var n WGNetwork
	_ = json.Unmarshal(w.Body.Bytes(), &n)
	if n.State != "waiting" || n.Pending == nil || !reWGIface.MatchString(n.Iface) {
		t.Fatalf("created %+v", n)
	}
	// The same range again clashes with the waiting one.
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard", "second-token",
		strings.Replace(body, "db mesh", "other", 1)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "overlaps") {
		t.Errorf("overlap: %d %s", w.Code, w.Body)
	}

	// Nothing reaches a server before approval.
	beat := &WGNetBeat{Supported: true}
	if o := e.beat(t, credA, a.Hostname, beat); o == nil || len(o.Networks) != 0 {
		t.Fatalf("before approval: %+v", o)
	}
	// The person who asked cannot approve while another admin could.
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard/"+n.ID+"/approve", "admin-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("approved their own: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard/"+n.ID+"/approve", "second-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}

	// a hears of the network, with no peers until b has a key.
	o := e.beat(t, credA, a.Hostname, beat)
	if len(o.Networks) != 1 || o.Networks[0].Address != "10.77.0.1/24" || len(o.Networks[0].Peers) != 0 || !o.Networks[0].OpenPort {
		t.Fatalf("a first: %+v", o)
	}
	e.beat(t, credA, a.Hostname, &WGNetBeat{Supported: true, Keys: map[string]string{n.ID: wgTestKey(1)}})
	o = e.beat(t, credB, b.Hostname, &WGNetBeat{Supported: true, Keys: map[string]string{n.ID: wgTestKey(2)}})
	if len(o.Networks) != 1 || len(o.Networks[0].Peers) != 1 || o.Networks[0].Peers[0].Endpoint != "203.0.113.7:51820" {
		t.Fatalf("b: %+v", o)
	}
	o = e.beat(t, credA, a.Hostname, beat)
	if len(o.Networks[0].Peers) != 1 || o.Networks[0].Peers[0].PublicKey != wgTestKey(2) {
		t.Fatalf("a after b's key: %+v", o)
	}

	// A change that was undone is shown, and raises an alert.
	run := &WGNetRun{ID: "r1", State: "rolled-back", Message: "the server did not reach Argus", At: time.Now(), Networks: []string{n.ID}}
	e.beat(t, credB, b.Hostname, &WGNetBeat{Supported: true, Run: run})
	got := e.list(t, "auditor-token")
	if len(got) != 1 || len(got[0].Members) != 2 || got[0].Members[1].State != "rolled-back" || got[0].Members[0].State != "applying" {
		t.Fatalf("members: %+v", got)
	}
	var alerts int
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM console_alerts WHERE kind = 'network' AND title = $1`, "A WireGuard network change on "+b.Hostname+" was undone").Scan(&alerts)
	if alerts != 1 {
		t.Errorf("%d alerts for the undone change", alerts)
	}
	e.beat(t, credB, b.Hostname, &WGNetBeat{Supported: true, Run: run})
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM console_alerts WHERE kind = 'network' AND title = $1`, "A WireGuard network change on "+b.Hostname+" was undone").Scan(&alerts)
	if alerts != 1 {
		t.Errorf("the same run alerted again: %d", alerts)
	}

	// Removal waits too, then takes it off the servers.
	if w := do(t, e.h, http.MethodDelete, "/api/v1/networks/wireguard/"+n.ID, "admin-token", ""); w.Code != http.StatusAccepted {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if o := e.beat(t, credA, a.Hostname, beat); len(o.Networks) != 1 {
		t.Error("removed before approval")
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/networks/wireguard/"+n.ID, "admin-token", body); w.Code != http.StatusConflict {
		t.Errorf("a second change while one waits: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard/"+n.ID+"/approve", "second-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("approve removal: %d %s", w.Code, w.Body)
	}
	if o := e.beat(t, credA, a.Hostname, beat); o == nil || len(o.Networks) != 0 {
		t.Errorf("after removal: %+v", o)
	}
	var audits int
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'wireguard.network.%' AND target = 'wireguard network db mesh'`).Scan(&audits)
	if audits-auditsBefore != 4 {
		t.Errorf("%d audit entries", audits-auditsBefore)
	}
}

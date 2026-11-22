package control

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func staffSpecOf(o *WGNetOrders, id string) *WGNetSpec {
	if o == nil {
		return nil
	}
	for i := range o.Networks {
		if o.Networks[i].ID == id {
			return &o.Networks[i]
		}
	}
	return nil
}

// The staff VPN end to end: a hub approved like any server network; teams
// given ranges, one with direct logins; a profile made only by someone a
// team reaches through, with a code when the VPN asks; the hub filtering
// each device by its person's teams; full traffic by a person's own
// setting; expiry, renewal and removal; and nobody touching another's
// device.
func TestStaffVPN(t *testing.T) {
	e := wgEnv{packagesAPI(t)}
	ctx := context.Background()
	_, _ = e.s.pool.Exec(ctx, `DELETE FROM wg_networks`)
	t.Cleanup(func() { _, _ = e.s.pool.Exec(ctx, `DELETE FROM wg_networks`) })
	hub, cred := e.server(t)

	body := `{"name":"Staff","shape":"staff","cidr":"10.66.0.0/24","port":51900,"members":[{"assetId":"` + hub.ID + `"}]}`
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard", "admin-token", body); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "public address") {
		t.Fatalf("no endpoint: %d %s", w.Code, w.Body)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard", "admin-token", strings.Replace(body, `"}]}`, `","endpoint":"vpn.northwind.id"}]}`, 1))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var n WGNetwork
	_ = json.Unmarshal(w.Body.Bytes(), &n)
	if n.Staff == nil || n.Staff.Default != "company" || n.Staff.Days != 90 || !n.Staff.MFA {
		t.Fatalf("policy %+v", n.Staff)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/wireguard/"+n.ID+"/approve", "second-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}

	// People and teams.
	alice, _ := newAccount(t, e.s, "user")
	bob, _ := newAccount(t, e.s, "user")
	e.a.UserTokens["alice-token"], e.a.UserTokens["bob-token"] = alice, bob
	ops, err := e.s.CreateTeam(ctx, unique("ops"), []string{alice}, "test")
	if err != nil {
		t.Fatal(err)
	}
	dba, err := e.s.CreateTeam(ctx, unique("dba"), []string{alice}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.s.pool.Exec(ctx, `DELETE FROM teams WHERE id = ANY($1::uuid[])`, []string{ops, dba}) })
	grant := func(token, team, b string, want int) {
		t.Helper()
		if w := do(t, e.h, http.MethodPut, "/api/v1/networks/staff/"+n.ID+"/grants/"+team, token, b); w.Code != want {
			t.Fatalf("grant: %d %s", w.Code, w.Body)
		}
	}
	grant("alice-token", ops, `{"ranges":["10.88.0.0/24"]}`, http.StatusForbidden)
	grant("admin-token", ops, `{"ranges":["8.8.8.0/24"]}`, http.StatusBadRequest)
	grant("admin-token", ops, `{"ranges":["10.88.0.0/24","192.168.10.0/24"]}`, http.StatusNoContent)
	grant("admin-token", dba, `{"ranges":["192.168.10.0/24"],"logins":true}`, http.StatusNoContent)

	makeDevice := func(token, b string, want int) StaffProfile {
		t.Helper()
		w := do(t, e.h, http.MethodPost, "/api/v1/networks/staff/"+n.ID+"/devices", token, b)
		if w.Code != want {
			t.Fatalf("device: %d %s, want %d", w.Code, w.Body, want)
		}
		var p StaffProfile
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		return p
	}
	// The VPN asks for a code, and alice has no authenticator.
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/staff/"+n.ID+"/devices", "alice-token", `{"name":"laptop"}`); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "authenticator") {
		t.Fatalf("no mfa: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/networks/staff/"+n.ID+"/policy", "admin-token", `{"default":"company","days":30,"mfa":false}`); w.Code != http.StatusNoContent {
		t.Fatalf("policy: %d %s", w.Code, w.Body)
	}
	// Before the hub has its key, there is nothing to connect to.
	makeDevice("alice-token", `{"name":"laptop"}`, http.StatusConflict)
	hubKey := wgTestKey(1)
	e.beat(t, cred, hub.Hostname, &WGNetBeat{Supported: true, Keys: map[string]string{n.ID: hubKey}})

	makeDevice("bob-token", `{"name":"phone"}`, http.StatusConflict) // no team reaches through it
	p := makeDevice("alice-token", `{"name":"laptop"}`, http.StatusCreated)
	for _, want := range []string{"PrivateKey = ", "Address = 10.66.0.2/32", "PublicKey = " + hubKey, "Endpoint = vpn.northwind.id:51900",
		"AllowedIPs = 10.66.0.0/24, 10.88.0.0/24, 192.168.10.0/24"} {
		if !strings.Contains(p.Config, want) {
			t.Errorf("profile lacks %q:\n%s", want, p.Config)
		}
	}
	if p.Mode != "company" || p.Device.ExpiresAt.Sub(p.Device.CreatedAt).Hours() != 30*24 {
		t.Fatalf("device %+v", p)
	}
	priv := strings.TrimSpace(strings.SplitN(strings.SplitN(p.Config, "PrivateKey = ", 2)[1], "\n", 2)[0])
	var kept int
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM wg_staff_devices WHERE public_key = $1`, priv).Scan(&kept)
	if kept != 0 {
		t.Fatal("the private key was kept")
	}

	// Her page under People says what the VPN gives her.
	sections, err := e.s.PersonAccess(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	var vpn []AccessItem
	for _, sec := range sections {
		if sec.Kind == "vpn" {
			vpn = sec.Items
		}
	}
	if len(vpn) != 2 || vpn[0].Name != "Staff" || !strings.Contains(vpn[0].What, "10.88.0.0/24, 192.168.10.0/24") ||
		!strings.Contains(vpn[0].What, "not record") || len(vpn[0].Teams) != 2 || vpn[1].Name != "laptop" || vpn[1].Until == nil {
		t.Fatalf("person access %+v", vpn)
	}

	// The hub: alice's device, SSH and RDP refused to 10.88.0.0/24 but not
	// to 192.168.10.0/24, which dba gives with logins.
	o := e.beat(t, cred, hub.Hostname, &WGNetBeat{Supported: true, Keys: map[string]string{n.ID: hubKey}})
	s := staffSpecOf(o, n.ID)
	if s == nil || s.Staff == nil || !s.Forward || !s.OpenPort || len(s.Peers) != 1 || s.Peers[0].AllowedIPs[0] != "10.66.0.2/32" {
		t.Fatalf("hub spec %+v", s)
	}
	if len(s.Staff.Rules) != 2 || !s.Staff.Rules[0].Logins || !slices.Equal(s.Staff.Rules[0].To, []string{"192.168.10.0/24"}) ||
		s.Staff.Rules[1].Logins || !slices.Equal(s.Staff.Rules[1].To, []string{"10.88.0.0/24"}) || len(s.Staff.Internet) != 0 {
		t.Fatalf("rules %+v", s.Staff)
	}
	// Full traffic for alice, by her own setting.
	if w := do(t, e.h, http.MethodPut, "/api/v1/networks/staff/"+n.ID+"/modes", "admin-token", `{"email":"`+alice+`","mode":"full"}`); w.Code != http.StatusNoContent {
		t.Fatalf("mode: %d %s", w.Code, w.Body)
	}
	s = staffSpecOf(e.beat(t, cred, hub.Hostname, &WGNetBeat{Supported: true, Keys: map[string]string{n.ID: hubKey}}), n.ID)
	if !slices.Equal(s.Staff.Internet, []string{"10.66.0.2/32"}) {
		t.Fatalf("internet %+v", s.Staff)
	}
	if p2 := makeDevice("alice-token", `{"name":"desktop"}`, http.StatusCreated); !strings.Contains(p2.Config, "AllowedIPs = 0.0.0.0/0") ||
		p2.Device.Address != "10.66.0.3" {
		t.Fatalf("full profile %+v", p2)
	}

	// What each sees.
	var view struct{ VPNs []StaffVPNView }
	_ = json.Unmarshal(do(t, e.h, http.MethodGet, "/api/v1/networks/staff", "bob-token", "").Body.Bytes(), &view)
	if len(view.VPNs) != 1 || len(view.VPNs[0].Devices) != 0 || view.VPNs[0].Grants != nil || len(view.VPNs[0].Mine.Reach) != 0 || !view.VPNs[0].Ready {
		t.Fatalf("bob sees %+v", view)
	}
	_ = json.Unmarshal(do(t, e.h, http.MethodGet, "/api/v1/networks/staff", "admin-token", "").Body.Bytes(), &view)
	if len(view.VPNs[0].Devices) != 2 || len(view.VPNs[0].Grants) != 2 || view.VPNs[0].Modes[strings.ToLower(alice)] != "full" {
		t.Fatalf("admin sees %+v", view.VPNs[0])
	}

	// Expired: off the hub. Renewed: back. Bob cannot touch it.
	_, _ = e.s.pool.Exec(ctx, `UPDATE wg_staff_devices SET expires_at = now() - interval '1 minute' WHERE id = $1`, p.Device.ID)
	if s := staffSpecOf(e.beat(t, cred, hub.Hostname, &WGNetBeat{Supported: true, Keys: map[string]string{n.ID: hubKey}}), n.ID); len(s.Peers) != 1 {
		t.Fatalf("expired still on the hub: %+v", s.Peers)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/staff/devices/"+p.Device.ID+"/renew", "bob-token", `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("bob renewed: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/staff/devices/"+p.Device.ID+"/renew", "alice-token", `{}`); w.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", w.Code, w.Body)
	}
	if s := staffSpecOf(e.beat(t, cred, hub.Hostname, &WGNetBeat{Supported: true, Keys: map[string]string{n.ID: hubKey}}), n.ID); len(s.Peers) != 2 {
		t.Fatalf("renewed not on the hub: %+v", s.Peers)
	}
	if w := do(t, e.h, http.MethodDelete, "/api/v1/networks/staff/devices/"+p.Device.ID, "bob-token", ""); w.Code != http.StatusNotFound {
		t.Fatalf("bob removed: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodDelete, "/api/v1/networks/staff/devices/"+p.Device.ID, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("admin remove: %d", w.Code)
	}
	var audits int
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'staff_vpn.%' AND detail LIKE '%not recorded%'`).Scan(&audits)
	if audits == 0 {
		t.Error("the grant with logins does not say they are not recorded")
	}
}

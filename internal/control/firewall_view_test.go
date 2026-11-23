package control

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

/*
A server's firewall, as its agent reads it, reaches the Firewall tab and
Assets › Firewalls for those who see the whole fleet and no one else; an
older agent's facts leave it alone; each rule says who made it; and each
listening port says who the firewall lets reach it.
*/

func TestFirewallReachesTheServerPage(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/firewall"

	w := do(t, h, http.MethodGet, path, "admin-token", "")
	var got ServerFirewall
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.Reported {
		t.Fatalf("before a report: %d %s", w.Code, w.Body)
	}

	facts := `{"hostname":"` + asset.Hostname + `","os":"Ubuntu",` +
		`"applications":{"checked_at":"2026-11-23T05:00:00Z","applications":[],"containers":[],"services":[],"sources":[],` +
		`"listeners":[{"address":"0.0.0.0","port":22,"process":"sshd"},{"address":"0.0.0.0","port":6379,"process":"redis-server","kind":"redis"},` +
		`{"address":"127.0.0.1","port":5432,"process":"postgres"},{"address":"::","port":443,"process":"nginx"},{"address":"0.0.0.0","port":3000,"process":"grafana"}]},` +
		`"firewall":{"kind":"ufw","installed":["ufw"],"active":true,"defaultIn":"deny","defaultOut":"allow","manageable":true,"readAt":"2026-11-23T05:00:00Z",` +
		`"rules":[{"direction":"in","action":"allow","protocol":"tcp","ports":"22","sources":["10.8.0.0/24"]},` +
		`{"direction":"in","action":"allow","protocol":"tcp","ports":"80,443"},` +
		`{"direction":"in","action":"allow","protocol":"tcp","ports":"6379","sources":["10.20.0.21"],"argus":true,"argusPort":6379,"name":"argus 6379"},` +
		`{"direction":"in","action":"allow","protocol":"tcp","ports":"6379","sources":["203.0.113.50"]}]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, facts); w.Code != http.StatusOK {
		t.Fatalf("facts: %d %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodGet, path, "admin-token", "")
	got = ServerFirewall{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Reported || got.Firewall.Kind != "ufw" || !got.Firewall.Active || len(got.Rules) != 4 || !got.ListeningRead {
		t.Fatalf("after a report: %s", w.Body)
	}
	if got.Rules[0].MadeBy != "outside" || got.Rules[2].MadeBy != "argus" {
		t.Errorf("who made it: %+v", got.Rules)
	}
	reach := map[int]FirewallListen{}
	for _, l := range got.Listening {
		reach[l.Port] = l
	}
	if l := reach[443]; !l.Reach.Everyone {
		t.Errorf("443 is open to everyone: %+v", l)
	}
	if l := reach[22]; l.Reach.Everyone || l.Reach.Public || !slices.Equal(l.Reach.Sources, []string{"10.8.0.0/24"}) {
		t.Errorf("22 is the VPN's only: %+v", l)
	}
	// A rule made by hand lets a public address reach Redis.
	if l := reach[6379]; l.Reach.Everyone || !l.Reach.Public {
		t.Errorf("6379 is reachable from the internet: %+v", l)
	}
	if l := reach[5432]; !l.LocalOnly {
		t.Errorf("5432 is only on loopback: %+v", l)
	}
	if l := reach[3000]; l.Reach.Everyone || len(l.Reach.Sources) != 0 {
		t.Errorf("3000 nobody can reach: %+v", l)
	}

	// The fleet list says the same in one line.
	w = do(t, h, http.MethodGet, "/api/v1/firewalls", "admin-token", "")
	var fleet struct {
		Servers []FirewallFleetRow `json:"servers"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &fleet)
	var row *FirewallFleetRow
	for i := range fleet.Servers {
		if fleet.Servers[i].AssetID == asset.ID {
			row = &fleet.Servers[i]
		}
	}
	if w.Code != http.StatusOK || row == nil {
		t.Fatalf("fleet: %d %s", w.Code, w.Body)
	}
	if !row.Active || row.Firewall != "ufw" || row.Rules != 4 || row.Outside != 3 || !slices.Equal(row.OpenToInternet, []int{443, 6379}) {
		t.Errorf("fleet row: %+v", row)
	}

	// An agent older than firewall reading leaves the report alone.
	old := `{"hostname":"` + asset.Hostname + `","os":"Ubuntu"}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, old); w.Code != http.StatusOK {
		t.Fatalf("old facts: %d %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodGet, path, "admin-token", "")
	got = ServerFirewall{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Reported || len(got.Rules) != 4 {
		t.Errorf("an old agent's facts erased the firewall: %s", w.Body)
	}

	for _, p := range []string{path, "/api/v1/firewalls"} {
		if w := do(t, h, http.MethodGet, p, "operator-token", ""); w.Code != http.StatusForbidden {
			t.Errorf("an operator read %s: %d", p, w.Code)
		}
	}
	if w := do(t, h, http.MethodGet, "/api/v1/assets/00000000-0000-0000-0000-000000000000/firewall", "admin-token", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown asset: %d", w.Code)
	}
}

package control

import (
	"bytes"
	"context"
	"log/slog"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cfapi"
	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/tunnel"
)

func problemKinds(v NetworksView) map[string]NetworkProblem {
	out := map[string]NetworkProblem{}
	for _, p := range v.Problems {
		out[p.Kind] = p
	}
	return out
}

func TestCloudVPNsAreJudged(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	down := cloud.Resource{Kind: cloud.KindVPN, ID: "vpn-1", Name: "office", Region: "ap-southeast-3"}
	cloud.SetTunnels(&down, []cloud.Tunnel{{Address: "3.3.3.1", Why: "IPSEC IS DOWN"}, {Address: "3.3.3.2"}})
	half := cloud.Resource{Kind: cloud.KindVPN, ID: "gw-to-dc", Name: "dc", Region: "asia-southeast2"}
	cloud.SetTunnels(&half, []cloud.Tunnel{{Address: "t0", Up: true}, {Address: "t1", Why: "NO_INCOMING_PACKETS"}})
	v := buildNetworks(nil, now)
	addCloudNetworks(&v, []CloudItem{
		{AccountID: "a1", Provider: "aws", AccountName: "prod", Resource: down},
		{AccountID: "a2", Provider: "gcp", AccountName: "data", Resource: half},
	}, nil, now)
	if len(v.CloudVPNs) != 2 || v.CloudVPNs[0].TunnelsTotal != 2 || len(v.CloudVPNs[0].Tunnels) != 2 ||
		v.CloudVPNs[0].Tunnels[0].Why != "IPSEC IS DOWN" {
		t.Fatalf("cloud vpns %+v", v.CloudVPNs)
	}
	ps := problemKinds(v)
	if p := ps["cloud-vpn-down"]; p.Severity != "down" || !strings.Contains(p.Detail, "IPSEC IS DOWN") {
		t.Errorf("down: %+v", p)
	}
	if p := ps["cloud-vpn-partial"]; p.Severity != "warn" || !strings.Contains(p.Title, "1 of 2") {
		t.Errorf("partial: %+v", p)
	}
	if v.Problems[0].Severity != "down" {
		t.Errorf("down not first: %+v", v.Problems)
	}
}

func TestCloudflareTunnelsMatchServers(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	synced := now.Add(-time.Minute)
	scan := VPNScan{CheckedAt: now, Cloudflared: []CFConnector{{Metrics: "127.0.0.1:20241", Ready: true, ReadyConnections: 4, ConnectorID: "AAAA"}}}
	v := buildNetworks([]networkRow{netRow("a1", "web1", now, scan, "")}, now)
	acct := CloudflareAccount{ID: "c1", Name: "main", AccountID: "0123456789abcdef0123456789abcdef", SyncedAt: &synced,
		snap: &cfapi.Snapshot{
			Read: []string{cfapi.ListTunnels, cfapi.ListRoutes},
			Tunnels: []cfapi.Tunnel{
				{ID: "t1", Name: "web", Status: "healthy", Connections: []cfapi.Connection{
					{ClientID: "aaaa", ClientVersion: "2026.10.0", ColoName: "sin01"},
					{ClientID: "aaaa", ClientVersion: "2026.10.0", ColoName: "sin02"}}},
				{ID: "t2", Name: "intranet", Status: "degraded", Connections: []cfapi.Connection{
					{ClientID: "bbbb", ClientVersion: "2024.6.1", ColoName: "cgk01", OriginIP: "198.51.100.4"},
					{ClientID: "aaaa", ClientVersion: "2026.10.0", ColoName: "cgk01"}}},
				{ID: "t3", Name: "old", Status: "inactive"},
				{ID: "t4", Name: "gone", Status: "down"},
			},
			Routes: []cfapi.Route{{Network: "10.20.0.0/16", TunnelID: "t3"}},
		}}
	addCloudNetworks(&v, nil, []CloudflareAccount{acct}, now)
	if v.Tunnels[0].TunnelName == "" {
		t.Errorf("the server's connector is not matched to its tunnel: %+v", v.Tunnels[0])
	}
	cf := v.Cloudflare[0]
	if cf.Stale || len(cf.Tunnels) != 4 {
		t.Fatalf("account %+v", cf)
	}
	for _, tn := range cf.Tunnels {
		if tn.Name == "web" && (len(tn.Connectors) != 1 || tn.Connectors[0].Hostname != "web1" || tn.Connectors[0].Connections != 2 ||
			len(tn.Connectors[0].Locations) != 2) {
			t.Errorf("web: %+v", tn.Connectors)
		}
	}
	ps := problemKinds(v)
	for _, k := range []string{"cf-single-connector", "cf-tunnel-degraded", "cf-unknown-machine", "cf-old-version", "cf-inactive", "cf-tunnel-down"} {
		if _, ok := ps[k]; !ok {
			t.Errorf("no %s problem: %+v", k, v.Problems)
		}
	}
	if p := ps["cf-unknown-machine"]; !strings.Contains(p.Title, "198.51.100.4") {
		t.Errorf("unknown machine: %+v", p)
	}
	if ps["cf-tunnel-down"].Severity != "down" {
		t.Errorf("tunnel down is not down")
	}
}

func TestCloudflaredOld(t *testing.T) {
	at := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	for v, want := range map[string]bool{"2026.1.0": false, "2025.6.1": true, "2024.1.0": true, "dev": false, "": false} {
		if got := cloudflaredOld(v, at); got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
}

func TestNetworkAlertChanges(t *testing.T) {
	v := NetworksView{quiet: map[string]bool{"quiet-asset": true}, Problems: []NetworkProblem{
		{Severity: "down", Key: "peer-down:a1:wg0/K"},
		{Severity: "down", Key: "cloud-vpn::x/y/z"},
		{Severity: "warn", Key: "cf-single-connector::x/t"},
	}}
	open := map[string]bool{
		"network:cloud-vpn::x/y/z":         true, // still down: stays
		"network:peer-down:a2:wg0/K":       true, // back up: closes
		"network:peer-down:quiet-asset:wg": true, // not known: stays
	}
	raise, closing := networkAlertChanges(v, open)
	if len(raise) != 1 || raise[0].Key != "peer-down:a1:wg0/K" {
		t.Errorf("raise %+v", raise)
	}
	if len(closing) != 1 || closing[0] != "network:peer-down:a2:wg0/K" {
		t.Errorf("closing %v", closing)
	}
}

// The routes: fleet roles read, admins connect; the token is tried first,
// kept sealed and never returned; the account shows up on Networks.
func TestCloudflareAccountRoutes(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	var logs bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	auditor, _ := newAccount(t, s, "auditor")
	a.UserTokens["auditor-token"] = auditor
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	a.Tunnels = &TunnelHub{GrantKey: key}
	var reads atomic.Int32
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Unauthorized"}]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/cfd_tunnel") {
			reads.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"t1","name":"web","status":"down","connections":[]}],"result_info":{"page":1,"total_pages":1,"total_count":1}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Unauthorized"}]}`))
	}))
	defer cf.Close()
	a.cloudflareBase, a.cloudflareHTTP = cf.URL, cf.Client()
	ctx := context.Background()
	_, _ = s.pool.Exec(ctx, `DELETE FROM cloudflare_accounts`)
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM cloudflare_accounts`) })

	acct := "0123456789abcdef0123456789abcdef"
	if w := do(t, h, http.MethodPost, "/api/v1/networks/cloudflare", "auditor-token",
		`{"name":"main","accountId":"`+acct+`","token":"good-token"}`); w.Code != http.StatusForbidden {
		t.Errorf("an auditor connected an account: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/networks/cloudflare", "admin-token",
		`{"name":"main","accountId":"`+acct+`","token":"bad-token"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a refused token was saved: %d %s", w.Code, w.Body)
	}
	w := do(t, h, http.MethodPost, "/api/v1/networks/cloudflare", "admin-token",
		`{"name":"main","accountId":"`+acct+`","token":"good-token"}`)
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "good-token") {
		t.Fatalf("add: %d %s\n%s", w.Code, w.Body, logs.String())
	}
	var added CloudflareAccount
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	if added.TokenTail != "ken" || added.SyncedAt == nil {
		t.Errorf("added %+v", added)
	}
	var sealed []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_sealed FROM cloudflare_accounts WHERE id = $1`, added.ID).Scan(&sealed); err != nil ||
		strings.Contains(string(sealed), "good-token") {
		t.Errorf("token stored in the clear (%v)", err)
	}

	w = do(t, h, http.MethodGet, "/api/v1/networks/cloudflare", "auditor-token", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "good-token") || !strings.Contains(w.Body.String(), `"canSave":true`) {
		t.Errorf("list: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/networks/cloudflare/"+added.ID+"/sync", "admin-token", ""); w.Code != http.StatusOK {
		t.Errorf("sync: %d %s", w.Code, w.Body)
	}
	if reads.Load() != 2 {
		t.Errorf("Cloudflare read %d times", reads.Load())
	}

	w = do(t, h, http.MethodGet, "/api/v1/networks", "admin-token", "")
	var v NetworksView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if len(v.Cloudflare) != 1 || len(v.Cloudflare[0].Tunnels) != 1 || v.Cloudflare[0].Tunnels[0].Status != "down" {
		t.Fatalf("networks: %s", w.Body)
	}
	if v.Cloudflare[0].Missing[cfapi.ListDevices] == "" {
		t.Errorf("a list the token could not read is not said to be missing: %+v", v.Cloudflare[0])
	}

	if w := do(t, h, http.MethodPut, "/api/v1/networks/cloudflare/"+added.ID, "admin-token", `{"name":"renamed"}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "renamed") || !strings.Contains(w.Body.String(), `"tokenTail":"ken"`) {
		t.Errorf("rename: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodDelete, "/api/v1/networks/cloudflare/"+added.ID, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: %d", w.Code)
	}
}

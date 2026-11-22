package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/tunnel"
)

func TestCheckIPsecDraft(t *testing.T) {
	hosts := map[string]string{"gw": "gw-jkt-01"}
	own := map[string][]netip.Prefix{"gw": {netip.MustParsePrefix("192.168.10.5/32")}}
	good := func() IPsecDraft {
		return IPsecDraft{Name: "office to aws", AssetID: "gw", Peer: "52.1.2.3", LocalRanges: []string{"192.168.10.0/24"},
			RemoteRanges: []string{" 10.30.0.0/16", ""}}
	}
	d := good()
	if err := checkIPsecDraft(&d, hosts, own); err != nil || d.Proposals != "strong" || d.Hostname != "gw-jkt-01" || len(d.RemoteRanges) != 1 {
		t.Fatalf("%v %+v", err, d)
	}
	for name, c := range map[string]struct {
		change func(*IPsecDraft)
		want   string
	}{
		"no server":  {func(d *IPsecDraft) { d.AssetID = "x" }, "pick the Argus server"},
		"bad peer":   {func(d *IPsecDraft) { d.Peer = "52.1.2.3\n}" }, "public IPv4 address or hostname"},
		"public":     {func(d *IPsecDraft) { d.RemoteRanges = []string{"0.0.0.0/0"} }, "not a private range"},
		"both sides": {func(d *IPsecDraft) { d.RemoteRanges = []string{"192.168.0.0/16"} }, "on both sides"},
		"own address": {func(d *IPsecDraft) {
			d.LocalRanges, d.RemoteRanges = []string{"10.1.0.0/16"}, []string{"192.168.0.0/16"}
		}, "own address"},
		"weak":      {func(d *IPsecDraft) { d.Proposals = "3des" }, "strong or compatible"},
		"no remote": {func(d *IPsecDraft) { d.RemoteRanges = nil }, "between 1 and 16"},
	} {
		d := good()
		c.change(&d)
		if err := checkIPsecDraft(&d, hosts, own); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNewPSK(t *testing.T) {
	for range 50 {
		k, err := newPSK()
		if err != nil || !reIPsecPSK.MatchString(k) || len(k) != 40 || k[0] >= '0' && k[0] <= '9' {
			t.Fatalf("%q %v", k, err)
		}
	}
}

func TestMatchCloudEnds(t *testing.T) {
	r := cloud.Resource{Kind: cloud.KindVPN, ID: "vpn-1", Name: "office", Region: "ap-southeast-3"}
	cloud.SetTunnels(&r, []cloud.Tunnel{{Address: "52.1.2.3", Up: false, Why: "IPSEC IS DOWN"}, {Address: "52.1.2.4", Up: true}})
	links := []IPsecLink{{Peer: "52.1.2.3"}, {Peer: "198.51.100.1"}}
	matchCloudEnds(links, []CloudItem{{Provider: "aws", AccountName: "prod", Resource: r}})
	if links[0].Cloud == nil || links[0].Cloud.Up || links[0].Cloud.Name != "office" || links[1].Cloud != nil {
		t.Fatalf("%+v %+v", links[0].Cloud, links[1].Cloud)
	}
}

func TestIPsecLinkRoutes(t *testing.T) {
	e := wgEnv{packagesAPI(t)}
	ctx := context.Background()
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	e.a.Tunnels = &TunnelHub{GrantKey: key}
	_, _ = e.s.pool.Exec(ctx, `DELETE FROM ipsec_links`)
	t.Cleanup(func() { _, _ = e.s.pool.Exec(ctx, `DELETE FROM ipsec_links`) })
	gw, cred := e.server(t)

	body := `{"name":"office to aws","assetId":"` + gw.ID + `","peer":"52.1.2.3","localRanges":["192.168.10.0/24"],"remoteRanges":["10.30.0.0/16"]}`
	w := do(t, e.h, http.MethodPost, "/api/v1/networks/ipsec", "admin-token", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var l IPsecLink
	_ = json.Unmarshal(w.Body.Bytes(), &l)
	beat := &WGNetBeat{Supported: true}
	if o := e.beat(t, cred, gw.Hostname, beat); o == nil || len(o.IPsec) != 0 {
		t.Fatalf("before approval: %+v", o)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/ipsec/"+l.ID+"/approve", "admin-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("approved their own: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/ipsec/"+l.ID+"/approve", "second-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	o := e.beat(t, cred, gw.Hostname, beat)
	if len(o.IPsec) != 1 || o.IPsec[0].Conn != l.Conn || len(o.IPsec[0].PSK) != 40 || o.IPsec[0].Proposals != "strong" {
		t.Fatalf("after approval: %+v", o)
	}
	psk := o.IPsec[0].PSK

	// The key is in no list; showing it is audited, and auditors cannot.
	e.beat(t, cred, gw.Hostname, &WGNetBeat{Supported: true, IPsec: map[string]string{l.ID: "down"},
		Run: &WGNetRun{ID: "r1", State: "applied", At: time.Now(), Networks: []string{l.ID}}})
	w = do(t, e.h, http.MethodGet, "/api/v1/networks/ipsec", "auditor-token", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), psk) || !strings.Contains(w.Body.String(), `"link":"down"`) ||
		!strings.Contains(w.Body.String(), `"server":"applied"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/ipsec/"+l.ID+"/key", "auditor-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an auditor saw the key: %d", w.Code)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/networks/ipsec/"+l.ID+"/key", "second-token", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), psk) {
		t.Fatalf("key: %d %s", w.Code, w.Body)
	}
	var shown int
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'ipsec.link.key_shown' AND target = $1`, "ipsec link office to aws").Scan(&shown)
	if shown == 0 {
		t.Error("showing the key was not audited")
	}
	// A link down is a problem on Networks.
	v, err := e.s.Networks(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range v.Problems {
		found = found || p.Kind == "ipsec-down" && strings.Contains(p.Title, "office to aws")
	}
	if !found {
		t.Errorf("no problem for the link down: %+v", v.Problems)
	}

	// A new key waits for approval too.
	if w := do(t, e.h, http.MethodPut, "/api/v1/networks/ipsec/"+l.ID, "second-token",
		strings.Replace(body, `"peer"`, `"newKey":true,"peer"`, 1)); w.Code != http.StatusAccepted {
		t.Fatalf("change: %d %s", w.Code, w.Body)
	}
	if o := e.beat(t, cred, gw.Hostname, beat); o.IPsec[0].PSK != psk {
		t.Error("the new key was used before approval")
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/networks/ipsec/"+l.ID+"/approve", "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("approve change: %d %s", w.Code, w.Body)
	}
	if o := e.beat(t, cred, gw.Hostname, beat); o.IPsec[0].PSK == psk || len(o.IPsec[0].PSK) != 40 {
		t.Error("the new key was not used")
	}
}

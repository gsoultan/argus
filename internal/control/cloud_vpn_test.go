package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/tunnel"
)

// Making the AWS end of an IPsec link: checked like the link itself, asked
// like any Change-level change on Production with its price in the plan,
// made with a key that is never in the change, and then the link from the
// server to the first tunnel waits for its own approval, holding that key.
func TestCloudVPNCreate(t *testing.T) {
	e := changeAPI(t)
	ctx := context.Background()
	region := "ap-southeast-3"
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	e.a.Tunnels = &TunnelHub{GrantKey: key}
	_, _ = e.s.pool.Exec(ctx, `DELETE FROM ipsec_links`)
	t.Cleanup(func() { _, _ = e.s.pool.Exec(ctx, `DELETE FROM ipsec_links`) })
	e.fake.Resources[region] = append(e.fake.Resources[region],
		cloud.Resource{Kind: cloud.KindNetwork, ID: "vpc-0abc1234", Name: "prod-vpc", Region: region, State: "available", Size: "10.30.0.0/16"})
	e.resync(t)
	host := unique("gw") + ".example"
	gw, err := e.s.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.21", Principals: []string{"ops"}, CredentialRef: "injected"})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(spec string, want int) CloudChange {
		return e.ask(t, "admin-token", `{"accountId":"`+e.acct.ID+`","region":"`+region+`","action":"vpn-create","reason":"office link","spec":`+spec+`}`, want)
	}
	spec := func(over string) string {
		s := `{"name":"Jakarta office","network":"vpc-0abc1234","assetId":"` + gw.ID + `","serverAddress":"36.72.1.9",` +
			`"serverRange":"192.168.10.0/24","ciphers":"strong","propagate":true` + over + `}`
		return strings.Replace(s, `,}`, `}`, 1)
	}

	// Refused before anything is asked.
	ask(strings.Replace(spec(""), "vpc-0abc1234", "vpc-0fff9999", 1), http.StatusNotFound)
	ask(strings.Replace(spec(""), "36.72.1.9", "10.0.0.9", 1), http.StatusBadRequest)
	ask(strings.Replace(spec(""), "192.168.10.0/24", "10.30.4.0/24", 1), http.StatusBadRequest)
	ask(spec(`,"cloudRange":"10.40.0.0/16"`), http.StatusBadRequest)
	ask(strings.Replace(spec(""), gw.ID, "00000000-0000-0000-0000-000000000000", 1), http.StatusBadRequest)

	c := ask(spec(""), http.StatusAccepted)
	if c.State != "awaiting_approval" || c.Level != cloud.LevelChange || !strings.Contains(c.Summary, "US$0.05 an hour") ||
		!strings.Contains(c.Summary, "prod-vpc (10.30.0.0/16) to "+host+" at 36.72.1.9 (192.168.10.0/24)") {
		t.Fatalf("plan: %+v", c)
	}
	done := e.decide(t, "approver-token", c.ID, true)
	if done.State != "applied" || !strings.HasPrefix(done.ResourceID, "vpn-") {
		t.Fatalf("applied: %+v", done)
	}
	var res cloud.Result
	_ = json.Unmarshal(done.Result, &res)
	if !strings.Contains(res.Detail, "virtual private gateway") || !strings.Contains(res.Detail, "waits for another admin's approval") {
		t.Fatalf("detail: %q", res.Detail)
	}

	links, err := e.s.IPsecLinks(ctx)
	if err != nil || len(links) != 1 {
		t.Fatalf("links: %+v %v", links, err)
	}
	l := links[0]
	if l.Name != "Jakarta office" || l.AssetID != gw.ID || !strings.HasPrefix(l.Peer, "203.0.113.") || l.RequestedBy != e.admin ||
		l.Pending == nil || l.LocalRanges[0] != "192.168.10.0/24" || l.RemoteRanges[0] != "10.30.0.0/16" {
		t.Fatalf("link: %+v", l)
	}
	_, sealed, err := e.s.ipsecLink(ctx, l.ID)
	if err != nil || len(sealed) == 0 {
		t.Fatalf("no key kept: %v", err)
	}
	psk, err := e.a.openPSK(ctx, sealed)
	if err != nil || len(psk) != 40 {
		t.Fatalf("key: %v", err)
	}
	for _, b := range [][]byte{done.Spec, done.Plan, done.Result} {
		if strings.Contains(string(b), psk) {
			t.Fatal("the key is in the change")
		}
	}

	// The same name again is refused: the link already has it.
	ask(spec(""), http.StatusConflict)
}

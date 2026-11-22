//go:build !windows

package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func askCheckRun(t *testing.T, svc TunnelService, key ed25519.PrivateKey, g tunnel.Grant) (tunnel.Answer, tunnel.AttemptResult) {
	t.Helper()
	tok, err := tunnel.SignGrant(key, g)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc(context.Background(), &Call{Header: tunnel.Header{Service: tunnel.CheckRunService, Grant: tok},
			Stream: pipeStream{a}, GrantKey: key.Public().(ed25519.PublicKey)})
	}()
	defer func() { _ = b.Close(); <-done }()
	var ans tunnel.Answer
	if err := tunnel.ReadJSON(b, &ans); err != nil {
		t.Fatal(err)
	}
	var res tunnel.AttemptResult
	if !ans.OK {
		return ans, res
	}
	typ, p, err := tunnel.NewFrameReader(b).Next()
	if err != nil || typ != tunnel.FrameAttemptResult {
		t.Fatalf("no result: %v %q", err, typ)
	}
	if err := json.Unmarshal(p, &res); err != nil {
		t.Fatal(err)
	}
	return ans, res
}

func checkGrant(c tunnel.GrantCheck) tunnel.Grant {
	g := tunnel.NewGrant("argus-control", "edge-01", "checkrun", "")
	g.Service = tunnel.CheckRunService
	g.Check = &c
	return g
}

// The agent runs the attempt its grant names, refuses a kind it does not
// know so argus-control can say it needs updating, and refuses a grant for
// something else.
func TestCheckRunService(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	var asked tunnel.SNMPQuery
	svc := CheckRun(CheckRunConfig{Verifier: &tunnel.GrantVerifier{Host: "edge-01"}, SNMP: &tunnel.SNMPProbe{
		Dial: func(_ context.Context, q tunnel.SNMPQuery) (*tunnel.SNMPAnswer, string, error) {
			asked = q
			return &tunnel.SNMPAnswer{Name: "sw1", UptimeSec: 9, Interfaces: []tunnel.SNMPInterface{{Index: 3, Name: "Gi0/3", Up: false}}}, "", nil
		},
	}})
	q := tunnel.SNMPQuery{Host: "10.0.0.5", Version: "v2c", Community: "s3cret", Watch: []int{3}}
	ans, res := askCheckRun(t, svc, key, checkGrant(tunnel.GrantCheck{Kind: "snmp", SNMP: &q}))
	if !ans.OK || res.OK || res.Fail != tunnel.SNMPFailInterface || !strings.Contains(res.Detail, "Gi0/3 is down") || asked.Community != "s3cret" {
		t.Fatalf("snmp: %+v %+v %+v", ans, res, asked)
	}
	ans, _ = askCheckRun(t, svc, key, checkGrant(tunnel.GrantCheck{Kind: "ping"}))
	if ans.OK || !strings.Contains(ans.Error, tunnel.ErrUnknownCheckKind.Error()) {
		t.Errorf("unknown kind: %+v", ans)
	}
	other := tunnel.NewGrant("argus-control", "edge-01", "sitecheck", "")
	other.Service = tunnel.SiteCheckService
	if ans, _ := askCheckRun(t, svc, key, other); ans.OK {
		t.Error("a grant for another service was accepted")
	}
	wrongHost := checkGrant(tunnel.GrantCheck{Kind: "snmp", SNMP: &q})
	wrongHost.Host = "edge-02"
	if ans, _ := askCheckRun(t, svc, key, wrongHost); ans.OK {
		t.Error("a grant for another server was accepted")
	}
}

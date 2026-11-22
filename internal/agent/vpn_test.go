package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeWG answers wg the way wg 1.0 does, and fails anything that would print
// a private key.
func fakeWG(t *testing.T) runner {
	return func(_ context.Context, name string, args ...string) (string, error) {
		cmd := name + " " + strings.Join(args, " ")
		for _, a := range args {
			switch a {
			case "dump", "showconf", "private-key", "preshared-keys", "all":
				t.Fatalf("ran %q, which prints secrets", cmd)
			}
		}
		switch cmd {
		case "wg show interfaces":
			return "wg0 -bad\n", nil
		case "wg show wg0 public-key":
			return "PUBwg0=\n", nil
		case "wg show wg0 listen-port":
			return "51820\n", nil
		case "wg show wg0 endpoints":
			return "PEERa=\t203.0.113.7:51820\nPEERb=\t(none)\n", nil
		case "wg show wg0 allowed-ips":
			return "PEERa=\t10.88.0.2/32 192.168.10.0/24\nPEERb=\t(none)\n", nil
		case "wg show wg0 latest-handshakes":
			return "PEERa=\t1700000000\nPEERb=\t0\n", nil
		case "wg show wg0 persistent-keepalive":
			return "PEERa=\t25\nPEERb=\toff\n", nil
		case "wg show wg0 transfer":
			return "PEERa=\t1000\t2000\nPEERb=\t0\t0\n", nil
		}
		return "", errors.New("unexpected " + cmd)
	}
}

func TestWireGuardReadsPublicFieldsOnly(t *testing.T) {
	now := time.Unix(1700000060, 0)
	scan := discoverVPN(context.Background(), fakeWG(t), func() []string { t.Fatal("sysfs read although wg answered"); return nil },
		nil, nil, now)
	if len(scan.WireGuard) != 1 {
		t.Fatalf("interfaces = %+v, want wg0 only (-bad is not a name)", scan.WireGuard)
	}
	wi := scan.WireGuard[0]
	if wi.PublicKey != "PUBwg0=" || wi.ListenPort != 51820 || len(wi.Peers) != 2 {
		t.Fatalf("interface = %+v", wi)
	}
	a, b := wi.Peers[0], wi.Peers[1]
	if a.Endpoint != "203.0.113.7:51820" || len(a.AllowedIPs) != 2 || a.RxBytes != 1000 || a.TxBytes != 2000 ||
		!a.LatestHandshake.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("peer a = %+v", a)
	}
	if a.Keepalive != 25 || b.Keepalive != 0 {
		t.Fatalf("keepalive a=%d b=%d", a.Keepalive, b.Keepalive)
	}
	if b.Endpoint != "" || len(b.AllowedIPs) != 0 || !b.LatestHandshake.IsZero() {
		t.Fatalf("peer b = %+v", b)
	}
	if strings.Join(scan.Sources, ",") != "wg" {
		t.Fatalf("sources = %v", scan.Sources)
	}
}

func TestWireGuardFallsBackToSysfs(t *testing.T) {
	noWG := func(context.Context, string, ...string) (string, error) { return "", errors.New("not found") }
	scan := discoverVPN(context.Background(), noWG, func() []string { return []string{"wg-office"} }, nil, nil, time.Now())
	if len(scan.WireGuard) != 1 || scan.WireGuard[0].Name != "wg-office" || scan.Sources[0] != "sysfs" {
		t.Fatalf("scan = %+v", scan)
	}
}

type fakeRT map[string]string

func (f fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := f[r.URL.Host+r.URL.Path]
	if !ok {
		return nil, errors.New("refused")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func TestCloudflaredAskedOnLoopbackOnly(t *testing.T) {
	rt := fakeRT{
		"127.0.0.1:20241/ready":   `{"status":200,"readyConnections":4,"connectorId":"0b6a1c2e-1111-2222-3333-444455556666"}`,
		"127.0.0.1:20241/metrics": "# HELP build_info\nbuild_info{goversion=\"go1.24\",revision=\"x\",type=\"\",version=\"2025.11.1\"} 1\n",
		"10.0.0.5:20242/ready":    `{"status":200,"readyConnections":4}`,
	}
	listeners := []Listener{
		{Address: "127.0.0.1", Port: 20241, Process: "cloudflared"},
		{Address: "10.0.0.5", Port: 20242, Process: "cloudflared"}, // not loopback: never asked
		{Address: "127.0.0.1", Port: 5432, Process: "postgres"},
		{Address: "127.0.0.1", Port: 20243, Process: "cloudflared"}, // does not answer
	}
	noWG := func(context.Context, string, ...string) (string, error) { return "", errors.New("no") }
	scan := discoverVPN(context.Background(), noWG, func() []string { return nil }, listeners, rt, time.Now())
	if len(scan.Cloudflared) != 2 {
		t.Fatalf("connectors = %+v", scan.Cloudflared)
	}
	c := scan.Cloudflared[0]
	if !c.Ready || c.ReadyConnections != 4 || c.ConnectorID != "0b6a1c2e-1111-2222-3333-444455556666" || c.Version != "2025.11.1" {
		t.Fatalf("connector = %+v", c)
	}
	if d := scan.Cloudflared[1]; d.Ready || d.Error == "" {
		t.Fatalf("silent connector = %+v", d)
	}
}

func TestVPNStateIgnoresCounters(t *testing.T) {
	now := time.Now()
	scan := func(rx uint64, hs time.Time) VPNScan {
		return VPNScan{WireGuard: []WGInterface{{Name: "wg0", Up: true, Peers: []WGPeer{{PublicKey: "A", RxBytes: rx, LatestHandshake: hs}}}}}
	}
	if scan(1, now.Add(-time.Minute)).State(now) != scan(999, now.Add(-30*time.Second)).State(now) {
		t.Fatal("traffic changed the state")
	}
	if scan(1, now.Add(-time.Minute)).State(now) == scan(1, now.Add(-10*time.Minute)).State(now) {
		t.Fatal("a stale handshake did not change the state")
	}
}

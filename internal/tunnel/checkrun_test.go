package tunnel

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSNMPQueryValidate(t *testing.T) {
	ok := SNMPQuery{Host: "10.0.0.1", User: "argus", AuthProto: "SHA", AuthPass: "12345678", PrivProto: "AES", PrivPass: "12345678"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("authPriv: %v", err)
	}
	for name, q := range map[string]SNMPQuery{
		"no host":           {User: "a"},
		"link-local":        {Host: "169.254.169.254", Version: "v2c", Community: "x"},
		"v2c no community":  {Host: "sw1", Version: "v2c"},
		"v3 no user":        {Host: "sw1"},
		"short auth":        {Host: "sw1", User: "a", AuthProto: "SHA", AuthPass: "short"},
		"priv without auth": {Host: "sw1", User: "a", PrivProto: "AES", PrivPass: "12345678"},
		"unknown protocol":  {Host: "sw1", User: "a", AuthProto: "SHA3", AuthPass: "12345678"},
		"unknown version":   {Host: "sw1", Version: "v1", Community: "x"},
		"long timeout":      {Host: "sw1", Version: "v2c", Community: "x", TimeoutSec: 61},
	} {
		if q.Validate() == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	if err := (GrantCheck{Kind: "ping"}).Validate(); err != ErrUnknownCheckKind {
		t.Errorf("unknown kind = %v", err)
	}
}

func TestSNMPVerdict(t *testing.T) {
	a := &SNMPAnswer{Name: "sw1", Interfaces: []SNMPInterface{{Index: 1, Name: "Gi0/1", Up: true}, {Index: 2, Name: "Gi0/2", Alias: "uplink"}}}
	if ok, d := SNMPVerdict(SNMPQuery{}, a); !ok || d != "sw1 answered" {
		t.Errorf("nothing watched: %v %q", ok, d)
	}
	if ok, d := SNMPVerdict(SNMPQuery{Watch: []int{1}}, a); !ok || !strings.Contains(d, "watched interface is up") {
		t.Errorf("watched up: %v %q", ok, d)
	}
	if ok, d := SNMPVerdict(SNMPQuery{Watch: []int{1, 2, 9}}, a); ok || !strings.Contains(d, "Gi0/2 (uplink) is down") || !strings.Contains(d, "interface 9 is gone") {
		t.Errorf("watched down: %v %q", ok, d)
	}
}

// A device that never answers is "no answer", within the timeout.
func TestSNMPProbeNoAnswer(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer pc.Close()
	port := pc.LocalAddr().(*net.UDPAddr).Port
	start := time.Now()
	res := SNMPProbe{}.Probe(context.Background(), SNMPQuery{Host: "127.0.0.1", Port: port, Version: SNMPv2c, Community: "public", TimeoutSec: 1})
	if res.OK || res.Fail != SNMPFailNoAnswer || !strings.Contains(res.Detail, "did not answer") {
		t.Errorf("%+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

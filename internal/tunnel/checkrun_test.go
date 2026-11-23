package tunnel

import (
	"context"
	"errors"
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

func TestSNMPDiscoverValidate(t *testing.T) {
	if err := (SNMPDiscover{Host: "10.0.0.1", Communities: []string{"public"}}).Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, d := range map[string]SNMPDiscover{
		"no host":        {Communities: []string{"public"}},
		"link-local":     {Host: "169.254.169.254", Communities: []string{"public"}},
		"no communities": {Host: "sw1"},
		"empty string":   {Host: "sw1", Communities: []string{""}},
		"too many":       {Host: "sw1", Communities: make([]string, snmpDiscoverMax+1)},
		"long timeout":   {Host: "sw1", Communities: []string{"public"}, TimeoutSec: SNMPTimeoutMax + 1},
	} {
		if d.Validate() == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	if err := (GrantCheck{Kind: CheckKindSNMPDiscover}).Validate(); err == nil {
		t.Error("discover with no payload was allowed")
	}
	if err := (GrantCheck{Kind: CheckKindSNMPDiscover, Discover: &SNMPDiscover{Host: "sw1", Communities: []string{"x"}}}).Validate(); err != nil {
		t.Errorf("valid discover grant: %v", err)
	}
}

// Discover stops at the first string that answers and names it.
func TestSNMPProbeDiscover(t *testing.T) {
	var tried []string
	p := SNMPProbe{DialSystem: func(_ context.Context, q SNMPQuery) (*SNMPAnswer, string, error) {
		tried = append(tried, q.Community)
		if q.Version != SNMPv2c {
			t.Errorf("discovery used %q, not v2c", q.Version)
		}
		if q.Community == "netmon" {
			return &SNMPAnswer{Name: "sw1", Description: "Cisco", UptimeSec: 42}, "", nil
		}
		return nil, SNMPFailNoAnswer, errors.New("no answer")
	}}
	res := p.Discover(context.Background(), SNMPDiscover{Host: "10.0.0.1", Communities: []string{"public", "private", "netmon", "late"}})
	if !res.Found || res.Community != "netmon" || res.SNMP == nil || res.SNMP.Name != "sw1" {
		t.Fatalf("found = %+v", res)
	}
	if res.Tried != 3 || len(tried) != 3 {
		t.Errorf("tried %d (%v), want 3 and to stop before 'late'", res.Tried, tried)
	}

	none := SNMPProbe{DialSystem: func(_ context.Context, _ SNMPQuery) (*SNMPAnswer, string, error) {
		return nil, SNMPFailNoAnswer, errors.New("no answer")
	}}.Discover(context.Background(), SNMPDiscover{Host: "10.0.0.1", Communities: []string{"public", "private"}})
	if none.Found || none.Tried != 2 || none.Detail == "" {
		t.Errorf("none = %+v", none)
	}
}

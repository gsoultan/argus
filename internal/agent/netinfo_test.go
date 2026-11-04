package agent

import (
	"fmt"
	"net"
	"testing"
)

func cidr(t *testing.T, s string) net.Addr {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	n.IP = ip
	return n
}

func TestInterfacesKeepWhatReachesTheMachine(t *testing.T) {
	mac, _ := net.ParseMAC("52:54:00:AB:CD:EF")
	got := interfacesFrom([]netIface{
		{name: "lo", flags: net.FlagUp | net.FlagLoopback, addrs: []net.Addr{cidr(t, "127.0.0.1/8")}},
		{name: "eth0", flags: net.FlagUp, mac: mac, addrs: []net.Addr{
			cidr(t, "203.0.113.10/24"), cidr(t, "fe80::1/64"), cidr(t, "2001:db8::10/64"),
		}},
		{name: "eth1", flags: 0, addrs: []net.Addr{cidr(t, "10.0.0.5/24")}},          // down
		{name: "veth0", flags: net.FlagUp, addrs: []net.Addr{cidr(t, "fe80::2/64")}}, // link-local only
		{name: "wg0", flags: net.FlagUp, addrs: []net.Addr{cidr(t, "10.8.0.1/24")}},
	})
	want := `[{eth0 52:54:00:ab:cd:ef [203.0.113.10/24 2001:db8::10/64]} {wg0  [10.8.0.1/24]}]`
	if fmt.Sprint(got) != want {
		t.Fatalf("got %v\nwant %s", got, want)
	}
}

func TestInterfacesAreBounded(t *testing.T) {
	var in []netIface
	for i := 0; i < maxInterfaces+5; i++ {
		var addrs []net.Addr
		for j := 0; j < maxInterfaceAddrs+3; j++ {
			addrs = append(addrs, cidr(t, fmt.Sprintf("10.%d.%d.1/24", i, j)))
		}
		in = append(in, netIface{name: fmt.Sprint("br", i), flags: net.FlagUp, addrs: addrs})
	}
	got := interfacesFrom(in)
	if len(got) != maxInterfaces || len(got[0].Addresses) != maxInterfaceAddrs {
		t.Fatalf("%d interfaces, %d addresses on the first", len(got), len(got[0].Addresses))
	}
}

func TestDiscoverInterfacesRuns(t *testing.T) {
	for _, i := range DiscoverInterfaces() {
		if i.Name == "" || len(i.Addresses) == 0 {
			t.Fatalf("empty interface reported: %+v", i)
		}
	}
}

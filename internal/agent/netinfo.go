package agent

import (
	"net"
	"strings"
)

/*
The addresses each network interface holds, for the System card.

Read from the interface table the Go runtime exposes (netlink on Linux, the
IP helper API on Windows), which `ip addr` or `ipconfig` show any local user.
Only interfaces that are up, never loopback, and never link-local addresses:
those are not how anyone reaches the machine.

An address held by an interface is the only kind reported. A cloud machine
behind NAT has a public address that no interface holds, and the agent does
not ask anything outside the machine to find it.
*/

// NetInterface is one network interface and the addresses it holds.
type NetInterface struct {
	Name string `json:"name"`
	// MAC is the hardware address, when the interface has one.
	MAC string `json:"mac,omitempty"`
	// Addresses are in CIDR form, "10.0.0.5/24", in the order the system
	// lists them.
	Addresses []string `json:"addresses"`
}

const (
	// maxInterfaces and maxInterfaceAddrs bound the report. A host running
	// many containers can hold more bridges than this; the console lists
	// what fits.
	maxInterfaces     = 32
	maxInterfaceAddrs = 16
)

// netIface is what interfacesFrom needs of a net.Interface, so the filter
// can be tested without the machine's own interfaces.
type netIface struct {
	name  string
	flags net.Flags
	mac   net.HardwareAddr
	addrs []net.Addr
}

// DiscoverInterfaces lists the interfaces that are up and hold an address.
// Best-effort: nil when the interface table cannot be read.
func DiscoverInterfaces() []NetInterface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	in := make([]netIface, 0, len(ifs))
	for _, i := range ifs {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		in = append(in, netIface{name: i.Name, flags: i.Flags, mac: i.HardwareAddr, addrs: addrs})
	}
	return interfacesFrom(in)
}

func interfacesFrom(in []netIface) []NetInterface {
	var out []NetInterface
	for _, i := range in {
		if len(out) == maxInterfaces {
			break
		}
		if i.flags&net.FlagUp == 0 || i.flags&net.FlagLoopback != 0 {
			continue
		}
		var addrs []string
		for _, a := range i.addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() || ipnet.IP.IsUnspecified() {
				continue
			}
			if len(addrs) == maxInterfaceAddrs {
				break
			}
			addrs = append(addrs, ipnet.String())
		}
		if len(addrs) == 0 {
			continue
		}
		mac := ""
		if len(i.mac) > 0 {
			mac = strings.ToLower(i.mac.String())
		}
		out = append(out, NetInterface{Name: i.name, MAC: mac, Addresses: addrs})
	}
	return out
}

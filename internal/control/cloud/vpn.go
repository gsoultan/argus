package cloud

import (
	"strconv"
	"strings"
)

// KindVPN is a site-to-site VPN connection: AWS Site-to-Site VPN, an Azure
// virtual network gateway connection, a Google Cloud VPN tunnel. Read only;
// Networks in the console shows them beside the links the agents report.
//
// Never stored: a connection's pre-shared keys. AWS hands them out in
// CustomerGatewayConfiguration and Azure in sharedKey; neither is read
// into a Resource.
const KindVPN Kind = "vpn"

// Details a VPN connection carries.
const (
	// DetailTunnelsUp and DetailTunnelsTotal count the connection's tunnels.
	DetailTunnelsUp    = "tunnelsUp"
	DetailTunnelsTotal = "tunnelsTotal"
	// DetailTunnels is each tunnel as "address=UP" or "address=DOWN: why",
	// separated by "; ".
	DetailTunnels = "tunnels"
	// DetailRemoteAddress is the far end's public address.
	DetailRemoteAddress = "remoteAddress"
	// DetailRemoteNetworks are the networks reached through it, comma
	// separated.
	DetailRemoteNetworks = "remoteNetworks"
	// DetailGateway is the cloud-side gateway's id.
	DetailGateway = "gateway"
	// DetailVPNType is the provider's own word: ipsec.1, IPsec, Vnet2Vnet,
	// HA VPN, Classic VPN.
	DetailVPNType = "vpnType"
	// DetailRouting is static or bgp.
	DetailRouting = "routing"
)

// VPN states, neutral. A provider's transitional states (pending,
// deleting) are passed through as they are.
const (
	VPNUp      = "up"
	VPNPartial = "partial"
	VPNDown    = "down"
)

// Tunnel is one tunnel of a connection, for VPNState.
type Tunnel struct {
	Address string
	Up      bool
	Why     string
}

// SetTunnels records a connection's tunnels on r and judges its State: up
// when every tunnel is, partial when some are, down when none is.
func SetTunnels(r *Resource, ts []Tunnel) {
	if r.Details == nil {
		r.Details = map[string]string{}
	}
	up := 0
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		s := t.Address + "="
		if t.Up {
			up++
			s += "UP"
		} else {
			s += "DOWN"
			if t.Why != "" {
				s += ": " + strings.ReplaceAll(t.Why, ";", ",")
			}
		}
		parts = append(parts, s)
	}
	r.Details[DetailTunnelsUp] = strconv.Itoa(up)
	r.Details[DetailTunnelsTotal] = strconv.Itoa(len(ts))
	r.Details[DetailTunnels] = strings.Join(parts, "; ")
	switch {
	case len(ts) == 0:
	case up == len(ts):
		r.State = VPNUp
	case up > 0:
		r.State = VPNPartial
	default:
		r.State = VPNDown
	}
}

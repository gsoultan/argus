package cloud

import (
	"context"
	"errors"
	"net/netip"
	"regexp"
)

/*
Making a cloud VPN gateway for an IPsec link, when the cloud end does not
exist yet. AWS first: a customer gateway at the Argus server's public
address, the VPC's virtual private gateway (made and attached when the VPC
has none), and a Site-to-Site VPN connection with static routing, one range
on each side, ciphers that match the link's, and the link's pre-shared key
on both tunnels. Argus links to the first tunnel; the second stays down.

The key goes from Argus to AWS once, in the call; it is never in the
change's spec, its plan or its result.
*/

// ActionVPNCreate makes a cloud VPN connection for an IPsec link.
const ActionVPNCreate Action = "vpn-create"

// VPNLinkTag is on every VPN connection, customer gateway and virtual
// private gateway Argus makes, naming the IPsec link it serves.
const VPNLinkTag = "argus:ipsec-link"

// VPNSpec is one new cloud VPN connection.
type VPNSpec struct {
	Region string
	// Name names the connection and the gateways Argus makes for it.
	Name string
	// Network is the VPC id.
	Network string
	// CustomerAddress is the Argus server's public IPv4 address, as the
	// cloud sees it.
	CustomerAddress string
	// CloudRange is the range on the cloud's side (in the VPC), and
	// ServerRange the one behind the Argus server.
	CloudRange  string
	ServerRange string
	// Ciphers is strong or compatible, as the IPsec link's.
	Ciphers string
	// PSK is the pre-shared key for both tunnels.
	PSK string
	// Propagate adds the server's range to the VPC's route tables through
	// the virtual private gateway.
	Propagate bool
	Tags      map[string]string
}

// VPNMade is what making a connection did.
type VPNMade struct {
	Result
	// Tunnels are the connection's outside addresses, first tunnel first.
	Tunnels []string `json:"tunnels,omitempty"`
	// Gateway is the virtual private gateway, and NewGateway whether Argus
	// made it.
	Gateway    string `json:"gateway,omitempty"`
	NewGateway bool   `json:"newGateway,omitempty"`
	// Propagated is whether the VPC's route tables now learn the server's
	// range; false says it must be done once the gateway is attached.
	Propagated bool `json:"propagated,omitempty"`
}

var (
	reVPNName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,79}$`)
	reVPCID   = regexp.MustCompile(`^vpc-[0-9a-f]{8,17}$`)
	rePSK     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._]{7,63}$`)
)

// VPNCiphers are the ciphers a connection may use.
var VPNCiphers = []string{"strong", "compatible"}

// PublicIPv4 is whether s is a public unicast IPv4 address.
func PublicIPv4(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// Check says what is wrong with s, or nil. The key is checked only when
// set, so a plan (which never holds it) checks too.
func (s VPNSpec) Check() error {
	if !reVPNName.MatchString(s.Name) {
		return errors.New("give the connection a name of up to 80 letters, digits, spaces and . _ ( ) -")
	}
	if !reVPCID.MatchString(s.Network) {
		return errors.New("pick the VPC the connection reaches")
	}
	if !PublicIPv4(s.CustomerAddress) {
		return errors.New("the Argus server's address must be its public IPv4 address, as AWS sees it")
	}
	c, err1 := netip.ParsePrefix(s.CloudRange)
	v, err2 := netip.ParsePrefix(s.ServerRange)
	if err1 != nil || err2 != nil || !c.Addr().Is4() || !v.Addr().Is4() || c != c.Masked() || v != v.Masked() {
		return errors.New("give one IPv4 range on each side, such as 10.30.0.0/16")
	}
	if c.Overlaps(v) {
		return errors.New("the two sides' ranges overlap")
	}
	if s.Ciphers != "strong" && s.Ciphers != "compatible" {
		return errors.New("the ciphers are strong or compatible")
	}
	if s.PSK != "" && !rePSK.MatchString(s.PSK) {
		return errors.New("AWS takes a pre-shared key of 8 to 64 letters, digits, dots and underscores, starting with a letter")
	}
	return nil
}

// VPNs is a provider that makes cloud VPN connections. It is optional.
type VPNs interface {
	CreateVPN(ctx context.Context, a Account, actor Actor, s VPNSpec) (VPNMade, error)
}

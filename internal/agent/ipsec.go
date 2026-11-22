package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
)

/*
IPsec links from a server to a cloud VPN gateway or an office firewall, set
up from the console with strongSwan (IKEv2). They ride with the WireGuard
networks (wgnet.go): the same orders on the heartbeat, the same helper, the
same two-minute trial that undoes a change which cut the server off.

Cloud gateways and most office firewalls speak IPsec only, so this is what
links them. Each link is one swanctl connection in
/etc/swanctl/conf.d/argus-<8 hex>.conf, readable by root alone, with the
pre-shared key Argus keeps sealed for it. The key arrives on the
heartbeat's reply over the agent's TLS connection, goes into that file and
the helper's plan (0600, removed after), and is never logged or reported.

What it will not do:

  - Touch a connection it did not make: only files named argus-<8 hex>.conf.
  - Carry public ranges, or a range holding the control plane's address,
    through a tunnel: both sides' ranges are private.
  - Use weak ciphers. "strong" is AES-256-GCM with SHA-384 and ECP-384;
    "compatible" (AES-256, SHA-256, MODP-2048) is for a gateway that cannot
    do better, such as an Azure gateway on its default policy.
*/

// IPsecSpec is one link as this server should have it.
type IPsecSpec struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Conn is argus- and eight hex digits: the connection and file name.
	Conn string `json:"conn"`
	// Peer is the other end's public address: a cloud tunnel's outside IP
	// or the office firewall.
	Peer         string   `json:"peer"`
	LocalRanges  []string `json:"local_ranges"`
	RemoteRanges []string `json:"remote_ranges"`
	// Proposals is strong or compatible.
	Proposals string `json:"proposals"`
	PSK       string `json:"psk"`
}

var (
	reIPsecConn = regexp.MustCompile(`^argus-[0-9a-f]{8}$`)
	reIPsecPSK  = regexp.MustCompile(`^[A-Za-z0-9._]{16,64}$`)
)

const ipsecMaxLinks = 16

// ipsecProposals are the IKE and ESP proposals for each choice.
var ipsecProposals = map[string][2]string{
	"strong":     {"aes256gcm16-prfsha384-ecp384", "aes256gcm16-ecp384"},
	"compatible": {"aes256-sha256-modp2048", "aes256-sha256-modp2048"},
}

// validIPsecSpec refuses a link that could reach outside private ranges,
// into the agent's way to Argus, or into the file's syntax.
func validIPsecSpec(s IPsecSpec, avoid []netip.Addr) error {
	if !reUUIDish.MatchString(s.ID) {
		return errors.New("malformed link id")
	}
	if !reIPsecConn.MatchString(s.Conn) {
		return fmt.Errorf("connection %q is not one Argus makes", s.Conn)
	}
	if s.Name != "" && !reWGName.MatchString(s.Name) {
		return errors.New("malformed link name")
	}
	if ip := net.ParseIP(s.Peer); ip == nil && (!reWGHost.MatchString(s.Peer) || strings.HasPrefix(s.Peer, "-")) {
		return fmt.Errorf("peer %q is not an address", s.Peer)
	}
	if _, ok := ipsecProposals[s.Proposals]; !ok {
		return fmt.Errorf("proposals %q are not ones Argus offers", s.Proposals)
	}
	if !reIPsecPSK.MatchString(s.PSK) {
		return errors.New("the pre-shared key is malformed")
	}
	for side, ranges := range map[string][]string{"local": s.LocalRanges, "remote": s.RemoteRanges} {
		if len(ranges) == 0 || len(ranges) > 16 {
			return fmt.Errorf("a link needs between 1 and 16 %s ranges", side)
		}
		for _, r := range ranges {
			p, err := netip.ParsePrefix(r)
			if err != nil || !p.Addr().Is4() || p != p.Masked() {
				return fmt.Errorf("%s range %q is not an IPv4 range", side, r)
			}
			if !wgPrivateRange(p) {
				return fmt.Errorf("%s range %s is not private", side, r)
			}
			if side == "remote" {
				for _, c := range avoid {
					if p.Contains(c) {
						return fmt.Errorf("remote range %s holds the control plane's address %s", r, c)
					}
				}
			}
		}
	}
	return nil
}

// swanConfig is a link's swanctl file.
func swanConfig(s IPsecSpec) string {
	p := ipsecProposals[s.Proposals]
	var b strings.Builder
	fmt.Fprintf(&b, "# Made by Argus for the link %q. Changes here are replaced.\n", s.Name)
	fmt.Fprintf(&b, "connections {\n  %s {\n", s.Conn)
	b.WriteString("    version = 2\n")
	fmt.Fprintf(&b, "    remote_addrs = %s\n", s.Peer)
	fmt.Fprintf(&b, "    proposals = %s\n", p[0])
	b.WriteString("    dpd_delay = 10s\n    rekey_time = 8h\n")
	b.WriteString("    local {\n      auth = psk\n    }\n")
	fmt.Fprintf(&b, "    remote {\n      auth = psk\n      id = %s\n    }\n", s.Peer)
	fmt.Fprintf(&b, "    children {\n      %s {\n", s.Conn)
	fmt.Fprintf(&b, "        local_ts = %s\n", strings.Join(s.LocalRanges, ","))
	fmt.Fprintf(&b, "        remote_ts = %s\n", strings.Join(s.RemoteRanges, ","))
	fmt.Fprintf(&b, "        esp_proposals = %s\n", p[1])
	b.WriteString("        rekey_time = 1h\n        start_action = start\n        dpd_action = restart\n        close_action = start\n")
	b.WriteString("      }\n    }\n  }\n}\n")
	fmt.Fprintf(&b, "secrets {\n  ike-%s {\n    id = %s\n    secret = \"%s\"\n  }\n}\n", s.Conn, s.Peer, s.PSK)
	return b.String()
}

// IPsec link states, as the daemon reads them from strongSwan.
const (
	IPsecUp      = "up"
	IPsecDown    = "down"
	IPsecUnknown = "unknown"
)

// ipsecStates reads whether each link is up, by asking swanctl. A link
// strongSwan cannot be asked about is unknown.
func ipsecStates(ctx context.Context, links []IPsecSpec, list func(ctx context.Context, conn string) (string, error)) map[string]string {
	if len(links) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, l := range links {
		text, err := list(ctx, l.Conn)
		switch {
		case err != nil:
			out[l.ID] = IPsecUnknown
		case strings.Contains(text, "ESTABLISHED") && strings.Contains(text, "INSTALLED"):
			out[l.ID] = IPsecUp
		default:
			out[l.ID] = IPsecDown
		}
	}
	return out
}

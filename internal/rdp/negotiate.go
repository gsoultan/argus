package rdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Security protocols a client may request (MS-RDPBCGR 2.2.1.1.1).
const (
	// ProtocolRDP is "standard RDP security": RC4 with a key exchange that has
	// been broken for years and offers no server authentication worth the name.
	// Argus refuses it; see MinimumProtocol.
	ProtocolRDP uint32 = 0x00000000
	// ProtocolSSL is TLS. The server is authenticated, the session is
	// encrypted, but credentials still cross to the login screen.
	ProtocolSSL uint32 = 0x00000001
	// ProtocolHybrid is CredSSP — network level authentication. The user is
	// authenticated before a desktop session is created, which is what stops an
	// unauthenticated peer from reaching the Windows logon surface at all.
	ProtocolHybrid uint32 = 0x00000002
	// ProtocolRDSTLS is used by the Remote Desktop Gateway redirection flow.
	ProtocolRDSTLS uint32 = 0x00000004
	// ProtocolHybridEx is CredSSP plus the Early User Authorization Result PDU.
	ProtocolHybridEx uint32 = 0x00000008
)

// Negotiation failure codes (MS-RDPBCGR 2.2.1.2.1).
const (
	FailSSLRequiredByServer    uint32 = 0x00000001
	FailSSLNotAllowedByServer  uint32 = 0x00000002
	FailSSLCertNotOnServer     uint32 = 0x00000003
	FailInconsistentFlags      uint32 = 0x00000004
	FailHybridRequiredByServer uint32 = 0x00000005
)

const (
	negTypeRequest  = 0x01
	negTypeResponse = 0x02
	negTypeFailure  = 0x03
	negLength       = 8
)

// ErrNoNegotiation reports a Connection Request with no RDP_NEG_REQ.
//
// A client that sends none is asking for standard RDP security implicitly.
// Treating that as an error rather than a default is deliberate: it is the one
// case where saying nothing means asking for the weakest option.
var ErrNoNegotiation = errors.New("connection request carries no negotiation structure")

// ConnectionRequestInfo is what Argus reads out of the opening PDU.
type ConnectionRequestInfo struct {
	// Cookie is the mstshash identifier, when the client sent one.
	//
	// This is the RDP equivalent of the SSH username: the one field a stock
	// client will put an operator-supplied string into and send before any
	// session exists. It is how a connection says which host and account it
	// wants without a custom client or a wrapper.
	Cookie string
	// RoutingToken is the load-balancer cookie, mutually exclusive with Cookie
	// in the protocol. Recorded because its presence means something in front
	// of Argus is making routing decisions of its own.
	RoutingToken string
	// RequestedProtocols is the bitmask the client offered.
	RequestedProtocols uint32
	// HasNegotiation is false for a client that sent no RDP_NEG_REQ at all.
	HasNegotiation bool
}

// Supports reports whether the client offered a protocol.
func (i ConnectionRequestInfo) Supports(protocol uint32) bool {
	if protocol == ProtocolRDP {
		// Standard RDP security is the absence of every other bit, not a bit of
		// its own. Testing it as a mask would report true for every client.
		return i.RequestedProtocols == 0
	}
	return i.RequestedProtocols&protocol != 0
}

// ParseConnectionRequest reads the client's opening PDU.
func ParseConnectionRequest(frame []byte) (ConnectionRequestInfo, error) {
	var info ConnectionRequestInfo

	variable, ok := x224Variable(frame)
	if !ok {
		return info, errors.New("malformed X.224 connection request")
	}

	// The cookie and routing token are ANSI text terminated by CRLF, and both
	// precede the negotiation structure.
	if idx := bytes.Index(variable, []byte("\r\n")); idx >= 0 {
		line := string(variable[:idx])
		switch {
		case strings.HasPrefix(line, "Cookie: mstshash="):
			info.Cookie = strings.TrimPrefix(line, "Cookie: mstshash=")
		case strings.HasPrefix(line, "Cookie: msts="):
			info.RoutingToken = strings.TrimPrefix(line, "Cookie: msts=")
		}
		variable = variable[idx+2:]
	}

	if len(variable) < negLength || variable[0] != negTypeRequest {
		return info, ErrNoNegotiation
	}
	// The length field is fixed at 8 by the specification. A different value
	// means the structure is not what it claims, and guessing would be reading
	// attacker-chosen bytes as a protocol selection.
	if l := binary.LittleEndian.Uint16(variable[2:4]); l != negLength {
		return info, fmt.Errorf("negotiation length is %d, want %d", l, negLength)
	}

	info.HasNegotiation = true
	info.RequestedProtocols = binary.LittleEndian.Uint32(variable[4:8])
	return info, nil
}

// MinimumProtocol is the weakest security protocol Argus will broker.
//
// TLS, not standard RDP security. Standard RDP security offers no meaningful
// server authentication, so a gateway that accepted it would be brokering
// privileged sessions it cannot prove reached the right host — the same failure
// that host-key pinning exists to prevent on the SSH side. Refusing is a real
// control, not a preference: it is the only point in the connection where the
// choice is still open.
const MinimumProtocol = ProtocolSSL

// SelectProtocol chooses the protocol to answer a client with.
//
// Prefers the strongest the client offered. CredSSP authenticates the user
// before a desktop is created, which keeps an unauthenticated peer away from
// the Windows logon surface entirely; TLS alone still lets them reach it.
func SelectProtocol(requested uint32) (uint32, bool) {
	for _, p := range []uint32{ProtocolHybridEx, ProtocolHybrid, ProtocolSSL} {
		if requested&p != 0 {
			return p, true
		}
	}
	return 0, false
}

// BuildConnectionConfirm builds the server's answer selecting a protocol.
func BuildConnectionConfirm(protocol uint32) []byte {
	return buildX224Response(negTypeResponse, protocol)
}

// BuildNegotiationFailure builds the server's refusal.
//
// A failure PDU rather than a dropped connection: the client renders the code
// as a specific message, so a user told "the server requires TLS" can act on
// it, where a closed socket sends them to a helpdesk.
func BuildNegotiationFailure(code uint32) []byte {
	return buildX224Response(negTypeFailure, code)
}

func buildX224Response(negType byte, value uint32) []byte {
	// TPKT(4) + X.224 CC fixed(7) + negotiation(8)
	const total = tpktHeader + 7 + negLength

	out := make([]byte, total)
	out[0] = tpktVersion
	out[1] = 0
	binary.BigEndian.PutUint16(out[2:4], total)

	x := out[tpktHeader:]
	x[0] = byte(len(x) - 1) // length indicator counts what follows it
	x[1] = tpduConnectionConfirm
	// DST-REF, SRC-REF and class are all zero for RDP.

	neg := x[7:]
	neg[0] = negType
	neg[1] = 0
	binary.LittleEndian.PutUint16(neg[2:4], negLength)
	binary.LittleEndian.PutUint32(neg[4:8], value)
	return out
}

// ParseConnectionConfirm reads the protocol a server selected.
func ParseConnectionConfirm(frame []byte) (protocol uint32, failure uint32, err error) {
	variable, ok := x224Variable(frame)
	if !ok {
		return 0, 0, errors.New("malformed X.224 connection confirm")
	}
	if len(variable) < negLength {
		// A server that answers without a negotiation structure has selected
		// standard RDP security.
		return ProtocolRDP, 0, nil
	}
	if l := binary.LittleEndian.Uint16(variable[2:4]); l != negLength {
		return 0, 0, fmt.Errorf("negotiation length is %d, want %d", l, negLength)
	}

	value := binary.LittleEndian.Uint32(variable[4:8])
	switch variable[0] {
	case negTypeResponse:
		return value, 0, nil
	case negTypeFailure:
		return 0, value, nil
	default:
		return 0, 0, fmt.Errorf("unexpected negotiation type 0x%02x", variable[0])
	}
}

// ProtocolName renders a protocol for logs and audit records.
func ProtocolName(p uint32) string {
	switch p {
	case ProtocolRDP:
		return "standard-rdp"
	case ProtocolSSL:
		return "tls"
	case ProtocolHybrid:
		return "credssp"
	case ProtocolRDSTLS:
		return "rdstls"
	case ProtocolHybridEx:
		return "credssp-ex"
	default:
		return fmt.Sprintf("unknown(0x%08x)", p)
	}
}

// FailureName renders a negotiation failure code.
func FailureName(c uint32) string {
	switch c {
	case FailSSLRequiredByServer:
		return "the server requires TLS"
	case FailSSLNotAllowedByServer:
		return "the server does not allow TLS"
	case FailSSLCertNotOnServer:
		return "the server has no certificate"
	case FailInconsistentFlags:
		return "inconsistent negotiation flags"
	case FailHybridRequiredByServer:
		return "the server requires network level authentication"
	default:
		return fmt.Sprintf("unknown failure 0x%08x", c)
	}
}

// Package rdp implements the Remote Desktop Protocol layers Argus needs to
// broker, inspect and record a session.
//
// Only the outer layers are decoded. RDP is an enormous protocol family, and
// Argus does not need to understand a session's graphics to be a privileged
// access gateway for it — it needs to decide whether a connection is allowed,
// force the connection onto a security protocol that is actually secure, and
// record the stream so it can be replayed and proved intact. Decoding beyond
// that is work that buys nothing and can only introduce parsing bugs on
// attacker-reachable bytes.
//
// The layering, outermost first:
//
//	TPKT   (RFC 1006)  4-byte framing over TCP
//	X.224  (ISO 8073)  connection-oriented transport, class 0
//	  └ RDP Negotiation Request/Response, which selects the security protocol
//
// Everything above X.224 — MCS, licensing, capability exchange, the graphics
// orders — is carried through untouched.
package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// TPKT framing constants (RFC 1006 section 6).
const (
	tpktVersion = 3
	tpktHeader  = 4
	// MaxPDU bounds a single TPKT unit. The length field is 16 bits, so this is
	// the protocol's own ceiling rather than a policy choice; stating it makes
	// the allocation below obviously bounded.
	MaxPDU = 0xFFFF
)

var (
	// ErrNotTPKT reports a first byte that is not a TPKT version.
	//
	// Worth distinguishing because it is what a plain HTTP request, a TLS
	// ClientHello or a port scanner looks like when it reaches the RDP
	// listener, and none of those should be logged as a protocol error.
	ErrNotTPKT = errors.New("not a TPKT frame")
	// ErrShortPDU reports a length field smaller than the header it describes.
	ErrShortPDU = errors.New("TPKT length is shorter than its own header")
)

// ReadPDU reads one complete TPKT frame, header included.
//
// Returns the whole frame rather than just the payload so a proxy can forward
// the exact bytes it received. Re-encoding a frame in order to forward it would
// mean the recording and the target could disagree about what was sent, and
// makes the gateway responsible for byte-level details it has no reason to own.
func ReadPDU(r io.Reader) ([]byte, error) {
	var head [tpktHeader]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	if head[0] != tpktVersion {
		return nil, fmt.Errorf("%w: first byte is 0x%02x", ErrNotTPKT, head[0])
	}

	length := int(binary.BigEndian.Uint16(head[2:4]))
	if length < tpktHeader {
		return nil, fmt.Errorf("%w: %d", ErrShortPDU, length)
	}

	// The length is 16 bits and includes the header, so this cannot exceed
	// MaxPDU no matter what the peer sends.
	frame := make([]byte, length)
	copy(frame, head[:])
	if _, err := io.ReadFull(r, frame[tpktHeader:]); err != nil {
		return nil, err
	}
	return frame, nil
}

// Payload returns the X.224 portion of a TPKT frame.
func Payload(frame []byte) ([]byte, error) {
	if len(frame) < tpktHeader {
		return nil, ErrShortPDU
	}
	return frame[tpktHeader:], nil
}

// X.224 TPDU codes (ISO 8073 section 13).
const (
	tpduConnectionRequest = 0xE0
	tpduConnectionConfirm = 0xD0
	tpduData              = 0xF0
)

// X224Kind names the transport PDU carried by a frame.
type X224Kind string

const (
	// ConnectionRequest is the client's opening PDU, carrying the negotiation
	// request and, usually, a routing cookie.
	ConnectionRequest X224Kind = "connection-request"
	// ConnectionConfirm is the server's answer, selecting a security protocol.
	ConnectionConfirm X224Kind = "connection-confirm"
	// Data is everything afterwards.
	Data X224Kind = "data"
	// Unknown is any other TPDU code.
	Unknown X224Kind = "unknown"
)

// Classify reports what kind of X.224 PDU a TPKT frame carries.
//
// Tolerant by design: a frame too short to classify is Unknown rather than an
// error, because the caller's next move is the same either way — forward it
// untouched. Only the connection sequence is inspected, and only for as long as
// the connection sequence lasts.
func Classify(frame []byte) X224Kind {
	payload, err := Payload(frame)
	if err != nil || len(payload) < 2 {
		return Unknown
	}
	// payload[0] is the length indicator; payload[1] is the TPDU code. The low
	// nibble of the code carries a credit field on some types, so compare the
	// high nibble only.
	switch payload[1] & 0xF0 {
	case tpduConnectionRequest:
		return ConnectionRequest
	case tpduConnectionConfirm:
		return ConnectionConfirm
	case tpduData:
		return Data
	default:
		return Unknown
	}
}

// x224Variable returns the bytes of a CR or CC PDU after the fixed header.
//
// The fixed part is: length indicator, code, DST-REF, SRC-REF, class. Anything
// after it is the variable part, which for RDP holds the cookie and the
// negotiation structure.
func x224Variable(frame []byte) ([]byte, bool) {
	payload, err := Payload(frame)
	if err != nil {
		return nil, false
	}
	const fixed = 7 // li(1) + code(1) + dstref(2) + srcref(2) + class(1)
	if len(payload) < fixed {
		return nil, false
	}
	// The length indicator counts the header bytes that follow it, so the PDU
	// occupies li+1 bytes. Trusting it without the bound check would let a
	// crafted value read past the frame.
	end := int(payload[0]) + 1
	if end > len(payload) || end < fixed {
		return nil, false
	}
	return payload[fixed:end], true
}

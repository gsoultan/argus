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
	// actionMask selects the two bits that say which framing a PDU uses, and
	// actionX224 is the value meaning TPKT/X.224. Fast-Path is anything else.
	actionMask = 0x03
	actionX224 = 0x03

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

// ReadPDU reads one complete PDU, header included.
//
// RDP carries two different framings on the same connection, and a reader that
// knows only the first works right up until the session actually starts. The
// connection sequence — negotiation, MCS, licensing, capability exchange — is
// TPKT/X.224. Once capabilities have been exchanged both sides switch to
// Fast-Path for input and output, which has its own compact header and no TPKT
// at all. A gateway that rejects Fast-Path completes every handshake perfectly
// and then drops the session the instant the desktop appears.
//
// The two are told apart by the low two bits of the first byte: 3 means X.224,
// which is also why TPKT's version byte is 3. Anything else is Fast-Path.
//
// Returns the whole frame rather than just the payload so a proxy can forward
// the exact bytes it received. Re-encoding in order to forward would make the
// gateway responsible for byte-level details it has no reason to own, and would
// let the recording and the target disagree about what was sent.
func ReadPDU(r io.Reader) ([]byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}

	if head[0]&actionMask == actionX224 {
		return readTPKT(r, head)
	}
	return readFastPath(r, head)
}

func readTPKT(r io.Reader, head [2]byte) ([]byte, error) {
	if head[0] != tpktVersion {
		// The low bits said X.224 but the version byte is not 3, so this is
		// neither framing.
		return nil, fmt.Errorf("%w: first byte is 0x%02x", ErrNotTPKT, head[0])
	}
	var lenBytes [2]byte
	if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(lenBytes[:]))
	if length < tpktHeader {
		return nil, fmt.Errorf("%w: %d", ErrShortPDU, length)
	}

	// The length is 16 bits and includes the header, so this cannot exceed
	// MaxPDU no matter what the peer sends.
	frame := make([]byte, length)
	frame[0], frame[1] = head[0], head[1]
	frame[2], frame[3] = lenBytes[0], lenBytes[1]
	if _, err := io.ReadFull(r, frame[tpktHeader:]); err != nil {
		return nil, err
	}
	return frame, nil
}

// readFastPath reads a Fast-Path input or output PDU.
//
// The length is one or two bytes and covers the header as well as the body: if
// the top bit of the first length byte is clear the remaining seven bits are
// the whole size, otherwise fifteen bits spread over both bytes are.
func readFastPath(r io.Reader, head [2]byte) ([]byte, error) {
	var length, headerLen int
	if head[1]&0x80 == 0 {
		length, headerLen = int(head[1]), 2
	} else {
		var ext [1]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, err
		}
		length = int(head[1]&0x7F)<<8 | int(ext[0])
		headerLen = 3

		if length < headerLen {
			return nil, fmt.Errorf("%w: fast-path length %d", ErrShortPDU, length)
		}
		frame := make([]byte, length)
		frame[0], frame[1], frame[2] = head[0], head[1], ext[0]
		if _, err := io.ReadFull(r, frame[headerLen:]); err != nil {
			return nil, err
		}
		return frame, nil
	}

	if length < headerLen {
		return nil, fmt.Errorf("%w: fast-path length %d", ErrShortPDU, length)
	}
	frame := make([]byte, length)
	frame[0], frame[1] = head[0], head[1]
	if _, err := io.ReadFull(r, frame[headerLen:]); err != nil {
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
	// Data is everything afterwards that is still TPKT-framed.
	Data X224Kind = "data"
	// FastPath is an input or output PDU from after the capability exchange.
	// It has no X.224 layer at all.
	FastPath X224Kind = "fast-path"
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
	// Two bytes is the smallest a Fast-Path header can be; anything shorter is
	// not a frame of either kind.
	if len(frame) >= 2 && frame[0]&actionMask != actionX224 {
		return FastPath
	}
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

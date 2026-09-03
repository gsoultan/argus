package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The MCS layer (T.125) that RDP's connection sequence runs over.
//
// Two encodings appear here and they are not interchangeable. Connect Initial
// and Connect Response are BER, because T.125 defines them that way. Everything
// after — erect domain, attach user, channel join — is PER, which is why the
// same conceptual field is written differently a few lines apart. Mixing them
// produces a PDU a server rejects with no diagnostic beyond a closed
// connection.
//
// Only the subset Argus needs to reach a desktop is implemented. The gateway
// does not need to be a general MCS implementation to broker one session type.

// MCS PDU tags, the top six bits of the first PER byte.
const (
	mcsErectDomainRequest = 1
	mcsAttachUserRequest  = 10
	mcsAttachUserConfirm  = 11
	mcsChannelJoinRequest = 14
	mcsChannelJoinConfirm = 15
	mcsSendDataRequest    = 25
	mcsSendDataIndication = 26
	mcsDisconnectProvider = 8
)

// Well-known channel identifiers.
const (
	// ChannelGlobal carries the graphics and input for the primary display.
	ChannelGlobal uint16 = 1003
	// userChannelBase is added to the user id the server assigns.
	userChannelBase uint16 = 1001
)

// ErrMCS reports a malformed or refused MCS exchange.
var ErrMCS = errors.New("MCS error")

/* ── BER ─────────────────────────────────────────────────────────────────── */

// berLength encodes a definite length.
//
// The short form covers everything under 128, which is most fields; the long
// form is needed for the user data block, which carries the whole GCC request.
func berLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	if n < 0x100 {
		return []byte{0x81, byte(n)}
	}
	return []byte{0x82, byte(n >> 8), byte(n)}
}

// berTag wraps a payload in a tag and length.
func berTag(tag []byte, payload []byte) []byte {
	out := make([]byte, 0, len(tag)+3+len(payload))
	out = append(out, tag...)
	out = append(out, berLength(len(payload))...)
	return append(out, payload...)
}

func berInteger(v int) []byte {
	switch {
	case v < 0x80:
		return berTag([]byte{0x02}, []byte{byte(v)})
	case v < 0x8000:
		return berTag([]byte{0x02}, []byte{byte(v >> 8), byte(v)})
	default:
		return berTag([]byte{0x02}, []byte{0x00, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
}

func berBoolean(v bool) []byte {
	b := byte(0x00)
	if v {
		b = 0xFF
	}
	return berTag([]byte{0x01}, []byte{b})
}

func berOctetString(v []byte) []byte { return berTag([]byte{0x04}, v) }

// domainParameters is the T.125 negotiation block.
//
// The three sets — target, minimum, maximum — are sent with the values every
// RDP implementation uses. They are not really negotiated in practice; a server
// that disagreed would be one no Windows client could reach either.
func domainParameters(maxChannels, maxUsers, maxTokens, maxPDU int) []byte {
	var p []byte
	for _, v := range []int{maxChannels, maxUsers, maxTokens, 1, 0, 1} {
		p = append(p, berInteger(v)...)
	}
	p = append(p, berInteger(maxPDU)...)
	p = append(p, berInteger(2)...)
	return berTag([]byte{0x30}, p)
}

/* ── Connect Initial ─────────────────────────────────────────────────────── */

// ConnectInitial builds the MCS Connect Initial carrying the GCC request.
func ConnectInitial(gccUserData []byte) []byte {
	var body []byte
	body = append(body, berOctetString(nil)...)       // callingDomainSelector
	body = append(body, berOctetString([]byte{1})...) // calledDomainSelector
	body = append(body, berBoolean(true)...)          // upwardFlag

	body = append(body, domainParameters(34, 2, 0, 0xFFFF)...)
	body = append(body, domainParameters(1, 1, 1, 0x420)...)
	body = append(body, domainParameters(0xFFFF, 0xFC17, 0xFFFF, 0xFFFF)...)
	body = append(body, berOctetString(gccUserData)...)

	// Application tag 101.
	return berTag([]byte{0x7F, 0x65}, body)
}

// ParseConnectResponse extracts the server's GCC user data.
//
// The response is BER with the same shape as the request; rather than decoding
// every field, the user data is located by its GCC signature. That is
// deliberate: the fields in between are values Argus neither uses nor is
// entitled to act on, and parsing them would be surface area for no gain.
func ParseConnectResponse(frame []byte) ([]byte, error) {
	payload, err := Payload(frame)
	if err != nil {
		return nil, err
	}
	// Skip the X.224 data header.
	if len(payload) < 3 {
		return nil, fmt.Errorf("%w: connect response is %d bytes", ErrMCS, len(payload))
	}
	body := payload[3:]

	// The GCC Conference Create Response begins with this object identifier
	// prefix in every implementation.
	marker := []byte{0x00, 0x05, 0x00, 0x14, 0x7C, 0x00, 0x01}
	idx := indexOf(body, marker)
	if idx < 0 {
		return nil, fmt.Errorf("%w: no GCC response found", ErrMCS)
	}
	return body[idx+len(marker):], nil
}

func indexOf(haystack, needle []byte) int {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

/* ── PER domain PDUs ─────────────────────────────────────────────────────── */

// ErectDomainRequest tells the server this endpoint is a leaf.
func ErectDomainRequest() []byte {
	// tag in the top six bits, then two PER integers, both zero.
	return x224Data([]byte{mcsErectDomainRequest << 2, 0x01, 0x00, 0x01, 0x00})
}

// AttachUserRequest asks for a user channel.
func AttachUserRequest() []byte {
	return x224Data([]byte{mcsAttachUserRequest << 2})
}

// ParseAttachUserConfirm reads the assigned user id.
func ParseAttachUserConfirm(frame []byte) (uint16, error) {
	payload, err := Payload(frame)
	if err != nil {
		return 0, err
	}
	if len(payload) < 3 {
		return 0, fmt.Errorf("%w: attach user confirm is too short", ErrMCS)
	}
	b := payload[3:]
	if len(b) < 2 || b[0]>>2 != mcsAttachUserConfirm {
		return 0, fmt.Errorf("%w: not an attach user confirm", ErrMCS)
	}
	// The low two bits of the tag byte carry the result; anything but zero is a
	// refusal, and continuing would join channels as a user that does not exist.
	if result := (b[0] & 0x03) | (b[1] >> 4); result != 0 {
		return 0, fmt.Errorf("%w: server refused to attach a user (result %d)", ErrMCS, result)
	}
	if len(b) < 4 {
		return 0, fmt.Errorf("%w: attach user confirm carries no user id", ErrMCS)
	}
	return binary.BigEndian.Uint16(b[2:4]) + userChannelBase, nil
}

// ChannelJoinRequest asks to join one channel.
func ChannelJoinRequest(userID, channel uint16) []byte {
	b := make([]byte, 5)
	b[0] = mcsChannelJoinRequest << 2
	binary.BigEndian.PutUint16(b[1:3], userID-userChannelBase)
	binary.BigEndian.PutUint16(b[3:5], channel)
	return x224Data(b)
}

// ParseChannelJoinConfirm verifies a channel was joined.
func ParseChannelJoinConfirm(frame []byte) (uint16, error) {
	payload, err := Payload(frame)
	if err != nil {
		return 0, err
	}
	if len(payload) < 3 {
		return 0, fmt.Errorf("%w: channel join confirm is too short", ErrMCS)
	}
	b := payload[3:]
	if len(b) < 8 || b[0]>>2 != mcsChannelJoinConfirm {
		return 0, fmt.Errorf("%w: not a channel join confirm", ErrMCS)
	}
	if result := b[0] & 0x03; result != 0 {
		return 0, fmt.Errorf("%w: channel join refused (result %d)", ErrMCS, result)
	}
	return binary.BigEndian.Uint16(b[5:7]), nil
}

// SendDataRequest wraps application data for a channel.
func SendDataRequest(userID, channel uint16, data []byte) []byte {
	head := make([]byte, 0, 8+len(data))
	head = append(head, mcsSendDataRequest<<2)
	head = binary.BigEndian.AppendUint16(head, userID-userChannelBase)
	head = binary.BigEndian.AppendUint16(head, channel)
	head = append(head, 0x70) // dataPriority high, segmentation begin+end

	// PER length: values above 127 use a two-byte form with the top bit set.
	if len(data) < 0x80 {
		head = append(head, byte(len(data)))
	} else {
		head = append(head, byte(0x80|len(data)>>8), byte(len(data)))
	}
	return x224Data(append(head, data...))
}

// ParseSendDataIndication returns the channel and payload of server data.
func ParseSendDataIndication(frame []byte) (channel uint16, data []byte, err error) {
	payload, perr := Payload(frame)
	if perr != nil {
		return 0, nil, perr
	}
	if len(payload) < 3 {
		return 0, nil, fmt.Errorf("%w: send data indication is too short", ErrMCS)
	}
	b := payload[3:]
	if len(b) < 7 {
		return 0, nil, fmt.Errorf("%w: send data indication header is truncated", ErrMCS)
	}
	switch b[0] >> 2 {
	case mcsSendDataIndication:
	case mcsDisconnectProvider:
		return 0, nil, fmt.Errorf("%w: server disconnected the MCS domain", ErrMCS)
	default:
		return 0, nil, fmt.Errorf("%w: unexpected MCS tag %d", ErrMCS, b[0]>>2)
	}

	channel = binary.BigEndian.Uint16(b[3:5])
	at := 6
	length := int(b[at])
	if length&0x80 != 0 {
		if len(b) < at+2 {
			return 0, nil, fmt.Errorf("%w: truncated PER length", ErrMCS)
		}
		length = (length&0x7F)<<8 | int(b[at+1])
		at += 2
	} else {
		at++
	}
	if at+length > len(b) {
		return 0, nil, fmt.Errorf("%w: payload claims %d bytes, %d remain",
			ErrMCS, length, len(b)-at)
	}
	return channel, b[at : at+length], nil
}

// x224Data wraps a payload in an X.224 data PDU inside TPKT framing.
func x224Data(payload []byte) []byte {
	total := tpktHeader + 3 + len(payload)
	out := make([]byte, 0, total)
	out = append(out, tpktVersion, 0)
	out = binary.BigEndian.AppendUint16(out, uint16(total))
	out = append(out, 0x02, tpduData, 0x80)
	return append(out, payload...)
}

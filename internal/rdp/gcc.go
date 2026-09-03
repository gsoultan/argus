package rdp

import (
	"encoding/binary"
	"fmt"
)

// GCC client data blocks (MS-RDPBCGR 2.2.1.3).
//
// These are what the client tells the server about itself before a desktop
// exists: how big a screen it wants, what colour depths it can decode, and
// which security protocol was already agreed. The colour depth in particular is
// not cosmetic — it decides which bitmap format the server sends, and asking
// for one Argus cannot decode produces a session that connects and then
// displays nothing.
const (
	blockClientCore     uint16 = 0xC001
	blockClientSecurity uint16 = 0xC002
	blockClientNetwork  uint16 = 0xC003
)

// Colour depth constants (MS-RDPBCGR 2.2.1.3.2).
const (
	rnsColour8BPP  uint16 = 0xCA01
	rnsColour16BPP uint16 = 0xCA02
	// supportedDepths advertises 24, 16 and 15 bit. 32-bit is deliberately not
	// claimed: it requires the surface-command path rather than plain bitmap
	// updates, and a server that took it up would send graphics this decoder
	// does not read.
	supportedDepths uint16 = 0x0007
)

// ClientInfo describes the endpoint Argus presents to the target.
type ClientInfo struct {
	Width  int
	Height int
	// Depth is the preferred colour depth in bits: 16 or 24.
	Depth int
	// SelectedProtocol is what the negotiation settled on, echoed back so the
	// server can confirm the client and it agree.
	SelectedProtocol uint32
	// Name appears in the server's session list, so it says what opened the
	// session rather than leaving an operator guessing.
	Name string
}

// GCCRequest builds the Conference Create Request carrying the client blocks.
func GCCRequest(info ClientInfo) []byte {
	if info.Width == 0 {
		info.Width = 1024
	}
	if info.Height == 0 {
		info.Height = 768
	}
	if info.Depth == 0 {
		info.Depth = 24
	}
	if info.Name == "" {
		info.Name = "argus"
	}

	blocks := clientCoreData(info)
	blocks = append(blocks, clientSecurityData()...)
	blocks = append(blocks, clientNetworkData()...)

	// The T.124 Conference Create Request wrapper. These bytes are fixed for
	// RDP: the object identifier, the conference name "Duca", and the flags no
	// implementation varies.
	out := []byte{
		0x00, 0x05, 0x00, 0x14, 0x7C, 0x00, 0x01,
	}
	out = append(out, perLength(len(blocks)+14)...)
	out = append(out,
		0x00, 0x08, 0x00, 0x10, 0x00, 0x01, 0xC0, 0x00,
		'D', 'u', 'c', 'a',
	)
	out = append(out, perLength(len(blocks))...)
	return append(out, blocks...)
}

// perLength encodes a PER length determinant.
func perLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	return []byte{byte(0x80 | n>>8), byte(n)}
}

// block wraps a payload in its type and length.
func block(kind uint16, payload []byte) []byte {
	out := make([]byte, 4, 4+len(payload))
	binary.LittleEndian.PutUint16(out[0:2], kind)
	binary.LittleEndian.PutUint16(out[2:4], uint16(len(payload)+4))
	return append(out, payload...)
}

func clientCoreData(info ClientInfo) []byte {
	depth := rnsColour16BPP
	high := uint16(16)
	if info.Depth >= 24 {
		high = 24
	}

	b := make([]byte, 0, 216)
	b = binary.LittleEndian.AppendUint32(b, 0x00080004) // RDP 5.0 and above
	b = binary.LittleEndian.AppendUint16(b, uint16(info.Width))
	b = binary.LittleEndian.AppendUint16(b, uint16(info.Height))
	b = binary.LittleEndian.AppendUint16(b, depth)
	b = binary.LittleEndian.AppendUint16(b, 0xAA03) // SAS sequence
	b = binary.LittleEndian.AppendUint32(b, 0x0409) // US keyboard
	b = binary.LittleEndian.AppendUint32(b, 2600)   // client build

	// The name is a fixed 32-byte UTF-16 field, NUL padded and NUL terminated.
	name := utf16leFixed(info.Name, 32)
	b = append(b, name...)

	b = binary.LittleEndian.AppendUint32(b, 4) // keyboard type: IBM enhanced
	b = binary.LittleEndian.AppendUint32(b, 0) // subtype
	b = binary.LittleEndian.AppendUint32(b, 12)
	b = append(b, make([]byte, 64)...) // imeFileName

	b = binary.LittleEndian.AppendUint16(b, rnsColour8BPP) // postBeta2ColorDepth
	b = binary.LittleEndian.AppendUint16(b, 1)             // clientProductId
	b = binary.LittleEndian.AppendUint32(b, 0)             // serialNumber
	b = binary.LittleEndian.AppendUint16(b, high)          // highColorDepth
	b = binary.LittleEndian.AppendUint16(b, supportedDepths)
	b = binary.LittleEndian.AppendUint16(b, 0x0001) // support error info PDU
	b = append(b, make([]byte, 64)...)              // clientDigProductId
	b = append(b, 0x07, 0x00)                       // connection type: LAN, pad
	b = binary.LittleEndian.AppendUint32(b, info.SelectedProtocol)

	return block(blockClientCore, b)
}

// clientSecurityData declares no legacy encryption.
//
// Zero for both methods is correct and required when the session runs over TLS
// or CredSSP: RDP's own encryption is the standard-security path Argus refuses,
// and claiming a method here would invite the server to use it.
func clientSecurityData() []byte {
	b := make([]byte, 8)
	return block(blockClientSecurity, b)
}

// clientNetworkData requests no virtual channels.
//
// Clipboard and drive redirection ride on virtual channels. Not requesting them
// is a deliberate default: they are the two features that turn a recorded
// session into an unrecorded file transfer, and enabling one should be a policy
// decision on the asset rather than something the gateway does because it can.
func clientNetworkData() []byte {
	b := make([]byte, 4) // channelCount = 0
	return block(blockClientNetwork, b)
}

// utf16leFixed encodes a string into a fixed-width UTF-16LE field.
func utf16leFixed(s string, size int) []byte {
	out := make([]byte, size)
	enc := utf16leBytes(s)
	// Two bytes are reserved for the terminator, so a long name is truncated
	// rather than running over the field that follows it.
	if len(enc) > size-2 {
		enc = enc[:size-2]
	}
	copy(out, enc)
	return out
}

func utf16leBytes(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		if r > 0xFFFF {
			r = 0xFFFD
		}
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// ServerChannels is what the server said about the channels it created.
type ServerChannels struct {
	// GlobalChannel carries graphics and input. Always present.
	GlobalChannel uint16
	// Extra are any virtual channels, in the order they were requested.
	Extra []uint16
}

// ParseServerNetworkData finds the channel identifiers in the GCC response.
func ParseServerNetworkData(userData []byte) (ServerChannels, error) {
	out := ServerChannels{GlobalChannel: ChannelGlobal}

	// The response is a sequence of length-prefixed blocks, the same shape as
	// the request.
	for at := 0; at+4 <= len(userData); {
		kind := binary.LittleEndian.Uint16(userData[at : at+2])
		size := int(binary.LittleEndian.Uint16(userData[at+2 : at+4]))
		if size < 4 || at+size > len(userData) {
			return out, fmt.Errorf("%w: block claims %d bytes at %d of %d",
				ErrMCS, size, at, len(userData))
		}
		body := userData[at+4 : at+size]
		at += size

		// SC_NET is 0x0C03.
		if kind != 0x0C03 || len(body) < 4 {
			continue
		}
		out.GlobalChannel = binary.LittleEndian.Uint16(body[0:2])
		count := int(binary.LittleEndian.Uint16(body[2:4]))
		for i := range count {
			o := 4 + i*2
			if o+2 > len(body) {
				break
			}
			out.Extra = append(out.Extra, binary.LittleEndian.Uint16(body[o:o+2]))
		}
	}
	return out, nil
}

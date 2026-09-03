package rdp

import (
	"encoding/binary"
	"fmt"
)

// The share-control PDUs that finish the connection sequence.
//
// After MCS the two sides exchange a client description, a licensing result and
// a set of capabilities. The capability exchange is the one that decides what
// the session looks like: it is where the client says which drawing primitives
// it understands, and a server will happily send orders a client claimed and
// cannot render.

// Share control PDU types (MS-RDPBCGR 2.2.8.1.1.1.1).
const (
	pduTypeDemandActive  = 0x1
	pduTypeConfirmActive = 0x3
	pduTypeDeactivateAll = 0x6
	pduTypeData          = 0x7
	// pduVersion occupies the high twelve bits of the type field.
	pduVersion = 0x10
)

// Security header flags (MS-RDPBCGR 2.2.8.1.1.2.1).
const secInfoPkt = 0x0040

// Client info flags (MS-RDPBCGR 2.2.1.11.1.1).
const (
	infoMouse             = 0x00000001
	infoDisableCtrlAltDel = 0x00000002
	infoAutoLogon         = 0x00000008
	infoUnicode           = 0x00000010
	infoMaximizeShell     = 0x00000020
	infoLogonNotify       = 0x00000040
	infoEnableWindowsKey  = 0x00000100
)

// ErrPDU reports a malformed or unexpected share-control PDU.
var errPDUPrefix = "share PDU"

// LogonInfo is what the client tells the target about the session to create.
type LogonInfo struct {
	Domain   string
	User     string
	Password string
	// ClientAddress appears in the target's own session list, so a Windows
	// administrator looking at who is logged on sees where from.
	ClientAddress string
}

// ClientInfoPDU builds the auto-logon packet.
//
// The password travels here when the session is TLS-only, because CredSSP —
// which would have authenticated before a desktop existed — is not available.
// It is inside the TLS channel and inside a session whose certificate has
// already been pinned, but it is still a password crossing to the host, which
// is why network level authentication is preferred wherever the target offers
// it.
func ClientInfoPDU(info LogonInfo) []byte {
	domain := utf16leZ(info.Domain)
	user := utf16leZ(info.User)
	password := utf16leZ(info.Password)
	shell := utf16leZ("")
	workDir := utf16leZ("")

	flags := uint32(infoMouse | infoUnicode | infoMaximizeShell |
		infoLogonNotify | infoEnableWindowsKey | infoDisableCtrlAltDel)
	if info.User != "" {
		// Without this the credentials are carried but ignored, and the user
		// is shown a logon screen with the fields already filled in — which
		// looks like the injection worked right up until they are asked to
		// press enter.
		flags |= infoAutoLogon
	}

	b := make([]byte, 0, 256)
	b = binary.LittleEndian.AppendUint32(b, 0) // code page
	b = binary.LittleEndian.AppendUint32(b, flags)

	// The counts exclude the terminator that follows each field, which is the
	// single most common way to build this packet wrong.
	b = binary.LittleEndian.AppendUint16(b, uint16(len(domain)-2))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(user)-2))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(password)-2))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(shell)-2))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(workDir)-2))

	b = append(b, domain...)
	b = append(b, user...)
	b = append(b, password...)
	b = append(b, shell...)
	b = append(b, workDir...)

	// Extended info, required from RDP 5.
	addr := utf16leZ(info.ClientAddress)
	b = binary.LittleEndian.AppendUint16(b, 2) // AF_INET
	b = binary.LittleEndian.AppendUint16(b, uint16(len(addr)))
	b = append(b, addr...)

	dir := utf16leZ("")
	b = binary.LittleEndian.AppendUint16(b, uint16(len(dir)))
	b = append(b, dir...)

	b = append(b, make([]byte, 172)...)        // time zone
	b = binary.LittleEndian.AppendUint32(b, 0) // session id
	b = binary.LittleEndian.AppendUint32(b, 0) // performance flags
	b = binary.LittleEndian.AppendUint16(b, 0) // auto-reconnect cookie length

	// Security header. The session is already encrypted by TLS, so no RDP
	// encryption is applied; the header still says what kind of packet follows.
	out := make([]byte, 4, 4+len(b))
	binary.LittleEndian.PutUint16(out[0:2], secInfoPkt)
	return append(out, b...)
}

// utf16leZ encodes a NUL-terminated UTF-16LE string.
func utf16leZ(s string) []byte {
	return append(utf16leBytes(s), 0, 0)
}

/* ── Share control ───────────────────────────────────────────────────────── */

// ShareControl is the header on every capability and data PDU.
type ShareControl struct {
	Type   uint16
	Source uint16
	Body   []byte
}

// ParseShareControl reads a share-control PDU.
func ParseShareControl(data []byte) (ShareControl, error) {
	if len(data) < 6 {
		return ShareControl{}, fmt.Errorf("%s: %d bytes, want at least 6", errPDUPrefix, len(data))
	}
	total := int(binary.LittleEndian.Uint16(data[0:2]))
	if total < 6 || total > len(data) {
		return ShareControl{}, fmt.Errorf("%s: declares %d bytes, %d available",
			errPDUPrefix, total, len(data))
	}
	return ShareControl{
		Type:   binary.LittleEndian.Uint16(data[2:4]) & 0x0F,
		Source: binary.LittleEndian.Uint16(data[4:6]),
		Body:   data[6:total],
	}, nil
}

// DemandActive is what the server sends to start the capability exchange.
type DemandActive struct {
	ShareID uint32
}

// ParseDemandActive reads the server's demand.
//
// Only the share identifier is taken. The server's capability sets describe
// what it can do, and Argus does not adapt to them: it asks for the small
// subset it can actually render, and a server that cannot provide that is one
// this gateway should not pretend to support.
func ParseDemandActive(body []byte) (DemandActive, error) {
	if len(body) < 4 {
		return DemandActive{}, fmt.Errorf("%s: demand active is %d bytes", errPDUPrefix, len(body))
	}
	return DemandActive{ShareID: binary.LittleEndian.Uint32(body[0:4])}, nil
}

/* ── Capabilities ────────────────────────────────────────────────────────── */

// Capability set types (MS-RDPBCGR 2.2.7).
const (
	capGeneral    uint16 = 1
	capBitmap     uint16 = 2
	capOrder      uint16 = 3
	capPointer    uint16 = 8
	capShare      uint16 = 9
	capColorCache uint16 = 10
	capInput      uint16 = 13
	capBrush      uint16 = 15
	capGlyphCache uint16 = 16
	capSound      uint16 = 12
)

// ConfirmActivePDU builds the client's answer to a demand.
//
// The capability set is deliberately small. Every primitive claimed here is one
// the server may then use, and a client that advertises drawing orders it
// cannot render gets a session that connects and paints nothing — the failure
// is silent because the server is behaving correctly.
//
// Order support is advertised as none, which makes the server fall back to
// sending bitmap updates. That is a real trade: orders are more compact for
// line drawing and window furniture, and bitmaps are what this decoder reads.
// Claiming orders would be faster and wrong.
func ConfirmActivePDU(shareID uint32, userID uint16, width, height, depth int) []byte {
	caps := generalCaps()
	caps = append(caps, bitmapCaps(width, height, depth)...)
	caps = append(caps, orderCaps()...)
	caps = append(caps, pointerCaps()...)
	caps = append(caps, inputCaps()...)
	caps = append(caps, brushCaps()...)
	caps = append(caps, glyphCaps()...)
	caps = append(caps, soundCaps()...)
	caps = append(caps, colorCacheCaps()...)
	caps = append(caps, shareCaps()...)

	const source = "MSTSC"
	numCaps := 10

	body := make([]byte, 0, 32+len(caps))
	body = binary.LittleEndian.AppendUint32(body, shareID)
	body = binary.LittleEndian.AppendUint16(body, 0x03EA) // originator: server
	body = binary.LittleEndian.AppendUint16(body, uint16(len(source)+1))
	body = binary.LittleEndian.AppendUint16(body, uint16(len(caps)+4))
	body = append(body, source...)
	body = append(body, 0)
	body = binary.LittleEndian.AppendUint16(body, uint16(numCaps))
	body = binary.LittleEndian.AppendUint16(body, 0) // pad
	body = append(body, caps...)

	total := 6 + len(body)
	out := make([]byte, 6, total)
	binary.LittleEndian.PutUint16(out[0:2], uint16(total))
	binary.LittleEndian.PutUint16(out[2:4], pduTypeConfirmActive|pduVersion<<4)
	binary.LittleEndian.PutUint16(out[4:6], userID)
	return append(out, body...)
}

// capSet wraps a capability body in its type and length.
func capSet(kind uint16, body []byte) []byte {
	out := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint16(out[0:2], kind)
	binary.LittleEndian.PutUint16(out[2:4], uint16(len(body)+4))
	return append(out, body...)
}

func generalCaps() []byte {
	b := make([]byte, 20)
	binary.LittleEndian.PutUint16(b[0:2], 1) // osMajorType: Windows
	binary.LittleEndian.PutUint16(b[2:4], 3) // osMinorType: Windows NT
	binary.LittleEndian.PutUint16(b[4:6], 0x0200)
	// FASTPATH_OUTPUT_SUPPORTED must be claimed or the server sends every
	// screen update wrapped in MCS and share-control headers instead. The
	// session still works and the decoder sees nothing, because it is watching
	// the fast path — a silent failure that looks exactly like a server with
	// nothing to draw.
	//
	// NO_BITMAP_COMPRESSION_HDR drops eight bytes of header per rectangle.
	const fastPathOutput = 0x0001
	const noBitmapCompressionHdr = 0x0400
	binary.LittleEndian.PutUint16(b[10:12], fastPathOutput|noBitmapCompressionHdr)
	return capSet(capGeneral, b)
}

func bitmapCaps(width, height, depth int) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b[0:2], uint16(depth))
	binary.LittleEndian.PutUint16(b[2:4], 1) // receive1BitPerPixel
	binary.LittleEndian.PutUint16(b[4:6], 1)
	binary.LittleEndian.PutUint16(b[6:8], 1)
	binary.LittleEndian.PutUint16(b[8:10], uint16(width))
	binary.LittleEndian.PutUint16(b[10:12], uint16(height))
	// desktopResizeFlag: the server may change the size mid-session, and a
	// client that did not accept it would show a stale desktop after any
	// resolution change.
	binary.LittleEndian.PutUint16(b[14:16], 1)
	binary.LittleEndian.PutUint16(b[16:18], 1) // bitmap compression
	return capSet(capBitmap, b)
}

// orderCaps advertises no drawing orders.
//
// The negotiate flag is left clear and every order in the support array is
// zero, which is what makes the server render server-side and send the result
// as bitmaps. The alternative is implementing thirty primitives; until then,
// claiming any of them means a session that draws nothing where they are used.
func orderCaps() []byte {
	b := make([]byte, 84)
	copy(b[0:16], make([]byte, 16))             // terminalDescriptor
	binary.LittleEndian.PutUint16(b[20:22], 1)  // desktopSaveXGranularity
	binary.LittleEndian.PutUint16(b[22:24], 20) // desktopSaveYGranularity
	binary.LittleEndian.PutUint16(b[26:28], 1)  // maximumOrderLevel
	binary.LittleEndian.PutUint16(b[28:30], 0)  // numberFonts
	// ORDERFLAGS_ZEROBOUNDSDELTASSUPPORT only. NEGOTIATEORDERSUPPORT is
	// deliberately not set.
	binary.LittleEndian.PutUint16(b[30:32], 0x0008)
	// orderSupport array stays zero: nothing is claimed.
	binary.LittleEndian.PutUint32(b[64:68], 0)      // desktopSaveSize
	binary.LittleEndian.PutUint16(b[72:74], 0x06A1) // textANSICodePage
	return capSet(capOrder, b)
}

func pointerCaps() []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[0:2], 1)  // colorPointerFlag
	binary.LittleEndian.PutUint16(b[2:4], 20) // colorPointerCacheSize
	binary.LittleEndian.PutUint16(b[4:6], 20)
	return capSet(capPointer, b)
}

func inputCaps() []byte {
	b := make([]byte, 84)
	// INPUT_FLAG_SCANCODES | INPUT_FLAG_MOUSEX | INPUT_FLAG_FASTPATH_INPUT2
	binary.LittleEndian.PutUint16(b[0:2], 0x0001|0x0004|0x0020)
	binary.LittleEndian.PutUint32(b[4:8], 0x0409) // keyboard layout
	binary.LittleEndian.PutUint32(b[8:12], 4)     // keyboard type
	binary.LittleEndian.PutUint32(b[16:20], 12)   // function keys
	return capSet(capInput, b)
}

func brushCaps() []byte {
	b := make([]byte, 4)
	return capSet(capBrush, b)
}

func glyphCaps() []byte {
	b := make([]byte, 48)
	// GLYPH_SUPPORT_NONE: glyph caching is a drawing-order feature, and the
	// same reasoning as orderCaps applies.
	binary.LittleEndian.PutUint16(b[44:46], 0)
	return capSet(capGlyphCache, b)
}

func soundCaps() []byte {
	b := make([]byte, 4)
	return capSet(capSound, b)
}

func colorCacheCaps() []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b[0:2], 6)
	return capSet(capColorCache, b)
}

func shareCaps() []byte {
	b := make([]byte, 4)
	return capSet(capShare, b)
}

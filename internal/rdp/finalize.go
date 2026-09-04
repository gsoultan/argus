package rdp

import (
	"encoding/binary"
	"fmt"
)

// Connection finalization (MS-RDPBCGR 1.3.1.1).
//
// After capabilities are agreed there is a fixed handshake of four client PDUs
// and four server replies. It carries almost no information — the synchronize
// PDU's only payload is a channel id, and the font list is empty on every
// modern client — but a server will not send a single screen update until it
// has seen all four. A client that stops after Confirm Active connects
// successfully and then waits forever on a blank canvas.

// Share data PDU types (MS-RDPBCGR 2.2.8.1.1.1.2).
const (
	pduType2Update      = 2
	pduType2Control     = 20
	pduType2Synchronize = 31
	pduType2FontList    = 39
	pduType2FontMap     = 40
	pduType2RefreshRect = 33
	pduType2ErrorInfo   = 47
)

// Control PDU actions (MS-RDPBCGR 2.2.1.15.1).
const (
	controlActionRequest   = 0x0001
	controlActionGranted   = 0x0002
	controlActionDetach    = 0x0003
	controlActionCooperate = 0x0004
)

// streamLow is the priority every client uses for these PDUs.
const streamLow = 1

// shareDataPDU wraps a payload in the share data and share control headers.
func shareDataPDU(shareID uint32, userID uint16, pduType2 byte, payload []byte) []byte {
	// Share data header.
	data := make([]byte, 12, 12+len(payload))
	binary.LittleEndian.PutUint32(data[0:4], shareID)
	data[5] = streamLow
	binary.LittleEndian.PutUint16(data[6:8], uint16(len(payload)+12))
	data[8] = pduType2
	data = append(data, payload...)

	// Share control header.
	total := 6 + len(data)
	out := make([]byte, 6, total)
	binary.LittleEndian.PutUint16(out[0:2], uint16(total))
	binary.LittleEndian.PutUint16(out[2:4], pduTypeData|pduVersion<<4)
	binary.LittleEndian.PutUint16(out[4:6], userID)
	return append(out, data...)
}

// SynchronizePDU tells the server the client is ready to synchronise.
func SynchronizePDU(shareID uint32, userID uint16) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b[0:2], 1) // SYNCMSGTYPE_SYNC
	binary.LittleEndian.PutUint16(b[2:4], 1002)
	return shareDataPDU(shareID, userID, pduType2Synchronize, b)
}

// ControlPDU sends one of the control actions.
func ControlPDU(shareID uint32, userID uint16, action uint16) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[0:2], action)
	return shareDataPDU(shareID, userID, pduType2Control, b)
}

// FontListPDU is the last PDU of the sequence.
//
// Empty, as every client since RDP 5 sends it: the fields describe a font
// negotiation that no longer happens. It exists because the server treats it as
// the signal to begin sending updates, so an empty one is not a stub — it is
// the message.
func FontListPDU(shareID uint32, userID uint16) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[4:6], 0x0003) // listFlags: first and last
	binary.LittleEndian.PutUint16(b[6:8], 0x0032) // entrySize
	return shareDataPDU(shareID, userID, pduType2FontList, b)
}

// ShareData is a decoded share data PDU.
type ShareData struct {
	Type2 byte
	Body  []byte
}

// ParseShareData reads the data header inside a share control PDU.
func ParseShareData(body []byte) (ShareData, error) {
	if len(body) < 12 {
		return ShareData{}, fmt.Errorf("%s: data PDU is %d bytes", errPDUPrefix, len(body))
	}
	return ShareData{Type2: body[8], Body: body[12:]}, nil
}

// ErrorInfo renders a server error code.
//
// Sent when a session is refused or torn down for a reason the server can name.
// Reporting the code rather than "connection closed" is the difference between
// an operator who knows the account is disabled and one who checks the network.
func ErrorInfo(body []byte) string {
	if len(body) < 4 {
		return "unspecified"
	}
	switch code := binary.LittleEndian.Uint32(body[0:4]); code {
	case 0x00000001:
		return "the user was disconnected by an administrator"
	case 0x00000002:
		return "the user was logged off by an administrator"
	case 0x00000003:
		return "the session was idle for too long"
	case 0x00000004:
		return "the session exceeded its time limit"
	case 0x00000005:
		return "the server refused the license"
	case 0x00000009:
		return "the server has replaced this session"
	case 0x0000000A:
		return "no more connections are permitted"
	case 0x00000C:
		return "the server denied the connection"
	case 0x00001004:
		return "the account is not authorised for remote access"
	case 0x00001005:
		return "the account is disabled or the password has expired"
	default:
		return fmt.Sprintf("server error 0x%08x", code)
	}
}

/* ── Licensing ───────────────────────────────────────────────────────────── */

// License message types (MS-RDPELE 2.2.2).
const (
	licenseErrorAlert = 0xFF
)

// LicenseResult reports how a licensing exchange concluded.
type LicenseResult int

const (
	// LicenseComplete means the server is satisfied and the sequence continues.
	LicenseComplete LicenseResult = iota
	// LicenseContinue means another licensing message is expected.
	LicenseContinue
)

// ParseLicensing reads a licensing PDU.
//
// Against a host that is not a licensing server — which is every target Argus
// is likely to broker — this is a single error alert saying no license is
// required. Treating that alert as a failure is the classic mistake: its name
// says error and its meaning is success.
func ParseLicensing(payload []byte) (LicenseResult, error) {
	// The security header precedes it.
	if len(payload) < 4 {
		return LicenseContinue, fmt.Errorf("licensing PDU is %d bytes", len(payload))
	}
	body := payload[4:]
	if len(body) < 4 {
		return LicenseContinue, fmt.Errorf("licensing body is %d bytes", len(body))
	}

	if body[0] != licenseErrorAlert {
		// Any other message means a real licensing negotiation, which Argus
		// does not implement. Saying so is better than continuing into a
		// sequence the server will not follow.
		return LicenseContinue, fmt.Errorf(
			"target requires a licensing exchange (message type 0x%02x) that Argus does not implement",
			body[0])
	}
	if len(body) < 12 {
		return LicenseContinue, fmt.Errorf("license error alert is truncated")
	}

	// STATUS_VALID_CLIENT is the "no license needed" answer.
	const statusValidClient = 0x00000007
	if code := binary.LittleEndian.Uint32(body[4:8]); code == statusValidClient {
		return LicenseComplete, nil
	} else {
		return LicenseContinue, fmt.Errorf("the server refused the license (0x%08x)", code)
	}
}

// RefreshRectPDU asks the server to resend a region of the screen.
//
// This is what lets someone attach to a session already in progress. Screen
// updates are deltas, so a viewer joining midway would otherwise see only what
// changed after they arrived — a mostly blank canvas that fills in slowly and
// misrepresents what the operator is looking at.
//
// The alternative is keeping a framebuffer per session on the gateway, which
// costs width by height by four bytes for every live session whether anyone is
// watching or not. Asking the server to redraw costs nothing until someone
// does.
func RefreshRectPDU(shareID uint32, userID uint16, width, height int) []byte {
	b := make([]byte, 0, 12)
	b = append(b, 1, 0, 0, 0) // one rectangle, then three bytes of padding
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 0)
	// The rectangle is inclusive of its right and bottom edges, so a full
	// screen ends one pixel short of the dimensions.
	b = binary.LittleEndian.AppendUint16(b, uint16(width-1))
	b = binary.LittleEndian.AppendUint16(b, uint16(height-1))
	return shareDataPDU(shareID, userID, pduType2RefreshRect, b)
}

package rdp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The byte counts exclude the terminator that follows each field, which is the
// single most common way to build this packet wrong — and the failure is a
// server that reads the password as part of the username.
func TestClientInfoLengthsExcludeTerminators(t *testing.T) {
	pdu := ClientInfoPDU(LogonInfo{Domain: "CORP", User: "ops", Password: "hunter2"})

	b := pdu[4:] // past the security header
	cbDomain := binary.LittleEndian.Uint16(b[8:10])
	cbUser := binary.LittleEndian.Uint16(b[10:12])
	cbPassword := binary.LittleEndian.Uint16(b[12:14])

	if cbDomain != uint16(len("CORP")*2) {
		t.Errorf("cbDomain = %d, want %d", cbDomain, len("CORP")*2)
	}
	if cbUser != uint16(len("ops")*2) {
		t.Errorf("cbUserName = %d, want %d", cbUser, len("ops")*2)
	}
	if cbPassword != uint16(len("hunter2")*2) {
		t.Errorf("cbPassword = %d, want %d", cbPassword, len("hunter2")*2)
	}

	// And the fields themselves must be recoverable at the offsets those counts
	// imply, terminators included.
	at := 18
	if got := decodeUTF16LE(b[at : at+int(cbDomain)]); got != "CORP" {
		t.Errorf("domain = %q", got)
	}
	at += int(cbDomain) + 2
	if got := decodeUTF16LE(b[at : at+int(cbUser)]); got != "ops" {
		t.Errorf("user = %q", got)
	}
	at += int(cbUser) + 2
	if got := decodeUTF16LE(b[at : at+int(cbPassword)]); got != "hunter2" {
		t.Errorf("password = %q", got)
	}
}

// Without the auto-logon flag the credentials are carried and ignored: the user
// gets a logon screen with the fields filled in, which looks like injection
// worked right up until they are asked to press enter.
func TestAutoLogonIsRequestedWhenCredentialsArePresent(t *testing.T) {
	with := ClientInfoPDU(LogonInfo{User: "ops", Password: "x"})
	flags := binary.LittleEndian.Uint32(with[8:12])
	if flags&infoAutoLogon == 0 {
		t.Error("credentials were supplied but auto-logon was not requested")
	}

	without := ClientInfoPDU(LogonInfo{})
	flags = binary.LittleEndian.Uint32(without[8:12])
	if flags&infoAutoLogon != 0 {
		t.Error("auto-logon was requested with no credentials")
	}
}

func TestClientInfoDeclaresUnicode(t *testing.T) {
	pdu := ClientInfoPDU(LogonInfo{User: "ops"})
	if flags := binary.LittleEndian.Uint32(pdu[8:12]); flags&infoUnicode == 0 {
		t.Error("the packet is UTF-16 but does not say so")
	}
	if got := binary.LittleEndian.Uint16(pdu[0:2]); got != secInfoPkt {
		t.Errorf("security header flags = %#x, want SEC_INFO_PKT", got)
	}
}

/* ── Share control ───────────────────────────────────────────────────────── */

func TestShareControlRoundTrip(t *testing.T) {
	pdu := ConfirmActivePDU(0x1234ABCD, 1004, 1920, 1080, 24)

	sc, err := ParseShareControl(pdu)
	if err != nil {
		t.Fatalf("ParseShareControl: %v", err)
	}
	if sc.Type != pduTypeConfirmActive {
		t.Errorf("type = %d, want confirm active", sc.Type)
	}
	if sc.Source != 1004 {
		t.Errorf("source = %d", sc.Source)
	}
	if got := binary.LittleEndian.Uint32(sc.Body[0:4]); got != 0x1234ABCD {
		t.Errorf("share id = %#x", got)
	}
}

// The length field is read from the wire. A PDU claiming more than it carries
// must be refused rather than sliced past its own end.
func TestShareControlBoundsItsLength(t *testing.T) {
	pdu := ConfirmActivePDU(1, 1004, 800, 600, 16)
	binary.LittleEndian.PutUint16(pdu[0:2], uint16(len(pdu)+500))
	if _, err := ParseShareControl(pdu); err == nil {
		t.Error("a PDU claiming more than it carries was accepted")
	}
	if _, err := ParseShareControl([]byte{1, 2, 3}); err == nil {
		t.Error("a truncated PDU was accepted")
	}
}

func TestDemandActiveShareID(t *testing.T) {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:4], 0xDEADBEEF)
	got, err := ParseDemandActive(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.ShareID != 0xDEADBEEF {
		t.Errorf("share id = %#x", got.ShareID)
	}
	if _, err := ParseDemandActive([]byte{1, 2}); err == nil {
		t.Error("a truncated demand active was accepted")
	}
}

/* ── Capabilities ────────────────────────────────────────────────────────── */

// capability walks the confirm-active PDU's capability list.
func capability(t *testing.T, pdu []byte, want uint16) []byte {
	t.Helper()
	sc, err := ParseShareControl(pdu)
	if err != nil {
		t.Fatal(err)
	}
	b := sc.Body
	srcLen := int(binary.LittleEndian.Uint16(b[6:8]))
	at := 12 + srcLen // shareId(4) originator(2) srcLen(2) capsLen(2) source numCaps(2) pad(2)
	at = 12 + srcLen + 0
	// numberCapabilities and pad follow the source descriptor.
	count := int(binary.LittleEndian.Uint16(b[10+srcLen : 12+srcLen]))
	at = 14 + srcLen

	for range count {
		if at+4 > len(b) {
			break
		}
		kind := binary.LittleEndian.Uint16(b[at : at+2])
		size := int(binary.LittleEndian.Uint16(b[at+2 : at+4]))
		if size < 4 || at+size > len(b) {
			t.Fatalf("capability at %d claims %d bytes", at, size)
		}
		if kind == want {
			return b[at+4 : at+size]
		}
		at += size
	}
	t.Fatalf("no capability set of type %d", want)
	return nil
}

// Every primitive claimed here is one the server may then use. A client that
// advertises drawing orders it cannot render gets a session that connects and
// paints nothing, and the failure is silent because the server is behaving
// correctly.
func TestOrderCapabilitiesClaimNothing(t *testing.T) {
	pdu := ConfirmActivePDU(1, 1004, 1024, 768, 24)
	caps := capability(t, pdu, capOrder)

	const negotiateOrderSupport = 0x0002
	flags := binary.LittleEndian.Uint16(caps[30:32])
	if flags&negotiateOrderSupport != 0 {
		t.Error("order negotiation was requested but no orders can be rendered")
	}
	// The support array must be entirely clear.
	if !bytes.Equal(caps[32:64], make([]byte, 32)) {
		t.Errorf("orderSupport claims primitives: %x", caps[32:64])
	}
}

func TestBitmapCapabilitiesMatchTheSession(t *testing.T) {
	pdu := ConfirmActivePDU(1, 1004, 1920, 1080, 24)
	caps := capability(t, pdu, capBitmap)

	if got := binary.LittleEndian.Uint16(caps[0:2]); got != 24 {
		t.Errorf("colour depth = %d", got)
	}
	if got := binary.LittleEndian.Uint16(caps[8:10]); got != 1920 {
		t.Errorf("width = %d", got)
	}
	if got := binary.LittleEndian.Uint16(caps[10:12]); got != 1080 {
		t.Errorf("height = %d", got)
	}
	// A server may resize mid-session; a client that did not accept it would
	// show a stale desktop after any resolution change.
	if got := binary.LittleEndian.Uint16(caps[14:16]); got != 1 {
		t.Error("desktop resize was not accepted")
	}
	if got := binary.LittleEndian.Uint16(caps[16:18]); got != 1 {
		t.Error("bitmap compression was not accepted")
	}
}

// Glyph caching is a drawing-order feature, and the same reasoning as the order
// capabilities applies.
func TestGlyphCacheIsNotClaimed(t *testing.T) {
	pdu := ConfirmActivePDU(1, 1004, 1024, 768, 16)
	caps := capability(t, pdu, capGlyphCache)
	if got := binary.LittleEndian.Uint16(caps[44:46]); got != 0 {
		t.Errorf("glyph support = %d, want none", got)
	}
}

func TestInputCapabilitiesDeclareScancodes(t *testing.T) {
	pdu := ConfirmActivePDU(1, 1004, 1024, 768, 16)
	caps := capability(t, pdu, capInput)
	const inputFlagScancodes = 0x0001
	if binary.LittleEndian.Uint16(caps[0:2])&inputFlagScancodes == 0 {
		t.Error("scancode input was not declared")
	}
}

// decodeUTF16LE is the test's own reader, so a bug in the encoder cannot hide
// behind the same bug in a shared decoder.
func decodeUTF16LE(b []byte) string {
	out := make([]rune, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		out = append(out, rune(uint16(b[i])|uint16(b[i+1])<<8))
	}
	return string(out)
}

package rdp

import (
	"encoding/binary"
	"testing"
)

// A Fast-Path input PDU must parse as one, or the target drops it silently and
// the session looks alive but ignores the keyboard.
func decodeInput(t *testing.T, pdu []byte) (numEvents int, body []byte) {
	t.Helper()
	if len(pdu) < 2 {
		t.Fatalf("pdu is %d bytes", len(pdu))
	}
	if pdu[0]&actionMask == actionX224 {
		t.Fatal("input PDU was framed as X.224, not fast-path")
	}
	numEvents = int(pdu[0]>>2) & 0x0F

	at := 2
	length := int(pdu[1])
	if length&0x80 != 0 {
		length = (length&0x7F)<<8 | int(pdu[2])
		at = 3
	}
	if length != len(pdu) {
		t.Fatalf("declared length %d, actual %d", length, len(pdu))
	}
	return numEvents, pdu[at:]
}

// The release flag's absence means "down", which is the opposite of the
// intuitive reading and a reliable source of keys stuck on the target.
func TestKeyEventDirection(t *testing.T) {
	down, err := EncodeInput(InputEvent{Kind: "key", Scancode: 0x1E, Down: true})
	if err != nil {
		t.Fatal(err)
	}
	n, body := decodeInput(t, down)
	if n != 1 {
		t.Errorf("numEvents = %d", n)
	}
	if body[0]>>5 != inputEventScancode || body[0]&keyRelease != 0 {
		t.Error("a key-down carried the release flag")
	}
	if body[1] != 0x1E {
		t.Errorf("scancode = %#x", body[1])
	}

	up, _ := EncodeInput(InputEvent{Kind: "key", Scancode: 0x1E, Down: false})
	_, body = decodeInput(t, up)
	if body[0]>>5 != inputEventScancode || body[0]&keyRelease == 0 {
		t.Error("a key-up did not carry the release flag")
	}
}

// Extended keys share a scancode with the keypad. Without the flag, pressing
// Home sends 7 — a bug that only shows up for the people who use those keys.
func TestExtendedKeysCarryTheirFlag(t *testing.T) {
	home, ext, ok := ScancodeFor("Home")
	if !ok || !ext {
		t.Fatalf("Home: code=%#x extended=%v ok=%v", home, ext, ok)
	}
	numpad7, ext7, _ := ScancodeFor("Numpad7")
	if numpad7 != home {
		t.Fatalf("Home and Numpad7 should share scancode %#x/%#x", home, numpad7)
	}
	if ext7 {
		t.Error("Numpad7 was marked extended")
	}

	pdu, _ := EncodeInput(InputEvent{Kind: "key", Scancode: home, Extended: true, Down: true})
	_, body := decodeInput(t, pdu)
	if body[0]>>5 != inputEventScancode || body[0]&keyExtended == 0 {
		t.Error("the extended flag was lost")
	}
}

// `code` is the physical position and does not change with the layout, which is
// what a scancode means. Keying on `key` would send the wrong code to anyone
// not on a US layout.
func TestScancodesAreKeyedOnPhysicalPosition(t *testing.T) {
	a, _, ok := ScancodeFor("KeyA")
	if !ok || a != 0x1E {
		t.Errorf("KeyA = %#x, ok=%v", a, ok)
	}
	// An unmapped code must be reported, not silently sent as zero — scancode
	// zero is a real key.
	if _, _, ok := ScancodeFor("BrowserSearch"); ok {
		t.Error("an unmapped key reported success")
	}
}

func TestMouseButtons(t *testing.T) {
	cases := map[int]uint16{0: pointerButton1, 1: pointerButton3, 2: pointerButton2}
	for browser, want := range cases {
		pdu, err := EncodeInput(InputEvent{Kind: "button", Button: browser, Down: true, X: 100, Y: 200})
		if err != nil {
			t.Fatalf("button %d: %v", browser, err)
		}
		_, body := decodeInput(t, pdu)
		if body[0]>>5 != inputEventMouse {
			t.Errorf("button %d went out as event code %d, not a mouse event", browser, body[0]>>5)
		}
		flags := binary.LittleEndian.Uint16(body[1:3])
		if flags&want == 0 {
			t.Errorf("browser button %d gave flags %#x, want %#x set", browser, flags, want)
		}
		if flags&pointerDown == 0 {
			t.Errorf("button %d down did not set the down flag", browser)
		}
	}

	if _, err := EncodeInput(InputEvent{Kind: "button", Button: 9}); err == nil {
		t.Error("an unknown button was accepted")
	}
}

// Coordinates are unsigned on the wire. A pointer that left the canvas would
// otherwise wrap to the far edge of the desktop.
func TestPointerCoordinatesAreClamped(t *testing.T) {
	pdu, _ := EncodeInput(InputEvent{Kind: "mouse", X: -50, Y: 999999})
	_, body := decodeInput(t, pdu)
	if got := binary.LittleEndian.Uint16(body[3:5]); got != 0 {
		t.Errorf("negative x became %d", got)
	}
	if got := binary.LittleEndian.Uint16(body[5:7]); got != 0xFFFF {
		t.Errorf("oversized y became %d", got)
	}
}

// A trackpad reports hundreds of units per gesture. Passing that through
// scrolls the remote desktop to the end of the document.
func TestWheelIsClampedToOneNotch(t *testing.T) {
	for _, delta := range []int{5, 500, -5, -500} {
		pdu, err := EncodeInput(InputEvent{Kind: "wheel", WheelDelta: delta})
		if err != nil {
			t.Fatal(err)
		}
		_, body := decodeInput(t, pdu)
		flags := binary.LittleEndian.Uint16(body[1:3])
		if flags&pointerWheel == 0 {
			t.Errorf("delta %d did not set the wheel flag", delta)
		}
		rotation := flags & 0x01FF
		if rotation&0xFF > 0x78 {
			t.Errorf("delta %d produced rotation %#x, more than one notch", delta, rotation)
		}
		if (delta < 0) != (flags&wheelNegative != 0) {
			t.Errorf("delta %d had the wrong sign bit", delta)
		}
	}
}

// Sent when focus returns, so a modifier released while the window was in the
// background does not stay stuck down on the target.
func TestSyncEvent(t *testing.T) {
	pdu, err := EncodeInput(InputEvent{Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	_, body := decodeInput(t, pdu)
	if body[0]>>5 != inputEventSync || len(body) != 1 {
		t.Errorf("event = % x, want a lone sync header", body)
	}
}

// The browser's Caps Lock and Num Lock go with the sync, so the target's
// match them before the first key: a password is typed as the hands mean it.
func TestSyncCarriesTheLockKeys(t *testing.T) {
	pdu, err := EncodeInput(InputEvent{Kind: "sync", Locks: LockCaps | LockNum})
	if err != nil {
		t.Fatal(err)
	}
	_, body := decodeInput(t, pdu)
	if len(body) != 1 || body[0]>>5 != inputEventSync || body[0]&0x1F != LockCaps|LockNum {
		t.Errorf("event = % x, want a sync header with caps and num set", body)
	}
}

// A character typed as itself: the code unit little-endian after the header,
// and the release flag on the way up.
func TestUnicodeEvent(t *testing.T) {
	for _, down := range []bool{true, false} {
		pdu, err := EncodeInput(InputEvent{Kind: "unicode", Unicode: 0x00E9, Down: down})
		if err != nil {
			t.Fatal(err)
		}
		_, body := decodeInput(t, pdu)
		wantFlags := byte(0)
		if !down {
			wantFlags = keyRelease
		}
		if len(body) != 3 || body[0]>>5 != inputEventUnicode || body[0]&0x1F != wantFlags ||
			body[1] != 0xE9 || body[2] != 0x00 {
			t.Errorf("down=%v: event = % x, want unicode é", down, body)
		}
	}
}

// The keys of ISO, Japanese and Brazilian boards, and the extended ones a
// full-size keyboard has, all reach the target.
func TestNationalAndExtraKeysHaveScancodes(t *testing.T) {
	for code, want := range map[string]scancodeEntry{
		"IntlBackslash": {0x56, false}, "IntlRo": {0x73, false}, "IntlYen": {0x7D, false},
		"PrintScreen": {0x37, true}, "F13": {0x64, false}, "F24": {0x76, false},
		"OSLeft": {0x5B, true}, "AudioVolumeUp": {0x30, true},
	} {
		sc, ext, ok := ScancodeFor(code)
		if !ok || sc != want.code || ext != want.extended {
			t.Errorf("%s = %#x ext=%v ok=%v, want %#x ext=%v", code, sc, ext, ok, want.code, want.extended)
		}
	}
}

func TestUnknownEventKindIsRefused(t *testing.T) {
	if _, err := EncodeInput(InputEvent{Kind: "telepathy"}); err == nil {
		t.Error("an unknown event kind was accepted")
	}
}

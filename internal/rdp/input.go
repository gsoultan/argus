package rdp

import (
	"encoding/binary"
	"fmt"
)

// Fast-Path input encoding (MS-RDPBCGR 2.2.8.1.2).
//
// Input goes back to the target on the same compact framing the graphics come
// down on, which is what keeps a keystroke to a handful of bytes. The slow path
// exists and works, but wraps every event in MCS and share headers — perhaps
// forty bytes to say a key went down, on the one path where latency is what the
// user actually feels.

// Fast-Path input event codes.
const (
	inputEventScancode = 0
	inputEventMouse    = 1
	inputEventMouseX   = 2
	inputEventSync     = 3
	inputEventUnicode  = 4
)

// Scancode event flags.
const (
	// keyRelease marks a key going up. Its absence means down, which is the
	// opposite of the intuitive reading and a reliable source of stuck keys.
	keyRelease  = 0x01
	keyExtended = 0x02
)

// Pointer flags (MS-RDPBCGR 2.2.8.1.2.2.3).
const (
	pointerMove    = 0x0800
	pointerDown    = 0x8000
	pointerButton1 = 0x1000 // left
	pointerButton2 = 0x2000 // right
	pointerButton3 = 0x4000 // middle
	pointerWheel   = 0x0200
	// wheelNegative is a sign bit, not a magnitude bit: the rotation lives in
	// the low nine bits and this says which way.
	wheelNegative = 0x0100
)

// InputEvent is one keyboard or pointer action from the browser.
type InputEvent struct {
	// Kind is "key", "mouse", "button" or "wheel".
	Kind string
	// Scancode is the PC/AT set 1 code for a key event.
	Scancode uint8
	// Extended marks the keys that share a scancode with the numeric keypad —
	// the arrow cluster, right control and right alt. Without the flag they
	// arrive as their keypad twins, which is a bug that only shows up for the
	// people who use those keys most.
	Extended bool
	Down     bool
	X, Y     int
	// Button is 0 left, 1 middle, 2 right, matching the browser's numbering
	// rather than RDP's, so the mapping happens in one place.
	Button int
	// WheelDelta is positive for scrolling down, as the browser reports it.
	WheelDelta int
}

// EncodeInput builds a Fast-Path input PDU carrying one event.
//
// One event per PDU rather than batching: the events arrive from a browser one
// at a time, and holding them back to fill a PDU would trade the latency this
// path exists to protect for a saving of a few bytes.
func EncodeInput(e InputEvent) ([]byte, error) {
	var body []byte

	switch e.Kind {
	case "key":
		flags := byte(0)
		if !e.Down {
			flags |= keyRelease
		}
		if e.Extended {
			flags |= keyExtended
		}
		body = []byte{inputEventScancode | flags<<5, e.Scancode}

	case "mouse":
		body = pointerEvent(pointerMove, e.X, e.Y)

	case "button":
		flags := uint16(0)
		switch e.Button {
		case 0:
			flags = pointerButton1
		case 1:
			flags = pointerButton3
		case 2:
			flags = pointerButton2
		default:
			return nil, fmt.Errorf("unknown mouse button %d", e.Button)
		}
		if e.Down {
			flags |= pointerDown
		}
		body = pointerEvent(flags, e.X, e.Y)

	case "wheel":
		// The rotation is a signed nine-bit value. Browsers report a delta in
		// pixels or lines depending on the device, so it is clamped to one
		// notch: a trackpad otherwise sends hundreds of units per gesture and
		// the remote desktop scrolls to the end of the document.
		flags := uint16(pointerWheel)
		if e.WheelDelta < 0 {
			flags |= wheelNegative | 0x0078
		} else {
			flags |= 0x0078
		}
		body = pointerEvent(flags, e.X, e.Y)

	case "sync":
		// Sent when focus returns, so a modifier released while the window was
		// in the background does not stay stuck down on the target.
		body = []byte{inputEventSync, 0, 0, 0, 0}

	default:
		return nil, fmt.Errorf("unknown input kind %q", e.Kind)
	}

	return fastPathInputPDU(body), nil
}

func pointerEvent(flags uint16, x, y int) []byte {
	// Coordinates are unsigned 16-bit. A negative value from a pointer that
	// left the canvas would wrap to the far edge of the desktop.
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x > 0xFFFF {
		x = 0xFFFF
	}
	if y > 0xFFFF {
		y = 0xFFFF
	}

	b := make([]byte, 7)
	b[0] = inputEventMouse
	binary.LittleEndian.PutUint16(b[1:3], flags)
	binary.LittleEndian.PutUint16(b[3:5], uint16(x))
	binary.LittleEndian.PutUint16(b[5:7], uint16(y))
	return b
}

// fastPathInputPDU wraps events in the Fast-Path header.
func fastPathInputPDU(events []byte) []byte {
	// numEvents goes in bits 2-5 of the header byte; one event here.
	header := byte(1 << 2)

	total := 2 + len(events)
	if total < 0x80 {
		out := make([]byte, 2, total)
		out[0] = header
		out[1] = byte(total)
		return append(out, events...)
	}
	total = 3 + len(events)
	out := make([]byte, 3, total)
	out[0] = header
	out[1] = byte(0x80 | total>>8)
	out[2] = byte(total)
	return append(out, events...)
}

// ScancodeFor maps a browser KeyboardEvent.code to a PC/AT set 1 scancode.
//
// Deliberately keyed on `code` rather than `key`: `code` is the physical
// position and does not change with the layout, which is what a scancode means.
// Using `key` would send the wrong code to anyone not on a US layout, and
// would send nothing at all for a keypress that produces no character.
func ScancodeFor(code string) (scancode uint8, extended bool, ok bool) {
	if sc, found := scancodes[code]; found {
		return sc.code, sc.extended, true
	}
	return 0, false, false
}

type scancodeEntry struct {
	code     uint8
	extended bool
}

var scancodes = map[string]scancodeEntry{
	"Escape": {0x01, false},
	"Digit1": {0x02, false}, "Digit2": {0x03, false}, "Digit3": {0x04, false},
	"Digit4": {0x05, false}, "Digit5": {0x06, false}, "Digit6": {0x07, false},
	"Digit7": {0x08, false}, "Digit8": {0x09, false}, "Digit9": {0x0A, false},
	"Digit0": {0x0B, false}, "Minus": {0x0C, false}, "Equal": {0x0D, false},
	"Backspace": {0x0E, false}, "Tab": {0x0F, false},
	"KeyQ": {0x10, false}, "KeyW": {0x11, false}, "KeyE": {0x12, false},
	"KeyR": {0x13, false}, "KeyT": {0x14, false}, "KeyY": {0x15, false},
	"KeyU": {0x16, false}, "KeyI": {0x17, false}, "KeyO": {0x18, false},
	"KeyP": {0x19, false}, "BracketLeft": {0x1A, false}, "BracketRight": {0x1B, false},
	"Enter": {0x1C, false}, "ControlLeft": {0x1D, false},
	"KeyA": {0x1E, false}, "KeyS": {0x1F, false}, "KeyD": {0x20, false},
	"KeyF": {0x21, false}, "KeyG": {0x22, false}, "KeyH": {0x23, false},
	"KeyJ": {0x24, false}, "KeyK": {0x25, false}, "KeyL": {0x26, false},
	"Semicolon": {0x27, false}, "Quote": {0x28, false}, "Backquote": {0x29, false},
	"ShiftLeft": {0x2A, false}, "Backslash": {0x2B, false},
	"KeyZ": {0x2C, false}, "KeyX": {0x2D, false}, "KeyC": {0x2E, false},
	"KeyV": {0x2F, false}, "KeyB": {0x30, false}, "KeyN": {0x31, false},
	"KeyM": {0x32, false}, "Comma": {0x33, false}, "Period": {0x34, false},
	"Slash": {0x35, false}, "ShiftRight": {0x36, false},
	"NumpadMultiply": {0x37, false}, "AltLeft": {0x38, false}, "Space": {0x39, false},
	"CapsLock": {0x3A, false},
	"F1":       {0x3B, false}, "F2": {0x3C, false}, "F3": {0x3D, false}, "F4": {0x3E, false},
	"F5": {0x3F, false}, "F6": {0x40, false}, "F7": {0x41, false}, "F8": {0x42, false},
	"F9": {0x43, false}, "F10": {0x44, false},
	"NumLock": {0x45, false}, "ScrollLock": {0x46, false},
	"Numpad7": {0x47, false}, "Numpad8": {0x48, false}, "Numpad9": {0x49, false},
	"NumpadSubtract": {0x4A, false},
	"Numpad4":        {0x4B, false}, "Numpad5": {0x4C, false}, "Numpad6": {0x4D, false},
	"NumpadAdd": {0x4E, false},
	"Numpad1":   {0x4F, false}, "Numpad2": {0x50, false}, "Numpad3": {0x51, false},
	"Numpad0": {0x52, false}, "NumpadDecimal": {0x53, false},
	"F11": {0x57, false}, "F12": {0x58, false},

	// Extended keys share their scancode with the keypad and are told apart
	// only by the flag. Omitting it sends Home when the user pressed 7.
	"NumpadEnter": {0x1C, true}, "ControlRight": {0x1D, true},
	"NumpadDivide": {0x35, true}, "AltRight": {0x38, true},
	"Home": {0x47, true}, "ArrowUp": {0x48, true}, "PageUp": {0x49, true},
	"ArrowLeft": {0x4B, true}, "ArrowRight": {0x4D, true},
	"End": {0x4F, true}, "ArrowDown": {0x50, true}, "PageDown": {0x51, true},
	"Insert": {0x52, true}, "Delete": {0x53, true},
	"MetaLeft": {0x5B, true}, "MetaRight": {0x5C, true}, "ContextMenu": {0x5D, true},
}

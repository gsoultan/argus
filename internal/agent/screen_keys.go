package agent

/*
Keys for the consoles that are not Windows.

The browser sends a key as the PC/AT set 1 scancode of its position (see
rdp.InputEvent), which is what Windows takes. Linux and macOS name keys by
their own codes, so the helper turns a scancode into the key at the same
position: an evdev code for X11 (whose keycodes are evdev's plus 8 on every
server built in the last fifteen years), a virtual key code for macOS.

By position, not by meaning, as Windows does: the layout chosen on the
console decides what the key types, so AltGr, dead keys and national
layouts behave as they do for a person sitting there. The Windows key is
the Super key on Linux and Command on a Mac, which is where it sits on a PC
keyboard plugged into either.
*/

// evdevKey is the Linux input event code for the key at a set 1 scancode's
// position, and false for a scancode no key has.
func evdevKey(sc uint8, extended bool) (uint16, bool) {
	if extended {
		k, ok := evdevExtended[sc]
		return k, ok
	}
	switch {
	case sc >= 0x01 && sc <= 0x53:
		// The base block: evdev numbered its codes after these.
		return uint16(sc), true
	case sc >= 0x64 && sc <= 0x6E: // F13 to F23
		return 183 + uint16(sc-0x64), true
	}
	k, ok := evdevOther[sc]
	return k, ok
}

var evdevOther = map[uint8]uint16{
	0x56: 86,  // KEY_102ND: the ISO key beside left Shift
	0x57: 87,  // F11
	0x58: 88,  // F12
	0x59: 117, // KEY_KPEQUAL
	0x70: 93,  // KEY_KATAKANAHIRAGANA
	0x71: 123, // KEY_HANJA
	0x72: 122, // KEY_HANGEUL
	0x73: 89,  // KEY_RO
	0x76: 194, // F24
	0x79: 92,  // KEY_HENKAN
	0x7B: 94,  // KEY_MUHENKAN
	0x7D: 124, // KEY_YEN
	0x7E: 95,  // KEY_KPJPCOMMA
}

var evdevExtended = map[uint8]uint16{
	0x1C: 96,  // KEY_KPENTER
	0x1D: 97,  // KEY_RIGHTCTRL
	0x35: 98,  // KEY_KPSLASH
	0x37: 99,  // KEY_SYSRQ (Print Screen)
	0x38: 100, // KEY_RIGHTALT
	0x47: 102, // KEY_HOME
	0x48: 103, // KEY_UP
	0x49: 104, // KEY_PAGEUP
	0x4B: 105, // KEY_LEFT
	0x4D: 106, // KEY_RIGHT
	0x4F: 107, // KEY_END
	0x50: 108, // KEY_DOWN
	0x51: 109, // KEY_PAGEDOWN
	0x52: 110, // KEY_INSERT
	0x53: 111, // KEY_DELETE
	0x5B: 125, // KEY_LEFTMETA
	0x5C: 126, // KEY_RIGHTMETA
	0x5D: 127, // KEY_COMPOSE (the menu key)
	0x20: 113, // KEY_MUTE
	0x2E: 114, // KEY_VOLUMEDOWN
	0x30: 115, // KEY_VOLUMEUP
	0x19: 163, // KEY_NEXTSONG
	0x10: 165, // KEY_PREVIOUSSONG
	0x22: 164, // KEY_PLAYPAUSE
	0x24: 166, // KEY_STOPCD
}

// macKey is the macOS virtual key code (kVK_*) for the key at a set 1
// scancode's position, and false for one a Mac keyboard has no key for.
func macKey(sc uint8, extended bool) (uint16, bool) {
	var k uint16
	var ok bool
	if extended {
		k, ok = macExtended[sc]
	} else {
		k, ok = macBase[sc]
	}
	return k, ok
}

var macBase = map[uint8]uint16{
	0x01: 0x35,                                                 // Escape
	0x02: 0x12, 0x03: 0x13, 0x04: 0x14, 0x05: 0x15, 0x06: 0x17, // 1 to 5
	0x07: 0x16, 0x08: 0x1A, 0x09: 0x1C, 0x0A: 0x19, 0x0B: 0x1D, // 6 to 0
	0x0C: 0x1B,                                                 // -
	0x0D: 0x18,                                                 // =
	0x0E: 0x33,                                                 // Backspace (kVK_Delete)
	0x0F: 0x30,                                                 // Tab
	0x10: 0x0C, 0x11: 0x0D, 0x12: 0x0E, 0x13: 0x0F, 0x14: 0x11, // Q W E R T
	0x15: 0x10, 0x16: 0x20, 0x17: 0x22, 0x18: 0x1F, 0x19: 0x23, // Y U I O P
	0x1A: 0x21,                                                 // [
	0x1B: 0x1E,                                                 // ]
	0x1C: 0x24,                                                 // Return
	0x1D: 0x3B,                                                 // Control
	0x1E: 0x00, 0x1F: 0x01, 0x20: 0x02, 0x21: 0x03, 0x22: 0x05, // A S D F G
	0x23: 0x04, 0x24: 0x26, 0x25: 0x28, 0x26: 0x25, // H J K L
	0x27: 0x29,                                                 // ;
	0x28: 0x27,                                                 // '
	0x29: 0x32,                                                 // `
	0x2A: 0x38,                                                 // Shift
	0x2B: 0x2A,                                                 // \
	0x2C: 0x06, 0x2D: 0x07, 0x2E: 0x08, 0x2F: 0x09, 0x30: 0x0B, // Z X C V B
	0x31: 0x2D, 0x32: 0x2E, // N M
	0x33: 0x2B,                                                 // ,
	0x34: 0x2F,                                                 // .
	0x35: 0x2C,                                                 // /
	0x36: 0x3C,                                                 // Right Shift
	0x37: 0x43,                                                 // Keypad *
	0x38: 0x3A,                                                 // Option
	0x39: 0x31,                                                 // Space
	0x3A: 0x39,                                                 // Caps Lock
	0x3B: 0x7A, 0x3C: 0x78, 0x3D: 0x63, 0x3E: 0x76, 0x3F: 0x60, // F1 to F5
	0x40: 0x61, 0x41: 0x62, 0x42: 0x64, 0x43: 0x65, 0x44: 0x6D, // F6 to F10
	0x45: 0x47,                         // Num Lock is the keypad's Clear on a Mac
	0x47: 0x59, 0x48: 0x5B, 0x49: 0x5C, // Keypad 7 8 9
	0x4A: 0x4E,                         // Keypad -
	0x4B: 0x56, 0x4C: 0x57, 0x4D: 0x58, // Keypad 4 5 6
	0x4E: 0x45,                                     // Keypad +
	0x4F: 0x53, 0x50: 0x54, 0x51: 0x55, 0x52: 0x52, // Keypad 1 2 3 0
	0x53: 0x41,                                     // Keypad .
	0x56: 0x0A,                                     // the ISO key (kVK_ISO_Section)
	0x57: 0x67,                                     // F11
	0x58: 0x6F,                                     // F12
	0x59: 0x51,                                     // Keypad =
	0x64: 0x69, 0x65: 0x6B, 0x66: 0x71, 0x67: 0x6A, // F13 to F16
	0x68: 0x40, 0x69: 0x4F, 0x6A: 0x50, 0x6B: 0x5A, // F17 to F20
	0x70: 0x68, // Kana
	0x73: 0x5E, // JIS underscore (IntlRo)
	0x7B: 0x66, // Eisu (NonConvert)
	0x7D: 0x5D, // JIS yen
	0x7E: 0x5F, // JIS keypad comma
}

var macExtended = map[uint8]uint16{
	0x1C: 0x4C, // Keypad Enter
	0x1D: 0x3E, // Right Control
	0x35: 0x4B, // Keypad /
	0x38: 0x3D, // Right Option
	0x47: 0x73, // Home
	0x48: 0x7E, // Up
	0x49: 0x74, // Page Up
	0x4B: 0x7B, // Left
	0x4D: 0x7C, // Right
	0x4F: 0x77, // End
	0x50: 0x7D, // Down
	0x51: 0x79, // Page Down
	0x52: 0x72, // Insert is Help where a Mac keyboard has one
	0x53: 0x75, // Forward Delete
	0x5B: 0x37, // Command
	0x5C: 0x36, // Right Command
	0x20: 0x4A, // Mute
	0x2E: 0x49, // Volume Down
	0x30: 0x48, // Volume Up
}

package agent

import (
	"strconv"
	"testing"

	"github.com/gsoultan/argus/internal/rdp"
)

// Every key the browser can send reaches a Linux console as a key: a code
// missing from the table is a key that silently does nothing.
func TestScreenKeysEveryBrowserKeyHasAnEvdevCode(t *testing.T) {
	codes := browserKeyCodes()
	seen := map[uint16]string{}
	for _, code := range codes {
		sc, ext, ok := rdp.ScancodeFor(code)
		if !ok {
			t.Fatalf("%s has no scancode", code)
		}
		k, ok := evdevKey(sc, ext)
		if !ok {
			t.Errorf("%s (scancode %#x, extended %v) has no evdev code", code, sc, ext)
			continue
		}
		// Two positions on one evdev code would type the wrong key; the
		// Windows key's old name is the only alias.
		if other, dup := seen[k]; dup && !isAlias(code, other) {
			t.Errorf("%s and %s both map to evdev %d", code, other, k)
		}
		seen[k] = code
	}
	for code, want := range map[string]uint16{
		"Escape": 1, "KeyA": 30, "Enter": 28, "NumpadEnter": 96, "ControlRight": 97,
		"ArrowUp": 103, "Delete": 111, "MetaLeft": 125, "F12": 88, "F13": 183, "F24": 194,
		"IntlBackslash": 86, "Numpad7": 71, "Home": 102,
	} {
		sc, ext, _ := rdp.ScancodeFor(code)
		if got, _ := evdevKey(sc, ext); got != want {
			t.Errorf("%s is evdev %d, want %d", code, got, want)
		}
	}
}

func TestScreenKeysMacKeysByPosition(t *testing.T) {
	for code, want := range map[string]uint16{
		"KeyA": 0x00, "KeyQ": 0x0C, "KeyZ": 0x06, "Digit1": 0x12, "Digit0": 0x1D,
		"Enter": 0x24, "Backspace": 0x33, "Tab": 0x30, "Space": 0x31, "Escape": 0x35,
		"ControlLeft": 0x3B, "AltLeft": 0x3A, "MetaLeft": 0x37, "MetaRight": 0x36,
		"ShiftLeft": 0x38, "ArrowLeft": 0x7B, "ArrowUp": 0x7E, "Delete": 0x75,
		"F1": 0x7A, "F12": 0x6F, "NumpadEnter": 0x4C, "IntlBackslash": 0x0A,
	} {
		sc, ext, ok := rdp.ScancodeFor(code)
		if !ok {
			t.Fatalf("%s has no scancode", code)
		}
		if got, ok := macKey(sc, ext); !ok || got != want {
			t.Errorf("%s is Mac key %#x (%v), want %#x", code, got, ok, want)
		}
	}
	// Every letter, digit and punctuation key a Mac keyboard has.
	for _, code := range browserKeyCodes() {
		sc, ext, _ := rdp.ScancodeFor(code)
		if _, ok := macKey(sc, ext); !ok && !macHasNo(code) {
			t.Errorf("%s has no Mac key", code)
		}
	}
}

// macHasNo is the keys of a PC keyboard that a Mac's has no key for.
func macHasNo(code string) bool {
	switch code {
	case "PrintScreen", "ScrollLock", "ContextMenu", "MediaTrackNext", "MediaTrackPrevious",
		"MediaPlayPause", "MediaStop", "F21", "F22", "F23", "F24", "Convert", "Lang1", "Lang2":
		return true
	}
	return false
}

func isAlias(a, b string) bool {
	alias := map[string]string{"OSLeft": "MetaLeft", "OSRight": "MetaRight"}
	return alias[a] == b || alias[b] == a
}

// browserKeyCodes is every KeyboardEvent.code the agent turns into a key.
func browserKeyCodes() []string {
	var out []string
	for _, c := range []string{
		"Escape", "Backquote", "Minus", "Equal", "Backspace", "Tab", "BracketLeft", "BracketRight",
		"Enter", "ControlLeft", "Semicolon", "Quote", "ShiftLeft", "Backslash", "Comma", "Period",
		"Slash", "ShiftRight", "NumpadMultiply", "AltLeft", "Space", "CapsLock", "NumLock",
		"ScrollLock", "NumpadSubtract", "NumpadAdd", "NumpadDecimal", "NumpadEqual",
		"IntlBackslash", "IntlRo", "IntlYen", "KanaMode", "Convert", "NonConvert", "Lang1", "Lang2",
		"NumpadComma", "NumpadEnter", "ControlRight", "NumpadDivide", "AltRight", "Home", "ArrowUp",
		"PageUp", "ArrowLeft", "ArrowRight", "End", "ArrowDown", "PageDown", "Insert", "Delete",
		"MetaLeft", "MetaRight", "ContextMenu", "OSLeft", "OSRight", "PrintScreen",
		"AudioVolumeMute", "AudioVolumeDown", "AudioVolumeUp", "MediaTrackNext",
		"MediaTrackPrevious", "MediaPlayPause", "MediaStop",
	} {
		out = append(out, c)
	}
	for c := 'A'; c <= 'Z'; c++ {
		out = append(out, "Key"+string(c))
	}
	for c := '0'; c <= '9'; c++ {
		out = append(out, "Digit"+string(c), "Numpad"+string(c))
	}
	for i := 1; i <= 24; i++ {
		out = append(out, "F"+strconv.Itoa(i))
	}
	return out
}

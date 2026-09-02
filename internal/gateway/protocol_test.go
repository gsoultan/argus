package gateway

import (
	"encoding/binary"
	"testing"
)

// These parsers read bytes that arrive from the network before any policy has
// run, so the property that matters is not "parses valid input" but "cannot be
// made to panic". A panic here takes down every session on the gateway, which
// makes a malformed packet a denial of service against the whole fleet.

func str(s string) []byte {
	b := make([]byte, 4+len(s))
	binary.BigEndian.PutUint32(b, uint32(len(s)))
	copy(b[4:], s)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func TestParsePTYRequest(t *testing.T) {
	payload := append(str("xterm-256color"), append(
		append(append(u32(120), u32(40)...), append(u32(0), u32(0)...)...),
		str("")...)...)

	term, cols, rows, _, err := parsePTYRequest(payload)
	if err != nil {
		t.Fatalf("parsePTYRequest: %v", err)
	}
	if term != "xterm-256color" {
		t.Errorf("term = %q", term)
	}
	if cols != 120 || rows != 40 {
		t.Errorf("dimensions = %dx%d, want 120x40", cols, rows)
	}
}

func TestParseWindowChange(t *testing.T) {
	payload := append(append(u32(200), u32(50)...), append(u32(0), u32(0)...)...)
	cols, rows, err := parseWindowChange(payload)
	if err != nil {
		t.Fatalf("parseWindowChange: %v", err)
	}
	if cols != 200 || rows != 50 {
		t.Errorf("got %dx%d, want 200x50", cols, rows)
	}
}

// A length field claiming more bytes than exist is the classic way to turn a
// parser into an out-of-bounds read.
func TestLengthFieldCannotReadPastTheBuffer(t *testing.T) {
	oversized := append(u32(0xFFFFFFFF), []byte("short")...)

	if _, err := parseStringPayload(oversized); err == nil {
		t.Error("a length field larger than the buffer was accepted")
	}
	if _, _, _, _, err := parsePTYRequest(oversized); err == nil {
		t.Error("pty-req accepted an oversized length field")
	}
}

func TestTruncatedPayloadsAreRejectedNotFatal(t *testing.T) {
	cases := map[string][]byte{
		"empty":                  {},
		"one byte":               {0x01},
		"length only":            u32(10),
		"length beyond buffer":   append(u32(64), []byte("abc")...),
		"term but no dimensions": str("xterm"),
		"partial dimensions":     append(str("xterm"), u32(80)...),
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			// The assertion is that none of these panic; an error is the
			// correct outcome for all of them.
			if _, err := parseStringPayload(payload); err == nil && len(payload) < 4 {
				t.Error("accepted a payload too short to contain a string")
			}
			_, _, _, _, _ = parsePTYRequest(payload)
			_, _, _ = parseWindowChange(payload)
		})
	}
}

func FuzzParsePTYRequest(f *testing.F) {
	f.Add([]byte{})
	f.Add(str("xterm"))
	f.Add(append(u32(0xFFFFFFFF), []byte("x")...))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Only that it returns rather than panics.
		_, _, _, _, _ = parsePTYRequest(data)
		_, _, _ = parseWindowChange(data)
		_, _ = parseStringPayload(data)
	})
}

func TestDecodeTerminalModesStopsAtEnd(t *testing.T) {
	// opcode 53 (ECHO) = 1, then TTY_OP_END, then trailing junk that must be
	// ignored rather than parsed as another mode.
	modes := append(append([]byte{53}, u32(1)...), 0x00, 0xFF, 0xFF)
	got := decodeTerminalModes(modes)
	if got[53] != 1 {
		t.Errorf("ECHO = %d, want 1", got[53])
	}
	if len(got) != 1 {
		t.Errorf("parsed %d modes, want 1 — trailing bytes after TTY_OP_END were read", len(got))
	}
}

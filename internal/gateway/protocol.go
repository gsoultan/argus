package gateway

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
)

// SSH wire-format helpers.
//
// x/crypto/ssh exposes these payloads as raw bytes on the server side, so a
// proxy has to parse them itself to translate a request onto the outbound
// session. Every parser here is bounds-checked: these bytes arrive from the
// network before any policy has run.

var errShortPayload = errors.New("payload truncated")

// parseStringPayload reads a single RFC 4251 string, used by exec and
// subsystem requests.
func parseStringPayload(payload []byte) (string, error) {
	s, _, err := readString(payload)
	if err != nil {
		return "", err
	}
	return s, nil
}

// parsePTYRequest decodes a pty-req payload:
//
//	string  TERM
//	uint32  columns, rows, width px, height px
//	string  encoded terminal modes
func parsePTYRequest(payload []byte) (term string, cols, rows int, modes ssh.TerminalModes, err error) {
	term, rest, err := readString(payload)
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("pty-req term: %w", err)
	}
	if len(rest) < 16 {
		return "", 0, 0, nil, fmt.Errorf("pty-req dimensions: %w", errShortPayload)
	}
	cols = int(binary.BigEndian.Uint32(rest[0:4]))
	rows = int(binary.BigEndian.Uint32(rest[4:8]))
	rest = rest[16:] // skip pixel width and height

	modeBytes, _, err := readString(rest)
	if err != nil {
		// Terminal modes are optional in practice; a missing block should not
		// cost the user their PTY.
		return term, cols, rows, ssh.TerminalModes{}, nil
	}
	return term, cols, rows, decodeTerminalModes([]byte(modeBytes)), nil
}

// parseWindowChange decodes a window-change payload: four uint32s.
func parseWindowChange(payload []byte) (cols, rows int, err error) {
	if len(payload) < 8 {
		return 0, 0, fmt.Errorf("window-change: %w", errShortPayload)
	}
	return int(binary.BigEndian.Uint32(payload[0:4])),
		int(binary.BigEndian.Uint32(payload[4:8])), nil
}

// readString reads a uint32 length-prefixed string and returns the remainder.
func readString(b []byte) (string, []byte, error) {
	if len(b) < 4 {
		return "", nil, errShortPayload
	}
	n := binary.BigEndian.Uint32(b[0:4])
	// Guard against a length field that would index past the buffer — the
	// difference between a parse error and a panic that drops every session on
	// this gateway.
	if uint64(n) > uint64(len(b)-4) {
		return "", nil, errShortPayload
	}
	return string(b[4 : 4+n]), b[4+n:], nil
}

// decodeTerminalModes reads the opcode/argument pairs of an encoded mode
// string, stopping at TTY_OP_END (0).
func decodeTerminalModes(b []byte) ssh.TerminalModes {
	modes := ssh.TerminalModes{}
	for len(b) >= 5 {
		op := b[0]
		if op == 0 { // TTY_OP_END
			break
		}
		modes[op] = binary.BigEndian.Uint32(b[1:5])
		b = b[5:]
	}
	return modes
}

// asExitError unwraps an *ssh.ExitError, which carries the remote status.
func asExitError(err error, target **ssh.ExitError) bool {
	return errors.As(err, target)
}

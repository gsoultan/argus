package execlog

// Decoding of the kernel probe's ring buffer records.
//
// Deliberately not behind the linux build tag. The layout is a property of the
// C struct in bpf/exec.bpf.c, not of the running kernel, and it is the most
// fragile part of the whole tier: a silent misread would produce plausible
// commands nobody ran, which is worse than reporting nothing. Keeping it here
// means it is covered by tests on every platform rather than only where a
// suitable kernel happens to exist.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

// Layout constants mirroring struct exec_event in bpf/exec.bpf.c.
//
// Kept explicit and checked against the C at startup rather than inferred:
// silently misreading the ring buffer would produce plausible-looking commands
// that nobody ran, which is worse than reporting nothing.
const (
	sessionLen  = 37
	commLen     = 16
	filenameLen = 256
	argsBufSize = 4096

	// Byte offset of the variable-length args tail.
	argsOffset = 4 + 4 + 4 + 4 + 1 + sessionLen + commLen + filenameLen
)

// MaxSessionID is the longest session id the probe can carry.
//
// Exported so the side that mints ids can be held to it. When ids became
// canonical UUIDs the field was still sized for 32 hex characters, and Track
// refused every session -- correctly, since a truncated id attributes an
// execution to a session that does not exist, but the kernel evidence tier was
// off and nothing failed to say so.
const MaxSessionID = sessionLen - 1

// rawEvent mirrors the fixed-size head of struct exec_event.
type rawEvent struct {
	PID       uint32
	PPID      uint32
	UID       uint32
	ArgsLen   uint32
	Truncated uint8
	Session   [sessionLen]byte
	Comm      [commLen]byte
	Filename  [filenameLen]byte
}

// decodeEvent parses one ring buffer record.
func decodeEvent(b []byte) (Exec, error) {
	if len(b) < argsOffset {
		return Exec{}, fmt.Errorf("record is %d bytes, shorter than the %d-byte header",
			len(b), argsOffset)
	}

	var raw rawEvent
	if err := binary.Read(bytes.NewReader(b[:argsOffset]), binary.LittleEndian, &raw); err != nil {
		return Exec{}, err
	}

	e := Exec{
		SessionID: cstring(raw.Session[:]),
		PID:       int(raw.PID),
		PPID:      int(raw.PPID),
		UID:       raw.UID,
		Comm:      cstring(raw.Comm[:]),
		Filename:  cstring(raw.Filename[:]),
		Truncated: raw.Truncated != 0,
		At:        time.Now().UTC(),
	}

	argsLen := int(raw.ArgsLen)
	if argsLen > len(b)-argsOffset {
		argsLen = len(b) - argsOffset
	}
	if argsLen > argsBufSize {
		argsLen = argsBufSize
	}
	if argsLen > 0 {
		e.Args = splitArgs(b[argsOffset : argsOffset+argsLen])
	}
	return e, nil
}

// splitArgs turns the kernel's NUL-separated argv into strings.
//
// An empty argument in the middle of the vector is real — `cmd "" x` is a
// meaningful invocation — so only the trailing empty produced by the region's
// final NUL is dropped.
func splitArgs(b []byte) []string {
	b = bytes.TrimSuffix(b, []byte{0})
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

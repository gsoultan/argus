package execlog

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
)

// buildRecord assembles a ring buffer record the way the BPF program does, so
// the Go decoder is tested against the layout it actually has to parse rather
// than against itself.
func buildRecord(t *testing.T, raw rawEvent, args []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, raw); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != argsOffset {
		t.Fatalf("header encoded to %d bytes but argsOffset is %d; the Go layout "+
			"and the C struct have diverged", buf.Len(), argsOffset)
	}
	buf.Write(args)
	return buf.Bytes()
}

// The offset the BPF program publishes from must match what the decoder reads
// from. If these ever disagree, every field after the divergence is garbage
// that still looks like a command.
func TestHeaderLayoutMatchesTheProbe(t *testing.T) {
	var raw rawEvent
	if got := binary.Size(raw); got != argsOffset {
		t.Fatalf("rawEvent is %d bytes, argsOffset is %d", got, argsOffset)
	}
	// Restated independently of the constant's own arithmetic.
	want := 4 + 4 + 4 + 4 + 1 + 33 + 16 + 256
	if argsOffset != want {
		t.Errorf("argsOffset = %d, want %d", argsOffset, want)
	}
}

func TestDecodeFullEvent(t *testing.T) {
	args := []byte("bash\x00-c\x00echo hello\x00")
	var raw rawEvent
	raw.PID, raw.PPID, raw.UID = 4242, 4200, 1000
	raw.ArgsLen = uint32(len(args))
	copy(raw.Session[:], "659bc048338d4ed2c3dd1e97ace98932")
	copy(raw.Comm[:], "bash")
	copy(raw.Filename[:], "/usr/bin/bash")

	e, err := decodeEvent(buildRecord(t, raw, args))
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if e.SessionID != "659bc048338d4ed2c3dd1e97ace98932" {
		t.Errorf("SessionID = %q", e.SessionID)
	}
	if e.PID != 4242 || e.PPID != 4200 || e.UID != 1000 {
		t.Errorf("pid/ppid/uid = %d/%d/%d", e.PID, e.PPID, e.UID)
	}
	if e.Comm != "bash" || e.Filename != "/usr/bin/bash" {
		t.Errorf("comm/filename = %q/%q", e.Comm, e.Filename)
	}
	if !slices.Equal(e.Args, []string{"bash", "-c", "echo hello"}) {
		t.Errorf("Args = %q", e.Args)
	}
	if got := e.CommandLine(); got != "bash -c 'echo hello'" {
		t.Errorf("CommandLine = %q", got)
	}
	if e.At.IsZero() {
		t.Error("no timestamp")
	}
}

// The reason this tier exists. A PTY recording of this session shows an opaque
// base64 blob; the kernel saw the same blob as an argument to a real command,
// and the decoder must preserve it exactly.
func TestDecodePreservesObfuscatedCommands(t *testing.T) {
	payload := "cm0gLXJmIC92YXIvbG9nL2F1ZGl0Cg=="
	args := []byte("sh\x00-c\x00echo " + payload + " | base64 -d | sh\x00")

	var raw rawEvent
	raw.ArgsLen = uint32(len(args))
	copy(raw.Session[:], "sess")
	copy(raw.Filename[:], "/bin/sh")

	e, err := decodeEvent(buildRecord(t, raw, args))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.CommandLine(), payload) {
		t.Errorf("the encoded payload was lost: %q", e.CommandLine())
	}
}

func TestDecodeRejectsShortRecords(t *testing.T) {
	if _, err := decodeEvent(make([]byte, argsOffset-1)); err == nil {
		t.Error("a truncated record decoded without error")
	}
	// Exactly the header and no argv is legitimate: a process can exec with an
	// empty argument vector.
	var raw rawEvent
	copy(raw.Session[:], "sess")
	copy(raw.Filename[:], "/bin/true")
	e, err := decodeEvent(buildRecord(t, raw, nil))
	if err != nil {
		t.Fatalf("header-only record: %v", err)
	}
	if got := e.CommandLine(); got != "/bin/true" {
		t.Errorf("CommandLine = %q, want the filename", got)
	}
}

// A record claiming more argv than it carries must not read past the buffer.
func TestDecodeClampsAnOverstatedLength(t *testing.T) {
	args := []byte("ls\x00")
	var raw rawEvent
	raw.ArgsLen = 9999 // more than the record holds
	copy(raw.Session[:], "sess")

	e, err := decodeEvent(buildRecord(t, raw, args))
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if !slices.Equal(e.Args, []string{"ls"}) {
		t.Errorf("Args = %q", e.Args)
	}
}

func TestTruncationSurvivesDecoding(t *testing.T) {
	args := []byte("python3\x00-c\x00import os\x00")
	var raw rawEvent
	raw.ArgsLen = uint32(len(args))
	raw.Truncated = 1
	copy(raw.Session[:], "sess")

	e, err := decodeEvent(buildRecord(t, raw, args))
	if err != nil {
		t.Fatal(err)
	}
	if !e.Truncated {
		t.Fatal("the truncation flag was lost; a cut-off command would read as complete")
	}
	if !strings.HasSuffix(e.CommandLine(), "…[truncated]") {
		t.Errorf("CommandLine does not show truncation: %q", e.CommandLine())
	}
}

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"trailing NUL is a separator, not an argument", "ls\x00-la\x00", []string{"ls", "-la"}},
		{"no trailing NUL", "ls\x00-la", []string{"ls", "-la"}},
		{"an empty argument in the middle is real", "cmd\x00\x00x\x00", []string{"cmd", "", "x"}},
		{"single argument", "whoami\x00", []string{"whoami"}},
		{"empty region", "", nil},
		{"only a NUL", "\x00", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitArgs([]byte(tc.in)); !slices.Equal(got, tc.want) {
				t.Errorf("splitArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A C string that fills its buffer has no terminator; reading past it would
// splice the next field onto the end of this one.
func TestCstringHandlesAnUnterminatedBuffer(t *testing.T) {
	full := []byte("abcd") // exactly fills its field, so no terminator
	if got := cstring(full); got != "abcd" {
		t.Errorf("cstring on a full buffer = %q", got)
	}
	if got := cstring([]byte{'a', 'b', 0, 'x', 'y'}); got != "ab" {
		t.Errorf("cstring stopped at the wrong place: %q", got)
	}
}

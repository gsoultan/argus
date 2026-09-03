package rdp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/recorder"
)

func TestRecordingIsWellFormed(t *testing.T) {
	var buf bytes.Buffer
	rec, err := NewRecorder(&buf, Header{
		Width: 1920, Height: 1080,
		Title:    "ops@win-01",
		Protocol: ProtocolName(ProtocolHybrid),
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	pdu := []byte{0x03, 0x00, 0x00, 0x09, 0x02, 0xF0, 0x80, 0xAA, 0xBB}
	if err := rec.Write(ServerOutput, pdu); err != nil {
		t.Fatal(err)
	}
	if err := rec.Write(ClientInput, []byte{0x01, 0x02}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want header plus two frames", len(lines))
	}

	var h Header
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		t.Fatalf("header is not JSON: %v", err)
	}
	if h.Version != FormatVersion || h.Width != 1920 || h.Protocol != "credssp" {
		t.Errorf("header = %+v", h)
	}

	var frame []any
	if err := json.Unmarshal([]byte(lines[1]), &frame); err != nil {
		t.Fatalf("frame is not JSON: %v", err)
	}
	if len(frame) != 3 || frame[1] != string(ServerOutput) {
		t.Fatalf("frame = %v", frame)
	}
	// The PDU must survive the round trip byte for byte: a replay that differs
	// from what crossed the wire is not evidence of anything.
	got, err := base64.StdEncoding.DecodeString(frame[2].(string))
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if !bytes.Equal(got, pdu) {
		t.Errorf("payload round-tripped as %x, want %x", got, pdu)
	}
}

// The reason the format is shaped like asciicast rather than being anything
// RDP-specific: one verifier already checks the terminal recordings, the audit
// chain, the console's Web Worker and the backup drill. Reusing the chain means
// RDP recordings get all of it without a line of new verification code — and
// without a second implementation of the property the product is sold on.
func TestTheExistingVerifierChecksRDPRecordings(t *testing.T) {
	var buf bytes.Buffer
	rec, err := NewRecorder(&buf, Header{Title: "ops@win-01"})
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.Write(ServerOutput, []byte("graphics"))
	_ = rec.Event(map[string]any{"kind": "clipboard", "bytes": 4096})
	_ = rec.Write(ClientInput, []byte("keystrokes"))
	head, err := rec.Close()
	if err != nil {
		t.Fatal(err)
	}

	v, err := recorder.Verify(strings.NewReader(buf.String()), head)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !v.OK {
		t.Fatalf("an untouched RDP recording failed to verify at line %d", v.BrokenAt)
	}
	if v.Head != head {
		t.Errorf("recomputed head %q, recorder reported %q", v.Head, head)
	}
}

func TestTamperingWithAnRDPRecordingIsDetected(t *testing.T) {
	build := func(t *testing.T) (string, string) {
		t.Helper()
		var buf bytes.Buffer
		rec, err := NewRecorder(&buf, Header{Width: 1920, Height: 1080,
			Protocol: ProtocolName(ProtocolHybrid)})
		if err != nil {
			t.Fatal(err)
		}
		_ = rec.Write(ServerOutput, []byte("first"))
		_ = rec.Event(map[string]any{"kind": "clipboard", "direction": "out"})
		_ = rec.Write(ServerOutput, []byte("second"))
		head, err := rec.Close()
		if err != nil {
			t.Fatal(err)
		}
		return buf.String(), head
	}

	cases := []struct {
		name  string
		alter func(string) string
	}{
		{
			// Rewriting which protocol a session ran under would let someone
			// claim a CredSSP session that was really brokered over TLS.
			"the header's protocol is edited",
			func(s string) string { return strings.Replace(s, "credssp", "tls", 1) },
		},
		{
			"a clipboard event is removed",
			func(s string) string {
				lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
				return strings.Join(append(lines[:2], lines[3:]...), "\n") + "\n"
			},
		},
		{
			"a frame's payload is replaced",
			func(s string) string {
				return strings.Replace(s,
					base64.StdEncoding.EncodeToString([]byte("second")),
					base64.StdEncoding.EncodeToString([]byte("harmless")), 1)
			},
		},
		{
			"the tail is truncated",
			func(s string) string {
				lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
				return strings.Join(lines[:len(lines)-1], "\n") + "\n"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, head := build(t)
			v, err := recorder.Verify(strings.NewReader(tc.alter(body)), head)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if v.OK {
				t.Error("the edit verified; the recording is forgeable")
			}
		})
	}
}

func TestEmptyPDUsAreNotRecorded(t *testing.T) {
	var buf bytes.Buffer
	rec, err := NewRecorder(&buf, Header{})
	if err != nil {
		t.Fatal(err)
	}
	before := rec.Head()
	if err := rec.Write(ServerOutput, nil); err != nil {
		t.Fatal(err)
	}
	if rec.Head() != before {
		t.Error("an empty PDU advanced the chain")
	}
}

func TestHeaderDefaultsAreSensible(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewRecorder(&buf, Header{}); err != nil {
		t.Fatal(err)
	}
	var h Header
	line, _, _ := strings.Cut(buf.String(), "\n")
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		t.Fatal(err)
	}
	if h.Width == 0 || h.Height == 0 || h.Timestamp == 0 {
		t.Errorf("header left blanks a player cannot use: %+v", h)
	}
}

func TestStatsTrackTheRecording(t *testing.T) {
	var buf bytes.Buffer
	rec, _ := NewRecorder(&buf, Header{})
	_ = rec.Write(ServerOutput, bytes.Repeat([]byte{0xAB}, 512))

	lines, size, dur := rec.Stats()
	if lines != 2 { // header plus one frame
		t.Errorf("lines = %d, want 2", lines)
	}
	if size <= 512 {
		t.Errorf("bytes = %d, want more than the payload after base64", size)
	}
	if dur <= 0 {
		t.Error("duration did not advance")
	}
}

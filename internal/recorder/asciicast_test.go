package recorder

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWritesValidAsciicastV2(t *testing.T) {
	var buf bytes.Buffer
	r, err := New(&buf, Header{Width: 120, Height: 34, Title: "ops@pay-01"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Write(Input, []byte("id\r")); err != nil {
		t.Fatalf("Write input: %v", err)
	}
	if err := r.Write(Output, []byte("uid=0(root)\r\n")); err != nil {
		t.Fatalf("Write output: %v", err)
	}
	if err := r.Resize(100, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if _, err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4 (header + 3 events)", len(lines))
	}

	var h Header
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		t.Fatalf("header is not JSON: %v", err)
	}
	if h.Version != 2 {
		t.Errorf("version = %d, want 2", h.Version)
	}
	if h.Width != 120 || h.Height != 34 {
		t.Errorf("dimensions = %dx%d, want 120x34", h.Width, h.Height)
	}

	// Every event must be [number, string, string] or the web player's decoder
	// silently skips it.
	for i, line := range lines[1:] {
		var ev []any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("event %d is not JSON: %v", i, err)
		}
		if len(ev) != 3 {
			t.Fatalf("event %d has %d fields, want 3", i, len(ev))
		}
		if _, ok := ev[0].(float64); !ok {
			t.Errorf("event %d time is %T, want number", i, ev[0])
		}
		if _, ok := ev[1].(string); !ok {
			t.Errorf("event %d stream is %T, want string", i, ev[1])
		}
	}
}

func TestChainDetectsTampering(t *testing.T) {
	var buf bytes.Buffer
	r, err := New(&buf, Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, cmd := range []string{"whoami\r", "cat /etc/shadow\r", "exit\r"} {
		if err := r.Write(Input, []byte(cmd)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	head, err := r.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	original := buf.String()

	t.Run("intact recording verifies", func(t *testing.T) {
		v, err := Verify(strings.NewReader(original), head)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !v.OK {
			t.Errorf("intact recording failed verification")
		}
		if v.Head != head {
			t.Errorf("recomputed head %q, want %q", v.Head, head)
		}
	})

	// The scenario that matters: someone edits the recording to hide what they
	// ran. The chain must catch it.
	t.Run("edited command is detected", func(t *testing.T) {
		tampered := strings.Replace(original, "cat /etc/shadow", "cat /etc/hostname", 1)
		if tampered == original {
			t.Fatal("test bug: nothing was replaced")
		}
		v, err := Verify(strings.NewReader(tampered), head)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if v.OK {
			t.Error("tampered recording passed verification")
		}
	})

	t.Run("truncation is detected", func(t *testing.T) {
		lines := strings.Split(strings.TrimRight(original, "\n"), "\n")
		truncated := strings.Join(lines[:len(lines)-1], "\n") + "\n"
		v, err := Verify(strings.NewReader(truncated), head)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if v.OK {
			t.Error("truncated recording passed verification")
		}
	})

	t.Run("appended line is detected", func(t *testing.T) {
		appended := original + `[9.0,"o","nothing to see here"]` + "\n"
		v, err := Verify(strings.NewReader(appended), head)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if v.OK {
			t.Error("appended recording passed verification")
		}
	})
}

// A killed gateway must still leave a replayable prefix — that is the whole
// reason for a streaming format.
func TestPartialRecordingIsStillValid(t *testing.T) {
	var buf bytes.Buffer
	r, err := New(&buf, Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Write(Output, []byte("partial")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Deliberately no Close — simulating a crash mid-session.

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for i, line := range lines {
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("line %d is not valid JSON after crash: %v", i, err)
		}
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	var buf bytes.Buffer
	r, _ := New(&buf, Header{})
	if _, err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Write(Output, []byte("late")); err == nil {
		t.Error("write after close succeeded, want error")
	}
}

// The tap is what live shadowing reads. If it could ever differ from what was
// written, an operator watching a session and an auditor replaying it would be
// looking at two different sessions.
func TestTapSeesExactlyWhatIsRecorded(t *testing.T) {
	var buf bytes.Buffer
	rec, err := New(&buf, Header{})
	if err != nil {
		t.Fatal(err)
	}

	var tapped [][]byte
	rec.SetTap(func(line []byte) {
		tapped = append(tapped, append([]byte(nil), line...))
	})

	if err := rec.Write(Output, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := rec.Write(Input, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	if err := rec.Resize(120, 40); err != nil {
		t.Fatal(err)
	}

	// Everything after the header line, which the tap does not carry.
	written := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")[1:]
	if len(tapped) != len(written) {
		t.Fatalf("tap saw %d frames, recording holds %d", len(tapped), len(written))
	}
	for i := range written {
		if string(tapped[i]) != written[i] {
			t.Errorf("frame %d: tap %q, recording %q", i, tapped[i], written[i])
		}
	}
}

// The tap must never be handed a frame the recording failed to accept.
func TestTapIsSilentWhenTheWriteFails(t *testing.T) {
	rec, err := New(&failingWriter{allow: 1}, Header{})
	if err != nil {
		t.Fatal(err)
	}
	var tapped int
	rec.SetTap(func([]byte) { tapped++ })

	if err := rec.Write(Output, []byte("this write fails")); err == nil {
		t.Fatal("expected the write to fail")
	}
	if tapped != 0 {
		t.Errorf("tap fired %d times for a frame that was never recorded", tapped)
	}
}

// failingWriter accepts allow writes — enough for the header — then refuses.
type failingWriter struct {
	allow int
	n     int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n > w.allow {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

// Kernel evidence must sit inside the hash chain. Outside it, the command list
// would be exactly as forgeable as the shell history it replaces — and a chain
// covering the terminal output but not the commands guarantees the wrong half.
func TestExecFramesAreChained(t *testing.T) {
	var buf bytes.Buffer
	rec, err := New(&buf, Header{})
	if err != nil {
		t.Fatal(err)
	}

	before := rec.Head()
	if err := rec.Exec(map[string]any{"pid": 42, "cmd": "rm -rf /var/log"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if rec.Head() == before {
		t.Fatal("an exec event did not advance the chain, so it is not covered by it")
	}

	line := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")[1]
	var frame []any
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("exec frame is not valid asciicast JSON: %v", err)
	}
	if len(frame) != 3 {
		t.Fatalf("frame has %d elements, want 3", len(frame))
	}
	if frame[1] != "x" {
		t.Errorf("stream = %v, want x", frame[1])
	}
	// The payload is JSON carried as the frame's data string, so a player that
	// predates this stream renders nothing rather than breaking.
	var payload map[string]any
	if err := json.Unmarshal([]byte(frame[2].(string)), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload["cmd"] != "rm -rf /var/log" {
		t.Errorf("payload = %v", payload)
	}
}

// Tampering with a recorded command must break verification, or the evidence
// is decorative.
func TestEditingAnExecFrameBreaksTheChain(t *testing.T) {
	var buf bytes.Buffer
	rec, err := New(&buf, Header{})
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.Write(Output, []byte("$ "))
	_ = rec.Exec(map[string]any{"cmd": "rm -rf /var/log/audit"})
	_ = rec.Write(Output, []byte("done\r\n"))
	head, err := rec.Close()
	if err != nil {
		t.Fatal(err)
	}

	v, err := Verify(strings.NewReader(buf.String()), head)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !v.OK {
		t.Fatalf("an untouched recording failed to verify at line %d", v.BrokenAt)
	}

	edited := strings.Replace(buf.String(), "rm -rf /var/log/audit", "ls -la /var/log", 1)
	v, err = Verify(strings.NewReader(edited), head)
	if err != nil {
		t.Fatalf("Verify on the edited recording: %v", err)
	}
	if v.OK {
		t.Fatal("editing a recorded command still verified; the evidence is forgeable")
	}
}

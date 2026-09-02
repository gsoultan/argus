package recorder

import (
	"bytes"
	"encoding/json"
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

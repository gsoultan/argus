package logquiet

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newTest(window time.Duration) (*slog.Logger, *bytes.Buffer, *time.Time) {
	var buf bytes.Buffer
	h := New(slog.NewTextHandler(&buf, nil), window)
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	h.st.now = func() time.Time { return now }
	return slog.New(h), &buf, &now
}

func lines(buf *bytes.Buffer) []string {
	s := strings.TrimSpace(buf.String())
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// The loop that motivated this: an agent that cannot reach the control plane
// logs the same warning on every retry, with a retry_in that changes each
// time. Before, every retry was a line in the journal.
func TestRepeatedLineIsWrittenOnceAWindowWithACount(t *testing.T) {
	log, buf, now := newTest(10 * time.Minute)
	for i := 0; i < 100; i++ {
		log.Warn("tunnel: cannot reach the control plane",
			"error", errors.New("connection refused"), "retry_in", time.Duration(i)*time.Second)
		*now = now.Add(5 * time.Second)
	}
	got := lines(buf)
	if len(got) != 1 {
		t.Fatalf("wrote %d lines in one window, want 1:\n%s", len(got), buf)
	}

	*now = now.Add(10 * time.Minute)
	log.Warn("tunnel: cannot reach the control plane",
		"error", errors.New("connection refused"), "retry_in", time.Minute)
	got = lines(buf)
	if len(got) != 2 || !strings.Contains(got[1], "repeated=99") {
		t.Fatalf("after the window, want the line again with repeated=99:\n%s", buf)
	}
}

// What tells two problems apart is never held back: a different host, a
// different error or a different level is a different line.
func TestDistinctLinesAreAllWritten(t *testing.T) {
	log, buf, _ := newTest(10 * time.Minute)
	log.Warn("unknown host", "hostname", "a")
	log.Warn("unknown host", "hostname", "b")
	log.Error("unknown host", "hostname", "a")
	log.Warn("unknown host", "hostname", "a")
	log.With("component", "x").Warn("unknown host", "hostname", "a")
	if got := lines(buf); len(got) != 4 {
		t.Fatalf("wrote %d lines, want 4:\n%s", len(got), buf)
	}
}

func TestKeysAreBounded(t *testing.T) {
	log, _, _ := newTest(time.Hour)
	h := log.Handler().(*Handler)
	for i := 0; i < 3*maxKeys; i++ {
		log.Info("line", "n", i)
	}
	if n := len(h.st.lines); n > maxKeys {
		t.Fatalf("remembers %d lines, bound is %d", n, maxKeys)
	}
}

func TestLevelIsHonoured(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: ParseLevel("warn")}), 0))
	log.Info("not wanted")
	log.Warn("wanted")
	if got := lines(&buf); len(got) != 1 || !strings.Contains(got[0], "wanted") {
		t.Fatalf("got %q", got)
	}
}

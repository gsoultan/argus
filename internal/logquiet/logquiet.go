// Package logquiet keeps a line that repeats from filling the disk.
//
// Both argus-control and argus-agent log to the systemd journal, and a loop
// that fails the same way every few seconds -- an agent that cannot reach the
// control plane, a heartbeat from a host the inventory does not know, a
// tunnel that a proxy drops every minute -- writes the same line for as long
// as the condition lasts. Across a fleet that is the bulk of what the
// journal holds, on every host at once, and it pushes out the lines that
// were worth keeping.
//
// Handler writes a line the first time it is seen and then stays quiet about
// it for a window. The next time the same line arrives after the window, it
// is written again with repeated=N, the number held back in between. Nothing
// is lost that a reader needs: the first occurrence is always written, and so
// is the fact and the count of the rest.
//
// It is shared because both binaries run the same journal on the same kind
// of host and fail in the same loops; it imports only the standard library.
package logquiet

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DefaultWindow is how long a repeated line is held back.
const DefaultWindow = 10 * time.Minute

// maxKeys bounds what is remembered. A line's key includes its attributes,
// and some of those come from outside (a hostname a report claims), so the
// set is cleared rather than allowed to grow without limit.
const maxKeys = 1024

type seen struct {
	at      time.Time // when it was last written
	skipped int
}

type state struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time
	lines  map[string]*seen
}

// Handler wraps another slog.Handler; see the package comment.
type Handler struct {
	next  slog.Handler
	st    *state
	attrs string // from WithAttrs and WithGroup, part of every key
}

// New wraps next. A window of zero or less is DefaultWindow.
func New(next slog.Handler, window time.Duration) *Handler {
	if window <= 0 {
		window = DefaultWindow
	}
	return &Handler{next: next, st: &state{window: window, now: time.Now,
		lines: map[string]*seen{}}}
}

// Enabled reports what the wrapped handler would.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

// Handle writes r, or counts it when the same line was written within the
// window.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	key := h.key(r)
	now := h.st.now()

	h.st.mu.Lock()
	s, ok := h.st.lines[key]
	if ok && now.Sub(s.at) < h.st.window {
		s.skipped++
		h.st.mu.Unlock()
		return nil
	}
	skipped := 0
	if ok {
		skipped = s.skipped
	}
	if !ok && len(h.st.lines) >= maxKeys {
		h.st.lines = map[string]*seen{}
	}
	h.st.lines[key] = &seen{at: now}
	h.st.mu.Unlock()

	if skipped > 0 {
		r = r.Clone()
		r.AddAttrs(slog.Int("repeated", skipped),
			slog.String("repeated_over", h.st.window.String()))
	}
	return h.next.Handle(ctx, r)
}

// WithAttrs returns a handler that shares what has been seen.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range as {
		writeAttr(&b, a)
	}
	return &Handler{next: h.next.WithAttrs(as), st: h.st, attrs: b.String()}
}

// WithGroup returns a handler that shares what has been seen.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), st: h.st, attrs: h.attrs + "[" + name + "]"}
}

// key is the line without what changes between two tellings of it: the time,
// and any duration or time attribute, such as how long until the next retry.
func (h *Handler) key(r slog.Record) string {
	var b strings.Builder
	b.WriteString(r.Level.String())
	b.WriteByte(0)
	b.WriteString(r.Message)
	b.WriteByte(0)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, a)
		return true
	})
	return b.String()
}

func writeAttr(b *strings.Builder, a slog.Attr) {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindDuration, slog.KindTime:
		return
	case slog.KindGroup:
		b.WriteString(a.Key)
		b.WriteByte('{')
		for _, g := range v.Group() {
			writeAttr(b, g)
		}
		b.WriteByte('}')
		return
	}
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(v.String())
	b.WriteByte(0)
}

// ParseLevel reads "debug", "info", "warn" or "error"; anything else is info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

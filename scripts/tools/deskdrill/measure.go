package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/rdp/pixels"
)

// What a Remote Desktop through the agent is like to use, measured from the
// browser's side of the control plane.
//
// It answers the questions the Windows speed target asks (docs/capacity.md):
// how long a keystroke takes to show, how often the screen changes while a
// window is busy, and how many bits that costs. The pixels are decoded with
// the same decoder the console runs as WebAssembly, natively here, so the
// decode figure is a floor for what a browser spends, not what it spends.

// update is one message from the control plane, as it arrived.
type update struct {
	at     time.Time
	bytes  int
	rects  int
	pixels int
	decode time.Duration
}

// Report is what -measure writes, for CI's summary and for comparing runs.
type Report struct {
	Width, Height int
	Depth         int

	FirstPaintMS    float64
	FirstPaintBytes int

	IdleBitsPerSec float64

	KeySamples int
	KeyP50MS   float64
	KeyP95MS   float64
	KeyMaxMS   float64
	KeyMissed  int

	BusySeconds        float64
	BusyMbitPerSec     float64
	BusyMessagesPerSec float64
	// BusyFramesPerSec counts 1/60 s slots in which the screen changed: what
	// a 60 Hz display in the browser would get to show.
	BusyFramesPerSec float64
	// BusyScreensPerSec is the area redrawn per second, in whole desktops.
	BusyScreensPerSec float64
	// BusyDecodeShare is the decode time per second of session, natively.
	BusyDecodeShare float64
	BusyGapP95MS    float64
	BusyGapMaxMS    float64
}

type meter struct {
	ctx     context.Context
	c       *websocket.Conn
	send    func(any)
	updates chan update
	errs    chan error
	width   int
	height  int
}

func measure(ctx context.Context, c *websocket.Conn, send func(any), busyFor time.Duration,
	keys int, reportPath string) {

	m := &meter{ctx: ctx, c: c, send: send, updates: make(chan update, 4096), errs: make(chan error, 1)}

	// The session first: until the desktop is up there is nothing to time.
	start := time.Now()
	for m.width == 0 {
		typ, data, err := c.Read(ctx)
		if err != nil {
			fail("the desktop ended before it was ready: %v", err)
		}
		if typ == websocket.MessageText {
			var t struct{ Type, Data, Session string }
			_ = json.Unmarshal(data, &t)
			switch t.Type {
			case "ready":
				fmt.Fprintf(os.Stderr, "session=%s\n", t.Session)
			case "error":
				fail("refused: %s", t.Data)
			}
			continue
		}
		if len(data) >= rdp.FrameHeaderSize && data[0] == rdp.FrameReady {
			m.width = int(binary.LittleEndian.Uint16(data[6:8]))
			m.height = int(binary.LittleEndian.Uint16(data[8:10]))
		}
	}
	go m.read()
	r := Report{Width: m.width, Height: m.height, Depth: 16}
	fmt.Fprintf(os.Stderr, "ready %dx%d\n", m.width, m.height)

	// First paint: everything until the desktop stops changing. A new
	// account's first sign-in plays its welcome screen here.
	got := m.until(2*time.Second, 90*time.Second)
	if len(got) == 0 {
		fail("the desktop never drew")
	}
	r.FirstPaintMS = ms(got[len(got)-1].at.Sub(start))
	for _, u := range got {
		r.FirstPaintBytes += u.bytes
	}

	// Idle: what a desktop nobody touches costs.
	idle := m.window(5 * time.Second)
	r.IdleBitsPerSec = float64(sum(idle, func(u update) int { return u.bytes })*8) / 5

	// Keystrokes into Notepad: from the key going down to the first update
	// after it. The caret's blink would answer some of them early; CI turns
	// it off for the account before the session.
	m.run("notepad")
	m.until(time.Second, 15*time.Second)
	var lat []float64
	for range keys {
		m.drain()
		sent := time.Now()
		m.key("KeyA", false)
		u, ok := m.next(2 * time.Second)
		if !ok {
			r.KeyMissed++
			continue
		}
		lat = append(lat, ms(u.at.Sub(sent)))
		m.until(300*time.Millisecond, 3*time.Second)
	}
	r.KeySamples = len(lat)
	if len(lat) > 0 {
		slices.Sort(lat)
		r.KeyP50MS, r.KeyP95MS, r.KeyMaxMS = pct(lat, 50), pct(lat, 95), lat[len(lat)-1]
	}

	// Busy: a maximised console listing a large tree, which redraws most of
	// the screen for as long as it runs.
	m.run(`cmd /c start /max cmd /k dir /s c:\windows`)
	busy := m.window(busyFor)
	if len(busy) > 1 {
		secs := busyFor.Seconds()
		r.BusySeconds = secs
		r.BusyMbitPerSec = float64(sum(busy, func(u update) int { return u.bytes })*8) / secs / 1e6
		r.BusyMessagesPerSec = float64(len(busy)) / secs
		slots := map[int64]bool{}
		var decode time.Duration
		var area int
		for _, u := range busy {
			slots[u.at.UnixNano()/int64(time.Second/60)] = true
			decode += u.decode
			area += u.pixels
		}
		r.BusyFramesPerSec = float64(len(slots)) / secs
		r.BusyScreensPerSec = float64(area) / float64(m.width*m.height) / secs
		r.BusyDecodeShare = decode.Seconds() / secs
		var gaps []float64
		for i := 1; i < len(busy); i++ {
			gaps = append(gaps, ms(busy[i].at.Sub(busy[i-1].at)))
		}
		slices.Sort(gaps)
		r.BusyGapP95MS, r.BusyGapMaxMS = pct(gaps, 95), gaps[len(gaps)-1]
	}

	_ = c.Close(websocket.StatusNormalClosure, "done")

	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
	if reportPath != "" {
		if err := os.WriteFile(reportPath, b, 0o644); err != nil {
			fail("%v", err)
		}
	}
	if r.KeySamples == 0 {
		fail("no keystroke drew anything")
	}
	if r.BusyMessagesPerSec == 0 {
		fail("the busy window never drew")
	}
}

// read timestamps every update as it arrives, then decodes it.
func (m *meter) read() {
	var dec pixels.Decoder
	for {
		typ, data, err := m.c.Read(m.ctx)
		if err != nil {
			m.errs <- err
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		u := update{at: time.Now(), bytes: len(data)}
		t0 := time.Now()
		for p := data; len(p) >= rdp.FrameHeaderSize; {
			w := int(binary.LittleEndian.Uint16(p[6:8]))
			h := int(binary.LittleEndian.Uint16(p[8:10]))
			size := rdp.FrameHeaderSize
			switch p[0] {
			case rdp.FrameEncoded:
				n := int(binary.LittleEndian.Uint16(p[10:12]))
				size += (n + 3) &^ 3
				if size > len(p) {
					fail("an update is cut short")
				}
				body := p[rdp.FrameHeaderSize : rdp.FrameHeaderSize+n]
				if _, err := dec.Decode(body, w, h, int(p[1]&0x7F), p[1]&0x80 != 0); err != nil {
					fail("an update did not decode: %v", err)
				}
				u.rects++
				u.pixels += w * h
			case rdp.FrameBitmap:
				size += w * h * 4
				u.rects++
				u.pixels += w * h
			case rdp.FrameClosed:
				m.errs <- fmt.Errorf("the desktop closed")
				return
			}
			p = p[min(size, len(p)):]
		}
		u.decode = time.Since(t0)
		if u.rects > 0 {
			m.updates <- u
		}
	}
}

// next waits up to d for one update.
func (m *meter) next(d time.Duration) (update, bool) {
	select {
	case u := <-m.updates:
		return u, true
	case err := <-m.errs:
		fail("the desktop ended mid-measurement: %v", err)
	case <-time.After(d):
	}
	return update{}, false
}

// until collects updates until none has come for quiet, or max has passed.
func (m *meter) until(quiet, max time.Duration) []update {
	var got []update
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		u, ok := m.next(min(quiet, time.Until(deadline)))
		if !ok {
			break
		}
		got = append(got, u)
	}
	return got
}

// window collects every update for exactly d.
func (m *meter) window(d time.Duration) []update {
	var got []update
	end := time.Now().Add(d)
	for {
		left := time.Until(end)
		if left <= 0 {
			return got
		}
		if u, ok := m.next(left); ok {
			got = append(got, u)
		}
	}
}

// drain drops updates already queued.
func (m *meter) drain() {
	for {
		select {
		case <-m.updates:
		default:
			return
		}
	}
}

func (m *meter) key(code string, shift bool) {
	if shift {
		m.send(map[string]any{"type": "key", "code": "ShiftLeft", "down": true})
	}
	m.send(map[string]any{"type": "key", "code": code, "down": true})
	m.send(map[string]any{"type": "key", "code": code, "down": false})
	if shift {
		m.send(map[string]any{"type": "key", "code": "ShiftLeft", "down": false})
	}
}

// run starts a command from the Run box, typed as a US keyboard would.
func (m *meter) run(command string) {
	m.send(map[string]any{"type": "key", "code": "MetaLeft", "down": true})
	m.key("KeyR", false)
	m.send(map[string]any{"type": "key", "code": "MetaLeft", "down": false})
	m.until(500*time.Millisecond, 5*time.Second)
	for _, r := range command {
		code, shift := keyFor(r)
		m.key(code, shift)
	}
	m.key("Enter", false)
}

func keyFor(r rune) (code string, shift bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return "Key" + strings.ToUpper(string(r)), false
	case r >= '0' && r <= '9':
		return "Digit" + string(r), false
	}
	switch r {
	case ' ':
		return "Space", false
	case '/':
		return "Slash", false
	case '\\':
		return "Backslash", false
	case '.':
		return "Period", false
	case '-':
		return "Minus", false
	case ':':
		return "Semicolon", true
	}
	fail("no key for %q", r)
	return "", false
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func pct(sorted []float64, p int) float64 {
	return sorted[min(len(sorted)-1, len(sorted)*p/100)]
}

func sum(us []update, f func(update) int) int {
	n := 0
	for _, u := range us {
		n += f(u)
	}
	return n
}

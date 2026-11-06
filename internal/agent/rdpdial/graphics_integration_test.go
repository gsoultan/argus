package rdpdial

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gsoultan/argus/internal/rdp"
	"net"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// What the graphics pipeline costs on the wire against bitmap updates, for
// the same busy screen on a real Windows host.
//
//	ARGUS_TEST_RDP_TARGET=127.0.0.1:3389 \
//	ARGUS_TEST_RDP_USER=alice ARGUS_TEST_RDP_PASSWORD=... \
//	go test ./internal/rdp/ -run TargetGraphics -v
//
// Each mode signs in at 1920x1080, starts a maximised console listing a large
// tree from the Run box, and counts for ARGUS_TEST_RDP_SECONDS (default 20)
// what arrives. ARGUS_TEST_RDP_REPORT names a file for the figures as JSON.
func TestTargetGraphics(t *testing.T) {
	addr := rdpTarget(t)
	user := os.Getenv("ARGUS_TEST_RDP_USER")
	password := os.Getenv("ARGUS_TEST_RDP_PASSWORD")
	if user == "" || password == "" {
		t.Skip("set ARGUS_TEST_RDP_USER and ARGUS_TEST_RDP_PASSWORD to log in")
	}
	busy := 20 * time.Second
	if s := os.Getenv("ARGUS_TEST_RDP_SECONDS"); s != "" {
		d, err := time.ParseDuration(s + "s")
		if err != nil {
			t.Fatal(err)
		}
		busy = d
	}

	report := map[string]graphicsMeasure{}
	for _, mode := range []string{"bitmaps", "graphics"} {
		m := measureMode(t, addr, user, password, mode == "graphics", busy)
		t.Logf("%-8s %s", mode, m)
		report[mode] = m
	}
	if path := os.Getenv("ARGUS_TEST_RDP_REPORT"); path != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := report["graphics"]
	if g.CodecCommands["bitmap"] > 0 {
		t.Errorf("Windows sent %d bitmap updates with the graphics pipeline asked for", g.CodecCommands["bitmap"])
	}
	if g.Commands == g.CodecCommands["bitmap"] {
		t.Fatal("the graphics pipeline drew nothing")
	}
	if g.Frames == 0 {
		t.Fatal("the graphics pipeline never ended a frame")
	}
}

type graphicsMeasure struct {
	Seconds       float64
	MbitPerSec    float64
	Frames        int // EndFrame, or updates for bitmaps
	FramesPerSec  float64
	Commands      int
	CodecBytes    map[string]int
	CodecCommands map[string]int
	// KeyFrames counts H.264 pictures a decoder could start on (an IDR)
	// while busy, and KeyAfterRefresh says whether one came within a few
	// seconds of asking for the screen again: what someone who starts
	// watching partway through needs.
	KeyFrames       int
	KeyAfterRefresh bool
	// DroppedOnRefresh says the server ended the session when asked to
	// repaint, which is what a viewer joining would do to it.
	DroppedOnRefresh bool
}

func (m graphicsMeasure) String() string {
	var codecs []string
	for k, v := range m.CodecBytes {
		codecs = append(codecs, fmt.Sprintf("%s=%dKiB/%d", k, v/1024, m.CodecCommands[k]))
	}
	sort.Strings(codecs)
	return fmt.Sprintf("%.2f Mbit/s, %.1f frames/s, %d commands, %s, %d key frames, key after refresh %v, dropped on refresh %v",
		m.MbitPerSec, m.FramesPerSec, m.Commands, strings.Join(codecs, " "), m.KeyFrames, m.KeyAfterRefresh,
		m.DroppedOnRefresh)
}

func measureMode(t *testing.T, addr, user, password string, graphics bool, busy time.Duration) graphicsMeasure {
	t.Helper()
	m := graphicsMeasure{CodecBytes: map[string]int{}, CodecCommands: map[string]int{}}
	counting, refreshed := false, false
	cfg := rdp.ClientConfig{Width: 1920, Height: 1080, Depth: 16}
	if graphics {
		cfg.Graphics = func(p rdp.GraphicsPDU) {
			if !counting {
				return
			}
			m.Commands++
			name := "other"
			switch p.Cmd {
			case rdp.GfxWireToSurface1, rdp.GfxWireToSurface2:
				name = rdp.CodecName(p.Codec)
				if hasIDR(p.Bitmap) {
					if refreshed {
						m.KeyAfterRefresh = true
					} else {
						m.KeyFrames++
					}
				}
			case rdp.GfxSolidFill:
				name = "solidfill"
			case rdp.GfxSurfaceToSurface:
				name = "surface-to-surface"
			case rdp.GfxCacheToSurface, rdp.GfxSurfaceToCache:
				name = "cache"
			case rdp.GfxEndFrame:
				m.Frames++
				return
			case rdp.GfxStartFrame:
				return
			}
			m.CodecBytes[name] += len(p.Body)
			m.CodecCommands[name]++
		}
	}

	conn, protocol, logon := dialForMeasurement(t, addr, "", user, password)
	cfg.SelectedProtocol, cfg.Logon = protocol, logon
	client, err := rdp.Connect(conn, cfg)
	if err != nil {
		conn.Close()
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	wire := 0
	client.SetTap(func(pdu []byte) {
		if counting {
			wire += len(pdu)
		}
	})

	read := func(d time.Duration) {
		client.SetReadTimeout(100 * time.Millisecond)
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			bitmaps, err := client.NextBitmaps()
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if err != nil && refreshed {
				m.DroppedOnRefresh = true
				return
			}
			if err != nil {
				t.Fatalf("reading updates: %v", err)
			}
			if counting && len(bitmaps) > 0 {
				m.Frames++
				m.Commands += len(bitmaps)
				for _, b := range bitmaps {
					m.CodecBytes["bitmap"] += len(b.Data)
					m.CodecCommands["bitmap"]++
				}
			}
		}
	}

	// Settle, then start the busy window and count only from there.
	read(5 * time.Second)
	go runFromRunBox(client, `cmd /c start /max cmd /k dir /s c:\windows`)
	counting = true
	start := time.Now()
	read(busy)
	secs := time.Since(start).Seconds()
	busyWire, busyFrames := wire, m.Frames
	if graphics {
		// A viewer arriving now: ask for the whole screen again, and see
		// whether a picture to start on follows.
		refreshed = true
		if err := client.Refresh(); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		read(3 * time.Second)
	}
	counting = false

	m.Seconds = secs
	m.Frames = busyFrames
	m.MbitPerSec = float64(busyWire) * 8 / secs / 1e6
	m.FramesPerSec = float64(busyFrames) / secs
	return m
}

// hasIDR reports whether an H.264 stream in Annex B holds an IDR slice.
func hasIDR(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 && b[i+3]&0x1F == 5 {
			return true
		}
	}
	return false
}

// runFromRunBox opens the Run box and types a command into it.
func runFromRunBox(c *rdp.Client, command string) {
	key := func(code string, down bool) {
		sc, ext, _ := rdp.ScancodeFor(code)
		_ = c.Send(rdp.InputEvent{Kind: "key", Scancode: sc, Extended: ext, Down: down})
	}
	press := func(code string) { key(code, true); key(code, false) }
	key("MetaLeft", true)
	press("KeyR")
	key("MetaLeft", false)
	time.Sleep(1500 * time.Millisecond)
	for _, r := range command {
		if r == ':' {
			key("ShiftLeft", true)
			press("Semicolon")
			key("ShiftLeft", false)
			continue
		}
		press(keyCodeFor(r))
		time.Sleep(10 * time.Millisecond)
	}
	press("Enter")
}

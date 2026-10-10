package rdp

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/credssp"
)

// What a browser session costs on the wire, measured against a real server.
//
//	ARGUS_TEST_RDP_TARGET=192.168.101.91:3389 \
//	ARGUS_TEST_RDP_USER=alice ARGUS_TEST_RDP_PASSWORD=... \
//	go test ./internal/rdp/ -run TargetBandwidth -v
//
// It logs in, types ARGUS_TEST_RDP_TYPE (default: a command that scrolls a
// terminal hard), and reports for the same updates what the gateway would send
// a browser two ways: decoded to RGBA, as it used to, and forwarded as the
// target sent them, as it does now. The difference between those two numbers
// is the difference between a session that keeps up on a slow link and one
// that does not.
//
// ARGUS_TEST_RDP_DOMAIN and ARGUS_TEST_RDP_DEPTH (16 or 24) are optional.
func TestTargetBandwidth(t *testing.T) {
	addr := rdpTarget(t)
	user := os.Getenv("ARGUS_TEST_RDP_USER")
	password := os.Getenv("ARGUS_TEST_RDP_PASSWORD")
	if user == "" || password == "" {
		t.Skip("set ARGUS_TEST_RDP_USER and ARGUS_TEST_RDP_PASSWORD to log in")
	}
	domain := os.Getenv("ARGUS_TEST_RDP_DOMAIN")
	depth := 16
	if os.Getenv("ARGUS_TEST_RDP_DEPTH") == "24" {
		depth = 24
	}
	typed := os.Getenv("ARGUS_TEST_RDP_TYPE")
	if typed == "" {
		typed = "ls -laR /usr\n"
	}

	conn, protocol, logon := dialForMeasurement(t, addr, domain, user, password)
	client, err := Connect(conn, ClientConfig{
		Width: 1280, Height: 800, Depth: depth,
		SelectedProtocol: protocol, Logon: logon,
	})
	if err != nil {
		conn.Close()
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var m measurement
	client.SetTap(func(pdu []byte) { m.wire += len(pdu) })

	// Let the desktop settle, so the first full paint is counted separately
	// from the activity.
	m.collect(t, client, 4*time.Second)
	t.Logf("first paint: %s", m.summary())
	first := m
	m = measurement{}

	go typeText(client, typed)
	m.collect(t, client, 12*time.Second)
	t.Logf("activity:    %s", m.summary())

	total := first
	total.add(m)
	t.Logf("total:       %s", total.summary())
	if total.encoded == 0 {
		t.Fatal("no screen updates arrived; nothing was measured")
	}
}

type measurement struct {
	wire, decoded, encoded, rects int
	decodeTime                    time.Duration
	elapsed                       time.Duration
}

func (m *measurement) add(o measurement) {
	m.wire += o.wire
	m.decoded += o.decoded
	m.encoded += o.encoded
	m.rects += o.rects
	m.decodeTime += o.decodeTime
	m.elapsed += o.elapsed
}

func (m *measurement) collect(t *testing.T, c *Client, d time.Duration) {
	t.Helper()
	start := time.Now()
	c.readTimeout = 250 * time.Millisecond
	for time.Since(start) < d {
		bitmaps, err := c.NextBitmaps()
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			continue
		}
		if err != nil {
			t.Fatalf("reading updates: %v", err)
		}
		for _, b := range bitmaps {
			m.rects++
			m.encoded += len(EncodeBitmap(b))
			began := time.Now()
			r, derr := b.Decode()
			m.decodeTime += time.Since(began)
			if derr != nil {
				t.Fatalf("decoding a %dx%d rectangle: %v", b.Width, b.Height, derr)
			}
			m.decoded += FrameHeaderSize + len(r.Pixels)
		}
	}
	m.elapsed += time.Since(start)
}

func (m measurement) summary() string {
	ratio := 0.0
	if m.encoded > 0 {
		ratio = float64(m.decoded) / float64(m.encoded)
	}
	secs := m.elapsed.Seconds()
	return strings.Join([]string{
		"rects=" + fmt.Sprint(m.rects),
		"wire=" + kb(m.wire),
		"to-browser decoded=" + kb(m.decoded) + " (" + kb(int(float64(m.decoded)/secs)) + "/s)",
		"encoded=" + kb(m.encoded) + " (" + kb(int(float64(m.encoded)/secs)) + "/s)",
		fmt.Sprintf("ratio=%.1fx", ratio),
		"decode=" + m.decodeTime.Round(time.Millisecond).String(),
	}, " ")
}

func dialForMeasurement(t *testing.T, addr, domain, user, password string) (net.Conn, uint32, LogonInfo) {
	t.Helper()
	auth := &credssp.Authenticator{Credentials: credssp.Credentials{
		Domain: domain, User: user, Password: password, Workstation: "ARGUS",
	}}
	conn, _, err := DialTargetWithAuth(addr, "", ProtocolHybrid, nil, 10*time.Second, auth)
	if err == nil {
		return conn, ProtocolHybrid, LogonInfo{Domain: domain, User: user, Password: password}
	}
	t.Logf("credssp failed (%v); retrying over tls", err)
	conn, _, err = DialTargetWithAuth(addr, "", ProtocolSSL, nil, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn, ProtocolSSL, LogonInfo{Domain: domain, User: user, Password: password}
}

// typeText sends lowercase text as key presses, a few per frame's worth of
// time, which is roughly how fast a person types a command they know.
func typeText(c *Client, text string) {
	for _, r := range text {
		code := keyCodeFor(r)
		if code == "" {
			continue
		}
		sc, ext, ok := ScancodeFor(code)
		if !ok {
			continue
		}
		_ = c.Send(InputEvent{Kind: "key", Scancode: sc, Extended: ext, Down: true})
		_ = c.Send(InputEvent{Kind: "key", Scancode: sc, Extended: ext, Down: false})
		time.Sleep(40 * time.Millisecond)
	}
}

func keyCodeFor(r rune) string {
	switch {
	case r >= 'a' && r <= 'z':
		return "Key" + strings.ToUpper(string(r))
	case r >= '0' && r <= '9':
		return "Digit" + string(r)
	}
	return map[rune]string{
		' ': "Space", '\n': "Enter", '-': "Minus", '/': "Slash", '.': "Period",
		'\\': "Backslash", ';': "Semicolon", '=': "Equal",
	}[r]
}

func kb(n int) string { return fmt.Sprintf("%.1fKiB", float64(n)/1024) }

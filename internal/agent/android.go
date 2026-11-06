package agent

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
An Android device's screen, through the Argus app on it.

	browser ──wss──▶ argus-control ──"android" stream──▶ the app ──▶ the device

The app embeds this agent (gomobile) and hands it the device: a screen to
read and input to play. Everything else is what a shared Windows console
already has -- the grant check against the pinned key, the recording and its
chain, the display frames the browser draws, the live view -- so the console,
the player and argus-verify do not know a phone from a server.

What the device is asked to do is kept to what any Android can do:

  - Frame: the screen as it is, whenever it changes. MediaProjection in the
    app, a display mirror for the unattended helper, a drawing in the fake.
  - Play: touches, device keys (Back, Home, Recents, Power, volume, the
    notification shade, Lock) and text. An accessibility service in the app,
    input injection in the helper.

The session acts as whoever holds the device, which is what the attended
mode makes explicit: the recording says the device's user accepted it, and
when. The principal the grant names is "view" or "control"; "view" plays
nothing, which this side enforces as well as the console does.
*/

// AndroidService is the name the control plane asks for. A grant for it
// cannot be spent on a desktop or a terminal, nor the reverse.
const AndroidService = "android"

// androidProtocol is what the recording's header says it is.
const androidProtocol = "android"

// AndroidInput is one input from the console, as the browser sent it.
//
// Type is "mouse" (a move, with X and Y), "button" (a press or release at X
// and Y: a tap, or a drag while held), "wheel" (Delta, positive to scroll
// down), "key" (Code, the browser's KeyboardEvent.code, with Down),
// "device" (Name: back, home, recents, power, volume_up, volume_down,
// notifications, lock) or "text" (Text, typed into whatever has focus).
type AndroidInput struct {
	Type   string `json:"type"`
	Code   string `json:"code,omitempty"`
	Down   bool   `json:"down,omitempty"`
	X      int    `json:"x,omitempty"`
	Y      int    `json:"y,omitempty"`
	Button int    `json:"button,omitempty"`
	Delta  int    `json:"delta,omitempty"`
	Name   string `json:"name,omitempty"`
	Text   string `json:"text,omitempty"`
}

// The two principals a device has: no accounts, so these are fixed, and
// the control plane's assets say the same (control.DeviceView/Control).
const (
	DeviceViewPrincipal    = "view"
	DeviceControlPrincipal = "control"
)

// AndroidDeviceKeys is every Name a "device" input may carry.
var AndroidDeviceKeys = []string{"back", "home", "recents", "power", "volume_up", "volume_down",
	"notifications", "lock"}

// AndroidDevice is the device, as the app or a stand-in provides it.
type AndroidDevice interface {
	// Size is the screen's size in pixels. It may change (rotation).
	Size() (width, height int)
	// Frame waits up to wait for the screen to change and returns it as
	// top-down 32-bit BGRX rows of stride bytes; changed is false when
	// nothing did within wait. The buffer may be reused by the next call.
	Frame(wait time.Duration) (fb []byte, stride int, changed bool, err error)
	// Play plays one input on the device.
	Play(AndroidInput) error
	Close() error
}

// androidFrameWait is how long one look at the screen waits for a change
// before giving the loop back, so a closed session is noticed.
const androidFrameWait = 250 * time.Millisecond

// androidInputer is a desktop client that takes the console's input as it
// was sent, rather than as Remote Desktop events.
type androidInputer interface {
	Play(AndroidInput) error
}

// androidClient is an AndroidDevice as a DesktopClient: the screen as
// bitmaps of the tiles that changed, recorded as the Fast-Path updates a
// Remote Desktop server would have sent.
type androidClient struct {
	dev   AndroidDevice
	diff  screenDiffer
	view  bool
	force bool
	// last is the screen as it was last taken, for a forced repaint.
	last       []byte
	lastStride int

	mu     sync.Mutex
	tap    func([]byte)
	closed bool
}

// AndroidConfig is what the android service needs beyond a desktop's.
type AndroidConfig struct {
	DesktopConfig
	// Device opens the device for a session. Called once per session, and
	// may take its time: in attended mode it returns once whoever holds the
	// device has accepted, or with why not.
	Device func(AndroidOpen) (AndroidDevice, error)
}

// AndroidOpen is who a session is for, for the device to show whoever holds
// it before it opens.
type AndroidOpen struct {
	// Person is the email the grant names.
	Person string
	// Principal is DeviceViewPrincipal or DeviceControlPrincipal.
	Principal string
}

// Android returns the android service: the device's screen, driven from
// the console.
func Android(cfg AndroidConfig) TunnelService {
	open := cfg.Device
	cfg.Connect = func(_ DesktopConfig, g tunnel.Grant, _ tunnel.DesktopOpen,
		_ *slog.Logger) (DesktopClient, DesktopInfo, error) {
		if open == nil {
			return nil, DesktopInfo{}, errors.New("this agent has no device to share")
		}
		dev, err := open(AndroidOpen{Person: g.Email, Principal: g.Principal})
		if err != nil {
			return nil, DesktopInfo{}, err
		}
		c, info := connectAndroid(dev, g.Principal)
		return c, info, nil
	}
	return desktopService(cfg.DesktopConfig, AndroidService)
}

// connectAndroid opens the device for one session. "view" opens it with
// input refused here, whatever the console sends.
func connectAndroid(dev AndroidDevice, principal string) (*androidClient, DesktopInfo) {
	return &androidClient{dev: dev, view: principal == DeviceViewPrincipal, force: true},
		DesktopInfo{Android: true}
}

func (c *androidClient) NextBitmaps() ([]rdp.Bitmap, error) {
	c.mu.Lock()
	closed, force := c.closed, c.force
	c.force = false
	c.mu.Unlock()
	if closed {
		return nil, errors.New("closed")
	}
	fb, stride, changed, err := c.dev.Frame(androidFrameWait)
	if err != nil {
		return nil, err
	}
	if changed {
		// Kept for a repaint of a still screen, which a viewer arriving
		// late asks for.
		c.last, c.lastStride = append(c.last[:0], fb...), stride
	} else if !force {
		return nil, nil
	} else {
		fb, stride = c.last, c.lastStride
	}
	w, h := c.dev.Size()
	if len(fb) < stride*h || stride < w*4 {
		return nil, errors.New("the device returned a frame smaller than its screen")
	}
	shot := screenShot{fb: fb, stride: stride, width: w, height: h}
	jobs := c.diff.find(shot, force)
	if len(jobs) == 0 {
		return nil, nil
	}
	out := make([]rdp.Bitmap, 0, len(jobs))
	for _, j := range jobs {
		// A buffer per tile: the bitmaps are handed on together.
		out = append(out, rdp.CompressRect(nil, fb, stride, j.x, j.y, j.w, j.h))
	}
	c.mu.Lock()
	tap := c.tap
	c.mu.Unlock()
	if tap != nil {
		for _, pdu := range rdp.FastPathBitmaps(out) {
			tap(pdu)
		}
	}
	return out, nil
}

// Send takes Remote Desktop events; the console sends the android client
// its input as it was typed instead (Play), so nothing arrives here.
func (c *androidClient) Send(rdp.InputEvent) error { return nil }

// Play plays the console's input, unless the session only watches.
func (c *androidClient) Play(in AndroidInput) error {
	if c.view {
		return nil
	}
	return c.dev.Play(in)
}

func (c *androidClient) Refresh() error {
	c.mu.Lock()
	c.force = true
	c.mu.Unlock()
	return nil
}

func (c *androidClient) Size() (int, int) { return c.dev.Size() }

func (c *androidClient) SetTap(fn func([]byte)) {
	c.mu.Lock()
	c.tap = fn
	c.mu.Unlock()
}

func (c *androidClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.dev.Close()
}

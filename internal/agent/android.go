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

// androidActivitySource is an AndroidDevice that also reports what the device
// is doing -- the app in front, the screen locked, secure content hidden --
// so the android client can chain it into the recording alongside the screen.
// The app provides it through the accessibility service; the fake scripts it.
// A device that does not implement it simply has no timeline.
type androidActivitySource interface {
	// Activity waits up to wait for the next thing to happen on the device.
	// ok is false when nothing did within wait, or the device has closed, so
	// the drain loop can check whether it should stop. Each event is returned
	// once.
	Activity(wait time.Duration) (ev AndroidActivity, ok bool)
}

// AndroidActivity is one thing that happened on the device during a session,
// recorded in the chain next to the screen. It is what the Windows plan does
// with window titles and UAC prompts: an auditor replaying the session sees
// which app was in front, when the screen locked, and when the picture went
// black because an app marked its window secure -- not only the pixels.
type AndroidActivity struct {
	// Kind is what happened:
	//   app      the foreground app changed (Package, Title)
	//   lock     the screen locked
	//   unlock   the screen unlocked
	//   secure   secure content is now hidden (Hidden true) or shown again
	//   typed    text was typed (Text), or into a password field (Password,
	//            and Text is never carried)
	//   consent  whoever holds the device accepted the session (By, at this
	//            point in the recording)
	//   ended    the session was ended on the device (By)
	Kind string `json:"kind"`
	// Package and Title are the foreground app, for Kind "app".
	Package string `json:"package,omitempty"`
	Title   string `json:"title,omitempty"`
	// Hidden is whether secure content is now hidden, for Kind "secure".
	Hidden bool `json:"hidden,omitempty"`
	// Text is what was typed, for Kind "typed" and never for a password.
	Text string `json:"text,omitempty"`
	// Password is whether the focused field was a password, for Kind "typed":
	// the timeline says "typed into a password field" and carries no text.
	Password bool `json:"password,omitempty"`
	// By is who, for Kind "consent" and "ended" (the device's user, or
	// "device").
	By string `json:"by,omitempty"`
}

// AndroidActivityKinds is every Kind an AndroidActivity may carry. The client
// drops any other, so a newer app cannot write an event the console will not
// understand into an auditor's recording.
var AndroidActivityKinds = []string{"app", "lock", "unlock", "secure", "typed", "consent", "ended"}

func validActivityKind(k string) bool {
	for _, v := range AndroidActivityKinds {
		if v == k {
			return true
		}
	}
	return false
}

// androidActivityNote is an AndroidActivity as it is written into the
// recording: a Meta event tagged so the console can tell it from the
// console-account note a shared desktop writes.
type androidActivityNote struct {
	Type string `json:"type"`
	AndroidActivity
}

// AndroidApps is the installed apps the device reported, its "what runs here".
type AndroidApps struct {
	CheckedAt time.Time    `json:"checked_at"`
	Apps      []AndroidApp `json:"apps"`
}

// AndroidApp is one installed app on a device.
type AndroidApp struct {
	// Package is the application id, the stable name (com.example.app).
	Package string `json:"package"`
	// Label is what the launcher shows, when the app has one.
	Label string `json:"label,omitempty"`
	// Version is the versionName the package reports.
	Version string `json:"version,omitempty"`
	// Source is where it came from: "play", "preinstalled" or "sideloaded".
	Source string `json:"source,omitempty"`
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
	// note writes a Meta event into the recording's chain; the desktop
	// service sets it. nil until then, and nil on a device with no activity.
	note    func(any) error
	actStop chan struct{}
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

// startNotes drains the device's activity into the recording. The desktop
// service calls it once it has set note, so every timeline event is in the
// same chain as the screen it describes. A device that reports no activity,
// or a session with no recording, starts nothing.
func (c *androidClient) startNotes() {
	src, ok := c.dev.(androidActivitySource)
	if !ok || c.note == nil {
		return
	}
	stop := make(chan struct{})
	c.mu.Lock()
	c.actStop = stop
	c.mu.Unlock()
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			ev, ok := src.Activity(androidFrameWait)
			if !ok {
				continue
			}
			if !validActivityKind(ev.Kind) {
				continue
			}
			if ev.Password {
				// The text of a password is never in the recording; the
				// timeline says only that one was typed.
				ev.Text = ""
			}
			if err := c.note(androidActivityNote{Type: "android-activity", AndroidActivity: ev}); err != nil {
				return
			}
		}
	}()
}

func (c *androidClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	stop := c.actStop
	c.actStop = nil
	c.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	return c.dev.Close()
}

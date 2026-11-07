package agent

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"
	"time"
)

/*
A stand-in for an Android device, so the console, the control plane and the
recording can be driven end to end with no phone: `argus-agent daemon
--fake-android` offers it on the android service, and the tests use it
directly.

It draws a phone: a status bar whose clock bar advances once a second, a
title, a search field that shows what was typed, and a list whose rows light
up when tapped. Back clears the tap, Home clears everything. Nothing in it
pretends to be Android; it is a picture that answers input, which is enough
to see that touches, keys and text arrive and that what was shown is in the
recording.
*/

// FakeAndroid is a drawn phone. The zero value is not ready; use
// NewFakeAndroid.
type FakeAndroid struct {
	width, height int

	mu      sync.Mutex
	fb      []byte // top-down BGRX
	dirty   bool
	closed  bool
	picked  int // the row last tapped, -1 for none
	typed   string
	second  int
	locked  bool
	plays   []AndroidInput
	changed chan struct{}
	done    chan struct{}

	// Activity the fake scripts from the input it is played, so the recording
	// gets a timeline with no phone: the first look reports the app in front,
	// then a lock, a type and so on follow what the console does.
	announced bool
	acts      []AndroidActivity
	actCh     chan struct{}
}

// Fake phone geometry, in pixels.
const (
	fakeStatusBar = 48
	fakeTitleTop  = 72
	fakeSearchTop = 150
	fakeRowsTop   = 230
	fakeRowHeight = 96
	fakeRows      = 10
)

// NewFakeAndroid makes a phone of the given size, portrait by default.
func NewFakeAndroid(width, height int) *FakeAndroid {
	if width <= 0 || height <= 0 {
		width, height = 540, 1200
	}
	f := &FakeAndroid{width: width, height: height, picked: -1,
		fb: make([]byte, width*height*4), changed: make(chan struct{}, 1), done: make(chan struct{}),
		actCh: make(chan struct{}, 1)}
	f.redraw()
	go f.tick()
	return f
}

// tick advances the clock bar once a second, so the screen is never
// entirely still: a live view with a frame rate of zero looks broken.
func (f *FakeAndroid) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-t.C:
			f.mu.Lock()
			f.second = (f.second + 1) % 60
			f.redraw()
			f.mu.Unlock()
		}
	}
}

// Plays is every input the device was asked to play, in order.
func (f *FakeAndroid) Plays() []AndroidInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AndroidInput(nil), f.plays...)
}

func (f *FakeAndroid) Size() (int, int) { return f.width, f.height }

func (f *FakeAndroid) Frame(wait time.Duration) ([]byte, int, bool, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	f.mu.Lock()
	// A signal may be left over from a redraw already taken, so a wake-up
	// is a reason to look again, not a change in itself.
	for !f.dirty && !f.closed {
		f.mu.Unlock()
		select {
		case <-f.changed:
		case <-f.done:
		case <-deadline.C:
			return nil, 0, false, nil
		}
		f.mu.Lock()
	}
	if f.closed {
		f.mu.Unlock()
		return nil, 0, false, errors.New("the device is closed")
	}
	defer f.mu.Unlock()
	f.dirty = false
	out := append([]byte(nil), f.fb...)
	return out, f.width * 4, true, nil
}

func (f *FakeAndroid) Play(in AndroidInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("the device is closed")
	}
	f.plays = append(f.plays, in)
	switch in.Type {
	case "button":
		if in.Down && in.Y >= fakeRowsTop {
			if row := (in.Y - fakeRowsTop) / fakeRowHeight; row < fakeRows {
				f.picked = row
				// Opening a row is a different app in front.
				f.emit(AndroidActivity{Kind: "app",
					Package: "com.argus.fakephone.item", Title: "Item"})
			}
		}
	case "device":
		switch in.Name {
		case "back":
			f.picked = -1
		case "home":
			f.picked, f.typed = -1, ""
			f.emit(AndroidActivity{Kind: "app",
				Package: "com.android.launcher", Title: "Home"})
		case "lock":
			if !f.locked {
				f.locked = true
				f.emit(AndroidActivity{Kind: "lock"})
			}
		}
	case "text":
		f.typed += in.Text
		f.emit(AndroidActivity{Kind: "typed", Text: in.Text})
	case "key":
		if in.Down && in.Code == "Backspace" && f.typed != "" {
			f.typed = f.typed[:len(f.typed)-1]
		}
	}
	f.redraw()
	return nil
}

// emit queues one activity event and wakes a waiting Activity. Called with
// mu held.
func (f *FakeAndroid) emit(a AndroidActivity) {
	f.acts = append(f.acts, a)
	select {
	case f.actCh <- struct{}{}:
	default:
	}
}

// Activity reports what the fake is doing: the app in front at the start,
// then whatever the input scripts. It satisfies androidActivitySource, so
// the android client chains these into the recording.
func (f *FakeAndroid) Activity(wait time.Duration) (AndroidActivity, bool) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	f.mu.Lock()
	if !f.announced && !f.closed {
		f.announced = true
		f.mu.Unlock()
		return AndroidActivity{Kind: "app", Package: "com.argus.fakephone", Title: "Argus Fake Phone"}, true
	}
	for len(f.acts) == 0 && !f.closed {
		f.mu.Unlock()
		select {
		case <-f.actCh:
		case <-f.done:
		case <-deadline.C:
			return AndroidActivity{}, false
		}
		f.mu.Lock()
	}
	if f.closed {
		f.mu.Unlock()
		return AndroidActivity{}, false
	}
	ev := f.acts[0]
	f.acts = f.acts[1:]
	f.mu.Unlock()
	return ev, true
}

// Apps is a fixed list of installed apps, so the device page's Apps tab has
// something to show with no phone.
func (f *FakeAndroid) Apps() AndroidApps {
	return AndroidApps{
		CheckedAt: time.Now().UTC(),
		Apps: []AndroidApp{
			{Package: "com.android.chrome", Label: "Chrome", Version: "131.0", Source: "play"},
			{Package: "com.android.settings", Label: "Settings", Version: "14", Source: "preinstalled"},
			{Package: "com.argus.fakephone", Label: "Argus Fake Phone", Version: "1.0", Source: "sideloaded"},
		},
	}
}

func (f *FakeAndroid) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

// redraw paints the whole screen and marks it changed. Called with mu held.
func (f *FakeAndroid) redraw() {
	img := &image.RGBA{Pix: f.fb, Stride: f.width * 4, Rect: image.Rect(0, 0, f.width, f.height)}
	fill := func(r image.Rectangle, c color.RGBA) {
		// BGRX in memory: swap what image.RGBA would write.
		draw.Draw(img, r, image.NewUniform(color.RGBA{R: c.B, G: c.G, B: c.R, A: 255}), image.Point{}, draw.Src)
	}
	w := f.width
	bg := color.RGBA{R: 0x0f, G: 0x17, B: 0x2a}
	fill(img.Rect, bg)
	// Status bar: black, a clock bar that grows with the seconds.
	fill(image.Rect(0, 0, w, fakeStatusBar), color.RGBA{})
	fill(image.Rect(16, 20, 16+w/3*(f.second+1)/60, 28), color.RGBA{R: 0xe5, G: 0xe7, B: 0xeb})
	// Title.
	fill(image.Rect(24, fakeTitleTop, 24+w/2, fakeTitleTop+40), color.RGBA{R: 0xff, G: 0xff, B: 0xff})
	// Search field, with the typed text as a bar of its length.
	fill(image.Rect(24, fakeSearchTop, w-24, fakeSearchTop+56), color.RGBA{R: 0x1e, G: 0x29, B: 0x3b})
	if n := len(strings.TrimSpace(f.typed)); n > 0 {
		fill(image.Rect(40, fakeSearchTop+20, min(40+n*14, w-40), fakeSearchTop+36),
			color.RGBA{R: 0xf8, G: 0xfa, B: 0xfc})
	}
	// Rows: an icon and two lines; the tapped one is highlighted.
	for i := 0; i < fakeRows; i++ {
		top := fakeRowsTop + i*fakeRowHeight
		if top+fakeRowHeight > f.height {
			break
		}
		if i == f.picked {
			fill(image.Rect(0, top, w, top+fakeRowHeight), color.RGBA{R: 0x1e, G: 0x3a, B: 0x8a})
		}
		icon := color.RGBA{R: 0x1e, G: 0x29, B: 0x3b}
		if i == f.picked {
			icon = color.RGBA{R: 0x25, G: 0x63, B: 0xeb}
		}
		fill(image.Rect(24, top+24, 72, top+72), icon)
		fill(image.Rect(96, top+28, 96+w/3, top+44), color.RGBA{R: 0xff, G: 0xff, B: 0xff})
		fill(image.Rect(96, top+56, 96+w/2, top+68), color.RGBA{R: 0x94, G: 0xa3, B: 0xb8})
	}
	f.dirty = true
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

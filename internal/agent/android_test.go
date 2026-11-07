package agent

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

/*
The android client against the drawn phone: the screen arrives as tiles and
is recorded as Fast-Path updates before it is handed on; a tap changes what
the next look sees; "view" plays nothing on the device.
*/

func TestADeviceScreenArrivesAsTilesAndIsRecordedFirst(t *testing.T) {
	dev := NewFakeAndroid(256, 512)
	c, info := connectAndroid(dev, DeviceControlPrincipal)
	defer c.Close()
	if !info.Android || info.Screen {
		t.Fatalf("info %+v; want a device, not a shared console", info)
	}
	var recorded [][]byte
	c.SetTap(func(pdu []byte) { recorded = append(recorded, append([]byte(nil), pdu...)) })

	first, err := c.NextBitmaps()
	if err != nil {
		t.Fatal(err)
	}
	cols, rows := (256+screenTile-1)/screenTile, (512+screenTile-1)/screenTile
	if len(first) != cols*rows {
		t.Fatalf("the first look sent %d tiles; want the whole screen, %d", len(first), cols*rows)
	}
	if len(recorded) == 0 {
		t.Fatal("nothing was recorded before the tiles were handed on")
	}
	// A Fast-Path update PDU: the first byte's low two bits are the action
	// (0 = Fast-Path) and the length follows.
	if recorded[0][0]&3 != 0 {
		t.Errorf("recorded %x; want a Fast-Path update", recorded[0][:4])
	}

	// A tap on the first row changes that row, and only tiles there are
	// sent next. The clock bar may tick meanwhile, which is the status bar.
	if err := c.Play(AndroidInput{Type: "button", Down: true, X: 100, Y: fakeRowsTop + 10}); err != nil {
		t.Fatal(err)
	}
	next, err := c.NextBitmaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(next) == 0 || len(next) >= cols*rows {
		t.Fatalf("after a tap %d tiles were sent; want the changed row only", len(next))
	}
	inRow := 0
	for _, b := range next {
		if b.Y >= fakeRowsTop-screenTile && b.Y < fakeRowsTop+fakeRowHeight {
			inRow++
		}
	}
	if inRow == 0 {
		t.Errorf("none of the %d tiles sent after the tap is on the tapped row", len(next))
	}
	plays := dev.Plays()
	if len(plays) != 1 || plays[0].Type != "button" {
		t.Errorf("the device played %+v; want the one tap", plays)
	}

	// Refresh sends the whole screen again.
	_ = c.Refresh()
	all, err := c.NextBitmaps()
	if err != nil || len(all) != cols*rows {
		t.Fatalf("after a refresh %d tiles, %v; want the whole screen", len(all), err)
	}
}

func TestViewPlaysNothingOnTheDevice(t *testing.T) {
	dev := NewFakeAndroid(128, 256)
	c, _ := connectAndroid(dev, DeviceViewPrincipal)
	defer c.Close()
	for _, in := range []AndroidInput{
		{Type: "button", Down: true, X: 10, Y: fakeRowsTop + 10},
		{Type: "device", Name: "home"},
		{Type: "text", Text: "hello"},
		{Type: "key", Code: "Enter", Down: true},
	} {
		if err := c.Play(in); err != nil {
			t.Fatal(err)
		}
	}
	if got := dev.Plays(); len(got) != 0 {
		t.Errorf("a viewer's input reached the device: %+v", got)
	}
}

func TestTheFakePhoneAnswersTextAndKeys(t *testing.T) {
	dev := NewFakeAndroid(200, 600)
	defer dev.Close()
	before, _, _, _ := dev.Frame(time.Second)
	at := func(fb []byte, x, y int) uint32 {
		return binary.LittleEndian.Uint32(fb[(y*200+x)*4:])
	}
	// The search field is empty, then shows what was typed, then less of
	// it after Backspace, then nothing after Home.
	if err := dev.Play(AndroidInput{Type: "text", Text: "abc"}); err != nil {
		t.Fatal(err)
	}
	typed, _, changed, _ := dev.Frame(time.Second)
	if !changed || at(typed, 44, fakeSearchTop+28) == at(before, 44, fakeSearchTop+28) {
		t.Fatal("typing did not change the search field")
	}
	_ = dev.Play(AndroidInput{Type: "key", Code: "Backspace", Down: true})
	_ = dev.Play(AndroidInput{Type: "device", Name: "home"})
	home, _, _, _ := dev.Frame(time.Second)
	if at(home, 44, fakeSearchTop+28) != at(before, 44, fakeSearchTop+28) {
		t.Error("Home did not clear the search field")
	}
	// Nothing changed: the look waits and says so, and the frame is not
	// re-sent, except for the clock, which is why the wait is short.
	if _, _, changed, err := dev.Frame(10 * time.Millisecond); err != nil || changed {
		t.Errorf("a still screen reported changed=%v err=%v", changed, err)
	}
}

// scriptedActivity is a device whose only purpose is to hand the client one
// activity event, to check what the client does with it.
type scriptedActivity struct {
	*FakeAndroid
	once sync.Once
	ev   AndroidActivity
}

func (s *scriptedActivity) Activity(wait time.Duration) (AndroidActivity, bool) {
	var ev AndroidActivity
	got := false
	s.once.Do(func() { ev, got = s.ev, true })
	if got {
		return ev, true
	}
	time.Sleep(wait)
	return AndroidActivity{}, false
}

func TestTheDeviceActivityIsChainedIntoTheRecording(t *testing.T) {
	dev := NewFakeAndroid(256, 512)
	c, _ := connectAndroid(dev, DeviceControlPrincipal)
	defer c.Close()

	var mu sync.Mutex
	var notes []androidActivityNote
	c.note = func(v any) error {
		n, ok := v.(androidActivityNote)
		if !ok {
			t.Errorf("a note of type %T was written; want an android-activity note", v)
			return nil
		}
		mu.Lock()
		notes = append(notes, n)
		mu.Unlock()
		return nil
	}
	c.startNotes()

	// The fake announces the app in front first, then follows the input: a
	// lock, then a typed event.
	if err := c.Play(AndroidInput{Type: "device", Name: "lock"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Play(AndroidInput{Type: "text", Text: "hello"}); err != nil {
		t.Fatal(err)
	}

	kinds := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var ks []string
		for _, n := range notes {
			ks = append(ks, n.Kind)
		}
		return ks
	}
	waitFor(t, time.Second, func() bool {
		ks := kinds()
		return len(ks) >= 3 && ks[0] == "app" && hasStr(ks, "lock") && hasStr(ks, "typed")
	}, "the app, lock and typed events were chained")

	mu.Lock()
	for _, n := range notes {
		if n.Type != "android-activity" {
			t.Errorf("a note was tagged %q; want android-activity", n.Type)
		}
		if n.Kind == "typed" && n.Text != "hello" {
			t.Errorf("the typed event carried %q; want the text", n.Text)
		}
	}
	mu.Unlock()
}

func TestAPasswordIsNeverTypedIntoTheRecording(t *testing.T) {
	dev := &scriptedActivity{FakeAndroid: NewFakeAndroid(256, 512),
		ev: AndroidActivity{Kind: "typed", Password: true, Text: "hunter2"}}
	c, _ := connectAndroid(dev, DeviceControlPrincipal)
	defer c.Close()

	var mu sync.Mutex
	var got *androidActivityNote
	c.note = func(v any) error {
		n := v.(androidActivityNote)
		if n.Kind == "typed" {
			mu.Lock()
			got = &n
			mu.Unlock()
		}
		return nil
	}
	c.startNotes()

	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got != nil
	}, "the typed-into-a-password event was chained")

	mu.Lock()
	defer mu.Unlock()
	if !got.Password {
		t.Error("the event lost that it was a password field")
	}
	if got.Text != "" {
		t.Errorf("the password text %q reached the recording", got.Text)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func hasStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

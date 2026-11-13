//go:build darwin

package agent

import (
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

// The frameworks open and the functions screen sharing calls are all
// there, called through purego as the helper calls them.
func TestMacCoreGraphicsLoads(t *testing.T) {
	cg, err := loadCoreGraphics()
	if err != nil {
		t.Fatal(err)
	}
	d := cg.mainDisplayID()
	w, h := cg.displayPixelsWide(d), cg.displayPixelsHigh(d)
	t.Logf("main display %d: %dx%d points; screen recording allowed: %v; accessibility: %v",
		d, w, h, cg.preflightCapture(), cg.axIsProcessTrusted())
	src := cg.eventSourceCreate(cgSourceHIDState)
	if src == 0 {
		t.Fatal("no event source")
	}
	defer cg.cfRelease(src)
	key := cg.eventCreateKeyboard(src, 0x00, true)
	mouse := cg.eventCreateMouse(src, cgEventMouseMoved, cgPoint{10, 20}, 0)
	scroll := cg.eventCreate(src)
	if key == 0 || mouse == 0 || scroll == 0 {
		t.Fatalf("events not made: key %x, mouse %x, scroll %x", key, mouse, scroll)
	}
	u := uint16('é')
	cg.eventSetUnicode(key, 1, &u)
	cg.eventSetFlags(key, cgFlagShift)
	cg.eventSetType(scroll, cgEventScrollWheel)
	cg.eventSetLocation(scroll, cgPoint{10, 20})
	cg.eventSetInt(scroll, cgFieldScrollAxis1, -3)
	for _, ev := range []uintptr{key, mouse, scroll} {
		cg.cfRelease(ev)
	}
}

// Where this runner lets it record the screen, frames come, whole first
// and then only where the screen changed.
func TestMacCapturesTheDisplay(t *testing.T) {
	cg, err := loadCoreGraphics()
	if err != nil {
		t.Fatal(err)
	}
	if !cg.preflightCapture() {
		t.Skip("this runner has not allowed screen recording")
	}
	c, err := newMacCapturer(cg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	shot, changed, err := c.Capture(5 * time.Second)
	if err != nil || !changed {
		t.Fatalf("no first frame: changed %v, %v", changed, err)
	}
	if shot.dirty != nil || shot.width != c.width || len(shot.fb) != shot.stride*shot.height {
		t.Fatalf("first frame %dx%d, dirty %v", shot.width, shot.height, shot.dirty)
	}
	// Moving the pointer changes the screen where it was and is.
	if cg.axIsProcessTrusted() {
		in := newMacInputer(cg)
		defer in.Close()
		_ = in.Input(rdp.InputEvent{Kind: "mouse", X: shot.width / 2, Y: shot.height / 2})
		_ = in.Input(rdp.InputEvent{Kind: "mouse", X: shot.width/2 + 40, Y: shot.height/2 + 40})
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			s, changed, err := c.Capture(500 * time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Logf("the pointer's move came back as %v", s.dirty)
				return
			}
		}
		t.Fatal("moving the pointer changed nothing on the screen")
	}
}

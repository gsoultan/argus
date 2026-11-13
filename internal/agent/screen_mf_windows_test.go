package agent

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Microsoft's encoder, where the host has it: a key frame with its
// parameter sets when asked, then a picture for every screen put in, none
// held back. ARGUS_TEST_H264=require fails rather than skips without it,
// for a runner that installed Media Foundation.
func TestMediaFoundationEncodesTheScreen(t *testing.T) {
	const w, h = 640, 360
	v, err := newMFEncoder(w, h)
	if err != nil {
		if os.Getenv("ARGUS_TEST_H264") == "require" {
			t.Fatalf("no H.264 encoder: %v", err)
		}
		if strings.Contains(err.Error(), "Media Foundation") {
			t.Skipf("no H.264 encoder here: %v", err)
		}
		t.Fatal(err)
	}
	defer v.Close()

	fb := make([]byte, w*h*4)
	for i := range fb {
		fb[i] = byte(i / 7)
	}
	shot := screenShot{fb: fb, stride: w * 4, width: w, height: h}
	key, err := v.Encode(shot, true)
	if err != nil {
		t.Fatal(err)
	}
	if !h264HasIDR(key) || !h264HasNAL(key, 7) {
		t.Fatalf("the first picture (%d bytes) is not a key frame with its parameter sets", len(key))
	}
	t.Logf("key frame: %d bytes", len(key))
	for i := range 5 {
		for j := 0; j < w*4*40; j++ {
			fb[(i*40*w*4+j)%len(fb)] ^= 0x5A
		}
		p, err := v.Encode(shot, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) == 0 {
			t.Fatalf("picture %d was held back", i+2)
		}
		if h264HasIDR(p) {
			t.Errorf("picture %d is a key frame nobody asked for", i+2)
		}
		t.Logf("picture %d: %d bytes", i+2, len(p))
	}
	again, err := v.Encode(shot, true)
	if err != nil {
		t.Fatal(err)
	}
	if !h264HasIDR(again) {
		t.Error("a key frame asked for again did not come")
	}
}

// The encoder's bitrate follows the link while it runs: held to a tenth,
// the same moving screen costs far less, with no new encoder and no key
// frame. If an encoder ignored the change, motion on a slow link would stay
// as large as on a fast one, and late.
func TestMediaFoundationBitrateFollowsTheLink(t *testing.T) {
	const w, h = 1280, 720
	v, err := newMFEncoder(w, h)
	if err != nil {
		if os.Getenv("ARGUS_TEST_H264") == "require" {
			t.Fatalf("no H.264 encoder: %v", err)
		}
		if strings.Contains(err.Error(), "Media Foundation") {
			t.Skipf("no H.264 encoder here: %v", err)
		}
		t.Fatal(err)
	}
	defer v.Close()
	rater, ok := v.(screenRater)
	if !ok {
		t.Fatal("the encoder takes no bitrate")
	}

	fb := make([]byte, w*h*4)
	shot := screenShot{fb: fb, stride: w * 4, width: w, height: h}
	seed := uint32(1)
	// run encodes n pictures of a screen that is all new each time, a
	// thirtieth of a second apart, and returns the bytes of the last half.
	run := func(n int) int {
		total := 0
		for i := range n {
			for j := range fb {
				seed = seed*1664525 + 1013904223
				fb[j] = byte(seed >> 24)
			}
			time.Sleep(time.Second / 30)
			p, err := v.Encode(shot, false)
			if err != nil {
				t.Fatal(err)
			}
			if i > 0 && h264HasIDR(p) {
				t.Fatalf("picture %d is a key frame nobody asked for", i)
			}
			if i >= n/2 {
				total += len(p)
			}
		}
		return total
	}
	full := run(30)
	rater.SetBitrate(screenMotionBits(w, h) / 10)
	held := run(60)
	t.Logf("last 15 pictures at full rate: %d bytes; last 30 held to a tenth: %d bytes", full, held)
	// Twice as many pictures, at a tenth of the rate: a fifth if it were
	// exact. Under half says the encoder followed.
	if held >= full/2 {
		t.Fatalf("held to a tenth of the rate the pictures came to %d bytes, against %d at full rate: "+
			"the encoder did not follow", held, full)
	}
}

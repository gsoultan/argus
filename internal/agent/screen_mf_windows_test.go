package agent

import (
	"os"
	"strings"
	"testing"
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

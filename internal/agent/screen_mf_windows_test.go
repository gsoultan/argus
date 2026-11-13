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
	// The screen is mid grey with faint noise over it, a few shades either
	// way and new every picture. Loud noise is no measure: the encoder's
	// coarsest picture of it is as large as the full rate allows, so held
	// or not the pictures came out the same size (29 kB for a screen of
	// it, 13 kB for a band, 15 kB for blocks of flat shades). Faint noise
	// costs all the encoder is given at a fine setting and next to
	// nothing at a coarse one, which rounds it away.
	paint := func() {
		for j := 0; j < len(fb); j += 4 {
			seed = seed*1664525 + 1013904223
			shade := byte(120 + seed>>28)
			fb[j], fb[j+1], fb[j+2], fb[j+3] = shade, shade, shade, 0xFF
		}
	}
	// run encodes n pictures, the screen all new each time, a
	// thirtieth of a second apart, and returns the bytes of the last half.
	var sizes []int
	run := func(n int) int {
		total := 0
		sizes = sizes[:0]
		for i := range n {
			paint()
			time.Sleep(time.Second / 30)
			p, err := v.Encode(shot, false)
			if err != nil {
				t.Fatal(err)
			}
			if i > 0 && h264HasIDR(p) {
				t.Fatalf("picture %d is a key frame nobody asked for", i)
			}
			sizes = append(sizes, len(p))
			if i >= n/2 {
				total += len(p)
			}
		}
		return total
	}
	full := run(30) / 15
	rater.SetBitrate(screenMotionBits(w, h) / 10)
	t.Logf("pictures at full rate: %v", sizes)
	// The encoder takes its time over a new rate, and does not settle on
	// it. A runner's went on at the old one for 51 pictures (20 kB each),
	// fell to under 1 kB over the next forty, then spent what it had saved
	// on a few of up to 60 kB before falling again. So what is asked is
	// that it turned: that some ten pictures in a row, of 120, average
	// under a quarter of a picture at the full rate. An encoder the new
	// rate never reached leaves every run of ten as large as before.
	run(120)
	t.Logf("pictures held to a tenth: %v", sizes)
	held := 0
	for i := 0; i+10 <= len(sizes); i++ {
		sum := 0
		for _, n := range sizes[i : i+10] {
			sum += n
		}
		if i == 0 || sum/10 < held {
			held = sum / 10
		}
	}
	t.Logf("a picture at full rate: %d bytes; the smallest ten in a row held to a tenth: %d bytes", full, held)
	if held >= full/4 {
		t.Fatalf("held to a tenth of the rate a picture came to %d bytes, against %d at full rate: "+
			"the encoder did not follow", held, full)
	}
}

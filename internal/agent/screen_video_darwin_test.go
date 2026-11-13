//go:build darwin

package agent

import "testing"

// VideoToolbox encodes a screen through purego: a key frame when asked,
// with its SPS and PPS, then a picture that is not one.
func TestMacVideoToolboxEncodesTheScreen(t *testing.T) {
	const w, h = 640, 480
	v, err := newMacVideo(w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	s := screenShot{fb: make([]byte, w*h*4), stride: w * 4, width: w, height: h}
	for i := range s.fb {
		s.fb[i] = byte(i * 7)
	}
	key, err := v.Encode(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if !h264HasIDR(key) || !h264HasNAL(key, 7) || !h264HasNAL(key, 8) {
		t.Fatalf("the key frame (%d bytes) has no IDR, SPS or PPS", len(key))
	}
	for i := 0; i < w*4*40; i++ {
		s.fb[i] ^= 0xFF
	}
	next, err := v.Encode(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) == 0 || h264HasIDR(next) {
		t.Fatalf("the next picture is %d bytes, key %v; want a small one that is not a key frame", len(next), h264HasIDR(next))
	}
	t.Logf("key frame %d bytes, next %d bytes", len(key), len(next))
}

package recorder

import (
	"bytes"
	"encoding/base64"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/gsoultan/argus/internal/rdp"
)

// desktopSample is a desktop recording made the way the agent makes one:
// a screen of text over a gradient, typed into and scrolled, its changed
// 64x64 tiles through the real tile encoder; input; and an event.
func desktopSample(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	r, err := rdp.NewRecorder(&buf, rdp.Header{Width: 640, Height: 384, Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	const w, h, tile = 640, 384, 64
	rng := rand.New(rand.NewSource(1))
	var glyphs [32][12][7]byte
	for g := range glyphs {
		for y := 1; y < 11; y++ {
			for x := 1; x < 6; x++ {
				glyphs[g][y][x] = byte(rng.Intn(3)/2) * 200
			}
		}
	}
	text := make([]int, 80*30)
	for i := range text {
		text[i] = rng.Intn(len(glyphs))
	}
	fb := make([]byte, w*h*4)
	draw := func(scroll int) {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				o := (y*w + x) * 4
				fb[o], fb[o+1], fb[o+2] = byte(40+x/8), byte(30+y/6), 60
				row, col := y/14+scroll, x/8
				if gy, gx := y%14, x%8; gy < 12 && gx < 7 && row < 30 && col < 80 {
					if v := glyphs[text[row*80+col]][gy][gx]; v > 0 {
						fb[o], fb[o+1], fb[o+2] = v, v, v
					}
				}
			}
		}
	}
	var scratch []byte
	for frame := 0; frame < 120; frame++ {
		text[(frame*7)%len(text)] = rng.Intn(len(glyphs))
		draw(frame / 40)
		for ty := 0; ty < h; ty += tile {
			for tx := 0; tx < w; tx += tile {
				// The first frame whole, then the band being typed into.
				if frame > 0 && (ty/tile != (frame*7)%len(text)/80*14/tile) {
					continue
				}
				b := rdp.CompressRect(scratch, fb, w*4, tx, ty, tile, tile)
				scratch = b.Data[:0]
				if err := r.Write(rdp.ServerOutput, rdp.EncodeBitmap(b)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if frame%10 == 0 {
			_ = r.Write(rdp.ClientInput, []byte{1, 2, byte(frame)})
			_ = r.Event(map[string]string{"kind": "clipboard", "text": "copied"})
		}
	}
	// The chain's head, as the session was sealed with.
	v, err := Verify(bytes.NewReader(buf.Bytes()), "")
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), v.Head
}

func zstdPlain(t *testing.T, z []byte) []byte {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A desktop recording is packed inside the zstd, reads back to exactly the
// bytes it was, verifies to the head it was sealed with at either level,
// and is smaller than the same recording compressed as it is.
func TestADesktopRecordingIsPackedAndReadsBackExactly(t *testing.T) {
	plain, head := desktopSample(t)
	for _, lvl := range []Level{Hot, Archive} {
		var z bytes.Buffer
		if err := Compress(&z, bytes.NewReader(plain), lvl); err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(zstdPlain(t, z.Bytes()), packedMagic) {
			t.Fatalf("level %d: a desktop recording was not packed", lvl)
		}
		r, err := Open(bytes.NewReader(z.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("level %d: read back %d bytes of %d, err %v", lvl, len(got), len(plain), err)
		}
		if v, err := Verify(bytes.NewReader(z.Bytes()), head); err != nil || !v.OK {
			t.Fatalf("level %d: %+v %v", lvl, v, err)
		}

		var unpacked bytes.Buffer
		zw, _ := zstd.NewWriter(&unpacked, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
		_, _ = zw.Write(plain)
		_ = zw.Close()
		t.Logf("level %d: %d -> %d packed, %d as it is", lvl, len(plain), z.Len(), unpacked.Len())
		if z.Len() >= unpacked.Len() {
			t.Errorf("level %d: packed %d bytes is no smaller than %d", lvl, z.Len(), unpacked.Len())
		}
	}

	// One altered pixel inside the packing is still caught.
	var bad bytes.Buffer
	lines := bytes.Split(plain, []byte("\n"))
	_, _, raw, ok := splitFrame(lines[5], nil)
	if !ok {
		t.Fatalf("line 5 does not pack: %.60s", lines[5])
	}
	raw[len(raw)/2] ^= 1
	altered := bytes.Replace(plain, lines[5][strings.Index(string(lines[5]), `"s","`)+5:len(lines[5])-2],
		[]byte(base64.StdEncoding.EncodeToString(raw)), 1)
	_ = Compress(&bad, bytes.NewReader(altered), Hot)
	if v, _ := Verify(bytes.NewReader(bad.Bytes()), head); v.OK {
		t.Fatal("an altered screen update inside the packing verified")
	}
}

// A terminal recording has nothing to pack, and stays a zstd file the zstd
// tool alone reads back.
func TestATerminalRecordingIsNotPacked(t *testing.T) {
	plain, _ := sample(t)
	var z bytes.Buffer
	if err := Compress(&z, bytes.NewReader(plain), Hot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(zstdPlain(t, z.Bytes()), plain) {
		t.Fatal("a terminal recording was changed inside the zstd")
	}
}

// Every line comes back as it was, whether it packs or not: non-canonical
// base64, a line that only looks like a frame, a line longer than one
// piece, blank lines, and a last line with no newline.
func TestPackingKeepsEveryLineAsItWas(t *testing.T) {
	long := `[1.5,"s","` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7, 8, 9}, packChunk/2)) + `"]`
	cases := map[string]string{
		"frames":         "{\"version\":1}\n[0.1,\"s\",\"AQID\"]\n[0.2,\"c\",\"AQ==\"]\n",
		"no newline":     "{\"version\":1}\n[0.1,\"s\",\"AQID\"]\n[0.2,\"s\",\"AQID\"]",
		"non-canonical":  "{}\n[0.1,\"s\",\"AQID\"]\n[0.1,\"s\",\"AR==\"]\n",
		"not a frame":    "{}\n[0.1,\"s\",\"AQID\"]\n[0.1,\"o\",\"AQID\"]\n[0.1,\"s\",\"AQID\",1]\n[0.1, \"s\",\"AQID\"]\n[\"s\",\"AQID\"]\n",
		"blank lines":    "{}\n\n[0.1,\"s\",\"AQID\"]\n\n\n",
		"long line":      "{}\n[0.1,\"s\",\"AQID\"]\n" + long + "\n" + long,
		"not base64":     "{}\n[0.1,\"s\",\"AQID\"]\n[0.1,\"s\",\"A Q I D\"]\n[0.1,\"s\",\"\"]\n[0.1,\"s\",\"AQ\\r\\nID\"]\n[0.1,\"s\",\"AQ\rID\"]\n",
		"escaped quotes": "{}\n[0.1,\"s\",\"AQID\"]\n[\"a\\\",\\\"s\\\",\\\"\",\"s\",\"AQID\"]\n",
	}
	for name, in := range cases {
		var z bytes.Buffer
		if err := Compress(&z, strings.NewReader(in), Hot); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.HasPrefix(zstdPlain(t, z.Bytes()), packedMagic) {
			t.Fatalf("%s: not packed", name)
		}
		r, err := Open(bytes.NewReader(z.Bytes()))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != in {
			t.Fatalf("%s: read back %q (err %v)", name, truncate(got), err)
		}
	}
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// A packed stream that is cut short, names a record it does not know, or
// claims a record past the bound is an error, not a short recording or an
// allocation.
func TestABrokenPackedStreamIsAnError(t *testing.T) {
	var whole bytes.Buffer
	_ = Compress(&whole, strings.NewReader("{}\n[0.1,\"s\",\"AQID\"]\n"), Hot)
	inner := zstdPlain(t, whole.Bytes())
	cases := map[string][]byte{
		"cut short":      inner[:len(inner)-2],
		"unknown record": append(append([]byte(nil), packedMagic...), 0x07, 0x00),
		"over the bound": append(append([]byte(nil), packedMagic...), 0x00, 0xff, 0xff, 0xff, 0xff, 0x0f),
		"unknown stream": append(append([]byte(nil), packedMagic...), 0x01, 0x01, '1', 'x', 0x01, 0x01, '\n'),
	}
	for name, b := range cases {
		var z bytes.Buffer
		zw, _ := zstd.NewWriter(&z)
		_, _ = zw.Write(b)
		_ = zw.Close()
		r, err := Open(bytes.NewReader(z.Bytes()))
		if err == nil {
			_, err = io.ReadAll(r)
		}
		if err == nil {
			t.Errorf("%s: read without an error", name)
		}
	}
}

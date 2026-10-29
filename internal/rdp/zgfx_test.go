package rdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"
)

// zgfxWriter is a compressor for tests: it emits every kind of token the
// decompressor reads, so a round trip exercises each path. It is not how
// Windows chooses tokens, only a legal encoding of the same format.
type zgfxWriter struct {
	out  []byte
	acc  byte
	used int
}

func (w *zgfxWriter) bits(v, n int) {
	for i := n - 1; i >= 0; i-- {
		w.acc = w.acc<<1 | byte(v>>i&1)
		w.used++
		if w.used == 8 {
			w.out = append(w.out, w.acc)
			w.acc, w.used = 0, 0
		}
	}
}

func (w *zgfxWriter) token(t zgfxToken) { w.bits(t.code, t.bits) }

func (w *zgfxWriter) literal(b byte) {
	for _, t := range zgfxTokens {
		if !t.match && t.valueBits == 0 && t.base == int(b) {
			w.token(t)
			return
		}
	}
	w.token(zgfxTokens[0])
	w.bits(int(b), 8)
}

func (w *zgfxWriter) match(distance, count int) {
	for _, t := range zgfxTokens {
		if t.match && distance >= t.base && distance < t.base+1<<t.valueBits && (t.base > 0 || distance > 0) {
			w.token(t)
			w.bits(distance-t.base, t.valueBits)
			break
		}
	}
	if count == 3 {
		w.bits(0, 1)
		return
	}
	lo, extra := 4, 2
	w.bits(1, 1)
	for count >= lo*2 {
		w.bits(1, 1)
		lo *= 2
		extra++
	}
	w.bits(0, 1)
	w.bits(count-lo, extra)
}

func (w *zgfxWriter) raw(p []byte) {
	w.token(zgfxTokens[1]) // distance base 0
	w.bits(0, 5)
	w.bits(len(p), 15)
	if w.used > 0 {
		w.bits(0, 8-w.used)
	}
	w.out = append(w.out, p...)
}

// segment ends the bit stream with its padding count, as a server does.
func (w *zgfxWriter) segment() []byte {
	pad := 0
	if w.used > 0 {
		pad = 8 - w.used
		w.bits(0, pad)
	}
	return append([]byte{zgfxTypeRDP8 | zgfxCompressed}, append(w.out, byte(pad))...)
}

func TestZGFXRoundTrip(t *testing.T) {
	var w zgfxWriter
	var want []byte
	put := func(b ...byte) { want = append(want, b...) }

	for _, b := range []byte("hello, \x00\x01\xff\x80 world") {
		w.literal(b)
		put(b)
	}
	// A run: a match overlapping what it writes.
	w.literal('a')
	put('a')
	w.match(1, 40)
	put(bytes.Repeat([]byte{'a'}, 40)...)
	// A match back to the start, with each length form.
	for _, n := range []int{3, 4, 7, 8, 15, 16, 100} {
		w.match(len(want), n)
		put(want[:n]...)
	}
	w.raw([]byte("raw bytes, aligned"))
	put([]byte("raw bytes, aligned")...)
	w.literal('!')
	put('!')

	z := NewDecompressor()
	got, err := z.Decompress(append([]byte{zgfxSingle}, w.segment()...))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	// The history carries over: a later message may point into this one.
	var w2 zgfxWriter
	w2.match(len(want), 5)
	got, err = z.Decompress(append([]byte{zgfxSingle}, w2.segment()...))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("a match into the previous message gave %q", got)
	}
}

func TestZGFXMultipartAndUncompressed(t *testing.T) {
	var w zgfxWriter
	w.literal('b')
	w.match(1, 9)
	seg1 := w.segment()
	seg2 := append([]byte{zgfxTypeRDP8}, "plain"...)

	msg := []byte{zgfxMultipart}
	msg = binary.LittleEndian.AppendUint16(msg, 2)
	msg = binary.LittleEndian.AppendUint32(msg, 15)
	for _, s := range [][]byte{seg1, seg2} {
		msg = binary.LittleEndian.AppendUint32(msg, uint32(len(s)))
		msg = append(msg, s...)
	}
	got, err := NewDecompressor().Decompress(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bbbbbbbbbbplain" {
		t.Fatalf("got %q", got)
	}
}

func TestZGFXLargeRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var w zgfxWriter
	var want []byte
	for len(want) < 200000 {
		switch {
		case len(want) > 10 && r.IntN(3) == 0:
			d := 1 + r.IntN(len(want))
			n := 3 + r.IntN(300)
			w.match(d, n)
			for range n {
				want = append(want, want[len(want)-d])
			}
		default:
			b := byte(r.IntN(256))
			w.literal(b)
			want = append(want, b)
		}
	}
	got, err := NewDecompressor().Decompress(append([]byte{zgfxSingle}, w.segment()...))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("a long stream did not round-trip")
	}
}

func TestZGFXRefusesHostileInput(t *testing.T) {
	var w zgfxWriter
	w.literal('x')
	w.match(50, 3) // further back than anything written
	for name, msg := range map[string][]byte{
		"empty":           {},
		"descriptor":      {0x42},
		"match too far":   append([]byte{zgfxSingle}, w.segment()...),
		"compression":     {zgfxSingle, 0x01, 'a'},
		"padding":         {zgfxSingle, zgfxTypeRDP8 | zgfxCompressed, 0x00, 9},
		"multipart short": {zgfxMultipart, 1, 0},
		"multipart size":  {zgfxMultipart, 0, 0, 0xFF, 0xFF, 0xFF, 0x7F},
	} {
		if _, err := NewDecompressor().Decompress(msg); !errors.Is(err, ErrZGFX) {
			t.Errorf("%s: got %v, want ErrZGFX", name, err)
		}
	}
}

func FuzzZGFX(f *testing.F) {
	var w zgfxWriter
	w.literal('a')
	w.match(1, 20)
	f.Add(append([]byte{zgfxSingle}, w.segment()...))
	f.Fuzz(func(t *testing.T, in []byte) {
		_, _ = NewDecompressor().Decompress(in)
	})
}

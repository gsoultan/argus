package recorder

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The line is hashed into the chain, so the hand-written encoder must produce
// the bytes json.Marshal did, or recordings would stop verifying the way they
// always have. Held to that across the awkward cases and under fuzzing.

func marshalEvent(t float64, stream string, data []byte) []byte {
	b, err := json.Marshal([]any{round3(t), stream, string(data)})
	if err != nil {
		panic(err)
	}
	return b
}

func TestAppendEventMatchesEncodingJSON(t *testing.T) {
	cases := []string{
		"", "a", "plain text", "\r\n", "\x1b[01;34mdir\x1b[0m",
		"<script>&amp;</script>", `"quoted" \ back\slash`,
		"\x00\x01\x07\x08\x0b\x0c\x1f\x7f", "\t tab", "ünïcödé ✓ 日本",
		"\xff\xfe invalid \xc3", "\u2028\u2029", "\xe2\x80", "😀 \xf0\x9f\x98",
	}
	times := []float64{0, 0.001, 0.0004, 0.0005, 1.5, 12.345, 59.9999, 3600, 86400.123, 1e7}
	for _, c := range cases {
		for _, tm := range times {
			want := marshalEvent(tm, "o", []byte(c))
			got := appendEvent(nil, round3(tm), "o", []byte(c))
			if !bytes.Equal(got, want) {
				t.Errorf("t=%v %q:\n got %s\nwant %s", tm, c, got, want)
			}
		}
	}
}

func FuzzAppendEventMatchesEncodingJSON(f *testing.F) {
	f.Add(1.25, []byte("hello <world> & \xff"))
	f.Add(0.0, []byte{})
	f.Fuzz(func(t *testing.T, tm float64, data []byte) {
		if tm < 0 || tm > 1e12 || tm != tm {
			t.Skip()
		}
		want := marshalEvent(tm, "i", data)
		got := appendEvent(nil, round3(tm), "i", data)
		if !bytes.Equal(got, want) {
			t.Fatalf("got %s\nwant %s", got, want)
		}
	})
}

// A recording written by this encoder verifies against the head it reports,
// and so does one read back from disk -- the property everything else rests on.
func TestRecordingStillVerifies(t *testing.T) {
	var buf bytes.Buffer
	r, err := New(&buf, Header{Width: 80, Height: 24, Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		_ = r.Write(Output, []byte("line <&> \xff\r\n"))
		_ = r.Write(Input, []byte{byte(i)})
	}
	_ = r.Resize(120, 40)
	_ = r.Exec(map[string]any{"argv": []string{"ls", "-la"}})
	head, err := r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if v, err := Verify(bytes.NewReader(buf.Bytes()), head); err != nil || !v.OK {
		t.Fatalf("verify: %+v %v", v, err)
	}
}

package tunnel

import (
	"bytes"
	"errors"
	"testing"
)

// An answer past one frame crosses as parts and is joined back byte for
// byte; one that fits stays a single frame, as older readers expect.
func TestAnswerInParts(t *testing.T) {
	for _, n := range []int{10, MaxFrame, MaxFrame + 1, 3*MaxFrame + 17} {
		want := bytes.Repeat([]byte("x"), n)
		var buf bytes.Buffer
		if err := WriteAnswer(&buf, want, true); err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		fr := NewFrameReader(&buf)
		var j AnswerJoiner
		frames := 0
		for {
			typ, p, err := fr.Next()
			if err != nil {
				t.Fatalf("%d bytes: %v", n, err)
			}
			frames++
			if typ == FramePGAnswerPart {
				if err := j.Part(p); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if typ != FramePGAnswer {
				t.Fatalf("frame %q", typ)
			}
			got, err := j.Answer(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%d bytes came back as %d", n, len(got))
			}
			break
		}
		if wantFrames := (n + MaxFrame - 1) / MaxFrame; frames != wantFrames {
			t.Fatalf("%d bytes took %d frames, want %d", n, frames, wantFrames)
		}
	}
}

// Without parts, or past MaxAnswer, nothing is written: the caller answers
// with an error instead of a frame that would end the session.
func TestAnswerTooLarge(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteAnswer(&buf, make([]byte, MaxFrame+1), false); !errors.Is(err, ErrAnswerTooLarge) || buf.Len() != 0 {
		t.Fatalf("past a frame without parts: %v, %d bytes written", err, buf.Len())
	}
	if err := WriteAnswer(&buf, make([]byte, MaxAnswer+1), true); !errors.Is(err, ErrAnswerTooLarge) || buf.Len() != 0 {
		t.Fatalf("past MaxAnswer: %v, %d bytes written", err, buf.Len())
	}
	var j AnswerJoiner
	for i := 0; i < MaxAnswer/MaxFrame; i++ {
		if err := j.Part(make([]byte, MaxFrame)); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Part(make([]byte, MaxFrame)); !errors.Is(err, ErrAnswerTooLarge) {
		t.Fatalf("a joiner kept parts past MaxAnswer: %v", err)
	}
}

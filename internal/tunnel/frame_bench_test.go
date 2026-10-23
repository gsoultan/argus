package tunnel

import (
	"io"
	"testing"
)

// The agent writes one output frame per read of the terminal, so this runs
// once per 32 KB a session prints.
func BenchmarkWriteFrame(b *testing.B) {
	p := make([]byte, 32<<10)
	b.SetBytes(int64(len(p)))
	b.ReportAllocs()
	for b.Loop() {
		if err := WriteFrame(io.Discard, FrameOutput, p); err != nil {
			b.Fatal(err)
		}
	}
}

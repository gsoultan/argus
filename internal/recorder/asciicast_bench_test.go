package recorder

import (
	"bytes"
	"io"
	"testing"
)

// The recorder sits on every byte a session prints: the agent records each
// chunk of terminal output before it forwards it. These measure what one
// chunk costs, at the sizes a terminal actually produces -- a keystroke echo,
// a line of output, and a full read buffer from `cat` of a large file.

func benchmarkWrite(b *testing.B, size int) {
	chunk := bytes.Repeat([]byte("ls -la /var/log\r\n\x1b[0m<b>&</b>"), size/29+1)[:size]
	r, err := New(io.Discard, Header{Width: 80, Height: 24, Timestamp: 1})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Write(Output, chunk); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteKeystroke(b *testing.B) { benchmarkWrite(b, 1) }
func BenchmarkWriteLine(b *testing.B)      { benchmarkWrite(b, 80) }
func BenchmarkWriteBulk(b *testing.B)      { benchmarkWrite(b, 32<<10) }

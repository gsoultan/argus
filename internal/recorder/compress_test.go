package recorder

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	r, err := New(&buf, Header{Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		_ = r.Write(Output, []byte("Nov  3 14:52:06 web-01 nginx[812]: GET /healthz 200 0.001\r\n"))
		_ = r.Write(Input, []byte("q"))
	}
	head, err := r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), head
}

// The chain covers the lines, not the file: a recording verifies to the
// same head compressed at either level, and as the gzip older agents left
// on hosts. Changing one line inside the compression is still caught.
func TestACompressedRecordingVerifiesToTheSameHead(t *testing.T) {
	plain, head := sample(t)
	for _, lvl := range []Level{Hot, Archive} {
		var z bytes.Buffer
		if err := Compress(&z, bytes.NewReader(plain), lvl); err != nil {
			t.Fatal(err)
		}
		if !Compressed(z.Bytes()) {
			t.Fatal("not recognised as compressed")
		}
		v, err := Verify(bytes.NewReader(z.Bytes()), head)
		if err != nil || !v.OK {
			t.Fatalf("level %d: %+v %v", lvl, v, err)
		}
		t.Logf("level %d: %d -> %d bytes (%.0fx)", lvl, len(plain), z.Len(), float64(len(plain))/float64(z.Len()))
	}

	var g bytes.Buffer
	gw := gzip.NewWriter(&g)
	_, _ = gw.Write(plain)
	_ = gw.Close()
	if v, err := Verify(bytes.NewReader(g.Bytes()), head); err != nil || !v.OK {
		t.Fatalf("gzip: %+v %v", v, err)
	}

	var bad bytes.Buffer
	_ = Compress(&bad, bytes.NewReader(bytes.Replace(plain, []byte("200"), []byte("500"), 1)), Hot)
	if v, _ := Verify(bytes.NewReader(bad.Bytes()), head); v.OK {
		t.Fatal("an altered line inside the compression verified")
	}

	// Plain recordings still read as they are.
	r, _ := Open(bytes.NewReader(plain))
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, plain) {
		t.Fatal("a plain recording was changed by Open")
	}
}

// CompressFile replaces the plain file with one that reads back to the same
// bytes, and leaves a compressed one alone.
func TestCompressFileReplacesThePlainCopy(t *testing.T) {
	plain, head := sample(t)
	p := filepath.Join(t.TempDir(), "s.cast")
	if err := os.WriteFile(p, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := CompressFile(p, Hot)
	if err != nil {
		t.Fatal(err)
	}
	if out != p+CompressedExt {
		t.Fatalf("compressed to %s", out)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("the plain copy is still there")
	}
	f, _ := os.Open(out)
	v, err := Verify(f, head)
	_ = f.Close()
	if err != nil || !v.OK {
		t.Fatalf("%+v %v", v, err)
	}
	if again, err := CompressFile(out, Hot); err != nil || again != out {
		t.Fatalf("compressing a compressed file: %s %v", again, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Errorf("left behind %d files", len(entries))
	}
}

// A hostile stream cannot make a reader allocate a window past the bound.
func TestOpenRefusesAnOversizedWindow(t *testing.T) {
	// A zstd frame header declaring a 2 GB window (window descriptor 0xF8:
	// exponent 31) and no content.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0xf8, 0x01, 0x00, 0x00}
	r, err := Open(bytes.NewReader(frame))
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "window") {
		t.Fatalf("err = %v, want a window refusal", err)
	}
}

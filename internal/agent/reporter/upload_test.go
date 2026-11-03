package reporter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gsoultan/argus/internal/recorder"
)

// A recording kept compressed on the host is sent compressed only to a
// control plane that said it reads zstd. Sent to one that has not, it goes
// decompressed: an older control plane would check the compressed bytes as
// lines, find they do not match the head, and refuse the evidence for good.
func TestACompressedRecordingShipsPlainUntilTheControlPlaneReadsZstd(t *testing.T) {
	plain := []byte("{\"version\":2}\n[0.1,\"o\",\"$ id\\r\\n\"]\n")
	var z bytes.Buffer
	if err := recorder.Compress(&z, bytes.NewReader(plain), recorder.Hot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s1.cast"+recorder.CompressedExt)
	if err := os.WriteFile(path, z.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []byte
	var format, encoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		format, encoding = r.Header.Get("X-Argus-Format"), r.Header.Get("X-Argus-Encoding")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", filepath.Join(t.TempDir(), "spool"), nil)

	if err := c.UploadRecording(context.Background(), "s1", "head", path); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) || encoding != "" || format != "cast" {
		t.Errorf("before zstd was offered: %d bytes, encoding %q, format %q", len(got), encoding, format)
	}

	c.SetShipZstd(true)
	if err := c.UploadRecording(context.Background(), "s1", "head", path); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, z.Bytes()) || encoding != "zstd" || format != "cast" {
		t.Errorf("once offered: %d bytes, encoding %q, format %q", len(got), encoding, format)
	}
}

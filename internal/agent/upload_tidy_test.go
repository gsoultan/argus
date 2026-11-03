package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/agent/reporter"
	"github.com/gsoultan/argus/internal/recorder"
)

// A shipped recording is compressed after an hour and deleted after Keep; one
// that never reached the control plane is never touched, however old.
func TestShippedRecordingsAreCompressedThenExpired(t *testing.T) {
	state := t.TempDir()

	// A real recording, so the compressed copy can be shown to verify.
	var buf bytes.Buffer
	r, err := recorder.New(&buf, recorder.Header{Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		_ = r.Write(recorder.Output, []byte("drwxr-xr-x 2 root root 4096 Nov  2 07:28 bin\r\n"))
	}
	head, _ := r.Close()
	shipped := filepath.Join(state, "shipped.cast")
	stuck := filepath.Join(state, "stuck.cast")
	_ = os.WriteFile(shipped, buf.Bytes(), 0o600)
	_ = os.WriteFile(stuck, buf.Bytes(), 0o600)

	send := &fakeSender{}
	u := NewUploads(state, send, nil)
	u.Keep = 48 * time.Hour
	u.Queue(SessionRecord{ID: "shipped", Path: shipped, ChainHead: head})
	u.Pass(context.Background())
	send.err = &reporter.UploadError{Status: http.StatusServiceUnavailable}
	u.Queue(SessionRecord{ID: "stuck", Path: stuck, ChainHead: head})
	u.Pass(context.Background())

	now := time.Now()
	if c, rm := u.Tidy(now); c != 0 || rm != 0 {
		t.Fatalf("a recording shipped moments ago was tidied: %d compressed, %d removed", c, rm)
	}
	if c, _ := u.Tidy(now.Add(2 * time.Hour)); c != 1 {
		t.Fatalf("compressed %d, want 1", c)
	}
	if _, err := os.Stat(shipped); !os.IsNotExist(err) {
		t.Error("the uncompressed copy is still there")
	}
	gz, err := os.Open(shipped + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := gz.Stat()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	_ = gz.Close()
	if !bytes.Equal(plain, buf.Bytes()) {
		t.Fatal("the compressed copy does not give back the recording's bytes")
	}
	if v, err := recorder.Verify(bytes.NewReader(plain), head); err != nil || !v.OK {
		t.Fatalf("the compressed copy no longer verifies: %+v %v", v, err)
	}
	t.Logf("recording %d bytes, compressed %d (%.1fx)", len(plain), fi.Size(),
		float64(len(plain))/float64(fi.Size()))
	if fi.Size()*4 > int64(len(plain)) {
		t.Errorf("compressed to %d of %d bytes", fi.Size(), len(plain))
	}

	if _, rm := u.Tidy(now.Add(49 * time.Hour)); rm != 1 {
		t.Fatalf("removed %d, want 1", rm)
	}
	if _, err := os.Stat(shipped + ".gz"); !os.IsNotExist(err) {
		t.Error("an expired shipped recording is still on the host")
	}
	// The one the control plane never had is exactly as it was.
	if b, err := os.ReadFile(stuck); err != nil || !bytes.Equal(b, buf.Bytes()) {
		t.Fatal("a recording that was never shipped was changed")
	}
	if c, rm := u.Tidy(now.Add(10 * 365 * 24 * time.Hour)); c+rm != 0 {
		t.Fatal("tidy touched something that was never shipped")
	}
}

// Keep zero keeps shipped recordings for ever, compressed.
func TestKeepZeroNeverDeletes(t *testing.T) {
	state := t.TempDir()
	rec := filepath.Join(state, "a.cast")
	_ = os.WriteFile(rec, []byte(strings.Repeat("[0.1,\"o\",\"x\"]\n", 100)), 0o600)
	u := NewUploads(state, &fakeSender{}, nil)
	u.Keep = 0
	u.Queue(SessionRecord{ID: "a", Path: rec, ChainHead: "h"})
	u.Pass(context.Background())
	if _, rm := u.Tidy(time.Now().Add(100 * 365 * 24 * time.Hour)); rm != 0 {
		t.Fatal("deleted with Keep 0")
	}
	if _, err := os.Stat(rec + ".gz"); err != nil {
		t.Fatal("not compressed")
	}
}

// The console can shorten how long shipped recordings stay while the agent
// runs; the next tidy goes by the new value.
func TestSetKeepAppliesToTheNextTidy(t *testing.T) {
	state := t.TempDir()
	rec := filepath.Join(state, "a.cast")
	_ = os.WriteFile(rec, []byte("[0.1,\"o\",\"x\"]\n"), 0o600)
	u := NewUploads(state, &fakeSender{}, nil)
	if u.Keep != 7*24*time.Hour {
		t.Fatalf("default keep = %v, want 7 days", u.Keep)
	}
	u.Queue(SessionRecord{ID: "a", Path: rec, ChainHead: "h"})
	u.Pass(context.Background())
	later := time.Now().Add(3 * 24 * time.Hour)
	if _, rm := u.Tidy(later); rm != 0 {
		t.Fatal("removed before the default keep")
	}
	u.SetKeep(48 * time.Hour)
	if _, rm := u.Tidy(later); rm != 1 {
		t.Fatal("not removed after keep was shortened")
	}
}

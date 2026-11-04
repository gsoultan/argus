package agent

import (
	"bytes"
	"context"
	"encoding/json"
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

// sampleRecording is a real recording, so a compressed copy can be shown to
// verify to the head it was sealed with.
func sampleRecording(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	r, err := recorder.New(&buf, recorder.Header{Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		_ = r.Write(recorder.Output, []byte("drwxr-xr-x 2 root root 4096 Nov  2 07:28 bin\r\n"))
	}
	head, _ := r.Close()
	return buf.Bytes(), head
}

// readPlain decompresses the recording at path.
func readPlain(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := recorder.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A sealed recording is compressed before it is shipped, and the compressed
// copy is what ships. It gives back the exact bytes, which still verify to
// the sealed head. Once shipped it is deleted after Keep; one that never
// reached the control plane is never deleted, however old.
func TestSealedRecordingsShipCompressedThenExpire(t *testing.T) {
	state := t.TempDir()
	plain, head := sampleRecording(t)
	shipped := filepath.Join(state, "shipped.cast")
	stuck := filepath.Join(state, "stuck.cast")
	_ = os.WriteFile(shipped, plain, 0o600)
	_ = os.WriteFile(stuck, plain, 0o600)

	send := &fakeSender{}
	u := NewUploads(state, send, nil)
	u.Keep = 48 * time.Hour
	u.Queue(SessionRecord{ID: "shipped", Path: shipped, ChainHead: head})
	u.Pass(context.Background())
	send.err = &reporter.UploadError{Status: http.StatusServiceUnavailable}
	u.Queue(SessionRecord{ID: "stuck", Path: stuck, ChainHead: head})
	u.Pass(context.Background())

	if len(send.paths) == 0 || send.paths[0] != shipped+recorder.CompressedExt {
		t.Fatalf("shipped %v, want the compressed copy", send.paths)
	}
	if _, err := os.Stat(shipped); !os.IsNotExist(err) {
		t.Error("the uncompressed copy is still there")
	}
	got := readPlain(t, shipped+recorder.CompressedExt)
	if !bytes.Equal(got, plain) {
		t.Fatal("the compressed copy does not give back the recording's bytes")
	}
	if v, err := recorder.Verify(bytes.NewReader(got), head); err != nil || !v.OK {
		t.Fatalf("the compressed copy no longer verifies: %+v %v", v, err)
	}
	// Verify reads through the compression on its own, as the control
	// plane and argus-verify do.
	f, _ := os.Open(shipped + recorder.CompressedExt)
	v, err := recorder.Verify(f, head)
	fi, _ := f.Stat()
	_ = f.Close()
	if err != nil || !v.OK {
		t.Fatalf("the compressed file does not verify: %+v %v", v, err)
	}
	t.Logf("recording %d bytes, compressed %d (%.1fx)", len(plain), fi.Size(),
		float64(len(plain))/float64(fi.Size()))
	if fi.Size()*4 > int64(len(plain)) {
		t.Errorf("compressed to %d of %d bytes", fi.Size(), len(plain))
	}

	now := time.Now()
	if c, rm := u.Tidy(now.Add(2 * time.Hour)); c != 0 || rm != 0 {
		t.Fatalf("tidied a compressed recording before keep: %d compressed, %d removed", c, rm)
	}
	if _, rm := u.Tidy(now.Add(49 * time.Hour)); rm != 1 {
		t.Fatalf("removed %d, want 1", rm)
	}
	if _, err := os.Stat(shipped + recorder.CompressedExt); !os.IsNotExist(err) {
		t.Error("an expired shipped recording is still on the host")
	}
	// The one the control plane never had is still there, compressed, and
	// still the same recording.
	if b := readPlain(t, stuck+recorder.CompressedExt); !bytes.Equal(b, plain) {
		t.Fatal("a recording that was never shipped was changed")
	}
	if c, rm := u.Tidy(now.Add(10 * 365 * 24 * time.Hour)); c+rm != 0 {
		t.Fatal("tidy touched something that was never shipped")
	}
}

// A recording an older agent shipped plain is compressed after an hour, and
// expiry removes the compressed copy under the plain name's marker.
func TestPlainShippedRecordingsAreCompressedThenExpired(t *testing.T) {
	state := t.TempDir()
	plain, head := sampleRecording(t)
	rec := filepath.Join(state, "old.cast")
	_ = os.WriteFile(rec, plain, 0o600)
	u := NewUploads(state, &fakeSender{}, nil)
	u.Keep = 48 * time.Hour
	now := time.Now()
	_ = os.MkdirAll(u.shippedDir(), 0o700)
	b, _ := json.Marshal(shippedMarker{ID: "old", Path: rec, ShippedAt: now})
	_ = os.WriteFile(filepath.Join(u.shippedDir(), "old.json"), b, 0o600)

	if c, _ := u.Tidy(now); c != 0 {
		t.Fatal("compressed a recording shipped moments ago")
	}
	if c, _ := u.Tidy(now.Add(2 * time.Hour)); c != 1 {
		t.Fatalf("compressed %d, want 1", c)
	}
	f, _ := os.Open(rec + recorder.CompressedExt)
	v, err := recorder.Verify(f, head)
	_ = f.Close()
	if err != nil || !v.OK {
		t.Fatalf("the compressed copy does not verify: %+v %v", v, err)
	}
	if _, rm := u.Tidy(now.Add(49 * time.Hour)); rm != 1 {
		t.Fatalf("removed %d, want 1", rm)
	}
	if _, err := os.Stat(rec + recorder.CompressedExt); !os.IsNotExist(err) {
		t.Error("an expired shipped recording is still on the host")
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
	if _, err := os.Stat(rec + recorder.CompressedExt); err != nil {
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
	if _, err := os.Stat(rec + recorder.CompressedExt); !os.IsNotExist(err) {
		t.Fatal("the compressed copy outlived keep")
	}
}

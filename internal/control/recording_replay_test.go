package control

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/hashchain"
	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/storage"
)

// countingWriter is a ResponseWriter that counts the body and keeps its
// last bytes, not the body: a test of a recording longer than memory should
// not hold it either.
type countingWriter struct {
	h    http.Header
	code int
	n    int64
	tail []byte
}

func (w *countingWriter) Header() http.Header { return w.h }
func (w *countingWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *countingWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.n += int64(len(b))
	w.tail = append(w.tail, b...)
	if len(w.tail) > 128<<10 {
		w.tail = append(w.tail[:0], w.tail[len(w.tail)-(64<<10):]...)
	}
	return len(b), nil
}

// reportClosed reports a closed session from a fresh host and returns its
// id and the host's credential.
func reportClosed(t *testing.T, s *Store, h http.Handler, protocol, head string) (string, string) {
	t.Helper()
	host := unique("replay")
	cred, _ := enrolled(t, s, h, host)
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id::text = $1`, id)
	})
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred,
		`{"id":"`+id+`","assetHostname":"`+host+`","principal":"root","protocol":"`+protocol+`",`+
			`"state":"closed","chainHead":"`+head+`"}`); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	return id, cred
}

// A recording longer than the 256 MB replay used to hold in memory replays
// whole, and verifies intact.
//
// Root cause: replay read the decompressed recording into memory, cut off
// at 256 MB, and verified what it had read; a longer recording was served
// short and reported as failing verification, though nothing had changed
// it. Replay now spools the stored, compressed object and reads the
// recording through it, so its length is bounded by nothing in memory.
func TestARecordingLongerThanTheOldReplayBoundReplaysWhole(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	dir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.SetStorage(nil)
	a.SetRecordingDir(dir)

	// 260 MB of terminal output, written straight into zstd and put in
	// storage as an agent's upload would leave it: the test holds neither
	// the recording nor a copy of it, and reads it through as few times as
	// it can, since under the race detector each pass takes seconds.
	zf, err := os.CreateTemp(t.TempDir(), "long-*.cast.zst")
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- recorder.Compress(zf, pr, recorder.Hot) }()
	chain := hashchain.New(pw)
	_ = chain.Emit([]byte(`{"version":2,"width":80,"height":24,"timestamp":1}`))
	chunk := []byte(`[1.5,"o","` + strings.Repeat("Nov  3 14:52:06 web-01 nginx: GET /healthz 200\\r\\n", 1300) + `"]`)
	var plainLen int64 = 51
	for plainLen <= 260<<20 {
		if err := chain.Emit(chunk); err != nil {
			t.Fatal(err)
		}
		plainLen += int64(len(chunk)) + 1
	}
	head, _ := chain.Close()
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = zf.Close()

	id, _ := reportClosed(t, s, h, "ssh", head)
	key, err := dir.Upload(context.Background(), zf.Name(), id, head, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreRecordingKey(context.Background(), id, head, key); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+id+"/recording", nil)
	r.Header.Set("Authorization", "Bearer admin-token")
	w := &countingWriter{h: http.Header{}}
	h.ServeHTTP(w, r)
	if w.code != http.StatusOK {
		t.Fatalf("replay: %d", w.code)
	}
	if v := w.h.Get("X-Argus-Chain-Verified"); v != "intact" {
		t.Errorf("a %d-byte recording verified %q", plainLen, v)
	}
	if w.n != plainLen || !bytes.HasSuffix(w.tail, append(chunk, '\n')) {
		t.Errorf("served %d bytes of %d", w.n, plainLen)
	}
}

// A desktop recording shipped plain is stored packed, and replays verified
// intact through the packing, from the same endpoint as before.
func TestADesktopRecordingIsStoredPackedAndReplays(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	var storedPath string
	var stored []byte
	dir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.SetStorage(nil)
	a.SetRecordingDir(dir)
	a.storeRecording = func(ctx context.Context, path, id, head string, at time.Time) (string, error) {
		storedPath = path
		stored, _ = os.ReadFile(path)
		return dir.Upload(ctx, path, id, head, at)
	}

	var plain bytes.Buffer
	rec, err := rdp.NewRecorder(&plain, rdp.Header{Width: 1280, Height: 720, Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		_ = rec.Write(rdp.ClientInput, []byte{0x04, byte(i), 0x00, 0x01, 0x02})
	}
	_ = rec.Event(map[string]string{"kind": "clipboard", "text": "copied"})
	head, err := rec.Close()
	if err != nil {
		t.Fatal(err)
	}

	id, cred := reportClosed(t, s, h, "rdp", head)
	if w := putRecordingAs(h, cred, id, head, "argusrdp", plain.Bytes()); w.Code != http.StatusOK {
		t.Fatalf("ship: %d %s", w.Code, w.Body)
	}
	if !strings.HasSuffix(storedPath, ".argusrdp.zst") {
		t.Errorf("stored as %s", storedPath)
	}
	if packed, err := recorder.IsPacked(bytes.NewReader(stored)); err != nil || !packed {
		t.Fatalf("a desktop recording was stored unpacked (%v)", err)
	}

	w := do(t, h, http.MethodGet, "/api/v1/sessions/"+id+"/rdp-replay", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	if v := w.Header().Get("X-Argus-Chain-Verified"); v != "intact" {
		t.Errorf("verified %q", v)
	}
	if !strings.Contains(w.Header().Get("X-Argus-Replay-Info"), `"width":1280`) {
		t.Errorf("replay info %q", w.Header().Get("X-Argus-Replay-Info"))
	}

	w = do(t, h, http.MethodGet, "/api/v1/sessions/"+id+"/recording", "admin-token", "")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), plain.Bytes()) {
		t.Fatalf("the recording does not read back as shipped: %d, %d bytes of %d",
			w.Code, w.Body.Len(), plain.Len())
	}
}

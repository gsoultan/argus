package control

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/storage"
)

// shipToDir reports a closed session from a fresh host, ships its recording
// into a directory store, and backdates it by age.
func shipToDir(t *testing.T, a *API, s *Store, h http.Handler, dir *storage.Dir, age time.Duration,
	cast []byte, head string) string {
	t.Helper()
	host := unique("arch")
	cred, _ := enrolled(t, s, h, host)
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id::text = $1`, id)
	})
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred,
		`{"id":"`+id+`","assetHostname":"`+host+`","principal":"root","protocol":"ssh",`+
			`"state":"closed","chainHead":"`+head+`"}`); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	if w := putRecordingAs(h, cred, id, head, "cast", cast); w.Code != http.StatusOK {
		t.Fatalf("ship: %d %s", w.Code, w.Body)
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE sessions SET started_at = now() - $2::interval WHERE id = $1::uuid`,
		id, age.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

func recordingKeyOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	got, err := s.Session(context.Background(), id)
	if err != nil || got == nil || got.RecordingKey == nil {
		t.Fatalf("session %s: %v", id, err)
	}
	return *got.RecordingKey
}

// A recording older than the archive age is recompressed, moved under
// archive/ and still replays from the console, verified intact. The move is
// in the audit chain, the old copy is gone, and a second pass does nothing.
func TestAnOldRecordingIsArchivedAndStillReplays(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	root := t.TempDir()
	dir, err := storage.NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	a.SetStorage(nil)
	a.SetRecordingDir(dir)
	a.ArchiveAfter = 30 * 24 * time.Hour

	cast, head := castFor(t)
	old := shipToDir(t, a, s, h, dir, 40*24*time.Hour, cast, head)
	recent := shipToDir(t, a, s, h, dir, 2*24*time.Hour, cast, head)
	before := recordingKeyOf(t, s, old)
	if !strings.HasSuffix(before, ".cast.zst") {
		t.Fatalf("stored as %s, want compressed", before)
	}

	n, err := a.ArchiveRecordings(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("archived %d", n)
	}
	after := recordingKeyOf(t, s, old)
	if !strings.HasPrefix(after, storage.ArchivePrefix) || !strings.HasSuffix(after, ".cast.zst") {
		t.Fatalf("archived to %s", after)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(before))); !os.IsNotExist(err) {
		t.Error("the copy before the archive is still there")
	}
	if k := recordingKeyOf(t, s, recent); storage.IsArchived(k) {
		t.Errorf("a recording two days old was archived: %s", k)
	}

	w := do(t, h, http.MethodGet, "/api/v1/sessions/"+old+"/recording", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	if !bytes.Equal(w.Body.Bytes(), cast) {
		t.Error("the archived recording does not replay as the one shipped")
	}
	if v := w.Header().Get("X-Argus-Chain-Verified"); v != "intact" {
		t.Errorf("verified %q", v)
	}
	if c, _ := s.AuditCountFor(context.Background(), "recording.archived", old); c != 1 {
		t.Errorf("recording.archived entries: %d", c)
	}

	if _, err := a.ArchiveRecordings(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if k := recordingKeyOf(t, s, old); k != after {
		t.Errorf("archived again: %s -> %s", after, k)
	}
}

// A stored recording that no longer verifies is not archived: it stays as
// found, and the finding is in the audit chain once.
func TestATamperedRecordingIsNotArchived(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	root := t.TempDir()
	dir, err := storage.NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	a.SetStorage(nil)
	a.SetRecordingDir(dir)
	a.ArchiveAfter = 24 * time.Hour

	cast, head := castFor(t)
	id := shipToDir(t, a, s, h, dir, 3*24*time.Hour, cast, head)
	key := recordingKeyOf(t, s, id)
	p := filepath.Join(root, filepath.FromSlash(key))
	var altered bytes.Buffer
	_ = recorder.Compress(&altered, bytes.NewReader(bytes.Replace(cast, []byte("uid=0"), []byte("uid=1"), 1)), recorder.Hot)
	if err := os.WriteFile(p, altered.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if _, err := a.ArchiveRecordings(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if k := recordingKeyOf(t, s, id); k != key {
		t.Fatalf("a tampered recording was moved to %s", k)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, altered.Bytes()) {
		t.Error("the tampered recording was changed")
	}
	if c, _ := s.AuditCountFor(context.Background(), "recording.unarchivable", id); c != 1 {
		t.Errorf("recording.unarchivable entries: %d, want 1", c)
	}
}

// Off means off.
func TestArchiveOffMovesNothing(t *testing.T) {
	a, _, _, _ := enrollAPI(t)
	a.ArchiveAfter = 0
	if n, err := a.ArchiveRecordings(context.Background(), time.Now().Add(100*365*24*time.Hour)); n != 0 || err != nil {
		t.Fatalf("archived %d, %v", n, err)
	}
}

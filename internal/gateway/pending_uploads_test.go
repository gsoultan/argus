package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/reporter"
)

/*
Recordings stranded by an object-store outage.

Measured before this existed: the store was stopped for one session and
restarted, and two minutes later the recording was still local-only. Nothing in
the gateway was ever going to change that -- the upload was attempted once,
failed, and was forgotten. The console went on saying the recording "exists only
on the host that produced it", and rebuilding that host would have destroyed it.

The artefact was never at risk; it stays on disk either way. What was missing is
the copy that outlives the gateway.
*/

// fakeStore stands in for object storage. The interesting case -- an upload
// that lands while the report about it does not -- needs a store that succeeds
// on demand, and needing a live MinIO for that would be a test CI never runs.
type fakeStore struct {
	fail   bool
	upload func(sessionID string)
}

func (f *fakeStore) Upload(_ context.Context, _, sessionID, _ string, _ time.Time) (string, error) {
	if f.upload != nil {
		f.upload(sessionID)
	}
	if f.fail {
		return "", errors.New("object store unreachable")
	}
	return "2026/01/01/" + sessionID + ".cast", nil
}

// deadReporter reaches nothing, so a report cannot land.
func deadReporter(t *testing.T) *reporter.Client {
	t.Helper()
	return reporter.New("https://127.0.0.1:1", "tok",
		filepath.Join(t.TempDir(), "spool.jsonl"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func pendingServer(t *testing.T) (*Server, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	srv := testServer(t, t.TempDir())
	srv.log = slog.New(slog.NewTextHandler(&logs, nil))
	return srv, &logs
}

func readPending(t *testing.T, srv *Server) []pendingUpload {
	t.Helper()
	data, err := os.ReadFile(srv.pendingPath())
	if err != nil {
		return nil
	}
	var out []pendingUpload
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var p pendingUpload
		if err := dec.Decode(&p); err != nil {
			break
		}
		out = append(out, p)
	}
	return out
}

// A failed upload is remembered, with what is needed to finish it later.
func TestAFailedUploadIsQueued(t *testing.T) {
	srv, _ := pendingServer(t)
	srv.queueUpload(pendingUpload{
		SessionID: "sess-1", ChainHead: "abc123",
		StartedAt: time.Now().UTC(), FailedAt: time.Now().UTC(),
	})

	got := readPending(t, srv)
	if len(got) != 1 || got[0].SessionID != "sess-1" {
		t.Fatalf("pending = %+v, want one entry for sess-1", got)
	}
	if got[0].ChainHead != "abc123" {
		t.Errorf("the chain head was not kept: %q -- without it the artefact "+
			"cannot be verified after it lands", got[0].ChainHead)
	}
}

// With no storage configured there is nothing to retry, and nothing to crash on.
func TestRetryWithoutStorageDoesNothing(t *testing.T) {
	srv, _ := pendingServer(t)
	srv.queueUpload(pendingUpload{SessionID: "sess-2", FailedAt: time.Now().UTC()})
	if n := srv.RetryUploads(context.Background()); n != 0 {
		t.Errorf("delivered %d with no storage configured", n)
	}
}

// An entry whose artefact has been deleted is dropped, loudly, rather than
// retried forever against a file that will never appear.
func TestAMissingArtefactIsNotRetriedForever(t *testing.T) {
	srv, logs := pendingServer(t)
	srv.cfg.Storage = &fakeStore{fail: true}

	srv.queueUpload(pendingUpload{SessionID: "gone", ChainHead: "h", FailedAt: time.Now().UTC()})
	srv.RetryUploads(context.Background())

	if got := readPending(t, srv); len(got) != 0 {
		t.Errorf("an entry with no artefact was kept: %+v", got)
	}
	if !strings.Contains(logs.String(), "no longer on disk") {
		t.Errorf("losing evidence has to be said out loud:\n%s", logs.String())
	}
}

// An entry that still fails is kept, or the retry forgets what it is retrying.
func TestEntriesThatStillFailAreKept(t *testing.T) {
	srv, _ := pendingServer(t)
	srv.cfg.Storage = &fakeStore{fail: true}

	path := filepath.Join(srv.cfg.RecordingDir, "sess-3.cast")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.queueUpload(pendingUpload{SessionID: "sess-3", ChainHead: "h", FailedAt: time.Now().UTC()})

	if n := srv.RetryUploads(context.Background()); n != 0 {
		t.Errorf("reported %d delivered against an unreachable store", n)
	}
	got := readPending(t, srv)
	if len(got) != 1 || got[0].SessionID != "sess-3" {
		t.Errorf("pending = %+v, want sess-3 still listed for the next pass", got)
	}
}

// Nothing pending means no file left behind to grow stale.
func TestAnEmptyListIsRemoved(t *testing.T) {
	srv, _ := pendingServer(t)
	srv.cfg.Storage = &fakeStore{fail: true}
	if err := os.WriteFile(srv.pendingPath(), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.RetryUploads(context.Background())
	if _, err := os.Stat(srv.pendingPath()); !os.IsNotExist(err) {
		t.Error("an empty pending list was left on disk")
	}
}

// Uploading the bytes and losing the pointer is not success.
//
// The first version reported delivered on any successful upload, even when the
// control plane never learned where the artefact went. Measured live: the file
// reached object storage, the report got a 404, the entry was dropped, and the
// console went on saying the recording existed only on the gateway -- the exact
// state this file exists to end.
func TestAnUploadWhoseReportFailsIsRetried(t *testing.T) {
	srv, logs := pendingServer(t)
	srv.cfg.Storage = &fakeStore{}
	// A reporter pointed at nothing: the upload will land, the report will not.
	srv.cfg.Reporter = deadReporter(t)

	path := filepath.Join(srv.cfg.RecordingDir, "sess-9.cast")
	if err := os.WriteFile(path, []byte("{\"version\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.queueUpload(pendingUpload{SessionID: "sess-9", ChainHead: "h", FailedAt: time.Now().UTC()})

	if n := srv.RetryUploads(context.Background()); n != 0 {
		t.Errorf("counted %d delivered when the control plane was never told", n)
	}
	got := readPending(t, srv)
	if len(got) != 1 || got[0].SessionID != "sess-9" {
		t.Errorf("pending = %+v, want sess-9 kept for another attempt", got)
	}
	if !strings.Contains(logs.String(), "could not tell the control plane") {
		t.Errorf("the half-finished delivery was not reported:\n%s", logs.String())
	}
}

package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/storage"
)

/*
Regressions in the stranded-upload recovery path.

Each of these was live on the gateway-observability branch: an unconfigured
store that spooled a retry it could never satisfy, a retry pass that held the
queue's lock across every upload in it, and a Remote Desktop recording the pass
looked for under the wrong extension.
*/

// unconfiguredStore is object storage that was never set up.
type unconfiguredStore struct{}

func (unconfiguredStore) Upload(context.Context, string, string, string, time.Time) (string, error) {
	return "", storage.ErrNotConfigured
}

// pathStore remembers the local path it was asked to upload.
type pathStore struct{ seen []string }

func (p *pathStore) Upload(_ context.Context, path, sessionID, _ string, _ time.Time) (string, error) {
	p.seen = append(p.seen, path)
	return "2026/01/01/" + sessionID, nil
}

// A store that does not exist is not an outage. Queueing one entry per session
// for a retry that can only give the same answer grows a file nothing drains.
func TestAnUnconfiguredStoreQueuesNothing(t *testing.T) {
	srv, _ := pendingServer(t)
	srv.cfg.Storage = unconfiguredStore{}

	sess := &Session{
		ID: "sess-nostore", StartedAt: time.Now().UTC(), srv: srv, log: srv.log,
	}
	if key := sess.uploadRecording("head-1"); key != "" {
		t.Errorf("uploadRecording returned %q with no store configured", key)
	}

	if got := readPending(t, srv); len(got) != 0 {
		t.Fatalf("spooled %d pending upload(s) for a store that does not exist: %+v\n"+
			"every session would add one, and no retry could ever clear them", len(got), got)
	}
}

// A retry pass must not hold the queue's lock while it uploads. It did, so a
// session ending during an outage waited 60s per stray to record one line --
// and the shutdown drain waited with it.
func TestQueueUploadIsNotBlockedByAnUploadInFlight(t *testing.T) {
	srv, _ := pendingServer(t)
	dir := srv.cfg.RecordingDir
	if err := os.WriteFile(filepath.Join(dir, "sess-slow.cast"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.queueUpload(pendingUpload{SessionID: "sess-slow", FailedAt: time.Now().UTC()})

	entered, release := make(chan struct{}), make(chan struct{})
	srv.cfg.Storage = &fakeStore{upload: func(string) {
		close(entered)
		<-release
	}}

	passDone := make(chan int, 1)
	go func() { passDone <- srv.RetryUploads(context.Background()) }()
	<-entered

	queued := make(chan struct{})
	go func() {
		srv.queueUpload(pendingUpload{SessionID: "sess-during", FailedAt: time.Now().UTC()})
		close(queued)
	}()

	select {
	case <-queued:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("queueUpload blocked behind an upload already in flight -- " +
			"a session ending mid-outage cannot record that it needs a retry")
	}
	close(release)

	if n := <-passDone; n != 1 {
		t.Errorf("delivered %d, want 1", n)
	}
	// The entry added mid-pass must survive the rewrite that ends it.
	got := readPending(t, srv)
	if len(got) != 1 || got[0].SessionID != "sess-during" {
		t.Fatalf("pending = %+v, want only sess-during -- an entry queued "+
			"during the pass was dropped by the rewrite that followed it", got)
	}
}

// A Remote Desktop recording is retried at its own extension. The pass only
// ever looked for .cast, so an RDP artefact was reported lost while it sat on
// disk under the name it was actually written with.
func TestARDPRecordingIsRetriedAtItsOwnExtension(t *testing.T) {
	srv, logs := pendingServer(t)
	dir := srv.cfg.RecordingDir
	if err := os.WriteFile(filepath.Join(dir, "sess-rdp"+rdp.Extension), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.queueUpload(pendingUpload{
		SessionID: "sess-rdp", Ext: rdp.Extension, FailedAt: time.Now().UTC(),
	})

	store := &pathStore{}
	srv.cfg.Storage = store
	if n := srv.RetryUploads(context.Background()); n != 1 {
		t.Fatalf("delivered %d, want 1 -- logs:\n%s", n, logs.String())
	}
	if len(store.seen) != 1 || !strings.HasSuffix(store.seen[0], rdp.Extension) {
		t.Fatalf("uploaded %v, want a path ending %s", store.seen, rdp.Extension)
	}
	if strings.Contains(logs.String(), "no longer on disk") {
		t.Error("the pass reported a recording lost that was on disk under its own extension")
	}
}

// Entries written before RDP recordings were queued here carry no extension,
// and a terminal capture is all they could have been.
func TestAnEntryWithNoExtensionIsATerminalRecording(t *testing.T) {
	srv, _ := pendingServer(t)
	dir := srv.cfg.RecordingDir
	if err := os.WriteFile(filepath.Join(dir, "sess-old.cast"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.queueUpload(pendingUpload{SessionID: "sess-old", FailedAt: time.Now().UTC()})

	store := &pathStore{}
	srv.cfg.Storage = store
	if n := srv.RetryUploads(context.Background()); n != 1 {
		t.Fatalf("delivered %d, want 1 -- an existing spool file stopped working", n)
	}
	if !strings.HasSuffix(store.seen[0], ".cast") {
		t.Errorf("uploaded %q, want a .cast path", store.seen[0])
	}
}

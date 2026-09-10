package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/storage"
)

/*
Recordings that could not be uploaded when their session ended.

An object store that is briefly unreachable used to strand a recording forever.
The artefact was sealed correctly, the chain head reached the control plane, and
the upload failed once and was never attempted again -- so the console showed a
session whose recording "exists only on the host that produced it", indefinitely,
and rebuilding that host destroyed it.

Measured: the store was stopped for one session and restarted, and two minutes
later the recording was still local-only. Nothing in the gateway was going to
change that.

The artefact itself is never at risk here -- it stays on disk either way. What
this recovers is the copy that outlives the gateway.
*/

// pendingUpload is one recording waiting to reach object storage.
type pendingUpload struct {
	SessionID string    `json:"sessionId"`
	ChainHead string    `json:"chainHead"`
	StartedAt time.Time `json:"startedAt"`
	FailedAt  time.Time `json:"failedAt"`
	// Ext is the artefact's extension, because a Remote Desktop recording and
	// a terminal one are different formats and a verifier handed the wrong one
	// reports tampering rather than a mismatch. Empty means ".cast": entries
	// written before RDP recordings were queued here carry no Ext, and a
	// terminal capture is all they could have been.
	Ext string `json:"ext,omitempty"`
}

// localPath is where this entry's artefact sits on disk.
func (p pendingUpload) localPath(dir string) string {
	ext := p.Ext
	if ext == "" {
		ext = ".cast"
	}
	return storage.LocalPathExt(dir, p.SessionID, ext)
}

// UploadRetryEvery is how often strays are retried.
//
// Slow on purpose: an outage lasting hours should produce a handful of attempts
// per recording, not thousands, and nothing here is urgent -- the evidence is
// safe on disk, it is only the durable copy that is missing.
// UploadRetryEvery is exported so the daemon can drive the loop.
const UploadRetryEvery = 2 * time.Minute

// pendingPath is where the list lives, beside the recordings it refers to.
func (s *Server) pendingPath() string {
	if s.cfg.RecordingDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.RecordingDir, ".pending-uploads.jsonl")
}

var pendingMu sync.Mutex

// queueUpload records that a recording still needs to reach object storage.
func (s *Server) queueUpload(p pendingUpload) {
	path := s.pendingPath()
	if path == "" {
		return
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.log.Error("cannot record a pending upload; this recording will stay local",
			"session", p.SessionID, "error", err)
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(p)
}

// readPendingLocked parses the list. The caller holds pendingMu.
func readPendingLocked(path string) []pendingUpload {
	f, err := os.Open(path)
	if err != nil {
		return nil // nothing pending
	}
	defer f.Close()
	var out []pendingUpload
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var p pendingUpload
		if json.Unmarshal(sc.Bytes(), &p) == nil && p.SessionID != "" {
			out = append(out, p)
		}
	}
	return out
}

// retryPassMu serialises retry passes without blocking queueUpload.
//
// A pass can outlast the ticker that starts it -- 60s per stray -- and two
// passes uploading the same artefact would race each other's rewrite of the
// list. Deliberately not pendingMu: see RetryUploads.
var retryPassMu sync.Mutex

// RetryUploads attempts every pending upload once, rewriting the list with what
// still failed. It returns how many were delivered.
//
// Called on a timer and once at start-up, so a gateway restarted after an
// outage picks up whatever the previous run could not deliver.
//
// The uploads run unlocked. Holding pendingMu across the pass put every session
// teardown behind 60s per stray: during an outage with a dozen of them, a
// session ending -- or the shutdown drain -- waited ten minutes to append one
// line. The list is re-read afterwards and only confirmed entries are removed,
// so anything queued mid-pass survives, and a crash mid-pass costs a repeated
// upload rather than a forgotten recording.
func (s *Server) RetryUploads(ctx context.Context) int {
	path := s.pendingPath()
	if path == "" || s.cfg.Storage == nil {
		return 0
	}
	if !retryPassMu.TryLock() {
		return 0 // a pass is already running; it will carry these
	}
	defer retryPassMu.Unlock()

	pendingMu.Lock()
	pending := readPendingLocked(path)
	pendingMu.Unlock()
	if len(pending) == 0 {
		pendingMu.Lock()
		_ = os.Remove(path)
		pendingMu.Unlock()
		return 0
	}

	// done is what must leave the list: delivered, or gone from disk and beyond
	// retrying. Anything not in it stays, including entries appended while this
	// pass was running.
	done := map[string]bool{}
	delivered := 0
	for _, p := range pending {
		local := p.localPath(s.cfg.RecordingDir)
		if _, err := os.Stat(local); err != nil {
			// The artefact is gone. Nothing to upload and nothing to retry, and
			// saying so is better than retrying a file that will never appear.
			s.log.Error("a pending recording is no longer on disk",
				"session", p.SessionID, "path", local,
				"detail", "it was never copied to object storage and is now lost")
			done[p.SessionID] = true
			continue
		}
		upCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		key, err := s.cfg.Storage.Upload(upCtx, local, p.SessionID, p.ChainHead, p.StartedAt)
		cancel()
		if err != nil {
			if errors.Is(err, storage.ErrNotConfigured) {
				// Not an outage: there is no store and never was. Retrying the
				// rest of the list would be the same answer N more times.
				//
				// Stop the pass rather than return from it: anything already
				// delivered still has to leave the list, or the next pass
				// uploads it a second time.
				s.log.Warn("pending recordings cannot be uploaded: no object storage is configured",
					"pending", len(pending),
					"detail", "the artefacts remain on this host; configure `storage` "+
						"and they will be delivered on the next pass")
				break
			}
			continue
		}
		// Tell the control plane, or the console goes on saying the recording
		// exists only on this host -- which is what it did when this reported
		// success on an upload whose pointer never arrived. The bytes being
		// safe is not the same as anyone being able to find them.
		if err := s.reportRecordingKey(ctx, p, key); err != nil {
			s.log.Warn("uploaded a stranded recording but could not tell the control plane",
				"session", p.SessionID, "key", key, "error", err,
				"detail", "kept for the next pass; the upload will simply happen again")
			continue
		}
		done[p.SessionID] = true
		delivered++
		s.log.Info("a stranded recording reached object storage",
			"session", p.SessionID, "key", key,
			"stranded_for", time.Since(p.FailedAt).Round(time.Second))
	}

	if len(done) == 0 {
		return delivered
	}
	// Re-read under the lock: entries appended while the uploads were running
	// are not in `pending`, and rewriting from that snapshot would drop them.
	pendingMu.Lock()
	var keep []pendingUpload
	for _, p := range readPendingLocked(path) {
		if !done[p.SessionID] {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		_ = os.Remove(path)
	} else {
		rewritePending(path, keep)
	}
	pendingMu.Unlock()
	return delivered
}

// rewritePending replaces the list atomically, so a crash mid-write cannot
// leave a half-written entry that parses as nothing.
func rewritePending(path string, entries []pendingUpload) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	enc := json.NewEncoder(f)
	for _, e := range entries {
		_ = enc.Encode(e)
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// reportRecordingKey tells the control plane where the artefact now lives.
//
// Synchronous, and its error matters. Uploading the bytes and losing the
// pointer leaves the console saying the recording exists only on this host,
// which is the state this whole file exists to end.
func (s *Server) reportRecordingKey(ctx context.Context, p pendingUpload, key string) error {
	if s.cfg.Reporter == nil || !s.cfg.Reporter.Enabled() {
		return nil // standalone: there is nobody to tell
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.cfg.Reporter.RecordingStored(rctx, p.SessionID, p.ChainHead, key)
}

// RecordingStore is the slice of object storage this package uses.
//
// An interface rather than *storage.Client because the interesting failure --
// an upload that lands while the report about it does not -- cannot be produced
// against a real store without one, and a test that needs a live MinIO and its
// credentials is a test that does not run in CI. One method, satisfied by
// *storage.Client as it stands.
type RecordingStore interface {
	Upload(ctx context.Context, path, sessionID, chainHead string, startedAt time.Time) (string, error)
}

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
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

// RetryUploads attempts every pending upload once, rewriting the list with what
// still failed. It returns how many were delivered.
//
// Called on a timer and once at start-up, so a gateway restarted after an
// outage picks up whatever the previous run could not deliver.
func (s *Server) RetryUploads(ctx context.Context) int {
	path := s.pendingPath()
	if path == "" || s.cfg.Storage == nil {
		return 0
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return 0 // nothing pending
	}
	var pending []pendingUpload
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var p pendingUpload
		if json.Unmarshal(sc.Bytes(), &p) == nil && p.SessionID != "" {
			pending = append(pending, p)
		}
	}
	f.Close()
	if len(pending) == 0 {
		_ = os.Remove(path)
		return 0
	}

	var stillFailing []pendingUpload
	delivered := 0
	for _, p := range pending {
		local := storage.LocalPath(s.cfg.RecordingDir, p.SessionID)
		if _, err := os.Stat(local); err != nil {
			// The artefact is gone. Nothing to upload and nothing to retry, and
			// saying so is better than retrying a file that will never appear.
			s.log.Error("a pending recording is no longer on disk",
				"session", p.SessionID, "path", local,
				"detail", "it was never copied to object storage and is now lost")
			continue
		}
		upCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		key, err := s.cfg.Storage.Upload(upCtx, local, p.SessionID, p.ChainHead, p.StartedAt)
		cancel()
		if err != nil {
			stillFailing = append(stillFailing, p)
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
			stillFailing = append(stillFailing, p)
			continue
		}
		delivered++
		s.log.Info("a stranded recording reached object storage",
			"session", p.SessionID, "key", key,
			"stranded_for", time.Since(p.FailedAt).Round(time.Second))
	}

	if len(stillFailing) == 0 {
		_ = os.Remove(path)
	} else {
		rewritePending(path, stillFailing)
	}
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

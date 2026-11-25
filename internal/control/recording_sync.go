package control

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/storage"
)

// RecordingsCopy is how far the control plane has got copying the
// recordings it kept on its own disk into object storage, after a bucket
// was configured. Until it is done, replay reads what the bucket does not
// have yet from this host.
type RecordingsCopy struct {
	// From is the directory being copied.
	From string `json:"from"`
	// Copied and Already are recordings now in the bucket: copied by this
	// control plane since it started, and found there already.
	Copied  int `json:"copied"`
	Already int `json:"already"`
	// Failed could not be copied on the last pass and are tried again.
	Failed int `json:"failed"`
	// Done is set once a pass has left nothing to copy.
	Done      bool       `json:"done"`
	LastError string     `json:"lastError,omitempty"`
	LastPass  *time.Time `json:"lastPass,omitempty"`
}

type recordingsCopy struct {
	mu    sync.Mutex
	state *RecordingsCopy
}

func (c *recordingsCopy) get() *RecordingsCopy {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		return nil
	}
	s := *c.state
	return &s
}

// CopyRecordingsFromHost serves recordings from b, reading anything b does
// not have yet from d, and copies d into b in the background until a pass
// leaves nothing behind, retrying every interval.
//
// It replaces SetStorage when a bucket is configured on a control plane
// that has been keeping recordings on its own disk.
func (a *API) CopyRecordingsFromHost(ctx context.Context, b storage.Bucket, d *storage.Dir, interval time.Duration) {
	if b == nil || d == nil {
		return
	}
	a.storage = b
	a.recordings = &storage.Layered{Primary: b, Fallbacks: []storage.Store{d}}
	a.hostCopy.mu.Lock()
	a.hostCopy.state = &RecordingsCopy{From: d.Root()}
	a.hostCopy.mu.Unlock()
	go a.runRecordingsCopy(ctx, b, d, interval)
}

func (a *API) runRecordingsCopy(ctx context.Context, b storage.Bucket, d *storage.Dir, interval time.Duration) {
	copied := 0
	for {
		res, err := storage.CopyObjects(ctx, d, b, chainHeadOf, a.log)
		if ctx.Err() != nil {
			return
		}
		copied += res.Copied
		if res.Copied > 0 {
			a.log.Info("copied recordings kept on this host to object storage",
				"copied", res.Copied, "bytes", res.Bytes, "bucket", b.Name())
			// One entry per pass, in the platform's log: the copies are
			// evidence moving, and who moved it is this control plane.
			if _, aerr := a.store.AppendAudit(PlatformContext(ctx), AuditEvent{
				Action: "recordings.copied", Severity: "info", ActorEmail: "argus-control",
				Target: b.Name(),
				Detail: fmt.Sprintf("Copied %d recordings kept on this host (%s) to object storage. "+
					"The copies on this host were left in place.", res.Copied, d.Root()),
			}); aerr != nil {
				a.log.Error("audit append failed", "error", aerr)
			}
		}
		now := time.Now().UTC()
		a.hostCopy.mu.Lock()
		st := a.hostCopy.state
		st.Copied, st.Already, st.Failed, st.LastPass = copied, res.Already, res.Failed, &now
		st.LastError = ""
		if err != nil {
			st.LastError = err.Error()
		}
		st.Done = err == nil
		a.hostCopy.mu.Unlock()

		if err == nil {
			return
		}
		a.log.Warn("copying recordings kept on this host to object storage is not finished",
			"error", err, "retry_in", interval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// chainHeadOf recomputes a recording's chain head, through its compression.
func chainHeadOf(r io.Reader) (string, error) {
	v, err := recorder.Verify(r, "")
	if err != nil {
		return "", err
	}
	return v.Head, nil
}

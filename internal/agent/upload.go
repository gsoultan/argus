package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/reporter"
)

/*
Shipping sealed recordings to the control plane.

	agent seals <id>.cast / <id>.argusrdp ──PUT──▶ argus-control ──▶ object storage
	                                                verifies the chain head first

Each sealed recording gets a marker in <state>/uploads before anything is
sent, so a restart, an outage or a full bucket only delays it. The marker goes
when the control plane has stored the bytes, or when it refuses them for good
-- a chain that does not verify is kept on the host and said loudly, not
retried for ever. The recording itself never leaves the host's disk: the
local copy stays the evidence of record on this machine, as it always was.
*/

// RecordingSender delivers one recording. *reporter.Client is one.
type RecordingSender interface {
	UploadRecording(ctx context.Context, id, chainHead, path string) error
}

// Uploads is the queue of sealed recordings still to ship.
type Uploads struct {
	dir  string
	send RecordingSender
	log  *slog.Logger
	kick chan struct{}

	mu     sync.Mutex
	warned map[string]bool
}

type uploadMarker struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	ChainHead string `json:"chain_head"`
}

// NewUploads keeps its queue under stateDir. A nil send queues nothing.
func NewUploads(stateDir string, send RecordingSender, log *slog.Logger) *Uploads {
	if log == nil {
		log = slog.Default()
	}
	return &Uploads{dir: filepath.Join(stateDir, "uploads"), send: send, log: log,
		kick: make(chan struct{}, 1), warned: map[string]bool{}}
}

// Queue marks rec's recording for shipping. A recording with no chain head
// was never sealed and has nothing a control plane could verify.
func (u *Uploads) Queue(rec SessionRecord) {
	if u == nil || u.send == nil || rec.ChainHead == "" || rec.Path == "" {
		return
	}
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		u.log.Error("cannot queue a recording for upload", "session", rec.ID, "error", err)
		return
	}
	b, _ := json.Marshal(uploadMarker{ID: rec.ID, Path: rec.Path, ChainHead: rec.ChainHead})
	if err := os.WriteFile(filepath.Join(u.dir, rec.ID+".json"), b, 0o600); err != nil {
		u.log.Error("cannot queue a recording for upload", "session", rec.ID, "error", err)
		return
	}
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// Run ships what is queued until ctx ends: at once when something is
// queued, and every interval for what is still waiting.
func (u *Uploads) Run(ctx context.Context, interval time.Duration) {
	if u == nil || u.send == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		u.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-u.kick:
		case <-t.C:
		}
	}
}

// Pass tries every queued recording once and returns how many are still
// waiting.
func (u *Uploads) Pass(ctx context.Context) int {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		return 0
	}
	waiting := 0
	for _, e := range entries {
		if ctx.Err() != nil {
			return waiting
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		marker := filepath.Join(u.dir, e.Name())
		b, err := os.ReadFile(marker)
		var m uploadMarker
		if err != nil || json.Unmarshal(b, &m) != nil || m.ID == "" {
			u.log.Error("dropping an unreadable upload marker", "marker", marker)
			_ = os.Remove(marker)
			continue
		}
		if _, err := os.Stat(m.Path); err != nil {
			u.log.Error("a queued recording is gone from the host; it cannot be shipped",
				"session", m.ID, "path", m.Path, "error", err)
			_ = os.Remove(marker)
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		err = u.send.UploadRecording(sctx, m.ID, m.ChainHead, m.Path)
		cancel()
		var ue *reporter.UploadError
		switch {
		case err == nil:
			_ = os.Remove(marker)
			u.forget(m.ID)
			u.log.Info("recording shipped to the control plane", "session", m.ID)
		case errors.As(err, &ue) && !ue.Retry():
			// Kept, renamed, so it is not sent again and is easy to find.
			_ = os.Rename(marker, strings.TrimSuffix(marker, ".json")+".refused")
			u.forget(m.ID)
			u.log.Error("the control plane refused a recording; it stays on this host only",
				"session", m.ID, "path", m.Path, "status", ue.Status, "reason", ue.Message)
		default:
			waiting++
			if u.warnOnce(m.ID) {
				u.log.Warn("cannot ship a recording yet; it stays queued",
					"session", m.ID, "error", err)
			}
		}
	}
	return waiting
}

func (u *Uploads) warnOnce(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.warned[id] {
		return false
	}
	u.warned[id] = true
	return true
}

func (u *Uploads) forget(id string) {
	u.mu.Lock()
	delete(u.warned, id)
	u.mu.Unlock()
}

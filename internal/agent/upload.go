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

	"github.com/gsoultan/argus/internal/agent/reporter"
	"github.com/gsoultan/argus/internal/recorder"
)

/*
Shipping sealed recordings to the control plane.

	agent seals <id>.cast / <id>.argusrdp ──PUT──▶ argus-control ──▶ object storage
	                                                verifies the chain head first

Each sealed recording gets a marker in <state>/uploads before anything is
sent, so a restart, an outage or a full bucket only delays it. The marker goes
when the control plane has stored the bytes, or when it refuses them for good
-- a chain that does not verify is kept on the host and said loudly, not
retried for ever.

A sealed recording is compressed before it is shipped: zstd, <id>.cast.zst
or <id>.argusrdp.zst, a desktop recording ten to twenty times smaller and a
terminal one five to ten. That is the one change made to the host's copy
before the control plane holds it, and it is made safely: the compressed copy
is read back to the same bytes before the plain one is removed, and the chain
covers the recording's lines, so it verifies to the same head. A control
plane that has not said it reads zstd is sent the recording decompressed.

Once shipped, the host's copy is a second copy, and a fleet of servers keeping
every session they ever recorded for ever is disk that fills on every host at
once. So once shipped (Tidy) it is deleted after Keep (7 days by default; the
console's Settings, or --keep-shipped on the host, which wins). Zero keeps it
for ever. A copy shipped plain by an older agent is compressed after
CompressAfter (an hour) in the meantime.

A recording that was never shipped, is still queued, or was refused is never
deleted: until the control plane holds a verified copy, the host's is the
only evidence there is.
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

	// Keep is how long a shipped recording stays on the host; zero keeps it
	// for ever. Set it before Run, or with SetKeep once Run has started.
	// CompressAfter is when it is gzipped; zero means an hour.
	Keep          time.Duration
	CompressAfter time.Duration

	mu       sync.Mutex
	warned   map[string]bool
	lastTidy time.Time
}

// DefaultKeepShipped is how long a shipped recording stays on the host when
// neither the console nor the host's own flag says otherwise. The control
// plane holds the verified copy; this one is a convenience.
const DefaultKeepShipped = 7 * 24 * time.Hour

// SetKeep changes Keep while Run is going, for a value the console sent.
func (u *Uploads) SetKeep(d time.Duration) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.Keep = d
	u.mu.Unlock()
}

// shippedMarker remembers a recording the control plane has stored.
type shippedMarker struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	ShippedAt time.Time `json:"shipped_at"`
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
		kick: make(chan struct{}, 1), warned: map[string]bool{}, Keep: DefaultKeepShipped}
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
		if u.lastTidy.IsZero() || time.Since(u.lastTidy) >= 10*time.Minute {
			u.Tidy(time.Now())
			u.lastTidy = time.Now()
		}
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
		if !strings.HasSuffix(m.Path, recorder.CompressedExt) {
			m = u.compress(marker, m)
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		err = u.send.UploadRecording(sctx, m.ID, m.ChainHead, m.Path)
		cancel()
		var ue *reporter.UploadError
		switch {
		case err == nil:
			u.markShipped(m)
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

// compress replaces a queued recording with its zstd copy and repoints the
// marker at it. A failure leaves the plain recording queued as it was: it
// ships plain, and is no less evidence for that.
func (u *Uploads) compress(marker string, m uploadMarker) uploadMarker {
	before, _ := os.Stat(m.Path)
	path, err := recorder.CompressFile(m.Path, recorder.Hot)
	if err != nil {
		u.log.Warn("cannot compress a recording before shipping it; it ships as it is",
			"session", m.ID, "path", m.Path, "error", err)
		return m
	}
	c := m
	c.Path = path
	b, _ := json.Marshal(c)
	if err := os.WriteFile(marker, b, 0o600); err != nil {
		// The plain file is already gone; the marker must not name it.
		u.log.Error("cannot repoint an upload marker at the compressed recording",
			"session", m.ID, "error", err)
	}
	if after, err := os.Stat(path); err == nil && before != nil {
		u.log.Info("recording compressed for shipping", "session", m.ID,
			"bytes", before.Size(), "compressed", after.Size())
	}
	return c
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

func (u *Uploads) shippedDir() string { return filepath.Join(u.dir, "shipped") }

// markShipped records that the control plane holds m's recording, which is
// what later lets Tidy compress and expire the host's copy -- and nothing
// else does.
func (u *Uploads) markShipped(m uploadMarker) {
	if err := os.MkdirAll(u.shippedDir(), 0o700); err != nil {
		return
	}
	b, _ := json.Marshal(shippedMarker{ID: m.ID, Path: m.Path, ShippedAt: time.Now().UTC()})
	_ = os.WriteFile(filepath.Join(u.shippedDir(), m.ID+".json"), b, 0o600)
}

// Tidy compresses and expires the host's copies of shipped recordings; see
// the package comment. It reports how many it compressed and removed.
func (u *Uploads) Tidy(now time.Time) (compressed, removed int) {
	if u == nil {
		return 0, 0
	}
	entries, err := os.ReadDir(u.shippedDir())
	if err != nil {
		return 0, 0
	}
	u.mu.Lock()
	keep := u.Keep
	u.mu.Unlock()
	after := u.CompressAfter
	if after <= 0 {
		after = time.Hour
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		marker := filepath.Join(u.shippedDir(), e.Name())
		b, err := os.ReadFile(marker)
		var m shippedMarker
		if err != nil || json.Unmarshal(b, &m) != nil || m.Path == "" {
			_ = os.Remove(marker)
			continue
		}
		age := now.Sub(m.ShippedAt)
		if keep > 0 && age >= keep {
			_ = os.Remove(m.Path)
			_ = os.Remove(m.Path + ".gz")
			_ = os.Remove(recorder.CompressedName(m.Path))
			_ = os.Remove(marker)
			removed++
			continue
		}
		if age < after || strings.HasSuffix(m.Path, ".gz") ||
			strings.HasSuffix(m.Path, recorder.CompressedExt) {
			continue
		}
		if _, err := os.Stat(m.Path); err != nil {
			continue
		}
		// Shipped plain, by an agent before compression at seal. The marker
		// keeps the plain name; expiry removes either.
		if _, err := recorder.CompressFile(m.Path, recorder.Hot); err != nil {
			u.log.Warn("cannot compress a shipped recording; it stays as it is",
				"session", m.ID, "path", m.Path, "error", err)
			continue
		}
		compressed++
	}
	if compressed+removed > 0 {
		u.log.Info("tidied shipped recordings on this host",
			"compressed", compressed, "removed", removed, "keep", keep.String())
	}
	return compressed, removed
}

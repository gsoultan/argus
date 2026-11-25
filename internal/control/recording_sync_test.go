package control

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/hashchain"
	"github.com/gsoultan/argus/internal/storage"
)

// dirBucket is a directory standing in for a bucket.
type dirBucket struct {
	*storage.Dir
	name string
}

func (d dirBucket) Name() string                                      { return d.name }
func (d dirBucket) EnsureBucket(context.Context) error                { return nil }
func (d dirBucket) ChainHead(context.Context, string) (string, error) { return "", nil }
func (d dirBucket) PresignedURL(context.Context, string, time.Duration) (*url.URL, error) {
	return nil, errors.New("a directory has no links")
}

// Turning object storage on must not strand the recordings this control
// plane kept on its own disk until then.
//
// Root cause: with a bucket configured, the control plane read recordings
// from the bucket alone, so every session recorded before the bucket was
// set up answered "not found" on replay, though the recording was on this
// host. They are now copied into the bucket, and read from this host until
// they are.
func TestRecordingsKeptOnThisHostReplayAfterABucketIsConfigured(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.SetStorage(nil)
	a.SetRecordingDir(host)

	// A recording made while there was no bucket.
	f, err := os.CreateTemp(t.TempDir(), "rec-*.cast")
	if err != nil {
		t.Fatal(err)
	}
	chain := hashchain.New(f)
	_ = chain.Emit([]byte(`{"version":2,"width":80,"height":24,"timestamp":1}`))
	_ = chain.Emit([]byte(`[0.5,"o","uptime\r\n"]`))
	head, _ := chain.Close()
	_ = f.Close()
	id, _ := reportClosed(t, s, h, "ssh", head)
	key, err := host.Upload(ctx, f.Name(), id, head, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreRecordingKey(ctx, id, head, key); err != nil {
		t.Fatal(err)
	}

	replay := func(when string) {
		t.Helper()
		w := do(t, h, http.MethodGet, "/api/v1/sessions/"+id+"/recording", "admin-token", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: replay %d %s", when, w.Code, w.Body)
		}
		if v := w.Header().Get("X-Argus-Chain-Verified"); v != "intact" {
			t.Errorf("%s: verified %q", when, v)
		}
	}

	// Now a bucket. Before the copy has run, the recording still replays.
	bdir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bucket := dirBucket{Dir: bdir, name: unique("bucket")}
	a.SetStorage(bucket)
	a.CopyRecordingsFromHost(ctx, bucket, host, time.Hour)
	replay("while copying")

	deadline := time.Now().Add(10 * time.Second)
	for st := a.hostCopy.get(); st == nil || !st.Done; st = a.hostCopy.get() {
		if time.Now().After(deadline) {
			t.Fatalf("the copy did not finish: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := a.hostCopy.get(); st.Copied != 1 || st.From != host.Root() {
		t.Errorf("status %+v, want one recording copied from %s", st, host.Root())
	}
	if n, _ := s.AuditCountFor(ctx, "recordings.copied", bucket.name); n != 1 {
		t.Errorf("%d audit entries for the copy, want 1", n)
	}

	// Gone from this host, it replays from the bucket.
	if err := os.Remove(filepath.Join(host.Root(), filepath.FromSlash(key))); err != nil {
		t.Fatal(err)
	}
	replay("from the bucket")
}

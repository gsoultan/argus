package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gsoultan/argus/internal/reporter"
)

type fakeSender struct {
	err  error
	sent []string
}

func (f *fakeSender) UploadRecording(_ context.Context, id, _, _ string) error {
	f.sent = append(f.sent, id)
	return f.err
}

// A recording stays queued until the control plane has it, survives the
// agent restarting in between, and is set aside -- not retried for ever --
// when the control plane refuses the bytes themselves.
func TestARecordingStaysQueuedUntilTheControlPlaneHasIt(t *testing.T) {
	state := t.TempDir()
	rec := filepath.Join(state, "s1.cast")
	if err := os.WriteFile(rec, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	send := &fakeSender{err: &reporter.UploadError{Status: http.StatusServiceUnavailable}}
	u := NewUploads(state, send, nil)
	u.Queue(SessionRecord{ID: "s1", Path: rec, ChainHead: "abc"})
	u.Queue(SessionRecord{ID: "unsealed", Path: rec})

	if n := u.Pass(context.Background()); n != 1 {
		t.Fatalf("waiting after an outage: %d", n)
	}
	send.err = errors.New("connection refused")
	// A new process finds the queue where the old one left it.
	u = NewUploads(state, send, nil)
	if n := u.Pass(context.Background()); n != 1 {
		t.Fatalf("waiting after a restart: %d", n)
	}
	send.err = nil
	if n := u.Pass(context.Background()); n != 0 {
		t.Fatalf("waiting after delivery: %d", n)
	}
	if len(send.sent) != 3 {
		t.Errorf("sent %v; the unsealed recording was queued", send.sent)
	}
	if _, err := os.Stat(rec); err != nil {
		t.Error("the host's copy went with the upload")
	}

	send.err = &reporter.UploadError{Status: http.StatusUnprocessableEntity, Message: "does not verify"}
	u.Queue(SessionRecord{ID: "s1", Path: rec, ChainHead: "abc"})
	u.Pass(context.Background())
	u.Pass(context.Background())
	if len(send.sent) != 4 {
		t.Errorf("a refused recording was sent again: %v", send.sent)
	}
	if _, err := os.Stat(filepath.Join(state, "uploads", "s1.refused")); err != nil {
		t.Error("a refused recording left no trace in the queue")
	}
}

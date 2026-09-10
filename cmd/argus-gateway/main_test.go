package main

import (
	"testing"

	"github.com/gsoultan/argus/internal/gateway"
	"github.com/gsoultan/argus/internal/storage"
)

// A gateway with no `storage:` block hands the server nothing, not a nil
// pointer wearing an interface.
//
// gateway.Config.Storage is an interface. A nil *storage.Client assigned to it
// is a non-nil interface holding a nil pointer, so every `cfg.Storage == nil`
// guard in the gateway goes dead at the same moment. That shipped: a gateway
// with no object storage logged an upload failure per session and spooled each
// recording for a retry that could never succeed.
func TestNoStorageConfiguredMeansNoStore(t *testing.T) {
	var unconfigured *storage.Client
	if got := recordingStore(unconfigured); got != nil {
		t.Fatalf("recordingStore(nil) = %#v, want a nil interface -- "+
			"the gateway's nil guards are only as good as this", got)
	}

	// Spelled out, because it is the reason the helper exists rather than a
	// plain assignment: this is what the direct form does.
	var direct gateway.RecordingStore = unconfigured
	if direct == nil {
		t.Fatal("a nil *storage.Client now compares nil as an interface; " +
			"recordingStore is no longer needed and this test is misleading")
	}
}

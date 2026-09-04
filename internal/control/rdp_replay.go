package control

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/recorder"
)

// getRDPReplay decodes a Remote Desktop recording into display frames.
//
// The decoding happens here rather than in the browser for the same reason it
// happens in the gateway during a live session: a codec in JavaScript runs on
// the main thread and costs a megabyte of script. Replay therefore reuses the
// live path's decoder, so what an auditor sees months later is produced by the
// same code that showed it to the operator at the time.
//
// The chain is verified before a single frame is served. Handing over a replay
// and only then discovering the recording was altered would be exactly
// backwards — the viewer would already have formed an impression of what
// happened.
func (a *API) getRDPReplay(w http.ResponseWriter, r *http.Request, actor string) {
	id := r.PathValue("id")

	sess, err := a.store.Session(r.Context(), id)
	if err != nil {
		a.fail(w, "replay", err)
		return
	}
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if sess.Protocol != "rdp" {
		writeErr(w, http.StatusBadRequest,
			"this session is not a Remote Desktop recording")
		return
	}
	if sess.RecordingKey == nil || *sess.RecordingKey == "" {
		writeErr(w, http.StatusNotFound,
			"recording is not in object storage; it exists only on the host that produced it")
		return
	}
	if a.storage == nil {
		writeErr(w, http.StatusServiceUnavailable, "object storage is not configured")
		return
	}

	obj, err := a.storage.Get(r.Context(), *sess.RecordingKey)
	if err != nil {
		a.fail(w, "fetch recording", err)
		return
	}
	defer obj.Close()

	body, err := io.ReadAll(io.LimitReader(obj, 256<<20))
	if err != nil {
		a.fail(w, "read recording", err)
		return
	}

	verdict := "unverified"
	if sess.ChainHead != nil && *sess.ChainHead != "" {
		v, verr := recorder.Verify(bytes.NewReader(body), *sess.ChainHead)
		switch {
		case verr != nil:
			verdict = "error"
		case v.OK:
			verdict = "intact"
		default:
			// Served anyway, clearly labelled. An altered recording is still
			// the only record of what happened and refusing to show it would
			// destroy the evidence of the tampering along with the session.
			verdict = "tampered"
			a.log.Error("serving a Remote Desktop replay that failed verification",
				"session", id, "actor", actor, "broken_at", v.BrokenAt)
		}
	}

	// Decoded to a buffer rather than streamed, so a decode failure produces an
	// error rather than a truncated body the browser would render as a session
	// that simply stopped.
	var frames bytes.Buffer
	info, err := rdp.DecodeRecording(bytes.NewReader(body), &frames)
	if err != nil {
		a.fail(w, "decode recording", err)
		return
	}

	meta, err := json.Marshal(info)
	if err != nil {
		a.fail(w, "encode replay info", err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Argus-Chain-Verified", verdict)
	// The metadata rides in a header so the body stays a pure frame stream the
	// worker can walk without first parsing a prefix of a different shape.
	w.Header().Set("X-Argus-Replay-Info", string(meta))
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(frames.Bytes())

	a.log.Info("rdp replay served",
		"session", id, "actor", actor, "verified", verdict,
		"bytes", frames.Len(), "duration_ms", info.DurationMS)
}

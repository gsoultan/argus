package control

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/storage"
)

// Storage is set when object storage is configured. Without it the control
// plane can still track sessions; it simply cannot serve their recordings,
// because they never left the host that produced them.
func (a *API) SetStorage(c *storage.Client) { a.storage = c }

// getRecording streams a session's recording to the console.
//
// Streamed through here rather than handed out as a presigned URL, because a
// presigned URL is a bearer capability that leaves no trace once issued: it can
// be forwarded, replayed, and used long after the person who requested it lost
// access. Proxying costs bandwidth and buys an audit entry for every read of
// privileged session content, which is the whole point of the system.
func (a *API) getRecording(w http.ResponseWriter, r *http.Request, actor string) {
	id := r.PathValue("id")

	sess, err := a.store.Session(r.Context(), id)
	if err != nil {
		a.fail(w, "recording", err)
		return
	}
	if sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
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

	// Buffered so the chain can be verified before a single byte is served.
	// Handing over a recording and only then discovering it was altered would
	// be exactly backwards.
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
			verdict = "tampered"
		}
	}

	// Reading a privileged session's contents is itself a privileged action.
	// A tampered artefact is a critical finding, not a footnote.
	severity := "notice"
	detail := "Recording retrieved. Chain verified " + verdict + "."
	if verdict == "tampered" {
		severity = "critical"
		detail = "Recording retrieved but its hash chain does NOT match what the " +
			"gateway sealed. The stored artefact has been altered."
	}
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "recording.read",
		Severity:   severity,
		ActorEmail: actor,
		Target:     sess.AssetHostname,
		Detail:     detail,
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	if verdict == "tampered" {
		a.log.Error("recording failed verification",
			"session", id, "expected", *sess.ChainHead)
		// Still served: an investigator needs the bytes to work out what was
		// changed. The header is what tells them not to trust the contents.
	}

	w.Header().Set("Content-Type", "application/x-asciicast")
	w.Header().Set("X-Argus-Chain-Verified", verdict)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// presignRecording is available for bulk export, where proxying gigabytes
// through the control plane is the wrong trade.
func (a *API) presignRecording(w http.ResponseWriter, r *http.Request, actor string) {
	id := r.PathValue("id")
	sess, err := a.store.Session(r.Context(), id)
	if err != nil || sess == nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if sess.RecordingKey == nil || a.storage == nil {
		writeErr(w, http.StatusNotFound, "recording is not in object storage")
		return
	}

	u, err := a.storage.PresignedURL(r.Context(), *sess.RecordingKey, 5*time.Minute)
	if err != nil {
		a.fail(w, "presign", err)
		return
	}

	// Logged as its own action: unlike a proxied read, what happens after this
	// link is issued is invisible to Argus.
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "recording.presigned",
		Severity:   "warning",
		ActorEmail: actor,
		Target:     sess.AssetHostname,
		Detail:     "A 5-minute direct download link was issued. Reads through it are not audited.",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":       u.String(),
		"expiresIn": 300,
		"chainHead": sess.ChainHead,
		"warning":   "Reads through this link are not audited.",
	})
}

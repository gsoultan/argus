package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/storage"
)

// MaxRecordingUpload bounds one recording an agent ships. A day-long desktop
// session is well under it; past it the agent keeps the file on the host and
// says so, rather than this process spooling something without limit.
const MaxRecordingUpload = 4 << 30

// putRecording serves PUT /api/v1/report/recording/{id}: an agent shipping a
// sealed recording.
//
// The bytes are checked against the chain head the session was reported
// with before anything is stored. A recording that does not verify is
// refused, and the refusal is in the audit chain: either the file was
// altered on the host after it was sealed, or the report was, and both are
// what this product exists to notice.
//
// Only the agent that reported the session may ship it, on its own per-host
// credential. The shared reporter token is refused: it names no host, so it
// cannot be tied to the session it claims to be evidence for.
func (a *API) putRecording(w http.ResponseWriter, r *http.Request) {
	host := agentHost(r)
	if host == "" {
		writeErr(w, http.StatusForbidden, "recordings are shipped on an agent's own credential")
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeErr(w, http.StatusBadRequest, "not a session id")
		return
	}
	ext := rdp.Extension
	switch r.Header.Get("X-Argus-Format") {
	case "cast":
		ext = ".cast"
	case strings.TrimPrefix(rdp.Extension, "."):
	default:
		writeErr(w, http.StatusBadRequest, "X-Argus-Format must be cast or "+
			strings.TrimPrefix(rdp.Extension, "."))
		return
	}

	sess, err := a.store.Session(r.Context(), id)
	if err != nil {
		a.fail(w, "load session", err)
		return
	}
	if sess == nil {
		// The report is spooled separately and may simply not be here yet.
		writeErr(w, http.StatusConflict, "this session has not been reported yet")
		return
	}
	if sess.ReportedBy != "agent:"+host || !strings.EqualFold(sess.AssetHostname, host) {
		a.log.Warn("an agent shipped a recording for a session it did not report",
			"agent", host, "session", id, "reported_by", sess.ReportedBy)
		writeErr(w, http.StatusForbidden, "this session was not reported by this host")
		return
	}
	if (sess.Protocol == "rdp") != (ext == rdp.Extension) {
		writeErr(w, http.StatusBadRequest, "this is not the recording format of a "+sess.Protocol+" session")
		return
	}
	if sess.State == "active" {
		writeErr(w, http.StatusConflict, "this session has not been reported as ended yet")
		return
	}
	head := r.Header.Get("X-Argus-Chain-Head")
	if sess.ChainHead == nil || *sess.ChainHead == "" || *sess.ChainHead != head {
		// The end report carries the head; until it lands, try again.
		writeErr(w, http.StatusConflict, "the session's chain head is not this one yet")
		return
	}
	if sess.RecordingKey != nil && *sess.RecordingKey != "" {
		// Already stored: a retry after a reply that was lost on the way.
		writeJSON(w, http.StatusOK, map[string]string{"status": "stored", "key": *sess.RecordingKey})
		return
	}
	if a.recordings == nil && a.storeRecording == nil {
		writeErr(w, http.StatusServiceUnavailable, "recording storage is not configured")
		return
	}

	// An agent that knows the control plane reads zstd ships the file it
	// compressed when the session was sealed; an older one ships it plain.
	// Either is verified the same way, through the compression.
	body := bufio.NewReaderSize(http.MaxBytesReader(w, r.Body, MaxRecordingUpload), 64<<10)
	magic, _ := body.Peek(4)
	shippedCompressed := recorder.Compressed(magic)
	if shippedCompressed {
		ext += recorder.CompressedExt
	}
	tmp, err := os.CreateTemp("", "argus-recording-*"+ext)
	if err != nil {
		a.fail(w, "spool recording", err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("a recording larger than %d bytes is kept on the host", int64(MaxRecordingUpload)))
			return
		}
		writeErr(w, http.StatusBadRequest, "the recording did not arrive whole")
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		a.fail(w, "rewind recording", err)
		return
	}
	v, verr := recorder.Verify(tmp, head)
	if verr != nil || !v.OK {
		why := "its hash chain does not end at the head the session was reported with"
		if verr != nil {
			why = verr.Error()
		}
		a.log.Error("refused a recording that does not verify",
			"session", id, "host", host, "bytes", n, "reason", why)
		if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
			Action:     "recording.rejected",
			Severity:   "critical",
			ActorEmail: "agent:" + host,
			Target:     id,
			Detail: "The agent on " + host + " shipped the recording of session " + id +
				" and it did not verify: " + why + ". Either the file changed on the host " +
				"after it was sealed, or the session's report did. The host's copy is the " +
				"only one, and should be preserved.",
		}); aerr != nil {
			a.log.Error("audit append failed", "error", aerr)
		}
		writeErr(w, http.StatusUnprocessableEntity, "the recording does not verify: "+why)
		return
	}
	_ = tmp.Close()

	// Stored compressed whatever arrived. A plain recording is compressed
	// here, and the copy read back before the plain one goes; the chain
	// head it verifies to is the one just checked.
	spooled, stored := tmp.Name(), n
	if !shippedCompressed {
		if spooled, err = recorder.CompressFile(tmp.Name(), recorder.Hot); err != nil {
			a.fail(w, "compress recording", err)
			return
		}
		defer os.Remove(spooled)
		if fi, err := os.Stat(spooled); err == nil {
			stored = fi.Size()
		}
	}

	store := a.storeRecording
	if store == nil {
		store = a.recordingsFor(r.Context()).Upload
	}
	key, err := store(r.Context(), spooled, id, head, sess.StartedAt)
	if err != nil {
		if errors.Is(err, storage.ErrNotConfigured) {
			writeErr(w, http.StatusServiceUnavailable, "recording storage is not configured")
			return
		}
		a.log.Error("storing a shipped recording failed", "session", id, "error", err)
		a.uploads.failed(time.Now(), err)
		writeErr(w, http.StatusServiceUnavailable, "recording storage did not take the recording")
		return
	}
	if err := a.store.StoreRecordingKey(r.Context(), id, head, key); err != nil {
		a.fail(w, "store recording key", err)
		return
	}
	a.uploads.ok(time.Now())
	a.log.Info("recording shipped by agent", "session", id, "host", host, "key", key,
		"bytes_received", n, "bytes_stored", stored, "arrived_compressed", shippedCompressed)
	writeJSON(w, http.StatusOK, map[string]string{"status": "stored", "key": key})
}

// recordingUploader stores a verified recording; see API.storeRecording.
type recordingUploader func(ctx context.Context, path, sessionID, chainHead string,
	startedAt time.Time) (string, error)

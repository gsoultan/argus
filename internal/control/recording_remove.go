package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/storage"
)

/*
Removing a session's recording.

A recording is evidence, and until now nothing in Argus removed one. Storage
is not free, though, and a recording that is mostly a file someone copied --
a CI deploy through a direct login made 257 MB of one -- is a cost with little
evidence in it. So an admin may remove one, deliberately and visibly:

  - admin or owner only, and only a recording they could open;
  - the host name typed back, and a reason, as a terminal's own "end session"
    asks for one;
  - the session row stays, with who removed the recording, when and why, so
    the console shows a removal rather than a missing file;
  - the removal is in the audit chain, written before the file goes. If the
    audit entry cannot be written, nothing is removed.

The file is deleted after the session stops pointing at it. If the delete
fails the recording is still gone from Argus and the object is an orphan, which
is said in the reply and the log so it can be removed by hand.
*/

// removeRecordingRequest is what the console sends.
type removeRecordingRequest struct {
	// Confirm is the session's host name, typed back.
	Confirm string `json:"confirm"`
	Reason  string `json:"reason"`
}

// maxRemovalReason bounds the reason kept on the session and in the audit.
const maxRemovalReason = 500

// recordingRemover is the part of storage removal needs; see API.removeObject.
type recordingRemover func(ctx context.Context, key string) error

// deleteRecording serves DELETE /api/v1/sessions/{id}/recording.
func (a *API) deleteRecording(w http.ResponseWriter, r *http.Request) {
	who, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if who.Role != "admin" && who.Role != "owner" {
		writeErr(w, http.StatusForbidden, "removing a recording requires the admin or owner role")
		return
	}
	var in removeRecordingRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "send the host name to confirm, and a reason")
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		writeErr(w, http.StatusBadRequest, "a reason is required: it is kept with the session and in the audit log")
		return
	}
	if len(reason) > maxRemovalReason {
		reason = reason[:maxRemovalReason]
	}

	id := r.PathValue("id")
	sess, err := a.store.Session(r.Context(), id)
	if err != nil {
		a.fail(w, "load session", err)
		return
	}
	if sess == nil || !a.mayReadSession(r, sess) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(in.Confirm), sess.AssetHostname) {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("type the host name, %s, to confirm", sess.AssetHostname))
		return
	}
	switch {
	case sess.RecordingRemovedAt != nil:
		writeErr(w, http.StatusConflict, "this recording was already removed")
		return
	case sess.State == "active":
		writeErr(w, http.StatusConflict, "the session is still running; its recording can be removed once it ends")
		return
	case sess.RecordingKey == nil || *sess.RecordingKey == "":
		writeErr(w, http.StatusConflict,
			"there is no stored recording to remove; it never left the host that produced it")
		return
	}
	key := *sess.RecordingKey

	remove := a.removeObject
	if remove == nil {
		if a.recordings == nil {
			writeErr(w, http.StatusServiceUnavailable, "recording storage is not configured")
			return
		}
		store := storage.ForTenant(a.recordings, tenantIDOf(r.Context()))
		if !store.Owns(key) {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		remove = store.Delete
	}

	// The session stops pointing at the file first, and only if it still
	// points where it did: a second admin, or the archiver moving it, gets
	// there once.
	removed, err := a.store.MarkRecordingRemoved(r.Context(), id, key, who.Email, reason)
	if err != nil {
		a.fail(w, "remove recording", err)
		return
	}
	if !removed {
		writeErr(w, http.StatusConflict, "the recording changed while this was asked; reload and try again")
		return
	}

	head := ""
	if sess.ChainHead != nil {
		head = short(*sess.ChainHead)
	}
	detail := fmt.Sprintf("Recording of session %s (%s@%s, %s) removed to free storage. "+
		"Chain head %s; recorded size %s. The session record stays. Reason: %s",
		id, sess.Principal, sess.AssetHostname, sess.StartedAt.UTC().Format(time.RFC3339),
		head, humanBytes(sess.RecordingBytes), reason)
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "recording.removed",
		Severity:   "warning",
		ActorEmail: who.Email,
		Target:     sess.AssetHostname,
		Detail:     detail,
	}); err != nil {
		// A removal with no audit entry is the one thing this must not do.
		// Put the session back; the file has not been touched.
		if rerr := a.store.UnmarkRecordingRemoved(r.Context(), id, key); rerr != nil {
			a.log.Error("could not restore a recording after its audit entry failed",
				"session", id, "key", key, "error", rerr)
		}
		a.fail(w, "write the audit entry for the removal", err)
		return
	}

	out := map[string]any{"session": id, "removed": true, "by": who.Email}
	if err := remove(r.Context(), key); err != nil {
		a.log.Warn("removed a recording from Argus but could not delete the stored file",
			"session", id, "key", key, "error", err)
		out["fileNotDeleted"] = "the stored file could not be deleted and is left at " + key +
			"; remove it from storage by hand"
	}
	a.log.Warn("recording removed", "session", id, "by", who.Email, "key", key)
	writeJSON(w, http.StatusOK, out)
}

// MarkRecordingRemoved clears a session's recording key and says who removed
// it, when and why. false when the session no longer points at key.
func (s *Store) MarkRecordingRemoved(ctx context.Context, id, key, by, reason string) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE sessions
		   SET recording_key = NULL,
		       recording_removed_at = now(),
		       recording_removed_by = $3,
		       recording_removed_reason = $4
		 WHERE id = $1::uuid AND recording_key = $2 AND recording_removed_at IS NULL`,
		id, key, by, reason)
	if err != nil {
		return false, fmt.Errorf("mark recording removed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UnmarkRecordingRemoved undoes MarkRecordingRemoved, for a removal whose
// audit entry could not be written.
func (s *Store) UnmarkRecordingRemoved(ctx context.Context, id, key string) error {
	_, err := s.db(ctx).Exec(ctx, `
		UPDATE sessions
		   SET recording_key = $2,
		       recording_removed_at = NULL,
		       recording_removed_by = NULL,
		       recording_removed_reason = NULL
		 WHERE id = $1::uuid AND recording_key IS NULL`, id, key)
	return err
}

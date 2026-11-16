-- A session's recording can be removed by an admin to free storage. The
-- session row stays, and says when, by whom and why the recording went, so a
-- missing artefact reads as a deliberate removal rather than a lost one. The
-- removal is also in the audit chain (recording.removed).
ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS recording_removed_at     timestamptz,
    ADD COLUMN IF NOT EXISTS recording_removed_by     text,
    ADD COLUMN IF NOT EXISTS recording_removed_reason text;

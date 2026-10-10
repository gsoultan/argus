package control

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// RecordingRef is a session the control plane believes has a stored
// recording, with the head that recording must verify to.
type RecordingRef struct {
	SessionID string
	Key       string
	ChainHead string
}

// RecordingIndex lists every session whose recording reached object storage.
// A backup is checked against this: an artefact the database names and the
// backup lacks is evidence the backup cannot restore.
func (s *Store) RecordingIndex(ctx context.Context) ([]RecordingRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, recording_key, COALESCE(chain_head, '')
		  FROM sessions WHERE recording_key IS NOT NULL AND recording_key <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecordingRef
	for rows.Next() {
		var r RecordingRef
		if err := rows.Scan(&r.SessionID, &r.Key, &r.ChainHead); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AuditHashSeq returns the sequence of the audit event with this hash, or 0
// when the log holds no such event. A restored log must contain the head the
// backup recorded; finding it later in a longer chain is fine.
func (s *Store) AuditHashSeq(ctx context.Context, hash string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT seq FROM audit_events WHERE hash = $1`, hash).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

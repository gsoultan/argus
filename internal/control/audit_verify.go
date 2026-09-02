package control

import (
	"context"
	"fmt"
	"time"
)

// AuditVerification is the result of recomputing the stored audit chain.
type AuditVerification struct {
	OK      bool          `json:"ok"`
	Checked int64         `json:"checked"`
	Head    string        `json:"head"`
	Took    time.Duration `json:"took"`

	// BrokenAt is the sequence number where the recomputed hash first differed
	// from the stored one, or 0 when the chain is intact. Everything from that
	// point forward is unverifiable.
	BrokenAt int64 `json:"brokenAt,omitempty"`
	// Detail explains the first divergence in terms an operator can act on.
	Detail string `json:"detail,omitempty"`
}

// VerifyAuditChain recomputes the whole chain from the stored rows.
//
// The console already does this in the browser, which is the more meaningful
// check because it does not require trusting the server. This exists for the
// cases the browser cannot cover: confirming a restored backup is intact before
// anyone relies on it, and catching corruption on a schedule rather than when
// an auditor happens to look.
//
// Streams in sequence order rather than loading the log into memory: an audit
// log that has grown past what fits in RAM is exactly the one worth verifying.
func (s *Store) VerifyAuditChain(ctx context.Context) (AuditVerification, error) {
	started := time.Now()
	v := AuditVerification{OK: true, Head: GenesisHash}

	rows, err := s.pool.Query(ctx, `
		SELECT seq, at, action, severity, actor_email, target, detail, prev_hash, hash
		FROM audit_events ORDER BY seq ASC`)
	if err != nil {
		return v, err
	}
	defer rows.Close()

	prev := GenesisHash
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.Seq, &e.At, &e.Action, &e.Severity,
			&e.ActorEmail, &e.Target, &e.Detail, &e.PrevHash, &e.Hash); err != nil {
			return v, err
		}

		// Two separate failures worth distinguishing. A wrong prev_hash means a
		// row was removed or reordered; a wrong hash means a row's contents
		// were edited in place.
		if e.PrevHash != prev {
			v.OK, v.BrokenAt = false, e.Seq
			v.Detail = fmt.Sprintf(
				"event %d links to %s but the previous event hashes to %s — "+
					"a record was removed, inserted or reordered",
				e.Seq, short(e.PrevHash), short(prev))
			v.Took = time.Since(started)
			return v, nil
		}

		expected := chainHash(prev, e)
		if expected != e.Hash {
			v.OK, v.BrokenAt = false, e.Seq
			v.Detail = fmt.Sprintf(
				"event %d stores hash %s but its contents hash to %s — "+
					"the record was modified after it was written",
				e.Seq, short(e.Hash), short(expected))
			v.Took = time.Since(started)
			return v, nil
		}

		prev = e.Hash
		v.Checked++
	}
	if err := rows.Err(); err != nil {
		return v, err
	}

	v.Head = prev
	v.Took = time.Since(started)
	return v, nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

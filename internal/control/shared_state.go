package control

import (
	"context"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

/* ── Ticket redemption ───────────────────────────────────────────────────── */

// Redeem makes the store the signer's record of used tickets.
//
// In memory, "single use" holds for exactly one control plane: a ticket burned
// on one replica stays valid on the next, so behind a load balancer it would
// quietly become "single use per replica".
func (s *Store) Redeem(ctx context.Context, t auth.Ticket) (alreadyUsed bool, err error) {
	return s.RedeemTicket(ctx, t.ID, t.Email, t.Target, t.Principal, t.ExpiresAt)
}

// RedeemTicket records a ticket as used and reports whether it already was.
//
// The insert itself is the check: ON CONFLICT DO NOTHING either claims the id
// or does not, atomically. Reading first and then writing would leave a window
// where two concurrent connections both see "unused" and both proceed, which is
// exactly the race single-use is meant to close.
func (s *Store) RedeemTicket(ctx context.Context, id, email, target, principal string,
	expiresAt time.Time) (alreadyUsed bool, err error) {

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO redeemed_tickets (id, email, target, principal, expires_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO NOTHING`,
		id, email, target, principal, expiresAt)
	if err != nil {
		return false, err
	}
	// No row inserted means the id was already present: a replay.
	return tag.RowsAffected() == 0, nil
}

// SweepTickets drops redemption records that can no longer matter.
//
// Once a ticket is past its own expiry, verification rejects it on time alone,
// so keeping the row buys nothing. Without this the table grows forever on a
// key derived from request volume.
func (s *Store) SweepTickets(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM redeemed_tickets WHERE expires_at < now() - interval '1 hour'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

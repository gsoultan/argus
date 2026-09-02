package control

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

/* ── Ticket redemption ───────────────────────────────────────────────────── */

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

/* ── Host key pins ───────────────────────────────────────────────────────── */

// HostKeyPin is one target's verified identity.
type HostKeyPin struct {
	Host        string    `json:"host"`
	Fingerprint string    `json:"fingerprint"`
	KeyType     string    `json:"keyType"`
	PinnedAt    time.Time `json:"pinnedAt"`
	PinnedBy    string    `json:"pinnedBy"`
}

// HostKeyPin returns the pin for host, or nil when none exists.
func (s *Store) HostKeyPin(ctx context.Context, host string) (*HostKeyPin, error) {
	var p HostKeyPin
	err := s.pool.QueryRow(ctx, `
		SELECT host, fingerprint, key_type, pinned_at, pinned_by
		FROM host_key_pins WHERE host = $1`, host).
		Scan(&p.Host, &p.Fingerprint, &p.KeyType, &p.PinnedAt, &p.PinnedBy)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// PinHostKey records a pin, refusing to silently replace a different one.
//
// Overwriting a pin is how a host-key mismatch turns into no alert at all, so
// changing one is a deliberate act (Repin) rather than a side effect of a
// gateway happening to connect during a rebuild.
func (s *Store) PinHostKey(ctx context.Context, p HostKeyPin) (conflicting *HostKeyPin, err error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO host_key_pins (host, fingerprint, key_type, pinned_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (host) DO NOTHING`,
		p.Host, p.Fingerprint, p.KeyType, p.PinnedBy)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}

	// A pin already existed. Return it so the caller can tell "someone else
	// pinned the same key first" from "this host presented something else".
	existing, err := s.HostKeyPin(ctx, p.Host)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Fingerprint == p.Fingerprint {
		return nil, nil // benign race: two gateways pinned the same key
	}
	return existing, nil
}

// RepinHostKey replaces a pin after an operator verified the new key out of
// band. Never called automatically.
func (s *Store) RepinHostKey(ctx context.Context, p HostKeyPin) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO host_key_pins (host, fingerprint, key_type, pinned_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (host) DO UPDATE SET
			fingerprint = EXCLUDED.fingerprint,
			key_type    = EXCLUDED.key_type,
			pinned_at   = now(),
			pinned_by   = EXCLUDED.pinned_by`,
		p.Host, p.Fingerprint, p.KeyType, p.PinnedBy)
	return err
}

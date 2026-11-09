package control

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Just-in-time cloud console access, as stored: one row per grant. A grant is
awaiting_approval, then active from starts_at to ends_at; one that has
passed ends_at reads as expired without anything writing it. See
cloud_access_routes.go for who may ask, decide and open sessions.
*/

// CloudAccess is one grant to an account's console and command line.
type CloudAccess struct {
	ID            string             `json:"id"`
	AccountID     string             `json:"accountId"`
	AccountName   string             `json:"accountName"`
	CloudAccount  string             `json:"cloudAccountId"`
	Level         cloud.ConsoleLevel `json:"level"`
	Environment   string             `json:"environment"`
	Minutes       int                `json:"minutes"`
	Reason        string             `json:"reason"`
	State         string             `json:"state"`
	RequestedBy   string             `json:"requestedBy"`
	RequestedAt   time.Time          `json:"requestedAt"`
	DecidedBy     *string            `json:"decidedBy"`
	DecidedAt     *time.Time         `json:"decidedAt"`
	StartsAt      *time.Time         `json:"startsAt"`
	EndsAt        *time.Time         `json:"endsAt"`
	EndedBy       *string            `json:"endedBy"`
	Sessions      int                `json:"sessions"`
	LastSessionAt *time.Time         `json:"lastSessionAt"`
}

// Live reports whether the grant is in force at t.
func (g CloudAccess) Live(t time.Time) bool {
	return g.State == "active" && g.EndsAt != nil && t.Before(*g.EndsAt)
}

var (
	// ErrCloudAccessNotFound is an unknown grant.
	ErrCloudAccessNotFound = errors.New("no such cloud access")
	// ErrCloudAccessOpen is a second grant asked for while one waits or runs.
	ErrCloudAccessOpen = errors.New("you already have this access to this account, or are waiting for it; use that one, or end it first")
	// ErrCloudAccessSettled is a grant no longer in the state the call needs.
	ErrCloudAccessSettled = errors.New("this access is no longer waiting for a decision")
)

const cloudAccessCols = `g.id::text, g.account_id::text, a.name, a.account_id, g.level, g.environment, g.minutes, g.reason,
	CASE WHEN g.state = 'active' AND g.ends_at <= now() THEN 'expired' ELSE g.state END,
	g.requested_by, g.requested_at, g.decided_by, g.decided_at, g.starts_at, g.ends_at, g.ended_by, g.sessions, g.last_session_at`

func scanCloudAccess(row pgx.Row) (CloudAccess, error) {
	var g CloudAccess
	err := row.Scan(&g.ID, &g.AccountID, &g.AccountName, &g.CloudAccount, &g.Level, &g.Environment, &g.Minutes, &g.Reason, &g.State,
		&g.RequestedBy, &g.RequestedAt, &g.DecidedBy, &g.DecidedAt, &g.StartsAt, &g.EndsAt, &g.EndedBy, &g.Sessions, &g.LastSessionAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudAccess{}, ErrCloudAccessNotFound
	}
	return g, err
}

// CreateCloudAccess records a grant: waiting, or active from now when it
// needs nobody else.
func (s *Store) CreateCloudAccess(ctx context.Context, g CloudAccess) (CloudAccess, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		// One open grant per person, account and level; the lock keeps two
		// requests at once from both getting through.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('cloud_access:' || $1 || $2))`, g.AccountID, g.RequestedBy); err != nil {
			return err
		}
		var open bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cloud_access WHERE account_id::text = $1 AND requested_by = $2
			AND level = $3 AND (state = 'awaiting_approval' OR (state = 'active' AND ends_at > now())))`,
			g.AccountID, g.RequestedBy, string(g.Level)).Scan(&open); err != nil {
			return err
		}
		if open {
			return ErrCloudAccessOpen
		}
		return tx.QueryRow(ctx, `INSERT INTO cloud_access (account_id, level, environment, minutes, reason, state, requested_by,
				starts_at, ends_at)
			VALUES ($1::uuid, $2, $3, $4::int, $5, $6::text, $7,
				CASE WHEN $6::text = 'active' THEN now() END, CASE WHEN $6::text = 'active' THEN now() + make_interval(mins => $4::int) END)
			RETURNING id::text`,
			g.AccountID, string(g.Level), g.Environment, g.Minutes, g.Reason, g.State, g.RequestedBy).Scan(&id)
	})
	if err != nil {
		return CloudAccess{}, err
	}
	return s.CloudAccessGrant(ctx, id)
}

// CloudAccessGrant is one grant.
func (s *Store) CloudAccessGrant(ctx context.Context, id string) (CloudAccess, error) {
	return scanCloudAccess(s.db(ctx).QueryRow(ctx, `SELECT `+cloudAccessCols+`
		FROM cloud_access g JOIN cloud_accounts a ON a.id = g.account_id WHERE g.id::text = $1`, id))
}

// CloudAccessList is the newest grants: everyone's, or one person's, or
// only those waiting.
func (s *Store) CloudAccessList(ctx context.Context, requester string, waiting bool, limit int) ([]CloudAccess, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+cloudAccessCols+`
		FROM cloud_access g JOIN cloud_accounts a ON a.id = g.account_id
		WHERE ($1 = '' OR g.requested_by = $1) AND (NOT $2 OR g.state = 'awaiting_approval')
		ORDER BY g.requested_at DESC LIMIT $3`, requester, waiting, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudAccess{}
	for rows.Next() {
		g, err := scanCloudAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) settleCloudAccess(ctx context.Context, id, query string, args ...any) (CloudAccess, error) {
	tag, err := s.db(ctx).Exec(ctx, query, append([]any{id}, args...)...)
	if err != nil {
		return CloudAccess{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.CloudAccessGrant(ctx, id); err != nil {
			return CloudAccess{}, err
		}
		return CloudAccess{}, ErrCloudAccessSettled
	}
	return s.CloudAccessGrant(ctx, id)
}

// DecideCloudAccess approves (starting it now) or declines a waiting grant.
func (s *Store) DecideCloudAccess(ctx context.Context, id, by string, approve bool) (CloudAccess, error) {
	if approve {
		return s.settleCloudAccess(ctx, id, `UPDATE cloud_access SET state = 'active', decided_by = $2, decided_at = now(),
			starts_at = now(), ends_at = now() + make_interval(mins => minutes) WHERE id::text = $1 AND state = 'awaiting_approval'`, by)
	}
	return s.settleCloudAccess(ctx, id, `UPDATE cloud_access SET state = 'declined', decided_by = $2, decided_at = now()
		WHERE id::text = $1 AND state = 'awaiting_approval'`, by)
}

// CancelCloudAccess withdraws a waiting grant; only the person who asked can.
func (s *Store) CancelCloudAccess(ctx context.Context, id, by string) (CloudAccess, error) {
	return s.settleCloudAccess(ctx, id, `UPDATE cloud_access SET state = 'cancelled', decided_by = $2, decided_at = now()
		WHERE id::text = $1 AND state = 'awaiting_approval' AND requested_by = $2`, by)
}

// EndCloudAccess ends a grant in force now. Sessions already open run out
// on their own, within the hour.
func (s *Store) EndCloudAccess(ctx context.Context, id, by string) (CloudAccess, error) {
	return s.settleCloudAccess(ctx, id, `UPDATE cloud_access SET state = 'ended', ended_by = $2, ends_at = now()
		WHERE id::text = $1 AND state = 'active' AND ends_at > now()`, by)
}

// NoteCloudAccessSession counts a session opened under a grant.
func (s *Store) NoteCloudAccessSession(ctx context.Context, id string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE cloud_access SET sessions = sessions + 1, last_session_at = now() WHERE id::text = $1`, id)
	return err
}

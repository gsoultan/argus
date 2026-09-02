package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AccessRequest is a time-bounded, justified ask for elevated access.
//
// This is the "just in time" half of PAM: nobody holds standing permission to
// open a root shell, they ask for it, someone else agrees, and the grant
// expires on its own. The expiry is what makes it work — a grant that must be
// remembered and revoked is a grant that never gets revoked.
type AccessRequest struct {
	ID              string     `json:"id"`
	RequesterEmail  string     `json:"requesterEmail"`
	AssetHostnames  []string   `json:"assetHostnames"`
	Principal       string     `json:"principal"`
	Justification   string     `json:"justification"`
	DurationMinutes int        `json:"durationMinutes"`
	State           string     `json:"state"`
	CreatedAt       time.Time  `json:"createdAt"`
	DecidedAt       *time.Time `json:"decidedAt"`
	DecidedByEmail  *string    `json:"decidedByEmail"`
	DecisionNote    *string    `json:"decisionNote"`
	ExpiresAt       *time.Time `json:"expiresAt"`
	BreakGlass      bool       `json:"breakGlass"`
}

var (
	// ErrSelfApproval means someone tried to decide their own request.
	ErrSelfApproval = errors.New("you cannot decide your own access request")
	// ErrAlreadyDecided means the request is no longer pending.
	ErrAlreadyDecided = errors.New("request has already been decided")
)

// MaxDurationMinutes caps how long any grant can last.
//
// A four-hour ceiling is not arbitrary: it is short enough that a forgotten
// grant expires within a working day, and long enough for real incident work.
const MaxDurationMinutes = 240

// CreateRequest records a new ask.
func (s *Store) CreateRequest(ctx context.Context, r AccessRequest) (AccessRequest, error) {
	if len(r.AssetHostnames) == 0 {
		return r, fmt.Errorf("at least one host is required")
	}
	if len(r.Justification) < 20 {
		// An approver cannot make a decision on "need access". Enforced here
		// rather than only in the form, because the form is not the boundary.
		return r, fmt.Errorf("justification must explain why, in at least 20 characters")
	}
	if r.DurationMinutes <= 0 || r.DurationMinutes > MaxDurationMinutes {
		return r, fmt.Errorf("duration must be between 1 and %d minutes", MaxDurationMinutes)
	}

	err := s.pool.QueryRow(ctx, `
		INSERT INTO access_requests
			(requester_email, asset_hostnames, principal, justification,
			 duration_minutes, state, break_glass)
		VALUES ($1,$2,$3,$4,$5,'pending',$6)
		RETURNING id::text, created_at`,
		r.RequesterEmail, r.AssetHostnames, r.Principal, r.Justification,
		r.DurationMinutes, r.BreakGlass).Scan(&r.ID, &r.CreatedAt)
	if err != nil {
		return r, fmt.Errorf("create request: %w", err)
	}
	r.State = "pending"
	return r, nil
}

// DecideRequest approves or denies, enforcing separation of duty.
func (s *Store) DecideRequest(ctx context.Context, id, decision, note, decidedBy string) (AccessRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccessRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row: two approvers clicking at once must not both decide, or the
	// audit log records a decision that never took effect.
	var requester, state string
	var duration int
	err = tx.QueryRow(ctx, `
		SELECT requester_email, state, duration_minutes
		FROM access_requests WHERE id = $1 FOR UPDATE`, id).
		Scan(&requester, &state, &duration)
	if err == pgx.ErrNoRows {
		return AccessRequest{}, fmt.Errorf("request not found")
	}
	if err != nil {
		return AccessRequest{}, err
	}
	if state != "pending" {
		return AccessRequest{}, fmt.Errorf("%w: it is %s", ErrAlreadyDecided, state)
	}

	// Separation of duty. Without it the approval step is decoration: anyone
	// could grant themselves root and the audit log would show a tidy approval
	// by the same person who asked.
	if requester == decidedBy {
		return AccessRequest{}, ErrSelfApproval
	}

	var expires *time.Time
	if decision == "approved" {
		t := time.Now().UTC().Add(time.Duration(duration) * time.Minute)
		expires = &t
	}

	var out AccessRequest
	err = tx.QueryRow(ctx, `
		UPDATE access_requests SET
			state = $2, decided_at = now(), decided_by_email = $3,
			decision_note = NULLIF($4,''), expires_at = $5
		WHERE id = $1
		RETURNING id::text, requester_email, asset_hostnames, principal,
		          justification, duration_minutes, state, created_at, decided_at,
		          decided_by_email, decision_note, expires_at, break_glass`,
		id, decision, decidedBy, note, expires).
		Scan(&out.ID, &out.RequesterEmail, &out.AssetHostnames, &out.Principal,
			&out.Justification, &out.DurationMinutes, &out.State, &out.CreatedAt,
			&out.DecidedAt, &out.DecidedByEmail, &out.DecisionNote, &out.ExpiresAt,
			&out.BreakGlass)
	if err != nil {
		return AccessRequest{}, err
	}
	return out, tx.Commit(ctx)
}

// Requests lists access requests, newest first.
func (s *Store) Requests(ctx context.Context, state string, limit int) ([]AccessRequest, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, requester_email, asset_hostnames, principal, justification,
		       duration_minutes, state, created_at, decided_at, decided_by_email,
		       decision_note, expires_at, break_glass
		FROM access_requests
		WHERE ($1 = '' OR state = $1)
		ORDER BY created_at DESC LIMIT $2`, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AccessRequest{}
	for rows.Next() {
		var r AccessRequest
		if err := rows.Scan(&r.ID, &r.RequesterEmail, &r.AssetHostnames, &r.Principal,
			&r.Justification, &r.DurationMinutes, &r.State, &r.CreatedAt, &r.DecidedAt,
			&r.DecidedByEmail, &r.DecisionNote, &r.ExpiresAt, &r.BreakGlass); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveGrant reports whether email currently holds an approved, unexpired
// grant for principal on host.
//
// This is what turns an approval into access. Checked at connect time rather
// than cached at approval time, so revoking or waiting out a grant takes effect
// immediately instead of at the next refresh.
func (s *Store) ActiveGrant(ctx context.Context, email, host, principal string) (bool, *time.Time, error) {
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT expires_at FROM access_requests
		WHERE requester_email = $1
		  AND principal       = $2
		  AND state           = 'approved'
		  AND expires_at      > now()
		  AND $3 = ANY(asset_hostnames)
		ORDER BY expires_at DESC
		LIMIT 1`, email, principal, host).Scan(&expires)
	if err == pgx.ErrNoRows {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, &expires, nil
}

// ExpireGrants marks approved requests whose window has closed.
//
// ActiveGrant already ignores expired rows, so this is cosmetic for
// enforcement and load-bearing for the console: a queue showing week-old
// "approved" grants trains people to ignore it.
func (s *Store) ExpireGrants(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE access_requests SET state = 'expired'
		WHERE state = 'approved' AND expires_at IS NOT NULL AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

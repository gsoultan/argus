package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// RestoreID is set when the request stands for restoring a database in
	// place; the approver's card shows what it would replace.
	RestoreID *string `json:"restoreId,omitempty"`
	// Statement is set when the request is to run statements on a
	// production database; the approver's card shows them.
	Statement *StatementAsk `json:"statement,omitempty"`
	// AsEditor is set on a change request the reader may decide as an
	// editor of its database, rather than by their role.
	AsEditor bool `json:"asEditor,omitempty"`
}

var (
	// ErrSelfApproval means someone tried to decide their own request.
	ErrSelfApproval = errors.New("you cannot decide your own access request")
	// ErrAlreadyDecided means the request is no longer pending. The error
	// returned is a requestClosed, which says who closed it and how.
	ErrAlreadyDecided = errors.New("request has already been decided")
	// ErrRequestNotFound is an id that names no request, or a malformed one.
	ErrRequestNotFound = errors.New("request not found")
	// ErrNotYourRequest is someone taking back a request they did not make.
	ErrNotYourRequest = errors.New("only the person who asked can cancel a request")
)

// requestClosed is a request that is no longer pending, in words the person
// who tried to act on it can act on: who closed it and how, rather than a
// bare "already decided".
type requestClosed struct {
	state string
	by    string
}

func (e requestClosed) Is(target error) bool { return target == ErrAlreadyDecided }

func (e requestClosed) Error() string {
	by := e.by
	if by == "" {
		by = "someone"
	}
	switch e.state {
	case "approved":
		return "this request was already approved by " + by
	case "denied":
		return "this request was already denied by " + by
	case "cancelled":
		return by + " cancelled this request; there is nothing left to decide"
	case "expired":
		return "this request was approved and has since expired"
	case "revoked":
		return "this request was approved and then revoked"
	}
	return "this request is " + e.state + ", not waiting for a decision"
}

// requestColumns are an AccessRequest's columns, in scanRequest's order.
const requestColumns = `id::text, requester_email, asset_hostnames, principal,
	justification, duration_minutes, state, created_at, decided_at,
	decided_by_email, decision_note, expires_at, break_glass, restore_id::text`

func scanRequest(row pgx.Row) (AccessRequest, error) {
	var r AccessRequest
	err := row.Scan(&r.ID, &r.RequesterEmail, &r.AssetHostnames, &r.Principal,
		&r.Justification, &r.DurationMinutes, &r.State, &r.CreatedAt, &r.DecidedAt,
		&r.DecidedByEmail, &r.DecisionNote, &r.ExpiresAt, &r.BreakGlass, &r.RestoreID)
	return r, err
}

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

	err := s.db(ctx).QueryRow(ctx, `
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
	if !uuidRE.MatchString(id) {
		return AccessRequest{}, ErrRequestNotFound
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return AccessRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row: two approvers clicking at once must not both decide, or the
	// audit log records a decision that never took effect.
	var requester, state string
	var by *string
	var duration int
	err = tx.QueryRow(ctx, `
		SELECT requester_email, state, duration_minutes, decided_by_email
		FROM access_requests WHERE id = $1 FOR UPDATE`, id).
		Scan(&requester, &state, &duration, &by)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessRequest{}, ErrRequestNotFound
	}
	if err != nil {
		return AccessRequest{}, err
	}
	if state != "pending" {
		return AccessRequest{}, requestClosed{state: state, by: deref(by)}
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

	out, err := scanRequest(tx.QueryRow(ctx, `
		UPDATE access_requests SET
			state = $2, decided_at = now(), decided_by_email = $3,
			decision_note = NULLIF($4,''), expires_at = $5
		WHERE id = $1
		RETURNING `+requestColumns,
		id, decision, decidedBy, note, expires))
	if err != nil {
		return AccessRequest{}, err
	}
	return out, tx.Commit(ctx)
}

// CancelRequest takes back a pending request, for the person who made it.
//
// Nobody else may: an approver who wants a request gone denies it, with a
// reason the requester reads. The row is locked as in DecideRequest, so a
// cancellation and a decision arriving together cannot both take effect.
func (s *Store) CancelRequest(ctx context.Context, id, email, note string) (AccessRequest, error) {
	if !uuidRE.MatchString(id) {
		return AccessRequest{}, ErrRequestNotFound
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return AccessRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var requester, state string
	var by *string
	err = tx.QueryRow(ctx, `
		SELECT requester_email, state, decided_by_email
		FROM access_requests WHERE id = $1 FOR UPDATE`, id).Scan(&requester, &state, &by)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessRequest{}, ErrRequestNotFound
	}
	if err != nil {
		return AccessRequest{}, err
	}
	if !strings.EqualFold(requester, email) {
		return AccessRequest{}, ErrNotYourRequest
	}
	if state != "pending" {
		return AccessRequest{}, requestClosed{state: state, by: deref(by)}
	}
	out, err := scanRequest(tx.QueryRow(ctx, `
		UPDATE access_requests SET
			state = 'cancelled', decided_at = now(), decided_by_email = $2,
			decision_note = NULLIF($3,'')
		WHERE id = $1
		RETURNING `+requestColumns, id, requester, note))
	if err != nil {
		return AccessRequest{}, err
	}
	return out, tx.Commit(ctx)
}

// Request reads one request, with its statements when it has any, and the
// database it is on, if any.
func (s *Store) Request(ctx context.Context, id string) (AccessRequest, string, error) {
	if !uuidRE.MatchString(id) {
		return AccessRequest{}, "", ErrRequestNotFound
	}
	var dbID *string
	var r AccessRequest
	err := s.db(ctx).QueryRow(ctx, `SELECT `+requestColumns+`, database_id::text
		FROM access_requests WHERE id = $1`, id).Scan(&r.ID, &r.RequesterEmail, &r.AssetHostnames, &r.Principal,
		&r.Justification, &r.DurationMinutes, &r.State, &r.CreatedAt, &r.DecidedAt,
		&r.DecidedByEmail, &r.DecisionNote, &r.ExpiresAt, &r.BreakGlass, &r.RestoreID, &dbID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessRequest{}, "", ErrRequestNotFound
	}
	if err != nil {
		return AccessRequest{}, "", err
	}
	if r.Principal == statementPrincipal {
		asks, err := s.StatementAsks(ctx, []string{r.ID})
		if err != nil {
			return AccessRequest{}, "", err
		}
		r.Statement = asks[r.ID]
	}
	return r, deref(dbID), nil
}

// Requests lists access requests, newest first. A non-empty requester narrows
// it to that person's own.
//
// With a requester, only theirs, and the change requests on the databases
// named in alsoDBs.
func (s *Store) Requests(ctx context.Context, state, requester string, limit int, alsoDBs ...string) ([]AccessRequest, error) {
	if alsoDBs == nil {
		alsoDBs = []string{}
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT `+requestColumns+`
		FROM access_requests
		WHERE ($1 = '' OR state = $1)
		  AND ($3 = '' OR lower(requester_email) = lower($3)
		       OR (principal = 'db:statement' AND database_id = ANY($4::uuid[])))
		ORDER BY created_at DESC LIMIT $2`, state, limit, requester, alsoDBs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AccessRequest{}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var ids []string
	for _, r := range out {
		if r.Principal == statementPrincipal {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) > 0 {
		asks, err := s.StatementAsks(ctx, ids)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].Statement = asks[out[i].ID]
		}
	}
	return out, nil
}

// ActiveGrant reports whether email currently holds an approved, unexpired
// grant for principal on host.
//
// This is what turns an approval into access. Checked at connect time rather
// than cached at approval time, so revoking or waiting out a grant takes effect
// immediately instead of at the next refresh.
func (s *Store) ActiveGrant(ctx context.Context, email, host, principal string) (bool, *time.Time, error) {
	var expires time.Time
	err := s.db(ctx).QueryRow(ctx, `
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
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE access_requests SET state = 'expired'
		WHERE state = 'approved' AND expires_at IS NOT NULL AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

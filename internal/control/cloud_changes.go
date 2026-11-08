package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud changes, as stored: one row per change, requested -> (approved) ->
applied or failed. See cloud_change_routes.go for who may ask and approve.
*/

// CloudChange is one change to a cloud resource.
type CloudChange struct {
	ID           string          `json:"id"`
	AccountID    string          `json:"accountId"`
	AccountName  string          `json:"accountName"`
	Region       string          `json:"region"`
	Action       cloud.Action    `json:"action"`
	Level        cloud.Level     `json:"level"`
	Environment  string          `json:"environment"`
	ResourceID   string          `json:"resourceId"`
	ResourceName string          `json:"resourceName"`
	Spec         json.RawMessage `json:"spec"`
	Plan         json.RawMessage `json:"plan"`
	PlanHash     string          `json:"planHash"`
	Summary      string          `json:"summary"`
	Reason       string          `json:"reason"`
	State        string          `json:"state"`
	RequestedBy  string          `json:"requestedBy"`
	RequestedAt  time.Time       `json:"requestedAt"`
	DecidedBy    *string         `json:"decidedBy"`
	DecidedAt    *time.Time      `json:"decidedAt"`
	AppliedAt    *time.Time      `json:"appliedAt"`
	RequestID    string          `json:"requestId"`
	Result       json.RawMessage `json:"result"`
	Error        string          `json:"error"`
}

// ErrCloudChangeBusy is a resource that already has a change waiting or
// running.
var ErrCloudChangeBusy = errors.New("this instance already has a change waiting or running; let that one finish first")

// ErrCloudChangeNotFound is an unknown change.
var ErrCloudChangeNotFound = errors.New("no such cloud change")

// ErrCloudChangeSettled is a change that is no longer waiting.
var ErrCloudChangeSettled = errors.New("this change is no longer waiting for a decision")

const cloudChangeCols = `c.id::text, c.account_id::text, a.name, c.region, c.action, c.level, c.environment,
	c.resource_id, c.resource_name, c.spec, c.plan, c.plan_hash, c.summary, c.reason, c.state,
	c.requested_by, c.requested_at, c.decided_by, c.decided_at, c.applied_at, c.request_id, c.result, c.error`

func scanCloudChange(row pgx.Row) (CloudChange, error) {
	var c CloudChange
	var spec, plan, result []byte
	err := row.Scan(&c.ID, &c.AccountID, &c.AccountName, &c.Region, &c.Action, &c.Level, &c.Environment,
		&c.ResourceID, &c.ResourceName, &spec, &plan, &c.PlanHash, &c.Summary, &c.Reason, &c.State,
		&c.RequestedBy, &c.RequestedAt, &c.DecidedBy, &c.DecidedAt, &c.AppliedAt, &c.RequestID, &result, &c.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudChange{}, ErrCloudChangeNotFound
	}
	c.Spec, c.Plan, c.Result = spec, plan, result
	return c, err
}

// CreateCloudChange records a change, waiting for approval or about to be
// applied.
func (s *Store) CreateCloudChange(ctx context.Context, c CloudChange) (CloudChange, error) {
	if len(c.Spec) == 0 {
		c.Spec = json.RawMessage("{}")
	}
	if len(c.Plan) == 0 {
		c.Plan = json.RawMessage("{}")
	}
	var id string
	err := s.db(ctx).QueryRow(ctx, `
		INSERT INTO cloud_changes (account_id, region, action, level, environment, resource_id, resource_name,
			spec, plan, plan_hash, summary, reason, state, requested_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id::text`,
		c.AccountID, c.Region, string(c.Action), string(c.Level), c.Environment, c.ResourceID, c.ResourceName,
		[]byte(c.Spec), []byte(c.Plan), c.PlanHash, c.Summary, c.Reason, c.State, c.RequestedBy).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return CloudChange{}, ErrCloudChangeBusy
		}
		return CloudChange{}, fmt.Errorf("record cloud change: %w", err)
	}
	return s.CloudChange(ctx, id)
}

// CloudChange reads one change.
func (s *Store) CloudChange(ctx context.Context, id string) (CloudChange, error) {
	return scanCloudChange(s.db(ctx).QueryRow(ctx, `SELECT `+cloudChangeCols+`
		FROM cloud_changes c JOIN cloud_accounts a ON a.id = c.account_id WHERE c.id::text = $1`, id))
}

// CloudChanges lists changes, newest first: only those waiting for a
// decision when pending, and only one resource's when resourceID is set.
func (s *Store) CloudChanges(ctx context.Context, pending bool, accountID, resourceID string, limit int) ([]CloudChange, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	where := []string{"true"}
	args := []any{}
	if pending {
		where = append(where, "c.state = 'awaiting_approval'")
	}
	if accountID != "" && resourceID != "" {
		args = append(args, accountID, resourceID)
		where = append(where, fmt.Sprintf("c.account_id::text = $%d AND c.resource_id = $%d", len(args)-1, len(args)))
	}
	args = append(args, limit)
	rows, err := s.db(ctx).Query(ctx, `SELECT `+cloudChangeCols+`
		FROM cloud_changes c JOIN cloud_accounts a ON a.id = c.account_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY c.requested_at DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudChange{}
	for rows.Next() {
		c, err := scanCloudChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DecideCloudChange approves (the change moves to applying) or declines a
// waiting change. Only one decision can win.
func (s *Store) DecideCloudChange(ctx context.Context, id, by string, approve bool) (CloudChange, error) {
	state := "declined"
	if approve {
		state = "applying"
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE cloud_changes SET state = $2, decided_by = $3, decided_at = now()
		WHERE id::text = $1 AND state = 'awaiting_approval'`, id, state, by)
	if err != nil {
		return CloudChange{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.CloudChange(ctx, id); err != nil {
			return CloudChange{}, err
		}
		return CloudChange{}, ErrCloudChangeSettled
	}
	return s.CloudChange(ctx, id)
}

// CancelCloudChange withdraws a waiting change; only the person who asked
// can.
func (s *Store) CancelCloudChange(ctx context.Context, id, by string) (CloudChange, error) {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE cloud_changes SET state = 'cancelled', decided_by = $2, decided_at = now()
		WHERE id::text = $1 AND state = 'awaiting_approval' AND requested_by = $2`, id, by)
	if err != nil {
		return CloudChange{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.CloudChange(ctx, id); err != nil {
			return CloudChange{}, err
		}
		return CloudChange{}, ErrCloudChangeSettled
	}
	return s.CloudChange(ctx, id)
}

// ReplanCloudChange puts a newer plan on an approved change that no longer
// matches what was approved, and sends it back for a decision.
func (s *Store) ReplanCloudChange(ctx context.Context, id string, plan []byte, hash, summary string) (CloudChange, error) {
	_, err := s.db(ctx).Exec(ctx, `UPDATE cloud_changes SET state = 'awaiting_approval', plan = $2, plan_hash = $3,
		summary = $4, decided_by = NULL, decided_at = NULL WHERE id::text = $1 AND state = 'applying'`, id, plan, hash, summary)
	if err != nil {
		return CloudChange{}, err
	}
	return s.CloudChange(ctx, id)
}

// FinishCloudChange records what an applying change did.
func (s *Store) FinishCloudChange(ctx context.Context, id string, ok bool, res cloud.Result, cause string) (CloudChange, error) {
	state := "applied"
	if !ok {
		state = "failed"
	}
	b, _ := json.Marshal(res)
	_, err := s.db(ctx).Exec(ctx, `UPDATE cloud_changes SET state = $2, applied_at = now(), request_id = $3,
		result = $4, error = $5, resource_id = CASE WHEN resource_id = '' THEN $6 ELSE resource_id END
		WHERE id::text = $1 AND state = 'applying'`, id, state, res.RequestID, b, cause, res.ResourceID)
	if err != nil {
		return CloudChange{}, err
	}
	return s.CloudChange(ctx, id)
}

// CloudInstance reads one instance as Argus last saw it.
func (s *Store) CloudInstance(ctx context.Context, accountID, region, id string) (cloud.Resource, error) {
	var data []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT data FROM cloud_resources
		WHERE account_id::text = $1 AND region = $2 AND kind = 'instance' AND provider_id = $3 AND gone_at IS NULL`,
		accountID, region, id).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return cloud.Resource{}, errors.New("Argus has not seen instance " + id + " in " + region + "; read the account again")
	}
	if err != nil {
		return cloud.Resource{}, err
	}
	var r cloud.Resource
	return r, json.Unmarshal(data, &r)
}

// CloudLaunchedServer is the name of the server Argus launched as id, if
// Argus launched it with the agent.
func (s *Store) CloudLaunchedServer(ctx context.Context, accountID, id string) (string, bool) {
	var name string
	err := s.db(ctx).QueryRow(ctx, `SELECT spec->>'name' FROM cloud_changes
		WHERE account_id::text = $1 AND resource_id = $2 AND action = 'launch' AND state = 'applied'
		AND (spec->>'agent')::boolean IS TRUE ORDER BY applied_at DESC LIMIT 1`, accountID, id).Scan(&name)
	return name, err == nil && name != ""
}

// SetCloudManageRole records the role changes are made with, or clears
// it, which makes the account read only.
func (s *Store) SetCloudManageRole(ctx context.Context, id, arn string) (CloudAccount, error) {
	access := "manage"
	var role any = arn
	if arn == "" {
		access, role = "read", nil
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE cloud_accounts SET manage_role_arn = $2, access = $3
		WHERE id::text = $1 AND archived_at IS NULL`, id, role, access)
	if err != nil {
		return CloudAccount{}, err
	}
	if tag.RowsAffected() == 0 {
		return CloudAccount{}, ErrCloudAccountNotFound
	}
	return s.CloudAccount(ctx, id)
}

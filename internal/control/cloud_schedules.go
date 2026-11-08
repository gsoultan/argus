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
Cloud schedules, as stored. See cloud_schedule_routes.go for who may do
what and cloud_scheduler.go for the loop that runs them.
*/

// CloudScheduleVersion is what a person sets on a schedule: what an edit
// changes and what an edit waiting for approval holds.
type CloudScheduleVersion struct {
	Name       string         `json:"name"`
	Timezone   string         `json:"timezone"`
	Windows    []cloud.Window `json:"windows"`
	SkipDates  []string       `json:"skipDates"`
	AccountIDs []string       `json:"accountIds"`
	Regions    []string       `json:"regions"`
	Mode       string         `json:"mode"`
}

// Plan is the version's timing, or why it has none.
func (v CloudScheduleVersion) Plan() (cloud.Plan, error) {
	loc, err := time.LoadLocation(v.Timezone)
	if err != nil || v.Timezone == "" || strings.EqualFold(v.Timezone, "local") {
		return cloud.Plan{}, fmt.Errorf("%q is not a time zone; pick one like Asia/Jakarta", v.Timezone)
	}
	return cloud.Plan{Zone: loc, Windows: v.Windows, Skip: v.SkipDates}, nil
}

// CloudSchedule is one schedule.
type CloudSchedule struct {
	ID string `json:"id"`
	CloudScheduleVersion
	Owner        string                `json:"owner"`
	State        string                `json:"state"`
	PausedReason string                `json:"pausedReason"`
	Pending      *CloudScheduleVersion `json:"pending,omitempty"`
	PendingBy    *string               `json:"pendingBy,omitempty"`
	PendingAt    *time.Time            `json:"pendingAt,omitempty"`
	Reason       string                `json:"reason"`
	CheckedAt    *time.Time            `json:"-"`
	CreatedBy    string                `json:"createdBy"`
	CreatedAt    time.Time             `json:"createdAt"`
	UpdatedBy    string                `json:"updatedBy"`
	UpdatedAt    time.Time             `json:"updatedAt"`
}

// Requester is who asked for what is waiting: the edit, or the schedule
// itself while it waits.
func (c CloudSchedule) Requester() string {
	if c.PendingBy != nil {
		return *c.PendingBy
	}
	return c.UpdatedBy
}

// Waiting reports whether something about the schedule waits for a
// decision.
func (c CloudSchedule) Waiting() bool { return c.State == "awaiting_approval" || c.Pending != nil }

// ErrCloudScheduleNotFound is an unknown schedule.
var ErrCloudScheduleNotFound = errors.New("no such schedule")

// ErrCloudScheduleExists is a name another schedule has.
var ErrCloudScheduleExists = errors.New("another schedule has this name")

// ErrCloudSchedulePending is an edit to a schedule that already has one
// waiting.
var ErrCloudSchedulePending = errors.New("an edit to this schedule is already waiting for a decision; decide or withdraw it first")

// ErrCloudScheduleSettled is a decision on a schedule with nothing waiting.
var ErrCloudScheduleSettled = errors.New("nothing about this schedule is waiting for a decision")

const cloudScheduleCols = `id::text, name, timezone, windows, skip_dates, account_ids::text[], regions, mode, owner,
	state, paused_reason, pending, pending_by, pending_at, reason, checked_at, created_by, created_at, updated_by, updated_at`

func scanCloudSchedule(row pgx.Row) (CloudSchedule, error) {
	var c CloudSchedule
	var windows, pending []byte
	err := row.Scan(&c.ID, &c.Name, &c.Timezone, &windows, &c.SkipDates, &c.AccountIDs, &c.Regions, &c.Mode, &c.Owner,
		&c.State, &c.PausedReason, &pending, &c.PendingBy, &c.PendingAt, &c.Reason, &c.CheckedAt, &c.CreatedBy,
		&c.CreatedAt, &c.UpdatedBy, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudSchedule{}, ErrCloudScheduleNotFound
	}
	if err != nil {
		return CloudSchedule{}, err
	}
	_ = json.Unmarshal(windows, &c.Windows)
	if len(pending) > 0 {
		var v CloudScheduleVersion
		if json.Unmarshal(pending, &v) == nil {
			c.Pending = &v
		}
	}
	c.CloudScheduleVersion = c.CloudScheduleVersion.norm()
	return c, nil
}

func (v CloudScheduleVersion) norm() CloudScheduleVersion {
	if v.Windows == nil {
		v.Windows = []cloud.Window{}
	}
	if v.SkipDates == nil {
		v.SkipDates = []string{}
	}
	if v.AccountIDs == nil {
		v.AccountIDs = []string{}
	}
	if v.Regions == nil {
		v.Regions = []string{}
	}
	return v
}

// CloudSchedules lists every schedule, by name.
func (s *Store) CloudSchedules(ctx context.Context) ([]CloudSchedule, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+cloudScheduleCols+` FROM cloud_schedules ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudSchedule{}
	for rows.Next() {
		c, err := scanCloudSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CloudSchedule reads one schedule.
func (s *Store) CloudSchedule(ctx context.Context, id string) (CloudSchedule, error) {
	return scanCloudSchedule(s.db(ctx).QueryRow(ctx, `SELECT `+cloudScheduleCols+` FROM cloud_schedules WHERE id::text = $1`, id))
}

// CreateCloudSchedule records a new schedule, active or waiting for
// approval. An active one is checked from now, so it does not act on
// transitions that came before it existed.
func (s *Store) CreateCloudSchedule(ctx context.Context, v CloudScheduleVersion, by, state, reason string) (CloudSchedule, error) {
	v = v.norm()
	w, _ := json.Marshal(v.Windows)
	var id string
	err := s.db(ctx).QueryRow(ctx, `
		INSERT INTO cloud_schedules (name, timezone, windows, skip_dates, account_ids, regions, mode, owner, state,
			reason, checked_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5::uuid[], $6, $7, $8, $9, $10, CASE WHEN $9 = 'active' THEN now() END, $8, $8)
		RETURNING id::text`,
		v.Name, v.Timezone, w, v.SkipDates, v.AccountIDs, v.Regions, v.Mode, by, state, reason).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return CloudSchedule{}, ErrCloudScheduleExists
		}
		return CloudSchedule{}, fmt.Errorf("record schedule: %w", err)
	}
	return s.CloudSchedule(ctx, id)
}

// UpdateCloudSchedule puts a version in effect at once. Its author owns
// the schedule from now on.
func (s *Store) UpdateCloudSchedule(ctx context.Context, id string, v CloudScheduleVersion, by, reason string) (CloudSchedule, error) {
	v = v.norm()
	w, _ := json.Marshal(v.Windows)
	var got string
	err := s.db(ctx).QueryRow(ctx, `UPDATE cloud_schedules SET name = $2, timezone = $3, windows = $4, skip_dates = $5,
		account_ids = $6::uuid[], regions = $7, mode = $8, owner = $9, updated_by = $9, updated_at = now(), reason = $10,
		pending = NULL, pending_by = NULL, pending_at = NULL
		WHERE id::text = $1 RETURNING id::text`,
		id, v.Name, v.Timezone, w, v.SkipDates, v.AccountIDs, v.Regions, v.Mode, by, reason).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudSchedule{}, ErrCloudScheduleNotFound
	}
	if err != nil {
		if isUniqueViolation(err) {
			return CloudSchedule{}, ErrCloudScheduleExists
		}
		return CloudSchedule{}, err
	}
	return s.CloudSchedule(ctx, id)
}

// SetCloudSchedulePending holds an edit for a decision, leaving the version
// in effect running.
func (s *Store) SetCloudSchedulePending(ctx context.Context, id string, v CloudScheduleVersion, by, reason string) (CloudSchedule, error) {
	v = v.norm()
	b, _ := json.Marshal(v)
	tag, err := s.db(ctx).Exec(ctx, `UPDATE cloud_schedules SET pending = $2, pending_by = $3, pending_at = now(), reason = $4
		WHERE id::text = $1 AND pending IS NULL`, id, b, by, reason)
	if err != nil {
		return CloudSchedule{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.CloudSchedule(ctx, id); err != nil {
			return CloudSchedule{}, err
		}
		return CloudSchedule{}, ErrCloudSchedulePending
	}
	return s.CloudSchedule(ctx, id)
}

// DecideCloudSchedule approves or declines what waits on a schedule. A new
// schedule approved becomes active; declined, it is deleted. An edit
// approved takes effect, owned by the person who asked; declined, it is
// dropped. ok is false when the schedule was deleted.
func (s *Store) DecideCloudSchedule(ctx context.Context, id string, approve bool) (CloudSchedule, bool, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return CloudSchedule{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanCloudSchedule(tx.QueryRow(ctx, `SELECT `+cloudScheduleCols+` FROM cloud_schedules WHERE id::text = $1 FOR UPDATE`, id))
	if err != nil {
		return CloudSchedule{}, false, err
	}
	kept := true
	switch {
	case c.Pending != nil && approve:
		if _, err := tx.Exec(ctx, `UPDATE cloud_schedules SET name = $2, timezone = $3, windows = $4, skip_dates = $5,
			account_ids = $6::uuid[], regions = $7, mode = $8, owner = pending_by, updated_by = pending_by,
			updated_at = now(), pending = NULL, pending_by = NULL, pending_at = NULL,
			state = CASE WHEN state = 'awaiting_approval' THEN 'active' ELSE state END,
			checked_at = CASE WHEN state = 'awaiting_approval' THEN now() ELSE checked_at END
			WHERE id::text = $1`, id, c.Pending.Name, c.Pending.Timezone, mustJSON(c.Pending.Windows), c.Pending.SkipDates,
			c.Pending.AccountIDs, c.Pending.Regions, c.Pending.Mode); err != nil {
			if isUniqueViolation(err) {
				return CloudSchedule{}, false, ErrCloudScheduleExists
			}
			return CloudSchedule{}, false, err
		}
	case c.Pending != nil:
		_, err = tx.Exec(ctx, `UPDATE cloud_schedules SET pending = NULL, pending_by = NULL, pending_at = NULL WHERE id::text = $1`, id)
	case c.State == "awaiting_approval" && approve:
		_, err = tx.Exec(ctx, `UPDATE cloud_schedules SET state = 'active', checked_at = now() WHERE id::text = $1`, id)
	case c.State == "awaiting_approval":
		kept = false
		_, err = tx.Exec(ctx, `DELETE FROM cloud_schedules WHERE id::text = $1`, id)
	default:
		return CloudSchedule{}, false, ErrCloudScheduleSettled
	}
	if err != nil {
		return CloudSchedule{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CloudSchedule{}, false, err
	}
	if !kept {
		return c, false, nil
	}
	out, err := s.CloudSchedule(ctx, id)
	return out, true, err
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// PauseCloudSchedule stops a schedule acting, with why.
func (s *Store) PauseCloudSchedule(ctx context.Context, id, reason string) (CloudSchedule, error) {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE cloud_schedules SET state = 'paused', paused_reason = $2
		WHERE id::text = $1 AND state = 'active'`, id, reason)
	if err != nil {
		return CloudSchedule{}, err
	}
	if tag.RowsAffected() == 0 {
		c, err := s.CloudSchedule(ctx, id)
		if err != nil {
			return CloudSchedule{}, err
		}
		return c, ErrCloudScheduleSettled
	}
	return s.CloudSchedule(ctx, id)
}

// ResumeCloudSchedule makes a paused schedule act again, owned by the
// person resuming it. Transitions that fell while it was paused are not
// made up.
func (s *Store) ResumeCloudSchedule(ctx context.Context, id, by string) (CloudSchedule, error) {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE cloud_schedules SET state = 'active', paused_reason = '', owner = $2,
		checked_at = now() WHERE id::text = $1 AND state = 'paused'`, id, by)
	if err != nil {
		return CloudSchedule{}, err
	}
	if tag.RowsAffected() == 0 {
		c, err := s.CloudSchedule(ctx, id)
		if err != nil {
			return CloudSchedule{}, err
		}
		return c, ErrCloudScheduleSettled
	}
	return s.CloudSchedule(ctx, id)
}

// DeleteCloudSchedule removes a schedule and its runs.
func (s *Store) DeleteCloudSchedule(ctx context.Context, id string) error {
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM cloud_schedules WHERE id::text = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrCloudScheduleNotFound
	}
	return nil
}

// SetCloudScheduleChecked records up to when the loop has looked.
func (s *Store) SetCloudScheduleChecked(ctx context.Context, id string, at time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE cloud_schedules SET checked_at = $2
		WHERE id::text = $1 AND (checked_at IS NULL OR checked_at < $2)`, id, at)
	return err
}

/* ── Runs ────────────────────────────────────────────────────────────────── */

// CloudRunTarget is one instance in a run: what was done to it, or why
// not.
type CloudRunTarget struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Region      string `json:"region"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	// Status is done, failed, skipped or waiting (for a session to end).
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	RequestID string     `json:"requestId,omitempty"`
	WaitUntil *time.Time `json:"waitUntil,omitempty"`
	Noticed   bool       `json:"noticed,omitempty"`
	Tries     int        `json:"tries,omitempty"`
}

// CloudScheduleRun is one transition of one schedule.
type CloudScheduleRun struct {
	ID         string           `json:"id"`
	ScheduleID string           `json:"scheduleId"`
	DueAt      time.Time        `json:"dueAt"`
	Action     cloud.Action     `json:"action"`
	State      string           `json:"state"`
	Targets    []CloudRunTarget `json:"targets"`
	RequestIDs []string         `json:"requestIds"`
	Detail     string           `json:"detail"`
	Attempts   int              `json:"attempts"`
	NextAt     *time.Time       `json:"nextAt,omitempty"`
	StartedAt  time.Time        `json:"startedAt"`
	FinishedAt *time.Time       `json:"finishedAt,omitempty"`
}

const cloudRunCols = `id::text, schedule_id::text, due_at, action, state, targets, request_ids, detail, attempts,
	next_at, started_at, finished_at`

func scanCloudRun(row pgx.Row) (CloudScheduleRun, error) {
	var r CloudScheduleRun
	var targets []byte
	err := row.Scan(&r.ID, &r.ScheduleID, &r.DueAt, &r.Action, &r.State, &targets, &r.RequestIDs, &r.Detail, &r.Attempts,
		&r.NextAt, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return r, err
	}
	_ = json.Unmarshal(targets, &r.Targets)
	if r.Targets == nil {
		r.Targets = []CloudRunTarget{}
	}
	if r.RequestIDs == nil {
		r.RequestIDs = []string{}
	}
	return r, nil
}

// ClaimCloudScheduleRun records that this host runs one transition. ok is
// false when another host (or an earlier tick) already has it.
func (s *Store) ClaimCloudScheduleRun(ctx context.Context, scheduleID string, due time.Time, act cloud.Action) (CloudScheduleRun, bool, error) {
	r, err := scanCloudRun(s.db(ctx).QueryRow(ctx, `INSERT INTO cloud_schedule_runs (schedule_id, due_at, action, state)
		VALUES ($1::uuid, $2, $3, 'running') ON CONFLICT (schedule_id, due_at, action) DO NOTHING
		RETURNING `+cloudRunCols, scheduleID, due, string(act)))
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudScheduleRun{}, false, nil
	}
	return r, err == nil, err
}

// SaveCloudScheduleRun records where a run is.
func (s *Store) SaveCloudScheduleRun(ctx context.Context, r CloudScheduleRun) error {
	if r.Targets == nil {
		r.Targets = []CloudRunTarget{}
	}
	if r.RequestIDs == nil {
		r.RequestIDs = []string{}
	}
	t, _ := json.Marshal(r.Targets)
	_, err := s.db(ctx).Exec(ctx, `UPDATE cloud_schedule_runs SET state = $2, targets = $3, request_ids = $4, detail = $5,
		attempts = $6, next_at = $7, finished_at = $8 WHERE id::text = $1`,
		r.ID, r.State, t, r.RequestIDs, r.Detail, r.Attempts, r.NextAt, r.FinishedAt)
	return err
}

// cloudRunLease is how long a host holds a run it took up again before
// another host may take it.
const cloudRunLease = 5 * time.Minute

// TakeDueCloudScheduleRuns hands this host the runs due another look: a
// retry, or an instance waiting for a session. Each is leased for a few
// minutes, so another host skips it while this one works on it.
func (s *Store) TakeDueCloudScheduleRuns(ctx context.Context, now time.Time, limit int) ([]CloudScheduleRun, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT `+cloudRunCols+` FROM cloud_schedule_runs
		WHERE next_at IS NOT NULL AND next_at <= $1 ORDER BY next_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	var out []CloudScheduleRun
	for rows.Next() {
		r, err := scanCloudRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		if _, err := tx.Exec(ctx, `UPDATE cloud_schedule_runs SET next_at = $2 WHERE id::text = $1`, r.ID, now.Add(cloudRunLease)); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit(ctx)
}

// CloudScheduleRuns lists a schedule's runs, newest first.
func (s *Store) CloudScheduleRuns(ctx context.Context, scheduleID string, limit int) ([]CloudScheduleRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+cloudRunCols+` FROM cloud_schedule_runs
		WHERE schedule_id::text = $1 ORDER BY due_at DESC, action LIMIT $2`, scheduleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudScheduleRun{}
	for rows.Next() {
		r, err := scanCloudRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ── Keep running ────────────────────────────────────────────────────────── */

// CloudKeepRunning is one instance every schedule leaves alone for now.
type CloudKeepRunning struct {
	AccountID  string    `json:"accountId"`
	InstanceID string    `json:"instanceId"`
	Until      time.Time `json:"until"`
	Reason     string    `json:"reason"`
	SetBy      string    `json:"setBy"`
	SetAt      time.Time `json:"setAt"`
}

// SetCloudKeepRunning keeps one instance running until a time.
func (s *Store) SetCloudKeepRunning(ctx context.Context, k CloudKeepRunning) error {
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO cloud_keep_running (account_id, instance_id, until, reason, set_by)
		VALUES ($1::uuid, $2, $3, $4, $5)
		ON CONFLICT (account_id, instance_id) DO UPDATE SET until = $3, reason = $4, set_by = $5, set_at = now()`,
		k.AccountID, k.InstanceID, k.Until, k.Reason, k.SetBy)
	return err
}

// ClearCloudKeepRunning lets schedules act on an instance again. ok is
// false when it was not being kept.
func (s *Store) ClearCloudKeepRunning(ctx context.Context, accountID, instanceID string) (CloudKeepRunning, bool, error) {
	var k CloudKeepRunning
	err := s.db(ctx).QueryRow(ctx, `DELETE FROM cloud_keep_running WHERE account_id::text = $1 AND instance_id = $2
		RETURNING account_id::text, instance_id, until, reason, set_by, set_at`, accountID, instanceID).
		Scan(&k.AccountID, &k.InstanceID, &k.Until, &k.Reason, &k.SetBy, &k.SetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return k, false, nil
	}
	return k, err == nil, err
}

// CloudKeepRunningNow lists the instances kept running at now, keyed by
// account and instance.
func (s *Store) CloudKeepRunningNow(ctx context.Context, now time.Time) (map[string]CloudKeepRunning, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT account_id::text, instance_id, until, reason, set_by, set_at
		FROM cloud_keep_running WHERE until > $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CloudKeepRunning{}
	for rows.Next() {
		var k CloudKeepRunning
		if err := rows.Scan(&k.AccountID, &k.InstanceID, &k.Until, &k.Reason, &k.SetBy, &k.SetAt); err != nil {
			return nil, err
		}
		out[k.AccountID+"/"+k.InstanceID] = k
	}
	return out, rows.Err()
}

// cloudOpenChanges is every instance with a change waiting or running,
// keyed by account and instance.
func (s *Store) cloudOpenChanges(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT account_id::text, resource_id FROM cloud_changes
		WHERE state IN ('awaiting_approval', 'applying') AND resource_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a, r string
		if err := rows.Scan(&a, &r); err != nil {
			return nil, err
		}
		out[a+"/"+r] = true
	}
	return out, rows.Err()
}

// liveSessionUsers is who has a live session on each server, by asset id.
func (s *Store) liveSessionUsers(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT DISTINCT asset_id::text, user_email FROM sessions
		WHERE state = 'active' AND ended_at IS NULL AND asset_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var a, u string
		if err := rows.Scan(&a, &u); err != nil {
			return nil, err
		}
		out[a] = append(out[a], u)
	}
	return out, rows.Err()
}

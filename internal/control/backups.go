package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Backups and restores of a database: the rows. The jobs that fill them are in
backup_jobs.go, the routes in backup_routes.go. Design and mockups:
plans/db-backup-restore in the project files.
*/

// BackupSchedule is when a database is backed up and what is kept.
type BackupSchedule struct {
	DatabaseID string `json:"databaseId"`
	Enabled    bool   `json:"enabled"`
	// Every is hourly, daily or weekly; AtHour and AtMinute the time of day
	// (control plane time), Weekday 0 = Sunday for weekly.
	Every       string     `json:"every"`
	AtHour      int        `json:"atHour"`
	AtMinute    int        `json:"atMinute"`
	Weekday     int        `json:"weekday"`
	Destination string     `json:"destination"`
	KeepLast    int        `json:"keepLast"`
	KeepDays    int        `json:"keepDays"`
	NextRunAt   *time.Time `json:"nextRunAt,omitempty"`
	// Misses is scheduled runs in a row that produced no backup: missed,
	// or started and failed.
	Misses    int       `json:"misses"`
	UpdatedBy string    `json:"updatedBy"`
	UpdatedAt time.Time `json:"updatedAt"`
	// TestEvery is when the newest backup is test-restored: off, after
	// (every scheduled backup), daily or weekly, at TestHour:TestMinute on
	// TestWeekday. TestChecks are the admin's own checks.
	TestEvery     string             `json:"testEvery"`
	TestHour      int                `json:"testHour"`
	TestMinute    int                `json:"testMinute"`
	TestWeekday   int                `json:"testWeekday"`
	TestNextAt    *time.Time         `json:"testNextAt,omitempty"`
	TestChecks    []tunnel.TestCheck `json:"testChecks"`
	TestOverdueAt *time.Time         `json:"-"`
	// Exists says a row was saved; the zero schedule is what the console
	// shows before one is.
	Exists bool `json:"exists"`
}

// DefaultBackupSchedule is what a database has before anyone sets one:
// nothing scheduled, nightly at two when they do.
func DefaultBackupSchedule(dbID string) BackupSchedule {
	return BackupSchedule{DatabaseID: dbID, Enabled: false, Every: "daily", AtHour: 2,
		Destination: tunnel.BackupDestServer, KeepLast: 14,
		TestEvery: "weekly", TestHour: 4, TestWeekday: 0, TestChecks: []tunnel.TestCheck{}}
}

func (b *BackupSchedule) validate() error {
	switch b.Every {
	case "hourly", "daily", "weekly":
	default:
		return ErrInvalidDatabase{"every must be hourly, daily or weekly"}
	}
	if b.AtHour < 0 || b.AtHour > 23 || b.AtMinute < 0 || b.AtMinute > 59 || b.Weekday < 0 || b.Weekday > 6 {
		return ErrInvalidDatabase{"the time of day is out of range"}
	}
	switch b.Destination {
	case tunnel.BackupDestServer, tunnel.BackupDestControl, tunnel.BackupDestBoth:
	default:
		return ErrInvalidDatabase{"destination must be server, control or both"}
	}
	if b.KeepLast < 1 || b.KeepLast > 1000 || b.KeepDays < 0 || b.KeepDays > 3650 {
		return ErrInvalidDatabase{"keep the last 1 to 1000 runs, and 0 to 3650 days"}
	}
	switch b.TestEvery {
	case "off", "daily", "weekly":
	case "after":
		if b.Every == "hourly" {
			return ErrInvalidDatabase{"a test restore after every hourly backup would never finish; test daily or weekly"}
		}
	default:
		return ErrInvalidDatabase{"test restores run off, after every backup, daily or weekly"}
	}
	if b.TestHour < 0 || b.TestHour > 23 || b.TestMinute < 0 || b.TestMinute > 59 || b.TestWeekday < 0 || b.TestWeekday > 6 {
		return ErrInvalidDatabase{"the test restore's time of day is out of range"}
	}
	if b.TestChecks == nil {
		b.TestChecks = []tunnel.TestCheck{}
	}
	for i := range b.TestChecks {
		b.TestChecks[i].Name = strings.TrimSpace(b.TestChecks[i].Name)
		b.TestChecks[i].SQL = strings.TrimSpace(b.TestChecks[i].SQL)
	}
	if err := tunnel.ValidateTestChecks(b.TestChecks); err != nil {
		return ErrInvalidDatabase{err.Error()}
	}
	return nil
}

// TestNextRun is when the next scheduled test restore is due after now;
// nil when tests are off or follow each backup.
func (b BackupSchedule) TestNextRun(now time.Time) *time.Time {
	switch b.TestEvery {
	case "daily", "weekly":
		every := map[string]string{"daily": "daily", "weekly": "weekly"}[b.TestEvery]
		t := BackupSchedule{Every: every, AtHour: b.TestHour, AtMinute: b.TestMinute, Weekday: b.TestWeekday}.NextRun(now)
		return &t
	}
	return nil
}

// TestInterval is how often a test should pass, for the overdue alert;
// zero when tests are off.
func (b BackupSchedule) TestInterval() time.Duration {
	switch b.TestEvery {
	case "daily":
		return 24 * time.Hour
	case "weekly":
		return 7 * 24 * time.Hour
	case "after":
		if b.Every == "weekly" {
			return 7 * 24 * time.Hour
		}
		return 24 * time.Hour
	}
	return 0
}

// DescribeTests is the test schedule in the console's words.
func (b BackupSchedule) DescribeTests() string {
	at := fmt.Sprintf("%02d:%02d", b.TestHour, b.TestMinute)
	switch b.TestEvery {
	case "after":
		return "after every scheduled backup"
	case "daily":
		return "every day at " + at
	case "weekly":
		return "every " + time.Weekday(b.TestWeekday).String() + " at " + at
	}
	return "off"
}

// NextRun is the first time after now the schedule fires.
func (b BackupSchedule) NextRun(now time.Time) time.Time {
	now = now.Local()
	switch b.Every {
	case "hourly":
		t := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), b.AtMinute, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(time.Hour)
		}
		return t
	case "weekly":
		t := time.Date(now.Year(), now.Month(), now.Day(), b.AtHour, b.AtMinute, 0, 0, now.Location())
		for int(t.Weekday()) != b.Weekday || !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), b.AtHour, b.AtMinute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Describe is the schedule in the console's words.
func (b BackupSchedule) Describe() string {
	if !b.Enabled {
		return "not scheduled"
	}
	at := fmt.Sprintf("%02d:%02d", b.AtHour, b.AtMinute)
	switch b.Every {
	case "hourly":
		return fmt.Sprintf("every hour at :%02d", b.AtMinute)
	case "weekly":
		return "every " + time.Weekday(b.Weekday).String() + " at " + at
	}
	return "every day at " + at
}

// DatabaseBackup is one backup, running or kept.
type DatabaseBackup struct {
	ID            string     `json:"id"`
	DatabaseID    string     `json:"databaseId"`
	StartedBy     string     `json:"startedBy"`
	Reason        string     `json:"reason"`
	State         string     `json:"state"`
	Stage         string     `json:"stage"`
	Detail        string     `json:"detail"`
	Error         string     `json:"error"`
	Destination   string     `json:"destination"`
	OnServer      bool       `json:"onServer"`
	OnControl     bool       `json:"onControl"`
	Bytes         int64      `json:"bytes"`
	SHA256        string     `json:"sha256"`
	KeyID         string     `json:"keyId"`
	ServerVersion string     `json:"serverVersion"`
	DumpVersion   string     `json:"dumpVersion"`
	Verified      bool       `json:"verified"`
	Pinned        bool       `json:"pinned"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	ReportedAt    time.Time  `json:"reportedAt"`
	// not sent to the console
	localPath  string
	storageKey string
	wrappedKey string
}

// StorageKey and LocalPath are where the file is.
func (b DatabaseBackup) StorageKey() string { return b.storageKey }
func (b DatabaseBackup) LocalPath() string  { return b.localPath }

// DatabaseRestore is one restore, asked for, running or done.
type DatabaseRestore struct {
	ID                string     `json:"id"`
	BackupID          string     `json:"backupId"`
	DatabaseID        string     `json:"databaseId"`
	Mode              string     `json:"mode"`
	NewName           string     `json:"newName"`
	RequestedBy       string     `json:"requestedBy"`
	Reason            string     `json:"reason"`
	RequestID         *string    `json:"requestId,omitempty"`
	State             string     `json:"state"`
	Stage             string     `json:"stage"`
	Detail            string     `json:"detail"`
	Error             string     `json:"error"`
	RenamedTo         string     `json:"renamedTo"`
	KeepUntil         *time.Time `json:"keepUntil,omitempty"`
	KeptDropped       bool       `json:"keptDropped"`
	CreatedDatabaseID *string    `json:"createdDatabaseId,omitempty"`
	Warnings          int        `json:"warnings"`
	CreatedAt         time.Time  `json:"createdAt"`
	StartedAt         *time.Time `json:"startedAt,omitempty"`
	FinishedAt        *time.Time `json:"finishedAt,omitempty"`
	ReportedAt        time.Time  `json:"reportedAt"`
	// BackupStartedAt and BackupBytes describe the backup, for the card.
	BackupStartedAt time.Time `json:"backupStartedAt"`
	BackupBytes     int64     `json:"backupBytes"`
	BackupReason    string    `json:"backupReason"`
	BackupBy        string    `json:"backupBy"`
	DBName          string    `json:"dbname"`
	Hostname        string    `json:"hostname"`
}

// ErrBackupNotFound and ErrRestoreNotFound are what they say.
var (
	ErrBackupNotFound  = errors.New("no such backup")
	ErrRestoreNotFound = errors.New("no such restore")
)

func notUUID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")
}

/* ── Schedules ───────────────────────────────────────────────────────────── */

const scheduleCols = `database_id::text, enabled, every, at_hour, at_minute, weekday, destination,
	keep_last, keep_days, next_run_at, misses, updated_by, updated_at,
	test_every, test_hour, test_minute, test_weekday, test_next_at, test_checks, test_overdue_at`

func scanSchedule(row pgx.Row) (BackupSchedule, error) {
	var b BackupSchedule
	var checks []byte
	err := row.Scan(&b.DatabaseID, &b.Enabled, &b.Every, &b.AtHour, &b.AtMinute, &b.Weekday, &b.Destination,
		&b.KeepLast, &b.KeepDays, &b.NextRunAt, &b.Misses, &b.UpdatedBy, &b.UpdatedAt,
		&b.TestEvery, &b.TestHour, &b.TestMinute, &b.TestWeekday, &b.TestNextAt, &checks, &b.TestOverdueAt)
	b.Exists = err == nil
	b.TestChecks = []tunnel.TestCheck{}
	if len(checks) > 0 {
		_ = json.Unmarshal(checks, &b.TestChecks)
	}
	return b, err
}

// BackupSchedule is the database's schedule, the default when none is saved.
func (s *Store) BackupSchedule(ctx context.Context, dbID string) (BackupSchedule, error) {
	b, err := scanSchedule(s.db(ctx).QueryRow(ctx,
		`SELECT `+scheduleCols+` FROM database_backup_schedules WHERE database_id = $1::uuid`, dbID))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return DefaultBackupSchedule(dbID), nil
	}
	return b, err
}

// SetBackupSchedule saves the schedule and computes its next runs.
func (s *Store) SetBackupSchedule(ctx context.Context, b BackupSchedule, actor string) (BackupSchedule, error) {
	if b.TestEvery == "" {
		b.TestEvery = "off"
	}
	if err := b.validate(); err != nil {
		return b, err
	}
	var next *time.Time
	if b.Enabled {
		t := b.NextRun(time.Now())
		next = &t
	}
	checks, _ := json.Marshal(b.TestChecks)
	out, err := scanSchedule(s.db(ctx).QueryRow(ctx, `
		INSERT INTO database_backup_schedules (database_id, enabled, every, at_hour, at_minute, weekday,
			destination, keep_last, keep_days, next_run_at, misses, updated_by, updated_at,
			test_every, test_hour, test_minute, test_weekday, test_next_at, test_checks)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, $11, now(), $12, $13, $14, $15, $16, $17::jsonb)
		ON CONFLICT (database_id) DO UPDATE SET enabled = EXCLUDED.enabled, every = EXCLUDED.every,
			at_hour = EXCLUDED.at_hour, at_minute = EXCLUDED.at_minute, weekday = EXCLUDED.weekday,
			destination = EXCLUDED.destination, keep_last = EXCLUDED.keep_last, keep_days = EXCLUDED.keep_days,
			next_run_at = EXCLUDED.next_run_at, misses = 0, updated_by = EXCLUDED.updated_by, updated_at = now(),
			test_every = EXCLUDED.test_every, test_hour = EXCLUDED.test_hour, test_minute = EXCLUDED.test_minute,
			test_weekday = EXCLUDED.test_weekday, test_next_at = EXCLUDED.test_next_at,
			test_checks = EXCLUDED.test_checks, test_overdue_at = NULL
		RETURNING `+scheduleCols,
		b.DatabaseID, b.Enabled, b.Every, b.AtHour, b.AtMinute, b.Weekday, b.Destination, b.KeepLast, b.KeepDays,
		next, actor, b.TestEvery, b.TestHour, b.TestMinute, b.TestWeekday, b.TestNextRun(time.Now()), string(checks)))
	if err != nil && (notUUID(err) || strings.Contains(err.Error(), "foreign key")) {
		return b, ErrDatabaseNotFound
	}
	return out, err
}

// DueBackupSchedules are the enabled schedules whose next run has passed.
func (s *Store) DueBackupSchedules(ctx context.Context, now time.Time) ([]BackupSchedule, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+scheduleCols+` FROM database_backup_schedules
		WHERE enabled AND next_run_at IS NOT NULL AND next_run_at <= $1 ORDER BY next_run_at`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupSchedule
	for rows.Next() {
		b, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AdvanceBackupSchedule moves the schedule past now, counting a miss when
// the run did not start. A run that starts leaves the count alone: whether
// it produced a backup is known when it ends (CountScheduledOutcome). It
// returns the count after.
func (s *Store) AdvanceBackupSchedule(ctx context.Context, b BackupSchedule, missed bool) (int, error) {
	next := b.NextRun(time.Now())
	var misses int
	err := s.db(ctx).QueryRow(ctx, `UPDATE database_backup_schedules
		SET next_run_at = $2, misses = CASE WHEN $3 THEN misses + 1 ELSE misses END
		WHERE database_id = $1::uuid RETURNING misses`, b.DatabaseID, next, missed).Scan(&misses)
	return misses, err
}

// CountScheduledOutcome records how a scheduled run ended: a failure adds to
// the runs in a row that produced no backup, a success clears them. It
// returns the count after.
func (s *Store) CountScheduledOutcome(ctx context.Context, dbID string, ok bool) (int, error) {
	var misses int
	err := s.db(ctx).QueryRow(ctx, `UPDATE database_backup_schedules
		SET misses = CASE WHEN $2 THEN 0 ELSE misses + 1 END
		WHERE database_id = $1::uuid RETURNING misses`, dbID, ok).Scan(&misses)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return misses, err
}

/* ── Backups ─────────────────────────────────────────────────────────────── */

const backupCols = `id::text, database_id::text, started_by, reason, state, stage, detail, error, destination,
	local_path, storage_key, bytes, sha256, wrapped_key, key_id, server_version, dump_version, verified, pinned,
	started_at, finished_at, reported_at`

func scanBackup(row pgx.Row) (DatabaseBackup, error) {
	var b DatabaseBackup
	err := row.Scan(&b.ID, &b.DatabaseID, &b.StartedBy, &b.Reason, &b.State, &b.Stage, &b.Detail, &b.Error,
		&b.Destination, &b.localPath, &b.storageKey, &b.Bytes, &b.SHA256, &b.wrappedKey, &b.KeyID,
		&b.ServerVersion, &b.DumpVersion, &b.Verified, &b.Pinned, &b.StartedAt, &b.FinishedAt, &b.ReportedAt)
	b.OnServer = b.localPath != ""
	b.OnControl = b.storageKey != ""
	return b, err
}

// ErrBackupRunning means a backup or restore of this database is going.
var ErrBackupRunning = errors.New("a backup of this database is already running")

// StartBackup records a running backup. One per database at a time.
func (s *Store) StartBackup(ctx context.Context, dbID, by, reason, destination string) (DatabaseBackup, error) {
	b, err := scanBackup(s.db(ctx).QueryRow(ctx, `
		INSERT INTO database_backups (database_id, started_by, reason, destination)
		VALUES ($1::uuid, $2, $3, $4) RETURNING `+backupCols, dbID, by, reason, destination))
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "database_backups_one_running"):
			return b, ErrBackupRunning
		case notUUID(err) || strings.Contains(err.Error(), "foreign key"):
			return b, ErrDatabaseNotFound
		}
	}
	return b, err
}

// RecordBackupOutcome writes a backup that never ran: missed or skipped.
func (s *Store) RecordBackupOutcome(ctx context.Context, dbID, by, state, why string) error {
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO database_backups (database_id, started_by, state, error, finished_at)
		VALUES ($1::uuid, $2, $3, $4, now())`, dbID, by, state, why)
	return err
}

// BackupProgress updates a running backup's stage.
func (s *Store) BackupProgress(ctx context.Context, id, stage, detail string, bytes int64) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_backups SET stage = $2, detail = $3, bytes = $4, reported_at = now()
		WHERE id = $1::uuid AND state = 'running'`, id, stage, detail, bytes)
	return err
}

// FinishBackup records how a backup ended.
func (s *Store) FinishBackup(ctx context.Context, id string, res tunnel.BackupResult, storageKey, serverVersion string) (DatabaseBackup, error) {
	state := "done"
	if !res.OK {
		state = "failed"
	}
	return scanBackup(s.db(ctx).QueryRow(ctx, `
		UPDATE database_backups SET state = $2, stage = $3, error = $4, local_path = $5, storage_key = $6,
			bytes = $7, sha256 = $8, wrapped_key = $9, key_id = $10, server_version = $11, dump_version = $12,
			compression = $13, verified = $14, finished_at = now(), reported_at = now(), detail = ''
		WHERE id = $1::uuid RETURNING `+backupCols,
		id, state, res.Stage, res.Error, res.LocalPath, storageKey, res.Bytes, res.SHA256, res.WrappedKey,
		res.KeyID, serverVersion, res.DumpVersion, res.Compression, res.Verified))
}

// FailStaleBackups marks running backups nothing has reported on for too long.
func (s *Store) FailStaleBackups(ctx context.Context, after time.Duration) ([]DatabaseBackup, error) {
	rows, err := s.db(ctx).Query(ctx, `UPDATE database_backups
		SET state = 'failed', error = 'the agent stopped reporting on this backup', finished_at = now()
		WHERE state = 'running' AND reported_at < now() - $1::interval RETURNING `+backupCols,
		fmt.Sprintf("%d seconds", int(after.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DatabaseBackup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Backup is one backup by id.
func (s *Store) Backup(ctx context.Context, id string) (DatabaseBackup, error) {
	b, err := scanBackup(s.db(ctx).QueryRow(ctx, `SELECT `+backupCols+` FROM database_backups WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return b, ErrBackupNotFound
	}
	return b, err
}

// WrappedKey is the backup's wrapped data key, for the restore grant.
func (s *Store) WrappedKey(b DatabaseBackup) string { return b.wrappedKey }

// Backups lists a database's backups, newest first, deleted ones last.
func (s *Store) Backups(ctx context.Context, dbID string, limit int) ([]DatabaseBackup, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+backupCols+` FROM database_backups
		WHERE database_id = $1::uuid AND state <> 'deleted' ORDER BY started_at DESC LIMIT $2`, dbID, limit)
	if err != nil {
		if notUUID(err) {
			return nil, ErrDatabaseNotFound
		}
		return nil, err
	}
	defer rows.Close()
	out := []DatabaseBackup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LastBackups is the newest finished backup per database, for the list.
func (s *Store) LastBackups(ctx context.Context) (map[string]DatabaseBackup, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT DISTINCT ON (database_id) `+backupCols+` FROM database_backups
		WHERE state IN ('done', 'failed', 'missed', 'running') ORDER BY database_id, started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DatabaseBackup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out[b.DatabaseID] = b
	}
	return out, rows.Err()
}

// PinBackup sets or clears the pin.
func (s *Store) PinBackup(ctx context.Context, id string, pinned bool) (DatabaseBackup, error) {
	b, err := scanBackup(s.db(ctx).QueryRow(ctx, `UPDATE database_backups SET pinned = $2
		WHERE id = $1::uuid AND state = 'done' RETURNING `+backupCols, id, pinned))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return b, ErrBackupNotFound
	}
	return b, err
}

// MarkBackupDeleted records that the files are gone.
func (s *Store) MarkBackupDeleted(ctx context.Context, id string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_backups SET state = 'deleted', local_path = '', storage_key = '',
		wrapped_key = '' WHERE id = $1::uuid`, id)
	return err
}

// ClearBackupLocal records that the server's copy is gone (pruned there).
func (s *Store) ClearBackupLocal(ctx context.Context, id string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_backups SET local_path = '' WHERE id = $1::uuid`, id)
	return err
}

// RetentionCandidates are the done, unpinned backups retention no longer
// keeps: beyond the newest keepLast, and older than keepDays when set. A
// backup being restored or tested is kept, and so is the last known good:
// the newest backup a test restore passed.
func (s *Store) RetentionCandidates(ctx context.Context, dbID string, keepLast, keepDays int) ([]DatabaseBackup, error) {
	rows, err := s.db(ctx).Query(ctx, `
		WITH ranked AS (
			SELECT `+backupCols+`, row_number() OVER (ORDER BY started_at DESC) AS rn
			FROM database_backups WHERE database_id = $1::uuid AND state = 'done' AND NOT pinned)
		SELECT `+backupCols+` FROM ranked
		WHERE rn > $2 AND ($3 = 0 OR started_at < now() - ($3 || ' days')::interval)
		  AND NOT EXISTS (SELECT 1 FROM database_restores r WHERE r.backup_id = ranked.id::uuid AND r.state = 'running')
		  AND NOT EXISTS (SELECT 1 FROM database_backup_tests t WHERE t.backup_id = ranked.id::uuid AND t.state = 'running')
		  -- The last known good: the newest backup a test restore passed is
		  -- kept until a newer one passes.
		  AND ranked.id::uuid IS DISTINCT FROM (SELECT t.backup_id FROM database_backup_tests t
			JOIN database_backups g ON g.id = t.backup_id
			WHERE t.database_id = $1::uuid AND t.state IN ('passed', 'warned') AND g.state = 'done'
			ORDER BY g.started_at DESC LIMIT 1)
		ORDER BY started_at`, dbID, keepLast, keepDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DatabaseBackup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

/* ── Restores ────────────────────────────────────────────────────────────── */

const restoreCols = `r.id::text, r.backup_id::text, r.database_id::text, r.mode, r.new_name, r.requested_by, r.reason,
	r.request_id::text, r.state, r.stage, r.detail, r.error, r.renamed_to, r.keep_until, r.kept_dropped,
	r.created_database_id::text, r.warnings, r.created_at, r.started_at, r.finished_at, r.reported_at,
	b.started_at, b.bytes, b.reason, b.started_by, d.dbname, a.hostname`

const restoreFrom = ` FROM database_restores r JOIN database_backups b ON b.id = r.backup_id
	JOIN databases d ON d.id = r.database_id JOIN assets a ON a.id = d.asset_id `

func scanRestore(row pgx.Row) (DatabaseRestore, error) {
	var r DatabaseRestore
	err := row.Scan(&r.ID, &r.BackupID, &r.DatabaseID, &r.Mode, &r.NewName, &r.RequestedBy, &r.Reason,
		&r.RequestID, &r.State, &r.Stage, &r.Detail, &r.Error, &r.RenamedTo, &r.KeepUntil, &r.KeptDropped,
		&r.CreatedDatabaseID, &r.Warnings, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.ReportedAt,
		&r.BackupStartedAt, &r.BackupBytes, &r.BackupReason, &r.BackupBy, &r.DBName, &r.Hostname)
	return r, err
}

// CreateRestore records a restore, pending (replace, waiting on an
// approval) or running (new).
func (s *Store) CreateRestore(ctx context.Context, r DatabaseRestore) (DatabaseRestore, error) {
	if r.Mode == tunnel.RestoreNew && !tunnel.ValidDBName.MatchString(r.NewName) {
		return r, ErrInvalidDatabase{"the new database's name must be lower-case letters, digits and underscores, up to 63"}
	}
	if r.Mode == tunnel.RestoreReplace {
		r.NewName = ""
	}
	var id string
	err := s.db(ctx).QueryRow(ctx, `INSERT INTO database_restores (backup_id, database_id, mode, new_name,
		requested_by, reason, state) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7) RETURNING id::text`,
		r.BackupID, r.DatabaseID, r.Mode, r.NewName, r.RequestedBy, r.Reason, r.State).Scan(&id)
	if err != nil {
		return r, err
	}
	return s.Restore(ctx, id)
}

// Restore is one restore by id.
func (s *Store) Restore(ctx context.Context, id string) (DatabaseRestore, error) {
	r, err := scanRestore(s.db(ctx).QueryRow(ctx, `SELECT `+restoreCols+restoreFrom+`WHERE r.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return r, ErrRestoreNotFound
	}
	return r, err
}

// RestoreByRequest is the restore an approval request is for.
func (s *Store) RestoreByRequest(ctx context.Context, requestID string) (DatabaseRestore, error) {
	r, err := scanRestore(s.db(ctx).QueryRow(ctx, `SELECT `+restoreCols+restoreFrom+`WHERE r.request_id = $1::uuid`, requestID))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return r, ErrRestoreNotFound
	}
	return r, err
}

// Restores lists a database's restores, newest first.
func (s *Store) Restores(ctx context.Context, dbID string) ([]DatabaseRestore, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+restoreCols+restoreFrom+
		`WHERE r.database_id = $1::uuid ORDER BY r.created_at DESC LIMIT 50`, dbID)
	if err != nil {
		if notUUID(err) {
			return nil, ErrDatabaseNotFound
		}
		return nil, err
	}
	defer rows.Close()
	out := []DatabaseRestore{}
	for rows.Next() {
		r, err := scanRestore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LinkRestoreRequest ties a pending restore to its approval request.
func (s *Store) LinkRestoreRequest(ctx context.Context, restoreID, requestID string) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE database_restores SET request_id = $2::uuid WHERE id = $1::uuid`, restoreID, requestID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE access_requests SET restore_id = $2::uuid WHERE id = $1::uuid`, requestID, restoreID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetRestoreState moves a restore along.
func (s *Store) SetRestoreState(ctx context.Context, id, state, stage, detail string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_restores SET state = $2, stage = $3, detail = $4, reported_at = now(),
		started_at = CASE WHEN $2 = 'running' AND started_at IS NULL THEN now() ELSE started_at END
		WHERE id = $1::uuid`, id, state, stage, detail)
	return err
}

// FinishRestore records how it ended.
func (s *Store) FinishRestore(ctx context.Context, id string, res tunnel.BackupResult, keep time.Duration, createdDB string) (DatabaseRestore, error) {
	state := "done"
	if !res.OK {
		state = "failed"
	}
	var keepUntil *time.Time
	if res.OK && res.RenamedTo != "" {
		t := time.Now().Add(keep)
		keepUntil = &t
	}
	var created *string
	if createdDB != "" {
		created = &createdDB
	}
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_restores SET state = $2, stage = $3, error = $4, renamed_to = $5,
		keep_until = $6, created_database_id = $7::uuid, warnings = $8, finished_at = now(), reported_at = now(), detail = ''
		WHERE id = $1::uuid`, id, state, res.Stage, res.Error, res.RenamedTo, keepUntil, created, res.Warnings)
	if err != nil {
		return DatabaseRestore{}, err
	}
	return s.Restore(ctx, id)
}

// KeptCopiesDue are the replaced databases whose keep window has passed.
func (s *Store) KeptCopiesDue(ctx context.Context) ([]DatabaseRestore, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+restoreCols+restoreFrom+
		`WHERE r.state = 'done' AND r.renamed_to <> '' AND NOT r.kept_dropped AND r.keep_until < now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DatabaseRestore
	for rows.Next() {
		r, err := scanRestore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkKeptDropped records the kept copy is gone, or that it was put back.
func (s *Store) MarkKeptDropped(ctx context.Context, id string, undone bool) error {
	state := "done"
	if undone {
		state = "undone"
	}
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_restores SET kept_dropped = true, state = $2, reported_at = now()
		WHERE id = $1::uuid`, id, state)
	return err
}

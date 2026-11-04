package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Test restores: the rows, and how one is judged. The job that fills them is
in backup_testrestore_jobs.go; the agent's half in
internal/agent/backup_testrestore.go. Plans: plans/db-backup-test-restore in
the project files.

The agent reports numbers. Whether they make a pass is decided here, with
the previous passing test of the same database to compare against, because
the live database has moved on since the backup was taken and is no yardstick.
*/

// BackupTest is one test restore.
type BackupTest struct {
	ID             string     `json:"id"`
	BackupID       string     `json:"backupId"`
	DatabaseID     string     `json:"databaseId"`
	StartedBy      string     `json:"startedBy"`
	State          string     `json:"state"`
	Stage          string     `json:"stage"`
	Detail         string     `json:"detail"`
	Error          string     `json:"error"`
	ScratchName    string     `json:"scratchName"`
	ScratchDropped bool       `json:"scratchDropped"`
	RestoreMS      int64      `json:"restoreMs"`
	Tables         int        `json:"tables"`
	TotalRows      int64      `json:"totalRows"`
	Bytes          int64      `json:"bytes"`
	Report         TestReport `json:"report"`
	StartedAt      time.Time  `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	ReportedAt     time.Time  `json:"reportedAt"`
	// BackupStartedAt is when the tested backup was taken, for the card.
	BackupStartedAt time.Time `json:"backupStartedAt"`
}

// TestReport is what the console shows of a test: numbers, never rows.
type TestReport struct {
	Steps  []tunnel.TestStep  `json:"steps"`
	Checks []TestCheckOutcome `json:"checks"`
	Tables []TestTableRows    `json:"tables"`
	// TablesTotal is every table in the copy; Tables lists at most
	// reportTables of them, the largest first.
	TablesTotal    int      `json:"tablesTotal"`
	Missing        []string `json:"missing,omitempty"`
	Indexes        int      `json:"indexes"`
	InvalidIndexes int      `json:"invalidIndexes"`
	Warnings       int      `json:"warnings"`
	NeedBytes      int64    `json:"needBytes"`
	FreeBytes      int64    `json:"freeBytes"`
	SourceBytes    int64    `json:"sourceBytes"`
	// Previous is the passing test this one was compared with.
	PreviousID    string `json:"previousId,omitempty"`
	PreviousBytes int64  `json:"previousBytes,omitempty"`
}

// TestCheckOutcome is one check: built in or the admin's, ok, warn or fail.
type TestCheckOutcome struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // builtin or custom
	State string `json:"state"`
	Value string `json:"value"`
}

// TestTableRows is a table's rows, and what the last passing test counted.
type TestTableRows struct {
	Name     string `json:"name"`
	Rows     int64  `json:"rows"`
	Previous *int64 `json:"previous,omitempty"`
}

// ErrTestRunning means a test of this database is already going.
var ErrTestRunning = errors.New("a test restore of this database is already running")

// ErrTestNotFound is what it says.
var ErrTestNotFound = errors.New("no such test restore")

const (
	// reportTables bounds the per-table list kept in a report.
	reportTables = 500
	// trashSchema is where the query builder's safe drop keeps a dropped
	// table for a week. A table moving there is expected, not a warning.
	trashSchema = "argus_trash."
	// rowsEmptiedFrom is how many rows a table must have had for an empty
	// one to be a warning.
	rowsEmptiedFrom = 1000
)

// evaluateTest judges an agent's result against the last passing test of
// the same database (prev, nil for none). It returns the state, the report
// and, when it did not pass, why in one line.
func evaluateTest(res tunnel.BackupResult, prev *BackupTest) (string, TestReport, string) {
	rep := TestReport{Steps: []tunnel.TestStep{}, Checks: []TestCheckOutcome{}, Tables: []TestTableRows{}}
	t := res.Test
	if t != nil {
		rep.Steps = append(rep.Steps, t.Steps...)
		rep.Warnings, rep.NeedBytes, rep.FreeBytes, rep.SourceBytes = t.Warnings, t.NeedBytes, t.FreeBytes, t.SourceBytes
	}
	if !res.OK {
		if res.Stage == "room" {
			return "skipped", rep, res.Error
		}
		return "failed", rep, "failed while " + stageWords(res.Stage) + ": " + res.Error
	}
	if t == nil || t.Data == nil {
		return "failed", rep, "the agent reported success but no numbers"
	}
	d := t.Data
	rep.TablesTotal, rep.Indexes, rep.InvalidIndexes = d.TablesTotal, d.Indexes, d.InvalidIndexes
	prevRows := map[string]int64{}
	if prev != nil {
		rep.PreviousID, rep.PreviousBytes = prev.ID, prev.Bytes
		for _, tr := range prev.Report.Tables {
			prevRows[tr.Name] = tr.Rows
		}
	}
	add := func(name, kind, state, value string) {
		rep.Checks = append(rep.Checks, TestCheckOutcome{Name: name, Kind: kind, State: state, Value: value})
	}
	var why []string

	// Restore completes.
	if t.Warnings > 0 {
		add("Restore completes", "builtin", "warn", fmt.Sprintf("%d errors ignored", t.Warnings))
		why = append(why, fmt.Sprintf("pg_restore ignored %d errors", t.Warnings))
	} else {
		add("Restore completes", "builtin", "ok", "0 errors")
	}

	// Every table the archive holds data for is there.
	have := map[string]bool{}
	for _, tr := range d.Tables {
		have[tr.Name] = true
	}
	for _, e := range t.Expected {
		if !have[e] {
			rep.Missing = append(rep.Missing, e)
		}
	}
	if len(rep.Missing) > 0 {
		add("Every table is there", "builtin", "fail", fmt.Sprintf("%d missing: %s", len(rep.Missing), firstFew(rep.Missing)))
		why = append(why, fmt.Sprintf("%d tables are missing from the copy", len(rep.Missing)))
	} else {
		add("Every table is there", "builtin", "ok", fmt.Sprintf("%d of %d", len(t.Expected), len(t.Expected)))
	}

	// Indexes are valid.
	if d.InvalidIndexes > 0 {
		add("Indexes are valid", "builtin", "fail", fmt.Sprintf("%d of %d invalid", d.InvalidIndexes, d.Indexes))
		why = append(why, fmt.Sprintf("%d indexes did not build", d.InvalidIndexes))
	} else {
		add("Indexes are valid", "builtin", "ok", fmt.Sprintf("%d valid", d.Indexes))
	}

	// Rows: nothing emptied or halved since the last pass. The trash schema
	// comes and goes by design.
	var shrunk []string
	var total int64
	uncounted := 0
	for _, tr := range d.Tables {
		row := TestTableRows{Name: tr.Name, Rows: tr.Rows}
		if p, ok := prevRows[tr.Name]; ok {
			row.Previous = &p
		}
		rep.Tables = append(rep.Tables, row)
		if tr.Rows < 0 {
			uncounted++
			continue
		}
		total += tr.Rows
		if strings.HasPrefix(tr.Name, trashSchema) || row.Previous == nil {
			continue
		}
		p := *row.Previous
		if (tr.Rows == 0 && p > rowsEmptiedFrom) || (p > 0 && tr.Rows*2 < p) {
			shrunk = append(shrunk, fmt.Sprintf("%s: %d rows, was %d", tr.Name, tr.Rows, p))
		}
	}
	switch {
	case prev == nil:
		add("Rows", "builtin", "ok", fmt.Sprintf("%d rows counted; the first test, nothing to compare", total))
	case len(shrunk) > 0:
		add("Rows", "builtin", "warn", firstFew(shrunk))
		why = append(why, fmt.Sprintf("%d tables emptied or halved since the last pass", len(shrunk)))
	default:
		add("Rows", "builtin", "ok", "no table emptied or halved")
	}
	if uncounted > 0 {
		add("Rows counted", "builtin", "warn", fmt.Sprintf("%d tables not counted in time", uncounted))
		why = append(why, fmt.Sprintf("%d tables could not be counted in time", uncounted))
	}

	// Size.
	if prev != nil && prev.Bytes > 0 && d.Bytes*2 < prev.Bytes {
		add("Size", "builtin", "warn", fmt.Sprintf("%s, was %s", humanSize(d.Bytes), humanSize(prev.Bytes)))
		why = append(why, "the copy is less than half the size of the last one")
	} else if prev != nil && prev.Bytes > 0 {
		add("Size", "builtin", "ok", fmt.Sprintf("%s, was %s", humanSize(d.Bytes), humanSize(prev.Bytes)))
	} else {
		add("Size", "builtin", "ok", humanSize(d.Bytes))
	}

	// The admin's checks.
	failed := len(rep.Missing) > 0 || d.InvalidIndexes > 0
	for _, c := range d.Checks {
		switch {
		case c.Error != "":
			add(c.Name, "custom", "fail", c.Error)
			why = append(why, fmt.Sprintf("check %q could not run: %s", c.Name, c.Error))
			failed = true
		case !c.OK:
			add(c.Name, "custom", "fail", c.Value)
			why = append(why, fmt.Sprintf("check %q returned %s", c.Name, c.Value))
			failed = true
		default:
			add(c.Name, "custom", "ok", c.Value)
		}
	}

	// Keep the largest tables in the report; the rest are summed above.
	sort.SliceStable(rep.Tables, func(i, j int) bool { return rep.Tables[i].Rows > rep.Tables[j].Rows })
	if len(rep.Tables) > reportTables {
		rep.Tables = rep.Tables[:reportTables]
	}
	state := "passed"
	switch {
	case failed:
		state = "failed"
	case len(why) > 0:
		state = "warned"
	}
	return state, rep, strings.Join(why, "; ")
}

func firstFew(s []string) string {
	if len(s) <= 3 {
		return strings.Join(s, ", ")
	}
	return strings.Join(s[:3], ", ") + fmt.Sprintf(" and %d more", len(s)-3)
}

func stageWords(stage string) string {
	switch stage {
	case "room":
		return "checking for room"
	case "receiving":
		return "receiving the backup"
	case "verifying":
		return "checking the backup's hash"
	case "reading":
		return "reading the backup's contents"
	case "preparing":
		return "creating the test database"
	case "restoring":
		return "restoring"
	case "checking":
		return "checking the copy"
	case "start", "":
		return "starting"
	}
	return stage
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

/* ── Rows ────────────────────────────────────────────────────────────────── */

const testCols = `t.id::text, t.backup_id::text, t.database_id::text, t.started_by, t.state, t.stage, t.detail,
	t.error, t.scratch_name, t.scratch_dropped, t.restore_ms, t.tables, t.total_rows, t.bytes, t.report,
	t.started_at, t.finished_at, t.reported_at, b.started_at`

const testFrom = ` FROM database_backup_tests t JOIN database_backups b ON b.id = t.backup_id `

func scanTest(row pgx.Row) (BackupTest, error) {
	var t BackupTest
	var report []byte
	err := row.Scan(&t.ID, &t.BackupID, &t.DatabaseID, &t.StartedBy, &t.State, &t.Stage, &t.Detail, &t.Error,
		&t.ScratchName, &t.ScratchDropped, &t.RestoreMS, &t.Tables, &t.TotalRows, &t.Bytes, &report,
		&t.StartedAt, &t.FinishedAt, &t.ReportedAt, &t.BackupStartedAt)
	if err == nil && len(report) > 0 {
		_ = json.Unmarshal(report, &t.Report)
	}
	return t, err
}

func scanTests(rows pgx.Rows, err error) ([]BackupTest, error) {
	if err != nil {
		if notUUID(err) {
			return nil, ErrDatabaseNotFound
		}
		return nil, err
	}
	defer rows.Close()
	out := []BackupTest{}
	for rows.Next() {
		t, err := scanTest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StartBackupTest records a running test. One per database at a time.
func (s *Store) StartBackupTest(ctx context.Context, dbID, backupID, by, scratch string) (BackupTest, error) {
	var id string
	err := s.db(ctx).QueryRow(ctx, `INSERT INTO database_backup_tests (backup_id, database_id, started_by, scratch_name, stage)
		VALUES ($1::uuid, $2::uuid, $3, $4, 'starting') RETURNING id::text`, backupID, dbID, by, scratch).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "database_backup_tests_one_running") {
			return BackupTest{}, ErrTestRunning
		}
		if notUUID(err) || strings.Contains(err.Error(), "foreign key") {
			return BackupTest{}, ErrBackupNotFound
		}
		return BackupTest{}, err
	}
	return s.BackupTest(ctx, id)
}

// RecordSkippedTest writes a test that never started, so the console and
// the overdue alert see why.
func (s *Store) RecordSkippedTest(ctx context.Context, dbID, backupID, by, why string) error {
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO database_backup_tests (backup_id, database_id, started_by, state, error,
		scratch_dropped, finished_at) VALUES ($1::uuid, $2::uuid, $3, 'skipped', $4, true, now())`, backupID, dbID, by, why)
	return err
}

// BackupTestProgress updates a running test's stage.
func (s *Store) BackupTestProgress(ctx context.Context, id, stage, detail string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_backup_tests SET stage = $2, detail = $3, reported_at = now()
		WHERE id = $1::uuid AND state = 'running'`, id, stage, detail)
	return err
}

// FinishBackupTest records how a test ended.
func (s *Store) FinishBackupTest(ctx context.Context, t BackupTest) (BackupTest, error) {
	report, _ := json.Marshal(t.Report)
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_backup_tests SET state = $2, stage = $3, error = $4,
		scratch_dropped = $5, restore_ms = $6, tables = $7, total_rows = $8, bytes = $9, report = $10::jsonb,
		detail = '', finished_at = now(), reported_at = now() WHERE id = $1::uuid`,
		t.ID, t.State, t.Stage, t.Error, t.ScratchDropped, t.RestoreMS, t.Tables, t.TotalRows, t.Bytes, string(report))
	if err != nil {
		return t, err
	}
	return s.BackupTest(ctx, t.ID)
}

// BackupTest is one test by id.
func (s *Store) BackupTest(ctx context.Context, id string) (BackupTest, error) {
	t, err := scanTest(s.db(ctx).QueryRow(ctx, `SELECT `+testCols+testFrom+`WHERE t.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return t, ErrTestNotFound
	}
	return t, err
}

// BackupTests lists a database's tests, newest first.
func (s *Store) BackupTests(ctx context.Context, dbID string, limit int) ([]BackupTest, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return scanTests(s.db(ctx).Query(ctx, `SELECT `+testCols+testFrom+
		`WHERE t.database_id = $1::uuid ORDER BY t.started_at DESC LIMIT $2`, dbID, limit))
}

// LastGoodTest is the database's newest passing test of a backup that is
// still kept, or nil.
func (s *Store) LastGoodTest(ctx context.Context, dbID string) (*BackupTest, error) {
	t, err := scanTest(s.db(ctx).QueryRow(ctx, `SELECT `+testCols+testFrom+`WHERE t.database_id = $1::uuid
		AND t.state IN ('passed', 'warned') AND b.state = 'done' ORDER BY b.started_at DESC, t.finished_at DESC LIMIT 1`, dbID))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UntestedBackup is the newest finished backup of the database that no
// test has reached a verdict on, or ErrBackupNotFound.
func (s *Store) UntestedBackup(ctx context.Context, dbID string) (DatabaseBackup, error) {
	b, err := scanBackup(s.db(ctx).QueryRow(ctx, `SELECT `+backupCols+` FROM database_backups b
		WHERE b.database_id = $1::uuid AND b.state = 'done'
		  AND NOT EXISTS (SELECT 1 FROM database_backup_tests t WHERE t.backup_id = b.id AND t.state <> 'skipped')
		ORDER BY b.started_at DESC LIMIT 1`, dbID))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return b, ErrBackupNotFound
	}
	return b, err
}

// FailStaleTests marks running tests nothing has reported on for too long.
// Their copies are left for the sweep to drop.
func (s *Store) FailStaleTests(ctx context.Context, after time.Duration) ([]BackupTest, error) {
	rows, err := s.db(ctx).Query(ctx, `UPDATE database_backup_tests
		SET state = 'failed', error = 'the agent stopped reporting on this test', finished_at = now()
		WHERE state = 'running' AND reported_at < now() - $1::interval RETURNING id::text`,
		fmt.Sprintf("%d seconds", int(after.Seconds())))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []BackupTest
	for _, id := range ids {
		if t, err := s.BackupTest(ctx, id); err == nil {
			out = append(out, t)
		}
	}
	return out, nil
}

// UndroppedCopies are finished tests whose throwaway database may still be
// on the server.
func (s *Store) UndroppedCopies(ctx context.Context) ([]BackupTest, error) {
	return scanTests(s.db(ctx).Query(ctx, `SELECT `+testCols+testFrom+`WHERE t.state <> 'running'
		AND t.scratch_name <> '' AND NOT t.scratch_dropped ORDER BY t.started_at LIMIT 50`))
}

// MarkCopyDropped records that a test's throwaway database is gone.
func (s *Store) MarkCopyDropped(ctx context.Context, id string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_backup_tests SET scratch_dropped = true WHERE id = $1::uuid`, id)
	return err
}

// HostBusy says whether a backup, restore or test is running on any
// database of the asset: a test restore runs alone on its server.
func (s *Store) HostBusy(ctx context.Context, assetID string) (bool, error) {
	var busy bool
	err := s.db(ctx).QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM database_backups b JOIN databases d ON d.id = b.database_id
			WHERE d.asset_id = $1::uuid AND b.state = 'running')
		OR EXISTS (SELECT 1 FROM database_restores r JOIN databases d ON d.id = r.database_id
			WHERE d.asset_id = $1::uuid AND r.state = 'running')
		OR EXISTS (SELECT 1 FROM database_backup_tests t JOIN databases d ON d.id = t.database_id
			WHERE d.asset_id = $1::uuid AND t.state = 'running')`, assetID).Scan(&busy)
	return busy, err
}

// RunningTestsOnHost are the tests running on any database of the asset,
// for a backup that must take their place.
func (s *Store) RunningTestsOnHost(ctx context.Context, assetID string) ([]BackupTest, error) {
	return scanTests(s.db(ctx).Query(ctx, `SELECT `+testCols+testFrom+`JOIN databases d ON d.id = t.database_id
		WHERE d.asset_id = $1::uuid AND t.state = 'running'`, assetID))
}

// DueTestSchedules are schedules whose next test has passed.
func (s *Store) DueTestSchedules(ctx context.Context, now time.Time) ([]BackupSchedule, error) {
	return s.schedulesWhere(ctx, `test_every <> 'off' AND test_next_at IS NOT NULL AND test_next_at <= $1`, now)
}

// TestingSchedules are every schedule with tests on, for the overdue check.
func (s *Store) TestingSchedules(ctx context.Context) ([]BackupSchedule, error) {
	return s.schedulesWhere(ctx, `test_every <> 'off'`)
}

func (s *Store) schedulesWhere(ctx context.Context, where string, args ...any) ([]BackupSchedule, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+scheduleCols+` FROM database_backup_schedules WHERE `+where, args...)
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

// SetTestNext sets when the next test is due; nil for none.
func (s *Store) SetTestNext(ctx context.Context, dbID string, at *time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_backup_schedules SET test_next_at = $2 WHERE database_id = $1::uuid`, dbID, at)
	return err
}

// SetTestOverdue records when the overdue alert was raised; nil clears it.
func (s *Store) SetTestOverdue(ctx context.Context, dbID string, at *time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_backup_schedules SET test_overdue_at = $2 WHERE database_id = $1::uuid`, dbID, at)
	return err
}

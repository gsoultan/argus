package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Test restores on the control plane: how a result is judged, the schedule's
test fields, and the bookkeeping the sweep and retention rely on. The agent
side, a real restore into a throwaway database, is tested in internal/agent.
*/

func okTest(tables map[string]int64, expected []string, checks ...tunnel.CheckResult) tunnel.BackupResult {
	d := &tunnel.TestData{TablesTotal: len(tables), Indexes: 12, Bytes: 4 << 30, Checks: checks}
	for n, r := range tables {
		d.Tables = append(d.Tables, tunnel.TableRows{Name: n, Rows: r})
	}
	return tunnel.BackupResult{OK: true, Test: &tunnel.BackupTestResult{Scratch: "argus_test_0000abcd", Dropped: true,
		RestoreMS: 252000, Expected: expected, Data: d}}
}

func passedBefore(tables map[string]int64, bytes int64) *BackupTest {
	p := &BackupTest{ID: "prev", Bytes: bytes}
	for n, r := range tables {
		p.Report.Tables = append(p.Report.Tables, TestTableRows{Name: n, Rows: r})
	}
	return p
}

func checkState(rep TestReport, name string) string {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c.State
		}
	}
	return ""
}

func TestEvaluateTestPassesWarnsAndFails(t *testing.T) {
	expected := []string{"public.orders", "public.customers"}
	before := map[string]int64{"public.orders": 612450, "public.customers": 184220, "argus_trash.old_1": 5000}

	// A clean restore with every table, compared with the last pass.
	state, rep, why := evaluateTest(okTest(map[string]int64{"public.orders": 620000, "public.customers": 185000}, expected,
		tunnel.CheckResult{Name: "Recent orders", OK: true, Value: "true"}), passedBefore(before, 4<<30))
	if state != "passed" || why != "" {
		t.Fatalf("clean restore: %s (%s)", state, why)
	}
	if checkState(rep, "Recent orders") != "ok" || checkState(rep, "Every table is there") != "ok" {
		t.Fatalf("checks: %+v", rep.Checks)
	}
	if rep.PreviousID != "prev" || len(rep.Tables) != 2 || rep.Tables[0].Previous == nil {
		t.Fatalf("report does not carry the comparison: %+v", rep)
	}

	// A table emptied since the last pass is a warning, not a failure; the
	// trash schema emptying is expected.
	state, rep, why = evaluateTest(okTest(map[string]int64{"public.orders": 0, "public.customers": 185000, "argus_trash.old_1": 0},
		expected), passedBefore(before, 4<<30))
	if state != "warned" || !strings.Contains(why, "emptied or halved") || checkState(rep, "Rows") != "warn" {
		t.Fatalf("emptied table: %s (%s)", state, why)
	}
	if strings.Contains(rep.Checks[3].Value, "argus_trash") {
		t.Fatalf("the trash schema was warned about: %+v", rep.Checks[3])
	}

	// pg_restore ignoring errors is a warning.
	r := okTest(map[string]int64{"public.orders": 1, "public.customers": 1}, expected)
	r.Test.Warnings = 3
	if state, _, _ := evaluateTest(r, nil); state != "warned" {
		t.Fatalf("ignored errors: %s", state)
	}

	// A missing table, an invalid index, or a check of yours failing fails.
	r = okTest(map[string]int64{"public.orders": 1}, expected)
	if state, rep, why := evaluateTest(r, nil); state != "failed" || len(rep.Missing) != 1 || !strings.Contains(why, "missing") {
		t.Fatalf("missing table: %s %v (%s)", state, rep.Missing, why)
	}
	r = okTest(map[string]int64{"public.orders": 1, "public.customers": 1}, expected)
	r.Test.Data.InvalidIndexes = 1
	if state, _, _ := evaluateTest(r, nil); state != "failed" {
		t.Fatalf("invalid index: %s", state)
	}
	r = okTest(map[string]int64{"public.orders": 1, "public.customers": 1}, expected,
		tunnel.CheckResult{Name: "Recent orders", Value: "false"})
	if state, rep, why := evaluateTest(r, nil); state != "failed" || checkState(rep, "Recent orders") != "fail" ||
		!strings.Contains(why, `"Recent orders" returned false`) {
		t.Fatalf("a failing check: %s (%s)", state, why)
	}

	// Not enough room is skipped, not failed: nothing was tested.
	state, _, why = evaluateTest(tunnel.BackupResult{Stage: "room", Error: "not enough room for a copy",
		Test: &tunnel.BackupTestResult{Dropped: true}}, nil)
	if state != "skipped" || !strings.Contains(why, "room") {
		t.Fatalf("no room: %s (%s)", state, why)
	}
	// A restore that broke is a failure that says where.
	state, _, why = evaluateTest(tunnel.BackupResult{Stage: "restoring", Error: "pg_restore failed: out of memory"}, nil)
	if state != "failed" || !strings.HasPrefix(why, "failed while restoring") {
		t.Fatalf("broken restore: %s (%s)", state, why)
	}
}

func TestBackupScheduleTestFields(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	ctx := context.Background()
	d, _ := s.BackupSchedule(ctx, db.ID)
	if d.Exists || d.TestEvery != "weekly" || d.TestHour != 4 {
		t.Fatalf("a new schedule should offer weekly tests at 04:00: %+v", d)
	}
	base := BackupSchedule{DatabaseID: db.ID, Enabled: true, Every: "hourly", Destination: "server", KeepLast: 5}
	bad := base
	bad.TestEvery = "after"
	if _, err := s.SetBackupSchedule(ctx, bad, admin); err == nil {
		t.Fatal("a test after every hourly backup was accepted")
	}
	bad = base
	bad.TestEvery, bad.TestChecks = "weekly", []tunnel.TestCheck{{Name: "wipe", SQL: "DELETE FROM orders"}}
	if _, err := s.SetBackupSchedule(ctx, bad, admin); err == nil {
		t.Fatal("a check that deletes was accepted")
	}
	good := base
	good.Every, good.TestEvery, good.TestWeekday, good.TestHour = "daily", "weekly", 0, 4
	good.TestChecks = []tunnel.TestCheck{{Name: "Recent orders", SQL: "SELECT max(created_at) > now() - interval '26 hours' FROM orders"}}
	saved, err := s.SetBackupSchedule(ctx, good, admin)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TestEvery != "weekly" || saved.TestNextAt == nil || saved.TestNextAt.Weekday() != time.Sunday ||
		len(saved.TestChecks) != 1 || saved.TestChecks[0].Name != "Recent orders" {
		t.Fatalf("saved: %+v", saved)
	}
	due, _ := s.DueTestSchedules(ctx, saved.TestNextAt.Add(time.Minute))
	found := false
	for _, d := range due {
		found = found || d.DatabaseID == db.ID
	}
	if !found {
		t.Fatal("the test is not due after its time")
	}
	off := good
	off.TestEvery = "off"
	saved, _ = s.SetBackupSchedule(ctx, off, admin)
	if saved.TestNextAt != nil {
		t.Fatalf("tests off keep a next run: %+v", saved)
	}
}

func TestTestRowsAndTheLastKnownGood(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	ctx := context.Background()
	day := 24 * time.Hour
	now := time.Now()
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, finishedBackup(t, s, db.ID, admin, now.Add(-time.Duration(i)*day), false).ID)
	}

	// Nothing tested yet: the newest is the one to test.
	if b, err := s.UntestedBackup(ctx, db.ID); err != nil || b.ID != ids[0] {
		t.Fatalf("untested: %v %v", b.ID, err)
	}
	// Test the oldest; one running per database, and the server is busy.
	tr, err := s.StartBackupTest(ctx, db.ID, ids[3], admin, "argus_test_0000abcd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBackupTest(ctx, db.ID, ids[2], admin, "argus_test_0000abce"); err != ErrTestRunning {
		t.Fatalf("a second test started: %v", err)
	}
	if busy, err := s.HostBusy(ctx, db.AssetID); err != nil || !busy {
		t.Fatalf("the server is not busy while a test runs: %v %v", busy, err)
	}
	if running, _ := s.RunningTestsOnHost(ctx, db.AssetID); len(running) != 1 || running[0].ID != tr.ID {
		t.Fatalf("running on host: %+v", running)
	}
	// A backup under test is not a retention candidate.
	if got, _ := s.RetentionCandidates(ctx, db.ID, 1, 0); containsBackup(got, ids[3]) {
		t.Fatal("a backup under test was offered for deletion")
	}
	tr.State, tr.ScratchDropped, tr.RestoreMS, tr.Tables, tr.Bytes = "passed", true, 252000, 38, 4<<30
	tr.Report = TestReport{Tables: []TestTableRows{{Name: "public.orders", Rows: 10}}}
	tr, err = s.FinishBackupTest(ctx, tr)
	if err != nil || tr.State != "passed" || tr.FinishedAt == nil || len(tr.Report.Tables) != 1 {
		t.Fatalf("finish: %+v %v", tr, err)
	}
	good, err := s.LastGoodTest(ctx, db.ID)
	if err != nil || good == nil || good.BackupID != ids[3] {
		t.Fatalf("last good: %+v %v", good, err)
	}
	// Retention keeps the last known good past keep 1.
	got, _ := s.RetentionCandidates(ctx, db.ID, 1, 0)
	if containsBackup(got, ids[3]) || !containsBackup(got, ids[2]) {
		t.Fatalf("retention: %+v", got)
	}
	// Once a newer backup passes, the old one is ordinary again.
	tr2, _ := s.StartBackupTest(ctx, db.ID, ids[1], admin, "argus_test_0000abcf")
	tr2.State, tr2.ScratchDropped = "warned", true
	if _, err := s.FinishBackupTest(ctx, tr2); err != nil {
		t.Fatal(err)
	}
	got, _ = s.RetentionCandidates(ctx, db.ID, 1, 0)
	if !containsBackup(got, ids[3]) || containsBackup(got, ids[1]) {
		t.Fatalf("after a newer pass: %+v", got)
	}
	// The newest untested is still the newest backup.
	if b, _ := s.UntestedBackup(ctx, db.ID); b.ID != ids[0] {
		t.Fatalf("untested after tests: %s", b.ID)
	}
	// A test that could not drop its copy is left for the sweep, once.
	tr3, _ := s.StartBackupTest(ctx, db.ID, ids[0], "schedule:"+db.ID, "argus_test_0000abd0")
	tr3.State = "failed"
	if _, err := s.FinishBackupTest(ctx, tr3); err != nil {
		t.Fatal(err)
	}
	left, _ := s.UndroppedCopies(ctx)
	if !containsTest(left, tr3.ID) {
		t.Fatal("an undropped copy is not left for the sweep")
	}
	if err := s.MarkCopyDropped(ctx, tr3.ID); err != nil {
		t.Fatal(err)
	}
	left, _ = s.UndroppedCopies(ctx)
	if containsTest(left, tr3.ID) {
		t.Fatal("a dropped copy is still left for the sweep")
	}
	// A test nothing reports on fails, and keeps its copy for the sweep.
	tr4, _ := s.StartBackupTest(ctx, db.ID, ids[0], admin, "argus_test_0000abd1")
	if _, err := s.db(ctx).Exec(ctx, `UPDATE database_backup_tests SET reported_at = now() - interval '1 hour' WHERE id = $1::uuid`, tr4.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := s.FailStaleTests(ctx, 10*time.Minute)
	if err != nil || !containsTest(stale, tr4.ID) {
		t.Fatalf("stale: %+v %v", stale, err)
	}
	if left, _ := s.UndroppedCopies(ctx); !containsTest(left, tr4.ID) {
		t.Fatal("a stale test's copy is not left for the sweep")
	}
	// Skipped tests count as untested.
	if err := s.RecordSkippedTest(ctx, db.ID, ids[0], admin, "no room"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.BackupTests(ctx, db.ID, 10)
	if len(list) != 5 || list[0].State != "skipped" {
		t.Fatalf("list: %d, first %+v", len(list), list[0])
	}
}

func TestTestCopyNamesAreNotDatabases(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	if _, err := s.CreateDatabase(context.Background(), Database{AssetID: db.AssetID, Name: "argus_test_0000abcd"}, admin); err == nil {
		t.Fatal("a test copy's name was added as a database")
	}
}

func containsBackup(list []DatabaseBackup, id string) bool {
	for _, b := range list {
		if b.ID == id {
			return true
		}
	}
	return false
}

func containsTest(list []BackupTest, id string) bool {
	for _, b := range list {
		if b.ID == id {
			return true
		}
	}
	return false
}

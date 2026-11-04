package control

import (
	"context"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Backup and restore records. The agent side is tested in internal/agent
against a real cluster; this is the control plane's bookkeeping: schedules
and their next run, one running backup at a time, what retention may delete,
and a replace waiting on an approval request.
*/

func TestBackupScheduleNextRun(t *testing.T) {
	loc := time.FixedZone("test", 0)
	now := time.Date(2026, 11, 2, 10, 30, 0, 0, loc) // a Monday
	cases := []struct {
		s    BackupSchedule
		want time.Time
	}{
		{BackupSchedule{Every: "daily", AtHour: 2}, time.Date(2026, 11, 3, 2, 0, 0, 0, loc)},
		{BackupSchedule{Every: "daily", AtHour: 11, AtMinute: 15}, time.Date(2026, 11, 2, 11, 15, 0, 0, loc)},
		{BackupSchedule{Every: "hourly", AtMinute: 45}, time.Date(2026, 11, 2, 10, 45, 0, 0, loc)},
		{BackupSchedule{Every: "hourly", AtMinute: 10}, time.Date(2026, 11, 2, 11, 10, 0, 0, loc)},
		{BackupSchedule{Every: "weekly", Weekday: 0, AtHour: 3}, time.Date(2026, 11, 8, 3, 0, 0, 0, loc)},
		{BackupSchedule{Every: "weekly", Weekday: 1, AtHour: 3}, time.Date(2026, 11, 9, 3, 0, 0, 0, loc)},
		{BackupSchedule{Every: "weekly", Weekday: 1, AtHour: 12}, time.Date(2026, 11, 2, 12, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		got := c.s.NextRun(now)
		if !got.Equal(c.want) {
			t.Errorf("%s: next after %s = %s, want %s", c.s.Describe(), now, got, c.want)
		}
	}
}

func TestBackupScheduleRoundTrip(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	ctx := context.Background()

	d, err := s.BackupSchedule(ctx, db.ID)
	if err != nil || d.Exists || d.Enabled || d.Every != "daily" || d.KeepLast != 14 {
		t.Fatalf("default schedule: %+v, %v", d, err)
	}
	if _, err := s.SetBackupSchedule(ctx, BackupSchedule{DatabaseID: db.ID, Enabled: true, Every: "monthly",
		Destination: "server", KeepLast: 7}, admin); err == nil {
		t.Fatal("an unknown interval was accepted")
	}
	saved, err := s.SetBackupSchedule(ctx, BackupSchedule{DatabaseID: db.ID, Enabled: true, Every: "weekly",
		Weekday: 6, AtHour: 4, AtMinute: 30, Destination: "both", KeepLast: 8, KeepDays: 90}, admin)
	if err != nil {
		t.Fatalf("SetBackupSchedule: %v", err)
	}
	if saved.NextRunAt == nil || saved.NextRunAt.Weekday() != time.Saturday || saved.UpdatedBy != admin {
		t.Fatalf("saved: %+v", saved)
	}
	due, err := s.DueBackupSchedules(ctx, saved.NextRunAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range due {
		found = found || b.DatabaseID == db.ID
	}
	if !found {
		t.Fatal("the schedule is not due just after its next run")
	}
	if m, err := s.AdvanceBackupSchedule(ctx, saved, true); err != nil || m != 1 {
		t.Fatalf("a miss should count: %d, %v", m, err)
	}
	// A run that starts leaves the count to how it ends.
	if m, err := s.AdvanceBackupSchedule(ctx, saved, false); err != nil || m != 1 {
		t.Fatalf("a run that started changed the count: %d, %v", m, err)
	}
	if m, err := s.CountScheduledOutcome(ctx, db.ID, false); err != nil || m != 2 {
		t.Fatalf("a scheduled run that failed should count like a miss: %d, %v", m, err)
	}
	if m, err := s.CountScheduledOutcome(ctx, db.ID, true); err != nil || m != 0 {
		t.Fatalf("a scheduled run that produced a backup should clear the count: %d, %v", m, err)
	}
	off, err := s.SetBackupSchedule(ctx, BackupSchedule{DatabaseID: db.ID, Enabled: false, Every: "daily",
		Destination: "server", KeepLast: 1}, admin)
	if err != nil || off.NextRunAt != nil {
		t.Fatalf("disabled schedule keeps a next run: %+v, %v", off, err)
	}
}

func finishedBackup(t *testing.T, s *Store, dbID, by string, startedAt time.Time, pinned bool) DatabaseBackup {
	t.Helper()
	ctx := context.Background()
	b, err := s.StartBackup(ctx, dbID, by, "", "server")
	if err != nil {
		t.Fatalf("StartBackup: %v", err)
	}
	b, err = s.FinishBackup(ctx, b.ID, tunnel.BackupResult{OK: true, Bytes: 10, SHA256: "ab", LocalPath: "/x/" + b.ID,
		WrappedKey: "w.e", KeyID: "k", Verified: true}, "", "16.1")
	if err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	if _, err := s.db(ctx).Exec(ctx, `UPDATE database_backups SET started_at = $2, pinned = $3 WHERE id = $1::uuid`,
		b.ID, startedAt, pinned); err != nil {
		t.Fatal(err)
	}
	b, _ = s.Backup(ctx, b.ID)
	return b
}

func TestOneRunningBackupPerDatabase(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	ctx := context.Background()
	first, err := s.StartBackup(ctx, db.ID, admin, "before the release", "server")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBackup(ctx, db.ID, admin, "", "server"); err != ErrBackupRunning {
		t.Fatalf("a second backup started: %v", err)
	}
	if err := s.BackupProgress(ctx, first.ID, "data", "orders", 1024); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Backup(ctx, first.ID)
	if got.Stage != "data" || got.Detail != "orders" || got.Bytes != 1024 {
		t.Fatalf("progress not recorded: %+v", got)
	}
	// A backup nothing reports on is failed by the sweep, which frees the slot.
	if _, err := s.db(ctx).Exec(ctx, `UPDATE database_backups SET reported_at = now() - interval '1 hour' WHERE id = $1::uuid`, first.ID); err != nil {
		t.Fatal(err)
	}
	stale, err := s.FailStaleBackups(ctx, 10*time.Minute)
	if err != nil || len(stale) == 0 {
		t.Fatalf("stale: %v, %v", stale, err)
	}
	if _, err := s.StartBackup(ctx, db.ID, admin, "", "server"); err != nil {
		t.Fatalf("after the stale one failed: %v", err)
	}
	if _, err := s.StartBackup(ctx, "not-a-uuid", admin, "", "server"); err != ErrDatabaseNotFound {
		t.Fatalf("bad id: %v", err)
	}
}

func TestRetentionCandidates(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	ctx := context.Background()
	day := 24 * time.Hour
	now := time.Now()
	var ids []string
	for i := 0; i < 5; i++ {
		b := finishedBackup(t, s, db.ID, admin, now.Add(-time.Duration(i)*day), i == 3)
		ids = append(ids, b.ID)
	}
	// Keep the last 2: the three older are candidates, except the pinned one.
	got, err := s.RetentionCandidates(ctx, db.ID, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{ids[2]: true, ids[4]: true}
	if len(got) != 2 || !want[got[0].ID] || !want[got[1].ID] {
		t.Fatalf("keep 2: got %d candidates: %+v", len(got), got)
	}
	// Keep 1 and 3 days: only the ones older than 3 days and beyond the first.
	got, err = s.RetentionCandidates(ctx, db.ID, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ids[4] {
		t.Fatalf("keep 1 and 3 days: %+v", got)
	}
	// A backup being restored from is never a candidate.
	if _, err := s.CreateRestore(ctx, DatabaseRestore{BackupID: ids[4], DatabaseID: db.ID, Mode: "new",
		NewName: "orders_copy", RequestedBy: admin, State: "running"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.RetentionCandidates(ctx, db.ID, 1, 3)
	if len(got) != 0 {
		t.Fatalf("a backup under restore was offered for deletion: %+v", got)
	}
	// Pinning and deleting.
	if _, err := s.PinBackup(ctx, ids[0], true); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBackupDeleted(ctx, ids[2]); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Backups(ctx, db.ID, 10)
	if len(list) != 4 {
		t.Fatalf("a deleted backup is still listed: %d", len(list))
	}
	last, _ := s.LastBackups(ctx)
	if last[db.ID].ID != ids[0] {
		t.Fatalf("last backup: %+v", last[db.ID])
	}
}

func TestReplaceRestoreWaitsForApproval(t *testing.T) {
	_, s, _, db, admin := dbFixture(t)
	ctx := context.Background()
	dba, _ := newAccount(t, s, "dba")
	approver, _ := newAccount(t, s, "approver")
	b := finishedBackup(t, s, db.ID, admin, time.Now().Add(-time.Hour), false)

	r, err := s.CreateRestore(ctx, DatabaseRestore{BackupID: b.ID, DatabaseID: db.ID, Mode: "replace",
		RequestedBy: dba, Reason: "bad migration at 09:12", State: "pending"})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}
	if r.DBName != "orders" || r.Hostname != db.Hostname || r.BackupBy != admin {
		t.Fatalf("restore row lacks its backup and database: %+v", r)
	}
	req, err := s.CreateRequest(ctx, AccessRequest{RequesterEmail: dba, AssetHostnames: []string{db.Hostname},
		Principal: "db:restore", Justification: "Replace orders from the backup: bad migration at 09:12",
		DurationMinutes: restoreApprovalMin})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkRestoreRequest(ctx, r.ID, req.ID); err != nil {
		t.Fatal(err)
	}
	byReq, err := s.RestoreByRequest(ctx, req.ID)
	if err != nil || byReq.ID != r.ID {
		t.Fatalf("RestoreByRequest: %+v, %v", byReq, err)
	}
	pending, _ := s.Requests(ctx, "pending", "", 50)
	found := false
	for _, p := range pending {
		if p.ID == req.ID {
			found = p.RestoreID != nil && *p.RestoreID == r.ID
		}
	}
	if !found {
		t.Fatal("the approver's queue does not point the request at the restore")
	}
	// The requester cannot approve their own replace.
	if _, err := s.DecideRequest(ctx, req.ID, "approved", "", dba); err != ErrSelfApproval {
		t.Fatalf("self approval: %v", err)
	}
	if _, err := s.DecideRequest(ctx, req.ID, "approved", "ok", approver); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRestoreState(ctx, r.ID, "running", "starting", ""); err != nil {
		t.Fatal(err)
	}
	done, err := s.FinishRestore(ctx, r.ID, tunnel.BackupResult{OK: true, RenamedTo: "orders" + tunnel.KeptMarker + "20261102", Warnings: 2},
		-time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "done" || done.KeepUntil == nil || done.Warnings != 2 {
		t.Fatalf("finished restore: %+v", done)
	}
	due, err := s.KeptCopiesDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	isDue := false
	for _, d := range due {
		isDue = isDue || d.ID == r.ID
	}
	if !isDue {
		t.Fatal("a kept copy past its day is not due to drop")
	}
	if err := s.MarkKeptDropped(ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Restore(ctx, r.ID)
	if after.State != "undone" || !after.KeptDropped {
		t.Fatalf("undone: %+v", after)
	}
	list, _ := s.Restores(ctx, db.ID)
	if len(list) != 1 {
		t.Fatalf("restores: %d", len(list))
	}
}

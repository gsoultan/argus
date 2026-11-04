package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Running a test restore, and the part of the sweep that schedules them, drops
the copies a crashed test left, and raises the alerts.

	RunBackupTest ─▶ grant {test, data key, hash, checks} ─▶ agent ─▶ 'p' progress … 'z' numbers
	              ─▶ evaluateTest (against the last pass) ─▶ row, audit, alert

A test restore runs alone on its server. A backup never waits behind one: it
cancels the test (recorded as skipped, retried an hour later) and goes first.
*/

const (
	// testStaleAfter marks a running test the agent stopped reporting.
	testStaleAfter = 10 * time.Minute
	// testBusyWait is how long a due test waits for its server to be free,
	// or its agent to connect, before it is recorded as skipped.
	testBusyWait = 2 * time.Hour
	// testPreemptWait is how long a backup waits for a test it cancelled
	// to drop its copy and end.
	testPreemptWait = 90 * time.Second
	// testRetryAfterPreempt is when a test that made way for a backup runs
	// again.
	testRetryAfterPreempt = time.Hour
)

// ErrHostBusy means the server is running another backup job.
var ErrHostBusy = errors.New("another backup, restore or test restore is running on this server; a test restore runs alone")

// preemptedTests are tests a backup cancelled, so their end is recorded as
// skipped rather than failed.
var preemptedTests sync.Map

func newScratchName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return tunnel.TestDBPrefix + hex.EncodeToString(b)
}

// unwrapDataKey opens a backup's data key with the organization's key, for
// one grant.
func unwrapDataKey(ctx context.Context, a *API, b DatabaseBackup) ([]byte, error) {
	priv, err := a.backupKeyFor(ctx)
	if err != nil {
		return nil, err
	}
	hdr := tunnel.DumpHeader{KeyID: b.KeyID, WrappedKey: b.wrappedKey}
	if i := strings.Index(b.wrappedKey, "."); i > 0 {
		hdr.WrappedKey, hdr.Ephemeral = b.wrappedKey[:i], b.wrappedKey[i+1:]
	}
	k, err := tunnel.UnwrapBackupKey(priv, hdr)
	if err != nil {
		return nil, fmt.Errorf("the backup's key does not open with this organization's key: %w", err)
	}
	return k, nil
}

// RunBackupTest test-restores backup b of db, as by (an email or
// schedule:<id>). It returns when the test has ended.
func (a *API) RunBackupTest(ctx context.Context, db Database, b DatabaseBackup, by string) (BackupTest, error) {
	if _, err := a.backupKeyFor(ctx); err != nil {
		return BackupTest{}, err
	}
	if b.State != "done" || (b.storageKey == "" && b.localPath == "") {
		return BackupTest{}, errors.New("this backup is not available to test")
	}
	if busy, err := a.store.HostBusy(ctx, db.AssetID); err != nil {
		return BackupTest{}, err
	} else if busy {
		return BackupTest{}, ErrHostBusy
	}
	prev, err := a.store.LastGoodTest(ctx, db.ID)
	if err != nil {
		return BackupTest{}, err
	}
	sched, err := a.store.BackupSchedule(ctx, db.ID)
	if err != nil {
		return BackupTest{}, err
	}
	scratch := newScratchName()
	row, err := a.store.StartBackupTest(ctx, db.ID, b.ID, by, scratch)
	if err != nil {
		return row, err
	}
	a.auditDB(ctx, "db.backup_test_started", "notice", by, db.Hostname,
		fmt.Sprintf("Test restore %s of %s from backup %s (taken %s) started, into %s with %d checks of the admins'.",
			row.ID, db.Name, b.ID, b.StartedAt.UTC().Format(time.RFC3339), scratch, len(sched.TestChecks)))
	finish := func(res tunnel.BackupResult) (BackupTest, error) {
		return a.finishBackupTest(ctx, db, b, row, res, prev)
	}

	dataKey, err := unwrapDataKey(ctx, a, b)
	if err != nil {
		return finish(tunnel.BackupResult{Stage: "start", Error: err.Error()})
	}
	source := "local"
	if b.localPath == "" {
		source = "stream"
	}
	grant, _, err := a.backupGrant(ctx, by, db, tunnel.GrantBackup{Job: tunnel.BackupJobTest, BackupID: b.ID,
		DataKey: base64.StdEncoding.EncodeToString(dataKey), SHA256: b.SHA256,
		Test: &tunnel.GrantTest{Scratch: scratch, Source: source}, Checks: sched.TestChecks,
		Reason: "test restore"})
	for i := range dataKey {
		dataKey[i] = 0
	}
	if err != nil {
		return finish(tunnel.BackupResult{Stage: "start", Error: "could not sign the job's grant"})
	}
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.jobs.add(row.ID, cancel)
	defer a.jobs.remove(row.ID)
	st, _, err := a.openBackupStream(jctx, db, grant)
	if err != nil {
		return finish(tunnel.BackupResult{Stage: "start", Error: err.Error()})
	}
	defer st.Close()
	var wmu sync.Mutex
	defer cancelOnDone(jctx, st, &wmu)()
	if source == "stream" {
		if err := a.streamDown(jctx, st, &wmu, b, func(n int64) {
			_ = a.store.BackupTestProgress(ctx, row.ID, "sending", humanSize(n))
		}); err != nil {
			return finish(tunnel.BackupResult{Stage: "receiving", Error: err.Error()})
		}
	}
	res, err := backupFrames(st, func(p tunnel.BackupProgress) {
		_ = a.store.BackupTestProgress(ctx, row.ID, p.Stage, p.Detail)
	}, nil)
	if err != nil {
		res = tunnel.BackupResult{Stage: "restoring", Error: err.Error()}
		if jctx.Err() != nil {
			res.Error = "cancelled"
		}
	}
	return finish(res)
}

// finishBackupTest judges and records a test's outcome, with its audit
// event and, when it failed, the alert.
func (a *API) finishBackupTest(ctx context.Context, db Database, b DatabaseBackup, row BackupTest,
	res tunnel.BackupResult, prev *BackupTest) (BackupTest, error) {
	state, rep, why := evaluateTest(res, prev)
	if _, ok := preemptedTests.LoadAndDelete(row.ID); ok {
		state, why = "skipped", "made way for a backup; it runs again in an hour"
		t := time.Now().Add(testRetryAfterPreempt)
		_ = a.store.SetTestNext(ctx, db.ID, &t)
	}
	row.State, row.Stage, row.Error, row.Report = state, res.Stage, why, rep
	if res.OK {
		row.Stage = ""
	}
	// Whether a copy may be left behind: none is made before the backup is
	// on the server; after that only the agent's word that it dropped it
	// counts, and without it the sweep asks again.
	row.ScratchDropped = res.Stage == "start" || res.Stage == "receiving"
	if t := res.Test; t != nil {
		row.ScratchDropped = t.Dropped
		row.RestoreMS = t.RestoreMS
		if t.Data != nil {
			row.Tables, row.Bytes = t.Data.TablesTotal, t.Data.Bytes
			for _, tr := range t.Data.Tables {
				if tr.Rows > 0 {
					row.TotalRows += tr.Rows
				}
			}
		}
	}
	out, err := a.store.FinishBackupTest(ctx, row)
	if err != nil {
		a.log.Error("backup test: could not record the outcome", "test", row.ID, "error", err)
		return row, err
	}
	taken := b.StartedAt.UTC().Format(time.RFC3339)
	switch state {
	case "passed", "warned":
		detail := fmt.Sprintf("Test restore %s of %s from backup %s (taken %s) %s: restored in %s, %d tables, %d rows, %s.",
			row.ID, db.Name, b.ID, taken, map[string]string{"passed": "passed", "warned": "passed with warnings"}[state],
			(time.Duration(row.RestoreMS) * time.Millisecond).Round(time.Second), row.Tables, row.TotalRows, humanSize(row.Bytes))
		if why != "" {
			detail += " Warnings: " + why + "."
		}
		a.auditDB(ctx, "db.backup_test_passed", "notice", row.StartedBy, db.Hostname, detail)
		_ = a.store.SetTestOverdue(ctx, db.ID, nil)
	case "skipped":
		a.auditDB(ctx, "db.backup_test_skipped", "notice", row.StartedBy, db.Hostname,
			fmt.Sprintf("Test restore %s of %s from backup %s (taken %s) skipped: %s.", row.ID, db.Name, b.ID, taken, why))
	default:
		a.auditDB(ctx, "db.backup_test_failed", "warn", row.StartedBy, db.Hostname,
			fmt.Sprintf("Test restore %s of %s from backup %s (taken %s) failed: %s.", row.ID, db.Name, b.ID, taken, why))
		title := "Test restore of " + db.Name + " on " + db.Hostname + " failed"
		a.alertBackup(ctx, db, "critical", title, "The backup of "+b.StartedAt.Local().Format("Jan 2, 15:04")+" "+why+".")
	}
	if state == "failed" {
		return out, errors.New(why)
	}
	return out, nil
}

// dropTestCopy asks the agent to drop a throwaway database a test left.
func (a *API) dropTestCopy(ctx context.Context, db Database, name string) error {
	grant, _, err := a.backupGrant(ctx, "system", db, tunnel.GrantBackup{Job: tunnel.BackupJobDropTest, Kept: name})
	if err != nil {
		return err
	}
	st, _, err := a.openBackupStream(ctx, db, grant)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := backupFrames(st, nil, nil)
	if err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	return nil
}

// runChecks runs checks against db's live database, read only.
func (a *API) runChecks(ctx context.Context, by string, db Database, checks []tunnel.TestCheck) ([]tunnel.CheckResult, error) {
	grant, _, err := a.backupGrant(ctx, by, db, tunnel.GrantBackup{Job: tunnel.BackupJobChecks, Checks: checks})
	if err != nil {
		return nil, err
	}
	st, _, err := a.openBackupStream(ctx, db, grant)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	res, err := backupFrames(st, nil, nil)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, errors.New(res.Error)
	}
	return res.Checks, nil
}

// makeWayForBackup cancels any test restore running on db's server, and
// waits for it to drop its copy and end, so a backup never waits behind a
// test.
func (a *API) makeWayForBackup(ctx context.Context, db Database) {
	tests, err := a.store.RunningTestsOnHost(ctx, db.AssetID)
	if err != nil || len(tests) == 0 {
		return
	}
	cancelled := false
	for _, t := range tests {
		preemptedTests.Store(t.ID, true)
		if a.jobs.cancel(t.ID) {
			cancelled = true
		} else {
			preemptedTests.Delete(t.ID)
		}
	}
	if !cancelled {
		return
	}
	deadline := time.Now().Add(testPreemptWait)
	for time.Now().Before(deadline) {
		if rest, err := a.store.RunningTestsOnHost(ctx, db.AssetID); err == nil && len(rest) == 0 {
			// The agent frees the server's slot just after its result.
			time.Sleep(2 * time.Second)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// backupTestSweep is the test restores' part of the sweep: stale tests,
// copies left behind, due tests, and tests overdue.
func (a *API) backupTestSweep(ctx context.Context, now time.Time) {
	if stale, err := a.store.FailStaleTests(ctx, testStaleAfter); err == nil {
		for _, t := range stale {
			a.log.Warn("backup test: the agent stopped reporting", "test", t.ID, "database", t.DatabaseID)
			if db, err := a.store.Database(ctx, t.DatabaseID); err == nil {
				a.auditDB(ctx, "db.backup_test_failed", "warn", t.StartedBy, db.Hostname,
					fmt.Sprintf("Test restore %s of %s failed: the agent stopped reporting on it.", t.ID, db.Name))
				a.alertBackup(ctx, db, "critical", "Test restore of "+db.Name+" on "+db.Hostname+" failed",
					"The agent stopped reporting on it. Argus will drop its copy, "+t.ScratchName+", when the agent is back.")
			}
		}
	}
	if left, err := a.store.UndroppedCopies(ctx); err == nil {
		for _, t := range left {
			db, err := a.store.Database(ctx, t.DatabaseID)
			if err != nil || !db.TunnelConnected {
				continue
			}
			if err := a.dropTestCopy(ctx, db, t.ScratchName); err != nil {
				a.log.Warn("backup test: could not drop a test copy", "test", t.ID, "copy", t.ScratchName, "error", err)
				continue
			}
			_ = a.store.MarkCopyDropped(ctx, t.ID)
			a.auditDB(ctx, "db.backup_test_copy_dropped", "notice", "system", db.Hostname,
				fmt.Sprintf("Dropped %s, the copy test restore %s of %s left behind.", t.ScratchName, t.ID, db.Name))
		}
	}
	if due, err := a.store.DueTestSchedules(ctx, now); err == nil {
		for _, s := range due {
			a.dueTest(ctx, s, now)
		}
	}
	a.overdueTests(ctx, now)
}

// dueTest starts, defers or skips one due test.
func (a *API) dueTest(ctx context.Context, s BackupSchedule, now time.Time) {
	next := s.TestNextRun(now)
	db, err := a.store.Database(ctx, s.DatabaseID)
	if err != nil || db.Engine != tunnel.EnginePostgres {
		_ = a.store.SetTestNext(ctx, s.DatabaseID, next)
		return
	}
	b, err := a.store.UntestedBackup(ctx, db.ID)
	if err != nil {
		// Nothing new since the last test.
		_ = a.store.SetTestNext(ctx, db.ID, next)
		return
	}
	by := "schedule:" + db.ID
	busy, _ := a.store.HostBusy(ctx, db.AssetID)
	if !db.TunnelConnected || busy {
		if now.Sub(*s.TestNextAt) < testBusyWait {
			return // wait for the agent or the server
		}
		why := "the agent on " + db.Hostname + " was not connected"
		if db.TunnelConnected {
			why = db.Hostname + " was busy with another backup job for " + testBusyWait.String()
		}
		_ = a.store.SetTestNext(ctx, db.ID, next)
		_ = a.store.RecordSkippedTest(ctx, db.ID, b.ID, by, why)
		a.auditDB(ctx, "db.backup_test_skipped", "notice", by, db.Hostname,
			fmt.Sprintf("The scheduled test restore of %s was skipped: %s.", db.Name, why))
		return
	}
	_ = a.store.SetTestNext(ctx, db.ID, next)
	go func() {
		if _, err := a.RunBackupTest(context.WithoutCancel(ctx), db, b, by); err != nil {
			a.log.Warn("scheduled test restore did not pass", "database", db.ID, "error", err)
		}
	}()
}

// overdueTests raises one alert per stretch of no passing test: when the
// last pass is older than twice the interval and a day.
func (a *API) overdueTests(ctx context.Context, now time.Time) {
	scheds, err := a.store.TestingSchedules(ctx)
	if err != nil {
		return
	}
	for _, s := range scheds {
		limit := 2*s.TestInterval() + 24*time.Hour
		since := s.UpdatedAt
		last, err := a.store.LastGoodTest(ctx, s.DatabaseID)
		if err != nil {
			continue
		}
		if last != nil && last.FinishedAt != nil && last.FinishedAt.After(since) {
			since = *last.FinishedAt
		}
		if now.Sub(since) < limit {
			if s.TestOverdueAt != nil {
				_ = a.store.SetTestOverdue(ctx, s.DatabaseID, nil)
			}
			continue
		}
		if s.TestOverdueAt != nil {
			continue
		}
		db, err := a.store.Database(ctx, s.DatabaseID)
		if err != nil {
			continue
		}
		days := int(now.Sub(since).Hours() / 24)
		what := "since test restores were turned on"
		if last != nil {
			what = "since " + last.FinishedAt.Local().Format("Jan 2")
		}
		a.auditDB(ctx, "db.backup_test_overdue", "warn", "schedule:"+db.ID, db.Hostname,
			fmt.Sprintf("No test restore of %s has passed in %d days (%s).", db.Name, days, what))
		a.alertBackup(ctx, db, "warning", fmt.Sprintf("No passing test restore of %s in %d days", db.Name, days),
			"Tests are set to run "+s.DescribeTests()+"; none has passed "+what+". Open Backups to see why.")
		t := now
		_ = a.store.SetTestOverdue(ctx, db.ID, &t)
	}
}

//go:build !windows

package agent

import (
	"database/sql"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// SQL Server's DBA tools through a DBA session: refused below dba; activity
// sees a blocked session and who blocks it, and ending the blocker frees it;
// a session elsewhere cannot be ended; indexes, statistics and CHECKDB run
// and are recorded; jobs are only this database's; settings change and read
// back.
func TestMSSQLDBA(t *testing.T) {
	m := newMSService(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	msDB(t, root, "argus_t_dba")
	msDB(t, root, "argus_t_dba_other")
	dropJobs := func() {
		for _, j := range []string{"argus_t_mine", "argus_t_theirs"} {
			_, _ = root.Exec("IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'" + j + "') EXEC msdb.dbo.sp_delete_job @job_name = N'" + j + "'")
		}
	}
	dropJobs()
	t.Cleanup(dropJobs)
	msExec(t, root,
		"USE argus_t_dba; CREATE TABLE dbo.orders (id INT IDENTITY PRIMARY KEY, customer INT, note NVARCHAR(200)); CREATE INDEX ix_customer ON dbo.orders (customer)",
		"USE argus_t_dba; INSERT INTO dbo.orders (customer, note) SELECT TOP 5000 ABS(CHECKSUM(NEWID())) % 100, REPLICATE(N'x', 150) FROM sys.all_objects a CROSS JOIN sys.all_objects b",
		"USE master",
		"EXEC msdb.dbo.sp_add_job @job_name = N'argus_t_mine', @enabled = 1",
		"EXEC msdb.dbo.sp_add_jobstep @job_name = N'argus_t_mine', @step_name = N'wait', @subsystem = N'TSQL', @database_name = N'argus_t_dba', @command = N'WAITFOR DELAY ''00:00:20''; SELECT 1'",
		"EXEC msdb.dbo.sp_add_jobserver @job_name = N'argus_t_mine'",
		"EXEC msdb.dbo.sp_add_job @job_name = N'argus_t_theirs', @enabled = 1",
		"EXEC msdb.dbo.sp_add_jobstep @job_name = N'argus_t_theirs', @step_name = N's', @subsystem = N'TSQL', @database_name = N'argus_t_dba_other', @command = N'SELECT 1'",
		"EXEC msdb.dbo.sp_add_jobserver @job_name = N'argus_t_theirs'")

	w := m.mustOpen(t, "argus_t_dba", tunnel.LevelWrite)
	if a := w.admin(1, tunnel.PGAdminOp{Kind: tunnel.AdminMSActivity}); a.Type != "error" || a.Code != "42501" {
		t.Fatalf("write read activity: %+v", a)
	}
	w.close()

	h := m.mustOpen(t, "argus_t_dba", tunnel.LevelDBA)
	ok := func(id uint32, op tunnel.PGAdminOp) tunnel.PGAnswer {
		t.Helper()
		a := h.admin(id, op)
		if a.Type != "admin" {
			t.Fatalf("%s: %+v", op.Kind, a)
		}
		return a
	}
	refused := func(id uint32, op tunnel.PGAdminOp, want string) {
		t.Helper()
		if a := h.admin(id, op); a.Type != "error" || !strings.Contains(a.Error, want) {
			t.Fatalf("%s: %+v, want an error with %q", op.Kind, a, want)
		}
	}

	// A blocker in this database, and a session it blocks.
	open := func(db string) *sql.DB {
		c, err := sql.Open("sqlserver", mssqlDSN("127.0.0.1", port, "sa", pw, db))
		if err != nil {
			t.Fatal(err)
		}
		c.SetMaxOpenConns(1)
		t.Cleanup(func() { c.Close() })
		return c
	}
	blocker := open("argus_t_dba")
	tx, err := blocker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE dbo.orders SET note = N'held' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	var blockerID int
	if err := tx.QueryRow("SELECT @@SPID").Scan(&blockerID); err != nil {
		t.Fatal(err)
	}
	waiter := open("argus_t_dba")
	waited := make(chan error, 1)
	go func() {
		_, err := waiter.Exec("UPDATE dbo.orders SET note = N'mine' WHERE id = 1")
		waited <- err
	}()
	elsewhere := open("argus_t_dba_other")
	var otherID int
	if err := elsewhere.QueryRow("SELECT @@SPID").Scan(&otherID); err != nil {
		t.Fatal(err)
	}

	var act MSActivity
	for i := 0; i < 50; i++ {
		act = msAdminData[MSActivity](t, ok(uint32(10+i), tunnel.PGAdminOp{Kind: tunnel.AdminMSActivity}))
		if slices.ContainsFunc(act.Sessions, func(s MSActivitySession) bool { return s.BlockedBy == blockerID }) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	blocked := slices.IndexFunc(act.Sessions, func(s MSActivitySession) bool { return s.BlockedBy == blockerID })
	if blocked < 0 || !strings.Contains(act.Sessions[blocked].Statement, "UPDATE") || act.Sessions[blocked].Wait == "" {
		t.Fatalf("no blocked session: %+v", act.Sessions)
	}
	if !slices.ContainsFunc(act.Sessions, func(s MSActivitySession) bool { return s.Session == blockerID && s.OpenTran > 0 }) {
		t.Fatalf("the blocker is not listed with its transaction: %+v", act.Sessions)
	}
	if slices.ContainsFunc(act.Sessions, func(s MSActivitySession) bool { return s.Session == otherID }) {
		t.Fatal("a session in another database is listed")
	}
	refused(70, tunnel.PGAdminOp{Kind: tunnel.AdminMSKill, MSDBA: &tunnel.MSSQLDBASpec{Session: otherID}}, "not connected to this database")
	refused(71, tunnel.PGAdminOp{Kind: tunnel.AdminMSKill, MSDBA: &tunnel.MSSQLDBASpec{Session: 12}}, "not one a person runs")
	ok(72, tunnel.PGAdminOp{Kind: tunnel.AdminMSKill, MSDBA: &tunnel.MSSQLDBASpec{Session: blockerID}})
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the waiter failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ending the blocker did not free the waiter")
	}
	_ = tx.Rollback()

	// Waits and this database's costly queries.
	for i := 0; i < 3; i++ {
		if _, err := waiter.Exec("SELECT COUNT(*) FROM dbo.orders WHERE note LIKE N'%y%'"); err != nil {
			t.Fatal(err)
		}
	}
	perf := msAdminData[MSPerformance](t, ok(73, tunnel.PGAdminOp{Kind: tunnel.AdminMSPerformance, MSDBA: &tunnel.MSSQLDBASpec{Sort: "reads"}}))
	if perf.Since == "" || len(perf.Waits) == 0 || !slices.ContainsFunc(perf.Queries, func(q MSQueryStat) bool {
		return strings.Contains(q.Statement, "LIKE N'%y%'") && q.Executions >= 3
	}) {
		t.Fatalf("performance: %+v", perf)
	}

	// Indexes and statistics: listed, rebuilt, reorganized, updated.
	ix := msAdminData[MSIndexes](t, ok(74, tunnel.PGAdminOp{Kind: tunnel.AdminMSIndexes}))
	if !slices.ContainsFunc(ix.Indexes, func(x MSIndex) bool { return x.Name == "ix_customer" && x.Pages > 0 }) ||
		!slices.ContainsFunc(ix.Statistics, func(x MSStatistic) bool { return x.Name == "ix_customer" }) {
		t.Fatalf("indexes: %+v", ix)
	}
	orders := tunnel.MSSQLDBASpec{Schema: "dbo", Table: "orders"}
	with := func(f func(*tunnel.MSSQLDBASpec)) *tunnel.MSSQLDBASpec { d := orders; f(&d); return &d }
	ok(75, tunnel.PGAdminOp{Kind: tunnel.AdminMSIndexRebuild, MSDBA: with(func(d *tunnel.MSSQLDBASpec) { d.Index = "ix_customer"; d.Online = true })})
	ok(76, tunnel.PGAdminOp{Kind: tunnel.AdminMSIndexReorg, MSDBA: &orders})
	ok(77, tunnel.PGAdminOp{Kind: tunnel.AdminMSStatsUpdate, MSDBA: with(func(d *tunnel.MSSQLDBASpec) { d.FullScan = true })})
	refused(78, tunnel.PGAdminOp{Kind: tunnel.AdminMSIndexRebuild, MSDBA: with(func(d *tunnel.MSSQLDBASpec) { d.Index = "nope" })}, "nope")
	ix = msAdminData[MSIndexes](t, ok(79, tunnel.PGAdminOp{Kind: tunnel.AdminMSIndexes}))
	if i := slices.IndexFunc(ix.Statistics, func(x MSStatistic) bool { return x.Name == "ix_customer" }); i < 0 || ix.Statistics[i].Sampled != ix.Statistics[i].Rows {
		t.Fatalf("a full scan did not read every row: %+v", ix.Statistics)
	}

	// CHECKDB, whole and one table; the database is clean, and says when.
	ok(80, tunnel.PGAdminOp{Kind: tunnel.AdminMSCheckDB})
	ok(81, tunnel.PGAdminOp{Kind: tunnel.AdminMSCheckDB, MSDBA: &tunnel.MSSQLDBASpec{Schema: "dbo", Table: "orders", PhysicalOnly: true}})
	integ := msAdminData[MSIntegrity](t, ok(82, tunnel.PGAdminOp{Kind: tunnel.AdminMSIntegrity}))
	if integ.LastGood == "" || integ.PageVerify != "CHECKSUM" || len(integ.Suspect) != 0 {
		t.Fatalf("integrity: %+v", integ)
	}

	// Jobs: this database's only.
	jobs := msAdminData[MSJobs](t, ok(83, tunnel.PGAdminOp{Kind: tunnel.AdminMSJobs}))
	names := []string{}
	for _, j := range jobs.Jobs {
		names = append(names, j.Name)
	}
	if !slices.Contains(names, "argus_t_mine") || slices.Contains(names, "argus_t_theirs") {
		t.Fatalf("jobs: %v", names)
	}
	refused(84, tunnel.PGAdminOp{Kind: tunnel.AdminMSJobStart, MSDBA: &tunnel.MSSQLDBASpec{Job: "argus_t_theirs"}}, "no step in this database")
	refused(85, tunnel.PGAdminOp{Kind: tunnel.AdminMSJobs, MSDBA: &tunnel.MSSQLDBASpec{Job: "argus_t_theirs"}}, "no step in this database")
	no := false
	ok(86, tunnel.PGAdminOp{Kind: tunnel.AdminMSJobEnable, MSDBA: &tunnel.MSSQLDBASpec{Job: "argus_t_mine", Enabled: &no}})
	var enabled int
	if root.QueryRow("SELECT enabled FROM msdb.dbo.sysjobs WHERE name = 'argus_t_mine'").Scan(&enabled); enabled != 0 {
		t.Fatal("the job is still enabled")
	}
	if jobs.Agent == "Running" {
		ok(87, tunnel.PGAdminOp{Kind: tunnel.AdminMSJobStart, MSDBA: &tunnel.MSSQLDBASpec{Job: "argus_t_mine"}})
		running := false
		for i := 0; i < 50 && !running; i++ {
			jobs = msAdminData[MSJobs](t, ok(uint32(100+i), tunnel.PGAdminOp{Kind: tunnel.AdminMSJobs, MSDBA: &tunnel.MSSQLDBASpec{Job: "argus_t_mine"}}))
			running = slices.ContainsFunc(jobs.Jobs, func(j MSJob) bool { return j.Name == "argus_t_mine" && j.Running })
			time.Sleep(100 * time.Millisecond)
		}
		if !running || len(jobs.Steps) != 1 || jobs.Steps[0].Database != "argus_t_dba" {
			t.Fatalf("the job did not start: %+v", jobs)
		}
		ok(160, tunnel.PGAdminOp{Kind: tunnel.AdminMSJobStop, MSDBA: &tunnel.MSSQLDBASpec{Job: "argus_t_mine"}})
	} else {
		t.Logf("SQL Server Agent is %q; starting a job is not tested", jobs.Agent)
	}

	// Settings: changed, and read back; a setting not offered is refused.
	ok(161, tunnel.PGAdminOp{Kind: tunnel.AdminMSDBSet, MSDBA: &tunnel.MSSQLDBASpec{Setting: "AUTO_UPDATE_STATISTICS_ASYNC", Value: "on"}})
	ok(162, tunnel.PGAdminOp{Kind: tunnel.AdminMSDBSet, MSDBA: &tunnel.MSSQLDBASpec{Setting: "COMPATIBILITY_LEVEL", Value: "150"}})
	refused(163, tunnel.PGAdminOp{Kind: tunnel.AdminMSDBSet, MSDBA: &tunnel.MSSQLDBASpec{Setting: "SINGLE_USER", Value: "ON"}}, "not a setting")
	set := msAdminData[MSSettings](t, ok(164, tunnel.PGAdminOp{Kind: tunnel.AdminMSSettings}))
	val := func(n string) string {
		for _, s := range set.Settings {
			if s.Name == n {
				return s.Value
			}
		}
		return ""
	}
	if val("AUTO_UPDATE_STATISTICS_ASYNC") != "ON" || val("COMPATIBILITY_LEVEL") != "150" || len(set.Files) < 2 || len(set.Server) == 0 {
		t.Fatalf("settings: %+v", set)
	}
	h.close()

	// Every change recorded, before it ran.
	b, err := os.ReadFile(m.dir + "/" + msRecording(t, m.dir, "dba"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"KILL ", "ALTER INDEX [ix_customer] ON [dbo].[orders] REBUILD WITH (ONLINE = ON)",
		"UPDATE STATISTICS [dbo].[orders] WITH FULLSCAN", "DBCC CHECKDB ([argus_t_dba])", "sp_update_job",
		"SET AUTO_UPDATE_STATISTICS_ASYNC ON"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the recording has no %q", want)
		}
	}
}

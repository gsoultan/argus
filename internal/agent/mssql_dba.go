package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
SQL Server's DBA tools for a DBA session (see tunnel/mssqldba.go): activity
and blocking, ending a session, waits and costly queries, index and
statistics upkeep, DBCC CHECKDB, SQL Server Agent jobs, and the database's
settings.

They run as the admin login, like logins and access: the person's login is
db_owner with VIEW SERVER STATE, which can read most of this but cannot end
another login's session or run a job. Each change is recorded before it runs.
Upkeep (a rebuild, statistics, CHECKDB) can take minutes, so it runs as the
session's current request: Cancel stops it, and the session takes nothing
else meanwhile.
*/

// msLongOp bounds index and statistics upkeep and CHECKDB, under the
// session's idle timeout.
const msLongOp = 25 * time.Minute

// MSActivitySession is one session connected to the database, or one
// elsewhere that blocks one that is.
type MSActivitySession struct {
	Session int    `json:"session"`
	Login   string `json:"login"`
	Host    string `json:"host,omitempty"`
	Program string `json:"program,omitempty"`
	// Status is the session's (sleeping, running) or its request's
	// (running, runnable, suspended).
	Status  string `json:"status"`
	Command string `json:"command,omitempty"`
	// Wait is what the request waits on now, for WaitMS so far, on Resource.
	Wait     string `json:"wait,omitempty"`
	WaitMS   int64  `json:"waitMs,omitempty"`
	Resource string `json:"resource,omitempty"`
	// BlockedBy is the session holding what this one waits for.
	BlockedBy int `json:"blockedBy,omitempty"`
	// ElapsedMS is how long the running request has run.
	ElapsedMS    int64  `json:"elapsedMs,omitempty"`
	CPUMS        int64  `json:"cpuMs,omitempty"`
	Reads        int64  `json:"reads,omitempty"`
	OpenTran     int    `json:"openTran,omitempty"`
	LastRequest  string `json:"lastRequest,omitempty"`
	Statement    string `json:"statement,omitempty"`
	Elsewhere    bool   `json:"elsewhere,omitempty"`
	Yours        bool   `json:"yours,omitempty"`
	ArgusSession bool   `json:"argus,omitempty"`
}

// MSActivity answers ms.activity.
type MSActivity struct {
	Sessions []MSActivitySession `json:"sessions"`
}

// MSWait is one wait type's total since the server started.
type MSWait struct {
	Wait    string  `json:"wait"`
	Tasks   int64   `json:"tasks"`
	WaitMS  int64   `json:"waitMs"`
	Signal  int64   `json:"signalMs"`
	Percent float64 `json:"percent"`
}

// MSQueryStat is one cached statement of this database and what it cost.
type MSQueryStat struct {
	Statement  string `json:"statement"`
	Executions int64  `json:"executions"`
	CPUMS      int64  `json:"cpuMs"`
	AvgCPUMS   int64  `json:"avgCpuMs"`
	AvgMS      int64  `json:"avgMs"`
	AvgReads   int64  `json:"avgReads"`
	LastRun    string `json:"lastRun,omitempty"`
}

// MSPerformance answers ms.performance.
type MSPerformance struct {
	Since   string        `json:"since"`
	Waits   []MSWait      `json:"waits"`
	Queries []MSQueryStat `json:"queries"`
}

// MSIndex is one index or heap, summed over its partitions.
type MSIndex struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// Name is empty for a heap.
	Name          string  `json:"name,omitempty"`
	Type          string  `json:"type"`
	Primary       bool    `json:"primary,omitempty"`
	Unique        bool    `json:"unique,omitempty"`
	Disabled      bool    `json:"disabled,omitempty"`
	Pages         int64   `json:"pages"`
	Fragmentation float64 `json:"fragmentation"`
	Seeks         int64   `json:"seeks"`
	Scans         int64   `json:"scans"`
	Lookups       int64   `json:"lookups"`
	Updates       int64   `json:"updates"`
	LastUsed      string  `json:"lastUsed,omitempty"`
	// Keys are the index's key columns, in order; empty for a heap.
	Keys []string `json:"keys,omitempty"`
}

// MSStatistic is one statistics object on a table or view.
type MSStatistic struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	Name        string `json:"name"`
	Auto        bool   `json:"auto,omitempty"`
	LastUpdated string `json:"lastUpdated,omitempty"`
	Rows        int64  `json:"rows"`
	Sampled     int64  `json:"sampled"`
	Modified    int64  `json:"modified"`
}

// MSMissingIndex is an index SQL Server's optimizer wished it had.
type MSMissingIndex struct {
	Schema   string  `json:"schema"`
	Table    string  `json:"table"`
	Equality string  `json:"equality,omitempty"`
	Range    string  `json:"range,omitempty"`
	Include  string  `json:"include,omitempty"`
	Seeks    int64   `json:"seeks"`
	Impact   float64 `json:"impact"`
	// Create is the CREATE INDEX that would make it.
	Create string `json:"create"`
}

// MSIndexes answers ms.indexes.
type MSIndexes struct {
	Indexes    []MSIndex        `json:"indexes"`
	Statistics []MSStatistic    `json:"statistics"`
	Missing    []MSMissingIndex `json:"missing"`
	Truncated  bool             `json:"truncated,omitempty"`
}

// MSSuspectPage is a page SQL Server found damaged.
type MSSuspectPage struct {
	File   int    `json:"file"`
	Page   int64  `json:"page"`
	Event  string `json:"event"`
	Errors int    `json:"errors"`
	When   string `json:"when"`
}

// MSIntegrity answers ms.integrity.
type MSIntegrity struct {
	// LastGood is when CHECKDB last found the database clean; empty if
	// never.
	LastGood   string          `json:"lastGood,omitempty"`
	PageVerify string          `json:"pageVerify"`
	Suspect    []MSSuspectPage `json:"suspect"`
}

// MSJob is a SQL Server Agent job with a step in this database.
type MSJob struct {
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Owner       string `json:"owner,omitempty"`
	Running     bool   `json:"running,omitempty"`
	// LastOutcome is succeeded, failed, retry, cancelled or in progress.
	LastOutcome string `json:"lastOutcome,omitempty"`
	LastRun     string `json:"lastRun,omitempty"`
	LastSecs    int    `json:"lastSecs,omitempty"`
	NextRun     string `json:"nextRun,omitempty"`
	Schedules   int    `json:"schedules"`
}

// MSJobStep is one step of a job.
type MSJobStep struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Subsystem string `json:"subsystem"`
	Database  string `json:"database,omitempty"`
	Command   string `json:"command,omitempty"`
}

// MSJobRun is one line of a job's history: the job's outcome (step 0) or
// a step's.
type MSJobRun struct {
	Step    int    `json:"step"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	When    string `json:"when"`
	Secs    int    `json:"secs"`
	Message string `json:"message,omitempty"`
}

// MSJobs answers ms.jobs.
type MSJobs struct {
	// Agent is the SQL Server Agent service's state: Running, Stopped, or
	// empty when the server does not say.
	Agent   string      `json:"agent"`
	Jobs    []MSJob     `json:"jobs"`
	Steps   []MSJobStep `json:"steps,omitempty"`
	History []MSJobRun  `json:"history,omitempty"`
}

// MSSetting is one database setting, by the name ms.db.set takes.
type MSSetting struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MSFile is one of the database's files.
type MSFile struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Path     string `json:"path"`
	SizeMB   int64  `json:"sizeMb"`
	UsedMB   int64  `json:"usedMb"`
	MaxMB    int64  `json:"maxMb"` // -1 grows until the disk is full
	Growth   string `json:"growth"`
	Readonly bool   `json:"readonly,omitempty"`
}

// MSConfig is one server configuration option.
type MSConfig struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// MSSettings answers ms.settings.
type MSSettings struct {
	Settings []MSSetting `json:"settings"`
	// Facts are what is shown and not changed here.
	Facts  []MSSetting `json:"facts"`
	Files  []MSFile    `json:"files"`
	Server []MSConfig  `json:"server"`
}

// dbaOp runs one DBA operation for a DBA session. adminOp has checked the
// level.
func (s *mssqlSession) dbaOp(ctx context.Context, req tunnel.PGRequest) error {
	op := req.Admin
	stmt, err := tunnel.MSSQLDBASQL(op, s.db.Name)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
	}
	if op.Preview {
		b, _ := json.Marshal(PGAdminDone{SQL: stmt})
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b})
	}
	d := op.MSDBA
	if d == nil {
		d = &tunnel.MSSQLDBASpec{}
	}

	start := time.Now()
	long := op.Kind == tunnel.AdminMSIndexRebuild || op.Kind == tunnel.AdminMSIndexReorg ||
		op.Kind == tunnel.AdminMSStatsUpdate || op.Kind == tunnel.AdminMSCheckDB
	limit := 90 * time.Second
	if long {
		limit = msLongOp
	}
	actx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if long {
		// The session's running request: Cancel reaches it.
		s.cmu.Lock()
		s.curID, s.killed, s.stop = req.ID, "", cancel
		s.cmu.Unlock()
		defer func() {
			s.cmu.Lock()
			s.curID, s.stop = 0, nil
			s.cmu.Unlock()
		}()
	}
	admin, err := mssqlAdminConn(actx, s.db.Host, s.db.Port, s.db.ConfigPath)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
	}
	defer admin.close()
	c, err := admin.db.Conn(actx)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
	}
	defer c.Close()
	if _, err := c.ExecContext(actx, "USE "+tunnel.MSSQLIdent(s.db.Name)); err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
	}

	if !op.Mutates() {
		var data any
		switch op.Kind {
		case tunnel.AdminMSActivity:
			data, err = msActivity(actx, c, s.login)
		case tunnel.AdminMSPerformance:
			data, err = msPerformance(actx, c, d.Sort)
		case tunnel.AdminMSIndexes:
			data, err = msIndexes(actx, c)
		case tunnel.AdminMSIntegrity:
			data, err = msIntegrity(actx, c)
		case tunnel.AdminMSJobs:
			data, err = msJobs(actx, c, d.Job)
		case tunnel.AdminMSSettings:
			data, err = msSettingsOf(actx, c)
		case tunnel.AdminMSHealth:
			data, err = msHealth(actx, c)
		}
		if err != nil {
			return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
		}
		b, _ := json.Marshal(data)
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b, MS: time.Since(start).Milliseconds()})
	}

	// What the statement cannot check for itself: that it stays in this
	// database.
	if why := msCheckReach(actx, c, op); why != "" {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: why})
	}

	// Recorded before it runs. No record, no statement.
	if err := s.rec.write(tunnel.PGRecord{Kind: "admin", Request: req.ID, SQL: stmt, Approval: req.Approval}); err != nil {
		return fmt.Errorf("the recording failed: %w", err)
	}
	// One statement, outside a transaction: ALTER DATABASE and DBCC CHECKDB
	// refuse one, and a rebuild should not hold one open.
	_, err = c.ExecContext(actx, stmt)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		msg := msAllMessages(err)
		s.cmu.Lock()
		killed := s.killed
		s.cmu.Unlock()
		switch {
		case long && killed != "":
			msg = "cancelled; SQL Server stopped it (" + msg + ")"
		case errors.Is(actx.Err(), context.DeadlineExceeded):
			msg = fmt.Sprintf("stopped after %s; run it from the Query tab to give it longer (%s)", limit, msg)
		}
		if rerr := s.rec.write(tunnel.PGRecord{Kind: "error", Request: req.ID, Error: msg, MS: elapsed}); rerr != nil {
			return rerr
		}
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: msg, MS: elapsed})
	}
	tag := msTag([]string{stmt})
	if err := s.rec.write(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: tag, MS: elapsed}); err != nil {
		return err
	}
	b, _ := json.Marshal(PGAdminDone{SQL: stmt, Tag: tag})
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b, Tag: tag, MS: elapsed})
}

// msAllMessages is every error SQL Server sent: CHECKDB reports each
// problem as one.
func msAllMessages(err error) string {
	var me mssql.Error
	if !errors.As(err, &me) || len(me.All) < 2 {
		return mssqlMessage(err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range me.All {
		if e.Message != "" && !seen[e.Message] {
			seen[e.Message] = true
			out = append(out, e.Message)
		}
	}
	return strings.Join(out, "\n")
}

// msCheckReach refuses a change that reaches past this database: ending a
// session that is SQL Server's, this agent's, or neither connected to this
// database nor blocking one that is; or a job with no step in it.
func msCheckReach(ctx context.Context, c *sql.Conn, op *tunnel.PGAdminOp) string {
	d := op.MSDBA
	switch op.Kind {
	case tunnel.AdminMSKill:
		var ok int
		err := c.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.dm_exec_sessions s
		  WHERE s.session_id = @p1 AND s.is_user_process = 1 AND s.session_id <> @@SPID
		    AND (s.database_id = DB_ID() OR s.session_id IN (
		      SELECT r.blocking_session_id FROM sys.dm_exec_requests r WHERE r.database_id = DB_ID()))`, d.Session).Scan(&ok)
		if err != nil {
			return mssqlMessage(err)
		}
		if ok == 0 {
			return fmt.Sprintf("session %d is not connected to this database, nor blocking anyone in it", d.Session)
		}
	case tunnel.AdminMSJobStart, tunnel.AdminMSJobStop, tunnel.AdminMSJobEnable:
		var ok int
		err := c.QueryRowContext(ctx, `SELECT COUNT(*) FROM msdb.dbo.sysjobs j
		  WHERE j.name = @p1 AND EXISTS (SELECT 1 FROM msdb.dbo.sysjobsteps st
		    WHERE st.job_id = j.job_id AND st.database_name = DB_NAME())`, d.Job).Scan(&ok)
		if err != nil {
			return mssqlMessage(err)
		}
		if ok == 0 {
			return "the job " + d.Job + " has no step in this database"
		}
	}
	return ""
}

// msTime writes a time the way the console reads it.
func msTime(t sql.NullTime) string {
	if !t.Valid || t.Time.Year() < 1901 {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

func msActivity(ctx context.Context, c *sql.Conn, mine string) (*MSActivity, error) {
	rows, err := c.QueryContext(ctx, `
	SELECT s.session_id, s.login_name, ISNULL(s.host_name, ''), ISNULL(s.program_name, ''),
	  COALESCE(r.status, s.status), ISNULL(r.command, ''), ISNULL(r.wait_type, ''), ISNULL(r.wait_time, 0),
	  ISNULL(r.wait_resource, ''), ISNULL(r.blocking_session_id, 0), ISNULL(r.total_elapsed_time, 0),
	  ISNULL(r.cpu_time, s.cpu_time), ISNULL(r.logical_reads, s.logical_reads), s.open_transaction_count,
	  s.last_request_start_time,
	  LEFT(ISNULL(CASE WHEN r.sql_handle IS NOT NULL THEN
	    SUBSTRING(t.text, r.statement_start_offset / 2 + 1,
	      (CASE r.statement_end_offset WHEN -1 THEN DATALENGTH(t.text) ELSE r.statement_end_offset END
	        - r.statement_start_offset) / 2 + 1)
	    ELSE t.text END, ''), 4000),
	  CASE WHEN s.database_id = DB_ID() THEN 0 ELSE 1 END
	FROM sys.dm_exec_sessions s
	LEFT JOIN sys.dm_exec_requests r ON r.session_id = s.session_id
	OUTER APPLY (SELECT TOP 1 most_recent_sql_handle h FROM sys.dm_exec_connections cn
	  WHERE cn.session_id = s.session_id ORDER BY cn.connect_time) cn
	OUTER APPLY sys.dm_exec_sql_text(COALESCE(r.sql_handle, cn.h)) t
	WHERE s.is_user_process = 1 AND s.session_id <> @@SPID
	  AND (s.database_id = DB_ID() OR s.session_id IN (
	    SELECT q.blocking_session_id FROM sys.dm_exec_requests q WHERE q.database_id = DB_ID()))
	ORDER BY CASE WHEN r.session_id IS NULL THEN 1 ELSE 0 END, ISNULL(r.total_elapsed_time, 0) DESC, s.session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &MSActivity{Sessions: []MSActivitySession{}}
	for rows.Next() {
		var a MSActivitySession
		var last sql.NullTime
		var elsewhere int
		var waitMS, elapsed, cpu, reads int64
		if err := rows.Scan(&a.Session, &a.Login, &a.Host, &a.Program, &a.Status, &a.Command, &a.Wait, &waitMS,
			&a.Resource, &a.BlockedBy, &elapsed, &cpu, &reads, &a.OpenTran, &last, &a.Statement, &elsewhere); err != nil {
			return nil, err
		}
		a.WaitMS, a.ElapsedMS, a.CPUMS, a.Reads = waitMS, elapsed, cpu, reads
		a.LastRequest = msTime(last)
		a.Elsewhere = elsewhere == 1
		a.Yours = mine != "" && strings.EqualFold(a.Login, mine)
		a.ArgusSession = strings.HasPrefix(strings.ToLower(a.Login), tunnel.StoreUserPrefix)
		if a.BlockedBy == a.Session {
			// Waiting on its own parallel workers is not being blocked.
			a.BlockedBy = 0
		}
		out.Sessions = append(out.Sessions, a)
	}
	return out, rows.Err()
}

// msBenignWaits are wait types that are idle time, not trouble.
var msBenignWaits = []string{
	"BROKER_EVENTHANDLER", "BROKER_RECEIVE_WAITFOR", "BROKER_TASK_STOP", "BROKER_TO_FLUSH", "BROKER_TRANSMITTER",
	"CHECKPOINT_QUEUE", "CHKPT", "CLR_AUTO_EVENT", "CLR_MANUAL_EVENT", "CLR_SEMAPHORE", "DBMIRROR_DBM_EVENT",
	"DBMIRROR_EVENTS_QUEUE", "DBMIRROR_WORKER_QUEUE", "DBMIRRORING_CMD", "DIRTY_PAGE_POLL", "DISPATCHER_QUEUE_SEMAPHORE",
	"EXECSYNC", "FSAGENT", "FT_IFTS_SCHEDULER_IDLE_WAIT", "FT_IFTSHC_MUTEX", "HADR_CLUSAPI_CALL",
	"HADR_FILESTREAM_IOMGR_IOCOMPLETION", "HADR_LOGCAPTURE_WAIT", "HADR_NOTIFICATION_DEQUEUE", "HADR_TIMER_TASK",
	"HADR_WORK_QUEUE", "KSOURCE_WAKEUP", "LAZYWRITER_SLEEP", "LOGMGR_QUEUE", "MEMORY_ALLOCATION_EXT",
	"ONDEMAND_TASK_QUEUE", "PARALLEL_REDO_DRAIN_WORKER", "PARALLEL_REDO_LOG_CACHE", "PARALLEL_REDO_TRAN_LIST",
	"PARALLEL_REDO_WORKER_SYNC", "PARALLEL_REDO_WORKER_WAIT_WORK", "PREEMPTIVE_OS_FLUSHFILEBUFFERS",
	"PREEMPTIVE_XE_GETTARGETSTATE", "PVS_PREALLOCATE", "PWAIT_ALL_COMPONENTS_INITIALIZED",
	"PWAIT_DIRECTLOGCONSUMER_GETNEXT", "PWAIT_EXTENSIBILITY_CLEANUP_TASK", "QDS_ASYNC_QUEUE",
	"QDS_CLEANUP_STALE_QUERIES_TASK_MAIN_LOOP_SLEEP", "QDS_PERSIST_TASK_MAIN_LOOP_SLEEP", "QDS_SHUTDOWN_QUEUE",
	"REDO_THREAD_PENDING_WORK", "REQUEST_FOR_DEADLOCK_SEARCH", "RESOURCE_QUEUE", "SERVER_IDLE_CHECK",
	"SLEEP_BPOOL_FLUSH", "SLEEP_DBSTARTUP", "SLEEP_DCOMSTARTUP", "SLEEP_MASTERDBREADY", "SLEEP_MASTERMDREADY",
	"SLEEP_MASTERUPGRADED", "SLEEP_MSDBSTARTUP", "SLEEP_SYSTEMTASK", "SLEEP_TASK", "SLEEP_TEMPDBSTARTUP",
	"SNI_HTTP_ACCEPT", "SOS_WORK_DISPATCHER", "SP_SERVER_DIAGNOSTICS_SLEEP", "SQLTRACE_BUFFER_FLUSH",
	"SQLTRACE_INCREMENTAL_FLUSH_SLEEP", "SQLTRACE_WAIT_ENTRIES", "UCS_SESSION_REGISTRATION", "VDI_CLIENT_OTHER",
	"WAIT_FOR_RESULTS", "WAIT_XTP_CKPT_CLOSE", "WAIT_XTP_HOST_WAIT", "WAIT_XTP_OFFLINE_CKPT_NEW_LOG",
	"WAIT_XTP_RECOVERY", "WAITFOR", "WAITFOR_TASKSHUTDOWN", "XE_BUFFERMGR_ALLPROCESSED_EVENT", "XE_DISPATCHER_JOIN",
	"XE_DISPATCHER_WAIT", "XE_LIVE_TARGET_TVF", "XE_TIMER_EVENT",
}

func msPerformance(ctx context.Context, c *sql.Conn, sort string) (*MSPerformance, error) {
	out := &MSPerformance{Waits: []MSWait{}, Queries: []MSQueryStat{}}
	var since sql.NullTime
	if err := c.QueryRowContext(ctx, `SELECT sqlserver_start_time FROM sys.dm_os_sys_info`).Scan(&since); err != nil {
		return nil, err
	}
	out.Since = msTime(since)
	benign := "'" + strings.Join(msBenignWaits, "','") + "'"
	rows, err := c.QueryContext(ctx, `
	WITH w AS (SELECT wait_type, waiting_tasks_count, wait_time_ms, signal_wait_time_ms FROM sys.dm_os_wait_stats
	  WHERE wait_time_ms > 0 AND wait_type NOT IN (`+benign+`) AND wait_type NOT LIKE 'SLEEP[_]%')
	SELECT TOP 15 wait_type, waiting_tasks_count, wait_time_ms, signal_wait_time_ms,
	  CAST(100.0 * wait_time_ms / NULLIF(SUM(wait_time_ms) OVER (), 0) AS float)
	FROM w ORDER BY wait_time_ms DESC`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var w MSWait
		var pct sql.NullFloat64
		if err := rows.Scan(&w.Wait, &w.Tasks, &w.WaitMS, &w.Signal, &pct); err != nil {
			rows.Close()
			return nil, err
		}
		w.Percent = pct.Float64
		out.Waits = append(out.Waits, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	order := "qs.total_worker_time"
	switch sort {
	case "duration":
		order = "qs.total_elapsed_time"
	case "reads":
		order = "qs.total_logical_reads"
	case "executions":
		order = "qs.execution_count"
	}
	rows, err = c.QueryContext(ctx, `
	SELECT TOP 25
	  LEFT(SUBSTRING(t.text, qs.statement_start_offset / 2 + 1,
	    (CASE qs.statement_end_offset WHEN -1 THEN DATALENGTH(t.text) ELSE qs.statement_end_offset END
	      - qs.statement_start_offset) / 2 + 1), 4000),
	  qs.execution_count, qs.total_worker_time / 1000,
	  qs.total_worker_time / 1000 / qs.execution_count, qs.total_elapsed_time / 1000 / qs.execution_count,
	  qs.total_logical_reads / qs.execution_count, qs.last_execution_time
	FROM sys.dm_exec_query_stats qs
	CROSS APPLY sys.dm_exec_sql_text(qs.sql_handle) t
	CROSS APPLY (SELECT CONVERT(int, value) dbid FROM sys.dm_exec_plan_attributes(qs.plan_handle)
	  WHERE attribute = 'dbid') pa
	WHERE pa.dbid = DB_ID() AND qs.execution_count > 0
	ORDER BY `+order+` DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var q MSQueryStat
		var last sql.NullTime
		if err := rows.Scan(&q.Statement, &q.Executions, &q.CPUMS, &q.AvgCPUMS, &q.AvgMS, &q.AvgReads, &last); err != nil {
			return nil, err
		}
		q.LastRun = msTime(last)
		out.Queries = append(out.Queries, q)
	}
	return out, rows.Err()
}

const msIndexesMax = 500

func msIndexes(ctx context.Context, c *sql.Conn) (*MSIndexes, error) {
	out := &MSIndexes{Indexes: []MSIndex{}, Statistics: []MSStatistic{}, Missing: []MSMissingIndex{}}
	rows, err := c.QueryContext(ctx, `
	WITH ps AS (
	  SELECT object_id, index_id, SUM(page_count) pages,
	    CASE WHEN SUM(page_count) = 0 THEN 0
	      ELSE SUM(avg_fragmentation_in_percent * page_count) / SUM(page_count) END frag
	  FROM sys.dm_db_index_physical_stats(DB_ID(), NULL, NULL, NULL, 'LIMITED')
	  WHERE alloc_unit_type_desc = 'IN_ROW_DATA' AND index_level = 0
	  GROUP BY object_id, index_id)
	SELECT TOP (@p1) sc.name, o.name, ISNULL(i.name, ''), i.type_desc, i.is_primary_key, i.is_unique, i.is_disabled,
	  ISNULL(ps.pages, 0), CAST(ISNULL(ps.frag, 0) AS float),
	  ISNULL(us.user_seeks, 0), ISNULL(us.user_scans, 0), ISNULL(us.user_lookups, 0), ISNULL(us.user_updates, 0),
	  (SELECT MAX(d) FROM (VALUES (us.last_user_seek), (us.last_user_scan), (us.last_user_lookup)) v(d)),
	  ISNULL(STUFF((SELECT N',' + c.name FROM sys.index_columns ic
	    JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
	    WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.key_ordinal > 0
	    ORDER BY ic.key_ordinal FOR XML PATH(''), TYPE).value('.', 'nvarchar(max)'), 1, 1, N''), N'')
	FROM sys.indexes i
	JOIN sys.objects o ON o.object_id = i.object_id AND o.is_ms_shipped = 0 AND o.type IN ('U', 'V')
	JOIN sys.schemas sc ON sc.schema_id = o.schema_id
	LEFT JOIN ps ON ps.object_id = i.object_id AND ps.index_id = i.index_id
	LEFT JOIN sys.dm_db_index_usage_stats us ON us.database_id = DB_ID() AND us.object_id = i.object_id AND us.index_id = i.index_id
	WHERE o.type = 'U' OR i.index_id > 0
	ORDER BY ISNULL(ps.pages, 0) DESC, sc.name, o.name, i.index_id`, msIndexesMax+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var x MSIndex
		var last sql.NullTime
		var keys string
		if err := rows.Scan(&x.Schema, &x.Table, &x.Name, &x.Type, &x.Primary, &x.Unique, &x.Disabled, &x.Pages,
			&x.Fragmentation, &x.Seeks, &x.Scans, &x.Lookups, &x.Updates, &last, &keys); err != nil {
			rows.Close()
			return nil, err
		}
		x.LastUsed = msTime(last)
		x.Type = strings.ToLower(x.Type)
		if keys != "" {
			x.Keys = strings.Split(keys, ",")
		}
		out.Indexes = append(out.Indexes, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Indexes) > msIndexesMax {
		out.Indexes, out.Truncated = out.Indexes[:msIndexesMax], true
	}

	rows, err = c.QueryContext(ctx, `
	SELECT TOP (@p1) sc.name, o.name, st.name, st.auto_created, sp.last_updated,
	  ISNULL(sp.rows, 0), ISNULL(sp.rows_sampled, 0), ISNULL(sp.modification_counter, 0)
	FROM sys.stats st
	JOIN sys.objects o ON o.object_id = st.object_id AND o.is_ms_shipped = 0 AND o.type IN ('U', 'V')
	JOIN sys.schemas sc ON sc.schema_id = o.schema_id
	OUTER APPLY sys.dm_db_stats_properties(st.object_id, st.stats_id) sp
	ORDER BY ISNULL(sp.modification_counter, 0) DESC, sc.name, o.name, st.name`, msIndexesMax)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var x MSStatistic
		var last sql.NullTime
		if err := rows.Scan(&x.Schema, &x.Table, &x.Name, &x.Auto, &last, &x.Rows, &x.Sampled, &x.Modified); err != nil {
			rows.Close()
			return nil, err
		}
		x.LastUpdated = msTime(last)
		out.Statistics = append(out.Statistics, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = c.QueryContext(ctx, `
	SELECT TOP 50 OBJECT_SCHEMA_NAME(d.object_id), OBJECT_NAME(d.object_id),
	  ISNULL(d.equality_columns, ''), ISNULL(d.inequality_columns, ''), ISNULL(d.included_columns, ''),
	  gs.user_seeks + gs.user_scans, CAST(gs.avg_user_impact AS float)
	FROM sys.dm_db_missing_index_details d
	JOIN sys.dm_db_missing_index_groups g ON g.index_handle = d.index_handle
	JOIN sys.dm_db_missing_index_group_stats gs ON gs.group_handle = g.index_group_handle
	WHERE d.database_id = DB_ID() AND OBJECT_NAME(d.object_id) IS NOT NULL
	ORDER BY gs.avg_total_user_cost * gs.avg_user_impact * (gs.user_seeks + gs.user_scans) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x MSMissingIndex
		if err := rows.Scan(&x.Schema, &x.Table, &x.Equality, &x.Range, &x.Include, &x.Seeks, &x.Impact); err != nil {
			return nil, err
		}
		x.Create = msCreateIndex(x)
		out.Missing = append(out.Missing, x)
	}
	return out, rows.Err()
}

// msCreateIndex writes the CREATE INDEX for a missing index. Its columns
// come from SQL Server already bracketed.
func msCreateIndex(x MSMissingIndex) string {
	keys := strings.Trim(strings.Join([]string{x.Equality, x.Range}, ", "), ", ")
	name := "IX_" + x.Table + "_" + strings.NewReplacer("[", "", "]", "", ", ", "_", " ", "").Replace(keys)
	if len([]rune(name)) > tunnel.MSSQLMaxName {
		name = string([]rune(name)[:tunnel.MSSQLMaxName])
	}
	s := "CREATE INDEX " + tunnel.MSSQLIdent(name) + " ON " + tunnel.MSSQLIdent(x.Schema) + "." + tunnel.MSSQLIdent(x.Table) +
		" (" + keys + ")"
	if x.Include != "" {
		s += " INCLUDE (" + x.Include + ")"
	}
	return s
}

func msIntegrity(ctx context.Context, c *sql.Conn) (*MSIntegrity, error) {
	out := &MSIntegrity{Suspect: []MSSuspectPage{}}
	var last sql.NullTime
	if err := c.QueryRowContext(ctx, `SELECT CAST(DATABASEPROPERTYEX(DB_NAME(), 'LastGoodCheckDbTime') AS datetime2),
	  page_verify_option_desc FROM sys.databases WHERE database_id = DB_ID()`).Scan(&last, &out.PageVerify); err != nil {
		return nil, err
	}
	out.LastGood = msTime(last)
	rows, err := c.QueryContext(ctx, `SELECT file_id, page_id,
	  CASE event_type WHEN 1 THEN 'damaged (823 or 824 error)' WHEN 2 THEN 'bad checksum' WHEN 3 THEN 'torn page'
	    WHEN 4 THEN 'restored' WHEN 5 THEN 'repaired' WHEN 7 THEN 'deallocated by repair' ELSE 'unknown' END,
	  error_count, last_update_date
	FROM msdb.dbo.suspect_pages WHERE database_id = DB_ID() ORDER BY last_update_date DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p MSSuspectPage
		var when sql.NullTime
		if err := rows.Scan(&p.File, &p.Page, &p.Event, &p.Errors, &when); err != nil {
			return nil, err
		}
		p.When = msTime(when)
		out.Suspect = append(out.Suspect, p)
	}
	return out, rows.Err()
}

// msOutcome names a job history run_status.
func msOutcome(n sql.NullInt64) string {
	if !n.Valid {
		return ""
	}
	switch n.Int64 {
	case 0:
		return "failed"
	case 1:
		return "succeeded"
	case 2:
		return "retry"
	case 3:
		return "cancelled"
	case 4:
		return "in progress"
	}
	return "unknown"
}

// msAgentDuration turns the Agent's HHMMSS duration into seconds.
func msAgentDuration(n int) int { return n/10000*3600 + n/100%100*60 + n%100 }

func msJobs(ctx context.Context, c *sql.Conn, job string) (*MSJobs, error) {
	out := &MSJobs{Jobs: []MSJob{}}
	var agent sql.NullString
	err := c.QueryRowContext(ctx, `SELECT TOP 1 status_desc FROM sys.dm_server_services
	  WHERE servicename LIKE 'SQL Server Agent%'`).Scan(&agent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out.Agent = agent.String
	rows, err := c.QueryContext(ctx, `
	SELECT j.name, j.enabled, ISNULL(j.description, ''), ISNULL(cat.name, ''), ISNULL(SUSER_SNAME(j.owner_sid), ''),
	  CASE WHEN ja.start_execution_date IS NOT NULL AND ja.stop_execution_date IS NULL THEN 1 ELSE 0 END,
	  h.run_status, CASE WHEN h.run_date > 0 THEN msdb.dbo.agent_datetime(h.run_date, h.run_time) END, ISNULL(h.run_duration, 0),
	  (SELECT MIN(msdb.dbo.agent_datetime(js.next_run_date, js.next_run_time)) FROM msdb.dbo.sysjobschedules js
	     JOIN msdb.dbo.sysschedules sch ON sch.schedule_id = js.schedule_id AND sch.enabled = 1
	     WHERE js.job_id = j.job_id AND js.next_run_date > 0),
	  (SELECT COUNT(*) FROM msdb.dbo.sysjobschedules js WHERE js.job_id = j.job_id)
	FROM msdb.dbo.sysjobs j
	LEFT JOIN msdb.dbo.syscategories cat ON cat.category_id = j.category_id
	OUTER APPLY (SELECT TOP 1 a.start_execution_date, a.stop_execution_date FROM msdb.dbo.sysjobactivity a
	  WHERE a.job_id = j.job_id AND a.session_id = (SELECT MAX(session_id) FROM msdb.dbo.syssessions)
	  ORDER BY a.start_execution_date DESC) ja
	OUTER APPLY (SELECT TOP 1 run_status, run_date, run_time, run_duration FROM msdb.dbo.sysjobhistory hh
	  WHERE hh.job_id = j.job_id AND hh.step_id = 0 ORDER BY hh.instance_id DESC) h
	WHERE EXISTS (SELECT 1 FROM msdb.dbo.sysjobsteps st WHERE st.job_id = j.job_id AND st.database_name = DB_NAME())
	ORDER BY j.name`)
	if err != nil {
		return nil, err
	}
	found := false
	for rows.Next() {
		var j MSJob
		var running int
		var status sql.NullInt64
		var last, next sql.NullTime
		var dur int
		if err := rows.Scan(&j.Name, &j.Enabled, &j.Description, &j.Category, &j.Owner, &running, &status, &last, &dur,
			&next, &j.Schedules); err != nil {
			rows.Close()
			return nil, err
		}
		j.Running = running == 1
		j.LastOutcome = msOutcome(status)
		j.LastRun = msTime(last)
		j.LastSecs = msAgentDuration(dur)
		j.NextRun = msTime(next)
		out.Jobs = append(out.Jobs, j)
		found = found || j.Name == job
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if job == "" {
		return out, nil
	}
	if !found {
		return nil, errors.New("the job " + job + " has no step in this database")
	}
	rows, err = c.QueryContext(ctx, `SELECT st.step_id, st.step_name, st.subsystem, ISNULL(st.database_name, ''),
	  LEFT(ISNULL(st.command, ''), 4000)
	  FROM msdb.dbo.sysjobsteps st JOIN msdb.dbo.sysjobs j ON j.job_id = st.job_id
	  WHERE j.name = @p1 ORDER BY st.step_id`, job)
	if err != nil {
		return nil, err
	}
	out.Steps = []MSJobStep{}
	for rows.Next() {
		var st MSJobStep
		if err := rows.Scan(&st.ID, &st.Name, &st.Subsystem, &st.Database, &st.Command); err != nil {
			rows.Close()
			return nil, err
		}
		out.Steps = append(out.Steps, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = c.QueryContext(ctx, `SELECT TOP 50 h.step_id, h.step_name, h.run_status,
	  msdb.dbo.agent_datetime(h.run_date, h.run_time), h.run_duration, LEFT(ISNULL(h.message, ''), 2000)
	  FROM msdb.dbo.sysjobhistory h JOIN msdb.dbo.sysjobs j ON j.job_id = h.job_id
	  WHERE j.name = @p1 ORDER BY h.instance_id DESC`, job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out.History = []MSJobRun{}
	for rows.Next() {
		var r MSJobRun
		var status sql.NullInt64
		var when sql.NullTime
		var dur int
		if err := rows.Scan(&r.Step, &r.Name, &status, &when, &dur, &r.Message); err != nil {
			return nil, err
		}
		r.Outcome = msOutcome(status)
		r.When = msTime(when)
		r.Secs = msAgentDuration(dur)
		out.History = append(out.History, r)
	}
	return out, rows.Err()
}

// msServerOptions are the server options shown with the database's
// settings: the ones a DBA looks at first.
var msServerOptions = []string{
	"max server memory (MB)", "min server memory (MB)", "max degree of parallelism", "cost threshold for parallelism",
	"optimize for ad hoc workloads", "backup compression default", "backup checksum default", "remote admin connections",
	"Agent XPs", "clr enabled", "xp_cmdshell", "contained database authentication", "max worker threads",
}

func msSettingsOf(ctx context.Context, c *sql.Conn) (*MSSettings, error) {
	out := &MSSettings{Settings: []MSSetting{}, Facts: []MSSetting{}, Files: []MSFile{}, Server: []MSConfig{}}
	var recovery, pageVerify, snapshot, collation, state, access, logWait, containment string
	var level, target int
	var autoClose, autoShrink, autoCreate, autoUpdate, autoAsync, rcsi, qs, readOnly bool
	var created sql.NullTime
	if err := c.QueryRowContext(ctx, `SELECT recovery_model_desc, compatibility_level, is_auto_close_on, is_auto_shrink_on,
	  is_auto_create_stats_on, is_auto_update_stats_on, is_auto_update_stats_async_on, page_verify_option_desc,
	  is_read_committed_snapshot_on, snapshot_isolation_state_desc, is_query_store_on, target_recovery_time_in_seconds,
	  ISNULL(collation_name, ''), state_desc, user_access_desc, log_reuse_wait_desc, containment_desc, is_read_only, create_date
	  FROM sys.databases WHERE database_id = DB_ID()`).Scan(&recovery, &level, &autoClose, &autoShrink, &autoCreate,
		&autoUpdate, &autoAsync, &pageVerify, &rcsi, &snapshot, &qs, &target, &collation, &state, &access, &logWait,
		&containment, &readOnly, &created); err != nil {
		return nil, err
	}
	onOff := func(b bool) string {
		if b {
			return "ON"
		}
		return "OFF"
	}
	out.Settings = []MSSetting{
		{"RECOVERY", recovery},
		{"COMPATIBILITY_LEVEL", fmt.Sprint(level)},
		{"PAGE_VERIFY", pageVerify},
		{"AUTO_CLOSE", onOff(autoClose)},
		{"AUTO_SHRINK", onOff(autoShrink)},
		{"AUTO_CREATE_STATISTICS", onOff(autoCreate)},
		{"AUTO_UPDATE_STATISTICS", onOff(autoUpdate)},
		{"AUTO_UPDATE_STATISTICS_ASYNC", onOff(autoAsync)},
		{"READ_COMMITTED_SNAPSHOT", onOff(rcsi)},
		{"ALLOW_SNAPSHOT_ISOLATION", onOff(snapshot == "ON" || snapshot == "IN_TRANSITION_TO_ON")},
		{"QUERY_STORE", onOff(qs)},
		{"TARGET_RECOVERY_TIME", fmt.Sprint(target)},
	}
	out.Facts = []MSSetting{
		{"State", state},
		{"User access", access},
		{"Read only", onOff(readOnly)},
		{"Collation", collation},
		{"Containment", containment},
		{"Log waits on", logWait},
		{"Created", msTime(created)},
	}
	rows, err := c.QueryContext(ctx, `SELECT name, type_desc, physical_name, CAST(size AS bigint) * 8 / 1024,
	  CAST(ISNULL(FILEPROPERTY(name, 'SpaceUsed'), 0) AS bigint) * 8 / 1024,
	  CASE max_size WHEN -1 THEN -1 WHEN 268435456 THEN -1 ELSE CAST(max_size AS bigint) * 8 / 1024 END,
	  CASE WHEN growth = 0 THEN 'none' WHEN is_percent_growth = 1 THEN CONCAT(growth, '%')
	    ELSE CONCAT(CAST(growth AS bigint) * 8 / 1024, ' MB') END, is_read_only
	  FROM sys.database_files ORDER BY file_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f MSFile
		if err := rows.Scan(&f.Name, &f.Type, &f.Path, &f.SizeMB, &f.UsedMB, &f.MaxMB, &f.Growth, &f.Readonly); err != nil {
			rows.Close()
			return nil, err
		}
		f.Type = strings.ToLower(f.Type)
		out.Files = append(out.Files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := make([]string, len(msServerOptions))
	for i, n := range msServerOptions {
		names[i] = tunnel.MSSQLString(n)
	}
	rows, err = c.QueryContext(ctx, `SELECT name, CONVERT(nvarchar(64), value_in_use), CONVERT(nvarchar(400), description)
	  FROM sys.configurations WHERE name IN (`+strings.Join(names, ", ")+`) ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x MSConfig
		if err := rows.Scan(&x.Name, &x.Value, &x.Description); err != nil {
			return nil, err
		}
		out.Server = append(out.Server, x)
	}
	return out, rows.Err()
}

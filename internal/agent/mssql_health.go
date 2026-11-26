package agent

import (
	"context"
	"database/sql"
	"strings"
)

/*
ms.health: what the Performance advice for SQL Server needs beyond what
ms.performance (waits, costly statements), ms.indexes (fragmentation, use,
statistics, missing indexes) and ms.settings (database options, files,
server options) already read. Read-only, as the admin login, at DBA only
like every SQL Server DBA panel.

Each source is read on its own: one the server refuses or lacks (an older
version without sys.dm_db_log_info, say) is named in Missing and the rest
still answers. The analysis is the console's (web/src/lib/mssqlPerformance).
*/

// MSHealth answers ms.health.
type MSHealth struct {
	Version string `json:"version"`
	Edition string `json:"edition"`
	// Since is when the server started; every counter here counts from it.
	Since    string `json:"since"`
	UptimeS  int64  `json:"uptimeS"`
	CPUs     int    `json:"cpus"`
	MemoryMB int64  `json:"memoryMb"`
	// TargetMB and CommittedMB are the buffer pool's target and what it
	// holds; SQLMemoryMB is the process's memory in use.
	TargetMB    int64 `json:"targetMb"`
	CommittedMB int64 `json:"committedMb"`
	SQLMemoryMB int64 `json:"sqlMemoryMb"`
	// Counters from sys.dm_os_performance_counters. The cumulative ones
	// (the "/sec" counters) count since the server started.
	PageLife      int64 `json:"pageLife"`
	GrantsPending int64 `json:"grantsPending"`
	Batches       int64 `json:"batches"`
	Compilations  int64 `json:"compilations"`
	Recompiles    int64 `json:"recompiles"`
	Deadlocks     int64 `json:"deadlocks"`
	PageSplits    int64 `json:"pageSplits"`
	FullScans     int64 `json:"fullScans"`
	// TempdbFiles is tempdb's number of data files, TempdbSizes their
	// distinct sizes in MB.
	TempdbFiles int     `json:"tempdbFiles"`
	TempdbSizes []int64 `json:"tempdbSizes"`
	// VLFs is this database's number of virtual log files.
	VLFs int `json:"vlfs"`
	// The plan cache: all of it, and the ad hoc plans used once.
	PlanCacheMB int64 `json:"planCacheMb"`
	SingleUseMB int64 `json:"singleUseMb"`
	// Blocked is how many requests in this database wait on another now;
	// OldestTranS the age of its oldest open transaction.
	Blocked     int   `json:"blocked"`
	OldestTranS int64 `json:"oldestTranS"`
	// Query Store's actual state, and why it went read-only if it did.
	QueryStore       string   `json:"queryStore,omitempty"`
	QueryStoreReason int64    `json:"queryStoreReason,omitempty"`
	QueryStoreUsedMB int64    `json:"queryStoreUsedMb,omitempty"`
	QueryStoreMaxMB  int64    `json:"queryStoreMaxMb,omitempty"`
	Missing          []string `json:"missing,omitempty"`
}

func msHealth(ctx context.Context, c *sql.Conn) (*MSHealth, error) {
	h := &MSHealth{TempdbSizes: []int64{}}
	// The first read must work: the rest is advice on top of it.
	var since sql.NullTime
	if err := c.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(128), SERVERPROPERTY('ProductVersion')),
	  CONVERT(nvarchar(128), SERVERPROPERTY('Edition')), sqlserver_start_time,
	  DATEDIFF(SECOND, sqlserver_start_time, SYSDATETIME()), cpu_count, physical_memory_kb / 1024,
	  committed_target_kb / 1024, committed_kb / 1024
	  FROM sys.dm_os_sys_info`).Scan(&h.Version, &h.Edition, &since, &h.UptimeS, &h.CPUs, &h.MemoryMB,
		&h.TargetMB, &h.CommittedMB); err != nil {
		return nil, err
	}
	h.Since = msTime(since)
	miss := func(what string, err error) {
		if err != nil {
			h.Missing = append(h.Missing, what+": "+mssqlMessage(err))
		}
	}
	miss("process memory", c.QueryRowContext(ctx,
		`SELECT physical_memory_in_use_kb / 1024 FROM sys.dm_os_process_memory`).Scan(&h.SQLMemoryMB))

	// Counter names are padded with spaces, and the object name carries the
	// instance's prefix (SQLServer: or MSSQL$NAME:).
	rows, err := c.QueryContext(ctx, `SELECT RTRIM(object_name), RTRIM(counter_name), RTRIM(instance_name), cntr_value
	  FROM sys.dm_os_performance_counters
	  WHERE (object_name LIKE '%Buffer Manager%' AND counter_name = 'Page life expectancy')
	     OR (object_name LIKE '%Memory Manager%' AND counter_name = 'Memory Grants Pending')
	     OR (object_name LIKE '%SQL Statistics%' AND counter_name IN ('Batch Requests/sec', 'SQL Compilations/sec', 'SQL Re-Compilations/sec'))
	     OR (object_name LIKE '%Locks%' AND counter_name = 'Number of Deadlocks/sec' AND instance_name = '_Total')
	     OR (object_name LIKE '%Access Methods%' AND counter_name IN ('Page Splits/sec', 'Full Scans/sec'))`)
	if err == nil {
		for rows.Next() {
			var obj, name, inst string
			var v int64
			if err = rows.Scan(&obj, &name, &inst, &v); err != nil {
				break
			}
			switch name {
			case "Page life expectancy":
				// Buffer Node rows repeat it per NUMA node; Buffer Manager's
				// is the server's.
				if strings.HasSuffix(obj, "Buffer Manager") {
					h.PageLife = v
				}
			case "Memory Grants Pending":
				h.GrantsPending = v
			case "Batch Requests/sec":
				h.Batches = v
			case "SQL Compilations/sec":
				h.Compilations = v
			case "SQL Re-Compilations/sec":
				h.Recompiles = v
			case "Number of Deadlocks/sec":
				h.Deadlocks = v
			case "Page Splits/sec":
				h.PageSplits = v
			case "Full Scans/sec":
				h.FullScans = v
			}
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
	}
	miss("performance counters", err)

	rows, err = c.QueryContext(ctx, `SELECT CAST(size AS bigint) * 8 / 1024 FROM tempdb.sys.database_files WHERE type = 0`)
	if err == nil {
		seen := map[int64]bool{}
		for rows.Next() {
			var mb int64
			if err = rows.Scan(&mb); err != nil {
				break
			}
			h.TempdbFiles++
			if !seen[mb] {
				seen[mb] = true
				h.TempdbSizes = append(h.TempdbSizes, mb)
			}
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
	}
	miss("tempdb files", err)

	// sys.dm_db_log_info is SQL Server 2016 SP2 and later.
	miss("virtual log files", c.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.dm_db_log_info(DB_ID())`).Scan(&h.VLFs))

	miss("plan cache", c.QueryRowContext(ctx, `SELECT
	  ISNULL(SUM(CAST(size_in_bytes AS bigint)), 0) / 1048576,
	  ISNULL(SUM(CASE WHEN objtype = 'Adhoc' AND usecounts = 1 THEN CAST(size_in_bytes AS bigint) ELSE 0 END), 0) / 1048576
	  FROM sys.dm_exec_cached_plans`).Scan(&h.PlanCacheMB, &h.SingleUseMB))

	miss("blocking", c.QueryRowContext(ctx, `SELECT
	  (SELECT COUNT(*) FROM sys.dm_exec_requests WHERE blocking_session_id <> 0 AND database_id = DB_ID()),
	  ISNULL((SELECT DATEDIFF(SECOND, MIN(t.transaction_begin_time), SYSDATETIME())
	    FROM sys.dm_tran_database_transactions d JOIN sys.dm_tran_active_transactions t ON t.transaction_id = d.transaction_id
	    WHERE d.database_id = DB_ID() AND d.database_transaction_begin_time IS NOT NULL), 0)`).Scan(&h.Blocked, &h.OldestTranS))

	var qs sql.NullString
	var reason, used, max sql.NullInt64
	err = c.QueryRowContext(ctx, `SELECT actual_state_desc, readonly_reason, current_storage_size_mb, max_storage_size_mb
	  FROM sys.database_query_store_options`).Scan(&qs, &reason, &used, &max)
	if err == sql.ErrNoRows {
		err = nil
	}
	miss("Query Store", err)
	h.QueryStore, h.QueryStoreReason, h.QueryStoreUsedMB, h.QueryStoreMaxMB = qs.String, reason.Int64, used.Int64, max.Int64
	return h, nil
}

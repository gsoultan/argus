package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The MySQL and MariaDB half of the Performance panel. The person's own user
holds grants on one schema, and must never be given SELECT on
performance_schema or the server's status (they show every schema's
statements), so the agent reads them as the administrator it already signs
in as for sessions, with fixed statements only, and answers the tables the
console's analysis needs (web/src/lib/mysqlPerformance.ts). Read-only, not
recorded, DBA only: the control plane refuses it below DBA, and so does this
file against the signed grant.
*/

// mysqlPerfRows caps each table.
const mysqlPerfRows = 500

var mysqlPerfStatus = []string{
	"Uptime", "Uptime_since_flush_status", "Questions", "Threads_connected", "Threads_running", "Max_used_connections",
	"Aborted_connects", "Aborted_clients", "Connection_errors_max_connections", "Innodb_buffer_pool_read_requests",
	"Innodb_buffer_pool_reads", "Innodb_buffer_pool_pages_total", "Innodb_buffer_pool_pages_free",
	"Innodb_buffer_pool_wait_free", "Created_tmp_disk_tables", "Created_tmp_tables", "Select_full_join", "Select_scan",
	"Sort_merge_passes", "Slow_queries", "Innodb_row_lock_waits", "Innodb_row_lock_time", "Innodb_row_lock_time_avg",
	"Innodb_deadlocks", "Innodb_log_waits", "Opened_tables", "Table_open_cache_misses", "Table_open_cache_hits",
	"Threads_created", "Connections", "Handler_read_rnd_next", "Handler_read_key",
}

var mysqlPerfVariables = []string{
	"version", "version_comment", "max_connections", "innodb_buffer_pool_size", "innodb_log_file_size",
	"innodb_redo_log_capacity", "innodb_log_buffer_size", "tmp_table_size", "max_heap_table_size", "slow_query_log",
	"long_query_time", "log_queries_not_using_indexes", "table_open_cache", "performance_schema",
	"innodb_flush_log_at_trx_commit", "sync_binlog", "wait_timeout", "interactive_timeout", "thread_cache_size",
	"innodb_buffer_pool_instances", "sort_buffer_size", "join_buffer_size", "innodb_io_capacity",
	"innodb_flush_method", "query_cache_type", "query_cache_size", "innodb_lock_wait_timeout", "max_allowed_packet",
}

// mysqlPerf answers mysql.perf for this session's schema.
func (s *mysqlSession) perf(ctx context.Context, req tunnel.PGRequest) error {
	fail := func(msg string) error { return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: msg}) }
	if s.db.Level != tunnel.LevelDBA {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Code: "42501",
			Error: "reading the server's performance statistics is DBA work; this session is at " + s.db.Level})
	}
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	admin, err := adminConn(actx, s.f.cfg.Sockets, s.db)
	if err != nil {
		return fail(err.Error())
	}
	defer admin.close()
	p := readMySQLPerf(actx, admin.db, s.db.Name)
	b, err := json.Marshal(p)
	if err != nil {
		return fail(err.Error())
	}
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b})
}

// readMySQLPerf runs the fixed reads against db for schema. Each failure is
// named in Missing; the rest still answer.
func readMySQLPerf(ctx context.Context, db *sql.DB, schema string) tunnel.MySQLPerf {
	p := tunnel.MySQLPerf{Status: map[string]string{}, Variables: map[string]string{}}
	table := func(name, q string, args ...any) *tunnel.PoolerTable {
		t, err := mysqlTable(ctx, db, q, args...)
		if err != nil {
			p.Missing = append(p.Missing, name+": "+err.Error())
			return nil
		}
		return t
	}
	// pairs reads NAME, VALUE rows into into. performance_schema is where
	// MySQL 8 keeps them; MariaDB and MySQL 5.7 answer from
	// information_schema (and MariaDB's performance_schema copy is empty
	// when it is off), so that is the second place asked.
	pairs := func(name string, keys []string, into map[string]string, places ...string) {
		var last error
		for _, from := range places {
			ks := keys
			if strings.HasPrefix(from, "information_schema") {
				ks = upperAll(keys)
			}
			args := make([]any, len(ks))
			for i, k := range ks {
				args[i] = k
			}
			t, err := mysqlTable(ctx, db, "SELECT VARIABLE_NAME, VARIABLE_VALUE FROM "+from+
				" WHERE VARIABLE_NAME IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ks)), ",")+")", args...)
			if err != nil {
				last = err
				continue
			}
			for _, r := range t.Rows {
				if len(r) >= 2 {
					into[strings.ToLower(r[0])] = r[1]
				}
			}
			if len(into) > 0 {
				return
			}
		}
		if last != nil {
			p.Missing = append(p.Missing, name+": "+last.Error())
		}
	}
	pairs("status", mysqlPerfStatus, p.Status, "performance_schema.global_status", "information_schema.GLOBAL_STATUS")
	pairs("variables", mysqlPerfVariables, p.Variables, "performance_schema.global_variables", "information_schema.GLOBAL_VARIABLES")
	p.Version = p.Variables["version"]
	if c := p.Variables["version_comment"]; c != "" {
		p.Version += " (" + c + ")"
	}
	p.Digests = table("statements", `SELECT DIGEST_TEXT, COUNT_STAR, SUM_TIMER_WAIT, AVG_TIMER_WAIT, SUM_LOCK_TIME,
  SUM_ROWS_EXAMINED, SUM_ROWS_SENT, SUM_ROWS_AFFECTED, SUM_NO_INDEX_USED, SUM_NO_GOOD_INDEX_USED,
  SUM_CREATED_TMP_DISK_TABLES, SUM_CREATED_TMP_TABLES, SUM_SORT_MERGE_PASSES, SUM_SELECT_FULL_JOIN,
  SUM_TIMER_WAIT / NULLIF((SELECT SUM(SUM_TIMER_WAIT) FROM performance_schema.events_statements_summary_by_digest WHERE SCHEMA_NAME = ?), 0)
FROM performance_schema.events_statements_summary_by_digest
WHERE SCHEMA_NAME = ? AND DIGEST_TEXT IS NOT NULL
ORDER BY SUM_TIMER_WAIT DESC LIMIT 25`, schema, schema)
	p.Tables = table("tables", `SELECT TABLE_NAME, ENGINE, TABLE_ROWS, DATA_LENGTH, INDEX_LENGTH, DATA_FREE,
  (SELECT COUNT(*) FROM information_schema.STATISTICS s WHERE s.TABLE_SCHEMA = t.TABLE_SCHEMA AND s.TABLE_NAME = t.TABLE_NAME AND s.INDEX_NAME = 'PRIMARY')
FROM information_schema.TABLES t WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
ORDER BY DATA_LENGTH + INDEX_LENGTH DESC LIMIT 300`, schema)
	p.Indexes = table("indexes", `SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, COALESCE(SUB_PART, ''), INDEX_TYPE
FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ?
ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX LIMIT 3000`, schema)
	p.IndexUsage = table("index use", `SELECT OBJECT_NAME, INDEX_NAME, COUNT_STAR, COUNT_READ, COUNT_WRITE
FROM performance_schema.table_io_waits_summary_by_index_usage
WHERE OBJECT_SCHEMA = ? AND INDEX_NAME IS NOT NULL LIMIT 3000`, schema)
	p.Trx = table("transactions", `SELECT trx_id, trx_state, TIMESTAMPDIFF(SECOND, trx_started, NOW()), trx_rows_locked,
  trx_rows_modified, COALESCE(trx_query, '')
FROM information_schema.INNODB_TRX WHERE trx_started < NOW() - INTERVAL 5 SECOND
ORDER BY trx_started LIMIT 50`)
	p.Processes = table("connections", `SELECT USER, COMMAND, COALESCE(STATE, ''), COUNT(*), MAX(TIME), COALESCE(DB, '')
FROM information_schema.PROCESSLIST GROUP BY USER, COMMAND, COALESCE(STATE, ''), COALESCE(DB, '') LIMIT 200`)
	return p
}

func upperAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}

// mysqlTable runs q and keeps every cell as text, NULL as "".
func mysqlTable(ctx context.Context, db *sql.DB, q string, args ...any) (*tunnel.PoolerTable, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	t := &tunnel.PoolerTable{Cols: cols, Rows: [][]string{}}
	for rows.Next() {
		if len(t.Rows) >= mysqlPerfRows*6 {
			break
		}
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := make([]string, len(cols))
		for i, c := range cells {
			r[i] = c.String
		}
		t.Rows = append(t.Rows, r)
	}
	return t, rows.Err()
}

package agent

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The pg-admin operations behind a test restore:

	database-size  how big Target is, and where Postgres keeps its data, for
	               the room check (connects to the maintenance database)
	create-test    CREATE DATABASE Target, a throwaway copy that takes no
	               connections but the admin account's
	drop-test      drop Target, which must be a throwaway copy
	test-checks    read the restored copy (connects to it): exact row counts,
	               index validity, size, and the custom checks
	run-checks     the custom checks alone, against a live database, read only

Only names matching tunnel.ValidTestDB are ever created or dropped here.
The checks run each in a READ ONLY transaction with a statement timeout,
and only their first value is kept: a check's answer goes to the control
plane, never a row.
*/

const (
	// testCountTables bounds how many tables a test counts exactly.
	testCountTables = 2000
	// testStatementTimeout bounds each count and each check.
	testStatementTimeout = "60s"
)

func pgTestOp(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	version := conn.ParameterStatus("server_version")
	switch req.Op {
	case "database-size":
		if !tunnel.ValidDBName.MatchString(req.Target) {
			return PGAdminResult{Error: "not a database name Argus will act on: " + req.Target}
		}
		res := conn.ExecParams(ctx, `SELECT pg_database_size($1), current_setting('data_directory')`,
			[][]byte{[]byte(req.Target)}, nil, nil, nil).Read()
		if res.Err != nil {
			return PGAdminResult{Error: res.Err.Error(), Version: version}
		}
		n, _ := strconv.ParseInt(string(res.Rows[0][0]), 10, 64)
		return PGAdminResult{OK: true, Version: version, Bytes: n, DataDir: string(res.Rows[0][1])}
	case "create-test":
		if !tunnel.ValidTestDB.MatchString(req.Target) {
			return PGAdminResult{Error: "not a test copy name: " + req.Target}
		}
		if err := execAll(ctx, conn, "CREATE DATABASE "+pgx(req.Target)+" TEMPLATE template0 CONNECTION LIMIT 0"); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		return PGAdminResult{OK: true, Version: version}
	case "drop-test":
		if !tunnel.ValidTestDB.MatchString(req.Target) {
			return PGAdminResult{Error: "Argus only drops the test copies it made; " + req.Target + " is not one"}
		}
		_ = conn.ExecParams(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = $1 AND pid <> pg_backend_pid()`, [][]byte{[]byte(req.Target)}, nil, nil, nil).Read()
		if err := execAll(ctx, conn, "DROP DATABASE IF EXISTS "+pgx(req.Target)); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		return PGAdminResult{OK: true, Version: version}
	case "test-checks":
		if !tunnel.ValidTestDB.MatchString(req.Database) {
			return PGAdminResult{Error: "test checks only read a test copy; " + req.Database + " is not one"}
		}
		d, err := pgReadTestCopy(ctx, conn)
		if err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		d.Checks = pgRunChecks(ctx, conn, req.Checks)
		return PGAdminResult{OK: true, Version: version, Test: &d}
	case "run-checks":
		if err := tunnel.ValidateTestChecks(req.Checks); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		return PGAdminResult{OK: true, Version: version, Checks: pgRunChecks(ctx, conn, req.Checks)}
	}
	return PGAdminResult{Error: "unknown operation " + req.Op}
}

// pgReadTestCopy counts what a restored copy holds.
func pgReadTestCopy(ctx context.Context, conn *pgconn.PgConn) (tunnel.TestData, error) {
	var d tunnel.TestData
	if err := execAll(ctx, conn, "SET statement_timeout = '"+testStatementTimeout+"'"); err != nil {
		return d, err
	}
	res := conn.ExecParams(ctx, `SELECT n.nspname, c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'
		ORDER BY 1, 2`, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return d, res.Err
	}
	d.TablesTotal = len(res.Rows)
	for i, r := range res.Rows {
		name := string(r[0]) + "." + string(r[1])
		if i >= testCountTables {
			d.Tables = append(d.Tables, tunnel.TableRows{Name: name, Rows: -1})
			continue
		}
		rows := int64(-1)
		ns, err1 := quoteIdent(string(r[0]))
		rel, err2 := quoteIdent(string(r[1]))
		if err1 != nil || err2 != nil {
			d.Tables = append(d.Tables, tunnel.TableRows{Name: name, Rows: rows})
			continue
		}
		c := conn.ExecParams(ctx, "SELECT count(*) FROM "+ns+"."+rel, nil, nil, nil, nil).Read()
		if c.Err == nil && len(c.Rows) == 1 {
			rows, _ = strconv.ParseInt(string(c.Rows[0][0]), 10, 64)
		}
		d.Tables = append(d.Tables, tunnel.TableRows{Name: name, Rows: rows})
	}
	ix := conn.ExecParams(ctx, `SELECT count(*), count(*) FILTER (WHERE NOT i.indisvalid) FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'`,
		nil, nil, nil, nil).Read()
	if ix.Err != nil {
		return d, ix.Err
	}
	n, _ := strconv.Atoi(string(ix.Rows[0][0]))
	bad, _ := strconv.Atoi(string(ix.Rows[0][1]))
	d.Indexes, d.InvalidIndexes = n, bad
	sz := conn.ExecParams(ctx, `SELECT pg_database_size(current_database())`, nil, nil, nil, nil).Read()
	if sz.Err == nil && len(sz.Rows) == 1 {
		d.Bytes, _ = strconv.ParseInt(string(sz.Rows[0][0]), 10, 64)
	}
	return d, nil
}

// pgRunChecks runs each check in its own read-only transaction and keeps
// only its first value.
func pgRunChecks(ctx context.Context, conn *pgconn.PgConn, checks []tunnel.TestCheck) []tunnel.CheckResult {
	var out []tunnel.CheckResult
	for _, c := range checks {
		r := tunnel.CheckResult{Name: c.Name}
		if err := tunnel.ReadOnlyCheckSQL(c.SQL); err != nil {
			r.Error = err.Error()
			out = append(out, r)
			continue
		}
		if err := execAll(ctx, conn, "BEGIN READ ONLY", "SET LOCAL statement_timeout = '"+testStatementTimeout+"'"); err != nil {
			r.Error = err.Error()
			_ = execAll(ctx, conn, "ROLLBACK")
			out = append(out, r)
			continue
		}
		// The extended protocol takes exactly one statement, whatever the
		// text says.
		sql := strings.TrimSuffix(strings.TrimSpace(c.SQL), ";")
		res := conn.ExecParams(ctx, sql, nil, nil, nil, nil).Read()
		_ = execAll(ctx, conn, "ROLLBACK")
		switch {
		case res.Err != nil:
			r.Error = checkError(res.Err)
		case len(res.Rows) == 0 || len(res.Rows[0]) == 0:
			r.Error = "returned no rows"
		default:
			r.OK, r.Value, r.Error = checkValue(res.Rows[0][0])
		}
		out = append(out, r)
	}
	return out
}

// checkValue reads a check's first value: true, or a number above zero,
// passes.
func checkValue(v []byte) (bool, string, string) {
	if v == nil {
		return false, "null", "returned null"
	}
	s := string(v)
	switch s {
	case "t", "true":
		return true, "true", ""
	case "f", "false":
		return false, "false", ""
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f > 0 {
			return true, s, ""
		}
		return false, s, ""
	}
	return false, "", "returned text; a check returns true, or a number above zero"
}

func checkError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "statement timeout") || strings.Contains(msg, "canceling statement") {
		return "timed out after " + testStatementTimeout
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

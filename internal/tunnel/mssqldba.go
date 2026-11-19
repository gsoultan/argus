package tunnel

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

/*
SQL Server's DBA tools for one database, the everyday work SSMS's Activity
Monitor, maintenance plans and database properties do: who is connected and
who blocks whom, ending a session, what the server waits on and which of this
database's queries cost most, rebuilding and reorganizing indexes, updating
statistics, DBCC CHECKDB, the SQL Server Agent jobs that work on this
database, and the database's own settings.

Like the access operations (mssqlaccess.go) they are fixed operations, not
statements typed into the editor: the agent runs them as the admin login, the
text built here is what is recorded, shown and approved, and the control
plane and the agent compute the same text.

The reach is this database. A session ended must be connected to it; a job
started, stopped, enabled or disabled must have a step that runs in it.
Server-wide settings are shown and not changed.
*/

// SQL Server DBA operation kinds.
const (
	// AdminMSActivity lists the sessions connected to this database, what
	// each runs and waits on, and who blocks whom.
	AdminMSActivity = "ms.activity"
	// AdminMSPerformance lists what the server waits on most, and the
	// queries of this database that cost most since they were cached.
	AdminMSPerformance = "ms.performance"
	// AdminMSIndexes lists the database's indexes with their size,
	// fragmentation and use, its statistics with their age, and the indexes
	// SQL Server says are missing.
	AdminMSIndexes = "ms.indexes"
	// AdminMSIntegrity says when CHECKDB last found the database clean,
	// and lists the pages SQL Server has found damaged.
	AdminMSIntegrity = "ms.integrity"
	// AdminMSJobs lists the SQL Server Agent jobs with a step in this
	// database; with MSDBA.Job, that job's steps and recent runs.
	AdminMSJobs = "ms.jobs"
	// AdminMSSettings lists the database's settings and files, and the
	// server's configuration (to read).
	AdminMSSettings = "ms.settings"

	AdminMSKill         = "ms.kill"
	AdminMSIndexRebuild = "ms.index.rebuild"
	AdminMSIndexReorg   = "ms.index.reorganize"
	AdminMSStatsUpdate  = "ms.stats.update"
	AdminMSCheckDB      = "ms.checkdb"
	AdminMSJobStart     = "ms.job.start"
	AdminMSJobStop      = "ms.job.stop"
	AdminMSJobEnable    = "ms.job.enable"
	AdminMSDBSet        = "ms.db.set"
)

// MSSQLDBASpec says what a DBA operation works on. Each kind reads only
// the fields it needs.
type MSSQLDBASpec struct {
	// Session is the session to end, for kill.
	Session int `json:"session,omitempty"`
	// Schema and Table are the table an index, statistics or CHECKTABLE is
	// on. With no Table, CHECKDB checks the whole database.
	Schema string `json:"schema,omitempty"`
	Table  string `json:"table,omitempty"`
	// Index is the index to rebuild or reorganize, or the statistics to
	// update; empty means all of the table's.
	Index string `json:"index,omitempty"`
	// Online rebuilds without locking the table out (Enterprise and
	// Developer editions).
	Online bool `json:"online,omitempty"`
	// FullScan reads every row to update statistics; Sample reads that
	// percent of them. Neither is SQL Server's default sample.
	FullScan bool `json:"fullScan,omitempty"`
	Sample   int  `json:"sample,omitempty"`
	// PhysicalOnly checks pages and allocation only: much faster, for big
	// databases.
	PhysicalOnly bool `json:"physicalOnly,omitempty"`
	// Job is the SQL Server Agent job's name.
	Job string `json:"job,omitempty"`
	// Enabled enables or disables Job, for job.enable.
	Enabled *bool `json:"enabled,omitempty"`
	// Sort orders the costly queries: cpu (the default), duration, reads or
	// executions.
	Sort string `json:"sort,omitempty"`
	// Setting and Value are a database setting to change, for db.set.
	Setting string `json:"setting,omitempty"`
	Value   string `json:"value,omitempty"`
}

// MSSQLDBAKind says whether kind is a SQL Server DBA operation.
func MSSQLDBAKind(kind string) bool {
	switch kind {
	case AdminMSActivity, AdminMSPerformance, AdminMSIndexes, AdminMSIntegrity, AdminMSJobs, AdminMSSettings,
		AdminMSKill, AdminMSIndexRebuild, AdminMSIndexReorg, AdminMSStatsUpdate, AdminMSCheckDB,
		AdminMSJobStart, AdminMSJobStop, AdminMSJobEnable, AdminMSDBSet:
		return true
	}
	return false
}

// MSSQLAdminKind says whether kind is any of SQL Server's administrative
// operations: access or DBA.
func MSSQLAdminKind(kind string) bool { return MSSQLAccessKind(kind) || MSSQLDBAKind(kind) }

// msSettings are the database settings db.set changes, each with the values
// it takes. A number setting takes a range instead.
var msSettings = map[string][]string{
	"RECOVERY":                     {"FULL", "BULK_LOGGED", "SIMPLE"},
	"PAGE_VERIFY":                  {"CHECKSUM", "TORN_PAGE_DETECTION", "NONE"},
	"AUTO_CLOSE":                   {"ON", "OFF"},
	"AUTO_SHRINK":                  {"ON", "OFF"},
	"AUTO_CREATE_STATISTICS":       {"ON", "OFF"},
	"AUTO_UPDATE_STATISTICS":       {"ON", "OFF"},
	"AUTO_UPDATE_STATISTICS_ASYNC": {"ON", "OFF"},
	"READ_COMMITTED_SNAPSHOT":      {"ON", "OFF"},
	"ALLOW_SNAPSHOT_ISOLATION":     {"ON", "OFF"},
	"QUERY_STORE":                  {"ON", "OFF"},
}

// msLevels are the compatibility levels SQL Server 2016 to 2022 take.
var msLevels = []string{"100", "110", "120", "130", "140", "150", "160"}

// MSSQLSettingValues lists the values a database setting takes, in order.
func MSSQLSettingValues(setting string) []string {
	switch setting {
	case "COMPATIBILITY_LEVEL":
		return msLevels
	case "TARGET_RECOVERY_TIME":
		return nil
	}
	return msSettings[setting]
}

// MSSQLDBASQL is a DBA operation's statement; empty for one that only
// reads. database is the session's database.
func MSSQLDBASQL(op *PGAdminOp, database string) (string, error) {
	if op == nil {
		return "", errors.New("no operation")
	}
	d := op.MSDBA
	if d == nil {
		d = &MSSQLDBASpec{}
	}
	switch op.Kind {
	case AdminMSActivity, AdminMSPerformance, AdminMSIndexes, AdminMSIntegrity, AdminMSJobs, AdminMSSettings:
		return "", nil

	case AdminMSKill:
		// SQL Server's own sessions are 50 and below.
		if d.Session <= 50 || d.Session > 32767 {
			return "", fmt.Errorf("session %d is not one a person runs", d.Session)
		}
		return "KILL " + strconv.Itoa(d.Session), nil

	case AdminMSIndexRebuild, AdminMSIndexReorg:
		table, err := msTable(d)
		if err != nil {
			return "", err
		}
		ix := "ALL"
		if d.Index != "" {
			if ix, err = mssqlName("the index", d.Index); err != nil {
				return "", err
			}
		}
		if op.Kind == AdminMSIndexReorg {
			if d.Online {
				return "", errors.New("reorganizing is always online")
			}
			return "ALTER INDEX " + ix + " ON " + table + " REORGANIZE", nil
		}
		s := "ALTER INDEX " + ix + " ON " + table + " REBUILD"
		if d.Online {
			s += " WITH (ONLINE = ON)"
		}
		return s, nil

	case AdminMSStatsUpdate:
		table, err := msTable(d)
		if err != nil {
			return "", err
		}
		s := "UPDATE STATISTICS " + table
		if d.Index != "" {
			st, err := mssqlName("the statistics", d.Index)
			if err != nil {
				return "", err
			}
			s += " " + st
		}
		switch {
		case d.FullScan && d.Sample != 0:
			return "", errors.New("read every row, or a sample, not both")
		case d.FullScan:
			s += " WITH FULLSCAN"
		case d.Sample != 0:
			if d.Sample < 1 || d.Sample > 100 {
				return "", errors.New("the sample is 1 to 100 percent")
			}
			s += " WITH SAMPLE " + strconv.Itoa(d.Sample) + " PERCENT"
		}
		return s, nil

	case AdminMSCheckDB:
		with := " WITH NO_INFOMSGS, ALL_ERRORMSGS"
		if d.PhysicalOnly {
			with += ", PHYSICAL_ONLY"
		}
		if d.Table != "" {
			table, err := msTable(d)
			if err != nil {
				return "", err
			}
			return "DBCC CHECKTABLE (" + MSSQLString(table) + ")" + with, nil
		}
		db, err := mssqlName("the database", database)
		if err != nil {
			return "", err
		}
		return "DBCC CHECKDB (" + db + ")" + with, nil

	case AdminMSJobStart, AdminMSJobStop, AdminMSJobEnable:
		if _, err := mssqlName("the job", d.Job); err != nil {
			return "", err
		}
		job := "@job_name = " + MSSQLString(d.Job)
		switch op.Kind {
		case AdminMSJobStart:
			return "EXEC msdb.dbo.sp_start_job " + job, nil
		case AdminMSJobStop:
			return "EXEC msdb.dbo.sp_stop_job " + job, nil
		}
		if d.Enabled == nil {
			return "", errors.New("enable the job, or disable it?")
		}
		on := "0"
		if *d.Enabled {
			on = "1"
		}
		return "EXEC msdb.dbo.sp_update_job " + job + ", @enabled = " + on, nil

	case AdminMSDBSet:
		db, err := mssqlName("the database", database)
		if err != nil {
			return "", err
		}
		set := strings.ToUpper(d.Setting)
		val := strings.ToUpper(strings.TrimSpace(d.Value))
		switch set {
		case "COMPATIBILITY_LEVEL":
			if !slices.Contains(msLevels, val) {
				return "", fmt.Errorf("the compatibility level is one of %s", strings.Join(msLevels, ", "))
			}
			return "ALTER DATABASE " + db + " SET COMPATIBILITY_LEVEL = " + val, nil
		case "TARGET_RECOVERY_TIME":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 || n > 300 {
				return "", errors.New("the target recovery time is 0 to 300 seconds")
			}
			return "ALTER DATABASE " + db + " SET TARGET_RECOVERY_TIME = " + strconv.Itoa(n) + " SECONDS", nil
		}
		vals, ok := msSettings[set]
		if !ok {
			return "", fmt.Errorf("%s is not a setting changed here", d.Setting)
		}
		if !slices.Contains(vals, val) {
			return "", fmt.Errorf("%s is one of %s", set, strings.Join(vals, ", "))
		}
		if set == "QUERY_STORE" {
			return "ALTER DATABASE " + db + " SET QUERY_STORE = " + val, nil
		}
		return "ALTER DATABASE " + db + " SET " + set + " " + val, nil
	}
	return "", fmt.Errorf("%s is not a SQL Server operation", op.Kind)
}

// msTable is a DBA operation's table, quoted: [schema].[table].
func msTable(d *MSSQLDBASpec) (string, error) {
	s, err := mssqlName("the schema", d.Schema)
	if err != nil {
		return "", err
	}
	t, err := mssqlName("the table", d.Table)
	if err != nil {
		return "", err
	}
	return s + "." + t, nil
}

// MSSQLAdminText is what any SQL Server administrative operation runs, as
// one text with any password redacted: what is recorded, shown and approved.
func MSSQLAdminText(op *PGAdminOp, database string) (string, error) {
	if op != nil && MSSQLDBAKind(op.Kind) {
		return MSSQLDBASQL(op, database)
	}
	return MSSQLAccessText(op, database)
}

package tunnel

import (
	"strings"
	"testing"
)

func TestMSSQLDBAText(t *testing.T) {
	yes, no := true, false
	tbl := func(f func(*MSSQLDBASpec)) *MSSQLDBASpec {
		d := &MSSQLDBASpec{Schema: "dbo", Table: "orders"}
		if f != nil {
			f(d)
		}
		return d
	}
	for _, c := range []struct {
		kind string
		d    *MSSQLDBASpec
		want string
	}{
		{AdminMSActivity, nil, ""},
		{AdminMSKill, &MSSQLDBASpec{Session: 57}, "KILL 57"},
		{AdminMSIndexRebuild, tbl(nil), "ALTER INDEX ALL ON [dbo].[orders] REBUILD"},
		{AdminMSIndexRebuild, tbl(func(d *MSSQLDBASpec) { d.Index = "ix]a"; d.Online = true }),
			"ALTER INDEX [ix]]a] ON [dbo].[orders] REBUILD WITH (ONLINE = ON)"},
		{AdminMSIndexReorg, tbl(func(d *MSSQLDBASpec) { d.Index = "ix" }), "ALTER INDEX [ix] ON [dbo].[orders] REORGANIZE"},
		{AdminMSStatsUpdate, tbl(nil), "UPDATE STATISTICS [dbo].[orders]"},
		{AdminMSStatsUpdate, tbl(func(d *MSSQLDBASpec) { d.Index = "st"; d.FullScan = true }), "UPDATE STATISTICS [dbo].[orders] [st] WITH FULLSCAN"},
		{AdminMSStatsUpdate, tbl(func(d *MSSQLDBASpec) { d.Sample = 25 }), "UPDATE STATISTICS [dbo].[orders] WITH SAMPLE 25 PERCENT"},
		{AdminMSCheckDB, nil, "DBCC CHECKDB ([shop]) WITH NO_INFOMSGS, ALL_ERRORMSGS"},
		{AdminMSCheckDB, tbl(func(d *MSSQLDBASpec) { d.Table = "o'k"; d.PhysicalOnly = true }),
			"DBCC CHECKTABLE (N'[dbo].[o''k]') WITH NO_INFOMSGS, ALL_ERRORMSGS, PHYSICAL_ONLY"},
		{AdminMSJobStart, &MSSQLDBASpec{Job: "nightly 'x'"}, "EXEC msdb.dbo.sp_start_job @job_name = N'nightly ''x'''"},
		{AdminMSJobStop, &MSSQLDBASpec{Job: "j"}, "EXEC msdb.dbo.sp_stop_job @job_name = N'j'"},
		{AdminMSJobEnable, &MSSQLDBASpec{Job: "j", Enabled: &no}, "EXEC msdb.dbo.sp_update_job @job_name = N'j', @enabled = 0"},
		{AdminMSJobEnable, &MSSQLDBASpec{Job: "j", Enabled: &yes}, "EXEC msdb.dbo.sp_update_job @job_name = N'j', @enabled = 1"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "recovery", Value: "full"}, "ALTER DATABASE [shop] SET RECOVERY FULL"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "COMPATIBILITY_LEVEL", Value: "160"}, "ALTER DATABASE [shop] SET COMPATIBILITY_LEVEL = 160"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "QUERY_STORE", Value: "ON"}, "ALTER DATABASE [shop] SET QUERY_STORE = ON"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "TARGET_RECOVERY_TIME", Value: "60"}, "ALTER DATABASE [shop] SET TARGET_RECOVERY_TIME = 60 SECONDS"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "READ_COMMITTED_SNAPSHOT", Value: "ON"}, "ALTER DATABASE [shop] SET READ_COMMITTED_SNAPSHOT ON"},
	} {
		got, err := MSSQLAdminText(&PGAdminOp{Kind: c.kind, MSDBA: c.d}, "shop")
		if err != nil || got != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.kind, got, err, c.want)
		}
	}
}

func TestMSSQLDBARefusals(t *testing.T) {
	for _, c := range []struct {
		kind string
		d    *MSSQLDBASpec
		want string
	}{
		{AdminMSKill, &MSSQLDBASpec{Session: 50}, "not one a person runs"},
		{AdminMSKill, nil, "not one a person runs"},
		{AdminMSIndexRebuild, &MSSQLDBASpec{Table: "t"}, "the schema"},
		{AdminMSIndexReorg, &MSSQLDBASpec{Schema: "s", Table: "t", Online: true}, "always online"},
		{AdminMSStatsUpdate, &MSSQLDBASpec{Schema: "s", Table: "t", FullScan: true, Sample: 10}, "not both"},
		{AdminMSStatsUpdate, &MSSQLDBASpec{Schema: "s", Table: "t", Sample: 101}, "1 to 100"},
		{AdminMSJobStart, &MSSQLDBASpec{}, "the job"},
		{AdminMSJobEnable, &MSSQLDBASpec{Job: "j"}, "enable the job"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "SINGLE_USER", Value: "ON"}, "not a setting"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "RECOVERY", Value: "FULL; DROP DATABASE x"}, "one of"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "COMPATIBILITY_LEVEL", Value: "90"}, "one of"},
		{AdminMSDBSet, &MSSQLDBASpec{Setting: "TARGET_RECOVERY_TIME", Value: "-1"}, "0 to 300"},
		{"ms.nope", nil, "not a SQL Server operation"},
	} {
		_, err := MSSQLDBASQL(&PGAdminOp{Kind: c.kind, MSDBA: c.d}, "shop")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %+v: %v, want %q", c.kind, c.d, err, c.want)
		}
	}
}

// Simple recovery waits for approval everywhere: the op and its text agree.
func TestMSSQLSimpleRecoveryAlwaysWaits(t *testing.T) {
	for _, c := range []struct {
		value string
		want  bool
	}{{"SIMPLE", true}, {"simple", true}, {"FULL", false}} {
		op := &PGAdminOp{Kind: AdminMSDBSet, MSDBA: &MSSQLDBASpec{Setting: "RECOVERY", Value: c.value}}
		text, err := MSSQLAdminText(op, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if MSSQLAlwaysApproval(op) != c.want || MSSQLTextAlwaysApproval(text) != c.want {
			t.Errorf("%s: op %v, text %v, want %v", c.value, MSSQLAlwaysApproval(op), MSSQLTextAlwaysApproval(text), c.want)
		}
	}
	if (&PGAdminOp{Kind: AdminMSActivity}).Mutates() || !(&PGAdminOp{Kind: AdminMSKill}).Mutates() {
		t.Fatal("reads and changes")
	}
}

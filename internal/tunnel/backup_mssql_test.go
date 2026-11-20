package tunnel

import (
	"strings"
	"testing"
)

func TestSQLServerBackupGrants(t *testing.T) {
	base := func(job string) *GrantBackup {
		return &GrantBackup{Job: job, Engine: EngineMSSQL, ConfigPath: MSSQLAdminFile, Name: "Sales DB",
			Host: "127.0.0.1", Port: 1433, DatabaseID: "db-1"}
	}
	if err := base(BackupJobCatalog).Validate(); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	r := base(BackupJobRestore)
	r.BackupID, r.DataKey, r.SHA256 = "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f", "k", strings.Repeat("a", 64)
	r.Restore = &GrantRestore{Mode: RestoreNew, NewName: "Sales Copy", Source: "local"}
	if err := r.Validate(); err != nil {
		t.Fatalf("a mixed-case SQL Server name: %v", err)
	}
	for _, c := range []struct {
		f    func(*GrantBackup)
		want string
	}{
		{func(b *GrantBackup) { b.Job = BackupJobTest }, "SQL Server has no test job"},
		{func(b *GrantBackup) { b.Job = BackupJobChecks }, "SQL Server has no checks job"},
		{func(b *GrantBackup) { b.ConfigPath = "/etc/shadow" }, "not an admin file"},
		{func(b *GrantBackup) { b.Name = "master" }, "not a database name"},
		{func(b *GrantBackup) { b.Engine = EngineMySQL }, "backups are not for mysql"},
		{func(b *GrantBackup) { b.Engine = ""; b.ConfigPath = "/etc/argus/mssql.conf" }, "reads no admin file"},
	} {
		b := base(BackupJobDrop)
		b.Kept = "Sales DB" + KeptMarker + "x"
		c.f(b)
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v, want %q", err, c.want)
		}
	}
	if !ValidBackupDBName(EnginePostgres, "sales") || ValidBackupDBName(EnginePostgres, "Sales") || !ValidBackupDBName(EngineMSSQL, "Sales") {
		t.Fatal("names")
	}
}

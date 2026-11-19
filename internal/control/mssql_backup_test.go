package control

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// SQL Server databases are backed up and restored through the same routes
// as Postgres's; test restores stay Postgres's.
func TestMSSQLBackupRoutes(t *testing.T) {
	_, s, h, pg, admin := dbFixture(t)
	ctx := context.Background()
	m, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMSSQL, Name: "Sales",
		ConfigPath: "/etc/argus/mssql.conf"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/databases/" + m.ID
	if w := do(t, h, http.MethodGet, base+"/backups", "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("backups: %d %s", w.Code, w.Body)
	}
	// A schedule saves with test restores off, and refuses turning them on.
	sched := `{"enabled":true,"every":"daily","atHour":2,"atMinute":0,"weekday":0,"destination":"server","keepLast":7,"keepDays":0`
	if w := do(t, h, http.MethodPut, base+"/backup-schedule", "admin-token", sched+`,"testEvery":"weekly"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a test schedule on SQL Server: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPut, base+"/backup-schedule", "admin-token", sched+`}`); w.Code != http.StatusOK {
		t.Fatalf("schedule: %d %s", w.Code, w.Body)
	}
	if got, _ := s.BackupSchedule(ctx, m.ID); !got.Enabled || got.TestEvery != "off" {
		t.Fatalf("saved schedule: %+v", got)
	}
	b := finishedBackup(t, s, m.ID, admin, time.Now().Add(-time.Hour), false)
	if w := do(t, h, http.MethodPost, base+"/backups/"+b.ID+"/test", "admin-token", `{}`); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "test restores are for Postgres") {
		t.Fatalf("a test restore on SQL Server: %d %s", w.Code, w.Body)
	}
	// SQL Server names: mixed case is a name, a system database is not.
	if _, err := s.CreateRestore(ctx, DatabaseRestore{BackupID: b.ID, DatabaseID: m.ID, Mode: tunnel.RestoreNew,
		NewName: "Sales_Copy", RequestedBy: admin, State: "running"}); err != nil {
		t.Fatalf("a SQL Server name: %v", err)
	}
	if _, err := s.CreateRestore(ctx, DatabaseRestore{BackupID: b.ID, DatabaseID: m.ID, Mode: tunnel.RestoreNew,
		NewName: "msdb", RequestedBy: admin, State: "running"}); err == nil {
		t.Fatal("restored into msdb")
	}
	// A Redis database has no backups.
	r, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineRedis, Name: "0"}, admin)
	if err == nil {
		if w := do(t, h, http.MethodGet, "/api/v1/databases/"+r.ID+"/backups", "admin-token", ""); w.Code != http.StatusBadRequest {
			t.Fatalf("redis backups: %d", w.Code)
		}
	}
}

package control

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/controlbackup"
)

func backupsFixture(t *testing.T) (*ControlBackups, string) {
	t.Helper()
	units := t.TempDir()
	for _, u := range []string{"argus-backup.service", "argus-backup-now.path"} {
		if err := os.WriteFile(filepath.Join(units, u), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	env := filepath.Join(t.TempDir(), "backup.env")
	return &ControlBackups{StateDir: t.TempDir(), EnvFile: env, UnitDirs: []string{units}, Root: root}, root
}

func writeRun(t *testing.T, root, name string, complete bool) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "argus.dump"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if complete {
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"),
			[]byte(`{"recordings":3,"chainEvents":40,"argusVersion":"1.2.22","database":"argus.dump"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestControlBackupsListsRunsNewestFirst(t *testing.T) {
	b, root := backupsFixture(t)
	writeRun(t, root, "20261101T020000Z", true)
	writeRun(t, root, "20261102T020000Z", false)
	writeRun(t, root, "20261103T020000Z", true)
	if err := os.MkdirAll(filepath.Join(root, "recordings"), 0o700); err != nil {
		t.Fatal(err)
	}
	v := b.view()
	if !v.Installed || !v.CanRunNow {
		t.Fatalf("view %+v", v)
	}
	if len(v.Runs) != 3 || v.Runs[0].Name != "20261103T020000Z" {
		t.Fatalf("runs %+v", v.Runs)
	}
	if !v.Runs[0].Complete || v.Runs[0].DatabaseBytes != 2048 || v.Runs[0].Recordings != 3 || v.Runs[0].Version != "1.2.22" {
		t.Fatalf("newest %+v", v.Runs[0])
	}
	if v.Runs[1].Complete {
		t.Fatal("a run without a manifest counted as complete")
	}
}

// What the console set wins over backup.env, which wins over the default.
func TestControlBackupsKeep(t *testing.T) {
	b, _ := backupsFixture(t)
	if v := b.view(); v.Keep != 14 || v.KeepSetHere {
		t.Fatalf("default %+v", v)
	}
	if err := os.WriteFile(b.EnvFile, []byte("# ours\nARGUS_BACKUP_KEEP=30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := b.view(); v.Keep != 30 || v.KeepSetHere {
		t.Fatalf("from backup.env %+v", v)
	}
	if err := controlbackup.WriteSettings(b.StateDir, controlbackup.Settings{Keep: 5, UpdatedBy: "ops@acme.co"}); err != nil {
		t.Fatal(err)
	}
	if v := b.view(); v.Keep != 5 || !v.KeepSetHere || v.KeepSetBy != "ops@acme.co" {
		t.Fatalf("from the console %+v", v)
	}
}

func TestControlBackupsRunningAndRequested(t *testing.T) {
	b, root := backupsFixture(t)
	now := time.Now().UTC()
	if err := controlbackup.WriteLastRun(root, controlbackup.LastRun{StartedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if v := b.view(); !v.Running || v.Requested != nil {
		t.Fatalf("running %+v", v)
	}
	if err := controlbackup.WriteLastRun(root, controlbackup.LastRun{
		StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-50 * time.Minute), OK: false, Error: "pg_dump is not installed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := controlbackup.WriteRequest(b.StateDir, controlbackup.Request{By: "ops@acme.co", At: now.Add(-5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	v := b.view()
	if v.Running || v.Requested == nil || !v.Late || v.Last.Error != "pg_dump is not installed" {
		t.Fatalf("requested %+v", v)
	}
}

func TestControlBackupsWithoutTheUnits(t *testing.T) {
	b, _ := backupsFixture(t)
	b.UnitDirs = []string{t.TempDir()}
	if v := b.view(); v.Installed || v.CanRunNow || v.Reason == "" {
		t.Fatalf("view %+v", v)
	}
}

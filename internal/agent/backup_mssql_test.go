//go:build !windows

package agent

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
SQL Server backups against a real server. SQL Server writes its .bak to a
folder the agent must read too: ARGUS_TEST_MSSQL_BACKUP_DIR is that folder
as host:server, the host path and the path it is mounted at in SQL Server's
container (owned by SQL Server's account).
*/

func msBackupTest(t *testing.T) (msService, string) {
	t.Helper()
	m := newMSService(t)
	both := os.Getenv("ARGUS_TEST_MSSQL_BACKUP_DIR")
	local, server, ok := strings.Cut(both, ":")
	if !ok {
		t.Skip("ARGUS_TEST_MSSQL_BACKUP_DIR is not set (host:server)")
	}
	pw, _ := msTest(t)
	if err := os.WriteFile(m.file, []byte("user = sa\npassword = "+pw+"\nbackup_dir = "+server+"\nbackup_dir_local = "+local+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, local
}

func msBackupGrant(job string, m msService, name string) tunnel.Grant {
	g := backupGrant(job, "127.0.0.1", m.port, name)
	g.Backup.Engine, g.Backup.ConfigPath, g.Backup.AdminUser = tunnel.EngineMSSQL, m.file, ""
	return g
}

func TestMSSQLBackupAndRestore(t *testing.T) {
	m, staging := msBackupTest(t)
	pw, port := msTest(t)
	root := msRoot(t, pw, port)
	const name, copyName = "argus_t_bak", "argus_t_bak_copy"
	kept := name + tunnel.KeptMarker + "20261119t1200"
	drop := func() {
		for _, n := range []string{name, copyName, kept} {
			_, _ = root.Exec("IF DB_ID(N'" + n + "') IS NOT NULL BEGIN ALTER DATABASE [" + n + "] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [" + n + "] END")
		}
	}
	drop()
	t.Cleanup(drop)
	msExec(t, root, "CREATE DATABASE ["+name+"]",
		"CREATE TABLE ["+name+"].dbo.probe (id int PRIMARY KEY, note nvarchar(50))",
		"INSERT INTO ["+name+"].dbo.probe VALUES (1, N'kept')")
	note := func(db string) string {
		var s string
		if err := root.QueryRow("SELECT note FROM [" + db + "].dbo.probe WHERE id = 1").Scan(&s); err != nil {
			t.Fatalf("%s: %v", db, err)
		}
		return s
	}

	dir := t.TempDir()
	svc, pub, key := newBackupService(t, "postgres://x@/x", dir)
	priv, _ := tunnel.DeriveBackupKey(testSecret, "default")

	// The catalog.
	h := runJob(t, svc, pub, key, msBackupGrant(tunnel.BackupJobCatalog, m, name), nil)
	if !h.answer.OK || !h.result.OK || h.result.Catalog == nil || !strings.HasPrefix(h.result.Catalog.DumpVersion, "SQL Server ") {
		t.Fatalf("catalog: %s %+v", h.answer.Error, h.result)
	}

	// A backup: encrypted, verified, streamed, and no .bak left behind.
	g := msBackupGrant(tunnel.BackupJobDump, m, name)
	g.Backup.Destination = tunnel.BackupDestBoth
	g.Backup.PublicKey = tunnel.EncodeBackupPublicKey(priv.PublicKey())
	h = runJob(t, svc, pub, key, g, nil)
	if !h.answer.OK || !h.result.OK {
		t.Fatalf("dump: %s; %s: %s", h.answer.Error, h.result.Stage, h.result.Error)
	}
	if !h.result.Verified || !h.ended || int64(h.data.Len()) != h.result.Bytes || tunnel.SHA256Hex(h.data.Bytes()) != h.result.SHA256 {
		t.Fatalf("dump result: %+v (streamed %d)", h.result, h.data.Len())
	}
	if left, _ := filepath.Glob(filepath.Join(staging, "*.bak")); len(left) != 0 {
		t.Fatalf("plaintext left in the staging folder: %v", left)
	}
	if strings.Contains(h.data.String(), "probe") {
		t.Fatal("the table name is visible in the encrypted file")
	}
	file := h.data.Bytes()
	hdr, _, err := tunnel.ReadDumpHeader(bufio.NewReader(bytes.NewReader(file)))
	if err != nil || hdr.Format != "mssql-bak" {
		t.Fatalf("header: %+v %v", hdr, err)
	}
	dk, err := tunnel.UnwrapBackupKey(priv, hdr)
	if err != nil {
		t.Fatal(err)
	}
	restore := func(r tunnel.GrantRestore, sha string) *backupHarness {
		rg := msBackupGrant(tunnel.BackupJobRestore, m, name)
		from := restoreGrant(g, file, dk, r)
		rg.Backup.DataKey, rg.Backup.SHA256, rg.Backup.Restore = from.Backup.DataKey, from.Backup.SHA256, from.Backup.Restore
		if sha != "" {
			rg.Backup.SHA256 = sha
		}
		return runJob(t, svc, pub, key, rg, nil)
	}

	// Into a new database, from the server's copy.
	h = restore(tunnel.GrantRestore{Mode: tunnel.RestoreNew, NewName: copyName, Source: "local"}, "")
	if !h.answer.OK || !h.result.OK {
		t.Fatalf("restore new: %s; %s: %s", h.answer.Error, h.result.Stage, h.result.Error)
	}
	if note(copyName) != "kept" || note(name) != "kept" {
		t.Fatal("restore new")
	}
	if left, _ := filepath.Glob(filepath.Join(staging, "*.bak")); len(left) != 0 {
		t.Fatalf("plaintext left after the restore: %v", left)
	}
	// Not over a name that is there.
	if h = restore(tunnel.GrantRestore{Mode: tunnel.RestoreNew, NewName: copyName, Source: "local"}, ""); h.result.OK {
		t.Fatal("restored over an existing database")
	}
	// Not from an archive whose hash is not the recorded one.
	if h = restore(tunnel.GrantRestore{Mode: tunnel.RestoreNew, NewName: "argus_t_bak_x", Source: "local"}, strings.Repeat("0", 64)); h.result.OK ||
		!strings.Contains(h.result.Error, "hash") {
		t.Fatalf("a wrong hash: %+v", h.result)
	}

	// Replace: the current database is kept under its new name.
	msExec(t, root, "UPDATE ["+name+"].dbo.probe SET note = N'changed' WHERE id = 1")
	h = restore(tunnel.GrantRestore{Mode: tunnel.RestoreReplace, ApprovalID: "req-1", RenameTo: kept, Source: "local"}, "")
	if !h.answer.OK || !h.result.OK || h.result.RenamedTo != kept {
		t.Fatalf("replace: %s; %s: %s", h.answer.Error, h.result.Stage, h.result.Error)
	}
	if note(name) != "kept" || note(kept) != "changed" {
		t.Fatal("replace")
	}
	// Undo puts the kept copy back.
	ug := msBackupGrant(tunnel.BackupJobUndo, m, name)
	ug.Backup.Kept = kept
	if h = runJob(t, svc, pub, key, ug, nil); !h.result.OK || note(name) != "changed" {
		t.Fatalf("undo: %+v", h.result)
	}
	// Replace again, then drop the kept copy.
	if h = restore(tunnel.GrantRestore{Mode: tunnel.RestoreReplace, ApprovalID: "req-2", RenameTo: kept, Source: "local"}, ""); !h.result.OK {
		t.Fatalf("replace again: %+v", h.result)
	}
	dg := msBackupGrant(tunnel.BackupJobDrop, m, name)
	dg.Backup.Kept = kept
	if h = runJob(t, svc, pub, key, dg, nil); !h.result.OK {
		t.Fatalf("drop kept: %+v", h.result)
	}
	var n int
	_ = root.QueryRow("SELECT COUNT(*) FROM sys.databases WHERE name = @p1", kept).Scan(&n)
	if n != 0 || note(name) != "kept" {
		t.Fatal("drop kept")
	}

	// SQL Server has no test restores.
	tg := msBackupGrant(tunnel.BackupJobChecks, m, name)
	tg.Backup.Checks = []tunnel.TestCheck{{Name: "x", SQL: "SELECT 1"}}
	if h = runJob(t, svc, pub, key, tg, nil); h.answer.OK {
		t.Fatal("a checks job on SQL Server was accepted")
	}
}

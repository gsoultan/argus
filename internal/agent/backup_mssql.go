package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
SQL Server backups, through the same service, grants and archive as
Postgres's.

	argus-control ──"backup" stream──▶ this daemon ──admin login──▶ BACKUP DATABASE … TO DISK
	                                        │
	                                        └── reads the .bak SQL Server wrote, encrypts it to
	                                            <dir>/<database>/<backup>.argusdump, deletes the .bak

SQL Server writes a backup itself, as its own service account, to a path it
can reach: it cannot write to a pipe the agent holds. So the plaintext .bak
lives in a staging folder for as long as it takes to check and encrypt it,
and is deleted whatever happens. A restore does the reverse: the archive is
decrypted into the staging folder, restored WITH MOVE to new files, and the
.bak is deleted.

The staging folder is the server's default backup folder, or backup_dir in
the admin file; backup_dir_local is the same folder as the agent sees it,
when SQL Server runs in a container with that folder mounted.

Every backup is COPY_ONLY, so it never breaks the chain of the backups a DBA
already takes, and WITH CHECKSUM; RESTORE VERIFYONLY reads it back before it
is encrypted, and the encrypted archive is read back and must decrypt to the
same bytes.

Replacing a database renames the current one and keeps it, as on Postgres;
the restored database's files are named after it and the backup, so they
never collide with the kept copy's.
*/

// msStaging is where a .bak waits: as SQL Server names it, and as the
// agent does.
type msStaging struct{ server, local string }

func (m msStaging) file(name string) (server, local string) {
	sep := "/"
	if strings.Contains(m.server, `\`) {
		sep = `\`
	}
	return strings.TrimRight(m.server, `/\`) + sep + name, filepath.Join(m.local, name)
}

// msStagingOf finds the staging folder.
func msStagingOf(ctx context.Context, a *mssqlAdmin) (msStaging, error) {
	m := msStaging{server: a.backupDir, local: a.backupLocal}
	if m.server == "" {
		var dir sql.NullString
		if err := a.db.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('InstanceDefaultBackupPath') AS nvarchar(4000))`).Scan(&dir); err != nil {
			return m, fmt.Errorf("could not ask SQL Server for its backup folder: %s", mssqlMessage(err))
		}
		if !dir.Valid || dir.String == "" {
			return m, errors.New("SQL Server has no default backup folder; put backup_dir = in the admin file")
		}
		m.server = dir.String
	}
	if m.local == "" {
		m.local = m.server
	}
	if st, err := os.Stat(m.local); err != nil || !st.IsDir() {
		return m, fmt.Errorf("the agent cannot reach SQL Server's backup folder %s; if SQL Server runs in a container, "+
			"mount it and put backup_dir_local = in the admin file", m.local)
	}
	return m, nil
}

// msJob signs in as the admin login.
func (j *backupJob) msConn(ctx context.Context) (*mssqlAdmin, error) {
	return mssqlAdminConn(ctx, j.b.Host, j.b.Port, j.b.ConfigPath)
}

// msRun is run for a SQL Server database.
func (j *backupJob) msRun(ctx context.Context, data <-chan []byte) tunnel.BackupResult {
	switch j.b.Job {
	case tunnel.BackupJobCatalog:
		return j.msCatalog(ctx)
	case tunnel.BackupJobPrune:
		return j.prune()
	case tunnel.BackupJobDrop, tunnel.BackupJobUndo:
		return j.msKept(ctx)
	case tunnel.BackupJobDump:
		return j.msDump(ctx)
	case tunnel.BackupJobRestore:
		return j.msRestore(ctx, data)
	}
	return j.fail("start", fmt.Errorf("SQL Server has no %s job", j.b.Job))
}

func (j *backupJob) msCatalog(ctx context.Context) tunnel.BackupResult {
	a, err := j.msConn(ctx)
	if err != nil {
		return j.fail("catalog", err)
	}
	defer a.close()
	c := tunnel.BackupCatalog{ServerVersion: a.version, DumpVersion: "SQL Server " + a.version,
		RestoreVersion: "SQL Server " + a.version, Compression: msCompression(a)}
	if c.Compression == "" {
		c.Compression = "none"
	}
	c.Dir = j.cfg.Dir
	c.FreeBytes = freeBytes(j.cfg.Dir)
	c.Local = map[string]int64{}
	if entries, err := os.ReadDir(filepath.Join(j.cfg.Dir, filepath.Base(j.b.DatabaseID))); err == nil {
		for _, e := range entries {
			if fi, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), backupExt) {
				c.Local[strings.TrimSuffix(e.Name(), backupExt)] = fi.Size()
			}
		}
	}
	return tunnel.BackupResult{OK: true, Catalog: &c}
}

// msCompression is how SQL Server compresses its backups: Express has no
// backup compression.
func msCompression(a *mssqlAdmin) string {
	if strings.Contains(strings.ToLower(a.edition), "express") {
		return ""
	}
	return "sqlserver"
}

// msPercent follows a running BACKUP or RESTORE on session spid, from a
// second connection, until stop closes.
func (j *backupJob) msPercent(ctx context.Context, stage string, spid int, stop <-chan struct{}) {
	a, err := j.msConn(ctx)
	if err != nil {
		return
	}
	defer a.close()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			var pct sql.NullFloat64
			if a.db.QueryRowContext(ctx, `SELECT percent_complete FROM sys.dm_exec_requests WHERE session_id = @p1`, spid).Scan(&pct) == nil && pct.Valid {
				j.progress(stage, fmt.Sprintf("%.0f%%", pct.Float64), -1)
			}
		}
	}
}

// msExec runs one long statement on a, reporting its percent as stage.
func (j *backupJob) msExec(ctx context.Context, a *mssqlAdmin, stage, q string) error {
	var spid int
	if err := a.db.QueryRowContext(ctx, `SELECT @@SPID`).Scan(&spid); err != nil {
		return errors.New(mssqlMessage(err))
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); j.msPercent(ctx, stage, spid, stop) }()
	_, err := a.db.ExecContext(ctx, q)
	close(stop)
	<-done
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("cancelled")
		}
		return errors.New(mssqlMessage(err))
	}
	return nil
}

// msDump backs the database up to the staging folder, checks it, encrypts
// it into the archive and deletes the .bak.
func (j *backupJob) msDump(ctx context.Context) tunnel.BackupResult {
	pub, err := tunnel.DecodeBackupPublicKey(j.b.PublicKey)
	if err != nil {
		return j.fail("start", err)
	}
	dir := filepath.Dir(j.localPath(j.b.BackupID))
	if err := os.MkdirAll(dir, backupDirMode); err != nil {
		return j.fail("start", fmt.Errorf("could not make the backup directory: %w", err))
	}
	_ = os.Chmod(j.cfg.Dir, backupDirMode)
	if free := freeBytes(j.cfg.Dir); j.b.MinFreeBytes > 0 && free > 0 && free < j.b.MinFreeBytes {
		return j.fail("start", fmt.Errorf("not enough free space under %s: %s free, the last backup needed %s",
			j.cfg.Dir, humanBytes(free), humanBytes(j.b.MinFreeBytes)))
	}
	j.progress("starting", "", 0)
	a, err := j.msConn(ctx)
	if err != nil {
		return j.fail("start", err)
	}
	defer a.close()
	stg, err := msStagingOf(ctx, a)
	if err != nil {
		return j.fail("start", err)
	}
	srvBak, locBak := stg.file("argus-" + filepath.Base(j.b.BackupID) + ".bak")
	// The plaintext never outlives the job.
	defer os.Remove(locBak)

	comp := msCompression(a)
	with := " WITH COPY_ONLY, CHECKSUM, INIT, FORMAT"
	if comp != "" {
		with += ", COMPRESSION"
	}
	q := "BACKUP DATABASE " + tunnel.MSSQLIdent(j.b.Name) + " TO DISK = " + tunnel.MSSQLString(srvBak) + with
	j.progress("data", "BACKUP DATABASE", -1)
	if err := j.msExec(ctx, a, "data", q); err != nil {
		_ = os.Remove(locBak)
		return j.fail("data", fmt.Errorf("BACKUP DATABASE failed: %s", err))
	}
	j.progress("verifying", "RESTORE VERIFYONLY", -1)
	if err := j.msExec(ctx, a, "verifying", "RESTORE VERIFYONLY FROM DISK = "+tunnel.MSSQLString(srvBak)+" WITH CHECKSUM"); err != nil {
		return j.fail("verifying", fmt.Errorf("SQL Server cannot read the backup back: %s", err))
	}

	in, err := os.Open(locBak)
	if err != nil {
		return j.fail("finishing", fmt.Errorf("the agent cannot read the backup SQL Server wrote to %s: %w", locBak, err))
	}
	defer in.Close()
	final := j.localPath(j.b.BackupID)
	tmp := final + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return j.fail("finishing", fmt.Errorf("could not write under %s: %w", dir, err))
	}
	defer os.Remove(tmp)
	defer f.Close()
	hasher := sha256.New()
	counting := &countWriter{w: io.MultiWriter(f, hasher), job: j}
	dw, hdr, err := tunnel.NewDumpWriter(counting, pub, tunnel.DumpHeader{
		BackupID: j.b.BackupID, DatabaseID: j.b.DatabaseID, Organization: j.b.Organization,
		DBName: j.b.Name, Host: j.g.Host, ServerVersion: a.version, DumpVersion: "SQL Server " + a.version,
		Format: "mssql-bak", Compression: comp, StartedAt: j.started.UTC(), Reason: j.b.Reason})
	if err != nil {
		return j.fail("finishing", err)
	}
	key := dw.DataKey()
	dw.Forget()
	defer zero(key)
	plain := sha256.New()
	j.progress("encrypting", "", 0)
	if _, err := io.Copy(dw, io.TeeReader(in, plain)); err != nil {
		return j.fail("encrypting", err)
	}
	if err := dw.Close(); err != nil {
		return j.fail("encrypting", err)
	}
	if err := f.Sync(); err != nil {
		return j.fail("finishing", err)
	}
	_ = f.Close()
	_ = in.Close()
	_ = os.Remove(locBak)
	size := dw.Written

	// Readable: the archive must decrypt to the very bytes SQL Server
	// verified.
	j.progress("verifying", "reading the archive back", size)
	if err := msReadBack(tmp, key, j.b.BackupID, plain.Sum(nil)); err != nil {
		return j.fail("verifying", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return j.fail("finishing", err)
	}
	res := tunnel.BackupResult{OK: true, Bytes: size, SHA256: hex.EncodeToString(hasher.Sum(nil)),
		WrappedKey: hdr.WrappedKey + "." + hdr.Ephemeral, KeyID: hdr.KeyID, LocalPath: final, Verified: true,
		Compression: comp, ServerVersion: a.version, DumpVersion: "SQL Server " + a.version}
	if j.b.Destination != tunnel.BackupDestServer {
		j.progress("uploading", "", 0)
		if err := j.upload(ctx, final, size); err != nil {
			return j.fail("uploading", err)
		}
		if j.b.Destination == tunnel.BackupDestControl {
			_ = os.Remove(final)
			res.LocalPath = ""
		}
	}
	return res
}

// msReadBack decrypts the archive at path and checks it is want.
func msReadBack(path string, key []byte, backupID string, want []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dr, err := tunnel.NewDumpReader(f, key, backupID)
	if err != nil {
		return fmt.Errorf("the file just written does not open: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, dr); err != nil {
		return fmt.Errorf("the file just written does not decrypt: %w", err)
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), hex.EncodeToString(want)) {
		return errors.New("the file just written does not decrypt to the backup SQL Server wrote")
	}
	return nil
}

// msFile is one file of a backup, from RESTORE FILELISTONLY.
type msFile struct {
	logical string
	typ     string
	id      int64
}

// msFiles lists the backup's files.
func msFiles(ctx context.Context, a *mssqlAdmin, srvBak string) ([]msFile, error) {
	rows, err := a.db.QueryContext(ctx, "RESTORE FILELISTONLY FROM DISK = "+tunnel.MSSQLString(srvBak))
	if err != nil {
		return nil, errors.New(mssqlMessage(err))
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []msFile
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		var f msFile
		for i, c := range cols {
			switch c {
			case "LogicalName":
				f.logical = fmt.Sprint(vals[i])
			case "Type":
				f.typ = fmt.Sprint(vals[i])
			case "FileId":
				f.id, _ = strconv.ParseInt(fmt.Sprint(vals[i]), 10, 64)
			}
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New(mssqlMessage(err))
	}
	if len(out) == 0 {
		return nil, errors.New("the backup lists no files")
	}
	return out, nil
}

// msMoves is the WITH MOVE list that puts each file of the backup under
// SQL Server's data and log folders, named for target and the backup.
func msMoves(ctx context.Context, a *mssqlAdmin, target, backupID string, files []msFile) (string, error) {
	var data, logs sql.NullString
	if err := a.db.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(4000)),
	  CAST(SERVERPROPERTY('InstanceDefaultLogPath') AS nvarchar(4000))`).Scan(&data, &logs); err != nil {
		return "", errors.New(mssqlMessage(err))
	}
	if !data.Valid || data.String == "" {
		return "", errors.New("SQL Server has no default data folder")
	}
	if !logs.Valid || logs.String == "" {
		logs = data
	}
	join := func(dir, name string) string {
		sep := "/"
		if strings.Contains(dir, `\`) {
			sep = `\`
		}
		return strings.TrimRight(dir, `/\`) + sep + name
	}
	// The file names: the target's name with anything a path would mind
	// replaced, the backup's first eight characters, the file's id.
	base := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' || r < 0x20 {
			return '_'
		}
		return r
	}, target)
	short := backupID
	if len(short) > 8 {
		short = short[:8]
	}
	var moves []string
	for _, f := range files {
		name := fmt.Sprintf("%s_%s_%d", base, short, f.id)
		dir := data.String
		switch f.typ {
		case "L":
			name += ".ldf"
			dir = logs.String
		case "D":
			if f.id == 1 {
				name += ".mdf"
			} else {
				name += ".ndf"
			}
		}
		moves = append(moves, "MOVE "+tunnel.MSSQLString(f.logical)+" TO "+tunnel.MSSQLString(join(dir, name)))
	}
	return strings.Join(moves, ", "), nil
}

// msExists says whether a database by that name is there.
func msExists(ctx context.Context, a *mssqlAdmin, name string) (bool, error) {
	var n int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.databases WHERE name = @p1`, name).Scan(&n); err != nil {
		return false, errors.New(mssqlMessage(err))
	}
	return n > 0, nil
}

// msRename ends the sessions on from and renames it to to.
func msRename(ctx context.Context, a *mssqlAdmin, from, to string) error {
	for _, q := range []string{
		"ALTER DATABASE " + tunnel.MSSQLIdent(from) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"ALTER DATABASE " + tunnel.MSSQLIdent(from) + " MODIFY NAME = " + tunnel.MSSQLIdent(to),
		"ALTER DATABASE " + tunnel.MSSQLIdent(to) + " SET MULTI_USER",
	} {
		if _, err := a.db.ExecContext(ctx, q); err != nil {
			// Leave it usable whatever happened.
			_, _ = a.db.ExecContext(context.WithoutCancel(ctx), "IF DB_ID("+tunnel.MSSQLString(from)+") IS NOT NULL ALTER DATABASE "+
				tunnel.MSSQLIdent(from)+" SET MULTI_USER")
			return errors.New(mssqlMessage(err))
		}
	}
	return nil
}

// msDrop ends the sessions on name and drops it.
func msDrop(ctx context.Context, a *mssqlAdmin, name string) error {
	ok, err := msExists(ctx, a, name)
	if err != nil || !ok {
		return err
	}
	for _, q := range []string{
		"ALTER DATABASE " + tunnel.MSSQLIdent(name) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"DROP DATABASE " + tunnel.MSSQLIdent(name),
	} {
		if _, err := a.db.ExecContext(ctx, q); err != nil {
			// A database left mid-restore cannot be set single user; drop it as it is.
			if _, err2 := a.db.ExecContext(ctx, "DROP DATABASE "+tunnel.MSSQLIdent(name)); err2 == nil {
				return nil
			}
			return errors.New(mssqlMessage(err))
		}
	}
	return nil
}

// msRestore applies the archive named by the grant.
func (j *backupJob) msRestore(ctx context.Context, data <-chan []byte) tunnel.BackupResult {
	key, err := base64.StdEncoding.DecodeString(j.b.DataKey)
	if err != nil || len(key) != 32 {
		return j.fail("start", errors.New("the grant's key is not readable"))
	}
	defer zero(key)
	r := j.b.Restore
	path, cleanup, res := j.archive(ctx, r.Source, data)
	defer cleanup()
	if res != nil {
		return *res
	}
	a, err := j.msConn(ctx)
	if err != nil {
		return j.fail("start", err)
	}
	defer a.close()
	stg, err := msStagingOf(ctx, a)
	if err != nil {
		return j.fail("start", err)
	}
	target := r.NewName
	if r.Mode == tunnel.RestoreReplace {
		target = j.b.Name
	} else if there, err := msExists(ctx, a, target); err != nil {
		return j.fail("start", err)
	} else if there {
		return j.fail("start", fmt.Errorf("%s already has a database called %s", j.g.Host, target))
	}

	// Decrypt into the staging folder, owned like it so SQL Server can read it.
	srvBak, locBak := stg.file("argus-restore-" + filepath.Base(j.b.BackupID) + ".bak")
	defer os.Remove(locBak)
	if err := j.msUnpack(path, locBak, key, stg.local); err != nil {
		return j.fail("unpacking", err)
	}
	files, err := msFiles(ctx, a, srvBak)
	if err != nil {
		return j.fail("unpacking", fmt.Errorf("SQL Server cannot read the backup: %s", err))
	}
	moves, err := msMoves(ctx, a, target, j.b.BackupID, files)
	if err != nil {
		return j.fail("unpacking", err)
	}

	renamed := ""
	if r.Mode == tunnel.RestoreReplace {
		j.progress("preparing", "ending sessions and renaming "+j.b.Name+" to "+r.RenameTo, -1)
		if there, err := msExists(ctx, a, r.RenameTo); err != nil || there {
			return j.fail("preparing", fmt.Errorf("%s is there already", r.RenameTo))
		}
		if err := msRename(ctx, a, j.b.Name, r.RenameTo); err != nil {
			return j.fail("preparing", fmt.Errorf("could not rename %s: %s", j.b.Name, err))
		}
		renamed = r.RenameTo
	}
	j.progress("restoring", "RESTORE DATABASE "+target, -1)
	q := "RESTORE DATABASE " + tunnel.MSSQLIdent(target) + " FROM DISK = " + tunnel.MSSQLString(srvBak) +
		" WITH " + moves + ", CHECKSUM, RECOVERY"
	if err := j.msExec(ctx, a, "restoring", q); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		_ = msDrop(rctx, a, target)
		return j.msRollback(rctx, a, renamed, j.fail("restoring", fmt.Errorf("RESTORE DATABASE failed: %s", err)))
	}
	fi, _ := os.Stat(path)
	var n int64
	if fi != nil {
		n = fi.Size()
	}
	return tunnel.BackupResult{OK: true, RenamedTo: renamed, Bytes: n}
}

// msUnpack decrypts the archive at path to out, a file only dir's owner
// (SQL Server) and root can read.
func (j *backupJob) msUnpack(path, out string, key []byte, dir string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	dr, err := tunnel.NewDumpReader(in, key, j.b.BackupID)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("could not write to SQL Server's backup folder %s: %w", dir, err)
	}
	defer f.Close()
	j.progress("unpacking", "", 0)
	if _, err := io.Copy(f, &countReader{r: dr, job: j}); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return ownLike(out, dir)
}

// msRollback puts a replaced database back after a failed restore.
func (j *backupJob) msRollback(ctx context.Context, a *mssqlAdmin, renamed string, res tunnel.BackupResult) tunnel.BackupResult {
	if renamed == "" {
		return res
	}
	if err := msRename(ctx, a, renamed, j.b.Name); err != nil {
		res.Error += "; and putting " + renamed + " back as " + j.b.Name + " failed too: " + err.Error() +
			" (the copy is still there under " + renamed + ")"
		res.RenamedTo = renamed
		return res
	}
	res.Error += "; " + j.b.Name + " was put back as it was"
	return res
}

// msKept drops the copy a replace kept, or puts it back over the restored
// database.
func (j *backupJob) msKept(ctx context.Context) tunnel.BackupResult {
	a, err := j.msConn(ctx)
	if err != nil {
		return j.fail("start", err)
	}
	defer a.close()
	if there, err := msExists(ctx, a, j.b.Kept); err != nil {
		return j.fail("start", err)
	} else if !there {
		return j.fail("start", fmt.Errorf("%s is not there", j.b.Kept))
	}
	if j.b.Job == tunnel.BackupJobDrop {
		if err := msDrop(ctx, a, j.b.Kept); err != nil {
			return j.fail("drop-database", err)
		}
		return tunnel.BackupResult{OK: true, RenamedTo: j.b.Kept}
	}
	if err := msDrop(ctx, a, j.b.Name); err != nil {
		return j.fail("rollback-replace", fmt.Errorf("could not drop the restored %s: %s", j.b.Name, err))
	}
	if err := msRename(ctx, a, j.b.Kept, j.b.Name); err != nil {
		return j.fail("rollback-replace", fmt.Errorf("could not rename %s back: %s", j.b.Kept, err))
	}
	return tunnel.BackupResult{OK: true, RenamedTo: j.b.Kept}
}

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Backups, on this server, through the tunnel.

	argus-control ──"backup" stream──▶ this daemon ──pg-backup helper as postgres──▶ pg_dump
	                                        │
	                                        └── encrypts, writes <dir>/<database>/<backup>.argusdump,
	                                            checks it reads back, streams it up if asked

A grant names one job. A dump grant carries the organization's public key and
nothing that could decrypt; the data key exists in this process only between
the first byte and the readability check, and the file on disk is useless to
whoever takes the disk. A restore grant carries the one key for the one
backup it names; the archive is decrypted in memory straight into
pg_restore, and the plaintext touches no disk.

Replacing a database renames the current one and keeps it; see
pgReplaceOp. A restore that fails puts it back.
*/

// BackupService is the tunnel service name.
const BackupService = tunnel.BackupService

// BackupConfig is the backup service.
type BackupConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// Dir is where backups are kept on this server; root-only.
	Dir string
	// AdminUser, Helper, Spawn and HelperEnv are as for postgres.
	AdminUser string
	Helper    string
	Spawn     SpawnMode
	HelperEnv []string
	// Admin runs pg-admin operations; nil runs the helper as AdminUser.
	Admin func(context.Context, PGAdminRequest) PGAdminResult
	// Tool runs one pg-backup request; nil runs the helper as AdminUser.
	// stderr sees the tool's progress lines.
	Tool func(ctx context.Context, req PGBackupRequest, stdin io.Reader, stdout, stderr io.Writer) error
}

const (
	backupDirMode   = 0o700
	backupProgress  = 2 * time.Second
	backupExt       = ".argusdump"
	backupStreamTTL = 30 * time.Second
)

// Backup is the backup service.
func Backup(cfg BackupConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(DefaultStateDir, "backups")
	}
	if cfg.Admin == nil {
		cfg.Admin = helperAdmin(PostgresConfig{AdminUser: cfg.AdminUser, Helper: cfg.Helper,
			Spawn: cfg.Spawn, HelperEnv: cfg.HelperEnv})
	}
	if cfg.Tool == nil {
		cfg.Tool = helperTool(cfg)
	}
	busy := &serverJobs{}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a backup job", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(BackupService) || g.Backup == nil {
			log.Warn("tunnel: refused a backup job on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for a backup"}.Error())
			return
		}
		if err := g.Backup.Validate(); err != nil {
			call.Refuse(err.Error())
			return
		}
		b := *g.Backup
		// One job per database at a time, whatever the control plane asks;
		// and a test restore, which copies a whole database, runs alone on
		// the server.
		switch b.Job {
		case tunnel.BackupJobDump, tunnel.BackupJobRestore:
			if !busy.take(b.DatabaseID) {
				call.Refuse("another backup, restore or test restore is still running on this server")
				return
			}
			defer busy.give(b.DatabaseID)
		case tunnel.BackupJobTest:
			if !busy.takeAlone(b.DatabaseID) {
				call.Refuse("another backup, restore or test restore is still running on this server; a test restore runs alone")
				return
			}
			defer busy.give(b.DatabaseID)
		}
		if err := call.Accept(b.BackupID); err != nil {
			return
		}
		j := &backupJob{cfg: cfg, log: log, g: g, b: b, stream: call.Stream, started: time.Now()}
		res := j.run(ctx)
		if !res.OK {
			log.Warn("tunnel: backup job failed", "job", b.Job, "database", b.Name, "backup", b.BackupID,
				"stage", res.Stage, "error", res.Error, "for", g.Email)
		} else {
			log.Info("tunnel: backup job done", "job", b.Job, "database", b.Name, "backup", b.BackupID,
				"bytes", res.Bytes, "for", g.Email)
		}
		_ = j.send(tunnel.FrameBackupResult, tunnel.EncodeBackupMessage(res))
		_ = call.Stream.Close()
	}
}

type backupJob struct {
	cfg     BackupConfig
	log     *slog.Logger
	g       tunnel.Grant
	b       tunnel.GrantBackup
	stream  tunnel.Stream
	started time.Time
	wmu     sync.Mutex
	// the latest progress, sent on a timer
	pmu   sync.Mutex
	stage string
	det   string
	bytes int64
}

func (j *backupJob) send(typ byte, p []byte) error {
	j.wmu.Lock()
	defer j.wmu.Unlock()
	_ = j.stream.SetDeadline(time.Now().Add(backupStreamTTL))
	err := tunnel.WriteFrame(j.stream, typ, p)
	_ = j.stream.SetDeadline(time.Time{})
	return err
}

func (j *backupJob) progress(stage, detail string, n int64) {
	j.pmu.Lock()
	if stage != "" {
		j.stage = stage
	}
	if detail != "" {
		j.det = detail
	}
	if n >= 0 {
		j.bytes = n
	}
	j.pmu.Unlock()
}

func (j *backupJob) flushProgress() {
	j.pmu.Lock()
	p := tunnel.BackupProgress{Stage: j.stage, Detail: j.det, Bytes: j.bytes,
		ElapsedMS: time.Since(j.started).Milliseconds()}
	j.pmu.Unlock()
	_ = j.send(tunnel.FrameBackupProgress, tunnel.EncodeBackupMessage(p))
}

func (j *backupJob) fail(stage string, err error) tunnel.BackupResult {
	return tunnel.BackupResult{Stage: stage, Error: err.Error()}
}

func (j *backupJob) socket() string {
	return socketDir(tunnel.GrantDB{Host: j.b.Host, Port: j.b.Port})
}

func (j *backupJob) localPath(id string) string {
	return filepath.Join(j.cfg.Dir, filepath.Base(j.b.DatabaseID), filepath.Base(id)+backupExt)
}

// run does the job, with a goroutine reading the stream for a cancel and a
// ticker sending progress.
func (j *backupJob) run(ctx context.Context) tunnel.BackupResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// What the control plane sends: a cancel, or the archive of a streamed
	// restore. The reader hands data frames to whoever is waiting for them.
	data := make(chan []byte, 8)
	go func() {
		defer close(data)
		fr := tunnel.NewFrameReader(j.stream)
		for {
			typ, p, err := fr.Next()
			if err != nil {
				cancel()
				return
			}
			switch typ {
			case tunnel.FrameBackupRequest:
				var req tunnel.BackupRequest
				if json.Unmarshal(p, &req) == nil && req.Op == "cancel" {
					cancel()
					return
				}
			case tunnel.FrameBackupData:
				c := make([]byte, len(p))
				copy(c, p)
				select {
				case data <- c:
				case <-ctx.Done():
					return
				}
			case tunnel.FrameBackupEnd:
				select {
				case data <- nil:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(backupProgress)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				j.flushProgress()
			case <-stop:
				return
			}
		}
	}()
	switch j.b.Job {
	case tunnel.BackupJobCatalog:
		return j.catalog(ctx)
	case tunnel.BackupJobPrune:
		return j.prune()
	case tunnel.BackupJobDrop, tunnel.BackupJobUndo:
		return j.kept(ctx)
	case tunnel.BackupJobDump:
		return j.dump(ctx)
	case tunnel.BackupJobRestore:
		return j.restore(ctx, data)
	case tunnel.BackupJobTest:
		return j.testRestore(ctx, data)
	case tunnel.BackupJobChecks:
		return j.runChecks(ctx)
	case tunnel.BackupJobDropTest:
		return j.dropTest(ctx, j.b.Kept)
	}
	return j.fail("start", errors.New("unknown job"))
}

func (j *backupJob) catalog(ctx context.Context) tunnel.BackupResult {
	var out, errb bytes.Buffer
	if err := j.cfg.Tool(ctx, PGBackupRequest{Op: "catalog", Socket: j.socket(), Port: j.b.Port, User: j.b.AdminUser}, nil, &out, &errb); err != nil {
		return j.fail("catalog", fmt.Errorf("%s", strings.TrimSpace(lastLine(errb.String())+" "+err.Error())))
	}
	var c tunnel.BackupCatalog
	if err := json.Unmarshal(out.Bytes(), &c); err != nil {
		return j.fail("catalog", errors.New("the helper's answer could not be read"))
	}
	if r := j.cfg.Admin(ctx, PGAdminRequest{Op: "list-databases", Database: "postgres", Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser}); r.OK {
		c.ServerVersion = r.Version
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

// kept drops the copy a replace kept, or puts it back over the restored
// database. Both are pg-admin operations that refuse any other name.
func (j *backupJob) kept(ctx context.Context) tunnel.BackupResult {
	op := "drop-database"
	if j.b.Job == tunnel.BackupJobUndo {
		op = "rollback-replace"
	}
	res := j.cfg.Admin(ctx, PGAdminRequest{Op: op, Database: "postgres", Socket: j.socket(), Port: j.b.Port,
		AdminUser: j.b.AdminUser, Target: j.b.Name, RenameTo: j.b.Kept})
	if op == "drop-database" {
		res = j.cfg.Admin(ctx, PGAdminRequest{Op: op, Database: "postgres", Socket: j.socket(), Port: j.b.Port,
			AdminUser: j.b.AdminUser, Target: j.b.Kept})
	}
	if !res.OK {
		return j.fail(op, errors.New(res.Error))
	}
	return tunnel.BackupResult{OK: true, RenamedTo: j.b.Kept}
}

func (j *backupJob) prune() tunnel.BackupResult {
	var pruned []string
	for _, id := range j.b.Prune {
		p := j.localPath(id)
		if err := os.Remove(p); err == nil {
			pruned = append(pruned, id)
		} else if !errors.Is(err, os.ErrNotExist) {
			return j.fail("prune", fmt.Errorf("could not delete %s: %w", p, err))
		} else {
			pruned = append(pruned, id)
		}
	}
	return tunnel.BackupResult{OK: true, Pruned: pruned}
}

// dump writes the archive, checks it reads back, and streams it up if asked.
func (j *backupJob) dump(ctx context.Context) tunnel.BackupResult {
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
	final := j.localPath(j.b.BackupID)
	tmp := final + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return j.fail("start", fmt.Errorf("could not write under %s: %w", dir, err))
	}
	defer os.Remove(tmp)
	defer f.Close()

	j.progress("starting", "", 0)
	hasher := sha256.New()
	counting := &countWriter{w: io.MultiWriter(f, hasher), job: j}
	dw, hdr, err := tunnel.NewDumpWriter(counting, pub, tunnel.DumpHeader{
		BackupID: j.b.BackupID, DatabaseID: j.b.DatabaseID, Organization: j.b.Organization,
		DBName: j.b.Name, Host: j.g.Host, Format: "custom", Compression: "gzip",
		StartedAt: j.started.UTC(), Reason: j.b.Reason})
	if err != nil {
		return j.fail("start", err)
	}
	// The key is needed once more, to read the file back. Unwrapping with
	// the private half is the control plane's; here it is kept in memory
	// for the length of the check and then dropped.
	key := dw.DataKey()
	dw.Forget()
	defer zero(key)

	pr, pw := io.Pipe()
	errLines := &progressLines{job: j}
	toolErr := make(chan error, 1)
	go func() {
		err := j.cfg.Tool(ctx, PGBackupRequest{Op: "dump", Socket: j.socket(), Port: j.b.Port,
			User: j.b.AdminUser, Database: j.b.Name, Compression: 6}, nil, pw, errLines)
		_ = pw.CloseWithError(err)
		toolErr <- err
	}()
	_, copyErr := io.Copy(dw, pr)
	_ = pr.Close()
	if err := <-toolErr; err != nil || copyErr != nil {
		if ctx.Err() != nil {
			return j.fail(j.stageNow(), errors.New("cancelled"))
		}
		why := strings.TrimSpace(errLines.last)
		if why == "" && err != nil {
			why = err.Error()
		}
		if why == "" {
			why = copyErr.Error()
		}
		return j.fail(j.stageNow(), fmt.Errorf("pg_dump failed%s: %s", errLines.where(), why))
	}
	if err := dw.Close(); err != nil {
		return j.fail("finishing", err)
	}
	if err := f.Sync(); err != nil {
		return j.fail("finishing", err)
	}
	_ = f.Close()
	sum := hex.EncodeToString(hasher.Sum(nil))
	size := dw.Written

	// Readable: pg_restore --list has to parse the table of contents. This
	// is what "verified" means in the console.
	j.progress("verifying", "", size)
	if err := j.verify(ctx, tmp, key); err != nil {
		return j.fail("verifying", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return j.fail("finishing", err)
	}
	// The wrap and the ephemeral key travel together; the control plane
	// keeps them as one string and splits them to unwrap.
	res := tunnel.BackupResult{OK: true, Bytes: size, SHA256: sum, WrappedKey: hdr.WrappedKey + "." + hdr.Ephemeral,
		KeyID: hdr.KeyID, LocalPath: final, Verified: true, Compression: "gzip", DumpVersion: errLines.version}
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

func (j *backupJob) stageNow() string {
	j.pmu.Lock()
	defer j.pmu.Unlock()
	if j.stage == "" {
		return "starting"
	}
	return j.stage
}

// verify runs pg_restore --list over the decrypted archive.
func (j *backupJob) verify(ctx context.Context, path string, key []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dr, err := tunnel.NewDumpReader(f, key, j.b.BackupID)
	if err != nil {
		return fmt.Errorf("the file just written does not open: %w", err)
	}
	var toc, errb bytes.Buffer
	if err := j.cfg.Tool(ctx, PGBackupRequest{Op: "toc", Socket: j.socket(), Port: j.b.Port, User: j.b.AdminUser}, dr, &toc, &errb); err != nil {
		return fmt.Errorf("pg_restore cannot read the archive back: %s", strings.TrimSpace(lastLine(errb.String())))
	}
	if !bytes.Contains(toc.Bytes(), []byte("Archive created at")) && toc.Len() == 0 {
		return errors.New("pg_restore read an empty table of contents")
	}
	return nil
}

// upload streams the file up the tunnel in BackupDataChunk pieces.
func (j *backupJob) upload(ctx context.Context, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, tunnel.BackupDataChunk)
	var sent int64
	for {
		if ctx.Err() != nil {
			return errors.New("cancelled")
		}
		n, err := f.Read(buf)
		if n > 0 {
			if werr := j.send(tunnel.FrameBackupData, buf[:n]); werr != nil {
				return fmt.Errorf("the control plane stopped taking the upload: %w", werr)
			}
			sent += int64(n)
			j.progress("uploading", fmt.Sprintf("%s of %s", humanBytes(sent), humanBytes(size)), sent)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return j.send(tunnel.FrameBackupEnd, nil)
}

// restore applies the archive named by the grant.
func (j *backupJob) restore(ctx context.Context, data <-chan []byte) tunnel.BackupResult {
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

	target := r.NewName
	renamed := ""
	if r.Mode == tunnel.RestoreReplace {
		target = j.b.Name
		j.progress("preparing", "ending sessions and renaming "+j.b.Name+" to "+r.RenameTo, -1)
		res := j.cfg.Admin(ctx, PGAdminRequest{Op: "prepare-replace", Database: "postgres", Socket: j.socket(),
			Port: j.b.Port, AdminUser: j.b.AdminUser, Target: j.b.Name, RenameTo: r.RenameTo})
		if !res.OK {
			return j.fail("preparing", errors.New(res.Error))
		}
		renamed = r.RenameTo
	}

	f, err := os.Open(path)
	if err != nil {
		return j.rollback(ctx, renamed, j.fail("restoring", err))
	}
	defer f.Close()
	dr, err := tunnel.NewDumpReader(f, key, j.b.BackupID)
	if err != nil {
		return j.rollback(ctx, renamed, j.fail("restoring", err))
	}
	j.progress("restoring", "creating "+target, 0)
	errLines := &progressLines{job: j, restore: true}
	counting := &countReader{r: dr, job: j}
	err = j.cfg.Tool(ctx, PGBackupRequest{Op: "restore", Socket: j.socket(), Port: j.b.Port, User: j.b.AdminUser, Database: target},
		counting, io.Discard, errLines)
	if ctx.Err() != nil {
		return j.rollback(ctx, renamed, j.fail("restoring", errors.New("cancelled")))
	}
	if err != nil {
		// pg_restore exits 1 when it ignored errors it was told to ignore;
		// a database that came up with warnings is a restore, a database
		// that is not there is not.
		if errLines.warnings > 0 && errLines.ignored {
			return tunnel.BackupResult{OK: true, Warnings: errLines.warnings, RenamedTo: renamed, Bytes: counting.n}
		}
		return j.rollback(ctx, renamed, j.fail("restoring",
			fmt.Errorf("pg_restore failed%s: %s", errLines.where(), lastLine(strings.TrimSpace(errLines.last)))))
	}
	return tunnel.BackupResult{OK: true, Warnings: errLines.warnings, RenamedTo: renamed, Bytes: counting.n}
}

// archive finds the ciphertext a restore or test reads: on this disk, or
// streamed into a scratch file first so its hash can be checked before a
// byte is restored. A non-nil result is the failure.
func (j *backupJob) archive(ctx context.Context, source string, data <-chan []byte) (string, func(), *tunnel.BackupResult) {
	path := j.localPath(j.b.BackupID)
	cleanup := func() {}
	fail := func(stage string, err error) (string, func(), *tunnel.BackupResult) {
		r := j.fail(stage, err)
		return "", cleanup, &r
	}
	if source == "stream" {
		if err := os.MkdirAll(filepath.Dir(path), backupDirMode); err != nil {
			return fail("start", err)
		}
		path = filepath.Join(filepath.Dir(path), "restore-"+filepath.Base(j.b.BackupID)+".part")
		p := path
		cleanup = func() { _ = os.Remove(p) }
		j.progress("receiving", "", 0)
		if err := j.receive(ctx, path, data); err != nil {
			return fail("receiving", err)
		}
	}
	j.progress("verifying", "", -1)
	if sum, err := fileSHA256(path); err != nil {
		return fail("verifying", fmt.Errorf("the backup is not on this server: %w", err))
	} else if sum != j.b.SHA256 {
		return fail("verifying", errors.New("the backup's hash is not what the control plane recorded; it was altered or replaced, so it will not be restored"))
	}
	return path, cleanup, nil
}

// rollback puts a replaced database back after a failed restore.
func (j *backupJob) rollback(ctx context.Context, renamed string, res tunnel.BackupResult) tunnel.BackupResult {
	if renamed == "" {
		return res
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	r := j.cfg.Admin(rctx, PGAdminRequest{Op: "rollback-replace", Database: "postgres", Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser, Target: j.b.Name, RenameTo: renamed})
	if !r.OK {
		res.Error += "; and putting " + renamed + " back as " + j.b.Name + " failed too: " + r.Error +
			" (the copy is still there under " + renamed + ")"
		res.RenamedTo = renamed
		return res
	}
	res.Error += "; " + j.b.Name + " was put back as it was"
	return res
}

// receive writes streamed data frames to path until the end frame.
func (j *backupJob) receive(ctx context.Context, path string, data <-chan []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var n int64
	for {
		select {
		case <-ctx.Done():
			return errors.New("cancelled")
		case p, ok := <-data:
			if !ok {
				return errors.New("the control plane stopped sending before the end of the archive")
			}
			if p == nil {
				return f.Sync()
			}
			if _, err := f.Write(p); err != nil {
				return err
			}
			n += int64(len(p))
			j.progress("receiving", humanBytes(n), n)
		}
	}
}

// progressLines is the tool's stderr: progress to the job, the last line
// kept for the error.
type progressLines struct {
	job      *backupJob
	restore  bool
	buf      bytes.Buffer
	last     string
	version  string
	table    string
	warnings int
	ignored  bool
}

func (p *progressLines) Write(b []byte) (int, error) {
	p.buf.Write(b)
	for {
		i := bytes.IndexByte(p.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := string(p.buf.Next(i + 1))
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p.last = line
		if p.restore {
			if strings.Contains(line, "errors ignored on restore") {
				p.warnings = restoreWarnings(line)
				p.ignored = true
			} else if strings.Contains(line, "processing data for table") || strings.Contains(line, "creating ") {
				if st, det, ok := dumpProgress(strings.TrimPrefix(line, "pg_restore: ")); ok {
					p.job.progress("restoring", det, -1)
					_ = st
				}
			}
			continue
		}
		if st, det, ok := dumpProgress(line); ok {
			if st == "data" {
				p.table = det
			}
			p.job.progress(st, det, -1)
		}
	}
	return len(b), nil
}

func (p *progressLines) where() string {
	if p.table != "" {
		return " while dumping " + p.table
	}
	return ""
}

type countWriter struct {
	w   io.Writer
	n   int64
	job *backupJob
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.job.progress("", "", c.n)
	return n, err
}

type countReader struct {
	r   io.Reader
	n   int64
	job *backupJob
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.job.progress("", "", c.n)
	return n, err
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

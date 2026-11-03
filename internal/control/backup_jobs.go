package control

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/storage"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Running a backup or a restore: open a "backup" stream to the database's
agent on a grant signed for exactly that job, follow its progress onto the
row, store what it streams up, and write the audit events.

	RunBackup ──▶ grant {dump, public key} ──▶ agent ──▶ 'p' progress, 'd' chunks, 'z' result
	RunRestore ─▶ grant {restore, data key, hash} ──▶ agent ──▶ ('d' chunks down, when streamed) ──▶ 'z'

The organization's backup key pair is derived from the grant key's seed, so
nothing new is stored or configured; the public half goes in every dump
grant, the private half unwraps a backup's data key for a restore grant and
nowhere else.
*/

const (
	// backupOpenWait is how long an agent gets to accept a job.
	backupOpenWait = 20 * time.Second
	// backupStaleAfter marks a running job the agent stopped reporting.
	backupStaleAfter = 10 * time.Minute
	// backupKeepReplaced is how long a replaced database is kept.
	backupKeepReplaced = 24 * time.Hour
	// backupMissedWindow: a schedule more than this late is a miss, not a
	// late run.
	backupMissedWindow = 6 * time.Hour
	backupMissAlertAt  = 3
	restoreApprovalMin = 30
)

// backupJobs tracks running jobs so they can be cancelled.
type backupJobs struct {
	mu   sync.Mutex
	stop map[string]context.CancelFunc
}

func (j *backupJobs) add(id string, cancel context.CancelFunc) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stop == nil {
		j.stop = map[string]context.CancelFunc{}
	}
	j.stop[id] = cancel
}

func (j *backupJobs) remove(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.stop, id)
}

func (j *backupJobs) cancel(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	c, ok := j.stop[id]
	if ok {
		c()
	}
	return ok
}

// backupKeyFor is the organization's backup key pair.
func (a *API) backupKeyFor(ctx context.Context) (*ecdh.PrivateKey, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, errors.New("this control plane cannot run backups through agents: set signing_secret")
	}
	return tunnel.DeriveBackupKey(hex.EncodeToString(a.Tunnels.GrantKey.Seed()), tenantIDOf(ctx))
}

// backupGrant signs a grant for one job on db's server.
func (a *API) backupGrant(ctx context.Context, email string, db Database, b tunnel.GrantBackup) (string, tunnel.Grant, error) {
	b.DatabaseID, b.Name, b.Host, b.Port, b.AdminUser = db.ID, db.Name, db.Host, db.Port, db.AdminUser
	b.Organization = tenantIDOf(ctx)
	g := tunnel.NewGrant(email, db.Hostname, "backup", "")
	g.Service = tunnel.BackupService
	g.Backup = &b
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	return tok, g, err
}

// openBackupStream opens the job on the agent.
func (a *API) openBackupStream(ctx context.Context, db Database, grant string) (tunnel.Stream, tunnel.Answer, error) {
	octx, cancel := context.WithTimeout(ctx, backupOpenWait)
	defer cancel()
	st, ans, err := a.Tunnels.OpenWith(octx, db.Hostname, tunnel.Header{Service: tunnel.BackupService, Grant: grant})
	switch {
	case errors.Is(err, ErrNoTunnel):
		return nil, ans, fmt.Errorf("the agent on %s is not connected, so nothing can run there", db.Hostname)
	case tunnel.IsRefused(err):
		return nil, ans, fmt.Errorf("the agent on %s refused the job: %s", db.Hostname, ans.Error)
	case err != nil:
		return nil, ans, fmt.Errorf("could not reach the agent on %s: %w", db.Hostname, err)
	}
	return st, ans, nil
}

// backupFrames reads a job's stream until its result. onProgress and onData
// see what arrives; a nil onData refuses data.
func backupFrames(st tunnel.Stream, onProgress func(tunnel.BackupProgress), onData func([]byte) error) (tunnel.BackupResult, error) {
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			return tunnel.BackupResult{}, fmt.Errorf("the agent went away before the job finished: %w", err)
		}
		switch typ {
		case tunnel.FrameBackupProgress:
			var pr tunnel.BackupProgress
			if json.Unmarshal(p, &pr) == nil && onProgress != nil {
				onProgress(pr)
			}
		case tunnel.FrameBackupData:
			if onData == nil {
				return tunnel.BackupResult{}, errors.New("the agent sent data this job did not ask for")
			}
			if err := onData(p); err != nil {
				return tunnel.BackupResult{}, err
			}
		case tunnel.FrameBackupEnd:
			if onData != nil {
				if err := onData(nil); err != nil {
					return tunnel.BackupResult{}, err
				}
			}
		case tunnel.FrameBackupResult:
			var res tunnel.BackupResult
			if err := json.Unmarshal(p, &res); err != nil {
				return res, errors.New("the agent's result could not be read")
			}
			return res, nil
		}
	}
}

// cancelOnDone sends a cancel to the agent when ctx ends before the job.
func cancelOnDone(ctx context.Context, st tunnel.Stream, wmu *sync.Mutex) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			wmu.Lock()
			_ = tunnel.WriteFrame(st, tunnel.FrameBackupRequest, tunnel.EncodeBackupMessage(tunnel.BackupRequest{Op: "cancel"}))
			wmu.Unlock()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// RunBackup takes a backup of db now, as by (an email or schedule:<id>).
// It returns when the job has ended; callers that do not want to wait run
// it in a goroutine. The row is created before it returns an error, so the
// console always sees why.
func (a *API) RunBackup(ctx context.Context, db Database, by, reason, destination string) (DatabaseBackup, error) {
	priv, err := a.backupKeyFor(ctx)
	if err != nil {
		return DatabaseBackup{}, err
	}
	if destination == "" {
		destination = tunnel.BackupDestServer
	}
	if destination != tunnel.BackupDestServer && a.backupStore(ctx) == nil {
		return DatabaseBackup{}, errors.New("the control plane has no storage for backups; choose the server, or configure object storage or a recordings directory")
	}
	row, err := a.store.StartBackup(ctx, db.ID, by, reason, destination)
	if err != nil {
		return row, err
	}
	a.auditDB(ctx, "db.backup_started", "notice", by, db.Hostname,
		fmt.Sprintf("Backup %s of %s started (%s)%s.", row.ID, db.Name, destination, reasonSuffix(reason)))

	// What came before sizes the free-space check.
	var minFree int64
	if last, err := a.store.Backups(ctx, db.ID, 20); err == nil {
		for _, b := range last {
			if b.State == "done" && b.Bytes > 0 {
				minFree = b.Bytes + b.Bytes/5
				break
			}
		}
	}
	if minFree == 0 {
		minFree = 1 << 30
	}
	grant, g, err := a.backupGrant(ctx, by, db, tunnel.GrantBackup{Job: tunnel.BackupJobDump, BackupID: row.ID,
		Destination: destination, PublicKey: tunnel.EncodeBackupPublicKey(priv.PublicKey()), Reason: reason,
		MinFreeBytes: minFree})
	if err != nil {
		return a.finishBackup(ctx, db, row, tunnel.BackupResult{Stage: "start", Error: "could not sign the job's grant"}, "", "")
	}
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.jobs.add(row.ID, cancel)
	defer a.jobs.remove(row.ID)

	st, ans, err := a.openBackupStream(jctx, db, grant)
	if err != nil {
		return a.finishBackup(ctx, db, row, tunnel.BackupResult{Stage: "start", Error: err.Error()}, "", "")
	}
	defer st.Close()
	_ = ans
	_ = g
	var wmu sync.Mutex
	defer cancelOnDone(jctx, st, &wmu)()

	// The upload, when there is one: written through to a temp file as
	// chunks arrive, then placed in storage under its key.
	var spool *os.File
	var spoolErr error
	if destination != tunnel.BackupDestServer {
		spool, spoolErr = os.CreateTemp("", "argus-backup-*.argusdump")
		if spoolErr != nil {
			return a.finishBackup(ctx, db, row, tunnel.BackupResult{Stage: "start", Error: "could not spool the upload: " + spoolErr.Error()}, "", "")
		}
		defer os.Remove(spool.Name())
		defer spool.Close()
	}
	var received int64
	storageKey := ""
	res, err := backupFrames(st, func(p tunnel.BackupProgress) {
		_ = a.store.BackupProgress(ctx, row.ID, p.Stage, p.Detail, p.Bytes)
	}, func(p []byte) error {
		if spool == nil {
			return errors.New("the agent sent data for a backup that stays on the server")
		}
		if p == nil {
			if err := spool.Sync(); err != nil {
				return err
			}
			key := fmt.Sprintf("backups/%s/%s.argusdump", db.ID, row.ID)
			k, err := a.backupStore(ctx).PutKey(ctx, spool.Name(), key)
			if err != nil {
				return fmt.Errorf("storing the backup failed: %w", err)
			}
			storageKey = k
			return nil
		}
		n, err := spool.Write(p)
		received += int64(n)
		return err
	})
	if err != nil {
		res = tunnel.BackupResult{Stage: "uploading", Error: err.Error()}
		if jctx.Err() != nil {
			res.Error = "cancelled"
		}
	}
	if res.OK && destination != tunnel.BackupDestServer {
		switch {
		case storageKey == "":
			res = tunnel.BackupResult{Stage: "uploading", Error: "the agent reported success but no upload arrived"}
		case received != res.Bytes:
			res = tunnel.BackupResult{Stage: "uploading", Error: fmt.Sprintf("the upload is %d bytes, the agent wrote %d", received, res.Bytes)}
		}
	}
	if res.OK && storageKey != "" && res.SHA256 != "" {
		if sum, err := fileSHA256(spool.Name()); err != nil || sum != res.SHA256 {
			res = tunnel.BackupResult{Stage: "uploading", Error: "the upload does not hash to what the agent wrote"}
			_ = a.backupStore(ctx).Delete(ctx, storageKey)
			storageKey = ""
		}
	}
	return a.finishBackup(ctx, db, row, res, storageKey, res.ServerVersion)
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// finishBackup writes the outcome, the audit event, and applies retention
// after a success.
func (a *API) finishBackup(ctx context.Context, db Database, row DatabaseBackup, res tunnel.BackupResult, storageKey, serverVersion string) (DatabaseBackup, error) {
	out, err := a.store.FinishBackup(ctx, row.ID, res, storageKey, serverVersion)
	if err != nil {
		a.log.Error("backup: could not record the outcome", "backup", row.ID, "error", err)
		return row, err
	}
	if res.OK {
		where := map[string]string{tunnel.BackupDestServer: "on the server", tunnel.BackupDestControl: "in the control plane's storage",
			tunnel.BackupDestBoth: "on the server and in the control plane's storage"}[row.Destination]
		a.auditDB(ctx, "db.backup_finished", "notice", row.StartedBy, db.Hostname,
			fmt.Sprintf("Backup %s of %s finished: %d bytes, sha256 %s, %s, encrypted to key %s, readable: %v.",
				row.ID, db.Name, res.Bytes, res.SHA256, where, res.KeyID, res.Verified))
		a.applyRetention(ctx, db)
		return out, nil
	}
	a.auditDB(ctx, "db.backup_failed", "warn", row.StartedBy, db.Hostname,
		fmt.Sprintf("Backup %s of %s failed while %s: %s", row.ID, db.Name, res.Stage, res.Error))
	return out, errors.New(res.Error)
}

// applyRetention deletes what the schedule no longer keeps, each deletion
// its own audit event.
func (a *API) applyRetention(ctx context.Context, db Database) {
	sched, err := a.store.BackupSchedule(ctx, db.ID)
	if err != nil || !sched.Exists {
		return
	}
	old, err := a.store.RetentionCandidates(ctx, db.ID, sched.KeepLast, sched.KeepDays)
	if err != nil {
		a.log.Error("backup: retention query failed", "database", db.ID, "error", err)
		return
	}
	for _, b := range old {
		if err := a.deleteBackup(ctx, db, b, "schedule:"+db.ID, "retention"); err != nil {
			a.log.Warn("backup: retention could not delete", "backup", b.ID, "error", err)
		}
	}
}

// deleteBackup removes a backup's files wherever they are and marks the row.
func (a *API) deleteBackup(ctx context.Context, db Database, b DatabaseBackup, by, why string) error {
	if b.State == "running" {
		return ErrBackupRunning
	}
	if b.storageKey != "" {
		if err := a.backupStore(ctx).Delete(ctx, b.storageKey); err != nil {
			return fmt.Errorf("could not delete the stored copy: %w", err)
		}
	}
	if b.localPath != "" {
		grant, _, err := a.backupGrant(ctx, by, db, tunnel.GrantBackup{Job: tunnel.BackupJobPrune, Prune: []string{b.ID}})
		if err != nil {
			return err
		}
		st, _, err := a.openBackupStream(ctx, db, grant)
		if err != nil {
			return fmt.Errorf("the server's copy could not be deleted: %w", err)
		}
		res, err := backupFrames(st, nil, nil)
		_ = st.Close()
		if err != nil {
			return err
		}
		if !res.OK {
			return errors.New(res.Error)
		}
	}
	if err := a.store.MarkBackupDeleted(ctx, b.ID); err != nil {
		return err
	}
	a.auditDB(ctx, "db.backup_deleted", "notice", by, db.Hostname,
		fmt.Sprintf("Backup %s of %s (%d bytes, taken %s) deleted: %s.", b.ID, db.Name, b.Bytes,
			b.StartedAt.UTC().Format(time.RFC3339), why))
	return nil
}

// Catalog asks the server what it can do.
func (a *API) backupCatalog(ctx context.Context, email string, db Database) (*tunnel.BackupCatalog, error) {
	grant, _, err := a.backupGrant(ctx, email, db, tunnel.GrantBackup{Job: tunnel.BackupJobCatalog})
	if err != nil {
		return nil, err
	}
	st, _, err := a.openBackupStream(ctx, db, grant)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	res, err := backupFrames(st, nil, nil)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, errors.New(res.Error)
	}
	return res.Catalog, nil
}

// RunRestore applies backup b to db as restore r describes. The restore row
// exists already (pending or running); this moves it to running and on.
func (a *API) RunRestore(ctx context.Context, db Database, b DatabaseBackup, r DatabaseRestore, approvalID string) (DatabaseRestore, error) {
	priv, err := a.backupKeyFor(ctx)
	if err != nil {
		return r, err
	}
	fail := func(stage, why string) (DatabaseRestore, error) {
		out, _ := a.store.FinishRestore(ctx, r.ID, tunnel.BackupResult{Stage: stage, Error: why}, 0, "")
		a.auditDB(ctx, "db.restore_failed", "warn", r.RequestedBy, db.Hostname,
			fmt.Sprintf("Restore %s of %s from backup %s failed while %s: %s", r.ID, db.Name, b.ID, stage, why))
		return out, errors.New(why)
	}
	if b.State != "done" || (b.storageKey == "" && b.localPath == "") {
		return fail("start", "this backup is not available to restore from")
	}
	_ = a.store.SetRestoreState(ctx, r.ID, "running", "starting", "")
	a.auditDB(ctx, "db.restore_started", "notice", r.RequestedBy, db.Hostname,
		fmt.Sprintf("Restore %s of %s from backup %s (taken %s) started: %s%s.", r.ID, db.Name, b.ID,
			b.StartedAt.UTC().Format(time.RFC3339), describeRestore(r, db), reasonSuffix(r.Reason)))

	// Unwrap the data key for this one grant.
	hdr := tunnel.DumpHeader{KeyID: b.KeyID, WrappedKey: b.wrappedKey}
	if i := strings.Index(b.wrappedKey, "."); i > 0 {
		hdr.WrappedKey, hdr.Ephemeral = b.wrappedKey[:i], b.wrappedKey[i+1:]
	}
	dataKey, err := tunnel.UnwrapBackupKey(priv, hdr)
	if err != nil {
		return fail("start", "the backup's key does not open with this organization's key: "+err.Error())
	}
	source := "local"
	if b.localPath == "" {
		source = "stream"
	}
	gr := tunnel.GrantRestore{Mode: r.Mode, NewName: r.NewName, Source: source, ApprovalID: approvalID}
	if r.Mode == tunnel.RestoreReplace {
		gr.RenameTo = fmt.Sprintf("%s%s%s", db.Name, tunnel.KeptMarker, time.Now().UTC().Format("20060102t1504"))
		if len(gr.RenameTo) > 63 {
			gr.RenameTo = gr.RenameTo[len(gr.RenameTo)-63:]
			gr.RenameTo = strings.TrimLeft(gr.RenameTo, "0123456789")
		}
	}
	grant, _, err := a.backupGrant(ctx, r.RequestedBy, db, tunnel.GrantBackup{Job: tunnel.BackupJobRestore, BackupID: b.ID,
		DataKey: base64.StdEncoding.EncodeToString(dataKey), SHA256: b.SHA256, Restore: &gr, Reason: r.Reason})
	for i := range dataKey {
		dataKey[i] = 0
	}
	if err != nil {
		return fail("start", "could not sign the job's grant")
	}
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.jobs.add(r.ID, cancel)
	defer a.jobs.remove(r.ID)
	st, _, err := a.openBackupStream(jctx, db, grant)
	if err != nil {
		return fail("start", err.Error())
	}
	defer st.Close()
	var wmu sync.Mutex
	defer cancelOnDone(jctx, st, &wmu)()

	// A backup held only here streams down first.
	if source == "stream" {
		if err := a.streamDown(jctx, st, &wmu, b, func(n int64) {
			_ = a.store.SetRestoreState(ctx, r.ID, "running", "sending", fmt.Sprintf("%d bytes", n))
		}); err != nil {
			return fail("sending", err.Error())
		}
	}
	res, err := backupFrames(st, func(p tunnel.BackupProgress) {
		_ = a.store.SetRestoreState(ctx, r.ID, "running", p.Stage, p.Detail)
	}, nil)
	if err != nil {
		if jctx.Err() != nil {
			return fail("restoring", "cancelled")
		}
		return fail("restoring", err.Error())
	}
	if !res.OK {
		return fail(res.Stage, res.Error)
	}
	created := ""
	if r.Mode == tunnel.RestoreNew {
		nd := Database{AssetID: db.AssetID, Engine: db.Engine, Host: db.Host, Port: db.Port, Name: r.NewName,
			AdminUser: db.AdminUser, Label: db.Label, Policy: db.Policy, PoolerHost: db.PoolerHost, PoolerPort: db.PoolerPort}
		if made, err := a.store.CreateDatabase(ctx, nd, r.RequestedBy); err == nil {
			created = made.ID
		} else {
			a.log.Warn("restore: the new database was restored but could not be added to Argus", "name", r.NewName, "error", err)
		}
	}
	out, err := a.store.FinishRestore(ctx, r.ID, res, backupKeepReplaced, created)
	if err != nil {
		return out, err
	}
	what := fmt.Sprintf("Restore %s of %s from backup %s finished: %s", r.ID, db.Name, b.ID, describeRestore(r, db))
	if res.RenamedTo != "" {
		what += fmt.Sprintf("; the previous database is kept as %s until %s", res.RenamedTo,
			out.KeepUntil.UTC().Format(time.RFC3339))
	}
	if res.Warnings > 0 {
		what += fmt.Sprintf("; pg_restore ignored %d errors", res.Warnings)
	}
	a.auditDB(ctx, "db.restore_finished", "notice", r.RequestedBy, db.Hostname, what+".")
	return out, nil
}

func describeRestore(r DatabaseRestore, db Database) string {
	if r.Mode == tunnel.RestoreNew {
		return "into a new database " + r.NewName + " on " + db.Hostname
	}
	return "replacing " + db.Name + " on " + db.Hostname
}

// streamDown sends a stored backup to the agent in chunks.
func (a *API) streamDown(ctx context.Context, st tunnel.Stream, wmu *sync.Mutex, b DatabaseBackup, onSent func(int64)) error {
	rc, err := a.backupStore(ctx).Get(ctx, b.storageKey)
	if err != nil {
		return fmt.Errorf("the stored backup could not be read: %w", err)
	}
	defer rc.Close()
	buf := make([]byte, tunnel.BackupDataChunk)
	var sent int64
	last := time.Now()
	for {
		if ctx.Err() != nil {
			return errors.New("cancelled")
		}
		n, err := rc.Read(buf)
		if n > 0 {
			wmu.Lock()
			werr := tunnel.WriteFrame(st, tunnel.FrameBackupData, buf[:n])
			wmu.Unlock()
			if werr != nil {
				return fmt.Errorf("the agent stopped taking the backup: %w", werr)
			}
			sent += int64(n)
			if time.Since(last) > 2*time.Second {
				onSent(sent)
				last = time.Now()
			}
		}
		if errors.Is(err, os.ErrClosed) || err != nil && err.Error() == "EOF" {
			break
		}
		if err != nil {
			return err
		}
	}
	wmu.Lock()
	defer wmu.Unlock()
	return tunnel.WriteFrame(st, tunnel.FrameBackupEnd, nil)
}

// keptCopy drops or puts back the database a replace kept.
func (a *API) keptCopy(ctx context.Context, by string, db Database, r DatabaseRestore, undo bool) error {
	job := tunnel.BackupJobDrop
	if undo {
		job = tunnel.BackupJobUndo
	}
	grant, _, err := a.backupGrant(ctx, by, db, tunnel.GrantBackup{Job: job, Kept: r.RenamedTo})
	if err != nil {
		return err
	}
	st, _, err := a.openBackupStream(ctx, db, grant)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := backupFrames(st, nil, nil)
	if err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	if err := a.store.MarkKeptDropped(ctx, r.ID, undo); err != nil {
		return err
	}
	if undo {
		a.auditDB(ctx, "db.restore_undone", "notice", by, db.Hostname,
			fmt.Sprintf("Restore %s of %s undone: %s is back in place of the restored database.", r.ID, db.Name, r.RenamedTo))
	} else {
		a.auditDB(ctx, "db.restore_kept_dropped", "notice", by, db.Hostname,
			fmt.Sprintf("The copy %s kept from restore %s of %s was dropped after its keep window.", r.RenamedTo, r.ID, db.Name))
	}
	return nil
}

// BackupSweep is what the control plane's sweep runs per organization:
// due schedules, stale jobs, and kept copies whose day is up.
func (a *API) BackupSweep(ctx context.Context) {
	now := time.Now()
	if stale, err := a.store.FailStaleBackups(ctx, backupStaleAfter); err == nil {
		for _, b := range stale {
			a.log.Warn("backup: the agent stopped reporting", "backup", b.ID, "database", b.DatabaseID)
		}
	}
	due, err := a.store.DueBackupSchedules(ctx, now)
	if err != nil {
		a.log.Error("backup: cannot list schedules", "error", err)
		return
	}
	for _, s := range due {
		db, err := a.store.Database(ctx, s.DatabaseID)
		if err != nil || db.Engine != tunnel.EnginePostgres {
			continue
		}
		by := "schedule:" + db.ID
		late := now.Sub(*s.NextRunAt)
		switch {
		case late > backupMissedWindow || !db.TunnelConnected:
			if late > backupMissedWindow || late > backupMissedWindow/2 {
				// Past the window: a miss, counted and audited.
				misses, _ := a.store.AdvanceBackupSchedule(ctx, s, true)
				why := "the agent on " + db.Hostname + " was not connected at the scheduled time"
				_ = a.store.RecordBackupOutcome(ctx, db.ID, by, "missed", why)
				a.auditDB(ctx, "db.backup_missed", "warn", by, db.Hostname,
					fmt.Sprintf("The scheduled backup of %s was missed: %s (%d in a row).", db.Name, why, misses))
				if misses == backupMissAlertAt {
					a.alertSecurity(ctx, "", "Backups of "+db.Name+" on "+db.Hostname+" are being missed",
						fmt.Sprintf("%d scheduled backups in a row did not run: %s.", misses, why))
				}
				if misses == backupMissAlertAt && a.Alerts != nil {
					a.Alerts.SecurityEvent("Backups of "+db.Name+" on "+db.Hostname+" are being missed",
						fmt.Sprintf("%d scheduled backups in a row did not run: %s.", misses, why))
				}
			}
			// Else: wait for the agent, within the window.
		default:
			if _, err := a.store.AdvanceBackupSchedule(ctx, s, false); err != nil {
				continue
			}
			go func(db Database, s BackupSchedule) {
				if _, err := a.RunBackup(context.WithoutCancel(ctx), db, by, "", s.Destination); err != nil {
					if errors.Is(err, ErrBackupRunning) {
						_ = a.store.RecordBackupOutcome(ctx, db.ID, by, "skipped", "the previous run was still going")
					}
				}
			}(db, s)
		}
	}
	kept, err := a.store.KeptCopiesDue(ctx)
	if err == nil {
		for _, r := range kept {
			db, err := a.store.Database(ctx, r.DatabaseID)
			if err != nil || !db.TunnelConnected {
				continue
			}
			if err := a.keptCopy(ctx, "system", db, r, false); err != nil {
				a.log.Warn("backup: could not drop a kept copy", "restore", r.ID, "kept", r.RenamedTo, "error", err)
			}
		}
	}
}

// backupStore is where control-plane copies go: the recordings store,
// scoped to the organization. Nil when the install has none.
func (a *API) backupStore(ctx context.Context) *storage.Scoped {
	if a.recordings == nil {
		return nil
	}
	return storage.ForTenant(a.recordings, tenantIDOf(ctx))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

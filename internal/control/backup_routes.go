package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Routes for backups and restores. Reading is open to anyone who may open the
database; backing up is Owner and above; the schedule is an admin's; a
restore into a new database is Owner's; replacing a database, deleting a
backup and downloading one are DBA's, and replacing needs an approver too.
*/

// dbLevelOf is the level email holds on db, "" when none; admins are dba.
func (a *API) dbLevelOf(ctx context.Context, email, role, dbID string) (string, error) {
	level, _, err := a.databaseLevelFor(ctx, email, role, dbID, "")
	return level, err
}

// withDatabase loads the database and the caller's level, refusing below
// min. The handler gets both.
func (a *API) withDatabase(min string, next func(w http.ResponseWriter, r *http.Request, email string, db Database, level string)) http.HandlerFunc {
	return a.withPostgres("backups", min, next)
}

// withPostgres is withDatabase for a feature named what, which only a
// Postgres database has.
func (a *API) withPostgres(what, min string, next func(w http.ResponseWriter, r *http.Request, email string, db Database, level string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		db, err := a.store.Database(r.Context(), r.PathValue("id"))
		if err != nil {
			a.dbErr(w, "database", err)
			return
		}
		if db.Engine != tunnel.EnginePostgres {
			writeErr(w, http.StatusBadRequest, what+" are for Postgres databases; "+db.Name+" is "+engineName(db.Engine))
			return
		}
		level := tunnel.LevelDBA
		if sess.Role == "auditor" {
			level = tunnel.LevelRead
		} else {
			level, err = a.dbLevelOf(r.Context(), sess.Email, sess.Role, db.ID)
			if err != nil {
				a.fail(w, "database access", err)
				return
			}
		}
		if level == "" {
			writeErr(w, http.StatusForbidden, "you are not assigned this database")
			return
		}
		if tunnel.LevelRank(level) < tunnel.LevelRank(min) {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("this needs %s access to %s; you have %s", min, db.Name, level))
			return
		}
		next(w, r, sess.Email, db, level)
	}
}

// GET /api/v1/databases/{id}/backups
func (a *API) getBackups(w http.ResponseWriter, r *http.Request, _ string, db Database, level string) {
	list, err := a.store.Backups(r.Context(), db.ID, 100)
	if err != nil {
		a.fail(w, "backups", err)
		return
	}
	sched, err := a.store.BackupSchedule(r.Context(), db.ID)
	if err != nil {
		a.fail(w, "backup schedule", err)
		return
	}
	restores, err := a.store.Restores(r.Context(), db.ID)
	if err != nil {
		a.fail(w, "restores", err)
		return
	}
	var running *DatabaseBackup
	for i := range list {
		if list[i].State == "running" {
			running = &list[i]
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backups": list, "running": running, "schedule": sched, "restores": restores, "level": level,
		"storage":     a.backupStore(r.Context()) != nil,
		"staleAfterS": 60,
	})
}

// GET /api/v1/databases/{id}/backups/catalog: what the server can do.
func (a *API) getBackupCatalog(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	c, err := a.backupCatalog(r.Context(), email, db)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	priv, kerr := a.backupKeyFor(r.Context())
	keyID := ""
	if kerr == nil {
		keyID = tunnel.BackupKeyID(priv.PublicKey())
	}
	writeJSON(w, http.StatusOK, map[string]any{"catalog": c, "keyId": keyID})
}

// POST /api/v1/databases/{id}/backups: back up now.
func (a *API) postBackup(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	var in struct {
		Reason      string `json:"reason"`
		Destination string `json:"destination"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if len(in.Reason) > 200 {
		writeErr(w, http.StatusBadRequest, "keep the reason under 200 characters")
		return
	}
	if in.Destination == "" {
		if s, err := a.store.BackupSchedule(r.Context(), db.ID); err == nil && s.Exists {
			in.Destination = s.Destination
		} else {
			in.Destination = tunnel.BackupDestServer
		}
	}
	if !db.TunnelConnected {
		writeErr(w, http.StatusConflict, "the agent on "+db.Hostname+" is not connected, so nothing can run there")
		return
	}
	if list, err := a.store.Backups(r.Context(), db.ID, 1); err == nil && len(list) > 0 && list[0].State == "running" {
		writeErr(w, http.StatusConflict, ErrBackupRunning.Error())
		return
	}
	go func() {
		ctx := context.WithoutCancel(r.Context())
		if _, err := a.RunBackup(ctx, db, email, in.Reason, in.Destination); err != nil {
			a.log.Warn("backup failed", "database", db.ID, "by", email, "error", err)
		}
	}()
	// Give the run a moment to create its row so the console's first poll
	// sees it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if list, err := a.store.Backups(r.Context(), db.ID, 1); err == nil && len(list) > 0 && list[0].State == "running" {
			writeJSON(w, http.StatusAccepted, list[0])
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"state": "starting"})
}

// POST /api/v1/databases/{id}/backups/{backup}/cancel
func (a *API) postBackupCancel(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	b, err := a.store.Backup(r.Context(), r.PathValue("backup"))
	if err != nil || b.DatabaseID != db.ID {
		writeErr(w, http.StatusNotFound, "no such backup")
		return
	}
	if !a.jobs.cancel(b.ID) {
		writeErr(w, http.StatusConflict, "this backup is not running on this control plane")
		return
	}
	a.auditDB(r.Context(), "db.backup_cancelled", "notice", email, db.Hostname, "Backup "+b.ID+" of "+db.Name+" cancelled.")
	writeJSON(w, http.StatusOK, map[string]string{"state": "cancelling"})
}

// DELETE /api/v1/databases/{id}/backups/{backup}
func (a *API) deleteBackupRoute(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	b, err := a.store.Backup(r.Context(), r.PathValue("backup"))
	if err != nil || b.DatabaseID != db.ID {
		writeErr(w, http.StatusNotFound, "no such backup")
		return
	}
	if b.Pinned {
		writeErr(w, http.StatusConflict, "this backup is pinned; unpin it first")
		return
	}
	if err := a.deleteBackup(r.Context(), db, b, email, "deleted by "+email); err != nil {
		if errors.Is(err, ErrBackupRunning) {
			writeErr(w, http.StatusConflict, "this backup is still running")
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/v1/databases/{id}/backups/{backup}/pin {pinned}
func (a *API) postBackupPin(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	var in struct {
		Pinned bool `json:"pinned"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, err := a.store.Backup(r.Context(), r.PathValue("backup"))
	if err != nil || b.DatabaseID != db.ID {
		writeErr(w, http.StatusNotFound, "no such backup")
		return
	}
	out, err := a.store.PinBackup(r.Context(), b.ID, in.Pinned)
	if err != nil {
		writeErr(w, http.StatusConflict, "only a finished backup can be pinned")
		return
	}
	verb := "unpinned"
	if in.Pinned {
		verb = "pinned"
	}
	a.auditDB(r.Context(), "db.backup_"+verb, "notice", email, db.Hostname,
		fmt.Sprintf("Backup %s of %s %s; retention %s it.", b.ID, db.Name, verb, map[bool]string{true: "skips", false: "may delete"}[in.Pinned]))
	writeJSON(w, http.StatusOK, out)
}

// GET /api/v1/databases/{id}/backups/{backup}/download: the encrypted file.
func (a *API) getBackupDownload(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	b, err := a.store.Backup(r.Context(), r.PathValue("backup"))
	if err != nil || b.DatabaseID != db.ID {
		writeErr(w, http.StatusNotFound, "no such backup")
		return
	}
	if b.storageKey == "" {
		writeErr(w, http.StatusConflict, "this backup is only on the server; a download needs a copy in the control plane's storage")
		return
	}
	rc, err := a.backupStore(r.Context()).Get(r.Context(), b.storageKey)
	if err != nil {
		a.fail(w, "backup download", err)
		return
	}
	defer rc.Close()
	a.auditDB(r.Context(), "db.backup_downloaded", "notice", email, db.Hostname,
		fmt.Sprintf("Backup %s of %s (%d bytes, encrypted) downloaded.", b.ID, db.Name, b.Bytes))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.argusdump"`, db.Name, b.StartedAt.UTC().Format("20060102T150405Z")))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, rc)
}

// GET /api/v1/databases/{id}/backup-schedule
func (a *API) getBackupSchedule(w http.ResponseWriter, r *http.Request, _ string, db Database, _ string) {
	s, err := a.store.BackupSchedule(r.Context(), db.ID)
	if err != nil {
		a.fail(w, "backup schedule", err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// PUT /api/v1/databases/{id}/backup-schedule (admin)
func (a *API) putBackupSchedule(w http.ResponseWriter, r *http.Request, actor string) {
	db, err := a.store.Database(r.Context(), r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	if db.Engine != tunnel.EnginePostgres {
		writeErr(w, http.StatusBadRequest, "backups are for Postgres databases; "+db.Name+" is "+engineName(db.Engine))
		return
	}
	var in struct {
		Enabled     bool   `json:"enabled"`
		Every       string `json:"every"`
		AtHour      int    `json:"atHour"`
		AtMinute    int    `json:"atMinute"`
		Weekday     int    `json:"weekday"`
		Destination string `json:"destination"`
		KeepLast    int    `json:"keepLast"`
		KeepDays    int    `json:"keepDays"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Destination != tunnel.BackupDestServer && a.backupStore(r.Context()) == nil {
		writeErr(w, http.StatusBadRequest, "the control plane has no storage for backups; choose the server, or configure object storage or a recordings directory")
		return
	}
	before, _ := a.store.BackupSchedule(r.Context(), db.ID)
	out, err := a.store.SetBackupSchedule(r.Context(), BackupSchedule{DatabaseID: db.ID, Enabled: in.Enabled, Every: in.Every,
		AtHour: in.AtHour, AtMinute: in.AtMinute, Weekday: in.Weekday, Destination: in.Destination,
		KeepLast: in.KeepLast, KeepDays: in.KeepDays}, actor)
	if err != nil {
		a.dbErr(w, "backup schedule", err)
		return
	}
	a.auditDB(r.Context(), "db.backup_schedule_changed", "notice", actor, db.Hostname,
		fmt.Sprintf("Backup schedule of %s changed: was %s (keep %d, %s), now %s (keep %d%s, %s).", db.Name,
			before.Describe(), before.KeepLast, before.Destination, out.Describe(), out.KeepLast,
			map[bool]string{true: fmt.Sprintf(" and %d days", out.KeepDays), false: ""}[out.KeepDays > 0], out.Destination))
	writeJSON(w, http.StatusOK, out)
}

// POST /api/v1/databases/{id}/backups/{backup}/restore
func (a *API) postRestore(w http.ResponseWriter, r *http.Request, email string, db Database, level string) {
	b, err := a.store.Backup(r.Context(), r.PathValue("backup"))
	if err != nil || b.DatabaseID != db.ID {
		writeErr(w, http.StatusNotFound, "no such backup")
		return
	}
	var in struct {
		Mode    string `json:"mode"`
		NewName string `json:"newName"`
		Reason  string `json:"reason"`
		// Confirm is the database's name, typed, for a replace.
		Confirm string `json:"confirm"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if b.State != "done" {
		writeErr(w, http.StatusConflict, "only a finished backup can be restored")
		return
	}
	if !db.TunnelConnected {
		writeErr(w, http.StatusConflict, "the agent on "+db.Hostname+" is not connected, so nothing can run there")
		return
	}
	switch in.Mode {
	case tunnel.RestoreNew:
		if tunnel.LevelRank(level) < tunnel.LevelRank(tunnel.LevelOwner) {
			writeErr(w, http.StatusForbidden, "restoring into a new database needs Owner access to "+db.Name)
			return
		}
		in.NewName = strings.TrimSpace(in.NewName)
		if !tunnel.ValidDBName.MatchString(in.NewName) {
			writeErr(w, http.StatusBadRequest, "the new database's name must be lower-case letters, digits and underscores, up to 63")
			return
		}
		if in.NewName == db.Name {
			writeErr(w, http.StatusBadRequest, "that is the existing database's name; pick another, or replace it")
			return
		}
		row, err := a.store.CreateRestore(r.Context(), DatabaseRestore{BackupID: b.ID, DatabaseID: db.ID, Mode: in.Mode,
			NewName: in.NewName, RequestedBy: email, Reason: in.Reason, State: "running"})
		if err != nil {
			a.dbErr(w, "restore", err)
			return
		}
		go func() {
			if _, err := a.RunRestore(context.WithoutCancel(r.Context()), db, b, row, ""); err != nil {
				a.log.Warn("restore failed", "restore", row.ID, "error", err)
			}
		}()
		writeJSON(w, http.StatusAccepted, row)
	case tunnel.RestoreReplace:
		if level != tunnel.LevelDBA {
			writeErr(w, http.StatusForbidden, "replacing "+db.Name+" needs DBA access")
			return
		}
		if in.Confirm != db.Name {
			writeErr(w, http.StatusBadRequest, "type the database's name, "+db.Name+", to confirm")
			return
		}
		if len(strings.TrimSpace(in.Reason)) < 10 {
			writeErr(w, http.StatusBadRequest, "say why, in at least 10 characters; approvers see it")
			return
		}
		row, err := a.store.CreateRestore(r.Context(), DatabaseRestore{BackupID: b.ID, DatabaseID: db.ID, Mode: in.Mode,
			RequestedBy: email, Reason: in.Reason, State: "pending"})
		if err != nil {
			a.dbErr(w, "restore", err)
			return
		}
		req, err := a.store.CreateRequest(r.Context(), AccessRequest{RequesterEmail: email, AssetHostnames: []string{db.Hostname},
			Principal: "db:restore", Justification: "Replace " + db.Name + " from the backup of " +
				b.StartedAt.Local().Format("Jan 2, 15:04") + ": " + in.Reason, DurationMinutes: restoreApprovalMin})
		if err != nil {
			a.fail(w, "restore request", err)
			return
		}
		if err := a.store.LinkRestoreRequest(r.Context(), row.ID, req.ID); err != nil {
			a.fail(w, "restore request", err)
			return
		}
		a.auditDB(r.Context(), "db.restore_requested", "notice", email, db.Hostname,
			fmt.Sprintf("Asked to replace %s from backup %s (taken %s): %s. Waiting for an approver (request %s).",
				db.Name, b.ID, b.StartedAt.UTC().Format(time.RFC3339), in.Reason, req.ID))
		a.alertRequestWaiting(r.Context(), req)
		row, _ = a.store.Restore(r.Context(), row.ID)
		writeJSON(w, http.StatusAccepted, row)
	default:
		writeErr(w, http.StatusBadRequest, "mode must be new or replace")
	}
}

// POST /api/v1/databases/{id}/restores/{restore}/undo: put the kept copy back.
func (a *API) postRestoreUndo(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	rs, err := a.store.Restore(r.Context(), r.PathValue("restore"))
	if err != nil || rs.DatabaseID != db.ID {
		writeErr(w, http.StatusNotFound, "no such restore")
		return
	}
	if rs.State != "done" || rs.RenamedTo == "" || rs.KeptDropped {
		writeErr(w, http.StatusConflict, "there is no kept copy to put back")
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := decode(r, &in); err != nil || in.Confirm != db.Name {
		writeErr(w, http.StatusBadRequest, "type the database's name, "+db.Name+", to confirm")
		return
	}
	if err := a.keptCopy(r.Context(), email, db, rs, true); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out, _ := a.store.Restore(r.Context(), rs.ID)
	writeJSON(w, http.StatusOK, out)
}

// POST /api/v1/databases/{id}/restores/{restore}/cancel
func (a *API) postRestoreCancel(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	rs, err := a.store.Restore(r.Context(), r.PathValue("restore"))
	if err != nil || rs.DatabaseID != db.ID {
		writeErr(w, http.StatusNotFound, "no such restore")
		return
	}
	if !a.jobs.cancel(rs.ID) {
		writeErr(w, http.StatusConflict, "this restore is not running on this control plane")
		return
	}
	a.auditDB(r.Context(), "db.restore_cancelled", "notice", email, db.Hostname, "Restore "+rs.ID+" of "+db.Name+" cancelled.")
	writeJSON(w, http.StatusOK, map[string]string{"state": "cancelling"})
}

// restoreApproved runs a replace once its request is approved. Called from
// the decision route.
func (a *API) restoreApproved(ctx context.Context, requestID, decidedBy string) {
	rs, err := a.store.RestoreByRequest(ctx, requestID)
	if err != nil {
		return
	}
	if rs.State != "pending" {
		return
	}
	db, err := a.store.Database(ctx, rs.DatabaseID)
	if err != nil {
		return
	}
	b, err := a.store.Backup(ctx, rs.BackupID)
	if err != nil {
		return
	}
	a.auditDB(ctx, "db.restore_approved", "notice", decidedBy, db.Hostname,
		fmt.Sprintf("Approved replacing %s from backup %s, asked by %s.", db.Name, b.ID, rs.RequestedBy))
	go func() {
		if _, err := a.RunRestore(context.WithoutCancel(ctx), db, b, rs, requestID); err != nil {
			a.log.Warn("approved restore failed", "restore", rs.ID, "error", err)
		}
	}()
}

// restoreDenied records the denial on the restore row.
func (a *API) restoreDenied(ctx context.Context, requestID, decidedBy, note string) {
	rs, err := a.store.RestoreByRequest(ctx, requestID)
	if err != nil || rs.State != "pending" {
		return
	}
	_, _ = a.store.FinishRestore(ctx, rs.ID, tunnel.BackupResult{Stage: "approval", Error: "denied by " + decidedBy + ": " + note}, 0, "")
	_ = a.store.SetRestoreState(ctx, rs.ID, "denied", "approval", "")
	a.auditDB(ctx, "db.restore_denied", "notice", decidedBy, rs.Hostname,
		fmt.Sprintf("Denied replacing %s from backup %s, asked by %s: %s", rs.DBName, rs.BackupID, rs.RequestedBy, note))
}

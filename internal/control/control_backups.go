package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/controlbackup"
)

/*
Settings › Control plane › Backups: the control plane's own backups.

The backups themselves are argus-backup.service (`argus-control backup run`),
on its timer and before every update from the console. This side only reads
what those runs left, sets how many dumps to keep, and asks for one now. The
control plane cannot start a unit -- it runs as argus with no privileges --
so "Back up now" writes a request into its own state directory and
argus-backup-now.path starts the backup when that file changes. The files
both sides use are described in internal/controlbackup.
*/

// backupRunLate is how long a request may wait before the console says the
// watcher is not running.
const backupRunLate = 2 * time.Minute

// backupRunMax is how long a run may say it is running before it counts as
// gone: the unit's own TimeoutStartSec.
const backupRunMax = 6 * time.Hour

// ControlBackups is this control plane's view of its own backups.
type ControlBackups struct {
	// StateDir is controlbackup's directory under the control plane's state:
	// settings and requests, written here, read by the backup.
	StateDir string
	// Root is where the backups are; empty is found from backup.env.
	Root string
	// EnvFile is the backup unit's own overrides; empty is /etc/argus/backup.env.
	EnvFile string
	// UnitDirs are where systemd units are installed; nil is the usual ones.
	UnitDirs []string
}

func (b *ControlBackups) unitDirs() []string {
	if b.UnitDirs != nil {
		return b.UnitDirs
	}
	return []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"}
}

func (b *ControlBackups) installed(unit string) bool {
	for _, d := range b.unitDirs() {
		if _, err := os.Stat(filepath.Join(d, unit)); err == nil {
			return true
		}
	}
	return false
}

// timerOn is whether argus-backup.timer is enabled: systemctl enable links
// it from timers.target.wants.
func (b *ControlBackups) timerOn() bool {
	for _, d := range b.unitDirs() {
		if _, err := os.Stat(filepath.Join(d, "timers.target.wants", "argus-backup.timer")); err == nil {
			return true
		}
	}
	return false
}

// unitEnv reads ARGUS_BACKUP_DIR and ARGUS_BACKUP_KEEP from the backup
// unit's overrides, when this account can read them.
func (b *ControlBackups) unitEnv() map[string]string {
	path := b.EnvFile
	if path == "" {
		path = "/etc/argus/backup.env"
	}
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

func (b *ControlBackups) root(env map[string]string) string {
	if b.Root != "" {
		return b.Root
	}
	if d := env["ARGUS_BACKUP_DIR"]; filepath.IsAbs(d) {
		return d
	}
	return controlbackup.DefaultDir
}

// ControlBackupsView is Settings › Control plane › Backups.
type ControlBackupsView struct {
	// Installed is whether argus-backup.service is on this host at all.
	Installed bool   `json:"installed"`
	Reason    string `json:"reason,omitempty"`
	// CanRunNow is whether "Back up now" has a watcher to start the backup.
	CanRunNow    bool   `json:"canRunNow"`
	RunNowReason string `json:"runNowReason,omitempty"`
	TimerOn      bool   `json:"timerOn"`
	Dir          string `json:"dir"`
	// Keep is how many dumps a run keeps; KeepSetHere says the console set it.
	Keep        int                    `json:"keep"`
	KeepSetHere bool                   `json:"keepSetHere"`
	KeepSetBy   string                 `json:"keepSetBy,omitempty"`
	Last        *controlbackup.LastRun `json:"last,omitempty"`
	Running     bool                   `json:"running"`
	// Requested is a "Back up now" not yet started; Late says it has waited
	// longer than the watcher should take.
	Requested *controlbackup.Request `json:"requested,omitempty"`
	Late      bool                   `json:"late,omitempty"`
	Runs      []controlbackup.Run    `json:"runs"`
	// ReadFailure is why the backups could not be listed.
	ReadFailure string `json:"readFailure,omitempty"`
}

func (b *ControlBackups) view() ControlBackupsView {
	env := b.unitEnv()
	v := ControlBackupsView{
		Installed: b.installed("argus-backup.service"),
		CanRunNow: b.installed("argus-backup-now.path"),
		TimerOn:   b.timerOn(),
		Dir:       b.root(env),
		Keep:      14,
		Runs:      []controlbackup.Run{},
	}
	if !v.Installed {
		v.Reason = "argus-backup.service is not installed: argus-control was not installed from its package"
		v.CanRunNow = false
	}
	if v.Installed && !v.CanRunNow {
		v.RunNowReason = "the watcher that starts a backup from the console is not installed: " +
			"upgrade the argus-control package by hand once"
	}
	if n, err := strconv.Atoi(env["ARGUS_BACKUP_KEEP"]); err == nil && n > 0 {
		v.Keep = n
	}
	if s, ok, err := controlbackup.ReadSettings(b.StateDir); err == nil && ok && s.Keep > 0 {
		v.Keep, v.KeepSetHere, v.KeepSetBy = s.Keep, true, s.UpdatedBy
	}
	if l, ok, err := controlbackup.ReadLastRun(v.Dir); err == nil && ok {
		v.Last = &l
		v.Running = l.FinishedAt.IsZero() && time.Since(l.StartedAt) < backupRunMax
	}
	if r, ok, err := controlbackup.ReadRequest(b.StateDir); err == nil && ok &&
		(v.Last == nil || r.At.After(v.Last.StartedAt)) && time.Since(r.At) < backupRunMax {
		v.Requested = &r
		v.Late = time.Since(r.At) > backupRunLate
	}
	runs, err := controlbackup.List(v.Dir)
	if err != nil {
		v.ReadFailure = "cannot read " + v.Dir + ": " + err.Error()
	} else {
		v.Runs = runs
	}
	return v
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) controlBackups(w http.ResponseWriter) (*ControlBackups, bool) {
	if a.Backups == nil {
		writeErr(w, http.StatusNotFound, "backups are not set up on this control plane")
		return nil, false
	}
	return a.Backups, true
}

func (a *API) getControlBackups(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	b, ok := a.controlBackups(w)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, b.view())
}

func (a *API) putControlBackupSettings(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	b, ok := a.controlBackups(w)
	if !ok {
		return
	}
	var in struct {
		Keep int `json:"keep"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if in.Keep < controlbackup.MinKeep || in.Keep > controlbackup.MaxKeep {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("keep between %d and %d backups",
			controlbackup.MinKeep, controlbackup.MaxKeep))
		return
	}
	before := b.view().Keep
	if err := controlbackup.WriteSettings(b.StateDir, controlbackup.Settings{
		Keep: in.Keep, UpdatedBy: sess.Email, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		a.fail(w, "backup settings", err)
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "control.backup_retention_changed", Severity: "notice", ActorEmail: sess.Email,
		Target: "argus-backup",
		Detail: fmt.Sprintf("%s set the control plane's backups to keep the newest %d (was %d). "+
			"Older ones are removed by the next backup.", sess.Email, in.Keep, before),
	}); err != nil {
		a.fail(w, "backup settings", err)
		return
	}
	writeJSON(w, http.StatusOK, b.view())
}

func (a *API) postControlBackupRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	b, ok := a.controlBackups(w)
	if !ok {
		return
	}
	v := b.view()
	switch {
	case !v.CanRunNow:
		reason := v.RunNowReason
		if reason == "" {
			reason = v.Reason
		}
		writeErr(w, http.StatusConflict, reason)
		return
	case v.Running:
		writeErr(w, http.StatusConflict, "a backup is already running")
		return
	case v.Requested != nil && !v.Late:
		writeErr(w, http.StatusConflict, "a backup was asked for "+
			time.Since(v.Requested.At).Round(time.Second).String()+" ago and has not started yet")
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "control.backup_requested", Severity: "info", ActorEmail: sess.Email,
		Target: "argus-backup",
		Detail: sess.Email + " asked for a backup of the control plane now.",
	}); err != nil {
		a.fail(w, "backup", err)
		return
	}
	if err := controlbackup.WriteRequest(b.StateDir, controlbackup.Request{
		By: sess.Email, At: time.Now().UTC(),
	}); err != nil {
		a.fail(w, "backup", err)
		return
	}
	writeJSON(w, http.StatusAccepted, b.view())
}

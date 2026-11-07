package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/apk"
	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/jackc/pgx/v5"
)

/*
Files in the Software library, and installing them on Android devices.

An APK added here is read before it is kept: its application id, version and
signing certificate are shown to the person adding it, and a signer that
differs from another APK of the same app is called out, because Android
refuses to update an app with a package signed by someone else. The file is
kept where recordings and backups are, under its SHA-256.

On a device the Apps tab installs a library APK, uninstalls an app someone
installed, and with the unattended engine disables a preinstalled app for the
device's user (never truly removing it) or restores it. Each is a package
run with an app_* mode, so it waits for approval where the update rule says
so, travels on the heartbeat, and lands in Activity. The device fetches the
file over its own credential, for that run only, checks the hash and what
the package says about itself, and only then asks Android to install it.
With the attended engine Android asks the person holding the device.
*/

// App run modes, on package runs.
const (
	AppInstall   = "app_install"
	AppUninstall = "app_uninstall"
	AppDisable   = "app_disable"
	AppRestore   = "app_restore"
)

func isAppMode(m string) bool {
	switch m {
	case AppInstall, AppUninstall, AppDisable, AppRestore:
		return true
	}
	return false
}

// MaxSoftwareFile bounds one library file.
const MaxSoftwareFile = 2 << 30

// protectedApps are never uninstalled or disabled from the console: the
// Argus app (it is how Argus reaches the device), and what a device cannot
// be used without. The device refuses its own launcher and keyboard too,
// which only it knows.
var protectedApps = map[string]string{
	"com.gsoultan.argus":                  "how Argus reaches this device",
	"com.android.settings":                "the device's Settings",
	"com.android.systemui":                "the device's System UI",
	"com.android.vending":                 "the Play Store",
	"com.google.android.gms":              "Google Play services",
	"com.android.packageinstaller":        "Android's package installer",
	"com.google.android.packageinstaller": "Android's package installer",
	"com.android.phone":                   "the phone service",
	"android":                             "Android itself",
}

// SoftwareFile is a library file as the console reads it.
type SoftwareFile struct {
	ID        string    `json:"id"`
	Platform  string    `json:"platform"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	FileName  string    `json:"fileName"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	App       *FileApp  `json:"app,omitempty"`
	Note      string    `json:"note"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	// Warnings are said once, when the file is added.
	Warnings   []string `json:"warnings,omitempty"`
	storageKey string
}

// FileApp is what an APK says about itself.
type FileApp struct {
	Package     string `json:"package"`
	VersionCode int64  `json:"versionCode"`
	VersionName string `json:"versionName"`
	Label       string `json:"label"`
	MinSDK      int    `json:"minSdk"`
	Signer      string `json:"signer"`
	Scheme      string `json:"scheme"`
}

func fileApp(i apk.Info) *FileApp {
	return &FileApp{Package: i.Package, VersionCode: i.VersionCode, VersionName: i.VersionName, Label: i.Label,
		MinSDK: i.MinSDK, Signer: i.Signer, Scheme: i.Scheme}
}

// AppRunOrder is the app half of a run order, as the device reads it.
type AppRunOrder struct {
	Package string `json:"package"`
	// File is what an install fetches: a path on this control plane the
	// device's own credential opens for this run only.
	File *AppRunFile `json:"file,omitempty"`
}

// AppRunFile is the file an install fetches, and what it must be.
type AppRunFile struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	VersionCode int64  `json:"version_code"`
	VersionName string `json:"version_name,omitempty"`
	Signer      string `json:"signer"`
}

func (a *API) registerSoftwareFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/software/files", a.getSoftwareFiles)
	mux.HandleFunc("POST /api/v1/software/files", a.postSoftwareFile)
	mux.HandleFunc("DELETE /api/v1/software/files/{id}", a.deleteSoftwareFile)
	mux.HandleFunc("POST /api/v1/assets/{id}/android-apps/runs", a.postAndroidAppRun)
	mux.HandleFunc("GET /api/v1/report/software-file/{run}", a.agentReporter(a.getRunSoftwareFile))
}

/* ── The library ─────────────────────────────────────────────────────────── */

// GET /api/v1/software/files
func (a *API) getSoftwareFiles(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "the software library is for administrators, owners and auditors")
		return
	}
	files, err := a.store.softwareFiles(r.Context(), r.URL.Query().Get("platform"))
	if err != nil {
		a.fail(w, "software files", err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

// POST /api/v1/software/files: multipart, one part named file; ?note=.
func (a *API) postSoftwareFile(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "adding files to the software library", "admin", "owner") {
		return
	}
	store := a.backupStore(r.Context())
	if a.recordings == nil {
		writeErr(w, http.StatusServiceUnavailable, "this control plane has nowhere to keep files: "+
			"configure object storage or a recordings directory first")
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		writeErr(w, http.StatusBadRequest, "send the file as multipart/form-data, in a part named file")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxSoftwareFile+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var (
		tmp      *os.File
		name     string
		size     int64
		sum      string
		tooLarge bool
	)
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "the upload was cut short: "+err.Error())
			return
		}
		if part.FormName() != "file" || tmp != nil {
			_ = part.Close()
			continue
		}
		name = filepath.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
		tmp, err = os.CreateTemp("", "argus-software-*")
		if err != nil {
			a.fail(w, "software file", err)
			return
		}
		h := sha256.New()
		size, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(part, MaxSoftwareFile+1))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				tooLarge = true
				break
			}
			writeErr(w, http.StatusBadRequest, "the upload was cut short: "+err.Error())
			return
		}
		sum = hex.EncodeToString(h.Sum(nil))
		_ = part.Close()
	}
	switch {
	case tooLarge || size > MaxSoftwareFile:
		writeErr(w, http.StatusRequestEntityTooLarge, "a library file is at most 2 GB")
		return
	case tmp == nil || size == 0:
		writeErr(w, http.StatusBadRequest, "no file came with the upload")
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".apk":
	case ".msi", ".exe":
		writeErr(w, http.StatusBadRequest, "MSI and EXE files come with installing on Windows, which is not built yet")
		return
	default:
		writeErr(w, http.StatusBadRequest, "the library takes Android packages (.apk); scripts go under Install scripts")
		return
	}
	info, err := apk.Read(tmp, size)
	if err != nil {
		writeErr(w, http.StatusBadRequest, name+": "+err.Error())
		return
	}
	if existing, err := a.store.softwareFileBySHA(r.Context(), sum); err == nil {
		writeJSON(w, http.StatusOK, existing)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		a.fail(w, "software file", err)
		return
	}
	f := SoftwareFile{
		Platform: "android", Kind: "apk", Name: firstNonEmpty(info.Label, info.Package), FileName: clip(name, 255),
		SHA256: sum, Size: size, App: fileApp(info), Note: clip(strings.TrimSpace(r.URL.Query().Get("note")), 500),
		CreatedBy: sess.Email,
	}
	f.Warnings, err = a.store.signerWarnings(r.Context(), info)
	if err != nil {
		a.fail(w, "software file", err)
		return
	}
	if info.Scheme == "v1" {
		f.Warnings = append(f.Warnings, "Signed only with the old v1 scheme. Android 11 and later refuse an app "+
			"that targets them without a v2 signature.")
	}
	key, err := store.PutKey(r.Context(), tmp.Name(), "software/"+sum+ext)
	if err != nil {
		a.fail(w, "store software file", err)
		return
	}
	f.storageKey = key
	if err := a.store.insertSoftwareFile(r.Context(), &f, info); err != nil {
		a.fail(w, "software file", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "software.file_added", Severity: "notice", ActorEmail: sess.Email, Target: f.Name,
		Detail: fmt.Sprintf("Added %s to the software library: %s %s (version code %d), signed by %s…, SHA-256 %s.",
			f.FileName, info.Package, info.VersionName, info.VersionCode, info.Signer[:16], sum)})
	writeJSON(w, http.StatusCreated, f)
}

// DELETE /api/v1/software/files/{id}
func (a *API) deleteSoftwareFile(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "removing files from the software library", "admin", "owner") {
		return
	}
	f, err := a.store.softwareFile(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "no such file in the library")
		return
	}
	if err != nil {
		a.fail(w, "software file", err)
		return
	}
	if _, err := a.store.db(r.Context()).Exec(r.Context(),
		`UPDATE software_files SET archived_at = now() WHERE id = $1::uuid AND archived_at IS NULL`, f.ID); err != nil {
		a.fail(w, "software file", err)
		return
	}
	if err := a.backupStore(r.Context()).Delete(r.Context(), f.storageKey); err != nil {
		a.log.Warn("software file: the stored copy was not deleted", "key", f.storageKey, "error", err)
	}
	a.auditAsset(r, AuditEvent{Action: "software.file_removed", Severity: "notice", ActorEmail: sess.Email, Target: f.Name,
		Detail: "Removed " + f.FileName + " from the software library. Devices that installed it keep the app."})
	w.WriteHeader(http.StatusNoContent)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

/* ── Installing on a device ──────────────────────────────────────────────── */

// POST /api/v1/assets/{id}/android-apps/runs {action, fileId?, package?}
func (a *API) postAndroidAppRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing apps on devices", "admin", "owner") {
		return
	}
	var in struct {
		Action  string `json:"action"`
		FileID  string `json:"fileId"`
		Package string `json:"package"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	if asset.Kind != "android" {
		writeErr(w, http.StatusConflict, "apps are installed this way on Android devices; a server installs from its Apps tab")
		return
	}
	t, err := a.packagesFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "device", err)
		return
	}
	switch {
	case !t.view.Enrolled:
		writeErr(w, http.StatusConflict, "this device's Argus app has not linked yet")
		return
	case !t.view.Reported:
		writeErr(w, http.StatusConflict, "the Argus app on this device is too old to install apps; update it first")
		return
	}
	sys, err := a.store.SystemFor(r.Context(), asset.ID)
	if err != nil {
		a.fail(w, "device", err)
		return
	}
	engine := ""
	if sys.System != nil {
		engine = sys.System.Engine
	}
	apps, err := a.store.AndroidAppsFor(r.Context(), asset.ID)
	if err != nil {
		a.fail(w, "device", err)
		return
	}
	installed := map[string]ServerAndroidApp{}
	for _, app := range apps.Apps {
		installed[app.Package] = app
	}

	n := newRun{
		Hostname: t.agent, AssetHostname: asset.Hostname, Mode: in.Action, Packages: []string{}, Holds: []string{},
		Reboot: "never", Trigger: "manual", State: RunQueued, RequestedBy: sess.Email,
	}
	var file SoftwareFile
	switch in.Action {
	case "install":
		n.Mode = AppInstall
		file, err = a.store.softwareFile(r.Context(), in.FileID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "no such file in the library")
			return
		}
		if err != nil {
			a.fail(w, "software file", err)
			return
		}
		if file.Kind != "apk" || file.App == nil {
			writeErr(w, http.StatusBadRequest, "that library file is not an Android package")
			return
		}
		if sys.System != nil && sys.System.CanInstallApps != nil && !*sys.System.CanInstallApps && engine != "unattended" {
			writeErr(w, http.StatusConflict, "the person holding this device has not allowed the Argus app to install apps yet: "+
				"open Argus on the device and allow Install unknown apps")
			return
		}
		if why, ok := protectedApps[file.App.Package]; ok {
			writeErr(w, http.StatusConflict, file.App.Package+" is protected ("+why+"); it is not installed from the library")
			return
		}
		n.SoftwareFileID = &file.ID
		n.Expect = file.SHA256
		n.Items = []tunnel.SoftwareItem{{Name: file.App.Package, Version: file.App.VersionName}}
	case "uninstall", "disable", "restore":
		n.Mode = "app_" + in.Action
		pkg := strings.TrimSpace(in.Package)
		if !apk.RePackage.MatchString(pkg) {
			writeErr(w, http.StatusBadRequest, "say which app, by its package name")
			return
		}
		if why, ok := protectedApps[pkg]; ok {
			writeErr(w, http.StatusConflict, pkg+" is protected: "+why)
			return
		}
		app, has := installed[pkg]
		switch in.Action {
		case "uninstall":
			if !has || app.Disabled {
				writeErr(w, http.StatusConflict, pkg+" is not installed on this device")
				return
			}
			if app.Source == "preinstalled" {
				writeErr(w, http.StatusConflict, pkg+" came with the device and cannot be uninstalled; "+
					"with the unattended engine it can be disabled for the device's user instead")
				return
			}
		case "disable", "restore":
			if engine != "unattended" {
				writeErr(w, http.StatusConflict, "disabling and restoring preinstalled apps needs the unattended engine")
				return
			}
			if !has || app.Source != "preinstalled" {
				writeErr(w, http.StatusConflict, "only an app that came with the device is disabled; uninstall others")
				return
			}
			if in.Action == "disable" && app.Disabled {
				writeErr(w, http.StatusConflict, pkg+" is already disabled")
				return
			}
			if in.Action == "restore" && !app.Disabled {
				writeErr(w, http.StatusConflict, pkg+" is not disabled")
				return
			}
		}
		n.Items = []tunnel.SoftwareItem{{Name: pkg, Version: app.Version}}
	default:
		writeErr(w, http.StatusBadRequest, "action is install, uninstall, disable or restore")
		return
	}
	if t.view.Rule.NeedsApproval {
		n.State = RunAwaitingApproval
	}
	runs, err := a.store.CreatePackageRuns(r.Context(), []newRun{n})
	if errors.Is(err, ErrRunActive) {
		writeErr(w, http.StatusConflict, "this device already has a change waiting or running; it does one at a time")
		return
	}
	if err != nil {
		a.fail(w, "app run", err)
		return
	}
	run := runs[0]
	run.AssetID = asset.ID
	extra := ""
	if n.Mode == AppInstall {
		extra = fmt.Sprintf(" from %s (SHA-256 %s)", file.FileName, file.SHA256)
	}
	switch {
	case engine == "attended" && (n.Mode == AppInstall || n.Mode == AppUninstall):
		extra += "; Android asks the person holding the device to confirm"
	case engine == "unattended":
		extra += "; the unattended engine does it without asking"
	}
	if run.State == RunAwaitingApproval {
		extra += "; it waits for another admin to approve it"
	}
	a.auditAsset(r, AuditEvent{Action: "software.app_requested", Severity: "notice", ActorEmail: sess.Email,
		Target: asset.Hostname, Detail: fmt.Sprintf("Asked for %s on %s%s.", runPhrase(run), asset.Hostname, extra)})
	if run.State == RunAwaitingApproval {
		a.raiseAlert(r.Context(), ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: sess.Email,
			Title:   sess.Email + " asks for " + runPhrase(run) + " on " + asset.Hostname,
			Detail:  "Approve or deny it on the device's Apps tab.",
			Link:    "/assets/" + asset.ID + "?tab=apps",
			Subject: packageRunSubject(run.ID),
		})
	}
	writeJSON(w, http.StatusAccepted, run)
}

// describeApp names what an app run changes.
func describeApp(r PackageRun) string {
	name := "an app"
	if len(r.Items) > 0 {
		name = r.Items[0].Name
		if r.Mode == AppInstall && r.Items[0].Version != "" {
			name += " " + r.Items[0].Version
		}
	}
	return name
}

// attachApp fills the app half of a run on its way to a device. A run that
// cannot be sent fails here, not on the device.
func (a *API) attachApp(ctx context.Context, host string, o *PackageRunOrder) bool {
	if o == nil || !isAppMode(o.Mode) {
		return true
	}
	order, err := a.appOrder(ctx, o)
	if err == nil {
		o.App = order
		return true
	}
	a.log.Error("heartbeat: cannot send an app run", "run", o.ID, "error", err)
	if r, ferr := a.store.failUnsentRun(ctx, host, o.ID, err.Error()); ferr == nil && r != nil {
		a.packageRunEnded(ctx, *r)
	}
	return false
}

func (a *API) appOrder(ctx context.Context, o *PackageRunOrder) (*AppRunOrder, error) {
	if len(o.Items) != 1 || !apk.RePackage.MatchString(o.Items[0].Name) {
		return nil, errors.New("the run names no app")
	}
	out := &AppRunOrder{Package: o.Items[0].Name}
	if o.Mode != AppInstall {
		return out, nil
	}
	var fileID *string
	if err := a.store.db(ctx).QueryRow(ctx, `SELECT software_file_id::text FROM package_runs WHERE id = $1::uuid`,
		o.ID).Scan(&fileID); err != nil {
		return nil, err
	}
	if fileID == nil {
		return nil, errors.New("the install names no library file")
	}
	f, err := a.store.softwareFileAny(ctx, *fileID)
	if err != nil {
		return nil, err
	}
	if f.archived {
		return nil, errors.New(f.FileName + " was removed from the software library")
	}
	if f.App == nil || f.App.Package != out.Package || f.SHA256 != o.Expect {
		return nil, errors.New("the library file is not the one asked for")
	}
	out.File = &AppRunFile{Path: "/api/v1/report/software-file/" + o.ID, SHA256: f.SHA256, Size: f.Size,
		VersionCode: f.App.VersionCode, VersionName: f.App.VersionName, Signer: f.App.Signer}
	return out, nil
}

// GET /api/v1/report/software-file/{run}: the file an install on this
// device fetches. Only the device the run is for, only while it runs.
func (a *API) getRunSoftwareFile(w http.ResponseWriter, r *http.Request) {
	host := agentHost(r)
	if host == "" {
		writeErr(w, http.StatusForbidden, "library files are fetched on a device's own credential")
		return
	}
	run, err := a.store.PackageRunByID(r.Context(), r.PathValue("run"))
	if err != nil {
		a.failRun(w, err)
		return
	}
	switch {
	case !strings.EqualFold(run.Hostname, host):
		writeErr(w, http.StatusNotFound, "no such run for this device")
		return
	case run.Mode != AppInstall || run.SoftwareFileID == nil:
		writeErr(w, http.StatusBadRequest, "that run installs no file")
		return
	case run.State != RunQueued && run.State != RunRunning:
		writeErr(w, http.StatusGone, "that run has ended")
		return
	}
	f, err := a.store.softwareFileAny(r.Context(), *run.SoftwareFileID)
	if err != nil || f.archived {
		writeErr(w, http.StatusGone, "the file was removed from the software library")
		return
	}
	rc, err := a.backupStore(r.Context()).Get(r.Context(), f.storageKey)
	if err != nil {
		a.fail(w, "software file", err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("X-Argus-SHA256", f.SHA256)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		a.log.Warn("software file: the device stopped fetching", "run", run.ID, "error", err)
	}
}

/* ── Store ───────────────────────────────────────────────────────────────── */

const softwareFileCols = `id::text, platform, kind, name, file_name, sha256, size_bytes, meta, note, created_by,
	created_at, storage_key, archived_at IS NOT NULL`

type softwareFileRow struct {
	SoftwareFile
	archived bool
}

func scanSoftwareFile(row pgx.Row) (softwareFileRow, error) {
	var (
		f    softwareFileRow
		meta []byte
	)
	err := row.Scan(&f.ID, &f.Platform, &f.Kind, &f.Name, &f.FileName, &f.SHA256, &f.Size, &meta, &f.Note,
		&f.CreatedBy, &f.CreatedAt, &f.storageKey, &f.archived)
	if err != nil {
		return f, err
	}
	if f.Kind == "apk" {
		var i apk.Info
		if json.Unmarshal(meta, &i) == nil && i.Package != "" {
			f.App = fileApp(i)
		}
	}
	return f, nil
}

func (s *Store) softwareFiles(ctx context.Context, platform string) ([]SoftwareFile, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+softwareFileCols+` FROM software_files
		WHERE archived_at IS NULL AND ($1 = '' OR platform = $1) ORDER BY lower(name), created_at DESC`, platform)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SoftwareFile{}
	for rows.Next() {
		f, err := scanSoftwareFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f.SoftwareFile)
	}
	return out, rows.Err()
}

// softwareFile is a live library file.
func (s *Store) softwareFile(ctx context.Context, id string) (SoftwareFile, error) {
	f, err := s.softwareFileAny(ctx, id)
	if err == nil && f.archived {
		return SoftwareFile{}, pgx.ErrNoRows
	}
	return f.SoftwareFile, err
}

// softwareFileAny is a library file, removed or not.
func (s *Store) softwareFileAny(ctx context.Context, id string) (softwareFileRow, error) {
	if !rePreviewID.MatchString(id) {
		return softwareFileRow{}, pgx.ErrNoRows
	}
	return scanSoftwareFile(s.db(ctx).QueryRow(ctx, `SELECT `+softwareFileCols+` FROM software_files
		WHERE id = $1::uuid`, id))
}

func (s *Store) softwareFileBySHA(ctx context.Context, sum string) (SoftwareFile, error) {
	f, err := scanSoftwareFile(s.db(ctx).QueryRow(ctx, `SELECT `+softwareFileCols+` FROM software_files
		WHERE sha256 = $1 AND archived_at IS NULL`, sum))
	return f.SoftwareFile, err
}

func (s *Store) insertSoftwareFile(ctx context.Context, f *SoftwareFile, info apk.Info) error {
	meta, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return s.db(ctx).QueryRow(ctx, `
		INSERT INTO software_files (platform, kind, name, file_name, sha256, size_bytes, storage_key, meta, note, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10)
		RETURNING id::text, created_at`,
		f.Platform, f.Kind, f.Name, f.FileName, f.SHA256, f.Size, f.storageKey, meta, f.Note, f.CreatedBy,
	).Scan(&f.ID, &f.CreatedAt)
}

// signerWarnings says when other library APKs of the same app are signed by
// someone else: a device with one installed refuses the other as an update.
func (s *Store) signerWarnings(ctx context.Context, info apk.Info) ([]string, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT DISTINCT file_name FROM software_files
		WHERE archived_at IS NULL AND kind = 'apk' AND meta->>'package' = $1 AND meta->>'signer' <> $2`,
		info.Package, info.Signer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil || len(names) == 0 {
		return nil, err
	}
	return []string{fmt.Sprintf("Signed by a different key than %s. A device that has one of them installed "+
		"refuses the other as an update; it has to be uninstalled first, which deletes the app's data.",
		strings.Join(names, ", "))}, nil
}

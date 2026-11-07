package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/apk"
)

/*
Installing, uninstalling, disabling and restoring apps on an Android device.

The console's order arrives on a heartbeat reply as a package run with an
app_* mode, like an update run on a server. For an install the device fetches
the library file on its own credential, checks the SHA-256 the console named
and that the package is the app, version and signer the console showed, and
only then hands it to the app (AppInstaller), which asks Android. With the
attended engine Android asks the person holding the device, who can say no;
with the unattended engine the shell helper does it. Nothing here runs a
command: the app decides how, this decides whether.
*/

// App run modes.
const (
	AppInstall   = "app_install"
	AppUninstall = "app_uninstall"
	AppDisable   = "app_disable"
	AppRestore   = "app_restore"
)

// IsAppMode reports whether a run is for an app on a device.
func IsAppMode(m string) bool {
	switch m {
	case AppInstall, AppUninstall, AppDisable, AppRestore:
		return true
	}
	return false
}

// AppOrder is the app half of a run.
type AppOrder struct {
	Package string   `json:"package"`
	File    *AppFile `json:"file,omitempty"`
}

// AppFile is what an install fetches and what it must be.
type AppFile struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	VersionCode int64  `json:"version_code"`
	VersionName string `json:"version_name,omitempty"`
	Signer      string `json:"signer"`
}

// AppInstaller is what the app does on the device. Each call blocks until
// Android has answered: installed, or refused, or the person said no.
type AppInstaller interface {
	Install(path, pkg string) error
	Uninstall(pkg string) error
	Disable(pkg string) error
	Restore(pkg string) error
}

// AppFetch fetches a control-plane path into w, at most limit bytes.
type AppFetch func(ctx context.Context, path string, w io.Writer, limit int64) (int64, error)

// Steps a device run reports.
const (
	AppStepDownload = "download"
	AppStepCheck    = "check"
	// AppStepDevice is Android asking the person, or the helper working.
	AppStepDevice = "device"
)

// appRunLimit bounds one run on the device: an attended install the person
// never answers gives up on the device after ten minutes; this is the
// backstop for an app that never answers at all.
const appRunLimit = 20 * time.Minute

// AppRuns runs app orders one at a time and keeps the last one's status to
// report.
type AppRuns struct {
	Dir       string
	Fetch     AppFetch
	Installer AppInstaller
	Log       *slog.Logger
	// Attended says whether the person holding the device answers, for the
	// status the console shows while it waits.
	Attended func() bool
	// Now is time.Now, replaced by tests.
	Now func() time.Time

	mu  sync.Mutex
	run PackageRunStatus
}

// Status is the run to report on the next heartbeat, nil before the first.
func (r *AppRuns) Status() *PackageRunStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run.ID == "" {
		return nil
	}
	s := r.run
	return &s
}

func (r *AppRuns) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *AppRuns) set(f func(s *PackageRunStatus)) {
	r.mu.Lock()
	f(&r.run)
	r.run.At = r.now()
	r.mu.Unlock()
}

// Start starts req once; a second order for the same run, or one while
// another goes, is ignored. done is called when it ends.
func (r *AppRuns) Start(ctx context.Context, req PackageRunRequest, done func()) {
	r.mu.Lock()
	if req.ID == r.run.ID || (r.run.ID != "" && !r.run.Finished()) || !reRunID.MatchString(req.ID) {
		r.mu.Unlock()
		return
	}
	now := r.now()
	r.run = PackageRunStatus{ID: req.ID, State: RunRunning, StartedAt: now, At: now}
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(ctx, appRunLimit)
		defer cancel()
		changes, err := r.do(ctx, req)
		r.set(func(s *PackageRunStatus) {
			s.FinishedAt, s.Step = r.now(), ""
			if err != nil {
				s.State, s.Message = RunFailed, err.Error()
				return
			}
			s.State, s.Upgraded = RunDone, changes
			s.Message = doneMessage(req)
		})
		if done != nil {
			done()
		}
	}()
}

func doneMessage(req PackageRunRequest) string {
	pkg := ""
	if req.App != nil {
		pkg = req.App.Package
	}
	switch req.Mode {
	case AppInstall:
		return pkg + " is installed"
	case AppUninstall:
		return pkg + " is uninstalled"
	case AppDisable:
		return pkg + " is disabled for the device's user"
	default:
		return pkg + " is restored"
	}
}

func (r *AppRuns) do(ctx context.Context, req PackageRunRequest) ([]PackageChange, error) {
	if r.Installer == nil {
		return nil, errors.New("this app cannot install apps; update it")
	}
	if req.App == nil || !apk.RePackage.MatchString(req.App.Package) {
		return nil, errors.New("the order names no app; nothing was changed")
	}
	pkg := req.App.Package
	step := func(st, msg string) {
		r.set(func(s *PackageRunStatus) { s.Step, s.Message = st, msg })
	}
	asking := "the helper is "
	if r.Attended != nil && r.Attended() {
		asking = "Android asked the person holding the device to confirm "
	}
	var err error
	switch req.Mode {
	case AppInstall:
		var path string
		path, err = r.fetch(ctx, req, step)
		if err != nil {
			return nil, err
		}
		defer os.Remove(path)
		step(AppStepDevice, asking+"installing "+pkg)
		err = r.Installer.Install(path, pkg)
		if err == nil {
			return []PackageChange{{Name: pkg, To: req.App.File.VersionName}}, nil
		}
	case AppUninstall:
		step(AppStepDevice, asking+"uninstalling "+pkg)
		err = r.Installer.Uninstall(pkg)
		if err == nil {
			return []PackageChange{{Name: pkg}}, nil
		}
	case AppDisable:
		step(AppStepDevice, "disabling "+pkg+" for the device's user")
		err = r.Installer.Disable(pkg)
	case AppRestore:
		step(AppStepDevice, "restoring "+pkg+" for the device's user")
		err = r.Installer.Restore(pkg)
	default:
		return nil, fmt.Errorf("an app run cannot be %q", req.Mode)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pkg, err)
	}
	return []PackageChange{{Name: pkg}}, nil
}

// fetch downloads the file, and keeps it only when it is what the console
// said: the hash, then the package, version code and signer inside it.
func (r *AppRuns) fetch(ctx context.Context, req PackageRunRequest, step func(string, string)) (string, error) {
	f := req.App.File
	if f == nil || f.Path == "" || len(f.SHA256) != 64 || f.Size <= 0 {
		return "", errors.New("the order names no file to install; nothing was changed")
	}
	if r.Fetch == nil {
		return "", errors.New("this app cannot fetch files; update it")
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return "", err
	}
	step(AppStepDownload, fmt.Sprintf("downloading %s (%d MB)", req.App.Package, (f.Size+1<<20-1)>>20))
	tmp, err := os.CreateTemp(r.Dir, "app-*.apk")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	n, err := r.Fetch(ctx, f.Path, io.MultiWriter(tmp, h), f.Size)
	if err != nil {
		return "", fmt.Errorf("the download failed: %w; nothing was installed", err)
	}
	step(AppStepCheck, "checking the file is the one the console showed")
	if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return "", errors.New("the downloaded file is not the one the console showed (its SHA-256 differs); nothing was installed")
	}
	info, err := apk.Read(tmp, n)
	if err != nil {
		return "", fmt.Errorf("the file is not an installable package: %w; nothing was installed", err)
	}
	switch {
	case info.Package != req.App.Package:
		return "", fmt.Errorf("the file is %s, not %s; nothing was installed", info.Package, req.App.Package)
	case info.VersionCode != f.VersionCode:
		return "", fmt.Errorf("the file is version code %d, not %d; nothing was installed", info.VersionCode, f.VersionCode)
	case info.Signer != f.Signer:
		return "", errors.New("the file is signed by a different key than the console showed; nothing was installed")
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	keep = true
	return tmp.Name(), nil
}

// AppRunsDir is where an install keeps the file while Android installs it.
func AppRunsDir(stateDir string) string { return filepath.Join(stateDir, "apps") }

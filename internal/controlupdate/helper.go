package controlupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultReleasesURL is where releases are downloaded from:
// <url>/v<version>/<file>.
const DefaultReleasesURL = "https://github.com/gsoultan/argus/releases/download"

const (
	// readyWait is how long a new version has to say it is serving.
	readyWait = 90 * time.Second
	// crashLoop is how many times it may exit on its own before the helper
	// stops waiting for it.
	crashLoop = 3
	// steadyFor is how long a version that writes no ready.json -- one from
	// before this helper -- must stay up, unrestarted, to count as back.
	steadyFor = 15 * time.Second
	// maxPackage bounds a download: the control plane's package, console
	// included, is tens of megabytes.
	maxPackage = 512 << 20
)

// Helper carries out an update request. It is what
// `argus-control update-apply` runs, as root, from argus-control-update.service.
type Helper struct {
	// Dir is the control plane's update directory: request in, status out.
	Dir string
	// Work is the helper's own directory, for packages and the copy of the
	// running binary. Root's alone.
	Work string
	// ReleasesURL is where packages come from; empty is DefaultReleasesURL.
	ReleasesURL string
	// Unit is the control plane's systemd unit; BackupUnit takes a backup.
	Unit, BackupUnit string
	// Executable is where the package puts the control plane.
	Executable string

	HTTP *http.Client
	Log  *slog.Logger
	// Run runs a command; nil is the real one. Output runs one and returns
	// what it printed. Replaced in tests.
	Run    func(name string, args ...string) error
	Output func(name string, args ...string) ([]byte, error)
	// Restarts reads how many times systemd has restarted Unit; nil reads it.
	Restarts func() int
	// Active reports whether Unit is running; nil asks systemctl.
	Active func() bool
	// ReadyWait and SteadyFor override the waits in tests.
	ReadyWait, SteadyFor time.Duration
	// Arch is the package architecture; empty is this machine's.
	Arch string

	status Status
}

func (h *Helper) defaults() {
	if h.Log == nil {
		h.Log = slog.Default()
	}
	if h.HTTP == nil {
		h.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	if h.ReleasesURL == "" {
		h.ReleasesURL = DefaultReleasesURL
	}
	if h.Unit == "" {
		h.Unit = "argus-control"
	}
	if h.BackupUnit == "" {
		h.BackupUnit = "argus-backup.service"
	}
	if h.Executable == "" {
		h.Executable = "/usr/bin/argus-control"
	}
	if h.ReadyWait == 0 {
		h.ReadyWait = readyWait
	}
	if h.SteadyFor == 0 {
		h.SteadyFor = steadyFor
	}
	if h.Arch == "" {
		h.Arch = runtime.GOARCH
	}
}

// Apply reads the request and carries it out, if it has not been already. It
// returns the state it ended in, which it has also written to status.json.
func (h *Helper) Apply(ctx context.Context) string {
	h.defaults()
	req, err := ReadRequest(h.Dir)
	if err != nil {
		h.Log.Error("update: cannot read the request", "error", err)
		return ""
	}
	if prev, ok := ReadStatus(h.Dir); ok && prev.ID == req.ID &&
		prev.State != StateWaiting && prev.State != StateRequested {
		// Already carried out, or being carried out by a helper that was cut
		// off (a reboot): either way not again. The control plane offers the
		// update afresh with a new id.
		h.Log.Info("update: this request was already handled", "id", req.ID, "state", prev.State)
		return prev.State
	}
	h.status = Status{ID: req.ID, Target: req.Version, RequestedBy: req.RequestedBy}

	// What is running is asked of the binary, not taken from the request.
	from, err := h.installedVersion()
	if err != nil {
		return h.fail("cannot tell which version is installed: " + err.Error())
	}
	h.status.From = from
	if from == req.Version {
		return h.end(StateDone, "already running "+from)
	}
	format := h.format()
	if format == "" {
		return h.fail("argus-control was not installed from its package, so there is no " +
			"package to upgrade; upgrade it by hand")
	}

	h.step(StateDownloading, "downloading "+req.Version)
	if err := os.RemoveAll(h.Work); err != nil {
		return h.fail("cannot clear " + h.Work + ": " + err.Error())
	}
	if err := os.MkdirAll(h.Work, 0o700); err != nil {
		return h.fail("cannot make " + h.Work + ": " + err.Error())
	}
	base := strings.TrimRight(h.ReleasesURL, "/")
	pkg, err := h.fetchVerified(ctx, base, req.Version, format, filepath.Join(h.Work, "new"))
	if err != nil {
		return h.fail(err.Error() + "; nothing was changed")
	}
	rollback := ""
	if ValidVersion(from) {
		if p, err := h.fetchVerified(ctx, base, from, format, filepath.Join(h.Work, "previous")); err == nil {
			rollback = p
		} else {
			h.Log.Warn("update: the running version's package could not be downloaded; "+
				"going back would restore the binary alone", "error", err)
		}
	}
	previous := filepath.Join(h.Work, "argus-control.previous")
	if err := copyFile(h.Executable, previous, 0o755); err != nil {
		return h.fail("cannot keep a copy of the running control plane: " + err.Error())
	}
	h.step(StateDownloading, req.Version+" downloaded; it matches the release's checksums")

	if h.unitExists(h.BackupUnit) {
		h.step(StateBackingUp, "taking a backup")
		if err := h.run("systemctl", "start", h.BackupUnit); err != nil {
			return h.fail("the backup did not finish, so nothing was changed: " + err.Error() +
				". See: journalctl -u " + h.BackupUnit)
		}
		h.step(StateBackingUp, "backup taken")
	} else {
		h.step(StateBackingUp, "no backup unit is installed; going on without a backup")
	}

	// Packages from before this update shipped a pre-removal script that
	// stopped and disabled the control plane on every upgrade. Whatever the
	// old package's script does, the unit is left enabled if it was.
	enabled := h.run("systemctl", "is-enabled", "--quiet", h.Unit) == nil

	h.step(StateInstalling, "installing "+req.Version)
	started := time.Now()
	reason := ""
	if err := h.install(format, pkg); err != nil {
		reason = "the package for " + req.Version + " would not install: " + err.Error()
	} else {
		h.step(StateRestarting, "restarting on "+req.Version)
		if err := h.restart(enabled); err != nil {
			reason = "argus-control would not restart on " + req.Version + ": " + err.Error()
		} else if err := h.awaitReady(req.Version, started, false); err != nil {
			reason = req.Version + " " + err.Error()
		}
	}
	if reason == "" {
		return h.end(StateDone, fmt.Sprintf("updated from %s to %s; ready in %s", from, req.Version,
			time.Since(started).Round(time.Second)))
	}

	h.Log.Error("update: "+reason+"; going back", "to", from)
	h.step(StateRollingBack, reason+"; going back to "+from)
	back := time.Now()
	err = h.goBack(format, rollback, previous)
	if err == nil {
		err = h.restart(enabled)
	}
	if err == nil {
		err = h.awaitReady(from, back, true)
	}
	if err != nil {
		return h.end(StateFailed, reason+", and going back to "+from+" did not work either: "+
			err.Error()+". On the control plane's host run: sudo systemctl status argus-control")
	}
	return h.end(StateRolledBack, reason+"; went back to "+from)
}

func (h *Helper) step(state, msg string) {
	now := time.Now().UTC()
	h.status.State, h.status.Message, h.status.At = state, msg, now
	h.status.Steps = append(h.status.Steps, Step{State: state, Message: msg, At: now})
	if err := WriteStatus(h.Dir, h.status); err != nil {
		h.Log.Error("update: cannot write the update's status", "error", err)
	}
	h.Log.Info("update: " + msg)
}

func (h *Helper) end(state, msg string) string {
	h.step(state, msg)
	// The packages and the copy go; status.json and the journal stay.
	_ = os.RemoveAll(h.Work)
	return state
}

func (h *Helper) fail(msg string) string { return h.end(StateFailed, msg) }

func (h *Helper) run(name string, args ...string) error {
	if h.Run != nil {
		return h.Run(name, args...)
	}
	cmd := exec.Command(name, args...)
	// The package's scripts restart nothing while the update is doing it.
	cmd.Env = append(os.Environ(), "ARGUS_CONTROL_UPDATING=1", "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return fmt.Errorf("%s: %v: %s", name, err, msg)
	}
	return nil
}

func (h *Helper) output(name string, args ...string) ([]byte, error) {
	if h.Output != nil {
		return h.Output(name, args...)
	}
	return exec.Command(name, args...).Output()
}

// installedVersion asks the installed binary which version it is.
func (h *Helper) installedVersion() (string, error) {
	out, err := h.output(h.Executable, "-version")
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return "", fmt.Errorf("%s -version printed %q", h.Executable, strings.TrimSpace(string(out)))
	}
	return strings.TrimPrefix(f[len(f)-1], "v"), nil
}

// format is the package manager that owns argus-control here.
func (h *Helper) format() string {
	if out, err := h.output("dpkg-query", "-W", "-f=${Status}", "argus-control"); err == nil &&
		strings.Contains(string(out), "ok installed") {
		return "deb"
	}
	if _, err := h.output("rpm", "-q", "argus-control"); err == nil {
		return "rpm"
	}
	if _, err := h.output("apk", "info", "-e", "argus-control"); err == nil {
		return "apk"
	}
	return ""
}

// FileName is the release's name for the control plane's package.
func FileName(version, arch, format string) string {
	return fmt.Sprintf("argus-control_%s_linux_%s.%s", version, arch, format)
}

func (h *Helper) unitExists(unit string) bool {
	return h.run("systemctl", "cat", unit) == nil
}

func (h *Helper) install(format, pkg string) error {
	switch format {
	case "deb":
		return h.run("dpkg", "-i", "--force-confold", pkg)
	case "rpm":
		return h.run("rpm", "-U", "--replacepkgs", "--oldpackage", pkg)
	case "apk":
		return h.run("apk", "add", "--allow-untrusted", pkg)
	}
	return fmt.Errorf("no way to install a %q package", format)
}

// goBack reinstalls the version that was running, or puts its binary back
// when its package could not be had.
func (h *Helper) goBack(format, rollback, previous string) error {
	if rollback != "" {
		err := h.install(format, rollback)
		if err == nil {
			return nil
		}
		h.Log.Error("update: the previous package would not install; restoring the binary", "error", err)
	}
	if h.Run != nil {
		return h.Run("restore", previous, h.Executable)
	}
	tmp := h.Executable + ".argus-rollback"
	if err := copyFile(previous, tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, h.Executable)
}

func (h *Helper) restart(enabled bool) error {
	_ = h.run("systemctl", "daemon-reload")
	if enabled {
		_ = h.run("systemctl", "enable", h.Unit)
	}
	// A version that kept exiting may have hit systemd's start limit, which
	// would refuse the restart that puts the old one back.
	_ = h.run("systemctl", "reset-failed", h.Unit)
	return h.run("systemctl", "restart", h.Unit)
}

func (h *Helper) restarts() int {
	if h.Restarts != nil {
		return h.Restarts()
	}
	out, err := h.output("systemctl", "show", "-p", "NRestarts", "--value", h.Unit)
	if err != nil {
		return 0
	}
	n := 0
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

func (h *Helper) active() bool {
	if h.Active != nil {
		return h.Active()
	}
	return h.run("systemctl", "is-active", "--quiet", h.Unit) == nil
}

// awaitReady waits for version to say in ready.json, after since, that it is
// listening with its database. Going back, the version may be one from
// before ready.json existed, so staying up unrestarted for SteadyFor counts
// too.
func (h *Helper) awaitReady(version string, since time.Time, orSteady bool) error {
	deadline := time.Now().Add(h.ReadyWait)
	base := h.restarts()
	var last Ready
	upSince := time.Time{}
	for time.Now().Before(deadline) {
		if n := h.restarts() - base; n >= crashLoop {
			return fmt.Errorf("exited %d times as it started", n)
		}
		if r, ok := ReadReady(h.Dir); ok && r.At.After(since.Add(-time.Second)) {
			last = r
			if r.Version == version && r.Listening && r.Database {
				return nil
			}
		}
		if orSteady {
			switch {
			case !h.active():
				upSince = time.Time{}
			case upSince.IsZero():
				upSince = time.Now()
			case time.Since(upSince) >= h.SteadyFor && h.restarts() == base:
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	switch {
	case last.Version == "":
		return fmt.Errorf("did not start within %s", h.ReadyWait)
	case last.Version != version:
		return fmt.Errorf("did not start within %s (%s answered)", h.ReadyWait, last.Version)
	case !last.Database:
		return fmt.Errorf("started but could not use its database within %s", h.ReadyWait)
	}
	return fmt.Errorf("started but was not listening within %s", h.ReadyWait)
}

/* ── Downloads ───────────────────────────────────────────────────────────── */

// fetchVerified downloads version's package into dir and checks it against
// the release's checksums.
func (h *Helper) fetchVerified(ctx context.Context, base, version, format, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := FileName(version, h.Arch, format)
	release := base + "/v" + version
	sums, err := h.get(ctx, release+"/checksums.txt", 1<<20)
	if err != nil {
		return "", fmt.Errorf("could not download the checksums for %s: %w", version, err)
	}
	want, ok := ChecksumFor(sums, name)
	if !ok {
		return "", fmt.Errorf("release %s lists no %s", version, name)
	}
	path := filepath.Join(dir, name)
	if err := h.download(ctx, release+"/"+name, path); err != nil {
		return "", fmt.Errorf("could not download %s: %w", name, err)
	}
	got, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if got != want {
		_ = os.Remove(path)
		return "", fmt.Errorf("%s does not match its checksum (got %s, the release says %s)",
			name, got[:12], want[:12])
	}
	return path, nil
}

func (h *Helper) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (h *Helper) download(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxPackage+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxPackage {
		err = errors.New("the package is larger than any control plane package")
	}
	return err
}

// ChecksumFor finds name in a sha256sum-style list.
func ChecksumFor(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			if _, err := hex.DecodeString(f[0]); err == nil {
				return strings.ToLower(f[0]), true
			}
		}
	}
	return "", false
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func copyFile(from, to string, mode os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := to + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

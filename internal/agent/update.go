package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

/*
Updating the agent from the console.

An agent update used to be a package upgrade someone ran by hand, usually in
a console terminal on the server. The package's postinstall restarted the
agent ten seconds later, and the restart took everything with it: the
terminal the upgrade was typed into, every other console session on the
server, file transfers and database sessions, the recording of every SSH
login, and -- for the second or two the collector socket was gone -- every
new SSH login, refused.

Now the console asks, on a heartbeat, and the agent does it in an order that
drops nothing it does not have to:

 1. downloading: the release's package and its checksum, verified before
    anything changes. The package for the version running now comes too, so
    there is something to go back to.
 2. waiting: until nothing is open on the server -- no SSH login, no console
    terminal, desktop, file transfer or database session. The operator can
    say "now" instead, and is told how many sessions that ends.
 3. restarting: new logins wait instead of being refused (the shim holds
    them until the new agent answers), the package is installed by a helper
    outside the agent's sandbox, and the agent restarts. The helper watches
    for the new version to answer on the collector socket and reconnect to
    the control plane, and if it does not within readyWait, reinstalls the
    version that was running and restarts that.
 4. done, rolled_back or failed, said on every heartbeat until the console
    has seen it.

The state lives in update.json in the state directory, so it survives the
restart in the middle of it: the new agent reads what the helper wrote.

A restart asked from the console is the same request with Restart set and
nothing to download: it waits and holds logins the same way, and the helper
has systemd restart the unit and waits for this version to come back and
reconnect. There is nothing to go back to, so one that does not come back
is reported as failed.
*/

// Update states, as the console reads them.
const (
	UpdateDownloading = "downloading"
	UpdateWaiting     = "waiting"
	UpdateRestarting  = "restarting"
	UpdateDone        = "done"
	UpdateRolledBack  = "rolled_back"
	UpdateFailed      = "failed"
	UpdateCancelled   = "cancelled"
)

// UpdateRequest is the control plane asking for an update, on a heartbeat
// reply.
type UpdateRequest struct {
	// ID names this request; a status carries it back.
	ID string `json:"id"`
	// Version is the release to install, like 0.9.1.
	Version string `json:"version"`
	// URL is where releases are downloaded from: <URL>/v<version>/<file>.
	URL string `json:"url"`
	// Now means do not wait for open sessions to end.
	Now bool `json:"now"`
	// Restart asks for a restart on the running version, not an update.
	Restart bool `json:"restart,omitempty"`
}

// UpdateStatus is what the agent says about updating itself, on every
// heartbeat.
type UpdateStatus struct {
	// Supported is whether this agent can update itself on this server;
	// Reason says why not.
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Restartable is whether this agent can be restarted from the console;
	// RestartReason says why not.
	Restartable   bool   `json:"restartable,omitempty"`
	RestartReason string `json:"restart_reason,omitempty"`
	// Restart marks a status about a restart rather than an update.
	Restart bool `json:"restart,omitempty"`

	ID      string `json:"id,omitempty"`
	From    string `json:"from,omitempty"`
	Target  string `json:"target,omitempty"`
	State   string `json:"state,omitempty"`
	Message string `json:"message,omitempty"`
	// Sessions is how many sessions the update is waiting for.
	Sessions int       `json:"sessions,omitempty"`
	At       time.Time `json:"at,omitempty"`
}

// Finished reports whether the update has reached an end.
func (s UpdateStatus) Finished() bool {
	switch s.State {
	case UpdateDone, UpdateRolledBack, UpdateFailed, UpdateCancelled:
		return true
	}
	return false
}

// UpdatePlan is what the helper is handed: everything it needs to install
// the new version, check it, and go back.
type UpdatePlan struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	Target string `json:"target"`
	// Format is the package manager's: deb, rpm or apk.
	Format string `json:"format"`
	// Package is the verified package for Target.
	Package string `json:"package"`
	// Rollback is the verified package for From, empty when it could not be
	// had (a build from source has no release to download).
	Rollback string `json:"rollback,omitempty"`
	// Previous is a copy of the binary that was running, for when there is
	// no rollback package.
	Previous string `json:"previous"`
	// Executable is where the package puts the agent.
	Executable string `json:"executable"`
	// Unit is the agent's systemd unit.
	Unit string `json:"unit"`
	// StateDir holds update.json and ready.json.
	StateDir string `json:"state_dir"`
	// Marker tells logins arriving during the restart to wait for it.
	Marker string `json:"marker"`
	// NeedTunnel is whether the new agent must reconnect to the control
	// plane before it counts as working.
	NeedTunnel bool `json:"need_tunnel"`
	// Restart is a restart on From: nothing to install or go back to.
	Restart bool `json:"restart,omitempty"`
}

// AgentReady is what a running agent writes once it is serving: the helper
// reads it to know the new version came up.
type AgentReady struct {
	Version string    `json:"version"`
	PID     int       `json:"pid"`
	At      time.Time `json:"at"`
	// Listening is the collector socket answering, so SSH logins work.
	Listening bool `json:"listening"`
	// Tunnel is the connection to the control plane being up, so console
	// sessions work.
	Tunnel bool `json:"tunnel"`
}

const (
	updateStateFile = "update.json"
	readyFile       = "ready.json"
	updateDir       = "update"

	// readyWait is how long the new version has to answer and reconnect
	// before the helper puts the old one back.
	readyWait = 90 * time.Second
	// helperLost is how long a restart may say nothing before the agent
	// gives up on the helper that was meant to finish it.
	helperLost = 5 * time.Minute
	// busyPoll is how often a waiting update looks at what is open.
	busyPoll = 2 * time.Second
	// maxPackage bounds a download. The agent package is a few tens of MB.
	maxPackage = 512 << 20
)

// UpdateMarkerName sits next to the collector socket while the agent
// restarts for an update. A login that finds it waits for the new agent
// rather than being refused.
const UpdateMarkerName = "updating"

// updateMarkerFresh is how old the marker may be and still be believed: a
// helper that crashed must not leave every login waiting forever.
const updateMarkerFresh = 3 * time.Minute

var reReleaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// updatePlatform is the part of updating that depends on the host.
type updatePlatform interface {
	// check says why this host cannot update its agent from the console,
	// or "" when it can.
	check() string
	// restartCheck says why this host cannot restart its agent from the
	// console, or "" when it can. A restart needs no package.
	restartCheck() string
	// format is the package manager the agent was installed with.
	format() string
	// fileName is the release file holding version for this host.
	fileName(version string) string
	// launch starts the helper that carries out the plan at planPath. It
	// must outlive the agent, which it restarts.
	launch(ctx context.Context, planPath string) error
}

// archiveInstall is a host whose release file is an archive holding the
// agent, rather than a package that installs it: Windows, where the agent
// is one executable the service runs. unpack takes the agent out of the
// verified archive into dir, before anything stops, so an archive without
// one is found while the old version is still serving. The binary is then
// the whole install, so the copy of the running one is all a rollback needs
// and the running version's archive is not downloaded.
type archiveInstall interface {
	unpack(archive, dir string) (string, error)
}

// Updater carries out update requests.
type Updater struct {
	StateDir string
	// Version is the agent running now.
	Version string
	// Executable is the running binary; Unit its service.
	Executable string
	Unit       string
	// Marker is the path of the restart marker next to the socket.
	Marker string
	// NeedTunnel is whether this agent holds a tunnel the new one must
	// bring back.
	NeedTunnel bool
	// Busy counts what is open on this server: SSH logins and every stream
	// through the tunnel.
	Busy func() int
	// Hold makes new sessions wait (true) or go ahead again (false), for
	// the moment between deciding to restart and restarting.
	Hold func(bool)
	// Changed is called when the status changes, so it reaches the console
	// without waiting for the next heartbeat.
	Changed func()
	HTTP    *http.Client
	Log     *slog.Logger

	platform updatePlatform

	mu      sync.Mutex
	status  UpdateStatus
	loaded  bool
	running string // the id being worked on, "" when idle
	now     bool
	cancel  context.CancelFunc
}

// NewUpdater builds an updater for this host.
func NewUpdater(stateDir, version, executable string, log *slog.Logger) *Updater {
	if log == nil {
		log = slog.Default()
	}
	return &Updater{
		StateDir: stateDir, Version: version, Executable: executable,
		Unit: "argus-agent", Log: log, platform: hostUpdatePlatform(executable),
		HTTP: &http.Client{
			Timeout:   10 * time.Minute,
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
		},
	}
}

// Status is what the next heartbeat says.
func (u *Updater) Status() UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.loadLocked()
	if u.status.State == UpdateRestarting && u.running == "" {
		// The helper writes the outcome; this agent may be the new version
		// it is checking, or the old one put back.
		if s, ok := u.readFile(); ok {
			u.status = s
		}
		if u.status.State == UpdateRestarting && time.Since(u.status.At) > helperLost {
			u.status.State = UpdateFailed
			u.status.Message = "the update helper stopped without saying how it went; " +
				"this server is running " + u.Version
			u.status.At = time.Now().UTC()
			u.saveLocked()
		}
	}
	s := u.status
	if reason := u.platform.check(); reason != "" {
		s.Supported, s.Reason = false, reason
	} else {
		s.Supported, s.Reason = true, ""
	}
	if reason := u.platform.restartCheck(); reason != "" {
		s.Restartable, s.RestartReason = false, reason
	} else {
		s.Restartable, s.RestartReason = true, ""
	}
	return s
}

// Handle acts on what the control plane said on a heartbeat: req is nil
// when it asked for nothing.
func (u *Updater) Handle(ctx context.Context, req *UpdateRequest) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.loadLocked()

	if req == nil {
		// Taken back while it could still be. Once the restart has begun it
		// finishes: stopping half way is worse than either end.
		if u.running != "" && (u.status.State == UpdateDownloading || u.status.State == UpdateWaiting) {
			u.cancel()
		}
		return
	}
	if u.running == req.ID {
		if req.Now && !u.now {
			u.Log.Warn("update: asked to go ahead without waiting for open sessions")
		}
		u.now = u.now || req.Now
		return
	}
	if u.status.ID == req.ID && (u.status.Finished() || u.status.State == UpdateRestarting) {
		// Already answered, or the helper is finishing it.
		return
	}
	if u.running != "" {
		// A newer request replaces one still waiting.
		u.cancel()
	}

	if req.Restart {
		base := UpdateStatus{ID: req.ID, From: u.Version, Target: u.Version, Restart: true,
			At: time.Now().UTC()}
		if reason := u.platform.restartCheck(); reason != "" {
			base.State, base.Message = UpdateFailed, reason
			u.setLocked(base)
			return
		}
		base.State, base.Message = UpdateWaiting, "getting ready to restart"
		u.setLocked(base)
		rctx, cancel := context.WithCancel(ctx)
		u.running, u.now, u.cancel = req.ID, req.Now, cancel
		go u.run(rctx, *req)
		return
	}

	base := UpdateStatus{ID: req.ID, From: u.Version, Target: req.Version, At: time.Now().UTC()}
	switch {
	case req.Version == u.Version:
		base.State, base.Message = UpdateDone, "already running "+u.Version
		u.setLocked(base)
		return
	case !reReleaseVersion.MatchString(req.Version):
		base.State, base.Message = UpdateFailed, fmt.Sprintf("%q is not a release version", req.Version)
		u.setLocked(base)
		return
	}
	if reason := u.platform.check(); reason != "" {
		base.State, base.Message = UpdateFailed, reason
		u.setLocked(base)
		return
	}
	base.State, base.Message = UpdateDownloading, "downloading "+req.Version
	u.setLocked(base)

	rctx, cancel := context.WithCancel(ctx)
	u.running, u.now, u.cancel = req.ID, req.Now, cancel
	go u.run(rctx, *req)
}

// set records a status change and says so.
func (u *Updater) set(s UpdateStatus) {
	u.mu.Lock()
	u.setLocked(s)
	u.mu.Unlock()
}

func (u *Updater) setLocked(s UpdateStatus) {
	s.At = time.Now().UTC()
	changed := s.State != u.status.State || s.Sessions != u.status.Sessions || s.ID != u.status.ID
	u.status = s
	u.saveLocked()
	if changed && u.Changed != nil {
		go u.Changed()
	}
}

func (u *Updater) run(ctx context.Context, req UpdateRequest) {
	st := UpdateStatus{ID: req.ID, From: u.Version, Target: req.Version, Restart: req.Restart}
	if req.Restart {
		st.Target = u.Version
	}
	finish := func(state, msg string) {
		st.State, st.Message, st.Sessions = state, msg, 0
		u.mu.Lock()
		u.running = ""
		u.setLocked(st)
		u.mu.Unlock()
		if state == UpdateFailed {
			u.Log.Error("update: "+msg, "target", req.Version)
		} else {
			u.Log.Info("update: "+msg, "target", req.Version)
		}
	}

	prepare := u.prepare
	if req.Restart {
		prepare = u.prepareRestart
	}
	plan, err := prepare(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			finish(UpdateCancelled, "taken back in the console before it started")
			return
		}
		finish(UpdateFailed, err.Error())
		return
	}

	for {
		// Wait for the server to be quiet, unless told not to.
		if err := u.waitIdle(ctx, &st); err != nil {
			finish(UpdateCancelled, "taken back in the console before it started")
			return
		}
		// Hold new sessions, then look again: a login may have arrived
		// between the last look and the hold.
		if u.Hold != nil {
			u.Hold(true)
		}
		if u.nowAsked() || u.Busy == nil || u.Busy() == 0 {
			break
		}
		if u.Hold != nil {
			u.Hold(false)
		}
	}

	open := 0
	if u.Busy != nil {
		open = u.Busy()
	}
	st.State, st.Sessions = UpdateRestarting, 0
	st.Message = "restarting on " + req.Version
	if req.Restart {
		st.Message = "restarting"
	}
	if open > 0 {
		st.Message += fmt.Sprintf("; ending %d open session(s) as asked", open)
	}
	// Written before the helper starts: it takes over the file from here.
	u.mu.Lock()
	u.running = ""
	u.setLocked(st)
	u.mu.Unlock()
	if u.Marker != "" {
		_ = os.WriteFile(u.Marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	}

	planPath := filepath.Join(u.StateDir, updateDir, "plan.json")
	err = writeFileAtomic(planPath, mustJSON(plan), 0o600)
	if err == nil {
		err = u.platform.launch(context.WithoutCancel(ctx), planPath)
	}
	if err != nil {
		if u.Hold != nil {
			u.Hold(false)
		}
		if u.Marker != "" {
			_ = os.Remove(u.Marker)
		}
		u.mu.Lock()
		st.State, st.Message = UpdateFailed, "could not start the update: "+err.Error()
		u.setLocked(st)
		u.mu.Unlock()
		u.Log.Error("update: " + st.Message)
		return
	}
	if req.Restart {
		u.Log.Warn("restart: restarting as asked from the console", "version", u.Version)
		return
	}
	u.Log.Warn("update: restarting on the new version", "from", u.Version, "to", req.Version)
}

// Watch reports the helper's outcome as soon as it writes it. The agent the
// helper is waiting on is this one, started on the new version; without this
// the console would hear "done" only at the next heartbeat, half a minute
// after the update finished.
func (u *Updater) Watch(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		u.mu.Lock()
		u.loadLocked()
		waiting := u.status.State == UpdateRestarting && u.running == ""
		var changed bool
		if waiting {
			if s, ok := u.readFile(); ok && (s.State != u.status.State || s.Message != u.status.Message) {
				u.status, changed = s, true
			}
		}
		u.mu.Unlock()
		if changed && u.Changed != nil {
			go u.Changed()
		}
	}
}

// Restarting reports whether this agent is stopping because an update
// restarts it. Its shutdown need not wait for sessions then: the update waited
// for them already, or was told not to, and an SSH login it disconnects
// carries on recording under the agent that replaces it.
func (u *Updater) Restarting() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status.State == UpdateRestarting && u.running == "" &&
		time.Since(u.status.At) < helperLost
}

func (u *Updater) nowAsked() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.now
}

// waitIdle returns once nothing is open, or "now" was asked.
func (u *Updater) waitIdle(ctx context.Context, st *UpdateStatus) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n := 0
		if u.Busy != nil {
			n = u.Busy()
		}
		if n == 0 || u.nowAsked() {
			return nil
		}
		if st.State != UpdateWaiting || st.Sessions != n {
			st.State, st.Sessions = UpdateWaiting, n
			st.Message = fmt.Sprintf("downloaded %s; waiting for %d open session(s) to end", st.Target, n)
			if st.Restart {
				st.Message = fmt.Sprintf("waiting for %d open session(s) to end before restarting", n)
			}
			u.set(*st)
		}
		t := time.NewTimer(busyPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// prepare downloads and verifies what the helper will install.
func (u *Updater) prepare(ctx context.Context, req UpdateRequest) (UpdatePlan, error) {
	dir := filepath.Join(u.StateDir, updateDir)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return UpdatePlan{}, fmt.Errorf("cannot make %s: %w", dir, err)
	}
	base := strings.TrimRight(req.URL, "/")
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return UpdatePlan{}, fmt.Errorf("the release address %q is not http(s)", req.URL)
	}

	plan := UpdatePlan{
		ID: req.ID, From: u.Version, Target: req.Version, Format: u.platform.format(),
		Executable: u.Executable, Unit: u.Unit, StateDir: u.StateDir, Marker: u.Marker,
		NeedTunnel: u.NeedTunnel,
	}
	pkg, err := u.fetchVerified(ctx, base, req.Version, dir)
	if err != nil {
		return UpdatePlan{}, err
	}
	plan.Package = pkg
	archive, isArchive := u.platform.(archiveInstall)
	if isArchive {
		bin, err := archive.unpack(pkg, dir)
		_ = os.Remove(pkg)
		if err != nil {
			return UpdatePlan{}, fmt.Errorf("the release for %s: %w; nothing was changed", req.Version, err)
		}
		plan.Package = bin
	}

	// Something to go back to. A build from source has no release, and a
	// release that cannot be had now still has the binary copied below.
	if reReleaseVersion.MatchString(u.Version) && !isArchive {
		if old, err := u.fetchVerified(ctx, base, u.Version, filepath.Join(dir, "previous")); err == nil {
			plan.Rollback = old
		} else if ctx.Err() == nil {
			u.Log.Warn("update: the running version's package could not be downloaded; "+
				"going back would restore the binary alone", "error", err)
		}
	}
	plan.Previous = filepath.Join(dir, "argus-agent.previous"+filepath.Ext(u.Executable))
	if err := copyFile(u.Executable, plan.Previous, 0o755); err != nil {
		return UpdatePlan{}, fmt.Errorf("cannot keep a copy of the running agent: %w", err)
	}
	return plan, ctx.Err()
}

// prepareRestart is the plan for a restart: nothing to download, and
// nothing to go back to.
func (u *Updater) prepareRestart(ctx context.Context, req UpdateRequest) (UpdatePlan, error) {
	dir := filepath.Join(u.StateDir, updateDir)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return UpdatePlan{}, fmt.Errorf("cannot make %s: %w", dir, err)
	}
	return UpdatePlan{
		ID: req.ID, From: u.Version, Target: u.Version, Restart: true,
		Executable: u.Executable, Unit: u.Unit, StateDir: u.StateDir, Marker: u.Marker,
		NeedTunnel: u.NeedTunnel,
	}, ctx.Err()
}

// fetchVerified downloads version's file for this host into dir and checks
// it against the release's checksums.
func (u *Updater) fetchVerified(ctx context.Context, base, version, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := u.platform.fileName(version)
	release := base + "/v" + version

	sums, err := u.get(ctx, release+"/checksums.txt", 1<<20)
	if err != nil {
		return "", fmt.Errorf("could not download the checksums for %s: %w", version, err)
	}
	want, ok := checksumFor(sums, name)
	if !ok {
		return "", fmt.Errorf("release %s lists no %s; is there a package for this server?", version, name)
	}

	path := filepath.Join(dir, name)
	if err := u.download(ctx, release+"/"+name, path); err != nil {
		return "", fmt.Errorf("could not download %s: %w", name, err)
	}
	got, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if got != want {
		_ = os.Remove(path)
		return "", fmt.Errorf("%s does not match its checksum (got %s, the release says %s); "+
			"nothing was changed", name, got[:12], want[:12])
	}
	return path, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (u *Updater) download(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.HTTP.Do(req)
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
		err = errors.New("the package is larger than any agent package")
	}
	return err
}

// checksumFor finds name in a sha256sum-style list.
func checksumFor(sums []byte, name string) (string, bool) {
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
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

// awaitAgentReady waits until version says in ready.json, after since, that
// it is serving: taking logins, and back on the tunnel when plan needs it.
// crashes says how many times the service has exited on its own since the
// wait began; crashLoop of them ends the wait early, because a binary that
// cannot start would otherwise keep logins waiting for all of wait.
func awaitAgentReady(plan UpdatePlan, version string, since time.Time, wait time.Duration,
	crashes func() int) error {
	deadline := time.Now().Add(wait)
	var last AgentReady
	for time.Now().Before(deadline) {
		if n := crashes(); n >= crashLoop {
			return fmt.Errorf("exited %d times as it started", n)
		}
		if r, ok := ReadReady(plan.StateDir); ok && r.At.After(since.Add(-time.Second)) {
			last = r
			if r.Version == version && r.Listening && (r.Tunnel || !plan.NeedTunnel) {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	switch {
	case last.Version == "":
		return fmt.Errorf("did not start within %s", wait)
	case last.Version != version:
		return fmt.Errorf("did not start within %s (%s is running)", wait, last.Version)
	case !last.Listening:
		return fmt.Errorf("started but did not take logins within %s", wait)
	}
	return fmt.Errorf("started but did not reconnect to Argus within %s", wait)
}

// crashLoop is how many times a new version may exit on its own before the
// helper stops waiting for it.
const crashLoop = 3

/* ── update.json ─────────────────────────────────────────────────────────── */

func (u *Updater) loadLocked() {
	if u.loaded {
		return
	}
	u.loaded = true
	if s, ok := u.readFile(); ok {
		u.status = s
	}
}

func (u *Updater) readFile() (UpdateStatus, bool) {
	return ReadUpdateStatus(u.StateDir)
}

func (u *Updater) saveLocked() {
	if err := WriteUpdateStatus(u.StateDir, u.status); err != nil {
		u.Log.Error("update: cannot keep the update's state", "error", err)
	}
}

// ReadUpdateStatus reads update.json from stateDir.
func ReadUpdateStatus(stateDir string) (UpdateStatus, bool) {
	b, err := os.ReadFile(filepath.Join(stateDir, updateStateFile))
	if err != nil {
		return UpdateStatus{}, false
	}
	var s UpdateStatus
	if json.Unmarshal(b, &s) != nil {
		return UpdateStatus{}, false
	}
	return s, true
}

// WriteUpdateStatus replaces update.json in stateDir.
func WriteUpdateStatus(stateDir string, s UpdateStatus) error {
	s.Supported, s.Reason = false, ""
	s.Restartable, s.RestartReason = false, ""
	return writeFileAtomic(filepath.Join(stateDir, updateStateFile), mustJSON(s), 0o600)
}

// WriteReady says this agent is serving, for an update helper watching for
// it. Best effort: an agent that cannot write it is one the helper will put
// back, which is the right answer for an agent that cannot write its state.
func WriteReady(stateDir, version string, listening, tunnel bool) {
	r := AgentReady{Version: version, PID: os.Getpid(), At: time.Now().UTC(),
		Listening: listening, Tunnel: tunnel}
	_ = writeFileAtomic(filepath.Join(stateDir, readyFile), mustJSON(r), 0o600)
}

// ReadReady reads what the running agent last said about itself.
func ReadReady(stateDir string) (AgentReady, bool) {
	b, err := os.ReadFile(filepath.Join(stateDir, readyFile))
	if err != nil {
		return AgentReady{}, false
	}
	var r AgentReady
	return r, json.Unmarshal(b, &r) == nil
}

// UpdateMarkerPath is where the restart marker goes for a socket.
func UpdateMarkerPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), UpdateMarkerName)
}

// updateMarkerSet reports whether an update restart is under way, by the
// marker next to the socket.
func updateMarkerSet(socket string) bool {
	fi, err := os.Stat(UpdateMarkerPath(socket))
	return err == nil && time.Since(fi.ModTime()) < updateMarkerFresh
}

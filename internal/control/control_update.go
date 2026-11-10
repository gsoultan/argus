package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/controlupdate"
)

/*
Updating the control plane from the console.

Settings › Control plane › Updates. The control plane cannot install a
package -- it runs as argus with no privileges -- so it writes a request into
its own state directory and a root helper unit picks it up (see
internal/controlupdate). What this side does:

  - says which release is newest, and whether going back from it would be
    clean: the release lists its migrations, and any it would add that take
    something away (migration_manifest.go) mean going back is a restore;
  - says which open sessions would end: a terminal on an agent that holds
    shells pauses and resumes, anything else ends with the restart;
  - "when nothing would end" waits for those, up to updateWaitFor, then gives
    up rather than restarting under someone;
  - after the restart, the new version (or the old one, gone back to) records
    the outcome in the audit chain once, and says it is ready for the helper.

One replica at a time: the request goes to the helper on the host that
answered. Each replica is updated from its own console address.
*/

// updateWaitFor is how long "when nothing would end" waits.
const updateWaitFor = 2 * time.Hour

// helperLate is how long a request may sit unanswered before the console
// says the helper is not running.
const helperLate = 2 * time.Minute

// ControlUpdater is this control plane's half of updating itself.
type ControlUpdater struct {
	// Dir is the update directory shared with the helper.
	Dir string
	// ReleasesURL is where release files are read from; empty is
	// DefaultAgentReleasesURL.
	ReleasesURL string
	// Unsupported says why this install cannot update itself; empty when it
	// can.
	Unsupported string
	HTTP        *http.Client
	Log         *slog.Logger

	mu        sync.Mutex
	waiting   context.CancelFunc
	manifests map[string]manifestEntry
}

type manifestEntry struct {
	list []MigrationInfo
	err  error
	at   time.Time
}

// ControlUpdateSupport says why the control plane running here cannot update
// itself from the console, or "" when it can: Linux, run by systemd, with the
// helper's units installed by the package, and a state directory to write.
func ControlUpdateSupport(dir string) string {
	switch {
	case runtime.GOOS != "linux":
		return "updating from the console needs Linux with systemd; upgrade this control plane by hand"
	case os.Getenv("INVOCATION_ID") == "":
		return "this control plane is not run by systemd, which the update needs to restart it; " +
			"upgrade it by hand"
	}
	found := false
	for _, d := range []string{"/lib/systemd/system", "/usr/lib/systemd/system", "/etc/systemd/system"} {
		if _, err := os.Stat(d + "/argus-control-update.path"); err == nil {
			found = true
			break
		}
	}
	if !found {
		return "the update helper is not installed: argus-control was not installed from its " +
			"package, or from one older than this version. Upgrade the package by hand once"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "cannot write " + dir + ": " + err.Error()
	}
	return ""
}

func (u *ControlUpdater) log() *slog.Logger {
	if u.Log != nil {
		return u.Log
	}
	return slog.Default()
}

func (u *ControlUpdater) releasesURL() string {
	if u.ReleasesURL != "" {
		return strings.TrimRight(u.ReleasesURL, "/")
	}
	return DefaultAgentReleasesURL
}

// manifest is the migration list release version publishes.
func (u *ControlUpdater) manifest(ctx context.Context, version string) ([]MigrationInfo, error) {
	u.mu.Lock()
	if e, ok := u.manifests[version]; ok && (e.err == nil || time.Since(e.at) < 10*time.Minute) {
		u.mu.Unlock()
		return e.list, e.err
	}
	u.mu.Unlock()

	list, err := u.fetchManifest(ctx, version)
	u.mu.Lock()
	if u.manifests == nil {
		u.manifests = map[string]manifestEntry{}
	}
	u.manifests[version] = manifestEntry{list: list, err: err, at: time.Now()}
	u.mu.Unlock()
	return list, err
}

// ManifestName is the file a release publishes its migrations in.
func ManifestName(version string) string {
	return "argus-control_" + version + "_migrations.json"
}

func (u *ControlUpdater) fetchManifest(ctx context.Context, version string) ([]MigrationInfo, error) {
	client := u.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := u.releasesURL() + "/v" + version + "/" + ManifestName(version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	var out struct {
		Migrations []MigrationInfo `json:"migrations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	return out.Migrations, nil
}

/* ── What the console shows ──────────────────────────────────────────────── */

// ControlRollback says whether going back from a release is clean.
type ControlRollback struct {
	// Known is false when the release's migration list could not be read.
	Known  bool   `json:"known"`
	Reason string `json:"reason,omitempty"`
	// Added is how many migrations the release would apply.
	Added int `json:"added"`
	// Breaking are those that take something away.
	Breaking []MigrationInfo `json:"breaking"`
}

// ControlUpdateSessions is what the restart would do to open sessions on this
// replica.
type ControlUpdateSessions struct {
	// Held terminals pause and pick up where they were.
	Held int `json:"held"`
	// Ending end with the restart, by kind.
	Ending    int `json:"ending"`
	Terminals int `json:"terminals"`
	Desktops  int `json:"desktops"`
	Databases int `json:"databases"`
}

// ControlUpdateView is the Updates page.
type ControlUpdateView struct {
	Running   string        `json:"running"`
	Hostname  string        `json:"hostname"`
	Supported bool          `json:"supported"`
	Reason    string        `json:"reason,omitempty"`
	Newest    *AgentRelease `json:"newest"`
	Newer     bool          `json:"newer"`
	// ReleaseChecks is whether this control plane looks for releases at all,
	// and so whether the console can ask it to look now.
	ReleaseChecks bool                  `json:"releaseChecks"`
	CheckedAt     *time.Time            `json:"checkedAt,omitempty"`
	CheckFailure  string                `json:"checkFailure,omitempty"`
	Rollback      *ControlRollback      `json:"rollback,omitempty"`
	Sessions      ControlUpdateSessions `json:"sessions"`
	Update        *controlupdate.Status `json:"update"`
	HelperLate    bool                  `json:"helperLate,omitempty"`
}

func (a *API) updateSessions() ControlUpdateSessions {
	var out ControlUpdateSessions
	a.live.mu.Lock()
	defer a.live.mu.Unlock()
	for _, s := range a.live.m {
		switch {
		case s.term != nil && s.term.agentHolds:
			out.Held++
		case s.protocol == "ssh":
			out.Terminals++
		case s.protocol == "rdp":
			out.Desktops++
		default:
			out.Databases++
		}
	}
	out.Ending = out.Terminals + out.Desktops + out.Databases
	return out
}

func (a *API) controlUpdateView(ctx context.Context) ControlUpdateView {
	v := ControlUpdateView{Running: a.Version, Sessions: a.updateSessions()}
	v.Hostname, _ = os.Hostname()
	u := a.Updates
	if u == nil {
		v.Reason = "updating from the console is not set up on this control plane"
		return v
	}
	v.Supported, v.Reason = u.Unsupported == "", u.Unsupported
	if st, ok := controlupdate.ReadStatus(u.Dir); ok {
		v.Update = &st
		v.HelperLate = st.State == controlupdate.StateRequested && time.Since(st.At) > helperLate
	}
	if a.Releases != nil {
		v.ReleaseChecks = true
		_, checked, failure := a.Releases.Latest()
		if !checked.IsZero() {
			v.CheckedAt = &checked
		}
		v.CheckFailure = failure
		v.Newest = a.Releases.LatestControl()
	}
	running := strings.TrimPrefix(a.Version, "v")
	if v.Newest != nil && reAgentVer.MatchString(running) && versionLess(running, v.Newest.Version) {
		v.Newer = true
		v.Rollback = a.rollbackFor(ctx, v.Newest.Version)
	}
	return v
}

// rollbackFor compares the migrations version would apply with this one's.
func (a *API) rollbackFor(ctx context.Context, version string) *ControlRollback {
	theirs, err := a.Updates.manifest(ctx, version)
	if err != nil {
		return &ControlRollback{Reason: "the release's list of migrations could not be read (" +
			err.Error() + "), so going back may need the backup"}
	}
	ours, err := MigrationManifest()
	if err != nil {
		return &ControlRollback{Reason: err.Error()}
	}
	have := map[string]bool{}
	for _, m := range ours {
		have[m.Name] = true
	}
	out := &ControlRollback{Known: true, Breaking: []MigrationInfo{}}
	for _, m := range theirs {
		if have[m.Name] {
			continue
		}
		out.Added++
		if m.Breaking {
			out.Breaking = append(out.Breaking, m)
		}
	}
	return out
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) getControlUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.controlUpdateView(r.Context()))
}

// postControlUpdateCheck asks for the newest release now, rather than at the
// watcher's next turn, and answers with the Updates page as it then stands.
// A check that fails is not a refusal: the page says why, as it does for the
// watcher's own checks.
func (a *API) postControlUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	if a.Releases == nil {
		writeErr(w, http.StatusConflict, "release checks are off on this control plane "+
			"(agent_release_check is false, or agent_releases_url names a mirror without agent_releases_api)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.Releases.CheckNow(ctx); err != nil {
		a.log.Warn("checking for a newer release, asked from the console", "error", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.controlUpdateView(r.Context()))
}

// getControlUpdateBanner is what every signed-in person's console asks, to
// show that an update is under way: only the state and the version.
func (a *API) getControlUpdateBanner(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(r); !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	out := map[string]any{"active": false}
	if a.Updates != nil {
		if st, ok := controlupdate.ReadStatus(a.Updates.Dir); ok && !controlupdate.Finished(st.State) &&
			time.Since(st.At) < 3*time.Hour {
			out = map[string]any{"active": true, "state": st.State, "target": st.Target}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type controlUpdateAsk struct {
	Version string `json:"version"`
	// When is "idle" (once nothing open would end) or "now".
	When string `json:"when"`
}

func (a *API) postControlUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	u := a.Updates
	if u == nil || u.Unsupported != "" {
		reason := "updating from the console is not set up on this control plane"
		if u != nil {
			reason = u.Unsupported
		}
		writeErr(w, http.StatusConflict, reason)
		return
	}
	var in controlUpdateAsk
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if in.When != "idle" && in.When != "now" {
		writeErr(w, http.StatusBadRequest, `when is "idle" or "now"`)
		return
	}
	// Only the newest release found, and only forward: the console never
	// installs a version someone typed.
	newest := (*AgentRelease)(nil)
	if a.Releases != nil {
		newest = a.Releases.LatestControl()
	}
	running := strings.TrimPrefix(a.Version, "v")
	switch {
	case newest == nil:
		writeErr(w, http.StatusConflict, "no newer release has been found")
		return
	case in.Version != newest.Version:
		writeErr(w, http.StatusConflict, "the newest release is "+newest.Version)
		return
	case !reAgentVer.MatchString(running) || !versionLess(running, newest.Version):
		writeErr(w, http.StatusConflict, "this control plane already runs "+a.Version)
		return
	}
	if st, ok := controlupdate.ReadStatus(u.Dir); ok && !controlupdate.Finished(st.State) &&
		time.Since(st.At) < 3*time.Hour {
		writeErr(w, http.StatusConflict, "an update to "+st.Target+" is already under way")
		return
	}

	id, err := newUpdateID()
	if err != nil {
		a.fail(w, "control update", err)
		return
	}
	req := controlupdate.Request{ID: id, Version: in.Version, From: running,
		RequestedBy: sess.Email, At: time.Now().UTC()}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "control.update_requested", Severity: "notice", ActorEmail: sess.Email,
		Target: "argus-control",
		Detail: fmt.Sprintf("%s asked the control plane to update from %s to %s (%s).", sess.Email,
			running, in.Version, map[string]string{"idle": "once nothing open would end", "now": "now"}[in.When]),
	}); err != nil {
		a.fail(w, "control update", err)
		return
	}

	if in.When == "now" || a.updateSessions().Ending == 0 {
		if err := u.hand(req); err != nil {
			a.fail(w, "control update", err)
			return
		}
	} else {
		u.wait(req, a.updateSessions)
	}
	st, _ := controlupdate.ReadStatus(u.Dir)
	writeJSON(w, http.StatusAccepted, st)
}

// deleteControlUpdate stops an update that is still waiting for sessions.
// Once the helper has it, it runs to the end.
func (a *API) deleteControlUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	u := a.Updates
	if u == nil {
		writeErr(w, http.StatusNotFound, "no update is waiting")
		return
	}
	st, ok := u.cancel("cancelled by " + sess.Email)
	if !ok {
		writeErr(w, http.StatusConflict, "no update is waiting; one already handed over runs to the end")
		return
	}
	a.auditControlUpdate(r.Context(), st)
	writeJSON(w, http.StatusOK, st)
}

func newUpdateID() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func orSomeone(s string) string {
	if s == "" {
		return "someone"
	}
	return s
}

/* ── Waiting and handing over ────────────────────────────────────────────── */

func (u *ControlUpdater) status(req controlupdate.Request, state, msg string) controlupdate.Status {
	now := time.Now().UTC()
	st := controlupdate.Status{ID: req.ID, From: req.From, Target: req.Version,
		RequestedBy: req.RequestedBy, State: state, Message: msg, At: now}
	if prev, ok := controlupdate.ReadStatus(u.Dir); ok && prev.ID == req.ID {
		st.Steps = prev.Steps
	}
	st.Steps = append(st.Steps, controlupdate.Step{State: state, Message: msg, At: now})
	if err := controlupdate.WriteStatus(u.Dir, st); err != nil {
		u.log().Error("control update: cannot write the status", "error", err)
	}
	return st
}

// hand gives the request to the helper.
func (u *ControlUpdater) hand(req controlupdate.Request) error {
	u.status(req, controlupdate.StateRequested, "handed to the update helper")
	if err := controlupdate.WriteRequest(u.Dir, req); err != nil {
		u.status(req, controlupdate.StateFailed, "cannot hand the update to the helper: "+err.Error())
		return err
	}
	u.log().Info("control update handed to the helper", "version", req.Version, "by", req.RequestedBy)
	return nil
}

// wait hands the request over once nothing open would end.
func (u *ControlUpdater) wait(req controlupdate.Request, sessions func() ControlUpdateSessions) {
	ctx, cancel := context.WithTimeout(context.Background(), updateWaitFor)
	u.mu.Lock()
	if u.waiting != nil {
		u.waiting()
	}
	u.waiting = cancel
	u.mu.Unlock()

	n := sessions().Ending
	u.status(req, controlupdate.StateWaiting, waitingMessage(n))
	go func() {
		defer cancel()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		last := n
		for {
			select {
			case <-ctx.Done():
				u.mu.Lock()
				mine := u.waiting != nil
				u.waiting = nil
				u.mu.Unlock()
				if errors.Is(ctx.Err(), context.DeadlineExceeded) && mine {
					u.status(req, controlupdate.StateCancelled, fmt.Sprintf(
						"%d sessions were still open after %s, so nothing was changed; update again, "+
							"or choose Now", last, updateWaitFor))
				}
				return
			case <-t.C:
			}
			n := sessions().Ending
			if n == 0 {
				u.mu.Lock()
				u.waiting = nil
				u.mu.Unlock()
				_ = u.hand(req)
				return
			}
			if n != last {
				last = n
				u.status(req, controlupdate.StateWaiting, waitingMessage(n))
			}
		}
	}()
}

func waitingMessage(n int) string {
	if n == 1 {
		return "waiting for 1 session that would end with the restart"
	}
	return fmt.Sprintf("waiting for %d sessions that would end with the restart", n)
}

func (u *ControlUpdater) cancel(why string) (controlupdate.Status, bool) {
	u.mu.Lock()
	stop := u.waiting
	u.waiting = nil
	u.mu.Unlock()
	if stop == nil {
		return controlupdate.Status{}, false
	}
	stop()
	st, ok := controlupdate.ReadStatus(u.Dir)
	if !ok || st.State != controlupdate.StateWaiting {
		return controlupdate.Status{}, false
	}
	req := controlupdate.Request{ID: st.ID, Version: st.Target, From: st.From, RequestedBy: st.RequestedBy}
	return u.status(req, controlupdate.StateCancelled, why), true
}

/* ── After the restart ───────────────────────────────────────────────────── */

// ControlUpdateStarted is called once this control plane is serving: it tells
// a helper waiting on it, and records how the last update ended.
func (a *API) ControlUpdateStarted(ctx context.Context) {
	u := a.Updates
	if u == nil || u.Dir == "" {
		return
	}
	// The outcome is the platform's, recorded in the default organization's
	// audit log; a strict store refuses a context that names none.
	ctx = PlatformContext(ctx)
	if err := controlupdate.WriteReady(u.Dir, controlupdate.Ready{
		Version: strings.TrimPrefix(a.Version, "v"), PID: os.Getpid(), At: time.Now().UTC(),
		Listening: true, Database: true,
	}); err != nil {
		u.log().Warn("control update: cannot say this version is ready", "error", err)
	}
	// A "waiting" update was this process's and died with the last one.
	if st, ok := controlupdate.ReadStatus(u.Dir); ok && st.State == controlupdate.StateWaiting {
		req := controlupdate.Request{ID: st.ID, Version: st.Target, From: st.From, RequestedBy: st.RequestedBy}
		st = u.status(req, controlupdate.StateCancelled,
			"the control plane restarted while the update waited; nothing was changed")
		a.auditControlUpdate(ctx, st)
		return
	}
	// The helper writes the outcome a moment after the restart it waits on;
	// look again until it has.
	go func() {
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			st, ok := controlupdate.ReadStatus(u.Dir)
			if !ok || st.Audited {
				return
			}
			if controlupdate.Finished(st.State) {
				a.auditControlUpdate(ctx, st)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
}

// auditControlUpdate records how an update ended, once.
func (a *API) auditControlUpdate(ctx context.Context, st controlupdate.Status) {
	if st.Audited || !controlupdate.Finished(st.State) {
		return
	}
	action, severity := "control.updated", "notice"
	switch st.State {
	case controlupdate.StateRolledBack:
		action, severity = "control.update_rolled_back", "warn"
	case controlupdate.StateFailed:
		action, severity = "control.update_failed", "warn"
	case controlupdate.StateCancelled:
		action = "control.update_cancelled"
	}
	detail := fmt.Sprintf("The control plane, asked by %s to update from %s to %s: %s.",
		orSomeone(st.RequestedBy), orUnknown(st.From), st.Target, strings.TrimSuffix(st.Message, "."))
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: action, Severity: severity, Target: "argus-control", Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
		return
	}
	st.Audited = true
	if err := controlupdate.WriteStatus(a.Updates.Dir, st); err != nil {
		a.log.Warn("control update: cannot mark the outcome recorded", "error", err)
	}
	a.log.Info("control plane update finished", "state", st.State, "from", st.From,
		"to", st.Target, "message", st.Message)
}

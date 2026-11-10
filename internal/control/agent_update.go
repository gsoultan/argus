package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Updating an agent from the console.

The console asks; the agent answers on its heartbeat. A request sits on the
agent's credential (update_* on agent_credentials, migration 034) and rides
every heartbeat reply until the agent reports an outcome for it, at which
point it is cleared and audited. What the agent says about updating -- can
it, and how far it has got -- is kept on its row in agents, so the server
page can show progress and the last result.

The agent does the careful part (internal/agent/update.go): it waits until
nothing is open on the server unless told "now", holds new logins through
the restart instead of refusing them, and goes back to the old version if
the new one does not reconnect.

Restarting rides the same request. A restart is a request with
update_restart set (migration 044) and the version the agent already runs:
the agent waits for the server to be free in the same way, holds new logins
through the restart, has systemd restart it, and reports "done" once it is
back and reconnected. There is nothing to go back to, so a restart that does
not come back is reported as failed, with the way in to look.

Nothing new is trusted here. The control plane names a version and a
release address; it could already open a root shell on every enrolled host
through the tunnel, so an update channel gives it no reach it did not have.
The agent checks the package against the release's checksums before it
installs anything.
*/

// DefaultAgentReleasesURL is where agent packages are downloaded from when
// agent_releases_url is not set: <url>/v<version>/<file>.
const DefaultAgentReleasesURL = "https://github.com/gsoultan/argus/releases/download"

// AgentUpdateStatus is what an agent says about updating itself, as it
// sends it. Kept as the agent wrote it; the console reads these fields.
type AgentUpdateStatus struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Restartable is whether the agent can be restarted from the console;
	// RestartReason says why not. An agent older than restarts sends
	// neither, which reads as not restartable.
	Restartable   bool   `json:"restartable,omitempty"`
	RestartReason string `json:"restart_reason,omitempty"`
	// Restart marks a status about a restart rather than an update.
	Restart  bool       `json:"restart,omitempty"`
	ID       string     `json:"id,omitempty"`
	From     string     `json:"from,omitempty"`
	Target   string     `json:"target,omitempty"`
	State    string     `json:"state,omitempty"`
	Message  string     `json:"message,omitempty"`
	Sessions int        `json:"sessions,omitempty"`
	At       *time.Time `json:"at,omitempty"`
}

func (s AgentUpdateStatus) finished() bool {
	switch s.State {
	case "done", "rolled_back", "failed", "cancelled":
		return true
	}
	return false
}

// AgentUpdateRequest is an update the console asked for and the agent has
// not finished.
type AgentUpdateRequest struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Now     bool   `json:"now"`
	// Restart is a restart on Version, the version the agent runs, rather
	// than an update to it.
	Restart     bool      `json:"restart"`
	RequestedAt time.Time `json:"requestedAt"`
	RequestedBy string    `json:"requestedBy"`
}

// AgentUpdateView is the server page's agent card.
type AgentUpdateView struct {
	Hostname string `json:"hostname"`
	// Enrolled is whether a live agent credential exists for the host.
	Enrolled bool `json:"enrolled"`
	// AgentVersion is what the agent last reported running; "" before it
	// has reported.
	AgentVersion   string     `json:"agentVersion"`
	AgentLastSeen  *time.Time `json:"agentLastSeenAt"`
	ActiveSessions int        `json:"activeSessions"`
	// Latest is the version an agent is offered: the newest agent release
	// the console found, or its own version when that is newer. "" for a
	// build from source that found no release.
	Latest string `json:"latest"`
	// ConsoleVersion is this control plane's own release, "" from source.
	ConsoleVersion string `json:"consoleVersion"`
	// Status is the agent's own account of updating; nil from an agent too
	// old to update itself.
	Status  *AgentUpdateStatus  `json:"status"`
	Request *AgentUpdateRequest `json:"request"`
}

// AgentUpdateView reads the agent card for hostname.
func (s *Store) AgentUpdateView(ctx context.Context, hostname string) (AgentUpdateView, error) {
	v := AgentUpdateView{Hostname: hostname}
	var (
		version            *string
		seen               *time.Time
		sessions           *int
		status             []byte
		reqID, reqVersion  *string
		reqNow, reqRestart *bool
		reqAt              *time.Time
		reqBy              *string
		enrolled           bool
	)
	err := s.db(ctx).QueryRow(ctx, `
		SELECT g.version, g.last_seen_at, g.active_sessions, g.update_status,
		       c.update_id, c.update_version, c.update_now, c.update_requested_at,
		       c.update_requested_by, c.update_restart, c.id IS NOT NULL
		  FROM (SELECT 1) one
		  LEFT JOIN agents g ON lower(g.hostname) = lower($1)
		  LEFT JOIN agent_credentials c ON lower(c.hostname) = lower($1) AND c.revoked_at IS NULL`,
		hostname).Scan(&version, &seen, &sessions, &status, &reqID, &reqVersion, &reqNow,
		&reqAt, &reqBy, &reqRestart, &enrolled)
	if err != nil {
		return v, err
	}
	v.Enrolled = enrolled
	if version != nil {
		v.AgentVersion = *version
	}
	v.AgentLastSeen = seen
	if sessions != nil {
		v.ActiveSessions = *sessions
	}
	if len(status) > 0 {
		var st AgentUpdateStatus
		if json.Unmarshal(status, &st) == nil {
			v.Status = &st
		}
	}
	if reqID != nil && reqVersion != nil {
		v.Request = &AgentUpdateRequest{ID: *reqID, Version: *reqVersion}
		if reqNow != nil {
			v.Request.Now = *reqNow
		}
		if reqAt != nil {
			v.Request.RequestedAt = *reqAt
		}
		if reqBy != nil {
			v.Request.RequestedBy = *reqBy
		}
		if reqRestart != nil {
			v.Request.Restart = *reqRestart
		}
	}
	return v, nil
}

// ErrNoLiveAgent is an update asked of a server with no enrolled agent.
var ErrNoLiveAgent = errors.New("this server has no enrolled agent")

// RequestAgentUpdate asks hostname's agent to update to version. A request
// already waiting for the same version keeps its id, so the agent goes on
// with the one it has; asking "now" only ever adds urgency. An update
// replaces a restart still waiting: it restarts the agent too.
func (s *Store) RequestAgentUpdate(ctx context.Context, hostname, version string, now bool,
	by string) (AgentUpdateRequest, error) {
	return s.requestAgent(ctx, hostname, version, now, false, by)
}

// RequestAgentRestart asks hostname's agent, running version, to restart. A
// restart already waiting keeps its id, as an update does.
func (s *Store) RequestAgentRestart(ctx context.Context, hostname, version string, now bool,
	by string) (AgentUpdateRequest, error) {
	return s.requestAgent(ctx, hostname, version, now, true, by)
}

// requestAgent puts an update or a restart on hostname's credential. A
// request of the same kind for the same version is kept and only made more
// urgent; anything else replaces it.
func (s *Store) requestAgent(ctx context.Context, hostname, version string, now, restart bool,
	by string) (AgentUpdateRequest, error) {
	var r AgentUpdateRequest
	err := s.db(ctx).QueryRow(ctx, `
		UPDATE agent_credentials SET
		       update_id = CASE WHEN update_id IS NOT NULL AND update_version = $2 AND update_restart = $5
		                        THEN update_id ELSE gen_random_uuid()::text END,
		       update_now = CASE WHEN update_id IS NOT NULL AND update_version = $2 AND update_restart = $5
		                         THEN update_now OR $3 ELSE $3 END,
		       update_requested_at = CASE WHEN update_id IS NOT NULL AND update_version = $2 AND update_restart = $5
		                                  THEN update_requested_at ELSE now() END,
		       update_requested_by = $4,
		       update_version = $2,
		       update_restart = $5
		 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL
		RETURNING update_id, update_version, update_now, update_restart, update_requested_at,
		          update_requested_by`,
		hostname, version, now, by, restart).Scan(&r.ID, &r.Version, &r.Now, &r.Restart,
		&r.RequestedAt, &r.RequestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNoLiveAgent
	}
	return r, err
}

// CancelAgentUpdate takes back hostname's update or restart request. The
// agent stops if it has not begun restarting. It returns the request it took
// back, nil when there was none.
func (s *Store) CancelAgentUpdate(ctx context.Context, hostname string) (*AgentUpdateRequest, error) {
	var r AgentUpdateRequest
	err := s.db(ctx).QueryRow(ctx, `
		WITH old AS (
			SELECT id, update_id, update_version, update_restart
			  FROM agent_credentials
			 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL AND update_id IS NOT NULL
			   FOR UPDATE)
		UPDATE agent_credentials c SET update_id = NULL, update_version = NULL,
		       update_now = false, update_restart = false, update_requested_at = NULL,
		       update_requested_by = NULL
		  FROM old WHERE c.id = old.id
		RETURNING old.update_id, old.update_version, old.update_restart`,
		hostname).Scan(&r.ID, &r.Version, &r.Restart)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// agentUpdateFor is the request to put on hostname's heartbeat reply, nil
// when there is none.
func (s *Store) agentUpdateFor(ctx context.Context, hostname string) (*AgentUpdateRequest, error) {
	var r AgentUpdateRequest
	err := s.db(ctx).QueryRow(ctx, `
		SELECT update_id, update_version, update_now, update_restart
		  FROM agent_credentials
		 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL AND update_id IS NOT NULL`,
		hostname).Scan(&r.ID, &r.Version, &r.Now, &r.Restart)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// recordAgentUpdate keeps what the agent said and, when it reports an
// outcome for the request standing, clears the request. It returns the
// request it cleared, so the outcome is audited once.
func (s *Store) recordAgentUpdate(ctx context.Context, hostname string,
	st AgentUpdateStatus) (*AgentUpdateRequest, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	if _, err := s.db(ctx).Exec(ctx,
		`UPDATE agents SET update_status = $2 WHERE lower(hostname) = lower($1)`,
		hostname, b); err != nil {
		return nil, err
	}
	if st.ID == "" || !st.finished() {
		return nil, nil
	}
	var r AgentUpdateRequest
	err = s.db(ctx).QueryRow(ctx, `
		WITH old AS (
			SELECT id, update_id, update_version, update_now, update_restart,
			       update_requested_at, update_requested_by
			  FROM agent_credentials
			 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL AND update_id = $2
			   FOR UPDATE)
		UPDATE agent_credentials c SET update_id = NULL, update_version = NULL,
		       update_now = false, update_restart = false, update_requested_at = NULL,
		       update_requested_by = NULL
		  FROM old WHERE c.id = old.id
		RETURNING old.update_id, old.update_version, old.update_now, old.update_restart,
		          old.update_requested_at, COALESCE(old.update_requested_by, '')`,
		hostname, st.ID).Scan(&r.ID, &r.Version, &r.Now, &r.Restart, &r.RequestedAt, &r.RequestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// AgentHostname is the name the agent on the server called hostname reports
// and holds its credential under: the server's agent_hostname when it has
// one, else its hostname. A server added from discovery is named by the FQDN
// its agent reported, and its agent_hostname is the short name the agent
// goes by; looking the agent up by the server's own name finds nothing.
func (s *Store) AgentHostname(ctx context.Context, hostname string) (string, error) {
	var h string
	err := s.db(ctx).QueryRow(ctx, `
		SELECT `+agentNameSQL+` FROM assets a WHERE a.hostname = $1`,
		hostname).Scan(&h)
	return h, err
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

// agentUpdateView is the card for an asset, with this control plane's
// version filled in.
func (a *API) agentUpdateView(r *http.Request, hostname string) (AgentUpdateView, error) {
	v, err := a.store.AgentUpdateView(r.Context(), hostname)
	if err != nil {
		return v, err
	}
	v.Latest, v.ConsoleVersion, _ = a.agentTarget()
	return v, nil
}

// agentTarget is the version agents are offered, this control plane's own
// release ("" from source), and the newest release found, if any. The newer
// of the two is offered.
func (a *API) agentTarget() (offer, own string, newest *AgentRelease) {
	if v := strings.TrimPrefix(a.Version, "v"); reAgentVer.MatchString(v) {
		own = v
	}
	offer = own
	newest, _, _ = a.Releases.Latest()
	if newest != nil && (offer == "" || versionLess(offer, newest.Version)) {
		offer = newest.Version
	}
	return offer, own, newest
}

func (a *API) getAssetAgentUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing a server's agent requires the admin, owner or auditor role")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	agentHost, err := a.store.AgentHostname(r.Context(), asset.Hostname)
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	v, err := a.agentUpdateView(r, agentHost)
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) postAssetAgentUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "updating a server's agent", "admin", "owner") {
		return
	}
	var in struct {
		Version string `json:"version"`
		Now     bool   `json:"now"`
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
	agentHost, err := a.store.AgentHostname(r.Context(), asset.Hostname)
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	version := strings.TrimPrefix(strings.TrimSpace(in.Version), "v")
	if version == "" {
		version, _, _ = a.agentTarget()
	}
	if !reAgentVer.MatchString(version) {
		writeErr(w, http.StatusBadRequest, "name the release to install, like 1.2.3; this control "+
			"plane was built from source, so it has no version of its own to offer")
		return
	}
	view, err := a.agentUpdateView(r, agentHost)
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	switch {
	case !view.Enrolled:
		writeErr(w, http.StatusConflict, "this server has no enrolled agent to update; "+
			"add it from Servers, Add a server")
		return
	case view.Status == nil:
		writeErr(w, http.StatusConflict, "this server's agent ("+orUnknown(view.AgentVersion)+
			") is too old to update itself from the console; upgrade it by hand once, and "+
			"later versions update from here")
		return
	case !view.Status.Supported:
		writeErr(w, http.StatusConflict, "this server's agent cannot update itself: "+
			view.Status.Reason)
		return
	case view.AgentVersion == version && (view.Request == nil || view.Request.Restart):
		writeErr(w, http.StatusConflict, "this server's agent already runs "+version)
		return
	}

	req, err := a.store.RequestAgentUpdate(r.Context(), agentHost, version, in.Now, sess.Email)
	if errors.Is(err, ErrNoLiveAgent) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	how := "once nothing is open on it"
	if req.Now {
		how = fmt.Sprintf("now, ending the %d session(s) open on it", view.ActiveSessions)
	}
	a.auditAsset(r, AuditEvent{
		Action:     "agent.update_requested",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail: "Asked the agent on " + asset.Hostname + " to update from " +
			orUnknown(view.AgentVersion) + " to " + version + ", " + how + ".",
	})
	writeJSON(w, http.StatusAccepted, req)
}

// postAssetAgentRestart asks a server's agent to restart on the version it
// runs: once nothing is open on the server, or now.
func (a *API) postAssetAgentRestart(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "restarting a server's agent", "admin", "owner") {
		return
	}
	var in struct {
		Now bool `json:"now"`
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
	agentHost, err := a.store.AgentHostname(r.Context(), asset.Hostname)
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	view, err := a.agentUpdateView(r, agentHost)
	if err != nil {
		a.fail(w, "agent restart", err)
		return
	}
	switch {
	case !view.Enrolled:
		writeErr(w, http.StatusConflict, "this server has no enrolled agent to restart; "+
			"add it from Servers, Add a server")
		return
	case view.Status == nil || (!view.Status.Restartable && view.Status.RestartReason == ""):
		writeErr(w, http.StatusConflict, "this server's agent ("+orUnknown(view.AgentVersion)+
			") is too old to restart from the console; update it, which restarts it, and "+
			"later versions restart from here")
		return
	case !view.Status.Restartable:
		writeErr(w, http.StatusConflict, "this server's agent cannot restart from the console: "+
			view.Status.RestartReason)
		return
	case view.Request != nil && !view.Request.Restart:
		writeErr(w, http.StatusConflict, "an update to "+view.Request.Version+" is already "+
			"asked of this server's agent, and it restarts the agent; take it back first to "+
			"restart without updating")
		return
	}

	req, err := a.store.RequestAgentRestart(r.Context(), agentHost, view.AgentVersion, in.Now, sess.Email)
	if errors.Is(err, ErrNoLiveAgent) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "agent restart", err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action:     "agent.restart_requested",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail: "Asked the agent on " + asset.Hostname + " (" + orUnknown(view.AgentVersion) +
			") to restart, " + updateWhen(req.Now, view.ActiveSessions) + ".",
	})
	writeJSON(w, http.StatusAccepted, req)
}

func (a *API) deleteAssetAgentUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "updating a server's agent", "admin", "owner") {
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	agentHost, err := a.store.AgentHostname(r.Context(), asset.Hostname)
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	cancelled, err := a.store.CancelAgentUpdate(r.Context(), agentHost)
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	if cancelled == nil {
		writeErr(w, http.StatusConflict, "there is no update or restart to take back on this server")
		return
	}
	action, what := "agent.update_cancelled", "the agent update"
	if cancelled.Restart {
		action, what = "agent.restart_cancelled", "the agent restart"
	}
	a.auditAsset(r, AuditEvent{
		Action:     action,
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail: "Took back " + what + " on " + asset.Hostname + ". If the agent had " +
			"already begun restarting, it finishes.",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// heartbeatUpdate keeps what an agent said about updating, audits an
// outcome, and returns the request to send back, if any.
func (a *API) heartbeatUpdate(ctx context.Context, hostname, version string,
	st *AgentUpdateStatus) any {
	if st != nil {
		done, err := a.store.recordAgentUpdate(ctx, hostname, *st)
		if err != nil {
			a.log.Error("heartbeat: cannot keep the agent's update status", "error", err)
		} else if done != nil {
			a.auditUpdateOutcome(ctx, hostname, *done, *st)
		}
	}
	req, err := a.store.agentUpdateFor(ctx, hostname)
	if err != nil {
		a.log.Error("heartbeat: cannot read whether the agent should update", "error", err)
		return nil
	}
	if req == nil {
		req = a.autoUpdate(ctx, hostname, version, st)
	}
	if req == nil {
		return nil
	}
	base := a.AgentReleasesURL
	if base == "" {
		base = DefaultAgentReleasesURL
	}
	out := map[string]any{"id": req.ID, "version": req.Version, "now": req.Now,
		"url": strings.TrimRight(base, "/")}
	if req.Restart {
		out["restart"] = true
	}
	return out
}

func (a *API) auditUpdateOutcome(ctx context.Context, hostname string, req AgentUpdateRequest,
	st AgentUpdateStatus) {
	action, severity := "agent.updated", "notice"
	switch st.State {
	case "rolled_back":
		action, severity = "agent.update_rolled_back", "warn"
	case "failed":
		action, severity = "agent.update_failed", "warn"
	case "cancelled":
		action = "agent.update_cancelled"
	}
	detail := fmt.Sprintf("The agent on %s, asked by %s to update to %s: %s.", hostname,
		orUnknown(req.RequestedBy), req.Version, strings.TrimSuffix(st.Message, "."))
	if req.Restart {
		switch st.State {
		case "done":
			action = "agent.restarted"
		case "cancelled":
			action = "agent.restart_cancelled"
		default:
			action, severity = "agent.restart_failed", "warn"
		}
		detail = fmt.Sprintf("The agent on %s, asked by %s to restart: %s.", hostname,
			orUnknown(req.RequestedBy), strings.TrimSuffix(st.Message, "."))
	}
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: action, Severity: severity, Target: hostname, Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
	}
	a.log.Info("agent update finished", "hostname", hostname, "state", st.State,
		"version", req.Version, "message", st.Message)
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown version"
	}
	return s
}

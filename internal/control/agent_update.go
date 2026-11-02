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
	Supported bool       `json:"supported"`
	Reason    string     `json:"reason,omitempty"`
	ID        string     `json:"id,omitempty"`
	From      string     `json:"from,omitempty"`
	Target    string     `json:"target,omitempty"`
	State     string     `json:"state,omitempty"`
	Message   string     `json:"message,omitempty"`
	Sessions  int        `json:"sessions,omitempty"`
	At        *time.Time `json:"at,omitempty"`
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
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	Now         bool      `json:"now"`
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
	// Latest is this control plane's own version when it is a release:
	// the version an agent is offered. "" for a build from source.
	Latest string `json:"latest"`
	// Status is the agent's own account of updating; nil from an agent too
	// old to update itself.
	Status  *AgentUpdateStatus  `json:"status"`
	Request *AgentUpdateRequest `json:"request"`
}

// AgentUpdateView reads the agent card for hostname.
func (s *Store) AgentUpdateView(ctx context.Context, hostname string) (AgentUpdateView, error) {
	v := AgentUpdateView{Hostname: hostname}
	var (
		version           *string
		seen              *time.Time
		sessions          *int
		status            []byte
		reqID, reqVersion *string
		reqNow            *bool
		reqAt             *time.Time
		reqBy             *string
		enrolled          bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT g.version, g.last_seen_at, g.active_sessions, g.update_status,
		       c.update_id, c.update_version, c.update_now, c.update_requested_at,
		       c.update_requested_by, c.id IS NOT NULL
		  FROM (SELECT 1) one
		  LEFT JOIN agents g ON lower(g.hostname) = lower($1)
		  LEFT JOIN agent_credentials c ON lower(c.hostname) = lower($1) AND c.revoked_at IS NULL`,
		hostname).Scan(&version, &seen, &sessions, &status, &reqID, &reqVersion, &reqNow,
		&reqAt, &reqBy, &enrolled)
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
	}
	return v, nil
}

// ErrNoLiveAgent is an update asked of a server with no enrolled agent.
var ErrNoLiveAgent = errors.New("this server has no enrolled agent")

// RequestAgentUpdate asks hostname's agent to update to version. A request
// already waiting for the same version keeps its id, so the agent goes on
// with the one it has; asking "now" only ever adds urgency.
func (s *Store) RequestAgentUpdate(ctx context.Context, hostname, version string, now bool,
	by string) (AgentUpdateRequest, error) {
	var r AgentUpdateRequest
	err := s.pool.QueryRow(ctx, `
		UPDATE agent_credentials SET
		       update_id = CASE WHEN update_id IS NOT NULL AND update_version = $2
		                        THEN update_id ELSE gen_random_uuid()::text END,
		       update_now = CASE WHEN update_id IS NOT NULL AND update_version = $2
		                         THEN update_now OR $3 ELSE $3 END,
		       update_requested_at = CASE WHEN update_id IS NOT NULL AND update_version = $2
		                                  THEN update_requested_at ELSE now() END,
		       update_requested_by = $4,
		       update_version = $2
		 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL
		RETURNING update_id, update_version, update_now, update_requested_at, update_requested_by`,
		hostname, version, now, by).Scan(&r.ID, &r.Version, &r.Now, &r.RequestedAt, &r.RequestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNoLiveAgent
	}
	return r, err
}

// CancelAgentUpdate takes back hostname's update request. The agent stops
// if it has not begun restarting. Reports whether there was one.
func (s *Store) CancelAgentUpdate(ctx context.Context, hostname string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agent_credentials SET update_id = NULL, update_version = NULL,
		       update_now = false, update_requested_at = NULL, update_requested_by = NULL
		 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL AND update_id IS NOT NULL`,
		hostname)
	return tag.RowsAffected() > 0, err
}

// agentUpdateFor is the request to put on hostname's heartbeat reply, nil
// when there is none.
func (s *Store) agentUpdateFor(ctx context.Context, hostname string) (*AgentUpdateRequest, error) {
	var r AgentUpdateRequest
	err := s.pool.QueryRow(ctx, `
		SELECT update_id, update_version, update_now
		  FROM agent_credentials
		 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL AND update_id IS NOT NULL`,
		hostname).Scan(&r.ID, &r.Version, &r.Now)
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
	if _, err := s.pool.Exec(ctx,
		`UPDATE agents SET update_status = $2 WHERE lower(hostname) = lower($1)`,
		hostname, b); err != nil {
		return nil, err
	}
	if st.ID == "" || !st.finished() {
		return nil, nil
	}
	var r AgentUpdateRequest
	err = s.pool.QueryRow(ctx, `
		WITH old AS (
			SELECT id, update_id, update_version, update_now, update_requested_at,
			       update_requested_by
			  FROM agent_credentials
			 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL AND update_id = $2
			   FOR UPDATE)
		UPDATE agent_credentials c SET update_id = NULL, update_version = NULL,
		       update_now = false, update_requested_at = NULL, update_requested_by = NULL
		  FROM old WHERE c.id = old.id
		RETURNING old.update_id, old.update_version, old.update_now,
		          old.update_requested_at, COALESCE(old.update_requested_by, '')`,
		hostname, st.ID).Scan(&r.ID, &r.Version, &r.Now, &r.RequestedAt, &r.RequestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

// agentUpdateView is the card for an asset, with this control plane's
// version filled in.
func (a *API) agentUpdateView(r *http.Request, hostname string) (AgentUpdateView, error) {
	v, err := a.store.AgentUpdateView(r.Context(), hostname)
	if err != nil {
		return v, err
	}
	if reAgentVer.MatchString(strings.TrimPrefix(a.Version, "v")) {
		v.Latest = strings.TrimPrefix(a.Version, "v")
	}
	return v, nil
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
	v, err := a.agentUpdateView(r, asset.Hostname)
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
	version := strings.TrimPrefix(strings.TrimSpace(in.Version), "v")
	if version == "" {
		version = strings.TrimPrefix(a.Version, "v")
	}
	if !reAgentVer.MatchString(version) {
		writeErr(w, http.StatusBadRequest, "name the release to install, like 1.2.3; this control "+
			"plane was built from source, so it has no version of its own to offer")
		return
	}
	view, err := a.agentUpdateView(r, asset.Hostname)
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
	case view.AgentVersion == version && view.Request == nil:
		writeErr(w, http.StatusConflict, "this server's agent already runs "+version)
		return
	}

	req, err := a.store.RequestAgentUpdate(r.Context(), asset.Hostname, version, in.Now, sess.Email)
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
	cancelled, err := a.store.CancelAgentUpdate(r.Context(), asset.Hostname)
	if err != nil {
		a.fail(w, "agent update", err)
		return
	}
	if !cancelled {
		writeErr(w, http.StatusConflict, "there is no update to take back on this server")
		return
	}
	a.auditAsset(r, AuditEvent{
		Action:     "agent.update_cancelled",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail: "Took back the agent update on " + asset.Hostname + ". If the agent had " +
			"already begun restarting, it finishes.",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// heartbeatUpdate keeps what an agent said about updating, audits an
// outcome, and returns the request to send back, if any.
func (a *API) heartbeatUpdate(ctx context.Context, hostname string, st *AgentUpdateStatus) any {
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
		return nil
	}
	base := a.AgentReleasesURL
	if base == "" {
		base = DefaultAgentReleasesURL
	}
	return map[string]any{"id": req.ID, "version": req.Version, "now": req.Now,
		"url": strings.TrimRight(base, "/")}
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

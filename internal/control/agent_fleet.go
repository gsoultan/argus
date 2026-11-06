package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

/*
Every server's agent at once.

The server page updates one agent. This is the fleet's view: which version
each agent runs against the version the console offers (the newest release
it found, or its own), an Update all that asks every agent that is behind,
and a switch that has the console do that by itself whenever a newer
version is out.

Each agent still updates the careful way (agent_update.go): only once nobody
is connected to its server, holding logins through the restart, and going
back on its own if the new version does not come up. So asking the whole
fleet at once is safe; servers in use update later, each when it is free.

Automatic updates never retry a version that went back or failed on a
server; that takes a person pressing Update, after reading why.
*/

// Agent states on the fleet view.
const (
	agentCurrent     = "current"      // runs the offered version, or newer
	agentBehind      = "behind"       // older, and can update from the console
	agentUpdating    = "updating"     // an update is asked and not finished
	agentHeld        = "held"         // the offered version went back or failed here
	agentCannot      = "cannot"       // older, and cannot update itself here
	agentTooOld      = "too_old"      // older than updating from the console
	agentNotReported = "not_reported" // has not said what it runs
)

// AgentFleetRow is one server's agent.
type AgentFleetRow struct {
	AssetID        string              `json:"assetId"`
	Hostname       string              `json:"hostname"`
	AgentVersion   string              `json:"agentVersion"`
	AgentLastSeen  *time.Time          `json:"agentLastSeenAt"`
	ActiveSessions int                 `json:"activeSessions"`
	Status         *AgentUpdateStatus  `json:"status"`
	Request        *AgentUpdateRequest `json:"request"`
	// State is one of the agent states above, against the offered version.
	State string `json:"state"`
	// Why says what keeps a held or cannot agent where it is.
	Why string `json:"why,omitempty"`
}

// AgentFleetView is the Servers page's agent card.
type AgentFleetView struct {
	// Latest is the version agents are offered; "" when there is none.
	Latest         string        `json:"latest"`
	ConsoleVersion string        `json:"consoleVersion"`
	Release        *AgentRelease `json:"release"`
	// CheckedAt is when the console last asked for releases; nil when it
	// does not ask (a mirror, or no watcher).
	CheckedAt    *time.Time      `json:"checkedAt"`
	CheckFailure string          `json:"checkFailure,omitempty"`
	Automatic    bool            `json:"automatic"`
	Servers      []AgentFleetRow `json:"servers"`
	// Behind counts servers in the behind state: what Update all asks.
	Behind   int `json:"behind"`
	Updating int `json:"updating"`
}

// AgentFleet reads every enrolled agent, by hostname.
func (s *Store) AgentFleet(ctx context.Context) ([]AgentFleetRow, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id, h.name, COALESCE(g.version, ''), g.last_seen_at,
		       COALESCE(g.active_sessions, 0), g.update_status,
		       c.update_id, c.update_version, c.update_now, c.update_requested_at,
		       c.update_requested_by, c.update_restart
		  FROM assets a
		  -- The agent's own name: a server added from discovery is named by
		  -- its FQDN and its agent by agent_hostname (see AgentHostname).
		  CROSS JOIN LATERAL (SELECT COALESCE(NULLIF(a.agent_hostname, ''), a.hostname) AS name) h
		  JOIN agent_credentials c ON lower(c.hostname) = lower(h.name) AND c.revoked_at IS NULL
		  LEFT JOIN agents g ON lower(g.hostname) = lower(h.name)
		 WHERE a.archived_at IS NULL
		 ORDER BY lower(h.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentFleetRow
	for rows.Next() {
		var (
			f                  AgentFleetRow
			status             []byte
			reqID, reqVersion  *string
			reqNow, reqRestart *bool
			reqAt              *time.Time
			reqBy              *string
		)
		if err := rows.Scan(&f.AssetID, &f.Hostname, &f.AgentVersion, &f.AgentLastSeen,
			&f.ActiveSessions, &status, &reqID, &reqVersion, &reqNow, &reqAt, &reqBy, &reqRestart); err != nil {
			return nil, err
		}
		if len(status) > 0 {
			var st AgentUpdateStatus
			if json.Unmarshal(status, &st) == nil {
				f.Status = &st
			}
		}
		if reqID != nil && reqVersion != nil {
			f.Request = &AgentUpdateRequest{ID: *reqID, Version: *reqVersion}
			if reqNow != nil {
				f.Request.Now = *reqNow
			}
			if reqAt != nil {
				f.Request.RequestedAt = *reqAt
			}
			if reqBy != nil {
				f.Request.RequestedBy = *reqBy
			}
			if reqRestart != nil {
				f.Request.Restart = *reqRestart
			}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AgentAutoUpdate reads whether the console updates agents on its own.
func (s *Store) AgentAutoUpdate(ctx context.Context) (bool, error) {
	var on bool
	err := s.db(ctx).QueryRow(ctx,
		`SELECT agent_auto_update FROM gateway_policy WHERE id = TRUE`).Scan(&on)
	return on, err
}

// SetAgentAutoUpdate turns automatic agent updates on or off, and reports
// whether that changed anything.
func (s *Store) SetAgentAutoUpdate(ctx context.Context, on bool, by string) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE gateway_policy SET agent_auto_update = $1, updated_at = now(), updated_by = $2
		 WHERE id = TRUE AND agent_auto_update IS DISTINCT FROM $1`, on, by)
	return tag.RowsAffected() > 0, err
}

// classifyAgent sets a row's state against the offered version.
func classifyAgent(f *AgentFleetRow, latest string) {
	// A restart is not an update: the row says where the version stands.
	f.State, f.Why = agentState(f.AgentVersion, latest, f.Status, f.Request != nil && !f.Request.Restart)
}

func agentState(running, latest string, st *AgentUpdateStatus, asked bool) (string, string) {
	switch {
	case asked:
		return agentUpdating, ""
	case running == "":
		return agentNotReported, ""
	case latest == "" || !versionLess(running, latest):
		return agentCurrent, ""
	case st == nil:
		return agentTooOld, "this agent is too old to update itself from the console; " +
			"upgrade the argus-agent package on the server by hand once"
	case !st.Supported:
		return agentCannot, st.Reason
	case st.Target == latest && (st.State == "rolled_back" || st.State == "failed"):
		return agentHeld, st.Message
	}
	return agentBehind, ""
}

func (a *API) agentFleetView(ctx context.Context) (AgentFleetView, error) {
	var v AgentFleetView
	v.Latest, v.ConsoleVersion, v.Release = a.agentTarget()
	if a.Releases != nil {
		if _, at, failure := a.Releases.Latest(); !at.IsZero() {
			v.CheckedAt, v.CheckFailure = &at, failure
		}
	}
	on, err := a.store.AgentAutoUpdate(ctx)
	if err != nil {
		return v, err
	}
	v.Automatic = on
	rows, err := a.store.AgentFleet(ctx)
	if err != nil {
		return v, err
	}
	for i := range rows {
		classifyAgent(&rows[i], v.Latest)
		switch rows[i].State {
		case agentBehind:
			v.Behind++
		case agentUpdating:
			v.Updating++
		}
	}
	v.Servers = rows
	if v.Servers == nil {
		v.Servers = []AgentFleetRow{}
	}
	return v, nil
}

func (a *API) getAgentFleet(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing the fleet's agents requires the admin, owner or auditor role")
		return
	}
	v, err := a.agentFleetView(r.Context())
	if err != nil {
		a.fail(w, "agent fleet", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// AgentFleetSkip is a server an Update all did not ask, and why.
type AgentFleetSkip struct {
	Hostname string `json:"hostname"`
	Why      string `json:"why"`
}

// postAgentFleetUpdate asks agents to update to the offered version: every
// one that is behind, or the hostnames named. Naming a server also retries
// one whose last try at this version went back.
func (a *API) postAgentFleetUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "updating agents", "admin", "owner") {
		return
	}
	var in struct {
		Hostnames []string `json:"hostnames"`
		Now       bool     `json:"now"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := a.agentFleetView(r.Context())
	if err != nil {
		a.fail(w, "agent fleet", err)
		return
	}
	if v.Latest == "" {
		writeErr(w, http.StatusConflict, "there is no version to offer: this control plane was "+
			"built from source and found no release; update a server from its own page, "+
			"naming the release")
		return
	}
	named := map[string]bool{}
	for _, h := range in.Hostnames {
		named[strings.ToLower(strings.TrimSpace(h))] = true
	}
	asked := []string{}
	skipped := []AgentFleetSkip{}
	for _, f := range v.Servers {
		pick := named[strings.ToLower(f.Hostname)]
		if len(named) > 0 && !pick {
			continue
		}
		ok := f.State == agentBehind || (pick && f.State == agentHeld)
		if !ok {
			if pick || f.State == agentHeld || f.State == agentCannot || f.State == agentTooOld {
				skipped = append(skipped, AgentFleetSkip{Hostname: f.Hostname, Why: skipReason(f, v.Latest)})
			}
			continue
		}
		req, err := a.store.RequestAgentUpdate(r.Context(), f.Hostname, v.Latest, in.Now, sess.Email)
		if err != nil {
			skipped = append(skipped, AgentFleetSkip{Hostname: f.Hostname, Why: err.Error()})
			continue
		}
		asked = append(asked, f.Hostname)
		a.auditAsset(r, AuditEvent{
			Action:     "agent.update_requested",
			Severity:   "notice",
			ActorEmail: sess.Email,
			Target:     f.Hostname,
			Detail: "Asked the agent on " + f.Hostname + " to update from " + orUnknown(f.AgentVersion) +
				" to " + v.Latest + ", " + updateWhen(req.Now, f.ActiveSessions) + ", with every agent behind.",
		})
	}
	for h := range named {
		if !hasHost(v.Servers, h) {
			skipped = append(skipped, AgentFleetSkip{Hostname: h, Why: "no enrolled agent by that name"})
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"version": v.Latest, "asked": asked, "skipped": skipped})
}

func hasHost(rows []AgentFleetRow, h string) bool {
	for _, f := range rows {
		if strings.EqualFold(f.Hostname, h) {
			return true
		}
	}
	return false
}

func skipReason(f AgentFleetRow, latest string) string {
	switch f.State {
	case agentCurrent:
		return "already runs " + f.AgentVersion
	case agentUpdating:
		return "already updating to " + f.Request.Version
	case agentNotReported:
		return "its agent has not said which version it runs yet"
	case agentHeld:
		return latest + " went back on this server last time (" + strings.TrimSuffix(f.Why, ".") +
			"); update it from its own page to try again"
	}
	return f.Why
}

func updateWhen(now bool, sessions int) string {
	if now {
		return fmt.Sprintf("now, ending the %d session(s) open on it", sessions)
	}
	return "once nothing is open on it"
}

func (a *API) putAgentAutoUpdate(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing automatic agent updates", "admin", "owner") {
		return
	}
	var in struct {
		On *bool `json:"on"`
	}
	if err := decode(r, &in); err != nil || in.On == nil {
		writeErr(w, http.StatusBadRequest, `send {"on": true} or {"on": false}`)
		return
	}
	changed, err := a.store.SetAgentAutoUpdate(r.Context(), *in.On, sess.Email)
	if err != nil {
		a.fail(w, "automatic agent updates", err)
		return
	}
	if changed {
		e := AuditEvent{Action: "agent.auto_update_off", Severity: "notice", ActorEmail: sess.Email,
			Target: "agents", Detail: "Turned off automatic agent updates. Agents update only " +
				"when someone asks."}
		if *in.On {
			e.Action = "agent.auto_update_on"
			e.Detail = "Turned on automatic agent updates: when a newer agent is out, every " +
				"server's agent updates to it once nobody is connected to that server."
		}
		a.auditAsset(r, e)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"on": *in.On})
}

// autoUpdate asks an agent that is behind to update, when automatic updates
// are on. It is called from the heartbeat with no request standing, and
// returns the request it made, if any.
func (a *API) autoUpdate(ctx context.Context, hostname, running string,
	st *AgentUpdateStatus) *AgentUpdateRequest {
	target, _, _ := a.agentTarget()
	// Cheap checks first: most heartbeats come from agents that are current.
	if state, _ := agentState(running, target, st, false); state != agentBehind {
		return nil
	}
	if st != nil && !st.finished() && st.State != "" {
		return nil // something of its own is under way; let it finish
	}
	if st != nil && st.Target == target && st.State == "cancelled" {
		return nil // someone took this version back here
	}
	on, err := a.store.AgentAutoUpdate(ctx)
	if err != nil {
		a.log.Error("heartbeat: cannot read whether agents update automatically", "error", err)
		return nil
	}
	if !on {
		return nil
	}
	req, err := a.store.RequestAgentUpdate(ctx, hostname, target, false, autoUpdater)
	if errors.Is(err, ErrNoLiveAgent) {
		return nil
	}
	if err != nil {
		a.log.Error("heartbeat: cannot ask the agent to update", "error", err)
		return nil
	}
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: "agent.update_requested", Severity: "notice", Target: hostname,
		Detail: "Automatic updates asked the agent on " + hostname + " to update from " +
			running + " to " + target + ", once nothing is open on it.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "agent.update_requested")
	}
	a.log.Info("automatic agent update asked", "hostname", hostname, "from", running, "to", target)
	return &req
}

// autoUpdater is who an automatic update was asked by.
const autoUpdater = "automatic updates"

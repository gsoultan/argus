package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Which logins the agent on a host records.

ScopeEveryLogin is what the agent has always done: every sshd login passes
through the shim and is recorded, whether it came through Argus or not.
ScopeArgusOnly records what arrives through Argus and lets a direct login
through unrecorded. Unrecorded is not unseen: the agent reports each such login
and it goes into the audit chain as a bypass, so the trade an administrator
makes here is storage and noise against a replay, never against knowing.

The setting is the console's. The agent learns it from the reply to its
heartbeat and echoes back what it applied, so the console can tell a saved
setting from an enforced one.
*/

const (
	ScopeEveryLogin = "every_login"
	ScopeArgusOnly  = "argus_only"
)

// effectiveScope picks the scope an agent should apply from every asset its
// hostname matched.
//
// A short hostname can match more than one asset. When they disagree the
// strict one wins: recording a login nobody needed is a cost, and not recording
// one somebody did is a gap. No match at all is the same: the agent has always
// recorded everything, and an inventory mistake must not be what turns that off.
func effectiveScope(scopes []string) string {
	if len(scopes) == 0 {
		return ScopeEveryLogin
	}
	for _, s := range scopes {
		if s != ScopeArgusOnly {
			return ScopeEveryLogin
		}
	}
	return ScopeArgusOnly
}

// SetRecordingScope changes one asset's scope and returns what it was.
func (s *Store) SetRecordingScope(ctx context.Context, id, scope string) (before string, after Asset, err error) {
	err = s.pool.QueryRow(ctx, `
		UPDATE assets a SET recording_scope = $2, updated_at = now()
		FROM (SELECT id, recording_scope FROM assets WHERE id = $1::uuid FOR UPDATE) old
		WHERE a.id = old.id AND a.archived_at IS NULL
		RETURNING old.recording_scope`, id, scope).Scan(&before)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", Asset{}, ErrAssetNotFound
		}
		return "", Asset{}, err
	}
	after, err = s.AssetByID(ctx, id)
	return before, after, err
}

// RecordingScopeFor is the scope the console has set for a reporting host,
// matched the way a heartbeat is matched.
func (s *Store) RecordingScopeFor(ctx context.Context, hostname string) (string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT recording_scope FROM assets
		WHERE archived_at IS NULL
		  AND (hostname = $1 OR agent_hostname = $1 OR split_part(hostname, '.', 1) = $1)`,
		hostname)
	if err != nil {
		return ScopeEveryLogin, err
	}
	defer rows.Close()
	var scopes []string
	for rows.Next() {
		var sc string
		if err := rows.Scan(&sc); err != nil {
			return ScopeEveryLogin, err
		}
		scopes = append(scopes, sc)
	}
	return effectiveScope(scopes), rows.Err()
}

// putRecordingScope is the console's control. Admin or owner, audited.
//
// Its own route rather than a field on the asset edit: an edit replaces every
// field it is sent, and a console that predates this setting would put a host
// back to every_login each time someone renamed it -- a control that changes
// without anyone choosing to change it.
func (a *API) putRecordingScope(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing what a server records", "admin", "owner") {
		return
	}
	var in struct {
		RecordingScope string `json:"recordingScope"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.RecordingScope != ScopeEveryLogin && in.RecordingScope != ScopeArgusOnly {
		writeErr(w, http.StatusBadRequest,
			"recordingScope must be every_login or argus_only")
		return
	}

	before, asset, err := a.store.SetRecordingScope(r.Context(), r.PathValue("id"), in.RecordingScope)
	if err != nil {
		a.failAsset(w, "set recording scope", err)
		return
	}
	if before == asset.RecordingScope {
		writeJSON(w, http.StatusOK, asset)
		return
	}

	// Loosening is the one an auditor needs to find: from here on, direct
	// logins to this host have no replay.
	e := AuditEvent{
		Action:     "asset.recording_scope_changed",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail: "Set " + asset.Hostname + " to record every login again. Direct " +
			"logins are recorded from the agent's next heartbeat.",
	}
	if asset.RecordingScope == ScopeArgusOnly {
		e.Severity = "warning"
		e.Detail = "Set " + asset.Hostname + " to record Argus sessions only. From the " +
			"agent's next heartbeat, a login that does not come through Argus is let " +
			"through unrecorded and reported as a bypass, with no replay."
	}
	a.auditAsset(r, e)
	writeJSON(w, http.StatusOK, asset)
}

// postLogin is an agent reporting a login it let through without recording.
//
// It only ever happens under argus_only, and it is the whole of what Argus
// will know about that session, so it goes into the audit chain -- the record
// an auditor reads and the one that cannot be quietly edited afterwards.
func (a *API) postLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hostname   string    `json:"hostname"`
		Principal  string    `json:"principal"`
		ClientAddr string    `json:"client_addr"`
		Command    string    `json:"command"`
		Origin     string    `json:"origin"`
		Scope      string    `json:"scope"`
		At         time.Time `json:"at"`
	}
	if err := decodeReport(r, &in, a.log, "report/login"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Hostname == "" || in.Principal == "" {
		writeErr(w, http.StatusBadRequest, "hostname and principal are required")
		return
	}
	if !a.mayReportFor(w, r, in.Hostname) {
		return
	}
	if in.At.IsZero() {
		in.At = time.Now().UTC()
	}

	what := "an interactive session"
	if in.Command != "" {
		what = fmt.Sprintf("the command %q", in.Command)
	}
	from := in.ClientAddr
	if from == "" {
		from = "an unknown address"
	}
	detail := fmt.Sprintf("%s logged in to %s from %s at %s and ran %s without "+
		"passing through Argus. The host records Argus sessions only, so the agent "+
		"let it through unrecorded: there is no replay, and no approval, time window "+
		"or principal check was applied.",
		in.Principal, in.Hostname, from, in.At.UTC().Format(time.RFC3339), what)

	// The agent says why it did not record. If the console disagrees, the
	// agent was acting on a setting it should no longer hold -- or the report
	// is not what it claims to be. Either way the auditor is told, not left to
	// assume the policy was followed.
	if set, err := a.store.RecordingScopeFor(r.Context(), in.Hostname); err != nil {
		a.log.Error("read recording scope", "error", err, "host", in.Hostname)
	} else if set != ScopeArgusOnly {
		detail += " The console has this host set to record every login, so the " +
			"agent was not following the current setting; check the agent on this host."
	}

	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:   "session.direct_unrecorded",
		Severity: "critical",
		Target:   in.Hostname,
		Detail:   detail,
	}); err != nil {
		a.fail(w, "report login", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

/*
Logins while Argus cannot be reached.

A server set to refuse direct SSH, or to ask for approval, refuses every
plain SSH login while its agent cannot reach Argus: nobody is there to
approve, and refuse means refuse. That is the safe answer until the outage is
the thing an admin needs to get in to fix.

This setting lets members of named groups in once the agent has gone
OutageLogins.AfterMinutes without hearing from Argus. Everything else stays:
sshd still checks the key or password, the login is still recorded on the
server, and it is reported, audited and put in the bell when Argus is back.
Everyone else is still refused, and told which groups are let in.

It rides the heartbeat reply, which the agent only acts on when it carries
the control plane's signature, and the agent keeps the last one it heard
across a restart, like the direct SSH setting itself. An agent older than it
ignores it and keeps refusing. Off (0) unless someone turns it on; a server's
own page can turn it off for that server alone.
*/

// OutageLoginsChoices is the minutes an admin can pick; 0 is off.
var OutageLoginsChoices = []int{0, 5, 15, 60}

// maxOutageGroups bounds the group list every heartbeat carries.
const maxOutageGroups = 16

// reGroupName is what a Unix group name may be.
var reGroupName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,31}$`)

var (
	// ErrInvalidOutageLogins is a setting outside what can be chosen.
	ErrInvalidOutageLogins = errors.New("let in after is 0 (off), 5, 15 or 60 minutes")
	// ErrInvalidOutageGroups is a group list that cannot be one.
	ErrInvalidOutageGroups = fmt.Errorf("name 1 to %d groups, each letters, digits, '.', '_' or '-', "+
		"starting with a letter or '_'", maxOutageGroups)
)

// OutageLogins is an organization's setting.
type OutageLogins struct {
	// AfterMinutes is how long an agent must have gone without hearing from
	// Argus before it lets a member of Groups in; 0 is off.
	AfterMinutes int      `json:"afterMinutes"`
	Groups       []string `json:"groups"`
	UpdatedAt    string   `json:"updatedAt,omitempty"`
	UpdatedBy    string   `json:"updatedBy,omitempty"`
}

// On reports whether anyone is let in this way.
func (o OutageLogins) On() bool { return o.AfterMinutes > 0 }

// agentOrder is what the heartbeat reply carries: the setting as this server
// applies it.
func (o OutageLogins) agentOrder(serverOff bool) map[string]any {
	after := o.AfterMinutes
	if serverOff {
		after = 0
	}
	groups := o.Groups
	if groups == nil {
		groups = []string{}
	}
	return map[string]any{"after_minutes": after, "groups": groups}
}

// cleanOutageGroups trims, drops repeats and checks each name.
func cleanOutageGroups(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g == "" || slices.Contains(out, g) {
			continue
		}
		if !reGroupName.MatchString(g) {
			return nil, ErrInvalidOutageGroups
		}
		out = append(out, g)
	}
	if len(out) == 0 || len(out) > maxOutageGroups {
		return nil, ErrInvalidOutageGroups
	}
	return out, nil
}

/* ── Store ───────────────────────────────────────────────────────────────── */

// OutageLogins reads the organization's setting.
func (s *Store) OutageLogins(ctx context.Context) (OutageLogins, error) {
	var o OutageLogins
	err := s.db(ctx).QueryRow(ctx, `
		SELECT outage_logins_after, outage_logins_groups,
		       to_char(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), updated_by
		  FROM gateway_policy WHERE id = TRUE`).Scan(&o.AfterMinutes, &o.Groups, &o.UpdatedAt, &o.UpdatedBy)
	if err != nil {
		return OutageLogins{}, fmt.Errorf("read outage logins: %w", err)
	}
	return o, nil
}

// SetOutageLogins stores the setting and returns the one it replaced.
func (s *Store) SetOutageLogins(ctx context.Context, o OutageLogins, by string) (before OutageLogins, err error) {
	if !slices.Contains(OutageLoginsChoices, o.AfterMinutes) {
		return before, ErrInvalidOutageLogins
	}
	if o.Groups, err = cleanOutageGroups(o.Groups); err != nil {
		return before, err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return before, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `SELECT outage_logins_after, outage_logins_groups
		  FROM gateway_policy WHERE id = TRUE FOR UPDATE`).Scan(&before.AfterMinutes, &before.Groups); err != nil {
		return before, fmt.Errorf("read outage logins: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE gateway_policy SET outage_logins_after = $1,
		outage_logins_groups = $2, updated_at = now(), updated_by = $3 WHERE id = TRUE`,
		o.AfterMinutes, o.Groups, by); err != nil {
		return before, fmt.Errorf("save outage logins: %w", err)
	}
	return before, tx.Commit(ctx)
}

// OutageLoginsFor is the setting and whether the server reporting as
// hostname turned it off for itself, matched the way a heartbeat matches.
func (s *Store) OutageLoginsFor(ctx context.Context, hostname string) (OutageLogins, bool, error) {
	var (
		o   OutageLogins
		off *bool
	)
	err := s.db(ctx).QueryRow(ctx, `
		SELECT p.outage_logins_after, p.outage_logins_groups, a.outage_logins_off
		  FROM gateway_policy p
		  LEFT JOIN LATERAL (
		       SELECT outage_logins_off FROM assets
		        WHERE archived_at IS NULL
		          AND (hostname = $1 OR agent_hostname = $1 OR split_part(hostname, '.', 1) = $1)
		        ORDER BY (hostname = $1) DESC, (agent_hostname = $1) DESC
		        LIMIT 1) a ON TRUE
		 WHERE p.id = TRUE`, hostname).Scan(&o.AfterMinutes, &o.Groups, &off)
	if err != nil {
		return OutageLogins{}, false, fmt.Errorf("read outage logins: %w", err)
	}
	return o, off != nil && *off, nil
}

// AssetOutageLoginsOff reads whether a server turned the setting off.
func (s *Store) AssetOutageLoginsOff(ctx context.Context, id string) (bool, error) {
	var off bool
	err := s.db(ctx).QueryRow(ctx, `SELECT outage_logins_off FROM assets
		WHERE id::text = $1 AND archived_at IS NULL`, id).Scan(&off)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrAssetNotFound
	}
	return off, err
}

// SetAssetOutageLoginsOff turns the setting off, or back to the
// organization's, for one server, and reports whether that changed anything.
func (s *Store) SetAssetOutageLoginsOff(ctx context.Context, id string, off bool) (bool, error) {
	var before bool
	err := s.db(ctx).QueryRow(ctx, `
		UPDATE assets n SET outage_logins_off = $2, updated_at = now()
		  FROM assets o
		 WHERE n.id = o.id AND n.id::text = $1 AND n.archived_at IS NULL
		RETURNING o.outage_logins_off`, id, off).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrAssetNotFound
	}
	return before != off, err
}

// OutageLoginsReach counts the organization's servers that refuse plain SSH
// while Argus is down and would let the named groups in: refusing, with the
// setting on and the server not opted out.
func (s *Store) OutageLoginsReach(ctx context.Context) (int, error) {
	var n int
	err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM assets a, gateway_policy p
		WHERE p.id = TRUE AND p.outage_logins_after > 0 AND a.archived_at IS NULL
		  AND NOT a.outage_logins_off AND a.agent_direct_ssh IN ('refuse', 'approve')`).Scan(&n)
	return n, err
}

/* ── Describing it ───────────────────────────────────────────────────────── */

func outageLoginsLabel(o OutageLogins) string {
	if !o.On() {
		return "off"
	}
	return fmt.Sprintf("members of %s after %s without Argus", strings.Join(o.Groups, ", "),
		minutesLabel(o.AfterMinutes))
}

func minutesLabel(m int) string {
	switch {
	case m == 60:
		return "1 hour"
	case m == 1:
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) getOutageLogins(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(r); !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	o, err := a.store.OutageLogins(r.Context())
	if err != nil {
		a.fail(w, "outage logins", err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (a *API) putOutageLogins(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing who may log in while Argus cannot be reached", "admin", "owner") {
		return
	}
	var in OutageLogins
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := a.store.SetOutageLogins(r.Context(), in, sess.Email)
	switch {
	case errors.Is(err, ErrInvalidOutageLogins), errors.Is(err, ErrInvalidOutageGroups):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		a.fail(w, "outage logins", err)
		return
	}
	after, err := a.store.OutageLogins(r.Context())
	if err != nil {
		a.fail(w, "outage logins", err)
		return
	}
	if outageLoginsLabel(before) != outageLoginsLabel(after) {
		e := AuditEvent{Action: "policy.outage_logins_changed", Severity: "notice", ActorEmail: sess.Email,
			Target: "servers", Detail: fmt.Sprintf("Set logins while Argus cannot be reached from %s to %s.",
				outageLoginsLabel(before), outageLoginsLabel(after))}
		if after.On() {
			e.Severity = "warning"
			e.Detail += " Servers that refuse direct SSH let these groups in, recorded, once they " +
				"have not heard from Argus for that long."
		}
		a.auditAsset(r, e)
	}
	writeJSON(w, http.StatusOK, after)
}

// assetOutageLogins is one server's view: the organization's setting, and
// whether this server turned it off.
type assetOutageLogins struct {
	OutageLogins
	Off bool `json:"off"`
}

func (a *API) getAssetOutageLogins(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(r); !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	off, err := a.store.AssetOutageLoginsOff(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "outage logins", err)
		return
	}
	o, err := a.store.OutageLogins(r.Context())
	if err != nil {
		a.fail(w, "outage logins", err)
		return
	}
	writeJSON(w, http.StatusOK, assetOutageLogins{OutageLogins: o, Off: off})
}

func (a *API) putAssetOutageLogins(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing who may log in while Argus cannot be reached", "admin", "owner") {
		return
	}
	var in struct {
		Off *bool `json:"off"`
	}
	if err := decode(r, &in); err != nil || in.Off == nil {
		writeErr(w, http.StatusBadRequest, `send {"off": true} or {"off": false}`)
		return
	}
	id := r.PathValue("id")
	changed, err := a.store.SetAssetOutageLoginsOff(r.Context(), id, *in.Off)
	if err != nil {
		a.failAsset(w, "outage logins", err)
		return
	}
	asset, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	if changed {
		e := AuditEvent{Action: "asset.outage_logins_changed", Severity: "notice", ActorEmail: sess.Email,
			Target: asset.Hostname, Detail: fmt.Sprintf("Turned off logins while Argus cannot be reached on %s: "+
				"it refuses everyone while Argus is down.", asset.Hostname)}
		if !*in.Off {
			e.Severity = "warning"
			e.Detail = fmt.Sprintf("%s follows the organization's setting for logins while Argus cannot "+
				"be reached again.", asset.Hostname)
		}
		a.auditAsset(r, e)
	}
	o, err := a.store.OutageLogins(r.Context())
	if err != nil {
		a.fail(w, "outage logins", err)
		return
	}
	writeJSON(w, http.StatusOK, assetOutageLogins{OutageLogins: o, Off: *in.Off})
}

// outageSessionAdmitted checks a direct session the agent says it let in
// because Argus could not be reached: the setting must be on for its server.
// Which groups the account is in only the server knows, so that part is the
// agent's word, and the detail says so.
func (a *API) outageSessionAdmitted(ctx context.Context, s Session) (bool, string) {
	o, off, err := a.store.OutageLoginsFor(ctx, s.AssetHostname)
	if err != nil || off || !o.On() {
		return false, ""
	}
	return true, fmt.Sprintf("Direct login by %s on %s was let in while the server could not reach Argus, "+
		"because the account is in one of %s (as the server reports it) and Argus had been unreachable "+
		"for %s or more. It was recorded as session %s. Nobody approved it.",
		s.Principal, s.AssetHostname, strings.Join(o.Groups, ", "), minutesLabel(o.AfterMinutes), s.ID)
}

// alertOutageSession puts an outage login in the organization's bell, once,
// when its report arrives.
func (a *API) alertOutageSession(ctx context.Context, s Session) {
	a.raiseAlert(ctx, ConsoleAlert{
		Kind: AlertSecurity, Severity: "warning", Audience: AudienceAdmins, Actor: s.Principal,
		Title:  fmt.Sprintf("%s logged in to %s while Argus was unreachable", s.Principal, s.AssetHostname),
		Detail: "Let in because the account is in a group allowed in during an outage. The session was recorded.",
		Link:   "/sessions/" + s.ID,
	})
}

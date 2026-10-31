package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Direct SSH logins: what an agent does with a login that reached sshd without
the console.

	refuse   turned away before anything runs (the default, and what every
	         deployment did before this setting existed)
	approve  held at the server while an admin approves or denies it in the
	         console; silence, a denial or an unreachable control plane all
	         end in a refusal
	allow    let through and recorded, and reported as allowed by the setting

Whatever is chosen, a login that is admitted is recorded. The deployment holds
one choice in gateway_policy and a server may hold its own; the agent learns
the effective one on its heartbeat.

What this cannot do is say who the person was. A direct login carries the local
account and the address it came from, nothing else, so its sessions are
attributed to account@host and the approval names who let it in, not who it
was.
*/

// Direct SSH modes.
const (
	DirectSSHRefuse  = "refuse"
	DirectSSHApprove = "approve"
	DirectSSHAllow   = "allow"
)

// ErrInvalidDirectSSH is a mode that is not one of the three.
var ErrInvalidDirectSSH = errors.New(`direct SSH logins must be "refuse", "approve" or "allow"`)

func validDirectSSH(mode string) bool {
	switch mode {
	case DirectSSHRefuse, DirectSSHApprove, DirectSSHAllow:
		return true
	}
	return false
}

// directSSHRank orders the modes from closed to open, so a change can be
// classified as a loosening.
func directSSHRank(mode string) int {
	switch mode {
	case DirectSSHApprove:
		return 1
	case DirectSSHAllow:
		return 2
	}
	return 0
}

func directSSHLabel(mode string) string {
	switch mode {
	case DirectSSHApprove:
		return "ask for approval"
	case DirectSSHAllow:
		return "allow and record"
	case "":
		return "the deployment's setting"
	}
	return "refuse"
}

func directSSHConsequence(mode string) string {
	switch mode {
	case DirectSSHApprove:
		return "are held until an admin approves them"
	case DirectSSHAllow:
		return "open with no one approving them, recorded but attributed only to the server account"
	}
	return "are refused"
}

// DirectLoginWait is how long a direct login is held for a decision. Long
// enough for an admin to notice and answer; short enough that someone at a
// terminal is not left wondering whether anything is happening.
const DirectLoginWait = 2 * time.Minute

// maxPendingDirectLogins bounds what one host can have waiting. A row comes
// from someone sshd has already authenticated, but a script holding a key can
// still open logins in a loop, and the console's list must stay readable.
const maxPendingDirectLogins = 20

// directLoginPoll is how often a waiting agent's request rechecks the row,
// and directLoginLongPoll how long one request waits before the agent asks
// again. Variables so tests can shorten them.
var (
	directLoginPoll     = 500 * time.Millisecond
	directLoginLongPoll = 25 * time.Second
)

// DirectLogin is one direct login held for approval.
type DirectLogin struct {
	ID             string     `json:"id"`
	Hostname       string     `json:"hostname"`
	Principal      string     `json:"principal"`
	ClientAddr     string     `json:"clientAddr"`
	Command        string     `json:"command"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	DecidedAt      *time.Time `json:"decidedAt"`
	DecidedByEmail *string    `json:"decidedByEmail"`
	DecisionNote   *string    `json:"decisionNote"`
}

var (
	// ErrDirectLoginNotFound means no request has that id.
	ErrDirectLoginNotFound = errors.New("direct login not found")
	// ErrDirectLoginExpired means nobody decided in time and the login was
	// refused.
	ErrDirectLoginExpired = errors.New("this login waited too long and was refused")
	// ErrTooManyDirectLogins means the host already has the most it may have
	// waiting.
	ErrTooManyDirectLogins = errors.New("too many direct logins are already waiting on this server")
)

/* ── Persistence ─────────────────────────────────────────────────────────── */

const directLoginSelect = `
	SELECT id::text, hostname, principal, client_addr, command, state,
	       created_at, expires_at, decided_at, decided_by_email, decision_note
	  FROM direct_login_requests`

func scanDirectLogin(row pgx.Row) (DirectLogin, error) {
	var d DirectLogin
	err := row.Scan(&d.ID, &d.Hostname, &d.Principal, &d.ClientAddr, &d.Command,
		&d.State, &d.CreatedAt, &d.ExpiresAt, &d.DecidedAt, &d.DecidedByEmail, &d.DecisionNote)
	return d, err
}

// EffectiveDirectSSH is the mode the agent reporting as hostname must apply:
// its server's own choice, or the deployment's. Matched the way a heartbeat
// matches, on the inventory hostname, agent_hostname or the short name.
func (s *Store) EffectiveDirectSSH(ctx context.Context, hostname string) (string, error) {
	var mode string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(a.direct_ssh, p.direct_ssh)
		  FROM gateway_policy p
		  LEFT JOIN LATERAL (
		       SELECT direct_ssh FROM assets
		        WHERE archived_at IS NULL
		          AND (hostname = $1 OR agent_hostname = $1 OR split_part(hostname, '.', 1) = $1)
		        ORDER BY (hostname = $1) DESC, (agent_hostname = $1) DESC
		        LIMIT 1) a ON TRUE
		 WHERE p.id = TRUE`, hostname).Scan(&mode)
	if err != nil {
		return DirectSSHRefuse, fmt.Errorf("read direct ssh setting: %w", err)
	}
	return mode, nil
}

// SetAssetDirectSSH sets a server's own choice; "" follows the deployment.
// It returns the value it replaced.
func (s *Store) SetAssetDirectSSH(ctx context.Context, id, mode string) (before *string, err error) {
	if mode != "" && !validDirectSSH(mode) {
		return nil, ErrInvalidDirectSSH
	}
	var next *string
	if mode != "" {
		next = &mode
	}
	err = s.pool.QueryRow(ctx, `
		UPDATE assets n SET direct_ssh = $2, updated_at = now()
		  FROM assets o
		 WHERE n.id = o.id AND n.id::text = $1 AND n.archived_at IS NULL
		RETURNING o.direct_ssh`, id, next).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	return before, err
}

// CreateDirectLogin holds a login for approval, unless the host already has
// as many waiting as it may.
func (s *Store) CreateDirectLogin(ctx context.Context, d DirectLogin) (DirectLogin, error) {
	out, err := scanDirectLogin(s.pool.QueryRow(ctx, `
		INSERT INTO direct_login_requests (hostname, principal, client_addr, command, expires_at)
		SELECT $1, $2, $3, $4, now() + $5::interval
		 WHERE (SELECT count(*) FROM direct_login_requests
		         WHERE lower(hostname) = lower($1) AND state = 'pending'
		           AND expires_at > now()) < $6
		RETURNING id::text, hostname, principal, client_addr, command, state,
		          created_at, expires_at, decided_at, decided_by_email, decision_note`,
		d.Hostname, d.Principal, d.ClientAddr, d.Command,
		fmt.Sprintf("%d seconds", int(DirectLoginWait.Seconds())), maxPendingDirectLogins))
	if errors.Is(err, pgx.ErrNoRows) {
		return DirectLogin{}, ErrTooManyDirectLogins
	}
	return out, err
}

// DirectLoginByID reads one request.
func (s *Store) DirectLoginByID(ctx context.Context, id string) (DirectLogin, error) {
	d, err := scanDirectLogin(s.pool.QueryRow(ctx, directLoginSelect+` WHERE id::text = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrDirectLoginNotFound
	}
	return d, err
}

// DecideDirectLogin approves or denies a waiting login.
func (s *Store) DecideDirectLogin(
	ctx context.Context, id string, approve bool, actor, note string,
) (DirectLogin, error) {
	state := "denied"
	if approve {
		state = "approved"
	}
	var notePtr *string
	if note = strings.TrimSpace(note); note != "" {
		notePtr = &note
	}
	d, err := scanDirectLogin(s.pool.QueryRow(ctx, `
		UPDATE direct_login_requests
		   SET state = $2, decided_at = now(), decided_by_email = $3, decision_note = $4
		 WHERE id::text = $1 AND state = 'pending' AND expires_at > now()
		RETURNING id::text, hostname, principal, client_addr, command, state,
		          created_at, expires_at, decided_at, decided_by_email, decision_note`,
		id, state, actor, notePtr))
	if !errors.Is(err, pgx.ErrNoRows) {
		return d, err
	}
	// Not decided: say why.
	cur, err := s.DirectLoginByID(ctx, id)
	if err != nil {
		return cur, err
	}
	if cur.State == "pending" || cur.State == "expired" {
		return cur, ErrDirectLoginExpired
	}
	return cur, ErrAlreadyDecided
}

// ExpireDirectLogins closes every waiting login whose time has run out and
// returns them, so each can be written to the audit log exactly once.
func (s *Store) ExpireDirectLogins(ctx context.Context) ([]DirectLogin, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE direct_login_requests SET state = 'expired', decided_at = now()
		 WHERE state = 'pending' AND expires_at <= now()
		RETURNING id::text, hostname, principal, client_addr, command, state,
		          created_at, expires_at, decided_at, decided_by_email, decision_note`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DirectLogin
	for rows.Next() {
		d, err := scanDirectLogin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DirectLogins lists the most recent requests, newest first.
func (s *Store) DirectLogins(ctx context.Context, limit int) ([]DirectLogin, error) {
	rows, err := s.pool.Query(ctx, directLoginSelect+` ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DirectLogin{}
	for rows.Next() {
		d, err := scanDirectLogin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

/* ── Agent routes ────────────────────────────────────────────────────────── */

func describeDirectLogin(d DirectLogin) string {
	what := "an interactive session"
	if d.Command != "" {
		what = fmt.Sprintf("the command %q", d.Command)
	}
	from := d.ClientAddr
	if from == "" {
		from = "an unknown address"
	}
	return fmt.Sprintf("%s on %s from %s, to run %s", d.Principal, d.Hostname, from, what)
}

// expireAndAudit closes timed-out requests and writes each to the audit log.
func (a *API) expireAndAudit(ctx context.Context) {
	expired, err := a.store.ExpireDirectLogins(ctx)
	if err != nil {
		a.log.Error("expire direct logins", "error", err)
		return
	}
	for _, d := range expired {
		if _, err := a.store.AppendAudit(ctx, AuditEvent{
			Action:   "session.direct_expired",
			Severity: "notice",
			Target:   d.Hostname,
			Detail: "Nobody approved the direct login by " + describeDirectLogin(d) +
				" within " + DirectLoginWait.String() + ", so the agent refused it.",
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
	}
}

// postDirectLogin is an agent asking whether a direct login may go ahead.
//
// The agent asks because its last heartbeat said "approve", but the answer is
// decided here from the setting as it is now: a server switched to refuse
// since the last heartbeat is refused, not held.
func (a *API) postDirectLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hostname   string `json:"hostname"`
		Principal  string `json:"principal"`
		ClientAddr string `json:"client_addr"`
		Command    string `json:"command"`
	}
	if err := decodeReport(r, &in, a.log, "report/direct-login"); err != nil {
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
	// Bounded so a login cannot write an unbounded row or audit entry.
	if len(in.Principal) > 256 || len(in.ClientAddr) > 256 || len(in.Command) > 4096 {
		writeErr(w, http.StatusBadRequest, "principal, client_addr or command is too long")
		return
	}

	mode, err := a.store.EffectiveDirectSSH(r.Context(), in.Hostname)
	if err != nil {
		// Fail closed: a setting that cannot be read cannot let anyone in.
		a.log.Error("direct login: cannot read the setting; refusing", "error", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"state": "denied", "reason": "Argus could not check this server's setting",
		})
		return
	}
	switch mode {
	case DirectSSHAllow:
		writeJSON(w, http.StatusOK, map[string]any{"state": "approved", "mode": mode})
		return
	case DirectSSHApprove:
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"state": "denied", "mode": mode,
			"reason": "this server accepts sessions from the Argus console only",
		})
		return
	}

	d, err := a.store.CreateDirectLogin(r.Context(), DirectLogin{
		Hostname: in.Hostname, Principal: in.Principal,
		ClientAddr: in.ClientAddr, Command: in.Command,
	})
	if errors.Is(err, ErrTooManyDirectLogins) {
		writeJSON(w, http.StatusOK, map[string]any{"state": "denied", "reason": err.Error()})
		return
	}
	if err != nil {
		a.fail(w, "create direct login", err)
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:   "session.direct_requested",
		Severity: "warning",
		Target:   d.Hostname,
		Detail: "Direct login waiting for approval: " + describeDirectLogin(d) +
			". sshd has already accepted the account's key or password; the agent " +
			"holds the login until an admin decides.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	a.log.Warn("direct login waiting for approval",
		"id", d.ID, "hostname", d.Hostname, "principal", d.Principal, "client", d.ClientAddr)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": d.ID, "state": d.State, "expiresAt": d.ExpiresAt,
	})
}

// postDirectLoginWait holds an agent's request until the login is decided or
// a long-poll interval passes, then says where it stands.
func (a *API) postDirectLoginWait(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, err := a.store.DirectLoginByID(r.Context(), id)
	if errors.Is(err, ErrDirectLoginNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "direct login", err)
		return
	}
	if !a.mayReportFor(w, r, d.Hostname) {
		return
	}

	deadline := time.Now().Add(directLoginLongPoll)
	t := time.NewTicker(directLoginPoll)
	defer t.Stop()
	for d.State == "pending" {
		if !time.Now().Before(d.ExpiresAt) {
			a.expireAndAudit(r.Context())
			if d, err = a.store.DirectLoginByID(r.Context(), id); err != nil {
				a.fail(w, "direct login", err)
				return
			}
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
		if d, err = a.store.DirectLoginByID(r.Context(), id); err != nil {
			a.fail(w, "direct login", err)
			return
		}
	}
	out := map[string]any{"id": d.ID, "state": d.State, "expiresAt": d.ExpiresAt}
	if d.DecidedByEmail != nil {
		out["decidedBy"] = *d.DecidedByEmail
	}
	if d.DecisionNote != nil {
		out["note"] = *d.DecisionNote
	}
	writeJSON(w, http.StatusOK, out)
}

/* ── Console routes ──────────────────────────────────────────────────────── */

// getDirectLogins lists recent direct logins for the console. Readable by any
// signed-in user, as the audit log is: an auditor needs to see what was let in.
func (a *API) getDirectLogins(w http.ResponseWriter, r *http.Request, _ string) {
	a.expireAndAudit(r.Context())
	list, err := a.store.DirectLogins(r.Context(), 100)
	if err != nil {
		a.fail(w, "direct logins", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// postDirectLoginDecision approves or denies a waiting direct login. The roles
// that decide access requests decide these too, and every decision is audited
// with who made it.
func (a *API) postDirectLoginDecision(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "deciding a direct login", "approver", "admin", "owner") {
		return
	}
	var in struct {
		Approve bool   `json:"approve"`
		Note    string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(in.Note) > 1000 {
		writeErr(w, http.StatusBadRequest, "note is too long")
		return
	}
	d, err := a.store.DecideDirectLogin(r.Context(), r.PathValue("id"), in.Approve, sess.Email, in.Note)
	switch {
	case errors.Is(err, ErrDirectLoginNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrDirectLoginExpired):
		a.expireAndAudit(r.Context())
		writeErr(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, ErrAlreadyDecided):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		a.fail(w, "decide direct login", err)
		return
	}

	e := AuditEvent{
		Action:     "session.direct_denied",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     d.Hostname,
		Detail:     sess.Email + " denied the direct login by " + describeDirectLogin(d) + ".",
	}
	if in.Approve {
		e.Action, e.Severity = "session.direct_approved", "warning"
		e.Detail = sess.Email + " approved the direct login by " + describeDirectLogin(d) +
			". It is recorded, and attributed to the server account: Argus does not " +
			"know which person holds that key or password."
	}
	if d.DecisionNote != nil {
		e.Detail += " Note: " + *d.DecisionNote
	}
	if _, err := a.store.AppendAudit(r.Context(), e); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	writeJSON(w, http.StatusOK, d)
}

// putAssetDirectSSH sets a server's own direct SSH choice, or clears it so
// the server follows the deployment's.
func (a *API) putAssetDirectSSH(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing direct SSH logins", "admin", "owner") {
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Mode != "" && !validDirectSSH(in.Mode) {
		writeErr(w, http.StatusBadRequest, ErrInvalidDirectSSH.Error())
		return
	}
	id := r.PathValue("id")
	prev, err := a.store.SetAssetDirectSSH(r.Context(), id, in.Mode)
	if err != nil {
		a.failAsset(w, "set direct ssh", err)
		return
	}
	asset, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}

	// Loosening is judged on what the server actually does, before and after,
	// so clearing an override back to an open deployment setting counts too.
	policy, _ := a.store.GatewayPolicy(r.Context())
	was, now := policy.DirectSSH, policy.DirectSSH
	wasLabel := directSSHLabel("")
	if prev != nil {
		was, wasLabel = *prev, directSSHLabel(*prev)
	}
	if in.Mode != "" {
		now = in.Mode
	}
	detail := fmt.Sprintf("Set direct SSH logins on %s from %s to %s.",
		asset.Hostname, wasLabel, directSSHLabel(in.Mode))
	if in.Mode == "" {
		detail = fmt.Sprintf("Set direct SSH logins on %s from %s to the deployment's setting (%s).",
			asset.Hostname, wasLabel, directSSHLabel(policy.DirectSSH))
	}
	severity := "notice"
	if directSSHRank(now) > directSSHRank(was) {
		severity = "warning"
		detail += " Logins that reach sshd without the console now " + directSSHConsequence(now) + "."
	}
	a.auditAsset(r, AuditEvent{
		Action:     "asset.direct_ssh_changed",
		Severity:   severity,
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail:     detail,
	})
	writeJSON(w, http.StatusOK, asset)
}

// directSessionAdmitted reports whether a finished direct session was let in
// by the setting, and if so describes how. The agent's claim is checked
// against what is held here: an approval that does not exist, is for another
// host or account, or was not granted, and an "allowed" from a server not set
// to allow, are not believed, and the session stays a critical bypass.
func (a *API) directSessionAdmitted(
	ctx context.Context, s Session, approvalID, approval string,
) (bool, string) {
	switch approval {
	case "approved":
		d, err := a.store.DirectLoginByID(ctx, approvalID)
		if err != nil || d.State != "approved" ||
			!strings.EqualFold(d.Hostname, s.AssetHostname) || d.Principal != s.Principal {
			return false, ""
		}
		by := ""
		if d.DecidedByEmail != nil {
			by = *d.DecidedByEmail
		}
		return true, fmt.Sprintf("Direct login by %s on %s, approved by %s, was recorded as session %s.",
			s.Principal, s.AssetHostname, by, s.ID)
	case "personal":
		p, ok, err := a.store.PersonalOwner(ctx, s.AssetHostname, s.Principal)
		if err != nil || !ok || p.Direct != DirectSSHAllow || !strings.EqualFold(p.Owner, s.UserEmail) {
			return false, ""
		}
		return true, fmt.Sprintf("Direct login by %s to their personal account %s on %s was let in "+
			"because a team of theirs may SSH directly to it, and was recorded as session %s. "+
			"Nobody approved it.", p.Owner, s.Principal, s.AssetHostname, s.ID)
	case "allowed":
		mode, err := a.store.EffectiveDirectSSH(ctx, s.AssetHostname)
		if err != nil || mode != DirectSSHAllow {
			return false, ""
		}
		return true, fmt.Sprintf("Direct login by %s on %s was let in because the server is set to "+
			"allow and record direct SSH logins, and was recorded as session %s. Nobody approved it.",
			s.Principal, s.AssetHostname, s.ID)
	}
	return false, ""
}

// directSessionOwner is who a direct session is attributed to: the owner of
// the personal account it used when the agent names them and the control
// plane agrees, otherwise the account on the host, as every direct session
// was before personal accounts.
func (a *API) directSessionOwner(ctx context.Context, s Session) string {
	account := s.Principal + "@" + s.AssetHostname
	if s.UserEmail == "" || s.UserEmail == account {
		return account
	}
	p, ok, err := a.store.PersonalOwner(ctx, s.AssetHostname, s.Principal)
	if err != nil || !ok || !strings.EqualFold(p.Owner, s.UserEmail) {
		a.log.Warn("direct session names a person the control plane does not know for that account",
			"session", s.ID, "host", s.AssetHostname, "principal", s.Principal, "claimed", s.UserEmail)
		return account
	}
	return p.Owner
}

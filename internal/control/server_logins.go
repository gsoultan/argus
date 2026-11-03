package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Login accounts an administrator makes on a server from the console.

A server's Login accounts list says which accounts Argus brokers there, but
until now every one of them had to exist on the server already: someone with
root made it by hand, outside Argus and outside its audit log. Now an
administrator can have the server's agent make one, the way it makes personal
accounts, and take it away again.

Taking it away locks it. Nobody can log in, whatever runs as it is stopped,
and its home and files stay, so what it did can still be read. Deleting it
for good, home folder and all, is a second, separate step on a locked
account, and cannot be undone. The agent only ever touches accounts it made
(see internal/agent/personal.go): an account the server already had is
reported as taken, never adopted.

Creating, locking, unlocking and deleting are admin and owner only, and each
is an audit entry naming who did it and on which server. Making an account
also puts it on the server's Login accounts list, so it can be assigned and
asked for; locking or deleting takes it off.
*/

// serverLoginOwner is how a shared login's state is told apart from a
// personal account's in what the agent reports.
const serverLoginOwner = "shared login"

// ServerLogin is one console-made login account on a server, with what the
// agent last said about it.
type ServerLogin struct {
	Name      string    `json:"name"`
	State     string    `json:"state"` // active, locked, delete, deleted
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	ChangedBy string    `json:"changedBy"`
	ChangedAt time.Time `json:"changedAt"`
	// Agent is the agent's own word on the account: ready, locked, deleted,
	// taken or failed. Empty until it has said anything.
	Agent       string `json:"agent"`
	AgentDetail string `json:"agentDetail,omitempty"`
}

// ServerLoginsWanted is what a heartbeat tells the agent to do with them.
type ServerLoginsWanted struct {
	Keep   []string `json:"keep"`
	Delete []string `json:"delete"`
}

var (
	// ErrServerLoginNotFound is a name the server has no console-made login by.
	ErrServerLoginNotFound = errors.New("no login account by that name was made on this server from Argus")
	// errServerLoginNotLocked guards delete-for-good.
	errServerLoginNotLocked = ErrInvalidAsset{Reason: "remove the account first: only a locked account can be deleted for good"}
)

// ServerLogins lists the console-made logins on asset id, newest first among
// equals, with the agent's last word on each.
func (s *Store) ServerLogins(ctx context.Context, assetID string) ([]ServerLogin, error) {
	var states []byte
	err := s.db(ctx).QueryRow(ctx,
		`SELECT agent_personal_accounts FROM assets WHERE id::text = $1 AND archived_at IS NULL`,
		assetID).Scan(&states)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	agent := map[string]PersonalAccountState{}
	if len(states) > 0 {
		var list []PersonalAccountState
		if json.Unmarshal(states, &list) == nil {
			for _, st := range list {
				agent[st.Name] = st
			}
		}
	}

	rows, err := s.db(ctx).Query(ctx, `
		SELECT name, state, created_by, created_at, changed_by, changed_at
		  FROM server_logins WHERE asset_id::text = $1
		 ORDER BY (state = 'deleted'), name`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServerLogin{}
	for rows.Next() {
		var l ServerLogin
		if err := rows.Scan(&l.Name, &l.State, &l.CreatedBy, &l.CreatedAt, &l.ChangedBy, &l.ChangedAt); err != nil {
			return nil, err
		}
		if st, ok := agent[l.Name]; ok {
			l.Agent, l.AgentDetail = st.State, st.Detail
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// validServerLoginName refuses what Linux would, what a server already has,
// and what a person already holds as their own account name.
func (s *Store) validServerLoginName(ctx context.Context, name string) error {
	if !reLoginName.MatchString(name) {
		return errInvalidLoginName
	}
	if reservedLoginNames[name] {
		return ErrInvalidAsset{Reason: fmt.Sprintf(
			"%q is an account servers already have or reserve; Argus does not make or remove it", name)}
	}
	var personal bool
	if err := s.db(ctx).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE login_name = $1)`, name).Scan(&personal); err != nil {
		return err
	}
	if personal {
		return ErrInvalidAsset{Reason: fmt.Sprintf(
			"%q is someone's personal account name; pick another name", name)}
	}
	return nil
}

// CreateServerLogin asks the server's agent to make a login account, or to
// unlock one it made before. The name joins the server's Login accounts list.
// It returns the asset's hostname and whether the account was unlocked
// rather than new.
func (s *Store) CreateServerLogin(ctx context.Context, assetID, name, actor string) (host string, reopened bool, err error) {
	name = strings.TrimSpace(name)
	if err := s.validServerLoginName(ctx, name); err != nil {
		return "", false, err
	}
	actor = strings.ToLower(actor)
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var principals []string
	err = tx.QueryRow(ctx, `
		SELECT hostname, principals FROM assets
		 WHERE id::text = $1 AND archived_at IS NULL FOR UPDATE`, assetID).Scan(&host, &principals)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrAssetNotFound
	}
	if err != nil {
		return "", false, err
	}

	var before string
	err = tx.QueryRow(ctx, `SELECT state FROM server_logins WHERE asset_id::text = $1 AND name = $2`,
		assetID, name).Scan(&before)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// An account the server lists but Argus did not make is someone
		// else's. Making it here would mean adopting it.
		if slices.Contains(principals, name) {
			return "", false, ErrInvalidAsset{Reason: fmt.Sprintf(
				"%s already lists %q as a login account; Argus makes only new accounts", host, name)}
		}
	case err != nil:
		return "", false, err
	case before == "active":
		return "", false, ErrInvalidAsset{Reason: fmt.Sprintf("%q is already a login account on %s", name, host)}
	case before == "delete":
		return "", false, ErrInvalidAsset{Reason: fmt.Sprintf(
			"%q is being deleted from %s; wait until its agent says it is gone", name, host)}
	}
	reopened = before == "locked"

	if _, err := tx.Exec(ctx, `
		INSERT INTO server_logins (asset_id, name, state, created_by, changed_by)
		VALUES ($1::uuid, $2, 'active', $3, $3)
		ON CONFLICT (asset_id, name) DO UPDATE
		   SET state = 'active', changed_by = $3, changed_at = now(),
		       created_by = CASE WHEN server_logins.state = 'deleted' THEN $3 ELSE server_logins.created_by END,
		       created_at = CASE WHEN server_logins.state = 'deleted' THEN now() ELSE server_logins.created_at END`,
		assetID, name, actor); err != nil {
		return "", false, err
	}
	if !slices.Contains(principals, name) {
		if len(principals) >= maxPrincipals {
			return "", false, ErrInvalidAsset{Reason: fmt.Sprintf(
				"%s already lists %d login accounts, the most a server may", host, maxPrincipals)}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE assets SET principals = array_append(principals, $2) WHERE id::text = $1`,
			assetID, name); err != nil {
			return "", false, err
		}
	}
	return host, reopened, tx.Commit(ctx)
}

// RemoveServerLogin locks a console-made login, or, with forGood, deletes a
// locked one and its home. Either way it leaves the Login accounts list.
func (s *Store) RemoveServerLogin(ctx context.Context, assetID, name, actor string, forGood bool) (host string, err error) {
	actor = strings.ToLower(actor)
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		SELECT hostname FROM assets WHERE id::text = $1 AND archived_at IS NULL FOR UPDATE`,
		assetID).Scan(&host)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAssetNotFound
	}
	if err != nil {
		return "", err
	}
	var state string
	err = tx.QueryRow(ctx, `SELECT state FROM server_logins WHERE asset_id::text = $1 AND name = $2 FOR UPDATE`,
		assetID, name).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrServerLoginNotFound
	}
	if err != nil {
		return "", err
	}
	next := "locked"
	if forGood {
		if state != "locked" {
			return "", errServerLoginNotLocked
		}
		next = "delete"
	} else if state != "active" {
		return "", ErrInvalidAsset{Reason: fmt.Sprintf("%q is not an active login account on %s", name, host)}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE server_logins SET state = $3, changed_by = $4, changed_at = now()
		 WHERE asset_id::text = $1 AND name = $2`, assetID, name, next, actor); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE assets SET principals = array_remove(principals, $2) WHERE id::text = $1`,
		assetID, name); err != nil {
		return "", err
	}
	return host, tx.Commit(ctx)
}

// ServerLoginsFor is what a heartbeat from hostname tells its agent to keep
// and to delete. A name that is also a personal account here is left out:
// it is the person's, and the shared login cannot be made under it.
func (s *Store) ServerLoginsFor(ctx context.Context, hostname string, personal []PersonalAccount) (ServerLoginsWanted, error) {
	out := ServerLoginsWanted{Keep: []string{}, Delete: []string{}}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT name, state FROM server_logins
		 WHERE asset_id = (`+heartbeatAssetSQL+`) AND state IN ('active', 'delete')
		 ORDER BY name`, hostname)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, state string
		if err := rows.Scan(&name, &state); err != nil {
			return out, err
		}
		if slices.ContainsFunc(personal, func(p PersonalAccount) bool { return p.Name == name }) {
			continue
		}
		if state == "active" {
			out.Keep = append(out.Keep, name)
		} else {
			out.Delete = append(out.Delete, name)
		}
	}
	return out, rows.Err()
}

// RecordServerLoginsDeleted marks as deleted each login the agent said it
// deleted, and returns their names so each can be audited once.
func (s *Store) RecordServerLoginsDeleted(ctx context.Context, hostname string, states []PersonalAccountState) ([]string, error) {
	var names []string
	for _, st := range states {
		if st.State == "deleted" {
			names = append(names, st.Name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.db(ctx).Query(ctx, `
		UPDATE server_logins SET state = 'deleted', changed_at = now()
		 WHERE asset_id = (`+heartbeatAssetSQL+`) AND state = 'delete' AND name = ANY($2)
		RETURNING name`, hostname, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var done []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		done = append(done, n)
	}
	return done, rows.Err()
}

/* ── Routes ─────────────────────────────────────────────────────────────── */

// getServerLogins lists them for whoever sees the whole fleet.
func (a *API) getServerLogins(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing a server's login accounts requires the admin, owner or auditor role")
		return
	}
	out, err := a.store.ServerLogins(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "server logins", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// postServerLogin makes one, or unlocks one made before.
func (a *API) postServerLogin(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "making a login account on a server", "admin", "owner") {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(in.Name)
	host, reopened, err := a.store.CreateServerLogin(r.Context(), r.PathValue("id"), name, sess.Email)
	if err != nil {
		a.failAsset(w, "create server login", err)
		return
	}
	action, detail := "server_login.created",
		"Asked the agent on "+host+" to make the login account "+name+", and added it to the server's login accounts."
	if reopened {
		action, detail = "server_login.unlocked",
			"Asked the agent on "+host+" to unlock the login account "+name+", and added it back to the server's login accounts."
	}
	a.auditAsset(r, AuditEvent{Action: action, Severity: "warning", ActorEmail: sess.Email, Target: host, Detail: detail})
	a.writeServerLogins(w, r)
}

// deleteServerLogin locks one; ?forGood=true deletes a locked one and its
// home, and must name the account again in ?confirm= so a slip cannot.
func (a *API) deleteServerLogin(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "removing a login account from a server", "admin", "owner") {
		return
	}
	name := r.PathValue("name")
	forGood := r.URL.Query().Get("forGood") == "true"
	if forGood && r.URL.Query().Get("confirm") != name {
		writeErr(w, http.StatusBadRequest, "type the account's name to delete it for good")
		return
	}
	host, err := a.store.RemoveServerLogin(r.Context(), r.PathValue("id"), name, sess.Email, forGood)
	if errors.Is(err, ErrServerLoginNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.failAsset(w, "remove server login", err)
		return
	}
	action, severity, detail := "server_login.locked", "warning",
		"Asked the agent on "+host+" to lock the login account "+name+
			" and stop what runs as it; its home and files stay. Removed it from the server's login accounts."
	if forGood {
		action, severity, detail = "server_login.delete_requested", "critical",
			"Asked the agent on "+host+" to delete the login account "+name+" and its home folder for good."
	}
	a.auditAsset(r, AuditEvent{Action: action, Severity: severity, ActorEmail: sess.Email, Target: host, Detail: detail})
	a.writeServerLogins(w, r)
}

func (a *API) writeServerLogins(w http.ResponseWriter, r *http.Request) {
	out, err := a.store.ServerLogins(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "server logins", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// recordServerLoginsDeleted closes each delete the agent reports done, with
// an audit entry saying it happened rather than only that it was asked for.
func (a *API) recordServerLoginsDeleted(r *http.Request, hostname string, states []PersonalAccountState) {
	done, err := a.store.RecordServerLoginsDeleted(r.Context(), hostname, states)
	if err != nil {
		a.log.Error("heartbeat: cannot record deleted server logins", "error", err)
		return
	}
	for _, name := range done {
		a.auditAsset(r, AuditEvent{
			Action: "server_login.deleted", Severity: "critical", ActorEmail: "agent:" + hostname,
			Target: hostname, Detail: "The agent on " + hostname + " deleted the login account " + name + " and its home folder.",
		})
	}
}

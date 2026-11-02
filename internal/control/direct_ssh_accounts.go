package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"
)

/*
Direct SSH logins, per login account.

A server's direct SSH setting used to apply to every account on it. A server
can now name accounts that do something else: staging allowed and recorded,
sys-admin held for an approver, root refused. An account it does not name
follows the server's setting, and that follows the deployment's:

	account's own  >  server's own  >  deployment's

The account is the one sshd authenticated, which is all a direct login says
about who it is, so this is a setting about accounts, not people.
*/

// maxDirectSSHAccounts bounds how many accounts one server may name, so the
// map every heartbeat carries stays small.
const maxDirectSSHAccounts = 64

// reDirectSSHAccount is what a login account name may be: a POSIX name, with
// capitals allowed for the accounts Windows' OpenSSH logs in to.
var reDirectSSHAccount = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,63}$`)

var (
	// ErrInvalidDirectSSHAccount is an account name that cannot be a login.
	ErrInvalidDirectSSHAccount = errors.New("an account name is letters, digits, '.', '_' or '-', " +
		"starts with a letter or '_', and is at most 64 characters")
	// ErrTooManyDirectSSHAccounts is a server naming more accounts than it may.
	ErrTooManyDirectSSHAccounts = fmt.Errorf("a server can name at most %d accounts; "+
		"set the server's own setting for the rest", maxDirectSSHAccounts)
)

// DirectSSHSetting is what an agent must apply: Mode for every account, and
// Accounts for the ones its server names.
type DirectSSHSetting struct {
	Mode     string
	Accounts map[string]string
}

// For is the mode for one login account. A value that is not one of the
// three refuses, so a damaged row cannot let anyone in.
func (d DirectSSHSetting) For(account string) string {
	if m, ok := d.Accounts[account]; ok {
		if validDirectSSH(m) {
			return m
		}
		return DirectSSHRefuse
	}
	return d.Mode
}

// Strictest is the most closed mode any account gets. It is what an agent
// too old to read per-account settings is told to apply to everyone, so a
// named account is never more open there than it was asked to be.
func (d DirectSSHSetting) Strictest() string {
	out := d.Mode
	for a := range d.Accounts {
		if m := d.For(a); directSSHRank(m) < directSSHRank(out) {
			out = m
		}
	}
	return out
}

// DirectSSHSettingFor is the setting the agent reporting as hostname must
// apply, matched the way a heartbeat matches.
func (s *Store) DirectSSHSettingFor(ctx context.Context, hostname string) (DirectSSHSetting, error) {
	var (
		out      DirectSSHSetting
		accounts []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(a.direct_ssh, p.direct_ssh), COALESCE(a.direct_ssh_accounts, '{}'::jsonb)
		  FROM gateway_policy p
		  LEFT JOIN LATERAL (
		       SELECT direct_ssh, direct_ssh_accounts FROM assets
		        WHERE archived_at IS NULL
		          AND (hostname = $1 OR agent_hostname = $1 OR split_part(hostname, '.', 1) = $1)
		        ORDER BY (hostname = $1) DESC, (agent_hostname = $1) DESC
		        LIMIT 1) a ON TRUE
		 WHERE p.id = TRUE`, hostname).Scan(&out.Mode, &accounts)
	if err != nil {
		return DirectSSHSetting{Mode: DirectSSHRefuse}, fmt.Errorf("read direct ssh setting: %w", err)
	}
	if err := json.Unmarshal(accounts, &out.Accounts); err != nil {
		return DirectSSHSetting{Mode: DirectSSHRefuse}, fmt.Errorf("read direct ssh accounts: %w", err)
	}
	return out, nil
}

// EffectiveDirectSSHFor is the mode for one login account on hostname.
func (s *Store) EffectiveDirectSSHFor(ctx context.Context, hostname, account string) (string, error) {
	d, err := s.DirectSSHSettingFor(ctx, hostname)
	if err != nil {
		return DirectSSHRefuse, err
	}
	return d.For(account), nil
}

// SetAssetDirectSSHAccount sets what one login account on a server does; ""
// removes the account's own setting so it follows the server's. It returns
// the value it replaced, nil when the account had none.
func (s *Store) SetAssetDirectSSHAccount(ctx context.Context, id, account, mode string) (before *string, err error) {
	if !reDirectSSHAccount.MatchString(account) {
		return nil, ErrInvalidDirectSSHAccount
	}
	if mode != "" && !validDirectSSH(mode) {
		return nil, ErrInvalidDirectSSH
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		named int
		has   bool
	)
	// Locked, so two admins naming the last free slot cannot both get it.
	err = tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM jsonb_object_keys(direct_ssh_accounts)), direct_ssh_accounts ? $2
		  FROM assets WHERE id::text = $1 AND archived_at IS NULL FOR UPDATE`,
		id, account).Scan(&named, &has)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	if mode != "" && !has && named >= maxDirectSSHAccounts {
		return nil, ErrTooManyDirectSSHAccounts
	}
	err = tx.QueryRow(ctx, `
		UPDATE assets n
		   SET direct_ssh_accounts = CASE WHEN $3 = '' THEN n.direct_ssh_accounts - $2
		                                  ELSE n.direct_ssh_accounts || jsonb_build_object($2::text, $3::text) END,
		       updated_at = now()
		  FROM assets o
		 WHERE n.id = o.id AND n.id::text = $1
		RETURNING o.direct_ssh_accounts ->> $2`, id, account, mode).Scan(&before)
	if err != nil {
		return nil, err
	}
	return before, tx.Commit(ctx)
}

// putAssetDirectSSHAccount sets what one login account on a server does with
// a direct SSH login, or clears it so the account follows the server.
func (a *API) putAssetDirectSSHAccount(w http.ResponseWriter, r *http.Request) {
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
	id, account := r.PathValue("id"), r.PathValue("account")
	if !reDirectSSHAccount.MatchString(account) {
		writeErr(w, http.StatusBadRequest, ErrInvalidDirectSSHAccount.Error())
		return
	}
	if in.Mode != "" && !validDirectSSH(in.Mode) {
		writeErr(w, http.StatusBadRequest, ErrInvalidDirectSSH.Error())
		return
	}
	prev, err := a.store.SetAssetDirectSSHAccount(r.Context(), id, account, in.Mode)
	switch {
	case errors.Is(err, ErrTooManyDirectSSHAccounts):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		a.failAsset(w, "set direct ssh account", err)
		return
	}
	asset, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}

	// What the server does for every account it does not name: its own
	// choice, or the deployment's.
	policy, _ := a.store.GatewayPolicy(r.Context())
	server := policy.DirectSSH
	if asset.DirectSSH != nil {
		server = *asset.DirectSSH
	}
	serverLabel := "the server's setting (" + directSSHLabel(server) + ")"

	// Loosening is judged on what the account actually gets, before and
	// after, so clearing a strict account back to an open server counts too.
	was, wasLabel := server, serverLabel
	if prev != nil {
		was, wasLabel = *prev, directSSHLabel(*prev)
	}
	now, nowLabel := server, serverLabel
	if in.Mode != "" {
		now, nowLabel = in.Mode, directSSHLabel(in.Mode)
	}
	detail := fmt.Sprintf("Set direct SSH logins as %s on %s from %s to %s.",
		account, asset.Hostname, wasLabel, nowLabel)
	severity := "notice"
	if directSSHRank(now) > directSSHRank(was) {
		severity = "warning"
		detail += " Logins as " + account + " that reach sshd without the console now " +
			directSSHConsequence(now) + "."
	}
	a.auditAsset(r, AuditEvent{
		Action:     "asset.direct_ssh_account_changed",
		Severity:   severity,
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail:     detail,
	})
	writeJSON(w, http.StatusOK, asset)
}

package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Who can log in to a server directly, and how each direct login signed in.

The agent's posture scan lists every account with a login shell: whether it
has a password, how many keys it holds, and whether sshd as configured would
let it in. It rides the heartbeat inside the posture the agents table already
keeps, so this reads it from there rather than storing it twice.

Each direct login also says how it signed in -- a password, or which key --
when sshd's ExposeAuthInfo is on. The agent checks a key against the
account's authorized_keys before naming it, and that is what reaches here.
*/

// ServerLoginAccount is one account with a login shell on a server.
type ServerLoginAccount struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Keys     int    `json:"keys"`
	CanLogin bool   `json:"canLogin"`
}

// ServerLoginAccounts is what a server's agent last said about who can log in.
type ServerLoginAccounts struct {
	// Reported is false when the agent has not sent the list: it is too old,
	// or has not checked in.
	Reported  bool                 `json:"reported"`
	CheckedAt *time.Time           `json:"checkedAt"`
	Accounts  []ServerLoginAccount `json:"accounts"`
	// Passwords and Keys are whether sshd accepts each; ExposeAuthInfo is
	// whether it tells the agent how a login signed in.
	Passwords      bool `json:"passwords"`
	Keys           bool `json:"keys"`
	ExposeAuthInfo bool `json:"exposeAuthInfo"`
}

// LoginAccountsFor reads the login accounts the agent on asset id reported.
func (s *Store) LoginAccountsFor(ctx context.Context, id string) (ServerLoginAccounts, error) {
	var (
		accounts, auth []byte
		seen           *time.Time
	)
	err := s.db(ctx).QueryRow(ctx, `
		SELECT ag.posture -> 'login_accounts', ag.posture -> 'sshd_auth', ag.last_seen_at
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT posture, last_seen_at FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.id::text = $1 AND a.archived_at IS NULL`, id).Scan(&accounts, &auth, &seen)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerLoginAccounts{}, ErrAssetNotFound
	}
	if err != nil {
		return ServerLoginAccounts{}, err
	}
	out := ServerLoginAccounts{Accounts: []ServerLoginAccount{}}
	if len(accounts) == 0 || string(accounts) == "null" {
		return out, nil
	}
	var list []struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Keys     int    `json:"keys"`
		CanLogin bool   `json:"can_login"`
	}
	if err := json.Unmarshal(accounts, &list); err != nil {
		return out, nil // an agent's report that does not parse is not reported
	}
	var a struct {
		Passwords      bool `json:"passwords"`
		Keys           bool `json:"keys"`
		ExposeAuthInfo bool `json:"expose_auth_info"`
	}
	_ = json.Unmarshal(auth, &a)
	out.Reported, out.CheckedAt = true, seen
	out.Passwords, out.Keys, out.ExposeAuthInfo = a.Passwords, a.Keys, a.ExposeAuthInfo
	for _, l := range list {
		out.Accounts = append(out.Accounts, ServerLoginAccount{
			User: l.User, Password: l.Password, Keys: l.Keys, CanLogin: l.CanLogin,
		})
	}
	return out, nil
}

// getLoginAccounts lists who can log in to a server directly. Readable by
// whoever sees the whole fleet, as who is assigned to it is.
func (a *API) getLoginAccounts(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing who can log in to a host requires the admin, owner or auditor role")
		return
	}
	out, err := a.store.LoginAccountsFor(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "login accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// LoginAuth is how one direct login signed in, as the agent reports it.
type LoginAuth struct {
	Method         string `json:"method"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	KeyType        string `json:"key_type,omitempty"`
	KeyComment     string `json:"key_comment,omitempty"`
	KeyFound       bool   `json:"key_found,omitempty"`
}

// bounded trims what an agent sent to what a row or an audit entry takes.
func (l *LoginAuth) bounded() *LoginAuth {
	if l == nil {
		return nil
	}
	cut := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	return &LoginAuth{
		Method:         cut(l.Method, 64),
		KeyFingerprint: cut(l.KeyFingerprint, 128),
		KeyType:        cut(l.KeyType, 64),
		KeyComment:     cut(l.KeyComment, 256),
		KeyFound:       l.KeyFound,
	}
}

// describe says how a login signed in, for an audit entry. Empty when the
// agent did not say, which an agent older than this does not.
func (l *LoginAuth) describe() string {
	switch {
	case l == nil:
		return ""
	case l.Method == "":
		return " It is not known how it signed in: the server's sshd has ExposeAuthInfo off."
	case l.KeyFingerprint == "":
		return " It signed in with " + l.Method + "."
	}
	key := l.KeyType + " key " + l.KeyFingerprint
	if l.KeyComment != "" {
		key += " (" + l.KeyComment + ")"
	}
	if !l.KeyFound {
		key += ", which is not in the account's authorized_keys now"
	}
	return " It signed in with " + l.Method + " using " + key + "."
}

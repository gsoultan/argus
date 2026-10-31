package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

/*
Personal server accounts, and direct SSH for chosen people.

A team assignment can give each member an account of their own on the server,
named after them (ana.sari for ana.sari@corp.example). The agent creates it,
keeps it in step with the team, and locks it -- never deletes it -- when the
person leaves the team, the team loses the server, or the person is removed
from Argus. Console sessions for that person can open as it, so the recording
is in their name as well as their account's.

That account is also what makes a per-person direct SSH rule mean anything. A
direct login carries only the local account it used; on "ubuntu" that is
nobody in particular, on "ana.sari" it is Ana. So the team's assignment can say
its members may SSH straight to the server -- let in, or held for an admin's
approval each time -- and it only ever opens their personal accounts. The agent
writes a person's public key into their account only while that holds, and
every such login is still recorded and attributed to them. Sudo on a personal
account asks no password, because the account has none to ask for.

The control plane says what should exist; the agent does it and says what it
did. A name already held on the server by an account Argus did not create is
left alone and reported, never taken over.
*/

// PersonalAccount is one person's account on one server, as the agent is told
// to keep it.
type PersonalAccount struct {
	// Name is the local account name.
	Name string `json:"name"`
	// Owner is the person it belongs to.
	Owner string `json:"owner"`
	// Sudo lets the account run anything as root without a password.
	Sudo bool `json:"sudo"`
	// Direct is whether the person may SSH straight to this account:
	// "approve" holds each login for an admin, "allow" lets it in, and ""
	// leaves it to the server's own direct SSH setting. Admitted logins are
	// recorded either way.
	Direct string `json:"direct,omitempty"`
	// Keys go into the account's authorized_keys. Empty unless Direct.
	Keys []string `json:"keys,omitempty"`
}

// PersonalAccountState is what an agent says about one account it manages.
type PersonalAccountState struct {
	Name   string `json:"name"`
	Owner  string `json:"owner,omitempty"`
	State  string `json:"state"` // ready, locked, taken, failed
	Detail string `json:"detail,omitempty"`
}

// errInvalidLoginName is a server account name Linux would refuse.
var errInvalidLoginName = ErrInvalidAsset{Reason: "a server account name is 1 to 32 characters: " +
	"lowercase letters, digits, dot, underscore and hyphen, starting with a letter or underscore"}

// ErrLoginNameTaken is a name another person already holds.
var ErrLoginNameTaken = errors.New("another person already has that server account name")

// errInvalidSSHKey is text that is not one public SSH key.
var errInvalidSSHKey = ErrInvalidAsset{Reason: "paste one public SSH key, " +
	"such as the contents of ~/.ssh/id_ed25519.pub"}

var reLoginName = regexp.MustCompile(`^[a-z_][a-z0-9._-]{0,31}$`)

// reservedLoginNames are accounts a server already has, or that would read as
// something other than a person. An email of root@corp.example must not come
// out as "root".
var reservedLoginNames = map[string]bool{
	"root": true, "admin": true, "administrator": true, "daemon": true, "bin": true,
	"sys": true, "sync": true, "games": true, "man": true, "lp": true, "mail": true,
	"news": true, "uucp": true, "proxy": true, "www-data": true, "backup": true,
	"list": true, "irc": true, "nobody": true, "sshd": true, "ubuntu": true,
	"ec2-user": true, "centos": true, "debian": true, "fedora": true, "admin1": true,
	"postgres": true, "mysql": true, "argus": true, "operator": true, "adm": true,
	"wheel": true, "sudo": true, "systemd-network": true, "messagebus": true,
}

// loginNameBase turns an email into the start of an account name: the part
// before the @, before any +tag, lowercased, with anything Linux refuses
// dropped. Never empty.
func loginNameBase(email string) string {
	local, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	local, _, _ = strings.Cut(local, "+")
	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		}
	}
	name := strings.Trim(b.String(), ".-")
	if name == "" || !(name[0] == '_' || (name[0] >= 'a' && name[0] <= 'z')) {
		name = "u" + name
	}
	// Room for a two-digit suffix inside Linux's 32.
	if len(name) > 30 {
		name = strings.TrimRight(name[:30], ".-")
	}
	return name
}

// loginNameCandidates are the names tried in order for one person.
func loginNameCandidates(email string) []string {
	base := loginNameBase(email)
	out := make([]string, 0, 99)
	for i := 1; i < 100; i++ {
		n := base
		if i > 1 {
			n = fmt.Sprintf("%s%d", base, i)
		}
		if !reservedLoginNames[n] && reLoginName.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

// ensurePersonalLoginNames gives a server account name to everyone who needs
// one and has none: a member of a team holding personal accounts somewhere.
// Called after anything that can create that need, rather than on the
// heartbeat, which must not write.
func (s *Store) ensurePersonalLoginNames(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT u.email
		  FROM users u
		  JOIN team_members m ON m.user_email = lower(u.email)
		  JOIN asset_team_assignments ta ON ta.team_id = m.team_id AND ta.personal_accounts
		 WHERE u.login_name IS NULL`)
	if err != nil {
		return err
	}
	emails, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, e := range emails {
		if _, err := s.ensureLoginName(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// ensureLoginName returns a person's server account name, choosing one if
// they have none. A name once chosen is kept.
func (s *Store) ensureLoginName(ctx context.Context, email string) (string, error) {
	var current *string
	err := s.pool.QueryRow(ctx,
		`SELECT login_name FROM users WHERE lower(email) = lower($1)`, email).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoAccount
	}
	if err != nil {
		return "", err
	}
	if current != nil {
		return *current, nil
	}
	for _, name := range loginNameCandidates(email) {
		tag, err := s.pool.Exec(ctx, `
			UPDATE users SET login_name = $2
			 WHERE lower(email) = lower($1) AND login_name IS NULL`, email, name)
		if err != nil && isUniqueViolation(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			// Named by someone else in the meantime: theirs stands.
			return s.ensureLoginName(ctx, email)
		}
		return name, nil
	}
	return "", fmt.Errorf("no free server account name for %s", email)
}

// SetLoginName renames a person's server account. The old account is locked
// on every server by the next heartbeat and a new one made under this name;
// the old one's history stays where it was.
func (s *Store) SetLoginName(ctx context.Context, email, name string) (before string, err error) {
	name = strings.TrimSpace(name)
	if !reLoginName.MatchString(name) {
		return "", errInvalidLoginName
	}
	if reservedLoginNames[name] {
		return "", ErrInvalidAsset{Reason: fmt.Sprintf(
			"%q is an account servers already have; pick a name of the person's own", name)}
	}
	var old *string
	err = s.pool.QueryRow(ctx, `
		UPDATE users n SET login_name = $2
		  FROM users o
		 WHERE n.id = o.id AND lower(n.email) = lower($1)
		RETURNING o.login_name`, email, name).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoAccount
	}
	if err != nil && isUniqueViolation(err) {
		return "", ErrLoginNameTaken
	}
	if err != nil {
		return "", err
	}
	if old != nil {
		before = *old
	}
	return before, nil
}

// normaliseSSHKey checks that text is one public key and returns it in
// authorized_keys form with its comment. Empty clears.
func normaliseSSHKey(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	if strings.ContainsAny(text, "\r\n") {
		return "", errInvalidSSHKey
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil || len(rest) > 0 {
		return "", errInvalidSSHKey
	}
	// Options (command=, from=, environment=) are the server's to set, not
	// something a person pastes into their profile.
	if len(options) > 0 {
		return "", ErrInvalidAsset{Reason: "paste the key alone, without options such as from= or command="}
	}
	if strings.HasPrefix(key.Type(), "ssh-dss") {
		return "", ErrInvalidAsset{Reason: "DSA keys are refused by current OpenSSH; use an ed25519 key"}
	}
	out := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if c := strings.Join(strings.Fields(comment), " "); c != "" {
		out += " " + c
	}
	return out, nil
}

// SetSSHKey stores a person's own public key. It returns the fingerprint of
// what was stored, or "" when cleared.
func (s *Store) SetSSHKey(ctx context.Context, email, text string) (string, error) {
	key, err := normaliseSSHKey(text)
	if err != nil {
		return "", err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET ssh_public_key = $2 WHERE lower(email) = lower($1)`, email, key)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNoAccount
	}
	return keyFingerprint(key), nil
}

func keyFingerprint(line string) string {
	if line == "" {
		return ""
	}
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

// ServerAccount is what a person sees of their own server account.
type ServerAccount struct {
	LoginName      string `json:"loginName"`
	SSHKey         string `json:"sshKey"`
	SSHFingerprint string `json:"sshFingerprint"`
}

// ServerAccountOf reads one person's server account settings.
func (s *Store) ServerAccountOf(ctx context.Context, email string) (ServerAccount, error) {
	var out ServerAccount
	var name *string
	err := s.pool.QueryRow(ctx, `
		SELECT login_name, ssh_public_key FROM users WHERE lower(email) = lower($1)`,
		email).Scan(&name, &out.SSHKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNoAccount
	}
	if err != nil {
		return out, err
	}
	if name != nil {
		out.LoginName = *name
	}
	out.SSHFingerprint = keyFingerprint(out.SSHKey)
	return out, nil
}

// heartbeatAssetSQL picks the one live asset a heartbeat's hostname stands
// for, the same way EffectiveDirectSSH does, so the account list and the
// direct SSH mode an agent is given always describe the same server.
const heartbeatAssetSQL = `
	SELECT id FROM assets
	 WHERE archived_at IS NULL
	   AND (hostname = $1 OR agent_hostname = $1 OR split_part(hostname, '.', 1) = $1)
	 ORDER BY (hostname = $1) DESC, (agent_hostname = $1) DESC
	 LIMIT 1`

// PersonalAccountsFor lists the personal accounts one server should hold.
//
// Removed people, people who left the team and teams that lost the server
// are simply absent, which is what tells the agent to lock their account.
// A person in two teams with the server holds one account, with whatever is
// most open between them: sudo if either gives it, and allow over approve
// over off for direct SSH.
func (s *Store) PersonalAccountsFor(ctx context.Context, hostname string) ([]PersonalAccount, error) {
	rows, err := s.pool.Query(ctx, `
		WITH a AS (`+heartbeatAssetSQL+`)
		SELECT u.login_name, lower(u.email), bool_or(ta.sudo),
		       max(CASE ta.direct_ssh WHEN 'allow' THEN 2 WHEN 'approve' THEN 1 ELSE 0 END),
		       u.ssh_public_key
		  FROM a
		  JOIN asset_team_assignments ta ON ta.asset_id = a.id AND ta.personal_accounts
		  JOIN team_members m ON m.team_id = ta.team_id
		  JOIN users u ON lower(u.email) = m.user_email
		 WHERE u.disabled_at IS NULL AND u.login_name IS NOT NULL
		 GROUP BY u.login_name, lower(u.email), u.ssh_public_key
		 ORDER BY u.login_name`, hostname)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PersonalAccount{}
	for rows.Next() {
		var p PersonalAccount
		var key string
		var direct int
		if err := rows.Scan(&p.Name, &p.Owner, &p.Sudo, &direct, &key); err != nil {
			return nil, err
		}
		switch direct {
		case 2:
			p.Direct = DirectSSHAllow
		case 1:
			p.Direct = DirectSSHApprove
		}
		// No key, no way in past the console: the setting alone opens nothing.
		if p.Direct != "" && key != "" {
			p.Keys = []string{key}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PersonalOwner says whose personal account a principal is on a host now,
// and how they may reach it directly. It is what a session report naming a
// person for a direct login is checked against.
func (s *Store) PersonalOwner(ctx context.Context, hostname, principal string) (PersonalAccount, bool, error) {
	list, err := s.PersonalAccountsFor(ctx, hostname)
	if err != nil {
		return PersonalAccount{}, false, err
	}
	for _, p := range list {
		if p.Name == principal {
			return p, true, nil
		}
	}
	return PersonalAccount{}, false, nil
}

// RecordPersonalAccountStates keeps what an agent said about its accounts.
func (s *Store) RecordPersonalAccountStates(ctx context.Context, hostname string, states []PersonalAccountState) error {
	b, err := json.Marshal(states)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE assets SET agent_personal_accounts = $2::jsonb
		 WHERE id = (`+heartbeatAssetSQL+`)`, hostname, string(b))
	return err
}

// personalPrincipalsSQL is every (asset, account name) a person holds as
// their own personal account. $1 is their email.
const personalPrincipalsSQL = `
	SELECT ta.asset_id, u.login_name
	  FROM asset_team_assignments ta
	  JOIN team_members m ON m.team_id = ta.team_id
	  JOIN users u ON lower(u.email) = m.user_email
	 WHERE m.user_email = lower($1)
	   AND ta.personal_accounts
	   AND u.login_name IS NOT NULL
	   AND u.disabled_at IS NULL`

func (s *Store) personalPrincipals(ctx context.Context, email string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT asset_id::text, login_name FROM (`+personalPrincipalsSQL+`) p`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// maxReportedStates bounds what one heartbeat may store about its accounts.
const maxReportedStates = 500

// capStates keeps an agent's account report to a size and shape the console
// can show: a bounded list of short fields.
func capStates(in []PersonalAccountState) []PersonalAccountState {
	if len(in) > maxReportedStates {
		in = in[:maxReportedStates]
	}
	clip := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	out := make([]PersonalAccountState, 0, len(in))
	for _, s := range in {
		switch s.State {
		case "ready", "locked", "taken", "failed":
		default:
			continue
		}
		out = append(out, PersonalAccountState{
			Name: clip(s.Name, 32), Owner: clip(s.Owner, 254),
			State: s.State, Detail: clip(s.Detail, 300),
		})
	}
	return out
}

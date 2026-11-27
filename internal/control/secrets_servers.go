package control

/*
argus-secret servers: where an organization's secret values live.

argus-control never holds a value or a key that opens one. It keeps, per
organization, the servers (an address, a TLS pin, the top-level folders each
keeps) and the rules saying who may do what. It pushes those rules to every
server, and collects each server's hash-chained read log into this
organization's audit log. Agents and app tokens read from argus-secret
directly; a person in the console reads through the routes in
secrets_routes.go, which relay a value sealed to the browser's own key.

Every management request is signed with the organization's management key,
derived from the tunnel grant key, so it needs no storage of its own and is
the same on every replica. An organization's servers are configured with its
public half (Settings › Secrets servers).

See .serena/memories/argus-secret.md.
*/

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/hashchain"
	"github.com/gsoultan/argus/internal/tlsconfig"
	"github.com/gsoultan/argus/internal/vault"
)

// SecretsSyncInterval is how often each server is contacted: well inside
// argus-secret's default rules_max_age of 15 minutes, so a server that can
// reach argus-control never refuses reads as stale.
const SecretsSyncInterval = time.Minute

const (
	maxSecretsServers = 20
	maxSecretRules    = 2000
	secretsTimeout    = 15 * time.Second
	// maxSecretsResponse bounds what argus-control reads from a server.
	maxSecretsResponse = 8 << 20
)

// SecretsServer is one argus-secret server, as the console shows it.
type SecretsServer struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	URL           string     `json:"url"`
	TLSPin        string     `json:"tlsPin"`
	Folders       []string   `json:"folders"`
	SealKey       string     `json:"sealKey"`
	Version       string     `json:"version"`
	SecretCount   int        `json:"secretCount"`
	RulesSerial   int64      `json:"rulesSerial"`
	RulesPushedAt *time.Time `json:"rulesPushedAt,omitempty"`
	LogSeq        int64      `json:"logSeq"`
	LogGap        string     `json:"logGap,omitempty"`
	LogGapAt      *time.Time `json:"logGapAt,omitempty"`
	LastContactAt *time.Time `json:"lastContactAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	LastErrorAt   *time.Time `json:"lastErrorAt,omitempty"`
	CreatedBy     string     `json:"createdBy"`
	CreatedAt     time.Time  `json:"createdAt"`

	rulesHash string
	logHead   string
}

// SecretsServerInput is a server as the console sends it.
type SecretsServerInput struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	TLSPin  string   `json:"tlsPin"`
	Folders []string `json:"folders"`
}

// SecretRule is who may do what with which secrets.
type SecretRule struct {
	ID            string          `json:"id"`
	Path          string          `json:"path"`
	Subjects      []vault.Subject `json:"subjects"`
	Can           []string        `json:"can"`
	NeedsApproval bool            `json:"needsApproval"`
	Note          string          `json:"note"`
	CreatedBy     string          `json:"createdBy"`
	CreatedAt     time.Time       `json:"createdAt"`
}

// ErrInvalidSecrets is a server, a rule or a request the console should fix.
type ErrInvalidSecrets struct{ msg string }

func (e ErrInvalidSecrets) Error() string { return e.msg }

// Errors the routes turn into 404s.
var (
	ErrNoSecretsServer = errors.New("no such secrets server")
	ErrNoSecretRule    = errors.New("no such secret rule")
	// ErrNoServerForSecret is a name whose top-level folder no server keeps.
	ErrNoServerForSecret = errors.New("no secrets server keeps this folder")
)

/* ── The management key ─────────────────────────────────────────────────── */

// secretsManageKey is the organization's management key. Derived, not stored: the
// grant key is already per organization and the same on every replica.
func (a *API) secretsManageKey(ctx context.Context) (ed25519.PrivateKey, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, ErrInvalidSecrets{"this control plane cannot sign for a secrets server: set signing_secret in its configuration"}
	}
	sum := sha256.Sum256(append([]byte("argus-secret manage key v1\x00"), a.Tunnels.grantKeyFor(ctx).Seed()...))
	return ed25519.NewKeyFromSeed(sum[:]), nil
}

// SecretsPublicKey is what a server's control_public_key must be set to.
func (a *API) SecretsPublicKey(ctx context.Context) (string, error) {
	k, err := a.secretsManageKey(ctx)
	if err != nil {
		return "", err
	}
	return "ed25519:" + base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey)), nil
}

/* ── Validation ─────────────────────────────────────────────────────────── */

func (in *SecretsServerInput) normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimRight(strings.TrimSpace(in.URL), "/")
	in.TLSPin = strings.TrimSpace(in.TLSPin)
	if l := len([]rune(in.Name)); l == 0 || l > 80 {
		return ErrInvalidSecrets{"give the server a name of up to 80 characters"}
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || len(in.URL) > 300 {
		return ErrInvalidSecrets{"the address is https://host or https://host:port, as argus-control reaches it"}
	}
	if in.TLSPin != "" {
		if _, err := tlsconfig.Pinned(in.TLSPin); err != nil {
			return ErrInvalidSecrets{"the pin is the sha256:… argus-secret printed when it started, or empty for a certificate the system trusts"}
		}
	}
	seen := map[string]bool{}
	folders := make([]string, 0, len(in.Folders))
	for _, f := range in.Folders {
		f = strings.TrimSuffix(strings.TrimSpace(f), "/")
		if f == "" || seen[f] {
			continue
		}
		if strings.Contains(f, "/") || vault.ValidName(f) != nil {
			return ErrInvalidSecrets{fmt.Sprintf("folder %q: a top-level folder is lowercase letters, digits, and . _ - after the first character", f)}
		}
		seen[f] = true
		folders = append(folders, f)
	}
	if len(folders) == 0 {
		return ErrInvalidSecrets{"name at least one top-level folder this server keeps, such as prod"}
	}
	if len(folders) > 100 {
		return ErrInvalidSecrets{"a server keeps at most 100 top-level folders"}
	}
	sort.Strings(folders)
	in.Folders = folders
	return nil
}

var secretPermissions = []string{vault.CanList, vault.CanRead, vault.CanWrite, vault.CanRotate, vault.CanManage}

var secretSubjectKinds = []string{vault.SubjectServer, vault.SubjectGroup, vault.SubjectApp, vault.SubjectPerson, vault.SubjectTeam, vault.SubjectRole}

func (r *SecretRule) normalize() error {
	r.Path = strings.TrimSpace(r.Path)
	r.Note = strings.TrimSpace(r.Note)
	if err := vault.ValidPrefix(r.Path); err != nil {
		return ErrInvalidSecrets{"path: " + err.Error()}
	}
	if len([]rune(r.Note)) > 500 {
		return ErrInvalidSecrets{"a note is at most 500 characters"}
	}
	if len(r.Subjects) == 0 || len(r.Subjects) > 100 {
		return ErrInvalidSecrets{"a rule is for 1 to 100 subjects"}
	}
	for i, s := range r.Subjects {
		s.ID = strings.TrimSpace(s.ID)
		if !slices.Contains(secretSubjectKinds, s.Kind) || s.ID == "" || len(s.ID) > 200 {
			return ErrInvalidSecrets{"each subject is a server, group, app, person, team or role, with its id"}
		}
		if s.Kind == vault.SubjectPerson {
			s.ID = strings.ToLower(s.ID)
		}
		r.Subjects[i] = s
	}
	can := []string{}
	for _, c := range r.Can {
		if !slices.Contains(secretPermissions, c) {
			return ErrInvalidSecrets{fmt.Sprintf("a rule grants list, read, write, rotate or manage, not %q", c)}
		}
		if !slices.Contains(can, c) {
			can = append(can, c)
		}
	}
	if len(can) == 0 {
		return ErrInvalidSecrets{"a rule grants at least one of list, read, write, rotate, manage"}
	}
	r.Can = can
	return nil
}

/* ── Store: servers ─────────────────────────────────────────────────────── */

const secretsServerColumns = `id::text, name, url, tls_pin, folders, seal_key, version, secret_count, rules_serial,
	rules_hash, rules_pushed_at, log_seq, log_head, log_gap, log_gap_at, last_contact_at, last_error, last_error_at,
	created_by, created_at`

func scanSecretsServer(row pgx.Row) (SecretsServer, error) {
	var s SecretsServer
	err := row.Scan(&s.ID, &s.Name, &s.URL, &s.TLSPin, &s.Folders, &s.SealKey, &s.Version, &s.SecretCount,
		&s.RulesSerial, &s.rulesHash, &s.RulesPushedAt, &s.LogSeq, &s.logHead, &s.LogGap, &s.LogGapAt,
		&s.LastContactAt, &s.LastError, &s.LastErrorAt, &s.CreatedBy, &s.CreatedAt)
	if s.Folders == nil {
		s.Folders = []string{}
	}
	return s, err
}

// SecretsServers are the organization's servers, by name.
func (s *Store) SecretsServers(ctx context.Context) ([]SecretsServer, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+secretsServerColumns+` FROM secrets_servers ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretsServer{}
	for rows.Next() {
		srv, err := scanSecretsServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// SecretsServer is one server.
func (s *Store) SecretsServer(ctx context.Context, id string) (SecretsServer, error) {
	if !validUUID(id) {
		return SecretsServer{}, ErrNoSecretsServer
	}
	srv, err := scanSecretsServer(s.db(ctx).QueryRow(ctx, `SELECT `+secretsServerColumns+` FROM secrets_servers WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return srv, ErrNoSecretsServer
	}
	return srv, err
}

// SecretsServerFor is the server that keeps name's top-level folder.
func (s *Store) SecretsServerFor(ctx context.Context, name string) (SecretsServer, error) {
	srv, err := scanSecretsServer(s.db(ctx).QueryRow(ctx, `SELECT `+secretsServerColumns+` FROM secrets_servers
		WHERE $1 = ANY(folders)`, vault.Folder(name)))
	if errors.Is(err, pgx.ErrNoRows) {
		return srv, ErrNoServerForSecret
	}
	return srv, err
}

// SaveSecretsServer adds a server (id empty) or changes one. A folder kept by
// another server is refused: each name has exactly one home.
func (s *Store) SaveSecretsServer(ctx context.Context, id string, in SecretsServerInput, sealKey, version, by string) (SecretsServer, error) {
	var out SecretsServer
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		// Two admins adding servers for the same folder at once would both
		// pass the check below without this.
		if _, err := tx.Exec(ctx, `LOCK TABLE secrets_servers IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var other, folder string
		err := tx.QueryRow(ctx, `SELECT name, f FROM secrets_servers, unnest(folders) f
			WHERE id::text <> $1 AND f = ANY($2::text[]) LIMIT 1`, id, in.Folders).Scan(&other, &folder)
		if err == nil {
			return ErrInvalidSecrets{fmt.Sprintf("%s already keeps the folder %s; a folder lives on one server", other, folder)}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if id == "" {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM secrets_servers`).Scan(&n); err != nil {
				return err
			}
			if n >= maxSecretsServers {
				return ErrInvalidSecrets{fmt.Sprintf("an organization can have at most %d secrets servers", maxSecretsServers)}
			}
			out, err = scanSecretsServer(tx.QueryRow(ctx, `INSERT INTO secrets_servers
				(name, url, tls_pin, folders, seal_key, version, last_contact_at, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, now(), $7) RETURNING `+secretsServerColumns,
				in.Name, in.URL, in.TLSPin, in.Folders, sealKey, version, strings.ToLower(by)))
		} else {
			// A changed address or pin is a different server as far as the
			// log is concerned only if it says so: the chain is checked on the
			// next collection either way.
			out, err = scanSecretsServer(tx.QueryRow(ctx, `UPDATE secrets_servers SET name = $2, url = $3, tls_pin = $4,
				folders = $5, seal_key = CASE WHEN $6 = '' THEN seal_key ELSE $6 END,
				version = CASE WHEN $7 = '' THEN version ELSE $7 END, rules_hash = '', updated_at = now()
				WHERE id = $1 RETURNING `+secretsServerColumns,
				id, in.Name, in.URL, in.TLSPin, in.Folders, sealKey, version))
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNoSecretsServer
			}
		}
		if err != nil && isUniqueViolation(err) {
			return ErrInvalidSecrets{"another secrets server already has that name"}
		}
		return err
	})
	return out, err
}

// RemoveSecretsServer forgets a server. Its secrets stay on it; argus-control
// just stops pushing rules to it and collecting its log.
func (s *Store) RemoveSecretsServer(ctx context.Context, id string) (SecretsServer, error) {
	if !validUUID(id) {
		return SecretsServer{}, ErrNoSecretsServer
	}
	srv, err := scanSecretsServer(s.db(ctx).QueryRow(ctx, `DELETE FROM secrets_servers WHERE id = $1 RETURNING `+secretsServerColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return srv, ErrNoSecretsServer
	}
	return srv, err
}

func (s *Store) secretsContact(ctx context.Context, id string, st vault.Status) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE secrets_servers SET last_contact_at = now(), last_error = '',
		seal_key = $2, version = $3, secret_count = $4 WHERE id = $1`, id, st.SealKey, st.Version, st.Secrets)
	return err
}

func (s *Store) secretsFailed(ctx context.Context, id, msg string) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.db(ctx).Exec(ctx, `UPDATE secrets_servers SET last_error = $2, last_error_at = now() WHERE id = $1`, id, msg)
	return err
}

func (s *Store) secretsRulesPushed(ctx context.Context, id string, serial int64, hash string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE secrets_servers SET rules_serial = $2, rules_hash = $3, rules_pushed_at = now()
		WHERE id = $1`, id, serial, hash)
	return err
}

// secretsLogAt records how far a server's log is in audit_events. Only moves
// forward from where it was, so two replicas collecting at once cannot move
// it back.
func (s *Store) secretsLogAt(ctx context.Context, id string, from, seq int64, head string) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE secrets_servers SET log_seq = $3, log_head = $4
		WHERE id = $1 AND log_seq = $2`, id, from, seq, head)
	return tag.RowsAffected() == 1, err
}

func (s *Store) secretsLogGap(ctx context.Context, id, gap string) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE secrets_servers SET log_gap = $2, log_gap_at = now()
		WHERE id = $1 AND log_gap = ''`, id, gap)
	return tag.RowsAffected() == 1, err
}

// resumeSecretsLog clears a gap and takes the server's log from seq on, with
// head as the link before it.
func (s *Store) resumeSecretsLog(ctx context.Context, id string, seq int64, head string) error {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE secrets_servers SET log_gap = '', log_gap_at = NULL, log_seq = $2, log_head = $3
		WHERE id = $1`, id, seq, head)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNoSecretsServer
	}
	return err
}

/* ── Store: rules ───────────────────────────────────────────────────────── */

const secretRuleColumns = `id::text, path, subjects, can, needs_approval, note, created_by, created_at`

func scanSecretRule(row pgx.Row) (SecretRule, error) {
	var r SecretRule
	var subjects []byte
	err := row.Scan(&r.ID, &r.Path, &subjects, &r.Can, &r.NeedsApproval, &r.Note, &r.CreatedBy, &r.CreatedAt)
	if err != nil {
		return r, err
	}
	_ = json.Unmarshal(subjects, &r.Subjects)
	if r.Subjects == nil {
		r.Subjects = []vault.Subject{}
	}
	return r, nil
}

// SecretRules are the organization's rules, by path.
func (s *Store) SecretRules(ctx context.Context) ([]SecretRule, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+secretRuleColumns+` FROM secret_rules ORDER BY path, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretRule{}
	for rows.Next() {
		r, err := scanSecretRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveSecretRule adds a rule (id empty) or changes one.
func (s *Store) SaveSecretRule(ctx context.Context, id string, r SecretRule, by string) (SecretRule, error) {
	subjects, _ := json.Marshal(r.Subjects)
	var out SecretRule
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		var err error
		if id == "" {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM secret_rules`).Scan(&n); err != nil {
				return err
			}
			if n >= maxSecretRules {
				return ErrInvalidSecrets{fmt.Sprintf("an organization can have at most %d secret rules", maxSecretRules)}
			}
			out, err = scanSecretRule(tx.QueryRow(ctx, `INSERT INTO secret_rules (path, subjects, can, needs_approval, note, created_by)
				VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+secretRuleColumns,
				r.Path, subjects, r.Can, r.NeedsApproval, r.Note, strings.ToLower(by)))
			return err
		}
		if !validUUID(id) {
			return ErrNoSecretRule
		}
		out, err = scanSecretRule(tx.QueryRow(ctx, `UPDATE secret_rules SET path = $2, subjects = $3, can = $4,
			needs_approval = $5, note = $6, updated_at = now() WHERE id = $1 RETURNING `+secretRuleColumns,
			id, r.Path, subjects, r.Can, r.NeedsApproval, r.Note))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSecretRule
		}
		return err
	})
	return out, err
}

// RemoveSecretRule deletes a rule.
func (s *Store) RemoveSecretRule(ctx context.Context, id string) (SecretRule, error) {
	if !validUUID(id) {
		return SecretRule{}, ErrNoSecretRule
	}
	r, err := scanSecretRule(s.db(ctx).QueryRow(ctx, `DELETE FROM secret_rules WHERE id = $1 RETURNING `+secretRuleColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNoSecretRule
	}
	return r, err
}

// secretsIdentities are the enrolled agents a bundle names: each live
// credential's hash, as enrollment.go stores it, and the groups of the asset
// with its hostname (its group and its tags). A server rule names the asset's
// id, or the credential's when there is no asset.
func (s *Store) secretsIdentities(ctx context.Context) ([]vault.ServerIdentity, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT COALESCE(a.id::text, c.id::text), c.hostname, c.secret_hash,
			COALESCE(a.group_name, ''), COALESCE(a.tags, '{}')
		FROM agent_credentials c LEFT JOIN assets a ON lower(a.hostname) = lower(c.hostname)
		WHERE c.revoked_at IS NULL ORDER BY c.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []vault.ServerIdentity{}
	for rows.Next() {
		var si vault.ServerIdentity
		var group string
		var tags []string
		if err := rows.Scan(&si.ID, &si.Hostname, &si.CredentialHash, &group, &tags); err != nil {
			return nil, err
		}
		if group != "" && group != "ungrouped" {
			si.Groups = append(si.Groups, group)
		}
		for _, t := range tags {
			if t != "" && !slices.Contains(si.Groups, t) {
				si.Groups = append(si.Groups, t)
			}
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// secretsTeamsOf are the ids of the teams email is in.
func (s *Store) secretsTeamsOf(ctx context.Context, email string) ([]string, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT team_id::text FROM team_members WHERE user_email = lower($1) ORDER BY 1`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

/* ── Talking to a server ────────────────────────────────────────────────── */

// SecretsError is a server's answer other than success: its status and its
// own words, which the console can show as they are.
type SecretsError struct {
	Status int
	Msg    string
}

func (e SecretsError) Error() string { return e.Msg }

func secretsHTTPClient(pin string) (*http.Client, error) {
	tlsCfg, err := tlsconfig.PinnedOrPublic(pin)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: secretsTimeout,
		// A server's redirect would take a signed request somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment,
			DisableKeepAlives: true, ResponseHeaderTimeout: secretsTimeout},
	}, nil
}

// controlActor is argus-control acting for itself.
var controlActor = vault.ManageActor{Kind: vault.ActorControl, ID: "argus-control"}

// secretsCall sends one signed management request and returns the raw answer
// on success. uri is the path and query, such as /manage/v1/log?after=3.
func (a *API) secretsCall(ctx context.Context, srv SecretsServer, method, uri string, actor vault.ManageActor, body []byte) ([]byte, error) {
	key, err := a.secretsManageKey(ctx)
	if err != nil {
		return nil, err
	}
	hdr, err := vault.SignManage(key, method, uri, actor, body, time.Now())
	if err != nil {
		return nil, err
	}
	cl, err := secretsHTTPClient(srv.TLSPin)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, srv.URL+uri, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s did not answer: %w", srv.Name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSecretsResponse+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", srv.Name, err)
	}
	if len(raw) > maxSecretsResponse {
		return nil, fmt.Errorf("%s sent more than argus-control reads", srv.Name)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("%s answered %s", srv.Name, resp.Status)
		}
		return nil, SecretsError{Status: resp.StatusCode, Msg: e.Error}
	}
	return raw, nil
}

func (a *API) secretsJSON(ctx context.Context, srv SecretsServer, method, uri string, actor vault.ManageActor, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	raw, err := a.secretsCall(ctx, srv, method, uri, actor, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s answered something argus-control cannot read: %w", srv.Name, err)
	}
	return nil
}

// secretsStatus asks a server about itself and checks it keeps this
// organization's secrets.
func (a *API) secretsStatus(ctx context.Context, srv SecretsServer) (vault.Status, error) {
	var st vault.Status
	if err := a.secretsJSON(ctx, srv, http.MethodGet, "/manage/v1/status", controlActor, nil, &st); err != nil {
		var se SecretsError
		if errors.As(err, &se) && se.Status == http.StatusUnauthorized {
			return st, ErrInvalidSecrets{srv.Name + " does not accept this organization's key: set its control_public_key to the key shown here, then restart it"}
		}
		return st, err
	}
	if org := tenantIDOf(ctx); st.Org != org {
		return st, ErrInvalidSecrets{fmt.Sprintf("%s keeps organization %q; set its organization to %q", srv.Name, st.Org, org)}
	}
	return st, nil
}

/* ── Rules ──────────────────────────────────────────────────────────────── */

// secretsBundle is what srv should hold, and a digest of it that leaves out
// the serial and time.
func (a *API) secretsBundle(ctx context.Context, srv SecretsServer) (vault.Bundle, string, error) {
	servers, err := a.store.secretsIdentities(ctx)
	if err != nil {
		return vault.Bundle{}, "", err
	}
	all, err := a.store.SecretRules(ctx)
	if err != nil {
		return vault.Bundle{}, "", err
	}
	rules := []vault.Rule{}
	for _, r := range all {
		if r.Path != "" && !slices.Contains(srv.Folders, vault.Folder(r.Path)) {
			continue
		}
		rules = append(rules, vault.Rule{Path: r.Path, Subjects: r.Subjects, Can: r.Can, NeedsApproval: r.NeedsApproval})
	}
	b := vault.Bundle{Org: tenantIDOf(ctx), Folders: srv.Folders, Servers: servers, Apps: []vault.AppIdentity{}, Rules: rules}
	raw, err := json.Marshal(b)
	if err != nil {
		return b, "", err
	}
	sum := sha256.Sum256(raw)
	return b, hex.EncodeToString(sum[:]), nil
}

/* ── Sync: status, rules, log ───────────────────────────────────────────── */

// RunSecretsSync keeps every organization's servers in step until ctx ends.
func (a *API) RunSecretsSync(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			srvs, err := a.store.SecretsServers(tctx)
			if err != nil {
				a.log.Error("secrets sync: cannot list servers", "error", err)
				return
			}
			for _, srv := range srvs {
				_ = a.SyncSecretsServer(tctx, srv.ID)
			}
		})
		if err != nil {
			a.log.Error("secrets sync", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SyncSecretsServer contacts one server: it checks the server, pushes newer
// rules, and collects its log. Any signed call is contact, so a server that
// is synced never treats its rules as stale. The error is also kept on the
// server's row for the console.
func (a *API) SyncSecretsServer(ctx context.Context, id string) error {
	mu, _ := a.secretsSyncs.LoadOrStore(tenantIDOf(ctx)+"/"+id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	srv, err := a.store.SecretsServer(ctx, id)
	if err != nil {
		return err
	}
	err = a.syncSecretsServer(ctx, srv)
	if err != nil {
		if ferr := a.store.secretsFailed(ctx, srv.ID, err.Error()); ferr != nil {
			a.log.Error("secrets sync: cannot record failure", "error", ferr)
		}
		a.log.Warn("secrets sync failed", "server", srv.Name, "error", err)
	}
	return err
}

func (a *API) syncSecretsServer(ctx context.Context, srv SecretsServer) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	st, err := a.secretsStatus(ctx, srv)
	if err != nil {
		return err
	}
	if err := a.store.secretsContact(ctx, srv.ID, st); err != nil {
		return err
	}
	if err := a.pushSecretsRules(ctx, srv, st); err != nil {
		return err
	}
	return a.collectSecretsLog(ctx, srv)
}

// pushSecretsRules sends the bundle when it changed, or when the server holds
// a serial other than the one last pushed (it was restored, or another
// control plane pushed to it).
func (a *API) pushSecretsRules(ctx context.Context, srv SecretsServer, st vault.Status) error {
	b, hash, err := a.secretsBundle(ctx, srv)
	if err != nil {
		return err
	}
	if hash == srv.rulesHash && st.Rules.Serial == srv.RulesSerial && st.Rules.Serial > 0 {
		return nil
	}
	b.Serial = max(srv.RulesSerial, st.Rules.Serial) + 1
	b.IssuedAt = time.Now().UTC()
	if err := a.secretsJSON(ctx, srv, http.MethodPut, "/manage/v1/rules", controlActor, b, nil); err != nil {
		return fmt.Errorf("push rules to %s: %w", srv.Name, err)
	}
	return a.store.secretsRulesPushed(ctx, srv.ID, b.Serial, hash)
}

// collectSecretsLog copies a server's new log entries into audit_events, then
// tells the server they are kept so it may prune them. Each entry must follow
// on from the last one collected; one that does not stops collection and
// raises an alert, because a gap or a changed entry is what tampering with a
// server's log looks like.
func (a *API) collectSecretsLog(ctx context.Context, srv SecretsServer) error {
	if srv.LogGap != "" {
		return nil
	}
	seq, head := srv.LogSeq, srv.logHead
	for range 50 {
		var batch vault.LogBatch
		uri := "/manage/v1/log?after=" + strconv.FormatInt(seq, 10) + "&limit=1000"
		if err := a.secretsJSON(ctx, srv, http.MethodGet, uri, controlActor, nil, &batch); err != nil {
			return fmt.Errorf("collect the read log of %s: %w", srv.Name, err)
		}
		if seq == 0 && head == "" {
			// The first collection starts from what the server says was
			// before its oldest entry: the chain's start, unless it was
			// pruned under another control plane.
			head = batch.State.Base
			if head == "" {
				head = hashchain.Genesis
			}
		}
		if len(batch.Entries) == 0 {
			if uint64(seq) > batch.State.Seq {
				return a.secretsGap(ctx, srv, fmt.Sprintf("its log ends at entry %d, but argus-control already holds entries up to %d: the server's data was replaced or rolled back", batch.State.Seq, seq))
			}
			return nil
		}
		from := seq
		for _, item := range batch.Entries {
			var e vault.Entry
			switch {
			case item.Seq != uint64(seq)+1:
				return a.secretsGap(ctx, srv, fmt.Sprintf("entry %d came after %d: entries are missing", item.Seq, seq))
			case hashchain.Next(head, item.Line) != item.Hash:
				return a.secretsGap(ctx, srv, fmt.Sprintf("entry %d does not hash to the link it claims", item.Seq))
			case json.Unmarshal(item.Line, &e) != nil:
				return a.secretsGap(ctx, srv, fmt.Sprintf("entry %d cannot be read", item.Seq))
			case e.Prev != head || e.Seq != item.Seq:
				return a.secretsGap(ctx, srv, fmt.Sprintf("entry %d does not follow on from entry %d", item.Seq, seq))
			}
			if _, err := a.store.AppendAudit(ctx, secretsAuditEvent(srv, e, item.Hash)); err != nil {
				return fmt.Errorf("record the read log of %s: %w", srv.Name, err)
			}
			seq, head = int64(item.Seq), item.Hash
		}
		moved, err := a.store.secretsLogAt(ctx, srv.ID, from, seq, head)
		if err != nil {
			return err
		}
		if !moved {
			// Another replica collected the same entries first; it acks.
			return nil
		}
		if err := a.secretsJSON(ctx, srv, http.MethodPost, "/manage/v1/log/ack", controlActor, map[string]int64{"seq": seq}, nil); err != nil {
			return fmt.Errorf("acknowledge the read log of %s: %w", srv.Name, err)
		}
		if uint64(seq) >= batch.State.Seq {
			return nil
		}
	}
	return nil
}

func (a *API) secretsGap(ctx context.Context, srv SecretsServer, why string) error {
	first, err := a.store.secretsLogGap(ctx, srv.ID, why)
	if err != nil {
		return err
	}
	if first {
		detail := "The read log of " + srv.Name + " no longer follows on from what argus-control collected: " + why +
			". Collection has stopped. Check the server, then resume it in Settings › Secrets servers."
		if _, err := a.store.AppendAudit(ctx, AuditEvent{Action: "secrets.log_gap", Severity: "critical",
			ActorEmail: "argus-control", Target: "secrets server " + srv.Name, Detail: detail}); err != nil {
			a.log.Error("audit append failed", "error", err, "action", "secrets.log_gap")
		}
		a.alertSecurity(ctx, "argus-control", "A secrets server's read log has a gap", detail)
	}
	return errors.New("the read log of " + srv.Name + " has a gap: " + why)
}

// secretsAuditEvent turns one of a server's log entries into an audit event.
// The entry's own seq and link are in the detail, so the audit log can be
// matched against the server's.
func secretsAuditEvent(srv SecretsServer, e vault.Entry, hash string) AuditEvent {
	actor := e.Actor.ID
	switch e.Actor.Kind {
	case vault.ActorServer:
		actor = "agent:" + e.Actor.Name
	case vault.ActorApp:
		actor = "app:" + e.Actor.Name
	case vault.ActorControl, vault.ActorSystem:
		actor = e.Actor.ID
	}
	severity := "notice"
	if e.Outcome == vault.OutcomeRefused {
		severity = "warning"
	}
	var d strings.Builder
	fmt.Fprintf(&d, "%s %s", secretsActionWords(e.Action), e.Outcome)
	if e.Secret != "" {
		fmt.Fprintf(&d, " for %s", e.Secret)
		if e.Version > 0 {
			fmt.Fprintf(&d, " version %d", e.Version)
		}
		if e.Field != "" {
			fmt.Fprintf(&d, " field %s", e.Field)
		}
	}
	if e.Route != "" {
		fmt.Fprintf(&d, ", %s", e.Route)
	}
	if e.Remote != "" {
		fmt.Fprintf(&d, " from %s", e.Remote)
	}
	if e.Reason != "" {
		fmt.Fprintf(&d, ". Reason: %s", e.Reason)
	}
	if e.Detail != "" {
		fmt.Fprintf(&d, ". %s", e.Detail)
	}
	fmt.Fprintf(&d, ". On %s at %s, log entry %d, link %s.", srv.Name, e.At.UTC().Format(time.RFC3339), e.Seq, hash)
	target := "secrets server " + srv.Name
	if e.Secret != "" {
		target = "secret " + e.Secret
	}
	return AuditEvent{Action: "secret." + e.Action, Severity: severity, ActorEmail: actor, Target: target, Detail: d.String()}
}

func secretsActionWords(action string) string {
	switch action {
	case "read":
		return "Read"
	case "write", "create", "write.generated", "create.generated":
		return "Wrote"
	case "rules":
		return "Rules applied"
	case "delete":
		return "Deleted"
	case "restore":
		return "Restored"
	case "purge":
		return "Purged"
	}
	if strings.HasPrefix(action, "label.") {
		return "Labelled " + strings.TrimPrefix(action, "label.")
	}
	return strings.ToUpper(action[:min(1, len(action))]) + action[min(1, len(action)):]
}

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/vault"
)

/*
App tokens: callers outside Argus that read secrets straight from argus-secret
(a CI job, a service on a host with no agent). A token is shown once, when it
is made; argus-control keeps its SHA-256 and pushes that to every server in
the rules bundle, so the token itself never travels again. A rule names an
app by its id. Revoking one, or its expiry, takes it out of the next bundle;
argus-secret also refuses an expired token on its own.

	GET    /api/v1/secrets/apps        tokens, without their secrets
	POST   /api/v1/secrets/apps        make one {name, days}; the token is in the answer, once
	DELETE /api/v1/secrets/apps/{id}   revoke one
*/

const (
	maxSecretApps     = 500
	defaultAppDays    = 90
	maxAppDays        = 365
	secretAppTailSize = 4
)

// SecretApp is an app token, without the token.
type SecretApp struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	TokenTail string     `json:"tokenTail"`
	ExpiresAt time.Time  `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	RevokedBy string     `json:"revokedBy,omitempty"`
	CreatedBy string     `json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
}

// ErrNoSecretApp is an app token that is not there.
var ErrNoSecretApp = errors.New("no such app token")

const secretAppColumns = `id::text, name, token_tail, expires_at, revoked_at, revoked_by, created_by, created_at`

func scanSecretApp(row pgx.Row) (SecretApp, error) {
	var a SecretApp
	err := row.Scan(&a.ID, &a.Name, &a.TokenTail, &a.ExpiresAt, &a.RevokedAt, &a.RevokedBy, &a.CreatedBy, &a.CreatedAt)
	return a, err
}

// SecretApps are the organization's app tokens, newest first.
func (s *Store) SecretApps(ctx context.Context) ([]SecretApp, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+secretAppColumns+` FROM secret_apps ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretApp{}
	for rows.Next() {
		a, err := scanSecretApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateSecretApp makes a token and returns it with the app. The token is not
// kept anywhere.
func (s *Store) CreateSecretApp(ctx context.Context, name string, ttl time.Duration, by string) (SecretApp, string, error) {
	token, err := newSecret(vault.AppTokenPrefix)
	if err != nil {
		return SecretApp{}, "", err
	}
	var out SecretApp
	err = pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM secret_apps WHERE revoked_at IS NULL AND expires_at > now()`).Scan(&n); err != nil {
			return err
		}
		if n >= maxSecretApps {
			return ErrInvalidSecrets{fmt.Sprintf("an organization can have at most %d live app tokens", maxSecretApps)}
		}
		var err error
		out, err = scanSecretApp(tx.QueryRow(ctx, `INSERT INTO secret_apps (name, token_hash, token_tail, expires_at, created_by)
			VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5) RETURNING `+secretAppColumns,
			name, hashSecret(token), token[len(token)-secretAppTailSize:], ttl.Seconds(), strings.ToLower(by)))
		return err
	})
	if err != nil {
		return SecretApp{}, "", err
	}
	return out, token, nil
}

// RevokeSecretApp revokes a live token.
func (s *Store) RevokeSecretApp(ctx context.Context, id, by string) (SecretApp, error) {
	if !validUUID(id) {
		return SecretApp{}, ErrNoSecretApp
	}
	a, err := scanSecretApp(s.db(ctx).QueryRow(ctx, `UPDATE secret_apps SET revoked_at = now(), revoked_by = $2, updated_at = now()
		WHERE id = $1 AND revoked_at IS NULL RETURNING `+secretAppColumns, id, strings.ToLower(by)))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNoSecretApp
	}
	return a, err
}

// secretsAppIdentities are the live tokens a bundle names.
func (s *Store) secretsAppIdentities(ctx context.Context) ([]vault.AppIdentity, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id::text, name, token_hash, expires_at FROM secret_apps
		WHERE revoked_at IS NULL AND expires_at > now() ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []vault.AppIdentity{}
	for rows.Next() {
		var a vault.AppIdentity
		if err := rows.Scan(&a.ID, &a.Name, &a.TokenHash, &a.ExpiresAt); err != nil {
			return nil, err
		}
		a.ExpiresAt = a.ExpiresAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

func (a *API) getSecretApps(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.secretsAdmin(w, r); !ok {
		return
	}
	apps, err := a.store.SecretApps(r.Context())
	if err != nil {
		a.secretsErr(w, "list app tokens", err)
		return
	}
	writeJSON(w, http.StatusOK, apps)
}

func (a *API) postSecretApp(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
		Days int    `json:"days"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if l := len([]rune(in.Name)); l == 0 || l > 80 {
		writeErr(w, http.StatusBadRequest, "give the app a name of up to 80 characters")
		return
	}
	if in.Days == 0 {
		in.Days = defaultAppDays
	}
	if in.Days < 1 || in.Days > maxAppDays {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a token lasts 1 to %d days", maxAppDays))
		return
	}
	app, token, err := a.store.CreateSecretApp(r.Context(), in.Name, time.Duration(in.Days)*24*time.Hour, actor)
	if err != nil {
		a.secretsErr(w, "make app token", err)
		return
	}
	a.audit(r, "secrets.app.add", actor, "app token "+app.Name,
		fmt.Sprintf("Made app token %s (ending %s), valid until %s.", app.Name, app.TokenTail, app.ExpiresAt.UTC().Format(time.RFC3339)))
	a.syncAllSoon(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, struct {
		SecretApp
		// Token is shown this once.
		Token string `json:"token"`
	}{app, token})
}

func (a *API) deleteSecretApp(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	app, err := a.store.RevokeSecretApp(r.Context(), r.PathValue("id"), actor)
	if err != nil {
		if errors.Is(err, ErrNoSecretApp) {
			writeErr(w, http.StatusNotFound, "no such live app token")
			return
		}
		a.secretsErr(w, "revoke app token", err)
		return
	}
	a.audit(r, "secrets.app.revoke", actor, "app token "+app.Name, fmt.Sprintf("Revoked app token %s (ending %s).", app.Name, app.TokenTail))
	a.syncAllSoon(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// agentSecretsServer is a server as an agent needs it: where it is, how to
// trust it, which folders it keeps. Nothing else of the server's row.
type agentSecretsServer struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	TLSPin  string   `json:"tlsPin,omitempty"`
	Folders []string `json:"folders"`
}

// getAgentSecretsServers tells an enrolled agent where its organization's
// secrets live, so it can read from them directly. Agent credentials only:
// the shared reporter token is not a server.
func (a *API) getAgentSecretsServers(w http.ResponseWriter, r *http.Request) {
	if agentHost(r) == "" {
		writeErr(w, http.StatusForbidden, "only an enrolled agent asks where secrets live")
		return
	}
	srvs, err := a.store.SecretsServers(r.Context())
	if err != nil {
		a.secretsErr(w, "agent secrets servers", err)
		return
	}
	out := make([]agentSecretsServer, 0, len(srvs))
	for _, s := range srvs {
		out = append(out, agentSecretsServer{Name: s.Name, URL: s.URL, TLSPin: s.TLSPin, Folders: s.Folders})
	}
	writeJSON(w, http.StatusOK, out)
}

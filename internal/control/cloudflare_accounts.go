package control

/*
Cloudflare accounts read for Networks: every Cloudflare Tunnel and WARP
Connector in the account with Cloudflare's own view of its connectors, the
private network routes, WARP-enrolled devices and Magic WAN tunnels
(cfapi). The agents already see cloudflared on the servers Argus manages
(internal/agent/vpn.go); the account adds what no agent can see: a tunnel
whose connector runs on a machine without an agent, one that has not run
for weeks, one with a single connector.

Read only, with a token that has read permissions alone. Nothing in
Cloudflare is created, changed or deleted. The token is sealed with a key
derived from signing_secret, as Telegram bot tokens are, and never leaves
the control plane again; the console sees its last three characters.

	GET    /api/v1/networks/cloudflare            the accounts (fleet roles)
	POST   /api/v1/networks/cloudflare            add one; the token is tried first (admin, owner)
	PUT    /api/v1/networks/cloudflare/{id}       rename, or a new token (admin, owner)
	DELETE /api/v1/networks/cloudflare/{id}       remove (admin, owner)
	POST   /api/v1/networks/cloudflare/{id}/sync  read it now (admin, owner)
*/

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/caduceus"

	"github.com/gsoultan/argus/internal/control/cfapi"
)

// CloudflareSyncInterval is how often each account is read.
const CloudflareSyncInterval = 5 * time.Minute

const maxCloudflareAccounts = 10

// CloudflareAccount is one account as the console sees it. The token is
// never in it.
type CloudflareAccount struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	AccountID   string            `json:"accountId"`
	TokenTail   string            `json:"tokenTail"`
	SyncedAt    *time.Time        `json:"syncedAt,omitempty"`
	LastError   string            `json:"lastError,omitempty"`
	LastErrorAt *time.Time        `json:"lastErrorAt,omitempty"`
	CreatedBy   string            `json:"createdBy"`
	CreatedAt   time.Time         `json:"createdAt"`
	Read        []string          `json:"read"`
	Missing     map[string]string `json:"missing,omitempty"`
	// snap is the last read, for Networks; never sent as it is.
	snap *cfapi.Snapshot
}

// CloudflareAccountInput is an account as the console sends it. An empty
// Token on a change keeps the saved one.
type CloudflareAccountInput struct {
	Name      string `json:"name"`
	AccountID string `json:"accountId"`
	Token     string `json:"token"`
}

// ErrInvalidCloudflare is input the console should fix.
type ErrInvalidCloudflare struct{ msg string }

func (e ErrInvalidCloudflare) Error() string { return e.msg }

// ErrNoCloudflareAccount is an account that is not there.
var ErrNoCloudflareAccount = errors.New("no such Cloudflare account")

/* ── Sealing tokens ─────────────────────────────────────────────────────── */

func (a *API) cloudflareKey(ctx context.Context) ([]byte, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, ErrInvalidCloudflare{"this control plane cannot keep API tokens safe: set signing_secret in its configuration"}
	}
	sum := sha256.Sum256(append([]byte("argus cloudflare tokens v1\x00"), a.Tunnels.grantKeyFor(ctx).Seed()...))
	return sum[:], nil
}

func (a *API) sealCloudflareToken(ctx context.Context, token string) ([]byte, error) {
	key, err := a.cloudflareKey(ctx)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(token), []byte("cloudflare-token")), nil
}

func (a *API) openCloudflareToken(ctx context.Context, sealed []byte) (string, error) {
	key, err := a.cloudflareKey(ctx)
	if err != nil {
		return "", err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("the account's token cannot be read")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("cloudflare-token"))
	if err != nil {
		return "", errors.New("the account's token cannot be read; was signing_secret changed? Enter the token again")
	}
	return string(plain), nil
}

/* ── Store ──────────────────────────────────────────────────────────────── */

const cloudflareColumns = `id::text, name, account_id, token_tail, synced_at, last_error, last_error_at, created_by, created_at, snapshot`

func scanCloudflare(row rowScanner, extra ...any) (CloudflareAccount, error) {
	var c CloudflareAccount
	var snap []byte
	dest := append([]any{&c.ID, &c.Name, &c.AccountID, &c.TokenTail, &c.SyncedAt, &c.LastError, &c.LastErrorAt,
		&c.CreatedBy, &c.CreatedAt, &snap}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, caduceus.ErrNoRows) {
			return c, ErrNoCloudflareAccount
		}
		return c, err
	}
	c.Read = []string{}
	if len(snap) > 0 {
		var s cfapi.Snapshot
		if json.Unmarshal(snap, &s) == nil {
			c.snap = &s
			c.Read = append(c.Read, s.Read...)
			if len(s.Errors) > 0 {
				c.Missing = s.Errors
			}
		}
	}
	return c, nil
}

// CloudflareAccounts lists the organization's accounts with their last read.
func (s *Store) CloudflareAccounts(ctx context.Context) ([]CloudflareAccount, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+cloudflareColumns+` FROM cloudflare_accounts ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudflareAccount{}
	for rows.Next() {
		c, err := scanCloudflare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// cloudflareAccount is one account and its sealed token.
func (s *Store) cloudflareAccount(ctx context.Context, id string) (CloudflareAccount, []byte, error) {
	if !validUUID(id) {
		return CloudflareAccount{}, nil, ErrNoCloudflareAccount
	}
	var sealed []byte
	c, err := scanCloudflare(s.db(ctx).QueryRow(ctx,
		`SELECT `+cloudflareColumns+`, token_sealed FROM cloudflare_accounts WHERE id = $1`, id), &sealed)
	return c, sealed, err
}

// SaveCloudflareAccount adds an account (id empty) or changes one. sealed
// nil keeps the saved token; snap, when given, is the read that proved it.
func (s *Store) SaveCloudflareAccount(ctx context.Context, id string, in CloudflareAccountInput, sealed []byte, snap *cfapi.Snapshot, by string) (CloudflareAccount, error) {
	var snapJSON []byte
	if snap != nil {
		b, err := json.Marshal(snap)
		if err != nil {
			return CloudflareAccount{}, err
		}
		snapJSON = b
	}
	tail := ""
	if sealed != nil {
		tail = tokenTail(in.Token)
	}
	if id == "" {
		var n int
		if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM cloudflare_accounts`).Scan(&n); err != nil {
			return CloudflareAccount{}, err
		}
		if n >= maxCloudflareAccounts {
			return CloudflareAccount{}, ErrInvalidCloudflare{fmt.Sprintf("an organization can connect up to %d Cloudflare accounts", maxCloudflareAccounts)}
		}
		c, err := scanCloudflare(s.db(ctx).QueryRow(ctx, `
			INSERT INTO cloudflare_accounts (name, account_id, token_sealed, token_tail, snapshot, synced_at, created_by)
			VALUES ($1, $2, $3, $4, $5::jsonb, CASE WHEN $5::jsonb IS NULL THEN NULL ELSE now() END, $6)
			RETURNING `+cloudflareColumns, in.Name, in.AccountID, sealed, tail, snapJSON, strings.ToLower(by)))
		if err != nil && isUniqueViolation(err) {
			return c, ErrInvalidCloudflare{"that Cloudflare account is already connected"}
		}
		return c, err
	}
	if !validUUID(id) {
		return CloudflareAccount{}, ErrNoCloudflareAccount
	}
	return scanCloudflare(s.db(ctx).QueryRow(ctx, `
		UPDATE cloudflare_accounts SET name = $2,
			token_sealed = COALESCE($3::bytea, token_sealed),
			token_tail = CASE WHEN $3::bytea IS NULL THEN token_tail ELSE $4 END,
			snapshot = COALESCE($5::jsonb, snapshot),
			synced_at = CASE WHEN $5::jsonb IS NULL THEN synced_at ELSE now() END,
			last_error = CASE WHEN $5::jsonb IS NULL THEN last_error ELSE '' END,
			updated_at = now()
		WHERE id = $1
		RETURNING `+cloudflareColumns, id, in.Name, sealed, tail, snapJSON))
}

// DeleteCloudflareAccount removes an account and what was read from it.
func (s *Store) DeleteCloudflareAccount(ctx context.Context, id string) (CloudflareAccount, error) {
	if !validUUID(id) {
		return CloudflareAccount{}, ErrNoCloudflareAccount
	}
	return scanCloudflare(s.db(ctx).QueryRow(ctx,
		`DELETE FROM cloudflare_accounts WHERE id = $1 RETURNING `+cloudflareColumns, id))
}

// NoteCloudflareRead stores a read, or the reason it failed. A failed read
// keeps the last snapshot, which Networks then shows as of when it was read.
func (s *Store) NoteCloudflareRead(ctx context.Context, id string, snap *cfapi.Snapshot, readErr error) error {
	if readErr != nil {
		_, err := s.db(ctx).Exec(ctx, `UPDATE cloudflare_accounts SET last_error = $2, last_error_at = now() WHERE id = $1`,
			id, clipText(readErr.Error(), 500))
		return err
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `UPDATE cloudflare_accounts SET snapshot = $2::jsonb, synced_at = now(), last_error = '' WHERE id = $1`, id, b)
	return err
}

/* ── Reading ────────────────────────────────────────────────────────────── */

func (a *API) cloudflareClient() *cfapi.Client {
	hc := a.cloudflareHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &cfapi.Client{Base: a.cloudflareBase, HTTP: hc}
}

// syncCloudflare reads one account and stores what it found.
func (a *API) syncCloudflare(ctx context.Context, c CloudflareAccount, sealed []byte) error {
	token, err := a.openCloudflareToken(ctx, sealed)
	if err == nil {
		var snap cfapi.Snapshot
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		snap, err = a.cloudflareClient().Read(rctx, c.AccountID, token, time.Now())
		cancel()
		if err == nil {
			return a.store.NoteCloudflareRead(ctx, c.ID, &snap, nil)
		}
	}
	if nerr := a.store.NoteCloudflareRead(ctx, c.ID, nil, err); nerr != nil {
		a.log.Error("cloudflare: cannot note a failed read", "error", nerr)
	}
	return err
}

// RunCloudflareSync reads every organization's Cloudflare accounts each
// interval until ctx ends.
func (a *API) RunCloudflareSync(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(45 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			list, err := a.store.CloudflareAccounts(tctx)
			if err != nil {
				a.log.Error("cloudflare sync: cannot list accounts", "error", err)
				return
			}
			for _, c := range list {
				if ctx.Err() != nil {
					return
				}
				_, sealed, err := a.store.cloudflareAccount(tctx, c.ID)
				if err != nil {
					continue
				}
				if err := a.syncCloudflare(tctx, c, sealed); err != nil {
					a.log.Warn("cloudflare sync", "account", c.Name, "error", err)
				}
			}
		})
		if err != nil {
			a.log.Error("cloudflare sync", "error", err)
		}
		t.Reset(interval)
	}
}

/* ── Routes ─────────────────────────────────────────────────────────────── */

func (a *API) cloudflareAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "only an admin or owner can connect or change Cloudflare accounts")
		return "", false
	}
	return sess.Email, true
}

func (a *API) cloudflareErr(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidCloudflare
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.msg)
	case errors.Is(err, cfapi.ErrToken):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNoCloudflareAccount):
		writeErr(w, http.StatusNotFound, "no such Cloudflare account")
	default:
		a.fail(w, what, err)
	}
}

// cloudflareView is the accounts as the console lists them.
type cloudflareView struct {
	Accounts []CloudflareAccount `json:"accounts"`
	// CanSave is false when tokens cannot be kept safe here, with why.
	CanSave    bool   `json:"canSave"`
	CannotSave string `json:"cannotSave,omitempty"`
}

func (a *API) getCloudflareAccounts(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing the networks requires the admin, owner or auditor role")
		return
	}
	list, err := a.store.CloudflareAccounts(r.Context())
	if err != nil {
		a.fail(w, "cloudflare accounts", err)
		return
	}
	v := cloudflareView{Accounts: list, CanSave: true}
	if _, err := a.cloudflareKey(r.Context()); err != nil {
		v.CanSave, v.CannotSave = false, err.Error()
	}
	writeJSON(w, http.StatusOK, v)
}

// readCloudflareInput checks what the console sent and, with a token, tries
// it against the account: an account is only saved once it can be read.
func (a *API) readCloudflareInput(r *http.Request, accountID string, needToken bool) (in CloudflareAccountInput, sealed []byte, snap *cfapi.Snapshot, err error) {
	if err = decode(r, &in); err != nil {
		return in, nil, nil, ErrInvalidCloudflare{err.Error()}
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Token = strings.TrimSpace(in.Token)
	in.AccountID = strings.ToLower(strings.TrimSpace(in.AccountID))
	if accountID != "" {
		in.AccountID = accountID
	}
	if l := len([]rune(in.Name)); l == 0 || l > 80 {
		return in, nil, nil, ErrInvalidCloudflare{"give the account a name of up to 80 characters"}
	}
	if !cfapi.ValidAccountID(in.AccountID) {
		return in, nil, nil, ErrInvalidCloudflare{"the account id is the 32 characters after dash.cloudflare.com/ in the address bar"}
	}
	if in.Token == "" {
		if needToken {
			return in, nil, nil, ErrInvalidCloudflare{"paste a read-only API token"}
		}
		return in, nil, nil, nil
	}
	if len(in.Token) > 200 || strings.ContainsAny(in.Token, " \t\r\n") {
		return in, nil, nil, ErrInvalidCloudflare{"that does not look like a Cloudflare API token"}
	}
	if _, err = a.cloudflareKey(r.Context()); err != nil {
		return in, nil, nil, err
	}
	s, err := a.cloudflareClient().Read(r.Context(), in.AccountID, in.Token, time.Now())
	if err != nil {
		if errors.Is(err, cfapi.ErrToken) {
			return in, nil, nil, err
		}
		return in, nil, nil, ErrInvalidCloudflare{"could not read the account with that token: " + err.Error()}
	}
	if sealed, err = a.sealCloudflareToken(r.Context(), in.Token); err != nil {
		return in, nil, nil, err
	}
	return in, sealed, &s, nil
}

func (a *API) postCloudflareAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudflareAdmin(w, r)
	if !ok {
		return
	}
	in, sealed, snap, err := a.readCloudflareInput(r, "", true)
	if err != nil {
		a.cloudflareErr(w, "add cloudflare account", err)
		return
	}
	c, err := a.store.SaveCloudflareAccount(r.Context(), "", in, sealed, snap, actor)
	if err != nil {
		a.cloudflareErr(w, "add cloudflare account", err)
		return
	}
	a.audit(r, "cloudflare.account.add", actor, "cloudflare account "+c.Name,
		fmt.Sprintf("Connected Cloudflare account %s (%s) read-only, token ending %s.", c.Name, c.AccountID, c.TokenTail))
	writeJSON(w, http.StatusCreated, c)
}

func (a *API) putCloudflareAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudflareAdmin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	before, _, err := a.store.cloudflareAccount(r.Context(), id)
	if err != nil {
		a.cloudflareErr(w, "change cloudflare account", err)
		return
	}
	in, sealed, snap, err := a.readCloudflareInput(r, before.AccountID, false)
	if err != nil {
		a.cloudflareErr(w, "change cloudflare account", err)
		return
	}
	c, err := a.store.SaveCloudflareAccount(r.Context(), id, in, sealed, snap, actor)
	if err != nil {
		a.cloudflareErr(w, "change cloudflare account", err)
		return
	}
	detail := fmt.Sprintf("Changed Cloudflare account %s (%s).", c.Name, c.AccountID)
	if before.Name != c.Name {
		detail += fmt.Sprintf(" Renamed from %s.", before.Name)
	}
	if sealed != nil {
		detail += fmt.Sprintf(" New token ending %s.", c.TokenTail)
	}
	a.audit(r, "cloudflare.account.change", actor, "cloudflare account "+c.Name, detail)
	writeJSON(w, http.StatusOK, c)
}

func (a *API) deleteCloudflareAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudflareAdmin(w, r)
	if !ok {
		return
	}
	c, err := a.store.DeleteCloudflareAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		a.cloudflareErr(w, "remove cloudflare account", err)
		return
	}
	a.audit(r, "cloudflare.account.remove", actor, "cloudflare account "+c.Name,
		fmt.Sprintf("Disconnected Cloudflare account %s (%s). Its token is deleted; revoke it in Cloudflare as well.", c.Name, c.AccountID))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) postCloudflareSync(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudflareAdmin(w, r); !ok {
		return
	}
	c, sealed, err := a.store.cloudflareAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		a.cloudflareErr(w, "read cloudflare account", err)
		return
	}
	if err := a.syncCloudflare(r.Context(), c, sealed); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	c, _, err = a.store.cloudflareAccount(r.Context(), c.ID)
	if err != nil {
		a.cloudflareErr(w, "read cloudflare account", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

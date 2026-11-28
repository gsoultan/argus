package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gsoultan/caduceus"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Settings › Account security › Fleet recovery key (tunnel/recovery.go).

The control plane holds only the public half, and sends it on every
heartbeat reply so each agent pins it. The private half is handed to a
platform owner once, when the key is made, and is never stored here: a
control plane that has been taken over can sign anything with the grant key,
but not with this.

What it signs is made on a machine that holds it, with argus-control:

	argus-control recovery-key replace -old OLD.key -out NEW.key
	argus-control rekey -recovery-key KEY

and stored here only to be passed on to agents: a replacement in
recovery_key.replace_token, a rekey per organization in rekey_orders.
*/

// ErrRecoveryKeyExists is a second key made where one is held; replacing it
// takes the old one's private half, so it is done with argus-control.
var ErrRecoveryKeyExists = errors.New("this deployment already has a fleet recovery key; " +
	"replace it with: argus-control recovery-key replace -old OLD.key -out NEW.key")

// RecoveryKey is the deployment's recovery key, as the console shows it.
type RecoveryKey struct {
	Set         bool      `json:"set"`
	PublicKey   string    `json:"publicKey,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
	CreatedBy   string    `json:"createdBy,omitempty"`
	// Holding is how many of Total servers' agents said at their last
	// check-in that they hold this key; Older hold none and are too old to.
	Holding int `json:"holding"`
	Total   int `json:"total"`
	// Rekeyed is when the newest rekey was made, if any.
	Rekeyed *time.Time `json:"rekeyed,omitempty"`
}

/* ── Store ───────────────────────────────────────────────────────────────── */

// RecoveryPublic reads the public half, nil when there is none.
func (s *Store) RecoveryPublic(ctx context.Context) (pub ed25519.PublicKey, replace string, err error) {
	var enc string
	err = s.pool.QueryRow(ctx, `SELECT public_key, replace_token FROM argus_platform.recovery_key`).
		Scan(&enc, &replace)
	if errors.Is(err, caduceus.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	pub, err = tunnel.DecodeGrantKey(enc)
	return pub, replace, err
}

// CreateRecoveryKey stores pub as the deployment's key, when it has none.
func (s *Store) CreateRecoveryKey(ctx context.Context, pub ed25519.PublicKey, by string) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO argus_platform.recovery_key (public_key, created_by)
		VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, tunnel.EncodeGrantKey(pub), by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRecoveryKeyExists
	}
	return nil
}

// ReplaceRecoveryKey stores a new key and the order, signed with the old
// one, that moves agents to it. The caller has checked the order.
func (s *Store) ReplaceRecoveryKey(ctx context.Context, pub ed25519.PublicKey, token, by string) error {
	_, err := s.pool.Exec(ctx, `UPDATE argus_platform.recovery_key SET public_key = $1,
		replace_token = $2, created_at = now(), created_by = $3`, tunnel.EncodeGrantKey(pub), token, by)
	return err
}

// SaveRekeyOrder stores an organization's rekey, replacing an older one.
func (s *Store) SaveRekeyOrder(ctx context.Context, tenantID, token string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO argus_platform.rekey_orders (tenant_id, token, issued_at)
		VALUES ($1, $2, $3) ON CONFLICT (tenant_id) DO UPDATE SET token = EXCLUDED.token,
		issued_at = EXCLUDED.issued_at`, tenantID, token, at)
	return err
}

// RekeyOrder is the organization's standing rekey, "" for none.
func (s *Store) RekeyOrder(ctx context.Context, tenantID string) (string, error) {
	var tok string
	err := s.pool.QueryRow(ctx, `SELECT token FROM argus_platform.rekey_orders WHERE tenant_id = $1`,
		tenantID).Scan(&tok)
	if errors.Is(err, caduceus.ErrNoRows) {
		return "", nil
	}
	return tok, err
}

// NoteAgentRecoveryKey records which recovery key a server's agent holds,
// writing only when it changed.
func (s *Store) NoteAgentRecoveryKey(ctx context.Context, hostname, fingerprint string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE assets SET agent_recovery_key = $2
		WHERE (hostname = $1 OR agent_hostname = $1 OR split_part(hostname, '.', 1) = $1)
		  AND agent_recovery_key IS DISTINCT FROM $2`, hostname, fingerprint)
	return err
}

// RecoveryHolding counts the organization's servers with an agent, and how
// many of those hold the key named by fingerprint.
func (s *Store) RecoveryHolding(ctx context.Context, fingerprint string) (holding, total int, err error) {
	err = s.db(ctx).QueryRow(ctx, `SELECT count(*) FILTER (WHERE agent_recovery_key = $1),
		count(*) FILTER (WHERE agent_last_seen_at IS NOT NULL)
		FROM assets WHERE archived_at IS NULL`, fingerprint).Scan(&holding, &total)
	return holding, total, err
}

/* ── Rekey ───────────────────────────────────────────────────────────────── */

// GrantPublicFor is the grant key an organization's agents pin under root,
// as TunnelHub.grantKeyFor derives it.
func GrantPublicFor(root ed25519.PrivateKey, tenantID string) ed25519.PublicKey {
	if tenantID == DefaultTenantID {
		return root.Public().(ed25519.PublicKey)
	}
	return tunnel.DeriveGrantKey(root, tenantID).Public().(ed25519.PublicKey)
}

// Rekey signs, with the recovery key, an order for every organization that
// moves its agents to the grant key derived from newSecret, and stores them.
// The recovery key must be the one this deployment holds.
func (s *Store) Rekey(ctx context.Context, recovery ed25519.PrivateKey, newSecret string, now time.Time) (int, error) {
	pub, _, err := s.RecoveryPublic(ctx)
	if err != nil {
		return 0, err
	}
	if pub == nil {
		return 0, errors.New("this deployment has no fleet recovery key, so agents have none to check a rekey with")
	}
	if !pub.Equal(recovery.Public()) {
		return 0, fmt.Errorf("that recovery key (%s) is not this deployment's (%s)",
			tunnel.RecoveryFingerprint(recovery.Public().(ed25519.PublicKey)), tunnel.RecoveryFingerprint(pub))
	}
	root, err := tunnel.GrantKeyFromSecret(newSecret)
	if err != nil {
		return 0, err
	}
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return 0, err
	}
	for _, t := range tenants {
		tok, err := tunnel.SignRecovery(recovery, tunnel.RecoveryOrder{Kind: tunnel.RecoveryRekey,
			Key: tunnel.EncodeGrantKey(GrantPublicFor(root, t.ID)), IssuedAt: now, Tenant: t.ID})
		if err != nil {
			return 0, err
		}
		if err := s.SaveRekeyOrder(ctx, t.ID, tok, now); err != nil {
			return 0, err
		}
	}
	return len(tenants), nil
}

/* ── Heartbeat ───────────────────────────────────────────────────────────── */

// recoveryReply adds the recovery key and any order signed with it to a
// heartbeat reply. A failed read leaves them out: the agent keeps what it has.
func (a *API) recoveryReply(ctx context.Context, reply map[string]any) {
	pub, replace, err := a.store.RecoveryPublic(ctx)
	if err != nil {
		a.log.Error("heartbeat: cannot read the fleet recovery key", "error", err)
		return
	}
	if pub == nil {
		return
	}
	reply["recovery_key"] = tunnel.EncodeGrantKey(pub)
	if replace != "" {
		reply["recovery_replace"] = replace
	}
	if tok, err := a.store.RekeyOrder(ctx, tenantIDOf(ctx)); err != nil {
		a.log.Error("heartbeat: cannot read the rekey order", "error", err)
	} else if tok != "" {
		reply["rekey"] = tok
	}
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) recoveryView(ctx context.Context) (RecoveryKey, error) {
	var v RecoveryKey
	err := a.store.pool.QueryRow(ctx, `SELECT public_key, created_at, created_by FROM argus_platform.recovery_key`).
		Scan(&v.PublicKey, &v.CreatedAt, &v.CreatedBy)
	if errors.Is(err, caduceus.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	pub, err := tunnel.DecodeGrantKey(v.PublicKey)
	if err != nil {
		return v, err
	}
	v.Set, v.Fingerprint = true, tunnel.RecoveryFingerprint(pub)
	var rekeyed *time.Time
	if err := a.store.pool.QueryRow(ctx, `SELECT max(issued_at) FROM argus_platform.rekey_orders`).
		Scan(&rekeyed); err == nil {
		v.Rekeyed = rekeyed
	}
	err = a.store.ForEachTenant(ctx, func(tctx context.Context, t *Tenant) {
		h, n, err := a.store.RecoveryHolding(tctx, v.Fingerprint)
		if err != nil {
			a.log.Warn("recovery key: cannot count servers", "organization", t.Slug, "error", err)
			return
		}
		v.Holding += h
		v.Total += n
	})
	return v, err
}

func (a *API) getRecoveryKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	v, err := a.recoveryView(r.Context())
	if err != nil {
		a.fail(w, "recovery key", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// postRecoveryKey makes the deployment's key and answers with its private
// half, once. Nothing keeps it after this answer.
func (a *API) postRecoveryKey(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	pub, priv, err := tunnel.NewRecoveryKey()
	if err != nil {
		a.fail(w, "recovery key", err)
		return
	}
	if err := a.store.CreateRecoveryKey(r.Context(), pub, sess.Email); err != nil {
		if errors.Is(err, ErrRecoveryKeyExists) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		a.fail(w, "recovery key", err)
		return
	}
	fp := tunnel.RecoveryFingerprint(pub)
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "control.recovery_key_created", Severity: "notice", ActorEmail: sess.Email,
		Target: "argus-control", Detail: fmt.Sprintf("Made the fleet recovery key %s. Its private half "+
			"was shown once and is not kept by Argus; agents pin the public half at their next check-in.", fp),
	}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "control.recovery_key_created")
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"fingerprint": fp, "privateKey": tunnel.EncodeRecoveryPrivate(priv),
		"fileName": "argus-recovery-" + fp + ".key",
	})
}

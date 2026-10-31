package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Per-host agent credentials.

Every agent used to report with the reporter token every gateway also holds.
Root on any one managed host could read it out of the agent's unit and report
sessions, heartbeats and facts as any other host -- or post audit events, read
the inventory and redeem tickets as a gateway. "This host said so" was worth
nothing as evidence, because every host could say it about every other.

Now an administrator mints a short-lived, single-use enrollment token; the
agent exchanges it once for a credential bound to its hostname. That credential
reaches the three routes an agent uses and nothing else, may only report as its
own host, and is revoked on its own. The shared token stays, for gateways.
*/

const (
	// enrollTokenPrefix and agentCredentialPrefix make the two secrets
	// recognisable on sight and in a secret scanner, and let the reporter gate
	// tell an agent credential from the shared token without a lookup.
	enrollTokenPrefix     = "argus-enrol-"
	agentCredentialPrefix = "argus-agent-"

	// DefaultEnrollTokenTTL is long enough to install a package and run one
	// command, and short enough that a token pasted into a ticket or a chat is
	// useless by the time anyone finds it.
	DefaultEnrollTokenTTL = time.Hour
	// MaxEnrollTokenTTL bounds what an administrator may ask for. A day covers
	// "make it now, install tomorrow morning"; longer is a standing credential.
	MaxEnrollTokenTTL = 24 * time.Hour
)

var (
	// ErrEnrollTokenInvalid covers a token nobody issued. Indistinguishable from
	// a mistyped one, deliberately.
	ErrEnrollTokenInvalid = errors.New("enrollment token not recognised")
	// ErrEnrollTokenExpired, ErrEnrollTokenUsed and ErrEnrollTokenWrongHost name
	// the specific refusal, so the person at the terminal knows what to fix.
	// Telling them apart reveals only that a 256-bit token exists, which nobody
	// can find by guessing.
	ErrEnrollTokenExpired   = errors.New("enrollment token has expired; make a new one in the console")
	ErrEnrollTokenUsed      = errors.New("enrollment token has already been used; make a new one in the console")
	ErrEnrollTokenWrongHost = errors.New("enrollment token was made for a different hostname")
	// ErrHostAlreadyEnrolled refuses a second live credential for one host.
	// Replacing it would let anyone holding an unbound token take over an
	// enrolled host's identity, with the real agent finding out only when its
	// reports started failing.
	ErrHostAlreadyEnrolled = errors.New("this hostname already has an enrolled agent; revoke it in the console first")
	// ErrAgentCredentialNotFound is a revoke of something that does not exist,
	// or is already revoked.
	ErrAgentCredentialNotFound = errors.New("no live agent credential with that id")
)

// EnrollToken is a minted token as the console sees it: the secret once, and
// what it can be used for.
type EnrollToken struct {
	ID        string    `json:"id"`
	Token     string    `json:"token,omitempty"`
	Hostname  string    `json:"hostname,omitempty"`
	CreatedBy string    `json:"createdBy"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AgentCredential is an enrolled agent, without its secret.
type AgentCredential struct {
	ID         string     `json:"id"`
	Hostname   string     `json:"hostname"`
	EnrolledAt time.Time  `json:"enrolledAt"`
	EnrolledBy string     `json:"enrolledBy"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	RevokedBy  string     `json:"revokedBy,omitempty"`
	// Tunnel is whether the agent's outbound connection is up, as of its
	// last ping from whichever replica holds it.
	Tunnel TunnelState `json:"tunnel"`
}

// newSecret returns prefix followed by 256 random bits.
func newSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret is what is stored and looked up. A plain digest rather than a
// password hash: the input is 256 random bits, so there is nothing for a slow
// hash to protect, and the lookup sits on the reporting path.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateEnrollToken mints a single-use token. hostname, when set, is the only
// host that may use it.
func (s *Store) CreateEnrollToken(ctx context.Context, createdBy, hostname string,
	ttl time.Duration) (EnrollToken, error) {
	if ttl <= 0 {
		ttl = DefaultEnrollTokenTTL
	}
	if ttl > MaxEnrollTokenTTL {
		ttl = MaxEnrollTokenTTL
	}
	secret, err := newSecret(enrollTokenPrefix)
	if err != nil {
		return EnrollToken{}, err
	}
	out := EnrollToken{Token: secret, Hostname: strings.TrimSpace(hostname), CreatedBy: createdBy}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO agent_enrollment_tokens (token_hash, hostname, created_by, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		RETURNING id::text, expires_at`,
		hashSecret(secret), out.Hostname, createdBy, ttl.Seconds()).Scan(&out.ID, &out.ExpiresAt)
	if err != nil {
		return EnrollToken{}, fmt.Errorf("create enrollment token: %w", err)
	}
	return out, nil
}

// Enrollment is what a successful exchange produced.
type Enrollment struct {
	CredentialID string
	Credential   string
	// TokenID and TokenCreatedBy say who authorised it, for the audit entry.
	TokenID        string
	TokenCreatedBy string
}

// Enroll exchanges a token for a credential bound to hostname.
//
// One transaction: the token is consumed in the statement that checks it, so
// two agents racing with one token cannot both win, and a refused credential
// (the host already has one) rolls the consumption back so the token can still
// be used once the old credential is revoked.
func (s *Store) Enroll(ctx context.Context, token, hostname string) (Enrollment, error) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return Enrollment{}, errors.New("hostname is required")
	}
	if !strings.HasPrefix(token, enrollTokenPrefix) {
		return Enrollment{}, ErrEnrollTokenInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out Enrollment
	err = tx.QueryRow(ctx, `
		UPDATE agent_enrollment_tokens
		   SET used_at = now(), used_by_host = $2
		 WHERE token_hash = $1
		   AND used_at IS NULL
		   AND expires_at > now()
		   AND (hostname = '' OR lower(hostname) = lower($2))
		RETURNING id::text, created_by`, hashSecret(token), hostname).
		Scan(&out.TokenID, &out.TokenCreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		// Asked on this transaction's own connection. Taking a second one from
		// the pool while holding this one deadlocks as soon as enough agents
		// enroll at once to fill the pool -- each waiting for a connection the
		// others hold.
		return Enrollment{}, whyEnrollRefused(ctx, tx, token, hostname)
	}
	if err != nil {
		return Enrollment{}, fmt.Errorf("consume enrollment token: %w", err)
	}

	out.Credential, err = newSecret(agentCredentialPrefix)
	if err != nil {
		return Enrollment{}, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_credentials (hostname, secret_hash, token_id)
		VALUES ($1, $2, $3::uuid)
		RETURNING id::text`, hostname, hashSecret(out.Credential), out.TokenID).
		Scan(&out.CredentialID)
	if err != nil {
		if isUniqueViolation(err) {
			return Enrollment{}, ErrHostAlreadyEnrolled
		}
		return Enrollment{}, fmt.Errorf("create agent credential: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Enrollment{}, err
	}
	return out, nil
}

// whyEnrollRefused names the reason a token was not accepted.
func whyEnrollRefused(ctx context.Context, tx pgx.Tx, token, hostname string) error {
	var (
		bound   string
		expired bool
		used    bool
	)
	err := tx.QueryRow(ctx, `
		SELECT hostname, expires_at <= now(), used_at IS NOT NULL
		  FROM agent_enrollment_tokens WHERE token_hash = $1`, hashSecret(token)).
		Scan(&bound, &expired, &used)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrEnrollTokenInvalid
	case err != nil:
		return err
	case used:
		return ErrEnrollTokenUsed
	case expired:
		return ErrEnrollTokenExpired
	case bound != "" && !strings.EqualFold(bound, hostname):
		return ErrEnrollTokenWrongHost
	}
	// Valid when re-read: it expired or was used between the two statements.
	return ErrEnrollTokenUsed
}

// AgentCredentials lists every enrolled agent, revoked ones included: which
// credential vouched for a past session is part of reading it.
func (s *Store) AgentCredentials(ctx context.Context) ([]AgentCredential, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text, c.hostname, c.created_at, t.created_by,
		       c.revoked_at, COALESCE(c.revoked_by, ''),
		       c.tunnel_transport, c.tunnel_replica, c.tunnel_connected_at,
		       c.tunnel_seen_at, c.tunnel_rtt_us, now()
		  FROM agent_credentials c
		  JOIN agent_enrollment_tokens t ON t.id = c.token_id
		 ORDER BY c.revoked_at IS NOT NULL, lower(c.hostname), c.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentCredential{}
	for rows.Next() {
		var (
			c                  AgentCredential
			transport, replica *string
			connected, seen    *time.Time
			rtt                *int32
			now                time.Time
		)
		if err := rows.Scan(&c.ID, &c.Hostname, &c.EnrolledAt, &c.EnrolledBy,
			&c.RevokedAt, &c.RevokedBy,
			&transport, &replica, &connected, &seen, &rtt, &now); err != nil {
			return nil, err
		}
		// The database's clock, not this process's: seen times are written
		// by whichever replica holds the tunnel, and replicas' clocks drift.
		if c.RevokedAt == nil {
			c.Tunnel = tunnelStateFrom(transport, replica, connected, seen, rtt, now)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeAgentCredential withdraws one agent's credential. Returns its hostname.
func (s *Store) RevokeAgentCredential(ctx context.Context, id, by string) (string, error) {
	var hostname string
	err := s.pool.QueryRow(ctx, `
		UPDATE agent_credentials SET revoked_at = now(), revoked_by = $2
		 WHERE id::text = $1 AND revoked_at IS NULL
		RETURNING hostname`, id, by).Scan(&hostname)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAgentCredentialNotFound
	}
	if err != nil {
		return "", err
	}
	return hostname, nil
}

// agentCredentialHost returns the hostname a live credential is bound to.
func (s *Store) agentCredentialHost(ctx context.Context, hash string) (id, hostname string, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT id::text, hostname FROM agent_credentials
		 WHERE secret_hash = $1 AND revoked_at IS NULL`, hash).Scan(&id, &hostname)
	return id, hostname, err
}

/* ── Lookup cache ────────────────────────────────────────────────────────── */

// agentCredCacheTTL bounds how long a revocation takes to reach a control plane
// that did not perform it. The one that did evicts at once.
const agentCredCacheTTL = 30 * time.Second

// agentCredCacheMax bounds the cache. Only credentials that verified are ever
// stored, so it cannot be grown by guessing -- only by enrolling hosts -- but a
// bound costs nothing and the standing rule is that every map keyed by what a
// caller sends has one.
const agentCredCacheMax = 20_000

type agentCredEntry struct {
	id, hostname string
	checked      time.Time
}

// agentCredCache spares a database round trip on every agent report. A fleet
// of a thousand agents heartbeating every 30 seconds would otherwise be 33
// lookups a second for an answer that changes when someone revokes a host.
type agentCredCache struct {
	mu sync.Mutex
	m  map[string]agentCredEntry
}

func (c *agentCredCache) get(hash string, now time.Time) (agentCredEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[hash]
	if !ok || now.Sub(e.checked) > agentCredCacheTTL {
		return agentCredEntry{}, false
	}
	return e, true
}

func (c *agentCredCache) put(hash string, e agentCredEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]agentCredEntry{}
	}
	if len(c.m) >= agentCredCacheMax {
		// Expired first; if that frees nothing, start again. Refilling costs
		// one query per agent, which is what having no cache costs every time.
		for k, v := range c.m {
			if e.checked.Sub(v.checked) > agentCredCacheTTL {
				delete(c.m, k)
			}
		}
		if len(c.m) >= agentCredCacheMax {
			clear(c.m)
		}
	}
	c.m[hash] = e
}

// forget drops a revoked credential at once.
func (c *agentCredCache) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.m {
		if v.id == id {
			delete(c.m, k)
		}
	}
}

// RequestAgentUninstall asks the live agent on hostname to uninstall itself,
// which it reads on its next heartbeat. Reports whether there was one to ask.
// Asking twice keeps the first request.
func (s *Store) RequestAgentUninstall(ctx context.Context, hostname, by string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		WITH asked AS (
			UPDATE agent_credentials
			   SET uninstall_requested_at = COALESCE(uninstall_requested_at, now()),
			       uninstall_requested_by = COALESCE(uninstall_requested_by, $2)
			 WHERE lower(hostname) = lower($1) AND revoked_at IS NULL
			RETURNING 1)
		SELECT count(*) FROM asked`, hostname, by).Scan(&n)
	return n > 0, err
}

// agentUninstallRequested reports whether hostname's live agent has been
// asked to uninstall itself.
func (s *Store) agentUninstallRequested(ctx context.Context, hostname string) (bool, error) {
	var asked bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM agent_credentials
		                WHERE lower(hostname) = lower($1) AND revoked_at IS NULL
		                  AND uninstall_requested_at IS NOT NULL)`, hostname).Scan(&asked)
	return asked, err
}

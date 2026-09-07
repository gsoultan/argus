// Package control implements the Argus control plane: the system of record for
// assets, sessions, agents and the audit log.
//
// The gateway and the agent are deliberately able to run without it — a
// control-plane outage must never stop a session being recorded, only stop it
// being reported promptly. Both spool locally and reconcile later.
package control

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// GenesisHash roots the audit chain. Fixed so a verifier needs nothing but the
// log itself.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Store is the control plane's persistence layer.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and applies migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// migrate applies every embedded migration in filename order.
//
// Each runs inside a transaction and is recorded, so a partially applied
// migration can never leave the schema in a state the next start would compound.
func (s *Store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}
	return s.applyMigrations(ctx, names)
}

// migrationNames lists every embedded migration in the order it must run.
//
// Ordering is by full filename, not by the numeric prefix, which is why two
// files may share a prefix without ambiguity -- 003_recording_path sorts before
// 003_shared_state and both are tracked separately. The prefix is a label for
// humans; the filename is the identity.
func migrationNames() ([]string, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// applyMigrations runs the named migrations that have not already been applied.
//
// Split out from migrate so the upgrade path can be tested the way it is
// actually taken: some migrations applied, real data written, then the rest.
// Running the whole set against an empty database -- which is all CI did --
// exercises the one case no customer is ever in.
func (s *Store) applyMigrations(ctx context.Context, names []string) error {
	for _, name := range names {
		var applied bool
		err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&applied)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

/* ── Sessions ────────────────────────────────────────────────────────────── */

// Session mirrors the console's Session type.
type Session struct {
	ID             string     `json:"id"`
	UserEmail      string     `json:"userEmail"`
	AssetID        *string    `json:"assetId"`
	AssetHostname  string     `json:"assetHostname"`
	Principal      string     `json:"principal"`
	Protocol       string     `json:"protocol"`
	Origin         string     `json:"origin"`
	OriginReason   *string    `json:"originReason,omitempty"`
	State          string     `json:"state"`
	StartedAt      time.Time  `json:"startedAt"`
	EndedAt        *time.Time `json:"endedAt"`
	ClientIP       string     `json:"clientIp"`
	Fidelity       string     `json:"fidelity"`
	RecordingBytes int64      `json:"recordingBytes"`
	CommandCount   *int       `json:"commandCount"`
	ExitCode       *int       `json:"exitCode,omitempty"`
	ChainHead      *string    `json:"chainHead"`
	RiskFlags      []string   `json:"riskFlags"`
	ReportedBy     string     `json:"reportedBy,omitempty"`

	// TerminatedBy and TerminationReason record an ended session that someone
	// or something stopped, rather than one that finished. The gateway sent
	// both long before there was anywhere to keep them, and decode() rejects
	// unknown fields, so every terminated session's report was refused whole.
	TerminatedBy      *string `json:"terminatedBy,omitempty"`
	TerminationReason *string `json:"terminationReason,omitempty"`
	// RecordingKey is the object-storage key. Empty means the artefact never
	// left the host that produced it.
	RecordingKey *string `json:"recordingKey,omitempty"`
	// RecordingPath is what a reporter sends; it becomes RecordingKey.
	RecordingPath string `json:"recordingPath,omitempty"`
}

// UpsertSession records or updates a session.
//
// Upsert rather than insert because the same session can legitimately be
// reported twice: the gateway reports it when it opens, and the host agent
// reports it independently. Reconciling on the session id keeps one row.
func (s *Store) UpsertSession(ctx context.Context, in Session) error {
	// Resolve the asset by hostname so a reporter never needs to know its id.
	var assetID *string
	var id string
	err := s.pool.QueryRow(ctx,
		`SELECT id::text FROM assets WHERE hostname = $1`, in.AssetHostname).Scan(&id)
	if err == nil {
		assetID = &id
	} else if err != pgx.ErrNoRows {
		return fmt.Errorf("resolve asset: %w", err)
	}

	if in.RiskFlags == nil {
		in.RiskFlags = []string{}
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO sessions (
			id, user_email, asset_id, asset_hostname, principal, protocol,
			origin, origin_reason, state, started_at, ended_at, client_ip,
			fidelity, recording_bytes, command_count, exit_code, chain_head,
			risk_flags, reported_by, recording_key, terminated_by,
			termination_reason
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (id) DO UPDATE SET
			state           = EXCLUDED.state,
			ended_at        = COALESCE(EXCLUDED.ended_at, sessions.ended_at),
			recording_bytes = GREATEST(EXCLUDED.recording_bytes, sessions.recording_bytes),
			command_count   = COALESCE(EXCLUDED.command_count, sessions.command_count),
			exit_code       = COALESCE(EXCLUDED.exit_code, sessions.exit_code),
			chain_head      = COALESCE(EXCLUDED.chain_head, sessions.chain_head),
			risk_flags      = EXCLUDED.risk_flags,
			recording_key   = COALESCE(EXCLUDED.recording_key, sessions.recording_key),
			-- COALESCE, not EXCLUDED: a later report that omits the reason must
			-- not erase why a session was stopped.
			terminated_by      = COALESCE(EXCLUDED.terminated_by, sessions.terminated_by),
			termination_reason = COALESCE(EXCLUDED.termination_reason, sessions.termination_reason)`,
		in.ID, in.UserEmail, assetID, in.AssetHostname, in.Principal, in.Protocol,
		in.Origin, in.OriginReason, in.State, in.StartedAt, in.EndedAt, in.ClientIP,
		in.Fidelity, in.RecordingBytes, in.CommandCount, in.ExitCode, in.ChainHead,
		in.RiskFlags, in.ReportedBy, nullIfEmpty(in.RecordingPath),
		in.TerminatedBy, in.TerminationReason)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// SessionFilter narrows a session query.
type SessionFilter struct {
	State  string
	Origin string
	Limit  int
}

// Sessions lists sessions, newest first.
func (s *Store) Sessions(ctx context.Context, f SessionFilter) ([]Session, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id::text, user_email, asset_id::text, asset_hostname, principal,
		       protocol, origin, origin_reason, state, started_at, ended_at,
		       client_ip, fidelity, recording_bytes, command_count, exit_code,
		       chain_head, risk_flags, reported_by, recording_key,
		       terminated_by, termination_reason
		FROM sessions
		WHERE ($1 = '' OR state = $1)
		  AND ($2 = '' OR origin = $2)
		ORDER BY started_at DESC
		LIMIT $3`, f.State, f.Origin, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var v Session
		if err := rows.Scan(&v.ID, &v.UserEmail, &v.AssetID, &v.AssetHostname,
			&v.Principal, &v.Protocol, &v.Origin, &v.OriginReason, &v.State,
			&v.StartedAt, &v.EndedAt, &v.ClientIP, &v.Fidelity, &v.RecordingBytes,
			&v.CommandCount, &v.ExitCode, &v.ChainHead, &v.RiskFlags, &v.ReportedBy,
			&v.RecordingKey, &v.TerminatedBy, &v.TerminationReason); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Session fetches one by id.
func (s *Store) Session(ctx context.Context, id string) (*Session, error) {
	var v Session
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, user_email, asset_id::text, asset_hostname, principal,
		       protocol, origin, origin_reason, state, started_at, ended_at,
		       client_ip, fidelity, recording_bytes, command_count, exit_code,
		       chain_head, risk_flags, reported_by, recording_key,
		       terminated_by, termination_reason
		FROM sessions WHERE id = $1`, id).Scan(
		&v.ID, &v.UserEmail, &v.AssetID, &v.AssetHostname, &v.Principal,
		&v.Protocol, &v.Origin, &v.OriginReason, &v.State, &v.StartedAt,
		&v.EndedAt, &v.ClientIP, &v.Fidelity, &v.RecordingBytes,
		&v.CommandCount, &v.ExitCode, &v.ChainHead, &v.RiskFlags, &v.ReportedBy,
		&v.RecordingKey, &v.TerminatedBy, &v.TerminationReason)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

/* ── Assets ──────────────────────────────────────────────────────────────── */

// Asset mirrors the console's Asset type.
type Asset struct {
	ID                   string     `json:"id"`
	Hostname             string     `json:"hostname"`
	Address              string     `json:"address"`
	Port                 int        `json:"port"`
	OS                   string     `json:"os"`
	Tags                 []string   `json:"tags"`
	GroupName            string     `json:"groupName"`
	CredentialMode       string     `json:"credentialMode"`
	CredentialRotatedAt  *time.Time `json:"credentialRotatedAt"`
	RotationIntervalDays *int       `json:"rotationIntervalDays"`
	HostKeyState         string     `json:"hostKeyState"`
	HostKeyFingerprint   *string    `json:"hostKeyFingerprint"`
	HostKeyPinnedAt      *time.Time `json:"hostKeyPinnedAt"`
	Health               string     `json:"health"`
	LastCheckedAt        time.Time  `json:"lastCheckedAt"`
	Principals           []string   `json:"principals"`
	AgentState           string     `json:"agentState"`
	AgentLastSeenAt      *time.Time `json:"agentLastSeenAt"`
	BypassPosture        string     `json:"bypassPosture"`
	UnmanagedKeyCount    int        `json:"unmanagedKeyCount"`
}

// UpsertAsset registers or updates an asset by hostname.
func (s *Store) UpsertAsset(ctx context.Context, a Asset) error {
	if a.Tags == nil {
		a.Tags = []string{}
	}
	if a.Principals == nil {
		a.Principals = []string{}
	}
	if a.Port == 0 {
		a.Port = 22
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO assets (hostname, address, port, os, tags, group_name,
			credential_mode, host_key_state, host_key_fingerprint,
			host_key_pinned_at, health, principals)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (hostname) DO UPDATE SET
			address              = EXCLUDED.address,
			port                 = EXCLUDED.port,
			os                   = COALESCE(NULLIF(EXCLUDED.os,''), assets.os),
			principals           = EXCLUDED.principals,
			credential_mode      = EXCLUDED.credential_mode,
			host_key_state       = EXCLUDED.host_key_state,
			host_key_fingerprint = COALESCE(EXCLUDED.host_key_fingerprint, assets.host_key_fingerprint),
			host_key_pinned_at   = COALESCE(EXCLUDED.host_key_pinned_at, assets.host_key_pinned_at),
			updated_at           = now()`,
		a.Hostname, a.Address, a.Port, a.OS, a.Tags, a.GroupName,
		a.CredentialMode, a.HostKeyState, a.HostKeyFingerprint,
		a.HostKeyPinnedAt, a.Health, a.Principals)
	return err
}

// Assets lists the inventory.
func (s *Store) Assets(ctx context.Context) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, hostname, address, port, os, tags, group_name,
		       credential_mode, credential_rotated_at, rotation_interval_days,
		       host_key_state, host_key_fingerprint, host_key_pinned_at,
		       health, last_checked_at, principals,
		       agent_state, agent_last_seen_at, bypass_posture, unmanaged_key_count
		FROM assets ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Asset{}
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.ID, &a.Hostname, &a.Address, &a.Port, &a.OS,
			&a.Tags, &a.GroupName, &a.CredentialMode, &a.CredentialRotatedAt,
			&a.RotationIntervalDays, &a.HostKeyState, &a.HostKeyFingerprint,
			&a.HostKeyPinnedAt, &a.Health, &a.LastCheckedAt, &a.Principals,
			&a.AgentState, &a.AgentLastSeenAt, &a.BypassPosture,
			&a.UnmanagedKeyCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

/* ── Agents ──────────────────────────────────────────────────────────────── */

// Heartbeat records agent liveness and posture.
//
// It also updates the asset's coverage fields, because "is this host being
// watched" is a property of the host that the console needs to show, not
// something an operator should have to join two views to work out.
func (s *Store) Heartbeat(ctx context.Context, hostname, version string,
	activeSessions int, posture any) error {

	var postureJSON []byte
	if posture != nil {
		b, err := json.Marshal(posture)
		if err != nil {
			return err
		}
		postureJSON = b
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO agents (hostname, version, last_seen_at, active_sessions, posture, updated_at)
		VALUES ($1,$2,now(),$3,$4,now())
		ON CONFLICT (hostname) DO UPDATE SET
			version         = EXCLUDED.version,
			last_seen_at    = now(),
			active_sessions = EXCLUDED.active_sessions,
			posture         = COALESCE(EXCLUDED.posture, agents.posture),
			updated_at      = now()`,
		hostname, version, activeSessions, postureJSON); err != nil {
		return err
	}

	rating, unmanaged := postureSummary(posture)

	// Match on the inventory hostname, the explicit agent_hostname mapping, or
	// the short name. An agent rarely knows the fully qualified name the
	// inventory uses.
	tag, err := tx.Exec(ctx, `
		UPDATE assets SET
			agent_state         = 'healthy',
			agent_last_seen_at  = now(),
			bypass_posture      = COALESCE(NULLIF($2,''), bypass_posture),
			unmanaged_key_count = COALESCE($3, unmanaged_key_count),
			updated_at          = now()
		WHERE hostname = $1
		   OR agent_hostname = $1
		   OR split_part(hostname, '.', 1) = $1`, hostname, rating, unmanaged)
	if err != nil {
		return err
	}
	matched := tag.RowsAffected() > 0

	if _, err := tx.Exec(ctx,
		`UPDATE agents SET matched_asset = $2 WHERE hostname = $1`, hostname, matched); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if !matched {
		// Not an error, but never silent: either the inventory is missing a
		// host, or the mapping is wrong and this host's coverage is not being
		// tracked at all.
		return ErrAgentUnmatched
	}
	return nil
}

// ErrAgentUnmatched means a heartbeat arrived from a host the inventory does not
// know. The heartbeat is still recorded; the asset's coverage is not updated.
var ErrAgentUnmatched = errors.New("agent hostname does not match any asset")

// MarkStaleAgents flags agents that have stopped reporting.
//
// This is the control that makes killing an agent detectable. Root on the host
// can stop the process; it cannot stop the absence being noticed here.
func (s *Store) MarkStaleAgents(ctx context.Context, after time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE assets SET agent_state = 'stale'
		WHERE agent_state = 'healthy'
		  AND agent_last_seen_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(after.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func postureSummary(posture any) (rating string, unmanaged *int) {
	m, ok := posture.(map[string]any)
	if !ok {
		return "", nil
	}
	if v, ok := m["rating"].(string); ok {
		rating = v
	}
	if keys, ok := m["unmanaged_keys"].([]any); ok {
		n := len(keys)
		unmanaged = &n
	}
	return rating, unmanaged
}

/* ── Audit ───────────────────────────────────────────────────────────────── */

// AuditEvent is one link in the chain.
type AuditEvent struct {
	Seq        int64     `json:"seq"`
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	Action     string    `json:"action"`
	Severity   string    `json:"severity"`
	ActorEmail string    `json:"actorEmail"`
	Target     string    `json:"target"`
	Detail     string    `json:"detail"`
	PrevHash   string    `json:"prevHash"`
	Hash       string    `json:"hash"`
}

// AppendAudit adds an event, chaining it to the current head.
//
// Serialised through a transaction that locks the tail row: two concurrent
// writers computing from the same head would produce two events claiming the
// same predecessor, and the chain would no longer verify.
func (s *Store) AppendAudit(ctx context.Context, e AuditEvent) (AuditEvent, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return e, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Advisory lock rather than a table lock: cheap, and scoped to audit
	// appends so it never blocks a session write.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('argus_audit'))`); err != nil {
		return e, err
	}

	prev := GenesisHash
	err = tx.QueryRow(ctx, `SELECT hash FROM audit_events ORDER BY seq DESC LIMIT 1`).Scan(&prev)
	if err != nil && err != pgx.ErrNoRows {
		return e, err
	}

	// Truncated to what the column can hold, before anything is hashed.
	//
	// TIMESTAMPTZ stores microseconds; Go's time.Time carries nanoseconds, and
	// chainHash formats with RFC3339Nano. So a hash computed over the in-memory
	// value could never be recomputed from the stored row -- verification read
	// back a timestamp Postgres had already rounded and declared the record
	// modified. It survived review because Go's clock on macOS is microsecond
	// granular, so the nanosecond digits were usually zero; on Linux they are
	// not, and the chain failed to verify for very nearly every event written.
	//
	// Truncating here rather than in chainHash keeps the rule in one place: the
	// value that is hashed is the value that is stored.
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.At = e.At.UTC().Truncate(time.Microsecond)
	if e.Severity == "" {
		e.Severity = "info"
	}
	e.PrevHash = prev
	e.Hash = chainHash(prev, e)

	err = tx.QueryRow(ctx, `
		INSERT INTO audit_events (at, action, severity, actor_email, target, detail, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING seq, id::text`,
		e.At, e.Action, e.Severity, e.ActorEmail, e.Target, e.Detail, e.PrevHash, e.Hash).
		Scan(&e.Seq, &e.ID)
	if err != nil {
		return e, err
	}
	return e, tx.Commit(ctx)
}

// chainHash must match the console's worker byte for byte, or a log that
// verifies on the server fails in the browser.
func chainHash(prev string, e AuditEvent) string {
	payload, _ := json.Marshal([]any{
		e.Action, e.ActorEmail, e.At.UTC().Format(time.RFC3339Nano),
		e.Detail, e.Severity, e.Target,
	})
	sum := sha256.New()
	sum.Write([]byte(prev))
	sum.Write(payload)
	return hex.EncodeToString(sum.Sum(nil))
}

// AuditEvents lists the log, newest first.
func (s *Store) AuditEvents(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT seq, id::text, at, action, severity, actor_email, target, detail, prev_hash, hash
		FROM audit_events ORDER BY seq DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.Seq, &e.ID, &e.At, &e.Action, &e.Severity,
			&e.ActorEmail, &e.Target, &e.Detail, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		// UTC, because chainHash hashes the UTC rendering and this value is
		// about to be marshalled to JSON for a console that recomputes the
		// same digest. pgx returns the driver's session timezone, so without
		// this the browser would be handed "…+07:00" and asked to reproduce a
		// hash taken over "…Z".
		e.At = e.At.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

/* ── Stats ───────────────────────────────────────────────────────────────── */

// FleetStats mirrors the console's dashboard tiles.
type FleetStats struct {
	AssetsTotal              int `json:"assetsTotal"`
	AssetsUnreachable        int `json:"assetsUnreachable"`
	HostKeysUnpinned         int `json:"hostKeysUnpinned"`
	SessionsActive           int `json:"sessionsActive"`
	SessionsToday            int `json:"sessionsToday"`
	RequestsPending          int `json:"requestsPending"`
	CredentialsOverdue       int `json:"credentialsOverdue"`
	StandingCredentialAssets int `json:"standingCredentialAssets"`
	SessionsDirectToday      int `json:"sessionsDirectToday"`
	AssetsUnmonitored        int `json:"assetsUnmonitored"`
	AgentsStale              int `json:"agentsStale"`
}

// Stats computes the dashboard counters in one round trip.
func (s *Store) Stats(ctx context.Context) (FleetStats, error) {
	var st FleetStats
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM assets),
		  (SELECT count(*) FROM assets WHERE health = 'unreachable'),
		  (SELECT count(*) FROM assets WHERE host_key_state <> 'pinned'),
		  (SELECT count(*) FROM sessions WHERE state = 'active'),
		  (SELECT count(*) FROM sessions WHERE started_at > now() - interval '24 hours'),
		  (SELECT count(*) FROM access_requests WHERE state = 'pending'),
		  (SELECT count(*) FROM assets
		     WHERE credential_rotated_at IS NOT NULL
		       AND rotation_interval_days IS NOT NULL
		       AND credential_rotated_at < now() - (rotation_interval_days || ' days')::interval),
		  (SELECT count(*) FROM assets WHERE credential_mode <> 'ca-certificate'),
		  (SELECT count(*) FROM sessions
		     WHERE origin = 'direct' AND started_at > now() - interval '24 hours'),
		  (SELECT count(*) FROM assets WHERE bypass_posture = 'open'),
		  (SELECT count(*) FROM assets WHERE agent_state = 'stale')`).
		Scan(&st.AssetsTotal, &st.AssetsUnreachable, &st.HostKeysUnpinned,
			&st.SessionsActive, &st.SessionsToday, &st.RequestsPending,
			&st.CredentialsOverdue, &st.StandingCredentialAssets,
			&st.SessionsDirectToday, &st.AssetsUnmonitored, &st.AgentsStale)
	return st, err
}

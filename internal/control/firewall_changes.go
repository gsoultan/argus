package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/jackc/pgx/v5"
)

/*
Changes to a server's firewall, kept (migration 114). The routes are in
firewall_routes.go, the heartbeat's half in firewall_beat.go.

A change holds a delta, not a whole firewall: add these lines, remove
those, take over these rules made by hand, put this policy version on.
It is worked out against what Argus keeps on the server (firewall_servers)
when it is sent, so two changes queued one after the other both land. What
the server held before is kept with the change, which is what Put back
puts back.
*/

// Change states.
const (
	FWAwaitingApproval = "awaiting_approval"
	FWQueued           = "queued"
	FWSent             = "sent"
	FWTrial            = "trial"
	FWWaitingKeep      = "waiting_keep"
	FWApplied          = "applied"
	FWFailed           = "failed"
	FWRolledBack       = "rolled_back"
	FWDenied           = "denied"
	FWCancelled        = "cancelled"
	FWInterrupted      = "interrupted"
	FWStopped          = "stopped"
)

func fwOpen(state string) bool {
	switch state {
	case FWAwaitingApproval, FWQueued, FWSent, FWTrial, FWWaitingKeep:
		return true
	}
	return false
}

// ErrFWChangeNotFound is a change id nobody knows.
var ErrFWChangeNotFound = errors.New("no such firewall change")

// fwServerState is what Argus keeps on one server.
type fwServerState struct {
	Own        []FWLine       `json:"own"`
	Policies   map[string]int `json:"policies"`
	DefaultOut string         `json:"defaultOut,omitempty"`
}

func (st fwServerState) clone() fwServerState {
	out := fwServerState{Own: slices.Clone(st.Own), Policies: map[string]int{}, DefaultOut: st.DefaultOut}
	for k, v := range st.Policies {
		out.Policies[k] = v
	}
	return out
}

// FWDelta is what one change does.
type FWDelta struct {
	// Add are new own lines, keys set.
	Add []FWLine `json:"add,omitempty"`
	// Remove are own lines' keys.
	Remove []string `json:"remove,omitempty"`
	// TakeOver are rules made by hand, by their raw text, that Argus
	// removes; their lines are in Add.
	TakeOver []string `json:"takeOver,omitempty"`
	TurnOn   bool     `json:"turnOn,omitempty"`
	Install  string   `json:"install,omitempty"`
	// PutBack is the change whose saved copy goes back.
	PutBack string `json:"putBack,omitempty"`
	// Policies sets a policy's version on the server; 0 takes it off.
	Policies map[string]int `json:"policies,omitempty"`
	// ExpectHash is the firewall's state the change was planned on, for
	// changes that act on rules made by hand.
	ExpectHash string `json:"expectHash,omitempty"`
}

// apply is st after d, at now: expired lines fall off on every change.
func (d FWDelta) apply(st fwServerState, now time.Time) fwServerState {
	out := st.clone()
	out.Own = out.Own[:0:0]
	for _, l := range st.Own {
		if slices.Contains(d.Remove, l.Key) || l.expired(now) {
			continue
		}
		out.Own = append(out.Own, l)
	}
	out.Own = append(out.Own, d.Add...)
	sortFWLines(out.Own)
	for id, v := range d.Policies {
		if v <= 0 {
			delete(out.Policies, id)
		} else {
			out.Policies[id] = v
		}
	}
	return out
}

// FirewallChange is one change to one server's firewall.
type FirewallChange struct {
	ID            string               `json:"id"`
	AssetID       string               `json:"assetId"`
	Hostname      string               `json:"-"`
	AssetHostname string               `json:"hostname"`
	Kind          string               `json:"kind"`
	Summary       string               `json:"summary"`
	Reason        string               `json:"reason"`
	Delta         FWDelta              `json:"delta"`
	Preview       []string             `json:"preview"`
	State         string               `json:"state"`
	RolloutID     *string              `json:"rolloutId,omitempty"`
	Wave          int                  `json:"wave"`
	RequestedBy   string               `json:"requestedBy"`
	RequestedAt   time.Time            `json:"requestedAt"`
	DecidedBy     *string              `json:"decidedBy,omitempty"`
	DecidedAt     *time.Time           `json:"decidedAt,omitempty"`
	KeptBy        *string              `json:"keptBy,omitempty"`
	KeptAt        *time.Time           `json:"keptAt,omitempty"`
	SentAt        *time.Time           `json:"sentAt,omitempty"`
	EndedAt       *time.Time           `json:"endedAt,omitempty"`
	UpdatedAt     time.Time            `json:"updatedAt"`
	Message       string               `json:"message"`
	Ran           []tunnel.FirewallRan `json:"ran"`
	// KeepBefore is when a change waiting for Keep undoes itself.
	KeepBefore *time.Time `json:"keepBefore,omitempty"`
	// CanPutBack: the change was applied and its saved copy is on the
	// server.
	CanPutBack bool `json:"canPutBack"`

	before *fwServerState
	spec   *tunnel.FirewallChangeSpec
}

const fwChangeCols = `id::text, asset_id::text, hostname, asset_hostname, kind, summary, reason, delta, before, spec,
	preview, state, rollout_id::text, wave, requested_by, requested_at, decided_by, decided_at, kept_by, kept_at,
	sent_at, ended_at, updated_at, message, ran`

func scanFWChange(row pgx.Row) (FirewallChange, error) {
	var c FirewallChange
	var delta, before, spec, preview, ran []byte
	err := row.Scan(&c.ID, &c.AssetID, &c.Hostname, &c.AssetHostname, &c.Kind, &c.Summary, &c.Reason, &delta,
		&before, &spec, &preview, &c.State, &c.RolloutID, &c.Wave, &c.RequestedBy, &c.RequestedAt, &c.DecidedBy,
		&c.DecidedAt, &c.KeptBy, &c.KeptAt, &c.SentAt, &c.EndedAt, &c.UpdatedAt, &c.Message, &ran)
	if err != nil {
		return c, err
	}
	_ = json.Unmarshal(delta, &c.Delta)
	_ = json.Unmarshal(preview, &c.Preview)
	_ = json.Unmarshal(ran, &c.Ran)
	if len(before) > 0 && string(before) != "null" {
		var st fwServerState
		if json.Unmarshal(before, &st) == nil {
			c.before = &st
		}
	}
	if len(spec) > 0 && string(spec) != "null" {
		var sp tunnel.FirewallChangeSpec
		if json.Unmarshal(spec, &sp) == nil {
			c.spec = &sp
		}
	}
	if c.Preview == nil {
		c.Preview = []string{}
	}
	if c.Ran == nil {
		c.Ran = []tunnel.FirewallRan{}
	}
	if c.State == FWWaitingKeep && c.KeptAt == nil {
		t := c.UpdatedAt.Add(tunnel.FirewallKeepWindow)
		c.KeepBefore = &t
	}
	c.CanPutBack = c.State == FWApplied && c.SentAt != nil && c.Delta.PutBack == ""
	return c, nil
}

func scanFWChanges(rows pgx.Rows) ([]FirewallChange, error) {
	defer rows.Close()
	out := []FirewallChange{}
	for rows.Next() {
		c, err := scanFWChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// fwServerFor reads what Argus keeps on a server, and who manages it.
func (s *Store) fwServerFor(ctx context.Context, assetID string) (fwServerState, []string, *time.Time, error) {
	var own, pols []byte
	var st fwServerState
	var managers []string
	var synced *time.Time
	err := s.db(ctx).QueryRow(ctx, `
		SELECT own_lines, policies, default_out, managers, synced_at FROM firewall_servers WHERE asset_id::text = $1`,
		assetID).Scan(&own, &pols, &st.DefaultOut, &managers, &synced)
	if errors.Is(err, pgx.ErrNoRows) {
		return fwServerState{Own: []FWLine{}, Policies: map[string]int{}}, []string{}, nil, nil
	}
	if err != nil {
		return st, nil, nil, err
	}
	_ = json.Unmarshal(own, &st.Own)
	_ = json.Unmarshal(pols, &st.Policies)
	if st.Own == nil {
		st.Own = []FWLine{}
	}
	if st.Policies == nil {
		st.Policies = map[string]int{}
	}
	return st, nonNilStrings(managers), synced, nil
}

// saveFWServer keeps what a server now holds.
func saveFWServer(ctx context.Context, q querier, assetID string, st fwServerState) error {
	own, _ := json.Marshal(st.Own)
	pols, _ := json.Marshal(st.Policies)
	_, err := q.Exec(ctx, `
		INSERT INTO firewall_servers (asset_id, own_lines, policies, default_out, synced_at, updated_at)
		VALUES ($1::uuid, $2::jsonb, $3::jsonb, $4, now(), now())
		ON CONFLICT (asset_id) DO UPDATE SET own_lines = $2::jsonb, policies = $3::jsonb, default_out = $4,
		       synced_at = now(), updated_at = now()`, assetID, own, pols, st.DefaultOut)
	return err
}

// SetFWManagers sets the operators who may change a server's firewall.
func (s *Store) SetFWManagers(ctx context.Context, assetID string, emails []string) error {
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO firewall_servers (asset_id, managers) VALUES ($1::uuid, $2)
		ON CONFLICT (asset_id) DO UPDATE SET managers = $2, updated_at = now()`, assetID, emails)
	return err
}

type newFWChange struct {
	AssetID, Hostname, AssetHostname, Kind, Summary, Reason, State, RequestedBy string
	Delta                                                                       FWDelta
	Preview                                                                     []string
	RolloutID                                                                   string
	Wave                                                                        int
}

// maxOpenFWChanges bounds the changes waiting on one server.
const maxOpenFWChanges = 10

// ErrFWBusy is a server with too many changes waiting.
var ErrFWBusy = errors.New("this server has ten firewall changes waiting already; let them finish first")

func (s *Store) createFWChange(ctx context.Context, q querier, n newFWChange) (FirewallChange, error) {
	var open int
	if err := q.QueryRow(ctx, `
		SELECT count(*) FROM firewall_changes WHERE asset_id::text = $1
		   AND state IN ('awaiting_approval', 'queued', 'sent', 'trial', 'waiting_keep')`, n.AssetID).Scan(&open); err != nil {
		return FirewallChange{}, err
	}
	if open >= maxOpenFWChanges {
		return FirewallChange{}, ErrFWBusy
	}
	delta, _ := json.Marshal(n.Delta)
	preview, _ := json.Marshal(nonNilStrings(n.Preview))
	var rollout *string
	if n.RolloutID != "" {
		rollout = &n.RolloutID
	}
	return scanFWChange(q.QueryRow(ctx, `
		INSERT INTO firewall_changes (asset_id, hostname, asset_hostname, kind, summary, reason, delta, preview,
		       state, requested_by, rollout_id, wave)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, $10, $11::uuid, $12)
		RETURNING `+fwChangeCols, n.AssetID, n.Hostname, n.AssetHostname, n.Kind, clip(n.Summary, 500),
		clip(n.Reason, 500), delta, preview, n.State, n.RequestedBy, rollout, n.Wave))
}

// CreateFWChange keeps a change asked for in the console.
func (s *Store) CreateFWChange(ctx context.Context, n newFWChange) (FirewallChange, error) {
	return s.createFWChange(ctx, s.db(ctx), n)
}

// FWChangeByID reads one change.
func (s *Store) FWChangeByID(ctx context.Context, id string) (FirewallChange, error) {
	if !isUUID(id) {
		return FirewallChange{}, ErrFWChangeNotFound
	}
	c, err := scanFWChange(s.db(ctx).QueryRow(ctx, `SELECT `+fwChangeCols+` FROM firewall_changes WHERE id::text = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrFWChangeNotFound
	}
	return c, err
}

// FWChangesFor is a server's latest changes, newest first.
func (s *Store) FWChangesFor(ctx context.Context, assetID string, limit int) ([]FirewallChange, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+fwChangeCols+` FROM firewall_changes
		 WHERE asset_id::text = $1 ORDER BY requested_at DESC LIMIT $2`, assetID, limit)
	if err != nil {
		return nil, err
	}
	return scanFWChanges(rows)
}

// DecideFWChange approves or denies a change waiting for approval.
func (s *Store) DecideFWChange(ctx context.Context, id, by string, approve, own bool) (FirewallChange, error) {
	c, err := s.FWChangeByID(ctx, id)
	if err != nil {
		return c, err
	}
	if c.State != FWAwaitingApproval {
		return c, fmt.Errorf("%w: this change is %s, not waiting for approval", ErrRunState, fwStateWords(c.State))
	}
	if !own && strings.EqualFold(c.RequestedBy, by) {
		return c, errOwnRun{"a firewall change you asked for is approved by another admin"}
	}
	next := FWDenied
	if approve {
		next = FWQueued
	}
	c, err = scanFWChange(s.db(ctx).QueryRow(ctx, `
		UPDATE firewall_changes SET state = $2, decided_by = $3, decided_at = now(), updated_at = now(),
		       ended_at = CASE WHEN $2 = 'denied' THEN now() ELSE NULL END,
		       message = CASE WHEN $2 = 'denied' THEN 'denied by ' || $3 ELSE message END
		 WHERE id::text = $1 AND state = 'awaiting_approval'
		RETURNING `+fwChangeCols, id, next, by))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, fmt.Errorf("%w: someone decided this change already", ErrRunState)
	}
	return c, err
}

// CancelFWChange takes back a change not sent yet.
func (s *Store) CancelFWChange(ctx context.Context, id, by string) (FirewallChange, error) {
	c, err := scanFWChange(s.db(ctx).QueryRow(ctx, `
		UPDATE firewall_changes SET state = 'cancelled', ended_at = now(), updated_at = now(),
		       message = 'taken back by ' || $2
		 WHERE id::text = $1 AND state IN ('awaiting_approval', 'queued')
		RETURNING `+fwChangeCols, id, by))
	if errors.Is(err, pgx.ErrNoRows) {
		if c, err := s.FWChangeByID(ctx, id); err != nil {
			return c, err
		}
		return c, fmt.Errorf("%w: only a change not sent yet can be taken back", ErrRunState)
	}
	return c, err
}

// KeepFWChange records a person's Keep on a change waiting for one.
func (s *Store) KeepFWChange(ctx context.Context, id, by string) (FirewallChange, error) {
	c, err := scanFWChange(s.db(ctx).QueryRow(ctx, `
		UPDATE firewall_changes SET kept_by = $2, kept_at = now(), updated_at = updated_at
		 WHERE id::text = $1 AND state = 'waiting_keep' AND kept_at IS NULL
		RETURNING `+fwChangeCols, id, by))
	if errors.Is(err, pgx.ErrNoRows) {
		if c, err := s.FWChangeByID(ctx, id); err != nil {
			return c, err
		}
		return c, fmt.Errorf("%w: this change is not waiting for Keep", ErrRunState)
	}
	return c, err
}

// fwChangeInFlight is the change the agent on host is working on, if any.
func (s *Store) fwChangeInFlight(ctx context.Context, host string) (*FirewallChange, error) {
	c, err := scanFWChange(s.db(ctx).QueryRow(ctx, `SELECT `+fwChangeCols+` FROM firewall_changes
		 WHERE lower(hostname) = lower($1) AND state IN ('sent', 'trial', 'waiting_keep')
		 ORDER BY sent_at LIMIT 1`, host))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// fwChangeNext is the oldest change waiting to go to the agent on host. A
// rollout's change goes only once every server in an earlier wave is done.
func (s *Store) fwChangeNext(ctx context.Context, host string) (*FirewallChange, error) {
	c, err := scanFWChange(s.db(ctx).QueryRow(ctx, `SELECT `+fwChangeCols+` FROM firewall_changes c
		 WHERE lower(c.hostname) = lower($1) AND c.state = 'queued'
		   AND (c.rollout_id IS NULL OR (
		        EXISTS (SELECT 1 FROM firewall_rollouts r WHERE r.id = c.rollout_id AND r.state = 'running')
		        AND NOT EXISTS (SELECT 1 FROM firewall_changes e WHERE e.rollout_id = c.rollout_id AND e.wave < c.wave
		            AND e.state IN ('awaiting_approval', 'queued', 'sent', 'trial', 'waiting_keep'))))
		 ORDER BY c.requested_at LIMIT 1`, host))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// markFWChangeSent keeps what went to the agent.
func (s *Store) markFWChangeSent(ctx context.Context, id string, before fwServerState, spec tunnel.FirewallChangeSpec,
	preview []string) error {
	b, _ := json.Marshal(before)
	sp, _ := json.Marshal(spec)
	pv, _ := json.Marshal(nonNilStrings(preview))
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE firewall_changes SET state = 'sent', before = $2::jsonb, spec = $3::jsonb, preview = $4::jsonb,
		       sent_at = now(), updated_at = now(), message = 'sent to the server'
		 WHERE id::text = $1 AND state = 'queued'`, id, b, sp, pv)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("%w: the change was no longer waiting", ErrRunState)
	}
	return err
}

// failFWChange ends a change that could not be sent.
func (s *Store) failFWChange(ctx context.Context, id, msg string) (*FirewallChange, error) {
	c, err := scanFWChange(s.db(ctx).QueryRow(ctx, `
		UPDATE firewall_changes SET state = 'failed', message = $2, ended_at = now(), updated_at = now()
		 WHERE id::text = $1 AND state IN ('queued', 'sent')
		RETURNING `+fwChangeCols, id, clip(msg, 1000)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// recordFWRun keeps how the agent says a change is going. It returns the
// change when this report ended it. An applied change becomes what the
// server holds.
func (s *Store) recordFWRun(ctx context.Context, host string, run tunnel.FirewallRun, now time.Time) (*FirewallChange, error) {
	if !isUUID(run.ID) {
		return nil, nil
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanFWChange(tx.QueryRow(ctx, `SELECT `+fwChangeCols+` FROM firewall_changes
		 WHERE id::text = $1 AND lower(hostname) = lower($2) FOR UPDATE`, run.ID, host))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fwOpen(c.State) || c.State == FWAwaitingApproval || c.State == FWQueued {
		return nil, nil
	}
	next := c.State
	switch run.State {
	case tunnel.FirewallRunApplying:
		next = FWSent
	case tunnel.FirewallRunTrial:
		next = FWTrial
	case tunnel.FirewallRunWaitKeep:
		next = FWWaitingKeep
	case tunnel.FirewallRunApplied:
		next = FWApplied
	case tunnel.FirewallRunFailed:
		next = FWFailed
	case tunnel.FirewallRunRolledBack:
		next = FWRolledBack
	default:
		return nil, nil
	}
	ran, _ := json.Marshal(clipRan(run.Ran))
	if next == c.State && run.Message == c.Message && len(run.Ran) == len(c.Ran) {
		return nil, tx.Commit(ctx)
	}
	ended := !fwOpen(next)
	row := tx.QueryRow(ctx, `
		UPDATE firewall_changes SET state = $2, message = $3, ran = $4::jsonb, updated_at = now(),
		       ended_at = CASE WHEN $5 THEN now() ELSE NULL END
		 WHERE id = $1::uuid RETURNING `+fwChangeCols, c.ID, next, clip(run.Message, 1000), ran, ended)
	c, err = scanFWChange(row)
	if err != nil {
		return nil, err
	}
	if next == FWApplied {
		if err := commitFWChange(ctx, tx, c, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if ended {
		return &c, nil
	}
	return nil, nil
}

// commitFWChange makes an applied change what the server holds.
func commitFWChange(ctx context.Context, tx pgx.Tx, c FirewallChange, now time.Time) error {
	if c.before == nil {
		return nil
	}
	after := c.Delta.apply(*c.before, *c.SentAt)
	if c.Delta.PutBack != "" {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT before FROM firewall_changes WHERE id::text = $1`, c.Delta.PutBack).Scan(&raw)
		if err == nil && len(raw) > 0 {
			var st fwServerState
			if json.Unmarshal(raw, &st) == nil {
				after = st
			}
		}
	}
	if c.spec != nil {
		switch c.spec.DefaultOut {
		case "deny":
			after.DefaultOut = "deny"
		case "allow":
			after.DefaultOut = "allow"
		}
	}
	return saveFWServer(ctx, tx, c.AssetID, after)
}

func clipRan(in []tunnel.FirewallRan) []tunnel.FirewallRan {
	if len(in) > 200 {
		in = in[:200]
	}
	out := make([]tunnel.FirewallRan, len(in))
	for i, r := range in {
		out[i] = tunnel.FirewallRan{Command: clip(r.Command, 2000), OK: r.OK, Output: clip(r.Output, 2000)}
	}
	return out
}

// fwStaleAfter is how long a sent change may go unreported. The agent's
// own trial ends in two minutes and Keep in ten, so a change quiet past
// this undid itself or never arrived.
const fwStaleAfter = 20 * time.Minute

// expireFWChanges ends changes on host that went quiet.
func (s *Store) expireFWChanges(ctx context.Context, host string) ([]FirewallChange, error) {
	rows, err := s.db(ctx).Query(ctx, `
		UPDATE firewall_changes SET state = 'interrupted', ended_at = now(), updated_at = now(),
		       message = 'the server stopped reporting this change; if it was on trial it put its saved copy back by itself'
		 WHERE lower(hostname) = lower($1) AND state IN ('sent', 'trial', 'waiting_keep')
		   AND updated_at < now() - $2::interval
		RETURNING `+fwChangeCols, host, fmt.Sprintf("%d seconds", int(fwStaleAfter.Seconds())))
	if err != nil {
		return nil, err
	}
	return scanFWChanges(rows)
}

// recordFWBeat keeps what the agent said about changing its firewall, and
// the firewall it read back after a change.
func (s *Store) recordFWBeat(ctx context.Context, host string, b tunnel.FirewallBeat) error {
	st := b
	if st.Run != nil {
		r := *st.Run
		r.After, r.Ran = nil, nil
		st.Run = &r
	}
	raw, _ := json.Marshal(st)
	if _, err := s.db(ctx).Exec(ctx, `
		UPDATE agents SET firewall_beat = $2::jsonb
		 WHERE lower(hostname) = lower($1) AND firewall_beat IS DISTINCT FROM $2::jsonb`, host, raw); err != nil {
		return err
	}
	if b.Run != nil && b.Run.After != nil && !b.Run.After.ReadAt.IsZero() {
		fw, _ := json.Marshal(b.Run.After)
		if len(fw) > 400000 {
			return nil
		}
		if _, err := s.db(ctx).Exec(ctx, `
			UPDATE agents SET firewall = $2::jsonb
			 WHERE lower(hostname) = lower($1)
			   AND (firewall IS NULL OR (firewall->>'readAt')::timestamptz < $3)`, host, fw, b.Run.After.ReadAt); err != nil {
			return err
		}
	}
	return nil
}

// fwBeatFor is what the agent on host last said about changing its
// firewall; nil when it never said (an older agent).
func (s *Store) fwBeatFor(ctx context.Context, host string) (*tunnel.FirewallBeat, error) {
	var raw []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT firewall_beat FROM agents WHERE lower(hostname) = lower($1)
		 ORDER BY last_seen_at DESC LIMIT 1`, host).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b tunnel.FirewallBeat
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, nil
	}
	return &b, nil
}

// fwResolverNow reads every server's addresses and tags, and which
// servers hold each policy.
func (s *Store) fwResolverNow(ctx context.Context) (fwResolver, error) {
	r := fwResolver{addrs: map[string][]string{}, tags: map[string][]string{}, policies: map[string][]string{}}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, a.address, a.tags, coalesce(ag.addresses, '{}')
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT addresses FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var id, address string
		var tags, addrs []string
		if err := rows.Scan(&id, &address, &tags, &addrs); err != nil {
			rows.Close()
			return r, err
		}
		for _, raw := range append([]string{address}, addrs...) {
			if a, ok := usableAddr(raw); ok && !slices.Contains(r.addrs[id], a) {
				r.addrs[id] = append(r.addrs[id], a)
			}
		}
		for _, t := range tags {
			k := strings.ToLower(t)
			r.tags[k] = append(r.tags[k], id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return r, err
	}
	prow, err := s.db(ctx).Query(ctx, `SELECT asset_id::text, key FROM firewall_servers, jsonb_object_keys(policies) AS key`)
	if err != nil {
		return r, err
	}
	defer prow.Close()
	for prow.Next() {
		var asset, pol string
		if err := prow.Scan(&asset, &pol); err != nil {
			return r, err
		}
		r.policies[pol] = append(r.policies[pol], asset)
	}
	return r, prow.Err()
}

// fwStateWords is a state as the console says it.
func fwStateWords(state string) string {
	switch state {
	case FWAwaitingApproval:
		return "waiting for approval"
	case FWQueued:
		return "waiting to be sent"
	case FWSent:
		return "being made"
	case FWTrial:
		return "on trial"
	case FWWaitingKeep:
		return "waiting for Keep"
	case FWRolledBack:
		return "undone"
	}
	return strings.ReplaceAll(state, "_", " ")
}

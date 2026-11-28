package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gsoultan/caduceus"
)

/*
Firewall policies (plans/server-firewall/design.md, part 4), like AWS
security groups: a named set of lines, going in and going out, put on
servers by name or on every server with a tag. Every save is a new version,
kept; the servers move to it in a rollout, one server first and then five
at a time, stopping at the first that fails.

A line in any policy, or on any server, can be for "servers with policy
X": the servers holding X, with their addresses as they are when the change
goes out.
*/

// FirewallPolicy is one policy.
type FirewallPolicy struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Version     int        `json:"version"`
	Lines       []FWLine   `json:"lines"`
	DefaultOut  string     `json:"defaultOut"`
	Starter     bool       `json:"starter"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedBy   string     `json:"updatedBy"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	Targets     []FWTarget `json:"targets"`
	// Servers counts the servers holding the policy now, at any version;
	// Current those at its latest.
	Servers int `json:"servers"`
	Current int `json:"current"`
	// Rollout is its latest rollout.
	Rollout *FWRollout `json:"rollout,omitempty"`
}

// FWTarget is where a policy is: one server, or every server with a tag.
type FWTarget struct {
	ID       string `json:"id"`
	AssetID  string `json:"assetId,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Tag      string `json:"tag,omitempty"`
}

// FWRollout is a policy going out to its servers.
type FWRollout struct {
	ID          string     `json:"id"`
	PolicyID    *string    `json:"policyId"`
	PolicyName  string     `json:"policyName"`
	Version     int        `json:"version"`
	State       string     `json:"state"`
	Summary     string     `json:"summary"`
	RequestedBy string     `json:"requestedBy"`
	RequestedAt time.Time  `json:"requestedAt"`
	DecidedBy   *string    `json:"decidedBy,omitempty"`
	DecidedAt   *time.Time `json:"decidedAt,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	Message     string     `json:"message"`
	// Servers is each server's change in the rollout.
	Servers []FirewallChange `json:"servers,omitempty"`
	Total   int              `json:"total"`
	Done    int              `json:"done"`
	Failed  int              `json:"failed"`
}

// Rollout states.
const (
	FWRolloutWaiting   = "awaiting_approval"
	FWRolloutRunning   = "running"
	FWRolloutDone      = "done"
	FWRolloutStopped   = "stopped"
	FWRolloutDenied    = "denied"
	FWRolloutCancelled = "cancelled"
)

// ErrFWPolicyNotFound is a policy id nobody knows.
var ErrFWPolicyNotFound = errors.New("no such firewall policy")

// ErrFWPolicyName is a name another policy has.
var ErrFWPolicyName = errors.New("another firewall policy has that name")

const fwPolicyCols = `id::text, name, description, version, lines, default_out, starter, created_by, created_at,
	updated_by, updated_at`

func scanFWPolicy(row rowScanner) (FirewallPolicy, error) {
	var p FirewallPolicy
	var lines []byte
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Version, &lines, &p.DefaultOut, &p.Starter, &p.CreatedBy,
		&p.CreatedAt, &p.UpdatedBy, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	_ = json.Unmarshal(lines, &p.Lines)
	if p.Lines == nil {
		p.Lines = []FWLine{}
	}
	p.Targets = []FWTarget{}
	return p, nil
}

// FWPolicies lists every policy, with where it is and its latest rollout.
func (s *Store) FWPolicies(ctx context.Context) ([]FirewallPolicy, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+fwPolicyCols+` FROM firewall_policies ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	var out []FirewallPolicy
	for rows.Next() {
		p, err := scanFWPolicy(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.fillFWPolicy(ctx, &out[i], false); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []FirewallPolicy{}
	}
	return out, nil
}

// FWPolicyByID reads one policy, with its latest rollout's servers.
func (s *Store) FWPolicyByID(ctx context.Context, id string) (FirewallPolicy, error) {
	if !isUUID(id) {
		return FirewallPolicy{}, ErrFWPolicyNotFound
	}
	p, err := scanFWPolicy(s.db(ctx).QueryRow(ctx, `SELECT `+fwPolicyCols+` FROM firewall_policies WHERE id::text = $1`, id))
	if errors.Is(err, caduceus.ErrNoRows) {
		return p, ErrFWPolicyNotFound
	}
	if err != nil {
		return p, err
	}
	return p, s.fillFWPolicy(ctx, &p, true)
}

func (s *Store) fillFWPolicy(ctx context.Context, p *FirewallPolicy, servers bool) error {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT t.id::text, coalesce(t.asset_id::text, ''), coalesce(a.hostname, ''), coalesce(t.tag, '')
		  FROM firewall_policy_targets t LEFT JOIN assets a ON a.id = t.asset_id
		 WHERE t.policy_id::text = $1 ORDER BY t.tag NULLS FIRST, a.hostname`, p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t FWTarget
		if err := rows.Scan(&t.ID, &t.AssetID, &t.Hostname, &t.Tag); err != nil {
			rows.Close()
			return err
		}
		p.Targets = append(p.Targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := s.db(ctx).QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE (policies->>$1)::int = $2)
		  FROM firewall_servers WHERE policies ? $1`, p.ID, p.Version).Scan(&p.Servers, &p.Current); err != nil {
		return err
	}
	r, err := s.latestFWRollout(ctx, p.ID, servers)
	if err != nil {
		return err
	}
	p.Rollout = r
	return nil
}

const fwRolloutCols = `id::text, policy_id::text, policy_name, version, state, summary, requested_by, requested_at,
	decided_by, decided_at, ended_at, message`

func scanFWRollout(row rowScanner) (FWRollout, error) {
	var r FWRollout
	err := row.Scan(&r.ID, &r.PolicyID, &r.PolicyName, &r.Version, &r.State, &r.Summary, &r.RequestedBy,
		&r.RequestedAt, &r.DecidedBy, &r.DecidedAt, &r.EndedAt, &r.Message)
	return r, err
}

func (s *Store) latestFWRollout(ctx context.Context, policyID string, servers bool) (*FWRollout, error) {
	r, err := scanFWRollout(s.db(ctx).QueryRow(ctx, `SELECT `+fwRolloutCols+` FROM firewall_rollouts
		 WHERE policy_id::text = $1 ORDER BY requested_at DESC LIMIT 1`, policyID))
	if errors.Is(err, caduceus.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, s.fillFWRollout(ctx, &r, servers)
}

// FWRolloutByID reads one rollout with each server's change.
func (s *Store) FWRolloutByID(ctx context.Context, id string) (FWRollout, error) {
	if !isUUID(id) {
		return FWRollout{}, ErrFWChangeNotFound
	}
	r, err := scanFWRollout(s.db(ctx).QueryRow(ctx, `SELECT `+fwRolloutCols+` FROM firewall_rollouts WHERE id::text = $1`, id))
	if errors.Is(err, caduceus.ErrNoRows) {
		return r, ErrFWChangeNotFound
	}
	if err != nil {
		return r, err
	}
	return r, s.fillFWRollout(ctx, &r, true)
}

func (s *Store) fillFWRollout(ctx context.Context, r *FWRollout, servers bool) error {
	if err := s.db(ctx).QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE state = 'applied'),
		       count(*) FILTER (WHERE state IN ('failed', 'rolled_back', 'interrupted'))
		  FROM firewall_changes WHERE rollout_id::text = $1`, r.ID).Scan(&r.Total, &r.Done, &r.Failed); err != nil {
		return err
	}
	if !servers {
		return nil
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+fwChangeCols+` FROM firewall_changes
		 WHERE rollout_id::text = $1 ORDER BY wave, asset_hostname`, r.ID)
	if err != nil {
		return err
	}
	r.Servers, err = scanFWChanges(rows)
	return err
}

// fwPolicyVersion is a policy's lines at a version.
func (s *Store) fwPolicyVersion(ctx context.Context, q querier, id string, version int) ([]FWLine, string, error) {
	var raw []byte
	var out string
	err := q.QueryRow(ctx, `SELECT lines, default_out FROM firewall_policy_versions
		 WHERE policy_id::text = $1 AND version = $2`, id, version).Scan(&raw, &out)
	if err != nil {
		return nil, "", err
	}
	var lines []FWLine
	_ = json.Unmarshal(raw, &lines)
	return lines, out, nil
}

// fwPolicyNames are policies' names, by id.
func (s *Store) fwPolicyNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id::text, name FROM firewall_policies`)
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

type fwPolicyInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Lines       []FWLine `json:"lines"`
	DefaultOut  string   `json:"defaultOut"`
	Note        string   `json:"note"`
	Starter     bool     `json:"starter"`
}

// validate checks a policy as written, giving new lines their keys and
// keeping the keys of lines that were already there.
func (in *fwPolicyInput) validate(old []FWLine, by string, now time.Time) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if in.Name == "" || len(in.Name) > 80 {
		return errors.New("give the policy a name of up to 80 characters")
	}
	if len(in.Description) > 500 {
		return errors.New("the description is at most 500 characters")
	}
	if in.DefaultOut == "" {
		in.DefaultOut = "allow"
	}
	if in.DefaultOut != "allow" && in.DefaultOut != "deny" {
		return errors.New("going out is allow or deny")
	}
	if len(in.Lines) > 60 {
		return errors.New("at most 60 lines in a policy")
	}
	keys := map[string]bool{}
	for _, l := range old {
		keys[l.Key] = true
	}
	seen := map[string]bool{}
	for i := range in.Lines {
		l := &in.Lines[i]
		if err := l.validate(); err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
		if l.Until != nil {
			return fmt.Errorf("line %d: a policy's line does not end by itself", i+1)
		}
		if !keys[l.Key] || seen[l.Key] {
			l.Key = randomKey(6)
			l.AddedBy = by
			t := now
			l.AddedAt = &t
		}
		seen[l.Key] = true
	}
	return nil
}

// CreateFWPolicy keeps a new policy, at version 1.
func (s *Store) CreateFWPolicy(ctx context.Context, in fwPolicyInput, by string) (FirewallPolicy, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return FirewallPolicy{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lines, _ := json.Marshal(in.Lines)
	p, err := scanFWPolicy(tx.QueryRow(ctx, `
		INSERT INTO firewall_policies (name, description, lines, default_out, starter, created_by, updated_by)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $6) RETURNING `+fwPolicyCols,
		in.Name, in.Description, lines, in.DefaultOut, in.Starter, by))
	if err != nil && isUniqueViolation(err) {
		return p, ErrFWPolicyName
	}
	if err != nil {
		return p, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO firewall_policy_versions (policy_id, version, lines, default_out, note, created_by)
		VALUES ($1::uuid, 1, $2::jsonb, $3, $4, $5)`, p.ID, lines, in.DefaultOut, clip(in.Note, 500), by); err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}

// UpdateFWPolicy keeps a new version of a policy. It reports whether the
// lines or going out changed, which is what needs a rollout.
func (s *Store) UpdateFWPolicy(ctx context.Context, id string, in fwPolicyInput, by string) (FirewallPolicy, bool, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return FirewallPolicy{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	old, err := scanFWPolicy(tx.QueryRow(ctx, `SELECT `+fwPolicyCols+` FROM firewall_policies WHERE id::text = $1 FOR UPDATE`, id))
	if errors.Is(err, caduceus.ErrNoRows) {
		return old, false, ErrFWPolicyNotFound
	}
	if err != nil {
		return old, false, err
	}
	a, _ := json.Marshal(old.Lines)
	lines, _ := json.Marshal(in.Lines)
	changed := string(a) != string(lines) || old.DefaultOut != in.DefaultOut
	version := old.Version
	if changed {
		version++
	}
	p, err := scanFWPolicy(tx.QueryRow(ctx, `
		UPDATE firewall_policies SET name = $2, description = $3, lines = $4::jsonb, default_out = $5, version = $6,
		       updated_by = $7, updated_at = now()
		 WHERE id::text = $1 RETURNING `+fwPolicyCols, id, in.Name, in.Description, lines, in.DefaultOut, version, by))
	if err != nil && isUniqueViolation(err) {
		return p, false, ErrFWPolicyName
	}
	if err != nil {
		return p, false, err
	}
	if changed {
		if _, err := tx.Exec(ctx, `
			INSERT INTO firewall_policy_versions (policy_id, version, lines, default_out, note, created_by)
			VALUES ($1::uuid, $2, $3::jsonb, $4, $5, $6)`, p.ID, version, lines, in.DefaultOut, clip(in.Note, 500), by); err != nil {
			return p, false, err
		}
	}
	return p, changed, tx.Commit(ctx)
}

// ErrFWPolicyInUse is deleting a policy servers still hold.
var ErrFWPolicyInUse = errors.New("take the policy off its servers and tags first")

// DeleteFWPolicy removes a policy no server holds.
func (s *Store) DeleteFWPolicy(ctx context.Context, id string) (FirewallPolicy, error) {
	p, err := s.FWPolicyByID(ctx, id)
	if err != nil {
		return p, err
	}
	if len(p.Targets) > 0 || p.Servers > 0 {
		return p, ErrFWPolicyInUse
	}
	var refs int
	if err := s.db(ctx).QueryRow(ctx, `
		SELECT count(*) FROM firewall_policies q, jsonb_array_elements(q.lines) l, jsonb_array_elements(l->'from') f
		 WHERE f->>'kind' = 'policy' AND f->>'value' = $1`, id).Scan(&refs); err != nil {
		return p, err
	}
	if refs > 0 {
		return p, errors.New("another policy has a line for servers with this policy; change that line first")
	}
	_, err = s.db(ctx).Exec(ctx, `DELETE FROM firewall_policies WHERE id::text = $1`, id)
	return p, err
}

// AddFWTarget puts a policy on a server or a tag.
func (s *Store) AddFWTarget(ctx context.Context, policyID, assetID, tag, by string) error {
	var asset, tg *string
	if assetID != "" {
		asset = &assetID
	} else {
		tg = &tag
	}
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO firewall_policy_targets (policy_id, asset_id, tag, created_by)
		VALUES ($1::uuid, $2::uuid, $3, $4)`, policyID, asset, tg, by)
	if err != nil && isUniqueViolation(err) {
		return errors.New("the policy is there already")
	}
	return err
}

// RemoveFWTarget takes a policy off a server or a tag.
func (s *Store) RemoveFWTarget(ctx context.Context, policyID, targetID string) error {
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM firewall_policy_targets WHERE policy_id::text = $1 AND id::text = $2`,
		policyID, targetID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrFWPolicyNotFound
	}
	return err
}

// fwPolicyWanted is, for one policy, every server that should hold it
// (true) and every server holding it that no longer should (false), by
// asset id, with each server's name and tags.
type fwPolicyServer struct {
	AssetID, Hostname string
	Tags              []string
	Want, Has         bool
	HasVersion        int
}

func (s *Store) fwPolicyServers(ctx context.Context, policyID string) ([]fwPolicyServer, error) {
	rows, err := s.db(ctx).Query(ctx, `
		WITH wanted AS (
		    SELECT a.id FROM assets a JOIN firewall_policy_targets t ON t.asset_id = a.id
		     WHERE t.policy_id::text = $1
		    UNION
		    SELECT a.id FROM assets a JOIN firewall_policy_targets t ON t.tag IS NOT NULL
		       AND EXISTS (SELECT 1 FROM unnest(a.tags) x WHERE lower(x) = lower(t.tag))
		     WHERE t.policy_id::text = $1
		), holding AS (
		    SELECT asset_id AS id, (policies->>$1)::int AS v FROM firewall_servers WHERE policies ? $1
		)
		SELECT a.id::text, a.hostname, a.tags, w.id IS NOT NULL, h.id IS NOT NULL, coalesce(h.v, 0)
		  FROM assets a
		  LEFT JOIN wanted w ON w.id = a.id
		  LEFT JOIN holding h ON h.id = a.id
		 WHERE (w.id IS NOT NULL OR h.id IS NOT NULL)
		   AND a.archived_at IS NULL AND a.kind IN ('server', 'workstation')
		 ORDER BY a.hostname`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fwPolicyServer
	for rows.Next() {
		var p fwPolicyServer
		if err := rows.Scan(&p.AssetID, &p.Hostname, &p.Tags, &p.Want, &p.Has, &p.HasVersion); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// fwPoliciesOn are the policies a server should hold, by id, at their
// latest version.
func (s *Store) fwPoliciesOn(ctx context.Context, assetID string) (map[string]int, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT DISTINCT p.id::text, p.version FROM firewall_policies p
		  JOIN firewall_policy_targets t ON t.policy_id = p.id
		  JOIN assets a ON a.id::text = $1
		 WHERE t.asset_id = a.id
		    OR (t.tag IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(a.tags) x WHERE lower(x) = lower(t.tag)))`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var v int
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// createFWRollout keeps a rollout and its servers' changes.
func (s *Store) createFWRollout(ctx context.Context, p FirewallPolicy, state, summary, by string,
	changes []newFWChange) (FWRollout, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return FWRollout{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A newer rollout of the same policy replaces one not finished.
	if _, err := tx.Exec(ctx, `
		UPDATE firewall_changes SET state = 'stopped', ended_at = now(), updated_at = now(),
		       message = 'a newer rollout of the policy replaced this one'
		 WHERE state IN ('awaiting_approval', 'queued')
		   AND rollout_id IN (SELECT id FROM firewall_rollouts WHERE policy_id::text = $1
		                       AND state IN ('awaiting_approval', 'running'))`, p.ID); err != nil {
		return FWRollout{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE firewall_rollouts SET state = 'stopped', ended_at = now(), message = 'a newer rollout replaced it'
		 WHERE policy_id::text = $1 AND state IN ('awaiting_approval', 'running')`, p.ID); err != nil {
		return FWRollout{}, err
	}
	r, err := scanFWRollout(tx.QueryRow(ctx, `
		INSERT INTO firewall_rollouts (policy_id, policy_name, version, state, summary, requested_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6) RETURNING `+fwRolloutCols, p.ID, p.Name, p.Version, state,
		clip(summary, 500), by))
	if err != nil {
		return r, err
	}
	for _, c := range changes {
		c.RolloutID = r.ID
		if _, err := s.createFWChange(ctx, tx, c); err != nil {
			return r, fmt.Errorf("%s: %w", c.AssetHostname, err)
		}
	}
	if len(changes) == 0 {
		if _, err := tx.Exec(ctx, `UPDATE firewall_rollouts SET state = 'done', ended_at = now(),
			message = 'every server already holds this version' WHERE id = $1::uuid AND state = 'running'`, r.ID); err != nil {
			return r, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return r, err
	}
	return s.FWRolloutByID(ctx, r.ID)
}

// DecideFWRollout approves or denies a rollout waiting for approval.
func (s *Store) DecideFWRollout(ctx context.Context, id, by string, approve, own bool) (FWRollout, error) {
	r, err := s.FWRolloutByID(ctx, id)
	if err != nil {
		return r, err
	}
	if r.State != FWRolloutWaiting {
		return r, fmt.Errorf("%w: this rollout is %s, not waiting for approval", ErrRunState, fwStateWords(r.State))
	}
	if !own && strings.EqualFold(r.RequestedBy, by) {
		return r, errOwnRun{"a rollout you asked for is approved by another admin"}
	}
	next := FWRolloutDenied
	if approve {
		next = FWRolloutRunning
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return r, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE firewall_rollouts SET state = $2, decided_by = $3, decided_at = now(),
		       ended_at = CASE WHEN $2 = 'denied' THEN now() ELSE NULL END
		 WHERE id::text = $1 AND state = 'awaiting_approval'`, id, next, by)
	if err != nil {
		return r, err
	}
	if tag.RowsAffected() == 0 {
		return r, fmt.Errorf("%w: someone decided this rollout already", ErrRunState)
	}
	if !approve {
		if _, err := tx.Exec(ctx, `
			UPDATE firewall_changes SET state = 'denied', ended_at = now(), updated_at = now(),
			       decided_by = $2, decided_at = now(), message = 'the rollout was denied by ' || $2
			 WHERE rollout_id::text = $1 AND state IN ('awaiting_approval', 'queued')`, id, by); err != nil {
			return r, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return r, err
	}
	return s.FWRolloutByID(ctx, id)
}

// StopFWRollout stops a rollout: servers not reached yet keep what they
// hold.
func (s *Store) StopFWRollout(ctx context.Context, id, msg string) (FWRollout, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return FWRollout{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE firewall_rollouts SET state = 'stopped', ended_at = now(), message = $2
		 WHERE id::text = $1 AND state IN ('awaiting_approval', 'running')`, id, clip(msg, 500))
	if err != nil {
		return FWRollout{}, err
	}
	if tag.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE firewall_changes SET state = 'stopped', ended_at = now(), updated_at = now(), message = $2
			 WHERE rollout_id::text = $1 AND state IN ('awaiting_approval', 'queued')`, id, clip(msg, 500)); err != nil {
			return FWRollout{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return FWRollout{}, err
	}
	return s.FWRolloutByID(ctx, id)
}

// finishFWRollout marks a rollout done once none of its changes is open.
func (s *Store) finishFWRollout(ctx context.Context, id string) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `
		UPDATE firewall_rollouts SET state = 'done', ended_at = now(), message = 'every server holds this version'
		 WHERE id::text = $1 AND state = 'running'
		   AND NOT EXISTS (SELECT 1 FROM firewall_changes WHERE rollout_id = firewall_rollouts.id
		                    AND state IN ('awaiting_approval', 'queued', 'sent', 'trial', 'waiting_keep'))`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// fwStarters are the policies a new deployment can start from.
func fwStarters() []fwPolicyInput {
	ssh := FWLine{Key: "ssh", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22",
		From: []FWSource{{Kind: FWFromPrivate}}, Note: "SSH from private networks"}
	ping := FWLine{Key: "ping", Direction: "in", Action: "allow", Protocol: "icmp",
		From: []FWSource{{Kind: FWFromPrivate}}, Note: "Ping from private networks"}
	rdp := FWLine{Key: "rdp", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "3389",
		From: []FWSource{{Kind: FWFromPrivate}}, Note: "Remote Desktop from private networks"}
	return []fwPolicyInput{
		{Name: "Linux base", Description: "SSH and ping from private networks. Everything going out is allowed.",
			Lines: []FWLine{ssh}, DefaultOut: "allow", Starter: true},
		{Name: "Windows base", Description: "Remote Desktop and ping from private networks. Everything going out is allowed.",
			Lines: []FWLine{rdp, ping}, DefaultOut: "allow", Starter: true},
		{Name: "Locked down", Description: "SSH from private networks only, and nothing going out but what Argus always allows: " +
			"its own connection, name lookups and system updates.",
			Lines: []FWLine{ssh}, DefaultOut: "deny", Starter: true},
	}
}

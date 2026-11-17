package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Scale groups, as stored: the group (where traffic comes in, what a new
server is launched from, its limits, rules and mode), its members from
launch until terminated, and what it did. See cloud/scale.go for the rules
and scaler.go for the loop that follows them.
*/

// scaleTemplate is what a new member is launched from.
type scaleTemplate struct {
	Image      string `json:"image"`
	ImageLabel string `json:"imageLabel"`
	Size       string `json:"size"`
	DiskGB     int32  `json:"diskGb"`
	// Subnets are where members go, spread evenly; one per zone is usual.
	Subnets   []string          `json:"subnets"`
	Firewalls []string          `json:"firewalls"`
	Tags      map[string]string `json:"tags,omitempty"`
	// From is the server the group was made from.
	From     string `json:"from"`
	FromName string `json:"fromName"`
}

// ScaleGroup is one group.
type ScaleGroup struct {
	ID          string            `json:"id"`
	AccountID   string            `json:"accountId"`
	AccountName string            `json:"accountName"`
	Region      string            `json:"region"`
	Name        string            `json:"name"`
	Environment string            `json:"environment"`
	TargetGroup string            `json:"targetGroup"`
	TargetName  string            `json:"targetName"`
	Port        int32             `json:"port"`
	Balancer    string            `json:"balancer"`
	Template    scaleTemplate     `json:"template"`
	Limits      cloud.ScaleLimits `json:"limits"`
	Rules       cloud.ScaleRules  `json:"rules"`
	Mode        string            `json:"mode"`
	// AutoBy is who turned automatic on; every automatic action names them.
	AutoBy       string     `json:"autoBy"`
	AutoAt       *time.Time `json:"autoAt"`
	State        string     `json:"state"`
	PausedReason string     `json:"pausedReason"`
	// Note is what the rules said the last time Argus looked.
	Note         string        `json:"note"`
	EnrollBase   string        `json:"-"`
	FailedStarts int           `json:"failedStarts"`
	LastAddAt    *time.Time    `json:"lastAddAt"`
	LastRemoveAt *time.Time    `json:"lastRemoveAt"`
	CheckedAt    *time.Time    `json:"checkedAt"`
	CreatedBy    string        `json:"createdBy"`
	CreatedAt    time.Time     `json:"createdAt"`
	Members      []ScaleMember `json:"members"`
	// AWSTrack is what AWS keeps the group near when it decides.
	AWSTrack cloud.AWSTrack `json:"awsTrack"`
	// AWSGroup and AWSTemplate are the Auto Scaling group and launch
	// template under the group, once made; AWSNote says why AWS refused
	// them, and AWSTriedAt when Argus last tried.
	AWSGroup    string     `json:"awsGroup"`
	AWSTemplate string     `json:"awsTemplate"`
	AWSNote     string     `json:"awsNote"`
	AWSTriedAt  *time.Time `json:"awsTriedAt"`
}

// ScaleMember is one server in a group.
type ScaleMember struct {
	InstanceID   string     `json:"instanceId"`
	Hostname     string     `json:"hostname"`
	Zone         string     `json:"zone"`
	Subnet       string     `json:"subnet"`
	State        string     `json:"state"`
	AddedBy      string     `json:"addedBy"`
	JoinedAt     time.Time  `json:"joinedAt"`
	RegisteredAt *time.Time `json:"registeredAt"`
	DrainUntil   *time.Time `json:"drainUntil"`
	LeftAt       *time.Time `json:"leftAt"`
	// InAWS is a server AWS Auto Scaling started, replaces and ends.
	InAWS bool `json:"inAws"`
}

// Active is whether the member is a server the group has now: starting,
// serving, or being drained.
func (m ScaleMember) Active() bool {
	return m.State == "joining" || m.State == "member" || m.State == "draining"
}

// ScaleActivity is one thing a group did, or was asked.
type ScaleActivity struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Actor    string    `json:"actor"`
	Detail   string    `json:"detail"`
	ChangeID string    `json:"changeId"`
}

// ErrScaleGroupNotFound is an unknown group.
var ErrScaleGroupNotFound = errors.New("no such scale group")

// ErrScaleGroupTaken is a name already used in the account and region.
var ErrScaleGroupTaken = errors.New("there is already a scale group by that name in this account and region")

const scaleGroupCols = `g.id::text, g.account_id::text, a.name, g.region, g.name, g.environment, g.target_group, g.target_name,
	g.port, g.balancer, g.template, g.min_size, g.max_size, g.cost_cap, g.add_wait, g.remove_wait, g.rules, g.mode,
	g.auto_by, g.auto_at, g.state, g.paused_reason, g.note, g.enroll_base, g.failed_starts, g.last_add_at,
	g.last_remove_at, g.checked_at, g.created_by, g.created_at, g.aws_track, g.aws_group, g.aws_template, g.aws_note,
	g.aws_tried_at`

func scanScaleGroup(row pgx.Row) (ScaleGroup, error) {
	var g ScaleGroup
	var tpl, rules, track []byte
	var lo, hi, addW, remW int32
	err := row.Scan(&g.ID, &g.AccountID, &g.AccountName, &g.Region, &g.Name, &g.Environment, &g.TargetGroup, &g.TargetName,
		&g.Port, &g.Balancer, &tpl, &lo, &hi, &g.Limits.CostCap, &addW, &remW, &rules, &g.Mode,
		&g.AutoBy, &g.AutoAt, &g.State, &g.PausedReason, &g.Note, &g.EnrollBase, &g.FailedStarts, &g.LastAddAt,
		&g.LastRemoveAt, &g.CheckedAt, &g.CreatedBy, &g.CreatedAt, &track, &g.AWSGroup, &g.AWSTemplate, &g.AWSNote,
		&g.AWSTriedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrScaleGroupNotFound
	}
	if err != nil {
		return g, err
	}
	g.Limits.Min, g.Limits.Max, g.Limits.AddWait, g.Limits.RemoveWait = int(lo), int(hi), int(addW), int(remW)
	_ = json.Unmarshal(tpl, &g.Template)
	_ = json.Unmarshal(rules, &g.Rules)
	_ = json.Unmarshal(track, &g.AWSTrack)
	g.Members = []ScaleMember{}
	return g, nil
}

// CreateScaleGroup records a new group.
func (s *Store) CreateScaleGroup(ctx context.Context, g ScaleGroup) (ScaleGroup, error) {
	tpl, _ := json.Marshal(g.Template)
	rules, _ := json.Marshal(g.Rules)
	track, _ := json.Marshal(g.AWSTrack)
	var id string
	err := s.db(ctx).QueryRow(ctx, `
		INSERT INTO scale_groups (account_id, region, name, environment, target_group, target_name, port, balancer,
			template, min_size, max_size, cost_cap, add_wait, remove_wait, rules, mode, auto_by, auto_at, enroll_base, created_by,
			aws_track)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
		RETURNING id::text`,
		g.AccountID, g.Region, g.Name, g.Environment, g.TargetGroup, g.TargetName, g.Port, g.Balancer, tpl,
		g.Limits.Min, g.Limits.Max, g.Limits.CostCap, g.Limits.AddWait, g.Limits.RemoveWait, rules, g.Mode,
		g.AutoBy, g.AutoAt, g.EnrollBase, g.CreatedBy, track).Scan(&id)
	if err != nil && isUniqueViolation(err) {
		return ScaleGroup{}, ErrScaleGroupTaken
	}
	if err != nil {
		return ScaleGroup{}, fmt.Errorf("record scale group: %w", err)
	}
	return s.ScaleGroup(ctx, id)
}

// ScaleGroup reads one group with its members.
func (s *Store) ScaleGroup(ctx context.Context, id string) (ScaleGroup, error) {
	g, err := scanScaleGroup(s.db(ctx).QueryRow(ctx, `SELECT `+scaleGroupCols+`
		FROM scale_groups g JOIN cloud_accounts a ON a.id = g.account_id WHERE g.id::text = $1`, id))
	if err != nil {
		return g, err
	}
	ms, err := s.scaleMembers(ctx, []string{id})
	g.Members = append(g.Members, ms[id]...)
	return g, err
}

// ScaleGroups lists every group of connected accounts, with members.
func (s *Store) ScaleGroups(ctx context.Context) ([]ScaleGroup, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+scaleGroupCols+`
		FROM scale_groups g JOIN cloud_accounts a ON a.id = g.account_id
		WHERE a.archived_at IS NULL ORDER BY lower(a.name), g.region, g.name`)
	if err != nil {
		return nil, err
	}
	out := []ScaleGroup{}
	for rows.Next() {
		g, err := scanScaleGroup(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, len(out))
	for i, g := range out {
		ids[i] = g.ID
	}
	ms, err := s.scaleMembers(ctx, ids)
	for i := range out {
		out[i].Members = append(out[i].Members, ms[out[i].ID]...)
	}
	return out, err
}

func (s *Store) scaleMembers(ctx context.Context, ids []string) (map[string][]ScaleMember, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT group_id::text, instance_id, hostname, zone, subnet, state, added_by, joined_at, registered_at, drain_until, left_at,
		       in_aws
		FROM scale_group_members WHERE group_id::text = ANY($1)
		ORDER BY joined_at, hostname`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]ScaleMember{}
	for rows.Next() {
		var gid string
		var m ScaleMember
		if err := rows.Scan(&gid, &m.InstanceID, &m.Hostname, &m.Zone, &m.Subnet, &m.State, &m.AddedBy, &m.JoinedAt,
			&m.RegisteredAt, &m.DrainUntil, &m.LeftAt, &m.InAWS); err != nil {
			return nil, err
		}
		out[gid] = append(out[gid], m)
	}
	return out, rows.Err()
}

// AddScaleMember records a server joining a group.
func (s *Store) AddScaleMember(ctx context.Context, groupID string, m ScaleMember) error {
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO scale_group_members (group_id, instance_id, hostname, zone, subnet, state, added_by, registered_at, in_aws)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (group_id, instance_id) DO UPDATE SET state = EXCLUDED.state, left_at = NULL`,
		groupID, m.InstanceID, m.Hostname, m.Zone, m.Subnet, m.State, m.AddedBy, m.RegisteredAt, m.InAWS)
	return err
}

// RegisterScaleMember records a member put in the target group at at.
func (s *Store) RegisterScaleMember(ctx context.Context, groupID, instance string, at time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE scale_group_members SET registered_at = $3
		WHERE group_id::text = $1 AND instance_id = $2`, groupID, instance, at)
	return err
}

// SetScaleMember changes a member's state, with the time that goes with
// it: when draining ends, or when it left.
func (s *Store) SetScaleMember(ctx context.Context, groupID, instance, state string, at time.Time) error {
	q := `UPDATE scale_group_members SET state = $3 WHERE group_id::text = $1 AND instance_id = $2 AND $4::timestamptz IS NOT NULL`
	switch state {
	case "draining":
		q = `UPDATE scale_group_members SET state = $3, drain_until = $4 WHERE group_id::text = $1 AND instance_id = $2`
	case "gone", "failed":
		q = `UPDATE scale_group_members SET state = $3, left_at = $4 WHERE group_id::text = $1 AND instance_id = $2`
	}
	_, err := s.db(ctx).Exec(ctx, q, groupID, instance, state, at)
	return err
}

// SetScaleGroupSettings changes a group's limits, rules and mode.
func (s *Store) SetScaleGroupSettings(ctx context.Context, g ScaleGroup) error {
	rules, _ := json.Marshal(g.Rules)
	track, _ := json.Marshal(g.AWSTrack)
	_, err := s.db(ctx).Exec(ctx, `UPDATE scale_groups SET min_size = $2, max_size = $3, cost_cap = $4, add_wait = $5,
		remove_wait = $6, rules = $7, mode = $8, auto_by = $9, auto_at = $10, state = $11, paused_reason = $12, aws_track = $13
		WHERE id::text = $1`, g.ID, g.Limits.Min, g.Limits.Max, g.Limits.CostCap, g.Limits.AddWait, g.Limits.RemoveWait,
		rules, g.Mode, g.AutoBy, g.AutoAt, g.State, g.PausedReason, track)
	return err
}

// SetScaleFloor records the Auto Scaling group and launch template made
// under a group, or why AWS refused them, as of at.
func (s *Store) SetScaleFloor(ctx context.Context, id, group, template, note string, at time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE scale_groups SET aws_group = $2, aws_template = $3, aws_note = $4, aws_tried_at = $5
		WHERE id::text = $1`, id, group, template, note, at)
	return err
}

// CreateScaleKey mints a group's enrollment key: one token for every
// server AWS starts in it, never used up, and each use checked against AWS
// (scale_floor.go). A new key replaces the group's last one.
func (s *Store) CreateScaleKey(ctx context.Context, groupID, createdBy string) (string, error) {
	secret, err := newSecret(enrollTokenPrefix + secretPrefixFor(tenantIDOf(ctx)))
	if err != nil {
		return "", err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE agent_enrollment_tokens SET expires_at = now()
		WHERE scale_group_id::text = $1 AND expires_at > now()`, groupID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_enrollment_tokens (token_hash, hostname, created_by, expires_at, scale_group_id)
		VALUES ($1, '', $2, now() + interval '10 years', $3::uuid)`, hashSecret(secret), createdBy, groupID); err != nil {
		return "", fmt.Errorf("create the group's key: %w", err)
	}
	return secret, tx.Commit(ctx)
}

// ScaleKeyGroup is the scale group a live key belongs to, or "" for any
// other token.
func (s *Store) ScaleKeyGroup(ctx context.Context, token string) (string, error) {
	var gid *string
	err := s.db(ctx).QueryRow(ctx, `SELECT scale_group_id::text FROM agent_enrollment_tokens
		WHERE token_hash = $1 AND expires_at > now()`, hashSecret(token)).Scan(&gid)
	if errors.Is(err, pgx.ErrNoRows) || gid == nil {
		return "", nil
	}
	return *gid, err
}

// SetScaleGroupState pauses or resumes a group.
func (s *Store) SetScaleGroupState(ctx context.Context, id, state, reason string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE scale_groups SET state = $2, paused_reason = $3 WHERE id::text = $1`, id, state, reason)
	return err
}

// SetScaleGroupLooked records what the rules said.
func (s *Store) SetScaleGroupLooked(ctx context.Context, id, note string, at time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE scale_groups SET note = $2, checked_at = $3 WHERE id::text = $1`, id, note, at)
	return err
}

// SetScaleGroupStamp records an add or take-away ("add" or "remove") at at.
func (s *Store) SetScaleGroupStamp(ctx context.Context, id, which string, at time.Time) error {
	col := "last_add_at"
	if which == "remove" {
		col = "last_remove_at"
	}
	_, err := s.db(ctx).Exec(ctx, `UPDATE scale_groups SET `+col+` = $2 WHERE id::text = $1`, id, at)
	return err
}

// SetScaleFailedStarts records how many new servers in a row failed.
func (s *Store) SetScaleFailedStarts(ctx context.Context, id string, n int) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE scale_groups SET failed_starts = $2 WHERE id::text = $1`, id, n)
	return err
}

// AddScaleActivity records something a group did, at e.At or now.
func (s *Store) AddScaleActivity(ctx context.Context, groupID string, e ScaleActivity) error {
	var at *time.Time
	if !e.At.IsZero() {
		at = &e.At
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO scale_group_activity (group_id, kind, actor, detail, change_id, at)
		VALUES ($1::uuid, $2, $3, $4, $5, coalesce($6, now()))`, groupID, e.Kind, e.Actor, e.Detail, e.ChangeID, at)
	return err
}

// ScaleActivityOf is a group's latest activity, newest first.
func (s *Store) ScaleActivityOf(ctx context.Context, groupID string, limit int) ([]ScaleActivity, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT at, kind, actor, detail, change_id FROM scale_group_activity
		WHERE group_id::text = $1 ORDER BY id DESC LIMIT $2`, groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScaleActivity{}
	for rows.Next() {
		var e ScaleActivity
		if err := rows.Scan(&e.At, &e.Kind, &e.Actor, &e.Detail, &e.ChangeID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ScaleAutoAdds counts the servers Argus added to a group by itself since
// since.
func (s *Store) ScaleAutoAdds(ctx context.Context, groupID string, since time.Time) (int, error) {
	var n int
	err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM cloud_changes WHERE resource_id = $1 AND action = 'scale-add'
		AND requested_by = $2 AND decided_by IS NULL AND state IN ('applying', 'applied') AND requested_at >= $3`,
		groupID, string(cloud.ScaleActor), since).Scan(&n)
	return n, err
}

// OpenScaleChange is a group's change waiting or running, if any.
func (s *Store) OpenScaleChange(ctx context.Context, accountID, groupID string) (*CloudChange, error) {
	var id string
	err := s.db(ctx).QueryRow(ctx, `SELECT id::text FROM cloud_changes WHERE account_id::text = $1 AND resource_id = $2
		AND state IN ('awaiting_approval', 'applying') LIMIT 1`, accountID, groupID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := s.CloudChange(ctx, id)
	return &c, err
}

// LapseScaleAsks lapses Argus's own questions nobody answered by before.
func (s *Store) LapseScaleAsks(ctx context.Context, before time.Time) ([]CloudChange, error) {
	rows, err := s.db(ctx).Query(ctx, `UPDATE cloud_changes SET state = 'lapsed', decided_at = now()
		WHERE state = 'awaiting_approval' AND requested_by = $1 AND requested_at < $2 RETURNING id::text`,
		string(cloud.ScaleActor), before)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]CloudChange, 0, len(ids))
	for _, id := range ids {
		c, err := s.CloudChange(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scaleSamples reads each host's 5-minute buckets since since.
func (s *Store) scaleSamples(ctx context.Context, hosts []string, since time.Time) (map[string][]cloud.ScaleSample, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT hostname, bucket, cpu_sum / NULLIF(cpu_n, 0), 100 - mem_pct, swap_pct, load_max,
		       steal_sum / NULLIF(steal_n, 0)
		FROM usage_history WHERE hostname = ANY($1) AND bucket >= $2 ORDER BY hostname, bucket`, hosts, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]cloud.ScaleSample{}
	for rows.Next() {
		var h string
		var at time.Time
		var cpu, mem, swap, load, steal *float64
		if err := rows.Scan(&h, &at, &cpu, &mem, &swap, &load, &steal); err != nil {
			return nil, err
		}
		v := map[string]float64{}
		for k, p := range map[string]*float64{cloud.SignalCPU: cpu, cloud.SignalMemory: mem, cloud.SignalSwap: swap,
			cloud.SignalLoad: load, cloud.SignalSteal: steal} {
			if p != nil {
				v[k] = *p
			}
		}
		out[h] = append(out[h], cloud.ScaleSample{At: at, Values: v})
	}
	return out, rows.Err()
}

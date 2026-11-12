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
Cloud accounts and their inventory, as stored. Read the design in
plans/cloud-management/design.md in the project files.

Nothing secret is here. An account is a role ARN and an account id; the
external id is derived on every call (cloudExternalID) and the credentials
are the control host's own.
*/

// CloudEnvironments are the three environments rules are kept for, the same
// three write approval uses.
var CloudEnvironments = []string{"production", "staging", "development"}

// CloudAccount is one connected account.
type CloudAccount struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	AccountID string `json:"accountId"`
	RoleARN   string `json:"roleArn"`
	// ManageRoleARN is the role changes are made with; empty when the
	// account is read only.
	ManageRoleARN string            `json:"manageRoleArn,omitempty"`
	Access        string            `json:"access"`
	Environment   string            `json:"environment"`
	IdentityARN   string            `json:"identityArn"`
	Regions       []string          `json:"regions"`
	ConnectedBy   string            `json:"connectedBy"`
	ConnectedAt   time.Time         `json:"connectedAt"`
	SyncedAt      *time.Time        `json:"syncedAt"`
	SyncErrors    map[string]string `json:"syncErrors"`
	// Counts is how many of each kind are present now, by kind.
	Counts map[string]int `json:"counts"`
	// ManageMissing is the permissions the manage role lacks, read from its
	// policy; ManageCheckedAt is when, nil when it could not be read.
	ManageMissing   []string   `json:"manageMissing"`
	ManageCheckedAt *time.Time `json:"manageCheckedAt"`
}

// ErrCloudAccountExists is an account that is already connected.
var ErrCloudAccountExists = errors.New("this account is already connected")

// ErrCloudAccountNotFound is an unknown or disconnected account.
var ErrCloudAccountNotFound = errors.New("no such cloud account")

const cloudAccountCols = `a.id::text, a.provider, a.name, a.account_id, a.role_arn, COALESCE(a.manage_role_arn, ''), a.access, a.environment,
	a.identity_arn, a.regions, a.connected_by, a.connected_at, a.synced_at, a.sync_errors,
	COALESCE((SELECT jsonb_object_agg(kind, n) FROM (
		SELECT kind, count(*) AS n FROM cloud_resources r
		WHERE r.account_id = a.id AND r.gone_at IS NULL GROUP BY kind) c), '{}'::jsonb),
	a.manage_missing, a.manage_checked_at`

func scanCloudAccount(row pgx.Row) (CloudAccount, error) {
	var c CloudAccount
	var errs, counts []byte
	if err := row.Scan(&c.ID, &c.Provider, &c.Name, &c.AccountID, &c.RoleARN, &c.ManageRoleARN, &c.Access, &c.Environment,
		&c.IdentityARN, &c.Regions, &c.ConnectedBy, &c.ConnectedAt, &c.SyncedAt, &errs, &counts,
		&c.ManageMissing, &c.ManageCheckedAt); err != nil {
		return CloudAccount{}, err
	}
	c.SyncErrors, c.Counts = map[string]string{}, map[string]int{}
	_ = json.Unmarshal(errs, &c.SyncErrors)
	_ = json.Unmarshal(counts, &c.Counts)
	if c.Regions == nil {
		c.Regions = []string{}
	}
	if c.ManageMissing == nil {
		c.ManageMissing = []string{}
	}
	return c, nil
}

// CloudAccounts lists the connected accounts, by name.
func (s *Store) CloudAccounts(ctx context.Context) ([]CloudAccount, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+cloudAccountCols+` FROM cloud_accounts a
		WHERE a.archived_at IS NULL ORDER BY lower(a.name), a.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudAccount{}
	for rows.Next() {
		c, err := scanCloudAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CloudAccount reads one connected account.
func (s *Store) CloudAccount(ctx context.Context, id string) (CloudAccount, error) {
	c, err := scanCloudAccount(s.db(ctx).QueryRow(ctx, `SELECT `+cloudAccountCols+` FROM cloud_accounts a
		WHERE a.id::text = $1 AND a.archived_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudAccount{}, ErrCloudAccountNotFound
	}
	return c, err
}

// CreateCloudAccount stores an account Verify has already let in.
func (s *Store) CreateCloudAccount(ctx context.Context, c CloudAccount) (CloudAccount, error) {
	var id string
	err := s.db(ctx).QueryRow(ctx, `
		INSERT INTO cloud_accounts (provider, name, account_id, role_arn, manage_role_arn, access, environment,
			identity_arn, regions, connected_by)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), CASE WHEN $5 = '' THEN 'read' ELSE 'manage' END, $6, $7, $8, $9)
		RETURNING id::text`,
		c.Provider, c.Name, c.AccountID, c.RoleARN, c.ManageRoleARN, c.Environment, c.IdentityARN, c.Regions,
		c.ConnectedBy).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return CloudAccount{}, ErrCloudAccountExists
		}
		return CloudAccount{}, fmt.Errorf("create cloud account: %w", err)
	}
	return s.CloudAccount(ctx, id)
}

// ArchiveCloudAccount disconnects an account. Its rows stay, so history
// that names it stays readable; its resources are marked gone, because
// Argus no longer knows anything current about them.
func (s *Store) ArchiveCloudAccount(ctx context.Context, id, by string) (CloudAccount, error) {
	c, err := s.CloudAccount(ctx, id)
	if err != nil {
		return CloudAccount{}, err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return CloudAccount{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE cloud_accounts SET archived_at = now(), archived_by = $2
		WHERE id::text = $1 AND archived_at IS NULL`, id, by)
	if err != nil {
		return CloudAccount{}, err
	}
	if tag.RowsAffected() == 0 {
		return CloudAccount{}, ErrCloudAccountNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE cloud_resources SET gone_at = now()
		WHERE account_id::text = $1 AND gone_at IS NULL`, id); err != nil {
		return CloudAccount{}, err
	}
	// A change still waiting for a decision would otherwise sit there: it
	// can no longer run, and nobody should approve it thinking it might.
	if _, err := tx.Exec(ctx, `UPDATE cloud_changes SET state = 'cancelled', decided_by = $2, decided_at = now()
		WHERE account_id::text = $1 AND state = 'awaiting_approval'`, id, by); err != nil {
		return CloudAccount{}, err
	}
	return c, tx.Commit(ctx)
}

// SaveCloudSnapshot records one region's read: every resource in it is
// upserted, and every resource of the kinds read that was not in it is
// marked gone. A failed read never reaches here, so a failure keeps the
// last inventory rather than emptying it.
func (s *Store) SaveCloudSnapshot(ctx context.Context, accountID string, snap cloud.Snapshot) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	batch := &pgx.Batch{}
	for _, r := range snap.Resources {
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		batch.Queue(`INSERT INTO cloud_resources (account_id, region, kind, provider_id, data, seen_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)
			ON CONFLICT (account_id, region, kind, provider_id)
			DO UPDATE SET data = EXCLUDED.data, seen_at = EXCLUDED.seen_at, gone_at = NULL`,
			accountID, snap.Region, string(r.Kind), r.ID, data, snap.ReadAt)
	}
	kinds := make([]string, len(snap.Kinds))
	for i, k := range snap.Kinds {
		kinds[i] = string(k)
	}
	batch.Queue(`UPDATE cloud_resources SET gone_at = $4
		WHERE account_id = $1::uuid AND region = $2 AND kind = ANY($3) AND gone_at IS NULL AND seen_at < $4`,
		accountID, snap.Region, kinds, snap.ReadAt)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("save %s inventory: %w", snap.Region, err)
	}
	return tx.Commit(ctx)
}

// SetCloudSynced records a finished sync and what failed in it, by region.
func (s *Store) SetCloudSynced(ctx context.Context, id string, at time.Time, errs map[string]string) error {
	if errs == nil {
		errs = map[string]string{}
	}
	b, _ := json.Marshal(errs)
	_, err := s.db(ctx).Exec(ctx, `UPDATE cloud_accounts SET synced_at = $2, sync_errors = $3
		WHERE id::text = $1`, id, at, b)
	return err
}

// CloudItem is one present resource with the account it is in.
type CloudItem struct {
	AccountID   string         `json:"accountId"`
	AccountName string         `json:"accountName"`
	Environment string         `json:"environment"`
	FirstSeen   time.Time      `json:"firstSeen"`
	SeenAt      time.Time      `json:"seenAt"`
	Resource    cloud.Resource `json:"resource"`
	// Server is the Argus server this instance appears to be.
	Server *CloudServerMatch `json:"server,omitempty"`
	// MadeByArgus is whether Argus launched or created it, by its own
	// record. Only what Argus made can be removed or deleted from Argus.
	MadeByArgus bool `json:"madeByArgus"`
}

// CloudServerMatch links an instance to a server, and says how. It is a
// match by address or name, not a fact the agent reported, and the console
// labels it so.
type CloudServerMatch struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	By       string `json:"by"` // "address" or "name"
}

// CloudResources lists the present resources of one kind across connected
// accounts.
func (s *Store) CloudResources(ctx context.Context, kind cloud.Kind) ([]CloudItem, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, a.name, a.environment, r.first_seen, r.seen_at, r.data, `+madeByArgusSQL+`
		FROM cloud_resources r JOIN cloud_accounts a ON a.id = r.account_id
		WHERE r.kind = $1 AND r.gone_at IS NULL AND a.archived_at IS NULL
		ORDER BY lower(a.name), r.region, r.data->>'name'`, string(kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudItem{}
	for rows.Next() {
		var it CloudItem
		var data []byte
		if err := rows.Scan(&it.AccountID, &it.AccountName, &it.Environment, &it.FirstSeen, &it.SeenAt, &data, &it.MadeByArgus); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &it.Resource); err != nil {
			return nil, fmt.Errorf("cloud resource %s: %w", it.AccountID, err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetCloudManageMissing records what the manage role lacks, as read from
// its policy now.
func (s *Store) SetCloudManageMissing(ctx context.Context, id string, missing []string) error {
	if missing == nil {
		missing = []string{}
	}
	_, err := s.db(ctx).Exec(ctx, `UPDATE cloud_accounts SET manage_missing = $2, manage_checked_at = now()
		WHERE id::text = $1`, id, missing)
	return err
}

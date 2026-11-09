package control

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Costs and budgets, as stored. An account's last billing read is one row,
kept whole; a failed read keeps the last good one and says why. Budgets are
monthly, for one account or all of them. See cloud_cost_routes.go for when
costs are read and when budgets alert.
*/

// CloudCostRead is one account's last billing read.
type CloudCostRead struct {
	AccountID    string       `json:"accountId"`
	AccountName  string       `json:"accountName"`
	CloudAccount string       `json:"cloudAccountId"`
	Environment  string       `json:"environment"`
	ReadAt       *time.Time   `json:"readAt"`
	Costs        *cloud.Costs `json:"costs"`
	Error        string       `json:"error,omitempty"`
	ErrorAt      *time.Time   `json:"errorAt,omitempty"`
	AskedAt      *time.Time   `json:"askedAt,omitempty"`
}

// CloudCostReads is every connected account's last read, by name; an
// account never read has no Costs.
func (s *Store) CloudCostReads(ctx context.Context) ([]CloudCostRead, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT a.id::text, a.name, a.account_id, a.environment, c.read_at, c.costs,
			COALESCE(c.error, ''), c.error_at, c.asked_at
		FROM cloud_accounts a LEFT JOIN cloud_costs c ON c.account_id = a.id
		WHERE a.archived_at IS NULL ORDER BY lower(a.name), a.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudCostRead{}
	for rows.Next() {
		var c CloudCostRead
		var data []byte
		if err := rows.Scan(&c.AccountID, &c.AccountName, &c.CloudAccount, &c.Environment, &c.ReadAt, &data,
			&c.Error, &c.ErrorAt, &c.AskedAt); err != nil {
			return nil, err
		}
		if len(data) > 0 {
			var costs cloud.Costs
			if err := json.Unmarshal(data, &costs); err == nil {
				c.Costs = &costs
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveCloudCosts records a good read and clears the last failure.
func (s *Store) SaveCloudCosts(ctx context.Context, accountID string, c cloud.Costs, at time.Time) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO cloud_costs (account_id, read_at, costs) VALUES ($1::uuid, $2, $3)
		ON CONFLICT (account_id) DO UPDATE SET read_at = $2, costs = $3, error = '', error_at = NULL`, accountID, at, data)
	return err
}

// SetCloudCostsError records why a read failed, keeping the last good one.
func (s *Store) SetCloudCostsError(ctx context.Context, accountID, msg string, at time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO cloud_costs (account_id, error, error_at) VALUES ($1::uuid, $2, $3)
		ON CONFLICT (account_id) DO UPDATE SET error = $2, error_at = $3`, accountID, msg, at)
	return err
}

// AskCloudCosts notes that a person asked for a read now, unless someone
// asked, or a read finished, within the last `within`; it reports whether
// the read may go ahead.
func (s *Store) AskCloudCosts(ctx context.Context, accountID string, within time.Duration) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `INSERT INTO cloud_costs (account_id, asked_at) VALUES ($1::uuid, now())
		ON CONFLICT (account_id) DO UPDATE SET asked_at = now()
		WHERE (cloud_costs.asked_at IS NULL OR cloud_costs.asked_at <= now() - make_interval(secs => $2))
		  AND (cloud_costs.read_at IS NULL OR cloud_costs.read_at <= now() - make_interval(secs => $2))`,
		accountID, within.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CloudBudget is a monthly budget.
type CloudBudget struct {
	ID string `json:"id"`
	// AccountID is the account it is for; "" is every account.
	AccountID  string    `json:"accountId"`
	Name       string    `json:"name"`
	Amount     float64   `json:"amount"`
	Thresholds []int     `json:"thresholds"`
	Forecast   bool      `json:"forecast"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	// AlertedMonth and Alerted are the alerts already sent: thresholds,
	// and 0 for the forecast.
	AlertedMonth string `json:"alertedMonth"`
	Alerted      []int  `json:"alerted"`
}

// ErrCloudBudgetNotFound is an unknown budget.
var ErrCloudBudgetNotFound = errors.New("no such budget")

const cloudBudgetCols = `id::text, COALESCE(account_id::text, ''), name, amount::float8, thresholds, forecast, created_by, created_at,
	alerted_month, alerted`

func scanCloudBudget(row pgx.Row) (CloudBudget, error) {
	var b CloudBudget
	var th, al []int32
	err := row.Scan(&b.ID, &b.AccountID, &b.Name, &b.Amount, &th, &b.Forecast, &b.CreatedBy, &b.CreatedAt, &b.AlertedMonth, &al)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudBudget{}, ErrCloudBudgetNotFound
	}
	b.Thresholds, b.Alerted = ints(th), ints(al)
	return b, err
}

func ints(v []int32) []int {
	out := make([]int, len(v))
	for i, x := range v {
		out[i] = int(x)
	}
	return out
}

func int32s(v []int) []int32 {
	out := make([]int32, len(v))
	for i, x := range v {
		out[i] = int32(x)
	}
	return out
}

// CloudBudgets lists the budgets, for every account first, then by name.
// A budget for an account that was disconnected is left out.
func (s *Store) CloudBudgets(ctx context.Context) ([]CloudBudget, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+cloudBudgetCols+` FROM cloud_budgets b
		WHERE b.account_id IS NULL OR EXISTS (SELECT 1 FROM cloud_accounts a WHERE a.id = b.account_id AND a.archived_at IS NULL)
		ORDER BY b.account_id IS NOT NULL, lower(b.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CloudBudget{}
	for rows.Next() {
		b, err := scanCloudBudget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CloudBudget reads one budget.
func (s *Store) CloudBudget(ctx context.Context, id string) (CloudBudget, error) {
	return scanCloudBudget(s.db(ctx).QueryRow(ctx, `SELECT `+cloudBudgetCols+` FROM cloud_budgets WHERE id::text = $1`, id))
}

// SaveCloudBudget creates a budget, or changes one when b.ID is set. A
// change starts its alerts afresh, so a raised amount alerts again.
func (s *Store) SaveCloudBudget(ctx context.Context, b CloudBudget) (CloudBudget, error) {
	var id string
	var err error
	if b.ID == "" {
		err = s.db(ctx).QueryRow(ctx, `INSERT INTO cloud_budgets (account_id, name, amount, thresholds, forecast, created_by)
			VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6) RETURNING id::text`,
			b.AccountID, b.Name, b.Amount, int32s(b.Thresholds), b.Forecast, b.CreatedBy).Scan(&id)
	} else {
		err = s.db(ctx).QueryRow(ctx, `UPDATE cloud_budgets SET account_id = NULLIF($2, '')::uuid, name = $3, amount = $4,
				thresholds = $5, forecast = $6, alerted_month = '', alerted = '{}'
			WHERE id::text = $1 RETURNING id::text`,
			b.ID, b.AccountID, b.Name, b.Amount, int32s(b.Thresholds), b.Forecast).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudBudget{}, ErrCloudBudgetNotFound
	}
	if err != nil {
		return CloudBudget{}, err
	}
	return s.CloudBudget(ctx, id)
}

// DeleteCloudBudget removes a budget.
func (s *Store) DeleteCloudBudget(ctx context.Context, id string) (CloudBudget, error) {
	b, err := s.CloudBudget(ctx, id)
	if err != nil {
		return CloudBudget{}, err
	}
	_, err = s.db(ctx).Exec(ctx, `DELETE FROM cloud_budgets WHERE id::text = $1`, id)
	return b, err
}

// MarkCloudBudgetAlerted records an alert as sent for month, unless it
// already was; it reports whether this call recorded it, so two checks at
// once send it once.
func (s *Store) MarkCloudBudgetAlerted(ctx context.Context, id, month string, which int) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE cloud_budgets SET
			alerted = CASE WHEN alerted_month = $2 THEN array_append(alerted, $3) ELSE ARRAY[$3]::integer[] END,
			alerted_month = $2
		WHERE id::text = $1 AND NOT (alerted_month = $2 AND $3 = ANY (alerted))`, id, month, which)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

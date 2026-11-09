package control

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Giving access to many databases at once, and to the ones added later.

A bulk grant writes exactly the rows giving it one database at a time would:
one database_assignments row per team or person per database. Nothing that
decides whether a session opens reads anything new. It never lowers anyone
unless asked, and the preview (dry run) and the write share one planner, so
what the console shows is what is written.

A rule (database_access_rules) is the "and the ones added later" half: when a
database is added on its server, each rule of its kind is applied to it as an
ordinary row that remembers the rule (rule_id), so taking the rule away can
take away what it gave.
*/

// Bounds on one request, so a caller cannot make one statement plan a
// million rows.
const (
	maxBulkDatabases = 500
	maxBulkWho       = 100
)

// Approval modes for a bulk grant or a rule: as the database's label says
// (on for Write and above on production), always, or never.
const (
	ApprovalByLabel = "label"
	ApprovalAlways  = "always"
	ApprovalNever   = "never"
)

// Rule kinds: which entries on a server a rule covers.
const (
	RuleKindDatabase = "database"
	RuleKindBroker   = "broker"
)

// BulkAccessInput gives teams and people a level on many databases.
type BulkAccessInput struct {
	DatabaseIDs []string `json:"databaseIds"`
	Teams       []string `json:"teams"`
	People      []string `json:"people"`
	// Level is for databases; BrokerLevel for brokers, and Level when empty.
	Level       string `json:"level"`
	BrokerLevel string `json:"brokerLevel,omitempty"`
	// Approval is label (the default), always or never.
	Approval string `json:"approval,omitempty"`
	// Lower also lowers anyone who has more than they are given here.
	Lower bool `json:"lower,omitempty"`
	// Later also gives it on every database (or broker) added to the same
	// server from now on: a rule per team or person and kind. The databases
	// must all be on one server.
	Later bool `json:"later,omitempty"`
}

// BulkAccessLine is one team or person on one database: what they have and
// what they will have.
type BulkAccessLine struct {
	DatabaseID     string  `json:"databaseId"`
	DatabaseName   string  `json:"databaseName"`
	Engine         string  `json:"engine"`
	Hostname       string  `json:"hostname"`
	Label          string  `json:"label"`
	TeamID         *string `json:"teamId,omitempty"`
	TeamName       string  `json:"teamName,omitempty"`
	UserEmail      *string `json:"userEmail,omitempty"`
	Before         string  `json:"before"`
	BeforeApproval bool    `json:"beforeApproval"`
	After          string  `json:"after"`
	AfterApproval  bool    `json:"afterApproval"`
	// Change is new, raised, approval (only whether sessions wait changes),
	// same, kept (has more, left alone), lowered, or removed.
	Change string `json:"change"`
}

// Writes reports whether applying the line changes a row.
func (l BulkAccessLine) Writes() bool {
	switch l.Change {
	case "new", "raised", "approval", "lowered", "removed":
		return true
	}
	return false
}

// DatabaseRule gives a team or person every database (or broker) added to a
// server from now on.
type DatabaseRule struct {
	ID        string    `json:"id"`
	AssetID   string    `json:"assetId"`
	Hostname  string    `json:"hostname"`
	TeamID    *string   `json:"teamId,omitempty"`
	TeamName  string    `json:"teamName,omitempty"`
	UserEmail *string   `json:"userEmail,omitempty"`
	Kind      string    `json:"kind"`
	Level     string    `json:"level"`
	Approval  string    `json:"approval"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	// Gave is how many rows it wrote that are still its own.
	Gave int `json:"gave"`
}

// querier is what the planner needs from a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// who is one team or one person.
type who struct {
	teamID   string
	teamName string
	email    string
}

func (w who) key() string {
	if w.teamID != "" {
		return "t:" + w.teamID
	}
	return "u:" + w.email
}

type bulkDB struct {
	id, name, engine, hostname, label, assetID string
}

func (d bulkDB) broker() bool { return Database{Engine: d.engine}.Broker() }

type bulkRow struct {
	level    string
	approval bool
	ruleID   string
}

// needsApproval is whether sessions at level on a database with label wait
// for an approver, under mode.
func needsApproval(mode, label, level string) bool {
	switch mode {
	case ApprovalAlways:
		return true
	case ApprovalNever:
		return false
	}
	return label == "production" && tunnel.LevelRank(level) > tunnel.LevelRank(tunnel.LevelRead)
}

func (in *BulkAccessInput) normalize() error {
	in.DatabaseIDs = dedupeIDs(in.DatabaseIDs, false)
	in.Teams = dedupeIDs(in.Teams, false)
	in.People = dedupeIDs(in.People, true)
	if in.BrokerLevel == "" {
		in.BrokerLevel = in.Level
	}
	if in.Approval == "" {
		in.Approval = ApprovalByLabel
	}
	switch {
	case len(in.DatabaseIDs) == 0:
		return ErrInvalidDatabase{"pick at least one database or broker"}
	case len(in.DatabaseIDs) > maxBulkDatabases:
		return ErrInvalidDatabase{fmt.Sprintf("at most %d databases at once", maxBulkDatabases)}
	case len(in.Teams)+len(in.People) == 0:
		return ErrInvalidDatabase{"pick at least one team or person"}
	case len(in.Teams)+len(in.People) > maxBulkWho:
		return ErrInvalidDatabase{fmt.Sprintf("at most %d teams and people at once", maxBulkWho)}
	case tunnel.LevelRank(in.Level) == 0 || tunnel.LevelRank(in.BrokerLevel) == 0:
		return ErrInvalidDatabase{"the level must be read, write, owner or dba"}
	case in.Approval != ApprovalByLabel && in.Approval != ApprovalAlways && in.Approval != ApprovalNever:
		return ErrInvalidDatabase{"approval must be label, always or never"}
	}
	return nil
}

func dedupeIDs(in []string, lower bool) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if lower {
			s = strings.ToLower(s)
		}
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// loadBulk reads the databases, the teams and what everyone has on them.
func loadBulk(ctx context.Context, q querier, dbIDs, teamIDs, people []string, lock bool) (
	[]bulkDB, []who, map[string]map[string]bulkRow, error) {
	rows, err := q.Query(ctx, `
		SELECT d.id::text, d.dbname, d.engine, a.hostname, d.label, d.asset_id::text
		FROM databases d JOIN assets a ON a.id = d.asset_id
		WHERE d.id::text = ANY($1)
		ORDER BY a.hostname, d.engine, d.dbname`, dbIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	dbs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (bulkDB, error) {
		var d bulkDB
		err := r.Scan(&d.id, &d.name, &d.engine, &d.hostname, &d.label, &d.assetID)
		return d, err
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if len(dbs) != len(dbIDs) {
		return nil, nil, nil, ErrInvalidDatabase{"a database picked is no longer in Argus; reload and pick again"}
	}

	var whos []who
	if len(teamIDs) > 0 {
		rows, err := q.Query(ctx, `SELECT id::text, name FROM teams WHERE id::text = ANY($1) ORDER BY name`, teamIDs)
		if err != nil {
			return nil, nil, nil, err
		}
		teams, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (who, error) {
			var w who
			err := r.Scan(&w.teamID, &w.teamName)
			return w, err
		})
		if err != nil {
			return nil, nil, nil, err
		}
		if len(teams) != len(teamIDs) {
			return nil, nil, nil, ErrInvalidDatabase{"a team picked no longer exists; reload and pick again"}
		}
		whos = append(whos, teams...)
	}
	for _, e := range people {
		whos = append(whos, who{email: e})
	}

	sql := `
		SELECT database_id::text, coalesce(team_id::text, ''), coalesce(user_email, ''), level, needs_approval,
		       coalesce(rule_id::text, '')
		FROM database_assignments
		WHERE database_id::text = ANY($1) AND (team_id::text = ANY($2) OR user_email = ANY($3))`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err = q.Query(ctx, sql, dbIDs, teamIDs, people)
	if err != nil {
		return nil, nil, nil, err
	}
	have := map[string]map[string]bulkRow{}
	for rows.Next() {
		var dbID, teamID, email string
		var r bulkRow
		if err := rows.Scan(&dbID, &teamID, &email, &r.level, &r.approval, &r.ruleID); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		k := who{teamID: teamID, email: email}.key()
		if have[dbID] == nil {
			have[dbID] = map[string]bulkRow{}
		}
		have[dbID][k] = r
	}
	rows.Close()
	return dbs, whos, have, rows.Err()
}

// planGive works out every line of a grant. A row written by ruleID is the
// rule's own and always follows it, as if Lower were set for it.
func planGive(in BulkAccessInput, dbs []bulkDB, whos []who, have map[string]map[string]bulkRow, ruleID string) []BulkAccessLine {
	lines := make([]BulkAccessLine, 0, len(dbs)*len(whos))
	for _, w := range whos {
		for _, d := range dbs {
			level := in.Level
			if d.broker() {
				level = in.BrokerLevel
			}
			l := BulkAccessLine{DatabaseID: d.id, DatabaseName: d.name, Engine: d.engine, Hostname: d.hostname,
				Label: d.label, TeamName: w.teamName,
				After: level, AfterApproval: needsApproval(in.Approval, d.label, level)}
			if w.teamID != "" {
				id := w.teamID
				l.TeamID = &id
			} else {
				e := w.email
				l.UserEmail = &e
			}
			cur, ok := have[d.id][w.key()]
			lower := in.Lower || (ruleID != "" && cur.ruleID == ruleID)
			l.Before, l.BeforeApproval = cur.level, cur.approval
			switch rb, ra := tunnel.LevelRank(cur.level), tunnel.LevelRank(level); {
			case !ok:
				l.Change = "new"
			case ra > rb:
				l.Change = "raised"
			case ra < rb && lower:
				l.Change = "lowered"
			case ra < rb:
				l.Change, l.After, l.AfterApproval = "kept", cur.level, cur.approval
			case cur.approval != l.AfterApproval:
				l.Change = "approval"
			default:
				l.Change = "same"
			}
			lines = append(lines, l)
		}
	}
	return lines
}

// PlanBulkAccess is the dry run: every line, nothing written.
func (s *Store) PlanBulkAccess(ctx context.Context, in BulkAccessInput) ([]BulkAccessLine, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	dbs, whos, have, err := loadBulk(ctx, s.db(ctx), in.DatabaseIDs, in.Teams, in.People, false)
	if err != nil {
		return nil, bulkErr(err)
	}
	if in.Later {
		if _, err := oneServer(dbs); err != nil {
			return nil, err
		}
	}
	return planGive(in, dbs, whos, have, ""), nil
}

func oneServer(dbs []bulkDB) (string, error) {
	asset := dbs[0].assetID
	for _, d := range dbs {
		if d.assetID != asset {
			return "", ErrInvalidDatabase{"databases added later can only be given on one server at a time"}
		}
	}
	return asset, nil
}

// ApplyBulkAccess writes a grant in one transaction: every line or none.
// With Later it also sets a rule per team or person for each kind picked,
// and the rows it writes are that rule's.
func (s *Store) ApplyBulkAccess(ctx context.Context, in BulkAccessInput, actor string) ([]BulkAccessLine, []DatabaseRule, error) {
	if err := in.normalize(); err != nil {
		return nil, nil, err
	}
	var lines []BulkAccessLine
	var rules []DatabaseRule
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		dbs, whos, have, err := loadBulk(ctx, tx, in.DatabaseIDs, in.Teams, in.People, true)
		if err != nil {
			return err
		}
		ruleFor := map[string]string{} // who key + kind -> rule id
		if in.Later {
			asset, err := oneServer(dbs)
			if err != nil {
				return err
			}
			kinds := map[string]string{}
			for _, d := range dbs {
				if d.broker() {
					kinds[RuleKindBroker] = in.BrokerLevel
				} else {
					kinds[RuleKindDatabase] = in.Level
				}
			}
			for _, w := range whos {
				for kind, level := range kinds {
					r, err := setRule(ctx, tx, DatabaseRule{AssetID: asset, Kind: kind, Level: level,
						Approval: in.Approval, TeamID: optional(w.teamID), UserEmail: optional(w.email), TeamName: w.teamName,
						Hostname: dbs[0].hostname}, actor)
					if err != nil {
						return err
					}
					ruleFor[w.key()+"|"+kind] = r.ID
					rules = append(rules, r)
				}
			}
		}
		lines = planGive(in, dbs, whos, have, "")
		for _, l := range lines {
			if !l.Writes() {
				continue
			}
			kind := RuleKindDatabase
			if (Database{Engine: l.Engine}).Broker() {
				kind = RuleKindBroker
			}
			k := who{teamID: deref(l.TeamID), email: deref(l.UserEmail)}.key()
			if err := writeAssignment(ctx, tx, l, actor, ruleFor[k+"|"+kind]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, bulkErr(err)
	}
	return lines, rules, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// writeAssignment upserts one line's row. ruleID marks it as a rule's.
func writeAssignment(ctx context.Context, q querier, l BulkAccessLine, actor, ruleID string) error {
	var rule *string
	if ruleID != "" {
		rule = &ruleID
	}
	if l.TeamID != nil {
		_, err := q.Exec(ctx, `
			INSERT INTO database_assignments (database_id, team_id, level, needs_approval, granted_by, rule_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::uuid)
			ON CONFLICT (database_id, team_id) WHERE team_id IS NOT NULL
			DO UPDATE SET level = EXCLUDED.level, needs_approval = EXCLUDED.needs_approval,
			              granted_by = EXCLUDED.granted_by, granted_at = now(), rule_id = EXCLUDED.rule_id`,
			l.DatabaseID, *l.TeamID, l.After, l.AfterApproval, actor, rule)
		return err
	}
	_, err := q.Exec(ctx, `
		INSERT INTO database_assignments (database_id, user_email, level, needs_approval, granted_by, rule_id)
		VALUES ($1::uuid, $2, $3, $4, $5, $6::uuid)
		ON CONFLICT (database_id, user_email) WHERE user_email IS NOT NULL
		DO UPDATE SET level = EXCLUDED.level, needs_approval = EXCLUDED.needs_approval,
		              granted_by = EXCLUDED.granted_by, granted_at = now(), rule_id = EXCLUDED.rule_id`,
		l.DatabaseID, *l.UserEmail, l.After, l.AfterApproval, actor, rule)
	return err
}

// BulkRemoveInput takes teams and people off many databases.
type BulkRemoveInput struct {
	DatabaseIDs []string `json:"databaseIds"`
	Teams       []string `json:"teams"`
	People      []string `json:"people"`
}

// RemoveBulkAccess deletes every row the teams and people have on the
// databases, in one transaction, and returns what went.
func (s *Store) RemoveBulkAccess(ctx context.Context, in BulkRemoveInput) ([]BulkAccessLine, error) {
	in.DatabaseIDs = dedupeIDs(in.DatabaseIDs, false)
	in.Teams = dedupeIDs(in.Teams, false)
	in.People = dedupeIDs(in.People, true)
	switch {
	case len(in.DatabaseIDs) == 0 || len(in.Teams)+len(in.People) == 0:
		return nil, ErrInvalidDatabase{"pick the databases and who loses access"}
	case len(in.DatabaseIDs) > maxBulkDatabases || len(in.Teams)+len(in.People) > maxBulkWho:
		return nil, ErrInvalidDatabase{"too many at once"}
	}
	var lines []BulkAccessLine
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		dbs, whos, have, err := loadBulk(ctx, tx, in.DatabaseIDs, in.Teams, in.People, true)
		if err != nil {
			return err
		}
		for _, w := range whos {
			for _, d := range dbs {
				cur, ok := have[d.id][w.key()]
				if !ok {
					continue
				}
				l := BulkAccessLine{DatabaseID: d.id, DatabaseName: d.name, Engine: d.engine, Hostname: d.hostname,
					Label: d.label, TeamID: optional(w.teamID), TeamName: w.teamName, UserEmail: optional(w.email),
					Before: cur.level, BeforeApproval: cur.approval, Change: "removed"}
				if w.teamID != "" {
					_, err = tx.Exec(ctx, `DELETE FROM database_assignments WHERE database_id = $1::uuid AND team_id = $2::uuid`, d.id, w.teamID)
				} else {
					_, err = tx.Exec(ctx, `DELETE FROM database_assignments WHERE database_id = $1::uuid AND user_email = $2`, d.id, w.email)
				}
				if err != nil {
					return err
				}
				lines = append(lines, l)
			}
		}
		return nil
	})
	if err != nil {
		return nil, bulkErr(err)
	}
	return lines, nil
}

func bulkErr(err error) error {
	var invalid ErrInvalidDatabase
	if errors.As(err, &invalid) {
		return invalid
	}
	return dbAssignErr(err)
}

// AssetDatabaseAccess is every row on every database of one server, for the
// Who has what grid.
func (s *Store) AssetDatabaseAccess(ctx context.Context, assetID string) ([]DBAssignment, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT g.id::text, g.database_id::text, g.team_id::text, coalesce(t.name, ''), g.user_email,
		       g.level, g.needs_approval, g.granted_by, g.granted_at, g.rule_id::text
		FROM database_assignments g
		JOIN databases d ON d.id = g.database_id
		LEFT JOIN teams t ON t.id = g.team_id
		WHERE d.asset_id = $1::uuid
		ORDER BY g.team_id IS NULL, t.name, g.user_email, d.dbname`, assetID)
	if err != nil {
		if strings.Contains(err.Error(), "invalid input syntax for type uuid") {
			return nil, ErrInvalidDatabase{"no such server"}
		}
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DBAssignment, error) {
		var g DBAssignment
		err := r.Scan(&g.ID, &g.DatabaseID, &g.TeamID, &g.TeamName, &g.UserEmail, &g.Level,
			&g.NeedsApproval, &g.GrantedBy, &g.GrantedAt, &g.RuleID)
		return g, err
	})
}

// DatabaseRules lists a server's rules for databases added later.
func (s *Store) DatabaseRules(ctx context.Context, assetID string) ([]DatabaseRule, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT r.id::text, r.asset_id::text, a.hostname, r.team_id::text, coalesce(t.name, ''), r.user_email, r.kind,
		       r.level, r.approval, r.created_by, r.created_at,
		       (SELECT count(*) FROM database_assignments g WHERE g.rule_id = r.id)
		FROM database_access_rules r JOIN assets a ON a.id = r.asset_id LEFT JOIN teams t ON t.id = r.team_id
		WHERE r.asset_id = $1::uuid
		ORDER BY r.kind, r.team_id IS NULL, t.name, r.user_email`, assetID)
	if err != nil {
		if strings.Contains(err.Error(), "invalid input syntax for type uuid") {
			return nil, ErrInvalidDatabase{"no such server"}
		}
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DatabaseRule, error) {
		var x DatabaseRule
		err := r.Scan(&x.ID, &x.AssetID, &x.Hostname, &x.TeamID, &x.TeamName, &x.UserEmail, &x.Kind, &x.Level, &x.Approval,
			&x.CreatedBy, &x.CreatedAt, &x.Gave)
		return x, err
	})
}

// setRule upserts one rule.
func setRule(ctx context.Context, q querier, r DatabaseRule, actor string) (DatabaseRule, error) {
	conflict := `(asset_id, kind, user_email) WHERE user_email IS NOT NULL`
	if r.TeamID != nil {
		conflict = `(asset_id, kind, team_id) WHERE team_id IS NOT NULL`
	}
	err := q.QueryRow(ctx, `
		INSERT INTO database_access_rules (asset_id, team_id, user_email, kind, level, approval, created_by)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)
		ON CONFLICT `+conflict+`
		DO UPDATE SET level = EXCLUDED.level, approval = EXCLUDED.approval,
		              created_by = EXCLUDED.created_by, created_at = now()
		RETURNING id::text, created_at`,
		r.AssetID, r.TeamID, r.UserEmail, r.Kind, r.Level, r.Approval, actor).Scan(&r.ID, &r.CreatedAt)
	r.CreatedBy = actor
	return r, err
}

// RemoveDatabaseRule stops a rule. With takeAway, the rows it gave that are
// still its own go too, and are returned.
func (s *Store) RemoveDatabaseRule(ctx context.Context, assetID, ruleID string, takeAway bool) (DatabaseRule, []BulkAccessLine, error) {
	var rule DatabaseRule
	var lines []BulkAccessLine
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT r.id::text, r.asset_id::text, a.hostname, r.team_id::text, coalesce(t.name, ''), r.user_email, r.kind,
			       r.level, r.approval
			FROM database_access_rules r JOIN assets a ON a.id = r.asset_id LEFT JOIN teams t ON t.id = r.team_id
			WHERE r.id = $1::uuid AND r.asset_id = $2::uuid FOR UPDATE OF r`, ruleID, assetID).
			Scan(&rule.ID, &rule.AssetID, &rule.Hostname, &rule.TeamID, &rule.TeamName, &rule.UserEmail, &rule.Kind, &rule.Level, &rule.Approval)
		if errors.Is(err, pgx.ErrNoRows) || (err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")) {
			return ErrDatabaseNotFound
		}
		if err != nil {
			return err
		}
		if takeAway {
			rows, err := tx.Query(ctx, `
				DELETE FROM database_assignments g USING databases d, assets a
				WHERE g.rule_id = $1::uuid AND d.id = g.database_id AND a.id = d.asset_id
				RETURNING g.database_id::text, d.dbname, d.engine, a.hostname, d.label, g.level, g.needs_approval`, ruleID)
			if err != nil {
				return err
			}
			lines, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (BulkAccessLine, error) {
				l := BulkAccessLine{TeamID: rule.TeamID, TeamName: rule.TeamName, UserEmail: rule.UserEmail, Change: "removed"}
				err := r.Scan(&l.DatabaseID, &l.DatabaseName, &l.Engine, &l.Hostname, &l.Label, &l.Before, &l.BeforeApproval)
				return l, err
			})
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM database_access_rules WHERE id = $1::uuid`, ruleID)
		return err
	})
	if err != nil {
		return rule, nil, err
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].DatabaseName < lines[j].DatabaseName })
	return rule, lines, nil
}

// ApplyDatabaseRules gives a database just added to its server to everyone
// a rule of its kind names there. It never lowers anyone.
func (s *Store) ApplyDatabaseRules(ctx context.Context, dbID, actor string) ([]BulkAccessLine, error) {
	var lines []BulkAccessLine
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		var assetID, engine string
		if err := tx.QueryRow(ctx, `SELECT asset_id::text, engine FROM databases WHERE id = $1::uuid`, dbID).
			Scan(&assetID, &engine); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrDatabaseNotFound
			}
			return err
		}
		kind := RuleKindDatabase
		if (Database{Engine: engine}).Broker() {
			kind = RuleKindBroker
		}
		rows, err := tx.Query(ctx, `
			SELECT r.id::text, coalesce(r.team_id::text, ''), coalesce(r.user_email, ''), r.level, r.approval
			FROM database_access_rules r WHERE r.asset_id = $1::uuid AND r.kind = $2`, assetID, kind)
		if err != nil {
			return err
		}
		type rr struct{ id, team, email, level, approval string }
		rules, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rr, error) {
			var x rr
			err := r.Scan(&x.id, &x.team, &x.email, &x.level, &x.approval)
			return x, err
		})
		if err != nil {
			return err
		}
		for _, r := range rules {
			in := BulkAccessInput{DatabaseIDs: []string{dbID}, Level: r.level, BrokerLevel: r.level, Approval: r.approval}
			var teams, people []string
			if r.team != "" {
				teams = []string{r.team}
			} else {
				people = []string{r.email}
			}
			dbs, whos, have, err := loadBulk(ctx, tx, in.DatabaseIDs, teams, people, true)
			if err != nil {
				return err
			}
			for _, l := range planGive(in, dbs, whos, have, r.id) {
				if !l.Writes() {
					continue
				}
				if err := writeAssignment(ctx, tx, l, actor, r.id); err != nil {
					return err
				}
				lines = append(lines, l)
			}
		}
		return nil
	})
	return lines, err
}

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Databases: what a person can open on a server besides a shell.

A database is a child of an asset. People and teams are assigned to it with a
level -- read, write, owner, dba -- and the agent turns the level into a
Postgres role for the person when they open a session. Nothing here is a
Postgres credential: the control plane knows where the database is and who
may open it, and the agent does the signing in on its own server.

What a person may do on a database is the highest level any of their rows
gives them, their own or a team's. A row marked needs_approval gives that
level only while an approved access request for it is live; without one the
person's level is the highest of their other rows, which may be nothing.
*/

// Database is one database on one server.
type Database struct {
	ID        string    `json:"id"`
	AssetID   string    `json:"assetId"`
	Hostname  string    `json:"hostname"`
	Engine    string    `json:"engine"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Name      string    `json:"dbname"`
	AdminUser string    `json:"adminUser"`
	Label     string    `json:"label"`
	Policy    DBPolicy  `json:"policy"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	// TunnelConnected says the server's agent is reachable, so the database
	// can be opened now.
	TunnelConnected bool `json:"tunnelConnected"`
}

// DBPolicy is the safety settings of one database. Zero means the default.
type DBPolicy struct {
	// StatementTimeoutS stops a statement after this long. 0 = 60 s.
	StatementTimeoutS int `json:"statementTimeoutS,omitempty"`
	// IdleTxTimeoutS ends a transaction left idle. 0 = 120 s.
	IdleTxTimeoutS int `json:"idleTxTimeoutS,omitempty"`
	// MaxRows a person can see from one statement. 0 = 100,000.
	MaxRows int `json:"maxRows,omitempty"`
	// IdleTimeoutS ends a session nobody is using. 0 = 30 min.
	IdleTimeoutS int `json:"idleTimeoutS,omitempty"`
	// MaxSessionS is the longest a session may last. 0 = 8 h.
	MaxSessionS int `json:"maxSessionS,omitempty"`
}

// DBAssignment is who may open a database, at what level.
type DBAssignment struct {
	ID            string    `json:"id"`
	DatabaseID    string    `json:"databaseId"`
	TeamID        *string   `json:"teamId,omitempty"`
	TeamName      string    `json:"teamName,omitempty"`
	UserEmail     *string   `json:"userEmail,omitempty"`
	Level         string    `json:"level"`
	NeedsApproval bool      `json:"needsApproval"`
	GrantedBy     string    `json:"grantedBy"`
	GrantedAt     time.Time `json:"grantedAt"`
}

// DBAccess is what one person may do on one database right now.
type DBAccess struct {
	// Level is what they can open without asking; "" is nothing.
	Level string `json:"level"`
	// Approvable is the highest level they could ask for; "" is none.
	Approvable string `json:"approvable,omitempty"`
	// GrantExpires is set when Level comes from an approved request.
	GrantExpires *time.Time `json:"grantExpires,omitempty"`
}

// ErrInvalidDatabase is a database that cannot be saved as given.
type ErrInvalidDatabase struct{ Reason string }

func (e ErrInvalidDatabase) Error() string { return e.Reason }

// ErrDatabaseNotFound is an id that names nothing.
var ErrDatabaseNotFound = errors.New("no such database")

func (d *Database) validate() error {
	d.Name = strings.TrimSpace(d.Name)
	d.Host = strings.TrimSpace(d.Host)
	d.AdminUser = strings.TrimSpace(d.AdminUser)
	switch {
	case d.Name == "" || len(d.Name) > 63 || strings.ContainsAny(d.Name, "\x00\"'\\"):
		return ErrInvalidDatabase{"the database name must be 1 to 63 characters with no quotes"}
	case d.Host == "":
		d.Host = "/var/run/postgresql"
	case !tunnel.LocalPGHost(d.Host):
		return ErrInvalidDatabase{"the database must be on the server itself: a socket directory " +
			"or a loopback address, not " + d.Host}
	}
	if d.Port == 0 {
		d.Port = 5432
	}
	if d.Port < 1 || d.Port > 65535 {
		return ErrInvalidDatabase{"the port must be between 1 and 65535"}
	}
	if d.AdminUser == "" {
		d.AdminUser = "postgres"
	}
	if !tunnel.ValidPersonalRole(d.AdminUser) && d.AdminUser != "postgres" {
		return ErrInvalidDatabase{"the admin account must be a plain Postgres role name"}
	}
	switch d.Label {
	case "":
		d.Label = "production"
	case "production", "staging", "development":
	default:
		return ErrInvalidDatabase{"the label must be production, staging or development"}
	}
	if d.Engine == "" {
		d.Engine = "postgres"
	}
	if d.Engine != "postgres" {
		return ErrInvalidDatabase{"only Postgres is supported for now"}
	}
	p := &d.Policy
	for _, v := range []*int{&p.StatementTimeoutS, &p.IdleTxTimeoutS, &p.MaxRows, &p.IdleTimeoutS, &p.MaxSessionS} {
		if *v < 0 {
			return ErrInvalidDatabase{"a limit cannot be negative"}
		}
	}
	if p.MaxRows > tunnel.MaxRowsCeiling {
		p.MaxRows = tunnel.MaxRowsCeiling
	}
	if p.MaxSessionS > 24*3600 {
		return ErrInvalidDatabase{"a session cannot last longer than a day"}
	}
	return nil
}

const databaseCols = `d.id::text, d.asset_id::text, a.hostname, d.engine, d.host, d.port, d.dbname,
	d.admin_user, d.label, d.policy, d.created_by, d.created_at,
	EXISTS (SELECT 1 FROM agent_credentials c WHERE lower(c.hostname) = lower(a.hostname)
	        AND c.revoked_at IS NULL AND c.tunnel_seen_at > now() - $1::interval)`

func scanDatabase(row pgx.Row) (Database, error) {
	var d Database
	var policy []byte
	if err := row.Scan(&d.ID, &d.AssetID, &d.Hostname, &d.Engine, &d.Host, &d.Port, &d.Name,
		&d.AdminUser, &d.Label, &policy, &d.CreatedBy, &d.CreatedAt, &d.TunnelConnected); err != nil {
		return d, err
	}
	_ = json.Unmarshal(policy, &d.Policy)
	return d, nil
}

// CreateDatabase adds a database to an asset.
func (s *Store) CreateDatabase(ctx context.Context, d Database, actor string) (Database, error) {
	if err := d.validate(); err != nil {
		return d, err
	}
	policy, _ := json.Marshal(d.Policy)
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO databases (asset_id, engine, host, port, dbname, admin_user, label, policy, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (asset_id, port, dbname) DO UPDATE SET host = EXCLUDED.host
		RETURNING id::text`,
		d.AssetID, d.Engine, d.Host, d.Port, d.Name, d.AdminUser, d.Label, policy, actor).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "foreign key") || strings.Contains(err.Error(), "invalid input syntax for type uuid") {
			return d, ErrInvalidDatabase{"no such server"}
		}
		return d, fmt.Errorf("create database: %w", err)
	}
	return s.Database(ctx, id)
}

// UpdateDatabase changes a database's label, policy or admin account.
func (s *Store) UpdateDatabase(ctx context.Context, d Database) (Database, error) {
	if err := d.validate(); err != nil {
		return d, err
	}
	policy, _ := json.Marshal(d.Policy)
	tag, err := s.pool.Exec(ctx, `UPDATE databases SET host = $2, port = $3, admin_user = $4,
		label = $5, policy = $6 WHERE id = $1::uuid`,
		d.ID, d.Host, d.Port, d.AdminUser, d.Label, policy)
	if err != nil {
		return d, fmt.Errorf("update database: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return d, ErrDatabaseNotFound
	}
	return s.Database(ctx, d.ID)
}

// DeleteDatabase removes a database and its assignments. Sessions and audit
// entries keep naming it by id.
func (s *Store) DeleteDatabase(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM databases WHERE id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDatabaseNotFound
	}
	return nil
}

// Database reads one.
func (s *Store) Database(ctx context.Context, id string) (Database, error) {
	d, err := scanDatabase(s.pool.QueryRow(ctx, `SELECT `+databaseCols+`
		FROM databases d JOIN assets a ON a.id = d.asset_id WHERE d.id = $2::uuid`,
		tunnelFreshFor.String(), id))
	if errors.Is(err, pgx.ErrNoRows) || (err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")) {
		return d, ErrDatabaseNotFound
	}
	return d, err
}

// Databases lists every database, or only those on one asset.
func (s *Store) Databases(ctx context.Context, assetID string) ([]Database, error) {
	q := `SELECT ` + databaseCols + ` FROM databases d JOIN assets a ON a.id = d.asset_id`
	args := []any{tunnelFreshFor.String()}
	if assetID != "" {
		q += ` WHERE d.asset_id = $2::uuid`
		args = append(args, assetID)
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY a.hostname, d.dbname`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Database{}
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DatabasesFor lists the databases email may open or ask for, with their
// access, by the same rule DatabaseAccess uses. Admins and owners see all of
// them at dba.
func (s *Store) DatabasesFor(ctx context.Context, email, role string) ([]Database, map[string]DBAccess, error) {
	all, err := s.Databases(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	access := map[string]DBAccess{}
	if role == "admin" || role == "owner" {
		for _, d := range all {
			access[d.ID] = DBAccess{Level: tunnel.LevelDBA}
		}
		return all, access, nil
	}
	out := all[:0]
	for _, d := range all {
		acc, err := s.DatabaseAccess(ctx, email, d.ID)
		if err != nil {
			return nil, nil, err
		}
		if acc.Level == "" && acc.Approvable == "" {
			continue
		}
		access[d.ID] = acc
		out = append(out, d)
	}
	return out, access, nil
}

// DatabaseAccess is what email may do on database dbID now.
func (s *Store) DatabaseAccess(ctx context.Context, email, dbID string) (DBAccess, error) {
	email = strings.ToLower(email)
	rows, err := s.pool.Query(ctx, `
		SELECT g.level, g.needs_approval FROM database_assignments g
		WHERE g.database_id = $1::uuid
		  AND (g.user_email = $2
		       OR g.team_id IN (SELECT team_id FROM team_members WHERE user_email = $2))`,
		dbID, email)
	if err != nil {
		return DBAccess{}, err
	}
	defer rows.Close()
	var acc DBAccess
	for rows.Next() {
		var level string
		var approval bool
		if err := rows.Scan(&level, &approval); err != nil {
			return acc, err
		}
		if approval {
			if tunnel.LevelRank(level) > tunnel.LevelRank(acc.Approvable) {
				acc.Approvable = level
			}
		} else if tunnel.LevelRank(level) > tunnel.LevelRank(acc.Level) {
			acc.Level = level
		}
	}
	if err := rows.Err(); err != nil {
		return acc, err
	}
	// An approved request raises the level for its window, up to what the
	// person could ask for.
	if acc.Approvable != "" && tunnel.LevelRank(acc.Approvable) > tunnel.LevelRank(acc.Level) {
		var level string
		var expires time.Time
		err := s.pool.QueryRow(ctx, `
			SELECT database_level, expires_at FROM access_requests
			WHERE requester_email = $1 AND database_id = $2::uuid AND state = 'approved'
			  AND expires_at > now()
			ORDER BY expires_at DESC LIMIT 1`, email, dbID).Scan(&level, &expires)
		if err == nil && tunnel.LevelRank(level) > tunnel.LevelRank(acc.Level) &&
			tunnel.LevelRank(level) <= tunnel.LevelRank(acc.Approvable) {
			acc.Level, acc.GrantExpires = level, &expires
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return acc, err
		}
	}
	if tunnel.LevelRank(acc.Approvable) <= tunnel.LevelRank(acc.Level) {
		acc.Approvable = ""
	}
	return acc, nil
}

// DBAssignments lists who may open a database.
func (s *Store) DBAssignments(ctx context.Context, dbID string) ([]DBAssignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id::text, g.database_id::text, g.team_id::text, coalesce(t.name, ''), g.user_email,
		       g.level, g.needs_approval, g.granted_by, g.granted_at
		FROM database_assignments g LEFT JOIN teams t ON t.id = g.team_id
		WHERE g.database_id = $1::uuid
		ORDER BY g.team_id IS NULL, t.name, g.user_email`, dbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DBAssignment{}
	for rows.Next() {
		var g DBAssignment
		if err := rows.Scan(&g.ID, &g.DatabaseID, &g.TeamID, &g.TeamName, &g.UserEmail, &g.Level,
			&g.NeedsApproval, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetDBAssignment gives a team or a person a level on a database, replacing
// what they had.
func (s *Store) SetDBAssignment(ctx context.Context, g DBAssignment, actor string) error {
	if tunnel.LevelRank(g.Level) == 0 {
		return ErrInvalidDatabase{"the level must be read, write, owner or dba"}
	}
	switch {
	case g.TeamID != nil && g.UserEmail == nil:
		_, err := s.pool.Exec(ctx, `
			INSERT INTO database_assignments (database_id, team_id, level, needs_approval, granted_by)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5)
			ON CONFLICT (database_id, team_id) WHERE team_id IS NOT NULL
			DO UPDATE SET level = EXCLUDED.level, needs_approval = EXCLUDED.needs_approval,
			              granted_by = EXCLUDED.granted_by, granted_at = now()`,
			g.DatabaseID, *g.TeamID, g.Level, g.NeedsApproval, actor)
		return dbAssignErr(err)
	case g.UserEmail != nil && g.TeamID == nil:
		email := strings.ToLower(strings.TrimSpace(*g.UserEmail))
		if email == "" {
			return ErrInvalidDatabase{"a person's assignment needs their email"}
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO database_assignments (database_id, user_email, level, needs_approval, granted_by)
			VALUES ($1::uuid, $2, $3, $4, $5)
			ON CONFLICT (database_id, user_email) WHERE user_email IS NOT NULL
			DO UPDATE SET level = EXCLUDED.level, needs_approval = EXCLUDED.needs_approval,
			              granted_by = EXCLUDED.granted_by, granted_at = now()`,
			g.DatabaseID, email, g.Level, g.NeedsApproval, actor)
		return dbAssignErr(err)
	}
	return ErrInvalidDatabase{"an assignment is for a team or a person, not both or neither"}
}

func dbAssignErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "foreign key") || strings.Contains(err.Error(), "invalid input syntax for type uuid") {
		return ErrInvalidDatabase{"no such database or team"}
	}
	return err
}

// RemoveDBAssignment takes an assignment away.
func (s *Store) RemoveDBAssignment(ctx context.Context, dbID, assignmentID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM database_assignments WHERE id = $1::uuid AND database_id = $2::uuid`,
		assignmentID, dbID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDatabaseNotFound
	}
	return nil
}

// CreateDatabaseRequest files an access request for a level on a database.
func (s *Store) CreateDatabaseRequest(ctx context.Context, email, dbID, level, justification string,
	minutes int) (AccessRequest, error) {
	if tunnel.LevelRank(level) == 0 {
		return AccessRequest{}, fmt.Errorf("the level must be read, write, owner or dba")
	}
	if len(justification) < 20 {
		return AccessRequest{}, fmt.Errorf("justification must explain why, in at least 20 characters")
	}
	if minutes <= 0 || minutes > MaxDurationMinutes {
		return AccessRequest{}, fmt.Errorf("duration must be between 1 and %d minutes", MaxDurationMinutes)
	}
	d, err := s.Database(ctx, dbID)
	if err != nil {
		return AccessRequest{}, err
	}
	acc, err := s.DatabaseAccess(ctx, email, dbID)
	if err != nil {
		return AccessRequest{}, err
	}
	if tunnel.LevelRank(level) > tunnel.LevelRank(acc.Approvable) {
		return AccessRequest{}, fmt.Errorf("you are not assigned %s on %s; ask an admin to assign it, "+
			"with approval, before asking for it", level, d.Name)
	}
	r := AccessRequest{RequesterEmail: email, AssetHostnames: []string{d.Hostname},
		Principal: "db:" + level, Justification: justification, DurationMinutes: minutes}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO access_requests
			(requester_email, asset_hostnames, principal, justification,
			 duration_minutes, state, break_glass, database_id, database_level)
		VALUES ($1,$2,$3,$4,$5,'pending',false,$6::uuid,$7)
		RETURNING id::text, created_at`,
		r.RequesterEmail, r.AssetHostnames, r.Principal, r.Justification,
		r.DurationMinutes, dbID, level).Scan(&r.ID, &r.CreatedAt)
	if err != nil {
		return r, fmt.Errorf("create request: %w", err)
	}
	r.State = "pending"
	return r, nil
}

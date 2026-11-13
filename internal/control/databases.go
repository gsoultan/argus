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
	ID       string `json:"id"`
	AssetID  string `json:"assetId"`
	Hostname string `json:"hostname"`
	Engine   string `json:"engine"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	// PoolerHost and PoolerPort are a PgBouncer sessions go through; a port
	// of 0 is none. Provisioning never goes through it.
	PoolerHost string `json:"poolerHost"`
	// ConfigPath is the server's redis.conf, for a Redis: where the agent
	// reads the admin password. A path on the server, never the password.
	ConfigPath string    `json:"configPath"`
	PoolerPort int       `json:"poolerPort"`
	Name       string    `json:"dbname"`
	AdminUser  string    `json:"adminUser"`
	Label      string    `json:"label"`
	Policy     DBPolicy  `json:"policy"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	// TunnelConnected says the server's agent is reachable, so the database
	// can be opened now.
	TunnelConnected bool `json:"tunnelConnected"`
	// AgentHost is the name the server's agent goes by, which its grants
	// name and its tunnel is found under; Hostname is what Argus calls the
	// server. They differ once a server is renamed. See Store.AgentNameFor.
	AgentHost string `json:"-"`
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
	// KeysLimit is the largest Redis db KEYS runs on. 0 = 10,000.
	KeysLimit int `json:"keysLimit,omitempty"`
	// WriteApproval: a statement that changes the database runs only once
	// someone else approves it. Unset follows the label: on for production,
	// off for staging and development. It is apart from a grant's
	// needs_approval, which is about opening a session.
	WriteApproval *bool `json:"writeApproval,omitempty"`
	// WriteApprovers is who decides those changes: "approvers" (unset) for
	// the organization's admins and approvers, or "editors" to add anyone
	// with Write or above on this database. See change_editors.go.
	WriteApprovers string `json:"writeApprovers,omitempty"`
	// RedisLevels are a Redis entry's own ACL rules per level, key patterns
	// included, replacing Argus's (tunnel.RedisLevelRules) for that level.
	RedisLevels map[string][]string `json:"redisLevels,omitempty"`
}

// WritesNeedApproval says whether a statement that changes d waits for
// someone else's approval.
func (d Database) WritesNeedApproval() bool {
	if d.Policy.WriteApproval != nil {
		return *d.Policy.WriteApproval
	}
	return d.Label == "production"
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
	// RuleID names the server's rule for databases added later that wrote
	// this row; nil once an admin sets it by hand.
	RuleID *string `json:"ruleId,omitempty"`
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
	d.ConfigPath = strings.TrimSpace(d.ConfigPath)
	if d.Engine == "" {
		d.Engine = tunnel.EnginePostgres
	}
	switch d.Engine {
	case tunnel.EngineRedis:
		return d.validateRedis()
	case tunnel.EngineRabbitMQ:
		return d.validateRabbit()
	case tunnel.EngineMySQL:
		return d.validateMySQL()
	case tunnel.EngineMongoDB:
		return d.validateMongo()
	case tunnel.EngineKafka:
		return d.validateKafka()
	case tunnel.EnginePostgres:
	default:
		return ErrInvalidDatabase{"the engine must be postgres, mysql, mongodb, redis, rabbitmq or kafka"}
	}
	if d.ConfigPath != "" {
		return ErrInvalidDatabase{"a config file is only for Redis, MySQL and MongoDB"}
	}
	switch {
	case d.Name == "" || len(d.Name) > 63 || strings.ContainsAny(d.Name, "\x00\"'\\"):
		return ErrInvalidDatabase{"the database name must be 1 to 63 characters with no quotes"}
	case strings.HasPrefix(d.Name, tunnel.TestDBPrefix):
		return ErrInvalidDatabase{tunnel.TestDBPrefix + " databases are the throwaway copies test restores make; Argus does not add them"}
	case d.Host == "":
		d.Host = "/var/run/postgresql"
	case !tunnel.LocalPGHost(d.Host) && !tunnel.RemotePGHost(d.Host):
		return ErrInvalidDatabase{"the database must be on the server itself (a socket directory " +
			"or a loopback address) or a Postgres its PgBouncer sends to, not " + d.Host}
	}
	if d.Port == 0 {
		d.Port = 5432
	}
	if d.Port < 1 || d.Port > 65535 {
		return ErrInvalidDatabase{"the port must be between 1 and 65535"}
	}
	d.PoolerHost = strings.TrimSpace(d.PoolerHost)
	switch {
	case d.PoolerPort < 0 || d.PoolerPort > 65535:
		return ErrInvalidDatabase{"the PgBouncer port must be between 1 and 65535"}
	case d.PoolerPort == 0:
		d.PoolerHost = ""
	case d.PoolerHost == "":
		d.PoolerHost = "127.0.0.1"
	case !tunnel.LocalPGHost(d.PoolerHost):
		return ErrInvalidDatabase{"PgBouncer must be on the server itself: a socket directory " +
			"or a loopback address, not " + d.PoolerHost}
	}
	if d.PoolerPort != 0 && d.PoolerPort == d.Port && d.PoolerHost == d.Host {
		return ErrInvalidDatabase{"PgBouncer cannot be at the same address and port as Postgres"}
	}
	if d.AdminUser == "" {
		d.AdminUser = "postgres"
	}
	if !tunnel.ValidPersonalRole(d.AdminUser) && d.AdminUser != "postgres" {
		return ErrInvalidDatabase{"the admin account must be a plain Postgres role name"}
	}
	return d.validateCommon()
}

// validateCommon checks what every engine has: the label and the limits.
func (d *Database) validateCommon() error {
	switch d.Label {
	case "":
		d.Label = "production"
	case "production", "staging", "development":
	default:
		return ErrInvalidDatabase{"the label must be production, staging or development"}
	}
	p := &d.Policy
	for _, v := range []*int{&p.StatementTimeoutS, &p.IdleTxTimeoutS, &p.MaxRows, &p.IdleTimeoutS, &p.MaxSessionS, &p.KeysLimit} {
		if *v < 0 {
			return ErrInvalidDatabase{"a limit cannot be negative"}
		}
	}
	switch p.WriteApprovers {
	case "", ApproversDefault, ApproversEditors:
	default:
		return ErrInvalidDatabase{`who can approve is "approvers" or "editors"`}
	}
	if p.WriteApprovers == ApproversDefault {
		p.WriteApprovers = ""
	}
	if p.MaxRows > tunnel.MaxRowsCeiling {
		p.MaxRows = tunnel.MaxRowsCeiling
	}
	if p.MaxSessionS > 24*3600 {
		return ErrInvalidDatabase{"a session cannot last longer than a day"}
	}
	if d.Engine != tunnel.EnginePostgres {
		// Nothing of Postgres's means anything here; keep it empty so a
		// Redis never shows a PgBouncer.
		d.PoolerHost, d.PoolerPort, d.AdminUser = "", 0, ""
	}
	if d.Engine == tunnel.EngineRedis {
		for level, rules := range p.RedisLevels {
			if tunnel.LevelRank(level) == 0 {
				return ErrInvalidDatabase{"unknown level " + level + " in the Redis rules"}
			}
			if len(rules) == 0 {
				return ErrInvalidDatabase{"the " + level + " level needs at least one rule; use Argus's default instead"}
			}
			if why := tunnel.ValidRedisRules(rules, level); why != "" {
				return ErrInvalidDatabase{level + ": " + why}
			}
		}
		if len(p.RedisLevels) == 0 {
			p.RedisLevels = nil
		}
	} else {
		p.RedisLevels = nil
	}
	if !d.SQL() && d.Engine != tunnel.EngineRedis {
		// SQL statements, and a Redis's bulk, configuration and user changes,
		// wait for approval; see statementGate and redisGate.
		p.WriteApproval, p.WriteApprovers = nil, ""
		if p.MaxRows > tunnel.MaxStoreItems {
			p.MaxRows = tunnel.MaxStoreItems
		}
	}
	return nil
}

// validateRedis: one Redis server on this asset, reached on loopback or its
// unix socket; dbname is the logical db it opens in.
func (d *Database) validateRedis() error {
	if d.Name == "" {
		d.Name = "0"
	}
	if !tunnel.ValidRedisDB(d.Name) {
		return ErrInvalidDatabase{"for Redis, the database is the logical db number it opens in, such as 0"}
	}
	if d.Host == "" {
		d.Host = "127.0.0.1"
	}
	if !tunnel.LocalPGHost(d.Host) {
		return ErrInvalidDatabase{"Redis must be on the server itself: a loopback address or its unix socket, not " + d.Host}
	}
	if d.Port == 0 {
		d.Port = 6379
	}
	if d.Port < 1 || d.Port > 65535 {
		return ErrInvalidDatabase{"the port must be between 1 and 65535"}
	}
	if d.ConfigPath == "" {
		d.ConfigPath = "/etc/redis/redis.conf"
	}
	if !tunnel.ValidRedisConfPath(d.ConfigPath) {
		return ErrInvalidDatabase{"the config file must be an absolute path to a .conf file on the server"}
	}
	return d.validateCommon()
}

// SQL says the entry speaks SQL through the Postgres session's frames:
// Postgres and MySQL. Its statements can wait for approval and its sessions
// open the SQL workspace.
// Broker reports whether the entry is a message broker (RabbitMQ, Kafka),
// which the console lists under Brokers rather than Databases.
func (d Database) Broker() bool {
	return d.Engine == tunnel.EngineRabbitMQ || d.Engine == tunnel.EngineKafka
}

// Noun is what the console calls the entry in a sentence.
func (d Database) Noun() string {
	if d.Broker() {
		return "broker"
	}
	return "database"
}

func (d Database) SQL() bool {
	return d.Engine == "" || d.Engine == tunnel.EnginePostgres || d.Engine == tunnel.EngineMySQL
}

// validateMySQL: one database of a MySQL or MariaDB on this asset, reached
// over its unix socket or loopback; dbname is the database. ConfigPath, when
// set, is an option file the agent signs in to MySQL with.
func (d *Database) validateMySQL() error {
	switch {
	case !tunnel.ValidMySQLDB(d.Name) || len(d.Name) > 63:
		return ErrInvalidDatabase{"the database name must be 1 to 63 characters, not one of MySQL's own " +
			"(mysql, sys, information_schema, performance_schema), with no / \\ or ."}
	case strings.HasPrefix(d.Name, tunnel.TestDBPrefix):
		return ErrInvalidDatabase{tunnel.TestDBPrefix + " databases are the throwaway copies test restores make; Argus does not add them"}
	}
	if d.Host == "" {
		d.Host = "127.0.0.1"
	}
	if !tunnel.LocalPGHost(d.Host) {
		return ErrInvalidDatabase{"MySQL must be on the server itself: a loopback address or its unix socket, not " + d.Host}
	}
	if d.Port == 0 {
		d.Port = 3306
	}
	if d.Port < 1 || d.Port > 65535 {
		return ErrInvalidDatabase{"the port must be between 1 and 65535"}
	}
	if d.ConfigPath != "" && !tunnel.ValidMySQLOptionFile(d.ConfigPath) {
		return ErrInvalidDatabase{"the option file must be an absolute path to a .cnf file on the server"}
	}
	return d.validateCommon()
}

// validateMongo: one database on a mongod on this asset, reached on loopback
// or its unix socket. ConfigPath is the file on the server with the admin
// credentials the agent signs in with; empty means tunnel.MongoAdminFile.
func (d *Database) validateMongo() error {
	if !tunnel.ValidMongoDB(d.Name) {
		return ErrInvalidDatabase{"the MongoDB database name must be 1 to 63 characters with none of / \\ . \" $ * < > : | ? or spaces, " +
			"and not admin, local or config"}
	}
	if strings.HasPrefix(d.Name, tunnel.TestDBPrefix) {
		return ErrInvalidDatabase{"databases named " + tunnel.TestDBPrefix + "… are Argus's own restore tests"}
	}
	if d.Host == "" {
		d.Host = "127.0.0.1"
	}
	if !tunnel.LocalPGHost(d.Host) {
		return ErrInvalidDatabase{"MongoDB must be on the server itself: a loopback address or its unix socket, not " + d.Host}
	}
	if d.Port == 0 {
		d.Port = 27017
	}
	if d.Port < 1 || d.Port > 65535 {
		return ErrInvalidDatabase{"the port must be between 1 and 65535"}
	}
	if d.ConfigPath != "" && !tunnel.ValidMongoAdminFile(d.ConfigPath) {
		return ErrInvalidDatabase{"the admin file must be an absolute path to a .conf file on the server"}
	}
	return d.validateCommon()
}

// validateRabbit: one vhost of a RabbitMQ on this asset, through its
// management API on loopback.
func (d *Database) validateRabbit() error {
	if d.Name == "" {
		d.Name = "/"
	}
	if !tunnel.ValidVhost(d.Name) || len(d.Name) > 63 || strings.ContainsAny(d.Name, "\x00\"'\\") {
		return ErrInvalidDatabase{"the vhost must be 1 to 63 characters with no quotes"}
	}
	if d.Host == "" {
		d.Host = "127.0.0.1"
	}
	if !tunnel.LocalLoopback(d.Host) {
		return ErrInvalidDatabase{"the RabbitMQ management API must be on the server itself: 127.0.0.1, ::1 or localhost, not " + d.Host}
	}
	if d.Port == 0 {
		d.Port = 15672
	}
	if d.Port < 1 || d.Port > 65535 {
		return ErrInvalidDatabase{"the port must be between 1 and 65535"}
	}
	if d.ConfigPath != "" {
		return ErrInvalidDatabase{"a config file is only for Redis and MySQL"}
	}
	return d.validateCommon()
}

// validateKafka: the topics of a Kafka cluster that one scope names, "*" for
// every topic or a prefix such as "orders.", through a SASL listener on this
// asset. ConfigPath is the file on the server with the admin credentials the
// agent signs in with; empty means tunnel.KafkaAdminFile.
func (d *Database) validateKafka() error {
	if d.Name == "" {
		d.Name = tunnel.KafkaAllTopics
	}
	if !tunnel.ValidKafkaScope(d.Name) || len(d.Name) > 63 {
		return ErrInvalidDatabase{"the topics must be * for every topic, or a prefix of 1 to 63 letters, digits, . _ or -"}
	}
	if d.Host == "" {
		d.Host = "127.0.0.1"
	}
	if !tunnel.LocalLoopback(d.Host) {
		return ErrInvalidDatabase{"Kafka's listener must be on the server itself: 127.0.0.1, ::1 or localhost, not " + d.Host}
	}
	if d.Port == 0 {
		d.Port = 9092
	}
	if d.Port < 1 || d.Port > 65535 {
		return ErrInvalidDatabase{"the port must be between 1 and 65535"}
	}
	if d.ConfigPath != "" && !tunnel.ValidKafkaAdminFile(d.ConfigPath) {
		return ErrInvalidDatabase{"the admin file must be an absolute path to a .conf file on the server"}
	}
	return d.validateCommon()
}

const databaseCols = `d.id::text, d.asset_id::text, a.hostname, d.engine, d.host, d.port, d.pooler_host, d.pooler_port, d.config_path, d.dbname,
	d.admin_user, d.label, d.policy, d.created_by, d.created_at,
	EXISTS (SELECT 1 FROM agent_credentials c
	         WHERE lower(c.hostname) IN (lower(a.hostname), lower(a.agent_hostname))
	           AND c.revoked_at IS NULL AND c.tunnel_seen_at > now() - $1::interval),
	` + agentNameSQL

// agent is the name db's server's agent goes by: the one a grant names and
// its tunnel is found under.
func (d Database) agent() string {
	if d.AgentHost != "" {
		return d.AgentHost
	}
	return d.Hostname
}

func scanDatabase(row pgx.Row) (Database, error) {
	var d Database
	var policy []byte
	if err := row.Scan(&d.ID, &d.AssetID, &d.Hostname, &d.Engine, &d.Host, &d.Port, &d.PoolerHost, &d.PoolerPort, &d.ConfigPath, &d.Name,
		&d.AdminUser, &d.Label, &policy, &d.CreatedBy, &d.CreatedAt, &d.TunnelConnected, &d.AgentHost); err != nil {
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
	err := s.db(ctx).QueryRow(ctx, `
		INSERT INTO databases (asset_id, engine, host, port, dbname, admin_user, label, policy, created_by,
			pooler_host, pooler_port, config_path)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (asset_id, port, dbname) DO UPDATE SET host = EXCLUDED.host,
			pooler_host = EXCLUDED.pooler_host, pooler_port = EXCLUDED.pooler_port,
			config_path = EXCLUDED.config_path
		WHERE databases.engine = EXCLUDED.engine
		RETURNING id::text`,
		d.AssetID, d.Engine, d.Host, d.Port, d.Name, d.AdminUser, d.Label, policy, actor,
		d.PoolerHost, d.PoolerPort, d.ConfigPath).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// The conflict was with an entry of another engine on that port.
		return d, ErrInvalidDatabase{fmt.Sprintf("port %d on this server already has an entry named %s "+
			"of another engine", d.Port, d.Name)}
	}
	if err != nil {
		if strings.Contains(err.Error(), "databases_redis_server_key") {
			return d, ErrInvalidDatabase{fmt.Sprintf("this server already has its Redis on port %d; "+
				"one entry covers every logical db, as Redis's own users do", d.Port)}
		}
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
	tag, err := s.db(ctx).Exec(ctx, `UPDATE databases SET host = $2, port = $3, admin_user = $4,
		label = $5, policy = $6, pooler_host = $7, pooler_port = $8, config_path = $9 WHERE id = $1::uuid`,
		d.ID, d.Host, d.Port, d.AdminUser, d.Label, policy, d.PoolerHost, d.PoolerPort, d.ConfigPath)
	if err != nil {
		if strings.Contains(err.Error(), "databases_redis_server_key") {
			return d, ErrInvalidDatabase{fmt.Sprintf("this server already has its Redis on port %d", d.Port)}
		}
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
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM databases WHERE id = $1::uuid`, id)
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
	d, err := scanDatabase(s.db(ctx).QueryRow(ctx, `SELECT `+databaseCols+`
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
	rows, err := s.db(ctx).Query(ctx, q+` ORDER BY a.hostname, d.dbname`, args...)
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
	rows, err := s.db(ctx).Query(ctx, `
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
		err := s.db(ctx).QueryRow(ctx, `
			SELECT database_level, expires_at FROM access_requests
			WHERE requester_email = $1 AND database_id = $2::uuid AND state = 'approved'
			  AND database_level IS NOT NULL
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
	rows, err := s.db(ctx).Query(ctx, `
		SELECT g.id::text, g.database_id::text, g.team_id::text, coalesce(t.name, ''), g.user_email,
		       g.level, g.needs_approval, g.granted_by, g.granted_at, g.rule_id::text
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
			&g.NeedsApproval, &g.GrantedBy, &g.GrantedAt, &g.RuleID); err != nil {
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
		_, err := s.db(ctx).Exec(ctx, `
			INSERT INTO database_assignments (database_id, team_id, level, needs_approval, granted_by)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5)
			ON CONFLICT (database_id, team_id) WHERE team_id IS NOT NULL
			DO UPDATE SET level = EXCLUDED.level, needs_approval = EXCLUDED.needs_approval,
			              granted_by = EXCLUDED.granted_by, granted_at = now(), rule_id = NULL`,
			g.DatabaseID, *g.TeamID, g.Level, g.NeedsApproval, actor)
		return dbAssignErr(err)
	case g.UserEmail != nil && g.TeamID == nil:
		email := strings.ToLower(strings.TrimSpace(*g.UserEmail))
		if email == "" {
			return ErrInvalidDatabase{"a person's assignment needs their email"}
		}
		_, err := s.db(ctx).Exec(ctx, `
			INSERT INTO database_assignments (database_id, user_email, level, needs_approval, granted_by)
			VALUES ($1::uuid, $2, $3, $4, $5)
			ON CONFLICT (database_id, user_email) WHERE user_email IS NOT NULL
			DO UPDATE SET level = EXCLUDED.level, needs_approval = EXCLUDED.needs_approval,
			              granted_by = EXCLUDED.granted_by, granted_at = now(), rule_id = NULL`,
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
	tag, err := s.db(ctx).Exec(ctx, `DELETE FROM database_assignments WHERE id = $1::uuid AND database_id = $2::uuid`,
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
		Principal: "db:" + level, Justification: justification, DurationMinutes: minutes, Broker: d.Broker()}
	err = s.db(ctx).QueryRow(ctx, `
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

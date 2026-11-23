package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Making a new database, vhost or topic on this server, for the console's
Create new. See tunnel/dbcreate.go.

Each engine is reached the way discover reaches it, as its administrator:
Postgres through the pg-admin helper as the postgres account, MySQL as root
over its socket or the entry's option file, MongoDB and Kafka with their
admin files, RabbitMQ through rabbitmqctl. The agent looks first and never
runs over a name that is there; the statements are tunnel.CreatePlan's, so
the console's preview is what runs.
*/

// CreateDBService is the tunnel service name; CreateDBOwnedService is the
// same service for a create that names an owner user (tunnel/dbowner.go).
const (
	CreateDBService      = tunnel.CreateService
	CreateDBOwnedService = tunnel.CreateOwnedService
)

// CreateDBConfig is the create service. Its fields are DiscoverDBConfig's.
type CreateDBConfig = DiscoverDBConfig

const createTimeout = 60 * time.Second

// CreateDatabases is the service that makes databases, vhosts and topics.
func CreateDatabases(cfg CreateDBConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.Admin == nil {
		cfg.Admin = helperAdmin(PostgresConfig{AdminUser: cfg.AdminUser, Helper: cfg.Helper,
			Spawn: cfg.Spawn, HelperEnv: cfg.HelperEnv})
	}
	if cfg.Ctl == nil {
		cfg.Ctl = runRabbitmqctl
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a create", "error", err)
			call.Refuse(err.Error())
			return
		}
		if (!g.ForService(CreateDBService) && !g.ForService(CreateDBOwnedService)) || g.Create == nil ||
			g.Service != tunnel.CreateServiceFor(*g.Create) {
			log.Warn("tunnel: refused a create on a grant for another service", "service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for creating databases"}.Error())
			return
		}
		c := *g.Create
		if err := c.Validate(); err != nil {
			call.Refuse(err.Error())
			return
		}
		if err := call.Accept(""); err != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, createTimeout)
		var res tunnel.CreateResult
		pw := ""
		if c.NeedsPassword() {
			p, err := readCreatePassword(cctx, call.Stream)
			if err == nil {
				if why := tunnel.OwnerPasswordProblem(c, p); why != "" {
					err = errors.New(why)
				}
			}
			if err != nil {
				res.Error = err.Error()
			}
			pw = p
		}
		if res.Error == "" {
			res = createOn(cctx, cfg, c, g.Email, pw)
		}
		pw = ""
		cancel()
		log.Info("tunnel: create", "op", c.Op, "engine", c.Engine, "name", c.Name, "port", c.Port,
			"ok", res.OK, "exists", res.Exists, "error", res.Error, "for", g.Email)
		b, _ := json.Marshal(res)
		_ = tunnel.WriteFrame(call.Stream, tunnel.FrameCreateResult, b)
		_ = call.Stream.Close()
	}
}

func createOn(ctx context.Context, cfg CreateDBConfig, c tunnel.GrantCreate, email, pw string) tunnel.CreateResult {
	switch c.Engine {
	case tunnel.EnginePostgres:
		return createPostgres(ctx, cfg, c, email, pw)
	case tunnel.EngineMySQL:
		socks := cfg.MySQLSockets
		if socks == nil {
			socks = mysqlSockets
		}
		if c.Op == tunnel.CreateOpSetOwner || c.Op == tunnel.CreateOpSetPassword {
			return ownMySQL(ctx, socks, c, pw)
		}
		return createMySQL(ctx, socks, c, pw)
	case tunnel.EngineMongoDB:
		if c.Op == tunnel.CreateOpSetOwner || c.Op == tunnel.CreateOpSetPassword {
			return ownMongo(ctx, c, pw)
		}
		return createMongo(ctx, c, pw)
	case tunnel.EngineRabbitMQ:
		return createRabbit(ctx, cfg.Ctl, c)
	case tunnel.EngineKafka:
		return createKafka(ctx, c)
	case tunnel.EngineMSSQL:
		if c.Op == tunnel.CreateOpSetOwner || c.Op == tunnel.CreateOpSetPassword {
			return ownMSSQL(ctx, c, pw)
		}
		return createMSSQL(ctx, c, pw)
	}
	return tunnel.CreateResult{Error: "Argus cannot create on engine " + c.Engine}
}

// createMSSQL makes a SQL Server database as the admin login. The person's
// logins and users are made when someone first opens it, like any entry.
func createMSSQL(ctx context.Context, c tunnel.GrantCreate, pw string) tunnel.CreateResult {
	admin, err := mssqlAdminConn(ctx, c.Host, c.Port, c.ConfigPath)
	if err != nil {
		return tunnel.CreateResult{Error: err.Error()}
	}
	defer admin.close()
	res := tunnel.CreateResult{}
	_ = admin.db.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(64))`).Scan(&res.Version)
	if c.Op == tunnel.CreateOpOptions {
		o, err := mssqlCreateOptions(ctx, admin.db)
		if err != nil {
			res.Error = "SQL Server did not answer: " + mssqlMessage(err)
			return res
		}
		o.Version = res.Version
		o.Users = mssqlOwnerUsers(ctx, admin.db)
		res.OK, res.Options = true, o
		return res
	}
	var n int
	if err := admin.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.databases WHERE name = @p1`, c.Name).Scan(&n); err != nil {
		res.Error = "SQL Server did not answer: " + mssqlMessage(err)
		return res
	}
	if n > 0 {
		res.Exists, res.Error = true, "this server already has a database named "+c.Name
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The name " + c.Name + " is free", OK: true})
	if m := c.MSSQL; m != nil && m.Collation != "" {
		if err := admin.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.fn_helpcollations() WHERE name = @p1`, m.Collation).Scan(&n); err != nil || n == 0 {
			res.Error = "this server has no collation " + m.Collation
			return res
		}
	}
	made, ok := mssqlOwnerBefore(ctx, admin, c, pw, &res)
	if !ok {
		return res
	}
	plan := tunnel.CreateDBPlan(c)
	if _, err := admin.db.ExecContext(ctx, plan[0]); err != nil {
		res.Error = mssqlMessage(err)
		res.Exists = strings.Contains(res.Error, "already exists")
		if made {
			res.Steps = append(res.Steps, mssqlUndoLogin(ctx, admin, c.Owner.Name))
		}
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Created the database " + c.Name, OK: true})
	res.Statements = append(res.Statements, plan[0])
	if len(plan) > 1 {
		if _, err := admin.db.ExecContext(ctx, plan[1]); err != nil {
			// The database is there: a partial success, said as one.
			res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Set the recovery model", Error: mssqlMessage(err)})
			res.Error = mssqlMessage(err)
			return res
		}
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Set the recovery model to " + c.MSSQL.Recovery, OK: true})
		res.Statements = append(res.Statements, plan[1])
	}
	res.OK = mssqlOwnerAfter(ctx, admin, c, pw, &res)
	return res
}

func mssqlCreateOptions(ctx context.Context, db *sql.DB) (*tunnel.CreateOptions, error) {
	o := &tunnel.CreateOptions{}
	if err := db.QueryRowContext(ctx, `SELECT CAST(SERVERPROPERTY('Collation') AS nvarchar(128)),
	  (SELECT recovery_model_desc FROM sys.databases WHERE name = 'model')`).Scan(&o.DefaultCollation, &o.DefaultRecovery); err != nil {
		return nil, err
	}
	// The Windows collations of the current versions, case and accent
	// sensitive or not, and the SQL_ ones: what people pick from, not all
	// five thousand.
	rows, err := db.QueryContext(ctx, `SELECT name FROM sys.fn_helpcollations()
	  WHERE (name LIKE 'SQL[_]%' OR name LIKE '%[_]100[_]%' OR name LIKE '%[_]140[_]%' OR name LIKE 'Latin1_General_%')
	    AND name NOT LIKE '%[_]KS%' AND name NOT LIKE '%[_]WS%' AND name NOT LIKE '%[_]VSS%'
	  ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			o.Collations = append(o.Collations, n)
		}
	}
	_ = rows.Close()
	rows, err = db.QueryContext(ctx, `SELECT name FROM sys.databases ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if rows.Scan(&n) != nil || tunnel.IsMSSQLSystemDB(n) {
			continue
		}
		if len(o.Taken) == tunnel.MaxDiscovered {
			o.Truncated = true
			break
		}
		o.Taken = append(o.Taken, n)
	}
	return o, rows.Err()
}

func createPostgres(ctx context.Context, cfg CreateDBConfig, c tunnel.GrantCreate, email, pw string) tunnel.CreateResult {
	socket := socketDir(tunnel.GrantDB{Host: c.Host, Port: c.Port})
	l := pgLocator{root: cfg.PGRoot}
	bouncer, pooled := findBouncer(l, c.PoolerPort)
	verifier := ""
	if pw != "" {
		v, err := scramVerifier(pw)
		if err != nil {
			return tunnel.CreateResult{Error: err.Error()}
		}
		verifier = v
	}
	ask := func(op, database string) PGAdminResult {
		r := cfg.Admin(ctx, PGAdminRequest{Op: op, Database: database, Socket: socket, Port: c.Port,
			AdminUser: c.AdminUser, Owner: email, Create: &c, Verifier: verifier,
			Pooled: pooled && poolerAuth(bouncer) == "argus"})
		if !r.OK && strings.Contains(r.Error, "unknown operation") {
			r.Error = "this agent's database helper is older than the agent; restart the agent and try again"
		}
		return r
	}
	switch c.Op {
	case tunnel.CreateOpOptions:
		r := ask("create-options", "postgres")
		if r.Options != nil && pooled {
			r.Options.Pooler = poolerInfo(l, bouncer)
		}
		return tunnel.CreateResult{OK: r.OK, Error: r.Error, Version: r.Version, Options: r.Options}
	case tunnel.CreateOpSetPassword:
		r := ask("set-password", "postgres")
		res := tunnel.CreateResult{OK: r.OK, Error: r.Error, Version: r.Version, Steps: r.Steps}
		if r.OK {
			res.Statements = tunnel.SetPasswordPlan(c)
			res.Steps = append(res.Steps, pgOwnerSignIn(ctx, c, pw, bouncer, pooled, &res)...)
		}
		return res
	case tunnel.CreateOpSetOwner:
		r := ask("set-owner", "postgres")
		res := tunnel.CreateResult{OK: r.OK, Error: r.Error, Version: r.Version, Steps: r.Steps}
		if r.OK {
			res.Statements = tunnel.SetOwnerPlan(c)
			if pooled {
				steps, changed, manual := ensurePooler(l, bouncer, c, verifier)
				res.Steps, res.Statements, res.Manual = append(res.Steps, steps...), append(res.Statements, changed...), manual
			}
			res.Steps = append(res.Steps, pgOwnerSignIn(ctx, c, pw, bouncer, pooled, &res)...)
		}
		return res
	}
	r := ask("create-database", "postgres")
	res := tunnel.CreateResult{OK: r.OK, Error: r.Error, Version: r.Version, Exists: r.Exists, Steps: r.Steps}
	if !r.OK {
		return res
	}
	pre, _ := tunnel.OwnerPlan(c, tunnel.PasswordRedacted)
	plan := tunnel.CreateDBPlan(c)
	res.Statements = append(append([]string{}, pre...), plan[0])
	// The level roles and the extensions are made inside the new database,
	// a second sign-in.
	p := ask("prepare-new-database", c.Name)
	res.Steps = append(res.Steps, p.Steps...)
	for i, e := range c.Postgres.Extensions {
		for _, s := range p.Steps {
			if s.OK && s.Text == "Enabled "+e {
				res.Statements = append(res.Statements, plan[1+i])
			}
		}
	}
	if !p.OK {
		// The database is there: a partial success, said as one.
		if len(p.Steps) == 0 {
			res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Set up the new database", Error: p.Error})
		}
		res.Error = p.Error
	}
	if pooled {
		steps, changed, manual := ensurePooler(l, bouncer, c, verifier)
		res.Steps, res.Statements, res.Manual = append(res.Steps, steps...), append(res.Statements, changed...), manual
	}
	res.Steps = append(res.Steps, pgOwnerSignIn(ctx, c, pw, bouncer, pooled, &res)...)
	return res
}

// pgOwnerSignIn checks the owner signs in with its new password, straight
// to Postgres and through PgBouncer. A failure is said, not fatal: what was
// made stays.
func pgOwnerSignIn(ctx context.Context, c tunnel.GrantCreate, pw string, b pgBouncerSeen, pooled bool, res *tunnel.CreateResult) []tunnel.CreateStep {
	if pw == "" || c.Owner == nil {
		return nil
	}
	user := c.Owner.Name
	var steps []tunnel.CreateStep
	if err := pgSignIn(ctx, c.Port, user, pw, c.Name, 1); err != nil {
		steps = append(steps, tunnel.CreateStep{Text: fmt.Sprintf("Sign in as %s on 127.0.0.1:%d", user, c.Port),
			Error: signInText(err) + "; check pg_hba.conf allows password sign-in from this server"})
	} else {
		steps = append(steps, tunnel.CreateStep{OK: true, Text: fmt.Sprintf("%s signs in straight to Postgres (%d)", user, c.Port)})
	}
	if pooled && len(res.Manual) == 0 {
		if err := pgSignIn(ctx, b.Port, user, pw, c.Name, 4); err != nil {
			steps = append(steps, tunnel.CreateStep{Text: fmt.Sprintf("Sign in as %s through PgBouncer %d", user, b.Port), Error: signInText(err)})
		} else {
			res.Pooled = true
			steps = append(steps, tunnel.CreateStep{OK: true, Text: fmt.Sprintf("%s signs in through PgBouncer (%d)", user, b.Port)})
		}
	}
	return steps
}

func createMySQL(ctx context.Context, sockets []string, c tunnel.GrantCreate, pw string) tunnel.CreateResult {
	admin, err := adminConn(ctx, sockets, tunnel.GrantDB{Host: c.Host, Port: c.Port, ConfigPath: c.ConfigPath})
	if err != nil {
		return tunnel.CreateResult{Error: err.Error()}
	}
	defer admin.close()
	res := tunnel.CreateResult{Version: admin.version}
	if c.Op == tunnel.CreateOpOptions {
		o, err := mysqlCreateOptions(ctx, admin)
		if err != nil {
			res.Error = "MySQL did not answer: " + mysqlMessage(err)
			return res
		}
		o.Version = admin.version
		o.Users = mysqlOwnerUsers(ctx, admin)
		res.OK, res.Options = true, o
		return res
	}
	var n int
	if err := admin.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?`, c.Name).Scan(&n); err != nil {
		res.Error = "MySQL did not answer: " + mysqlMessage(err)
		return res
	}
	if n > 0 {
		res.Exists, res.Error = true, "this server already has a database named "+c.Name
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The name " + c.Name + " is free", OK: true})
	if m := c.MySQL; m.Collation != "" || m.Charset != "" {
		q, args := `SELECT COUNT(*) FROM information_schema.COLLATIONS WHERE CHARACTER_SET_NAME = ?`, []any{m.Charset}
		if m.Charset == "" {
			q, args = `SELECT COUNT(*) FROM information_schema.COLLATIONS WHERE COLLATION_NAME = ?`, []any{m.Collation}
		} else if m.Collation != "" {
			q += ` AND COLLATION_NAME = ?`
			args = append(args, m.Collation)
		}
		if err := admin.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil || n == 0 {
			res.Error = "this server has no " + strings.Trim(m.Charset+" "+m.Collation, " ")
			return res
		}
	}
	made, ok := mysqlOwnerBefore(ctx, admin, c, pw, &res)
	if !ok {
		return res
	}
	plan := tunnel.CreateDBPlan(c)
	if _, err := admin.db.ExecContext(ctx, plan[0]); err != nil {
		res.Error = mysqlMessage(err)
		res.Exists = strings.Contains(res.Error, "exists")
		if made {
			res.Steps = append(res.Steps, mysqlUndoUser(ctx, admin, c.Owner))
		}
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Created the database " + c.Name, OK: true})
	res.Statements = append(res.Statements, plan...)
	res.OK = mysqlOwnerAfter(ctx, admin, c, pw, &res)
	return res
}

func mysqlCreateOptions(ctx context.Context, admin *mysqlAdmin) (*tunnel.CreateOptions, error) {
	o := &tunnel.CreateOptions{Charsets: map[string][]string{}}
	rows, err := admin.db.QueryContext(ctx, `SELECT CHARACTER_SET_NAME, COLLATION_NAME FROM information_schema.COLLATIONS
		WHERE CHARACTER_SET_NAME IS NOT NULL ORDER BY CHARACTER_SET_NAME, COLLATION_NAME LIMIT 2000`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cs, co string
		if rows.Scan(&cs, &co) == nil {
			o.Charsets[cs] = append(o.Charsets[cs], co)
		}
	}
	_ = rows.Close()
	_ = admin.db.QueryRowContext(ctx, `SELECT @@character_set_server, @@collation_server`).Scan(&o.DefaultCharset, &o.DefaultCollation)
	rows, err = admin.db.QueryContext(ctx, `SELECT SCHEMA_NAME FROM information_schema.SCHEMATA ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if rows.Scan(&n) != nil || tunnel.IsMySQLSystemDB(n) {
			continue
		}
		if len(o.Taken) == tunnel.MaxDiscovered {
			o.Truncated = true
			break
		}
		o.Taken = append(o.Taken, n)
	}
	return o, nil
}

func createMongo(ctx context.Context, c tunnel.GrantCreate, pw string) tunnel.CreateResult {
	s := &mongoSession{host: c.Host, port: c.Port, file: c.ConfigPath}
	if s.file == "" {
		s.file = tunnel.MongoAdminFile
	}
	admin, err := s.adminClient(ctx)
	if err != nil {
		return tunnel.CreateResult{Error: err.Error(), Version: s.version}
	}
	defer func() { _ = admin.Disconnect(context.WithoutCancel(ctx)) }()
	res := tunnel.CreateResult{Version: s.version}
	var out struct {
		Databases []struct {
			Name string `bson:"name"`
		} `bson:"databases"`
	}
	if err := admin.Database("admin").RunCommand(ctx, bson.D{{Key: "listDatabases", Value: 1}, {Key: "nameOnly", Value: true}}).Decode(&out); err != nil {
		res.Error = "MongoDB would not list its databases: " + err.Error()
		return res
	}
	if c.Op == tunnel.CreateOpOptions {
		o := &tunnel.CreateOptions{Version: s.version}
		for _, d := range out.Databases {
			if !tunnel.IsMongoSystemDB(d.Name) {
				o.Taken = append(o.Taken, d.Name)
			}
		}
		o.Users = mongoOwnerUsers(ctx, admin)
		res.OK, res.Options = true, o
		return res
	}
	for _, d := range out.Databases {
		if d.Name == c.Name {
			res.Exists, res.Error = true, "this server already has a database named "+c.Name
			return res
		}
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The name " + c.Name + " is free", OK: true})
	if !mongoOwnerBefore(ctx, admin, c, &res) {
		return res
	}
	m := c.Mongo
	cmd := bson.D{{Key: "create", Value: m.Collection}}
	switch m.Kind {
	case "timeseries":
		cmd = append(cmd, bson.E{Key: "timeseries", Value: bson.D{{Key: "timeField", Value: m.TimeField}}})
		if m.ExpireAfterS > 0 {
			cmd = append(cmd, bson.E{Key: "expireAfterSeconds", Value: m.ExpireAfterS})
		}
	case "capped":
		cmd = append(cmd, bson.E{Key: "capped", Value: true}, bson.E{Key: "size", Value: m.CappedBytes})
	}
	if err := admin.Database(c.Name).RunCommand(ctx, cmd).Err(); err != nil {
		res.Error = "MongoDB refused: " + err.Error()
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Created " + c.Name + " with the collection " + m.Collection, OK: true})
	res.Statements = tunnel.CreateDBPlan(c)
	res.OK = mongoOwnerAfter(ctx, admin, c, pw, &res)
	return res
}

func createRabbit(ctx context.Context, ctl func(context.Context, []string, string) ([]byte, error), c tunnel.GrantCreate) tunnel.CreateResult {
	found := discoverRabbit(ctx, ctl)
	if !found.OK {
		return tunnel.CreateResult{Error: found.Error}
	}
	if c.Op == tunnel.CreateOpOptions {
		o := &tunnel.CreateOptions{Truncated: found.Truncated}
		for _, d := range found.Databases {
			o.Taken = append(o.Taken, d.Name)
		}
		return tunnel.CreateResult{OK: true, Options: o}
	}
	res := tunnel.CreateResult{}
	for _, d := range found.Databases {
		if d.Name == c.Name {
			res.Exists, res.Error = true, "this broker already has a vhost named "+c.Name
			return res
		}
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The vhost " + c.Name + " is free", OK: true})
	r := c.Rabbit
	args := []string{"add_vhost", c.Name}
	if r.Description != "" {
		args = append(args, "--description", r.Description)
	}
	if r.QueueType != "" {
		args = append(args, "--default-queue-type", r.QueueType)
	}
	if _, err := ctl(ctx, args, ""); err != nil {
		res.Error = err.Error()
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Created the vhost " + c.Name, OK: true})
	plan := tunnel.CreateDBPlan(c)
	res.OK, res.Statements = true, plan[:1]
	if l := r.LimitsJSON(); l != "" {
		if _, err := ctl(ctx, []string{"set_vhost_limits", "-p", c.Name, l}, ""); err != nil {
			res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Set its limits", Error: err.Error()})
			res.Error = "the vhost was made, but its limits were not set: " + err.Error()
			return res
		}
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Set its limits", OK: true})
		res.Statements = plan
	}
	return res
}

func createKafka(ctx context.Context, c tunnel.GrantCreate) tunnel.CreateResult {
	s := &kafkaSession{addr: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), file: c.ConfigPath}
	if s.file == "" {
		s.file = tunnel.KafkaAdminFile
	}
	cl, err := s.adminClient(ctx)
	if err != nil {
		return tunnel.CreateResult{Error: err.Error()}
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	td, err := adm.ListTopics(ctx)
	if err != nil {
		return tunnel.CreateResult{Error: "Kafka would not list its topics: " + kafkaErrText(err)}
	}
	if c.Op == tunnel.CreateOpOptions {
		o := &tunnel.CreateOptions{}
		if b, err := adm.ListBrokers(ctx); err == nil {
			o.Brokers = len(b)
		}
		for _, t := range td.Sorted() {
			if t.IsInternal {
				continue
			}
			if len(o.Taken) == 1000 {
				o.Truncated = true
				break
			}
			o.Taken = append(o.Taken, t.Topic)
		}
		sort.Strings(o.Taken)
		return tunnel.CreateResult{OK: true, Options: o}
	}
	res := tunnel.CreateResult{}
	if td.Has(c.Name) {
		res.Exists, res.Error = true, "this cluster already has a topic named "+c.Name
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The topic " + c.Name + " is free", OK: true})
	k := c.Kafka
	repl := int16(-1)
	if k.Replicas > 0 {
		repl = int16(k.Replicas)
	}
	r, err := adm.CreateTopic(ctx, int32(k.Partitions), repl, k.Configs(), c.Name)
	if err == nil {
		err = r.Err
	}
	if err != nil {
		res.Error = fmt.Sprintf("Kafka refused: %s", kafkaErrText(err))
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Created the topic " + c.Name, OK: true})
	res.OK, res.Statements = true, tunnel.CreateDBPlan(c)
	return res
}

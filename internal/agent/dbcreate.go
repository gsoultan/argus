package agent

import (
	"context"
	"encoding/json"
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

// CreateDBService is the tunnel service name.
const CreateDBService = tunnel.CreateService

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
		if !g.ForService(CreateDBService) || g.Create == nil {
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
		res := createOn(cctx, cfg, c, g.Email)
		cancel()
		log.Info("tunnel: create", "op", c.Op, "engine", c.Engine, "name", c.Name, "port", c.Port,
			"ok", res.OK, "exists", res.Exists, "error", res.Error, "for", g.Email)
		b, _ := json.Marshal(res)
		_ = tunnel.WriteFrame(call.Stream, tunnel.FrameCreateResult, b)
		_ = call.Stream.Close()
	}
}

func createOn(ctx context.Context, cfg CreateDBConfig, c tunnel.GrantCreate, email string) tunnel.CreateResult {
	switch c.Engine {
	case tunnel.EnginePostgres:
		return createPostgres(ctx, cfg, c, email)
	case tunnel.EngineMySQL:
		socks := cfg.MySQLSockets
		if socks == nil {
			socks = mysqlSockets
		}
		return createMySQL(ctx, socks, c)
	case tunnel.EngineMongoDB:
		return createMongo(ctx, c)
	case tunnel.EngineRabbitMQ:
		return createRabbit(ctx, cfg.Ctl, c)
	case tunnel.EngineKafka:
		return createKafka(ctx, c)
	}
	return tunnel.CreateResult{Error: "Argus cannot create on engine " + c.Engine}
}

func createPostgres(ctx context.Context, cfg CreateDBConfig, c tunnel.GrantCreate, email string) tunnel.CreateResult {
	socket := socketDir(tunnel.GrantDB{Host: c.Host, Port: c.Port})
	ask := func(op, database string) PGAdminResult {
		r := cfg.Admin(ctx, PGAdminRequest{Op: op, Database: database, Socket: socket, Port: c.Port,
			AdminUser: c.AdminUser, Owner: email, Create: &c})
		if !r.OK && strings.Contains(r.Error, "unknown operation") {
			r.Error = "this agent's database helper is older than the agent; restart the agent and try again"
		}
		return r
	}
	switch c.Op {
	case tunnel.CreateOpOptions:
		r := ask("create-options", "postgres")
		return tunnel.CreateResult{OK: r.OK, Error: r.Error, Version: r.Version, Options: r.Options}
	case tunnel.CreateOpSetOwner:
		r := ask("set-owner", "postgres")
		res := tunnel.CreateResult{OK: r.OK, Error: r.Error, Version: r.Version, Steps: r.Steps}
		if r.OK {
			res.Statements = tunnel.SetOwnerPlan(c)
		}
		return res
	}
	r := ask("create-database", "postgres")
	res := tunnel.CreateResult{OK: r.OK, Error: r.Error, Version: r.Version, Exists: r.Exists, Steps: r.Steps}
	if !r.OK {
		return res
	}
	plan := tunnel.CreatePlan(c)
	res.Statements = plan[:1]
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
	return res
}

func createMySQL(ctx context.Context, sockets []string, c tunnel.GrantCreate) tunnel.CreateResult {
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
	plan := tunnel.CreatePlan(c)
	if _, err := admin.db.ExecContext(ctx, plan[0]); err != nil {
		res.Error = mysqlMessage(err)
		res.Exists = strings.Contains(res.Error, "exists")
		return res
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Created the database " + c.Name, OK: true})
	res.OK, res.Statements = true, plan
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

func createMongo(ctx context.Context, c tunnel.GrantCreate) tunnel.CreateResult {
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
	res.OK, res.Statements = true, tunnel.CreatePlan(c)
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
	plan := tunnel.CreatePlan(c)
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
	res.OK, res.Statements = true, tunnel.CreatePlan(c)
	return res
}

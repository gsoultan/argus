package tunnel

import (
	"errors"
	"fmt"
)

/*
The discover service: what databases a Postgres, Redis or RabbitMQ on this
server holds, so an admin adding one picks it from a list instead of typing
its name.

	argus-control ──"dbdiscover" stream──▶ agent ──▶ pg-admin list / Redis INFO / rabbitmqctl

One stream per question. After the header and the answer the agent sends one
'z' DiscoverResult (JSON) and closes. Nothing is created, granted or changed:
the agent signs in the way it already does for a session (peer
authentication, the redis.conf's requirepass, rabbitmqctl) and reads names.
An old agent refuses the service as unknown, which the console turns into
"type the name".
*/

// DiscoverService is the tunnel service name.
const DiscoverService = "dbdiscover"

// FrameDiscoverResult carries the one DiscoverResult.
const FrameDiscoverResult byte = 'z'

// MaxDiscovered bounds what one answer lists. A server with more databases
// than this is listed by its first MaxDiscovered names, and Truncated says so.
const MaxDiscovered = 500

// GrantDiscover is what a discover grant may look at; nil on every other
// grant.
type GrantDiscover struct {
	Engine string `json:"engine"`
	// Host and Port are where the engine listens on this server, as a
	// database entry would name them.
	Host string `json:"host"`
	Port int    `json:"port"`
	// AdminUser is the account Postgres's peer authentication signs in as;
	// empty is "postgres".
	AdminUser string `json:"adminUser,omitempty"`
	// ConfigPath is a Redis's redis.conf, where the agent reads requirepass,
	// or a MySQL option file the agent signs in with.
	ConfigPath string `json:"configPath,omitempty"`
}

// Validate refuses a grant that points anywhere but this server.
func (d *GrantDiscover) Validate() error {
	if d.Port < 1 || d.Port > 65535 {
		return errors.New("the port must be between 1 and 65535")
	}
	switch d.Engine {
	case EnginePostgres:
		// Another server only behind this server's PgBouncer; the agent
		// checks its pgbouncer.ini.
		if !LocalPGHost(d.Host) && !RemotePGHost(d.Host) {
			return fmt.Errorf("Postgres must be on this server or behind its PgBouncer; %s is not", d.Host)
		}
	case EngineRedis:
		if !LocalPGHost(d.Host) {
			return fmt.Errorf("Redis must be on this server; %s is not", d.Host)
		}
	case EngineMongoDB:
		if !LocalPGHost(d.Host) {
			return fmt.Errorf("MongoDB must be on this server; %s is not", d.Host)
		}
		if d.ConfigPath != "" && !ValidMongoAdminFile(d.ConfigPath) {
			return errors.New("the admin file must be an absolute path to a .conf file on the server")
		}
	case EngineKafka:
		if !LocalLoopback(d.Host) {
			return fmt.Errorf("Kafka must be on this server; %s is not", d.Host)
		}
		if d.ConfigPath != "" && !ValidKafkaAdminFile(d.ConfigPath) {
			return errors.New("the admin file must be an absolute path to a .conf file on the server")
		}
	case EngineRabbitMQ:
		if !LocalLoopback(d.Host) {
			return fmt.Errorf("the RabbitMQ management API must be on this server; %s is not", d.Host)
		}
	case EngineMySQL:
		if !LocalPGHost(d.Host) {
			return fmt.Errorf("MySQL must be on this server; %s is not", d.Host)
		}
		if d.ConfigPath != "" && !ValidMySQLOptionFile(d.ConfigPath) {
			return errors.New("the option file must be an absolute path to a .cnf file")
		}
	default:
		return errors.New("unknown engine " + d.Engine)
	}
	return nil
}

// Discovered is one database, logical db or vhost found on the server.
type Discovered struct {
	Name string `json:"name"`
	// Owner is a Postgres database's owner role.
	Owner string `json:"owner,omitempty"`
	// Bytes is a Postgres database's size on disk.
	Bytes int64 `json:"bytes,omitempty"`
	// Keys is how many keys a Redis logical db holds.
	Keys int64 `json:"keys,omitempty"`
}

// DiscoverResult is the agent's answer.
type DiscoverResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
	// Server is StoreAnswer.Server: "valkey" for a Valkey.
	Server string `json:"server,omitempty"`
	// Databases is what was found, in the engine's order: by name, or by
	// Redis db number.
	Databases []Discovered `json:"databases,omitempty"`
	// Configured is a Redis's number of logical dbs (CONFIG GET databases).
	Configured int  `json:"configured,omitempty"`
	Truncated  bool `json:"truncated,omitempty"`
	// Host and Port are where a Postgres listing signed in, when that is
	// not where it was asked: the agent read the server's own
	// configuration and found Postgres elsewhere.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Places are the other Postgres servers found on this server, for
	// picking another cluster.
	Places []DiscoverPlace `json:"places,omitempty"`
	// Poolers are the PgBouncers configured on this server and where they
	// send each database, which names the Postgres server when it is not
	// this one.
	Poolers []PoolerSeen `json:"poolers,omitempty"`
	// Tried is every way in a Postgres listing tried, each with why it
	// failed, when none answered.
	Tried []string `json:"tried,omitempty"`
}

// DiscoverPlace is one Postgres found on the server.
type DiscoverPlace struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Label names it: "16/main" for a Debian cluster, "pid 812".
	Label string `json:"label,omitempty"`
}

// PoolerSeen is one PgBouncer's configuration.
type PoolerSeen struct {
	Config  string         `json:"config"`
	Port    int            `json:"port"`
	Targets []PoolerTarget `json:"targets,omitempty"`
}

// PoolerTarget is one line of a pgbouncer.ini's [databases]: the name
// clients ask for, and the Postgres it is sent to. An empty Host is
// PgBouncer's own socket on this server; Name "*" is its fallback for any
// name.
type PoolerTarget struct {
	Name   string `json:"name"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port"`
	DBName string `json:"dbname,omitempty"`
}

// Local says whether the target is on the server PgBouncer runs on.
func (t PoolerTarget) Local() bool {
	return t.Host == "" || LocalPGHost(t.Host)
}

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
	// ConfigPath is a Redis's redis.conf, where the agent reads requirepass.
	ConfigPath string `json:"configPath,omitempty"`
}

// Validate refuses a grant that points anywhere but this server.
func (d *GrantDiscover) Validate() error {
	if d.Port < 1 || d.Port > 65535 {
		return errors.New("the port must be between 1 and 65535")
	}
	switch d.Engine {
	case EnginePostgres:
		if !LocalPGHost(d.Host) {
			return fmt.Errorf("Postgres must be on this server; %s is not", d.Host)
		}
	case EngineRedis:
		if !LocalPGHost(d.Host) {
			return fmt.Errorf("Redis must be on this server; %s is not", d.Host)
		}
	case EngineRabbitMQ:
		if !LocalLoopback(d.Host) {
			return fmt.Errorf("the RabbitMQ management API must be on this server; %s is not", d.Host)
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
	// Databases is what was found, in the engine's order: by name, or by
	// Redis db number.
	Databases []Discovered `json:"databases,omitempty"`
	// Configured is a Redis's number of logical dbs (CONFIG GET databases).
	Configured int  `json:"configured,omitempty"`
	Truncated  bool `json:"truncated,omitempty"`
}

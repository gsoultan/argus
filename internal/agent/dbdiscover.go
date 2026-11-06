package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
What databases a Postgres, Redis, RabbitMQ or MySQL on this server holds, for the
console's add form. See tunnel/discover.go.

Each engine is asked the way the agent already signs in to it, and only
read: Postgres through the pg-admin helper as the postgres account (peer
authentication), Redis as its administrator with the redis.conf's
requirepass, RabbitMQ through rabbitmqctl, MySQL as root over its socket or
with the entry's option file. Nothing is created and no
password leaves this process.
*/

// DiscoverDBService is the tunnel service name.
const DiscoverDBService = tunnel.DiscoverService

// DiscoverDBConfig is the discover service.
type DiscoverDBConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// AdminUser, Helper, Spawn and HelperEnv are as for postgres.
	AdminUser string
	Helper    string
	Spawn     SpawnMode
	HelperEnv []string
	// Admin runs pg-admin operations; nil runs the helper as AdminUser.
	Admin func(context.Context, PGAdminRequest) PGAdminResult
	// Ctl runs rabbitmqctl; nil runs the one installed.
	Ctl func(ctx context.Context, args []string, stdin string) ([]byte, error)
	// Redis asks a Redis; nil dials it. Tests replace it.
	Redis func(ctx context.Context, d tunnel.GrantDiscover) tunnel.DiscoverResult
	// MySQLSockets replaces where MySQL's socket is looked for; tests.
	MySQLSockets []string
}

const discoverTimeout = 20 * time.Second

// DiscoverDatabases is the service that lists a server's databases.
func DiscoverDatabases(cfg DiscoverDBConfig) TunnelService {
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
	if cfg.Redis == nil {
		cfg.Redis = discoverRedis
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a database listing", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(DiscoverDBService) || g.Discover == nil {
			log.Warn("tunnel: refused a database listing on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for listing databases"}.Error())
			return
		}
		d := *g.Discover
		if err := d.Validate(); err != nil {
			call.Refuse(err.Error())
			return
		}
		if err := call.Accept(""); err != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, discoverTimeout)
		res := discover(cctx, cfg, d, g.Email)
		cancel()
		log.Info("tunnel: listed databases", "engine", d.Engine, "port", d.Port,
			"found", len(res.Databases), "ok", res.OK, "for", g.Email)
		b, _ := json.Marshal(res)
		_ = tunnel.WriteFrame(call.Stream, tunnel.FrameDiscoverResult, b)
		_ = call.Stream.Close()
	}
}

func discover(ctx context.Context, cfg DiscoverDBConfig, d tunnel.GrantDiscover, email string) tunnel.DiscoverResult {
	switch d.Engine {
	case tunnel.EnginePostgres:
		r := cfg.Admin(ctx, PGAdminRequest{Op: "discover-databases", Database: "postgres",
			Socket: socketDir(tunnel.GrantDB{Host: d.Host, Port: d.Port}), Port: d.Port,
			AdminUser: d.AdminUser, Owner: email})
		if !r.OK {
			msg := r.Error
			if strings.Contains(msg, "unknown operation") {
				msg = "this agent's database helper is older than the agent; restart the agent and try again"
			}
			return tunnel.DiscoverResult{Error: msg, Version: r.Version}
		}
		return tunnel.DiscoverResult{OK: true, Version: r.Version, Databases: r.Found, Truncated: r.Truncated}
	case tunnel.EngineRedis:
		return cfg.Redis(ctx, d)
	case tunnel.EngineRabbitMQ:
		return discoverRabbit(ctx, cfg.Ctl)
	case tunnel.EngineMySQL:
		socks := cfg.MySQLSockets
		if socks == nil {
			socks = mysqlSockets
		}
		return discoverMySQL(ctx, socks, d)
	}
	return tunnel.DiscoverResult{Error: "unknown engine " + d.Engine}
}

// discoverRedis lists a Redis's logical dbs: every one up to CONFIG GET
// databases, with the keys INFO keyspace counts in it.
func discoverRedis(ctx context.Context, d tunnel.GrantDiscover) tunnel.DiscoverResult {
	conf := redisConf(d.ConfigPath)
	s := &redisSession{db: tunnel.GrantDB{Host: d.Host, Port: d.Port}, host: d.Host, port: d.Port, conf: conf}
	admin, err := s.adminConn(ctx)
	if err != nil {
		return tunnel.DiscoverResult{Error: err.Error()}
	}
	defer admin.close()
	lim := respLimit{items: 64, budget: 1 << 20}
	info, err := admin.do(ctx, 10*time.Second, lim, "INFO", "server")
	if err != nil {
		return tunnel.DiscoverResult{Error: "Redis did not answer: " + err.Error()}
	}
	server, version, _ := redisServerOf(info.V)
	res := tunnel.DiscoverResult{OK: true, Version: version, Server: server}
	if v, err := admin.do(ctx, 10*time.Second, lim, "CONFIG", "GET", "databases"); err == nil && v.T == "a" && len(v.A) == 2 {
		res.Configured, _ = strconv.Atoi(v.A[1].V)
	}
	keys := map[int]int64{}
	if ks, err := admin.do(ctx, 10*time.Second, lim, "INFO", "keyspace"); err == nil {
		keys = parseKeyspace(ks.V)
	}
	n := res.Configured
	if n <= 0 {
		// CONFIG can be renamed away; the keyspace still names every db
		// that holds a key, and 16 is Redis's default.
		n = 16
		for i := range keys {
			if i+1 > n {
				n = i + 1
			}
		}
	}
	if n > tunnel.MaxDiscovered {
		n, res.Truncated = tunnel.MaxDiscovered, true
	}
	for i := 0; i < n; i++ {
		res.Databases = append(res.Databases, tunnel.Discovered{Name: strconv.Itoa(i), Keys: keys[i]})
	}
	return res
}

// parseKeyspace reads INFO keyspace: "db0:keys=12,expires=0,avg_ttl=0".
func parseKeyspace(info string) map[int]int64 {
	out := map[int]int64{}
	for _, l := range strings.Split(info, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if !ok || !strings.HasPrefix(k, "db") {
			continue
		}
		i, err := strconv.Atoi(k[2:])
		if err != nil || i < 0 {
			continue
		}
		for _, f := range strings.Split(v, ",") {
			if n, ok := strings.CutPrefix(f, "keys="); ok {
				out[i], _ = strconv.ParseInt(n, 10, 64)
			}
		}
	}
	return out
}

// discoverRabbit lists the broker's vhosts with rabbitmqctl, which signs in
// with the node's Erlang cookie and needs no user.
func discoverRabbit(ctx context.Context, ctl func(context.Context, []string, string) ([]byte, error)) tunnel.DiscoverResult {
	out, err := ctl(ctx, []string{"list_vhosts", "--silent", "--formatter", "json", "name"}, "")
	if err != nil {
		return tunnel.DiscoverResult{Error: err.Error()}
	}
	var vhosts []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &vhosts); err != nil {
		return tunnel.DiscoverResult{Error: fmt.Errorf("rabbitmqctl's answer could not be read: %w", err).Error()}
	}
	res := tunnel.DiscoverResult{OK: true}
	for _, v := range vhosts {
		if !tunnel.ValidVhost(v.Name) {
			continue
		}
		if len(res.Databases) == tunnel.MaxDiscovered {
			res.Truncated = true
			break
		}
		res.Databases = append(res.Databases, tunnel.Discovered{Name: v.Name})
	}
	sort.Slice(res.Databases, func(i, j int) bool { return res.Databases[i].Name < res.Databases[j].Name })
	return res
}

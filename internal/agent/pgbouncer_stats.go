package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/caduceus/wire"
)

/*
The PgBouncer half of the Performance panel. PgBouncer's numbers live in its
admin console, a virtual database named pgbouncer that only its admin_users
and stats_users may open, so they cannot be read as the person's Postgres
role the way the rest of the panel is. PgBouncer lets one more in: the user
pgbouncer, with no password, over its unix socket, from a process with the
same uid as PgBouncer itself. So the agent runs `argus-agent pgbouncer-stats`
as the account PgBouncer runs as, which reads SHOW CONFIG, POOLS, STATS and
DATABASES and nothing else -- the admin console's SHOW commands only, never
PAUSE, KILL, RELOAD or SET. When that cannot work (no socket, PgBouncer in a
container, an old version) the answer says why and carries the settings as
written in pgbouncer.ini, so the advice that needs only those still holds.

Read-only, so not recorded, like the other lists; DBA only, like every admin
operation, because it shows every database and user behind this PgBouncer.
*/

// poolerConsoleRows caps each SHOW answer.
const poolerConsoleRows = 2000

// PoolerConsoleRequest is what the pgbouncer-stats helper is asked.
type PoolerConsoleRequest struct {
	Socket string `json:"socket"`
	Port   int    `json:"port"`
}

// PoolerConsoleResult is what it answers.
type PoolerConsoleResult struct {
	Version   string              `json:"version,omitempty"`
	Settings  map[string]string   `json:"settings,omitempty"`
	Pools     *tunnel.PoolerTable `json:"pools,omitempty"`
	Stats     *tunnel.PoolerTable `json:"stats,omitempty"`
	Databases *tunnel.PoolerTable `json:"databases,omitempty"`
	Error     string              `json:"error,omitempty"`
}

// poolerStats answers the Performance panel's pooler.stats.
func (s *pgSession) poolerStats(ctx context.Context, req tunnel.PGRequest) error {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	console := s.cfg.PoolerConsole
	if console == nil {
		console = helperPoolerConsole(s.cfg)
	}
	st := poolerStatsFor(cctx, pgLocator{root: s.cfg.PGRoot}, s.db, s.g.Email, console)
	b, err := json.Marshal(st)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
	}
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b})
}

// poolerConsoleFunc reads a PgBouncer's admin console as account.
type poolerConsoleFunc func(ctx context.Context, account string, req PoolerConsoleRequest, actor string) PoolerConsoleResult

// poolerStatsFor finds the PgBouncer this database goes through (or, failing
// that, one on this server that serves it) and reads it.
func poolerStatsFor(ctx context.Context, l pgLocator, db tunnel.GrantDB, actor string, console poolerConsoleFunc) tunnel.PoolerStats {
	loc := l.locate()
	if len(loc.Bouncers) == 0 {
		return tunnel.PoolerStats{}
	}
	pick, through := -1, false
	for i, b := range loc.Bouncers {
		if (db.PoolPort > 0 && b.Port == db.PoolPort) || (db.PoolPort == 0 && b.Port == db.Port) {
			pick, through = i, true
			break
		}
	}
	if pick < 0 {
		for i, b := range loc.Bouncers {
			if poolerCovers(b, db.Name) {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		pick = 0
	}
	b := loc.Bouncers[pick]
	st := tunnel.PoolerStats{Found: true, Through: through, Port: b.Port, Config: b.Config,
		Running: b.Pid != "", Settings: map[string]string{}, SettingsFrom: "file", DatabasePools: b.DatabasePools}
	for k, v := range b.Settings {
		st.Settings[k] = v
	}
	for i, o := range loc.Bouncers {
		if i != pick {
			st.Others = append(st.Others, o.Port)
		}
	}
	switch {
	case !st.Running:
		st.LiveError = "no running pgbouncer was started with " + b.Config + ", so only the file was read"
		return st
	case l.inContainer(proc{pid: b.Pid}):
		st.LiveError = "PgBouncer runs in a container, whose admin console the agent does not open; only the file was read"
		return st
	case b.Socket == "":
		st.LiveError = "PgBouncer listens on TCP only (unix_socket_dir is empty). Its admin console opens without a password only " +
			"through its socket, so only pgbouncer.ini was read. Set unix_socket_dir to read pools and waits too"
		return st
	}
	acct, err := l.processUser(b.Pid)
	if err != nil {
		st.LiveError = "could not tell which account PgBouncer runs as: " + err.Error()
		return st
	}
	st.RunsAs = acct
	if acct == "root" {
		st.LiveError = "PgBouncer runs as root, as which the agent does not open its console"
		return st
	}
	res := console(ctx, acct, PoolerConsoleRequest{Socket: b.Socket, Port: b.Port}, actor)
	if res.Error != "" {
		st.LiveError = "PgBouncer's admin console did not answer as " + acct + " through " + b.Socket + ": " + res.Error
		return st
	}
	st.Version, st.Pools, st.Stats, st.Databases = res.Version, res.Pools, res.Stats, res.Databases
	if len(res.Settings) > 0 {
		st.Settings, st.SettingsFrom = res.Settings, "console"
	}
	return st
}

// processUser is the account a process runs as (its effective uid).
func (l pgLocator) processUser(pid string) (string, error) {
	b, err := l.read("/proc/"+pid+"/status", 1<<14)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) < 2 {
				break
			}
			u, err := user.LookupId(f[1])
			if err != nil {
				return "", fmt.Errorf("uid %s has no account here", f[1])
			}
			return u.Username, nil
		}
	}
	return "", fmt.Errorf("process %s has no Uid line", pid)
}

// readPoolerConsole runs the admin console's read-only SHOW commands. The
// console speaks the simple query protocol only, which wire.Conn.Exec uses.
func readPoolerConsole(ctx context.Context, connString string) PoolerConsoleResult {
	config, err := wire.ParseConfig(connString)
	if err != nil {
		return PoolerConsoleResult{Error: err.Error()}
	}
	conn, err := wire.Connect(ctx, config)
	if err != nil {
		return PoolerConsoleResult{Error: err.Error()}
	}
	defer conn.Close(context.Background())
	var res PoolerConsoleResult
	show := func(what string) (*tunnel.PoolerTable, error) {
		rs, err := conn.Exec(ctx, "SHOW "+what)
		if err != nil {
			return nil, err
		}
		t := &tunnel.PoolerTable{Cols: []string{}, Rows: [][]string{}}
		for _, f := range rs.Fields {
			t.Cols = append(t.Cols, f.Name)
		}
		for i, r := range rs.Rows {
			if i >= poolerConsoleRows {
				break
			}
			row := make([]string, len(r))
			for j, c := range r {
				row[j] = string(c)
			}
			t.Rows = append(t.Rows, row)
		}
		return t, nil
	}
	if v, err := show("VERSION"); err == nil && len(v.Rows) > 0 && len(v.Rows[0]) > 0 {
		res.Version = v.Rows[0][0]
	}
	if c, err := show("CONFIG"); err == nil {
		res.Settings = map[string]string{}
		for _, r := range c.Rows {
			if len(r) >= 2 && tunnel.PoolerSettingKey(r[0]) {
				res.Settings[r[0]] = r[1]
			}
		}
	}
	if res.Pools, err = show("POOLS"); err != nil {
		return PoolerConsoleResult{Error: err.Error()}
	}
	// STATS and DATABASES are a bonus; a version without a column still
	// answers POOLS.
	res.Stats, _ = show("STATS")
	res.Databases, _ = show("DATABASES")
	return res
}

// PoolerConsoleMain is `argus-agent pgbouncer-stats`: one request on stdin,
// one answer on stdout, run as the account PgBouncer runs as.
func PoolerConsoleMain(ctx context.Context, in []byte) PoolerConsoleResult {
	var req PoolerConsoleRequest
	if err := json.Unmarshal(in, &req); err != nil {
		return PoolerConsoleResult{Error: "bad request: " + err.Error()}
	}
	if !strings.HasPrefix(req.Socket, "/") || strings.Contains(req.Socket, "..") || strings.ContainsAny(req.Socket, " '\\") {
		return PoolerConsoleResult{Error: "the socket directory must be a plain absolute path"}
	}
	if req.Port < 1 || req.Port > 65535 {
		return PoolerConsoleResult{Error: "no port"}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return readPoolerConsole(ctx, fmt.Sprintf("host=%s port=%d user=pgbouncer dbname=pgbouncer sslmode=disable connect_timeout=5", req.Socket, req.Port))
}

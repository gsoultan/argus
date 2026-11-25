package tunnel

/*
Performance analysis of a Postgres database (the console's Performance
panel). Postgres's own numbers -- pg_stat_statements, pg_stat_user_tables,
pg_settings and the rest -- are ordinary recorded statements run as the
person's role, so Postgres decides what each level sees. PgBouncer's are not
reachable that way: its admin console is a virtual database that only its
admin_users and stats_users may open. So the agent reads them itself, as the
read-only admin operation AdminPoolerStats, at DBA only like every admin op.

Nothing here changes anything. The console turns both into findings; it
never runs a fix on its own, it only puts the statement in the editor.
*/

// AdminPoolerStats reads the PgBouncer in front of this database: its
// effective settings and, when its admin console answers, SHOW POOLS, SHOW
// STATS and SHOW DATABASES.
const AdminPoolerStats = "pooler.stats"

// PoolerTable is one SHOW command's answer, every cell as text. Columns
// differ between PgBouncer versions, so they are passed through by name.
type PoolerTable struct {
	Cols []string   `json:"cols"`
	Rows [][]string `json:"rows"`
}

// PoolerStats answers AdminPoolerStats.
type PoolerStats struct {
	// Found is whether this server runs or is configured with a PgBouncer
	// at all. When false, nothing else is set.
	Found bool `json:"found"`
	// Through is whether this database's sessions go through that
	// PgBouncer (the entry names it as its pooler, or its port is
	// PgBouncer's). A PgBouncer that is found but not Through is one the
	// entry bypasses.
	Through bool   `json:"through"`
	Port    int    `json:"port,omitempty"`
	Config  string `json:"config,omitempty"`
	// Running is whether a pgbouncer process was found; RunsAs is the
	// account it runs as.
	Running bool   `json:"running"`
	RunsAs  string `json:"runsAs,omitempty"`
	// Settings are its tuning settings (never passwords or file contents):
	// from SHOW CONFIG when SettingsFrom is "console", which includes
	// PgBouncer's defaults, else as written in pgbouncer.ini ("file").
	Settings     map[string]string `json:"settings,omitempty"`
	SettingsFrom string            `json:"settingsFrom,omitempty"`
	// DatabasePools are per-database pool_size and pool_mode overrides
	// from [databases], by name.
	DatabasePools map[string]map[string]string `json:"databasePools,omitempty"`
	Version       string                       `json:"version,omitempty"`
	Pools         *PoolerTable                 `json:"pools,omitempty"`
	Stats         *PoolerTable                 `json:"stats,omitempty"`
	Databases     *PoolerTable                 `json:"databases,omitempty"`
	// LiveError says why the admin console could not be read; the
	// settings then come from the file only.
	LiveError string `json:"liveError,omitempty"`
	// Others are the ports of any other PgBouncer on this server.
	Others []int `json:"others,omitempty"`
}

// PoolerSettingKeys are the pgbouncer.ini settings the analysis reads. Only
// these leave the server: never auth_file, auth_query, passwords or paths.
var PoolerSettingKeys = []string{
	"pool_mode", "max_client_conn", "default_pool_size", "min_pool_size",
	"reserve_pool_size", "reserve_pool_timeout", "max_db_connections",
	"max_user_connections", "server_idle_timeout", "server_lifetime",
	"server_connect_timeout", "server_login_retry", "query_timeout",
	"query_wait_timeout", "client_idle_timeout", "idle_transaction_timeout",
	"max_prepared_statements", "server_reset_query", "server_reset_query_always",
	"server_check_delay", "server_round_robin", "auth_type", "listen_port",
	"so_reuseport", "pkt_buf", "tcp_keepalive", "application_name_add_host",
}

// PoolerSettingKey says whether key is one of PoolerSettingKeys.
func PoolerSettingKey(key string) bool {
	for _, k := range PoolerSettingKeys {
		if k == key {
			return true
		}
	}
	return false
}

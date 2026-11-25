//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

const poolerTestIni = `
[databases]
shop = host=127.0.0.1 port=5432 dbname=shop user=app password=s3cret-db pool_size=40 pool_mode=session
* = host=127.0.0.1 port=5432

[pgbouncer]
listen_port = 6432
unix_socket_dir = /var/run/postgresql
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/userlist.txt
auth_query = SELECT usename, passwd FROM pg_shadow WHERE usename=$1
pool_mode = transaction
max_client_conn = 1000
default_pool_size = 20
`

// pgBouncerAccount is who the made-up PgBouncer runs as: the account running
// the test, or nobody when that is root.
func pgBouncerAccount(t *testing.T) *user.User {
	t.Helper()
	u, err := user.Current()
	if err == nil && u.Uid == "0" {
		u, err = user.Lookup("nobody")
	}
	if err != nil {
		t.Skip("no account to stand in for PgBouncer's")
	}
	return u
}

// pgFakeBouncer is a server with one running PgBouncer.
func pgFakeBouncer(t *testing.T, ini string) *pgFakeHost {
	f := newPGFakeHost(t)
	u := pgBouncerAccount(t)
	f.process("700", "/usr/sbin/pgbouncer", "-d", "/etc/pgbouncer/pgbouncer.ini")
	f.file("/proc/700/status", fmt.Sprintf("Name:\tpgbouncer\nUid:\t%s\t%s\t%s\t%s\n", u.Uid, u.Uid, u.Uid, u.Uid))
	f.file("/etc/pgbouncer/pgbouncer.ini", ini)
	return f
}

func noSecrets(t *testing.T, st tunnel.PoolerStats) {
	t.Helper()
	b, _ := json.Marshal(st)
	for _, s := range []string{"s3cret-db", "userlist.txt", "pg_shadow", "password"} {
		if strings.Contains(string(b), s) {
			t.Fatalf("the answer carries %q: %s", s, b)
		}
	}
}

// What the console answers wins over the file, and the PgBouncer the entry
// names is the one read.
func TestPoolerStatsReadsTheConsole(t *testing.T) {
	f := pgFakeBouncer(t, poolerTestIni)
	me := pgBouncerAccount(t)
	var asked PoolerConsoleRequest
	var as string
	console := func(_ context.Context, account string, req PoolerConsoleRequest, _ string) PoolerConsoleResult {
		as, asked = account, req
		return PoolerConsoleResult{Version: "PgBouncer 1.23.1",
			Settings: map[string]string{"pool_mode": "transaction", "default_pool_size": "20", "max_client_conn": "1000"},
			Pools:    &tunnel.PoolerTable{Cols: []string{"database", "user", "cl_waiting"}, Rows: [][]string{{"shop", "app", "7"}}}}
	}
	st := poolerStatsFor(context.Background(), pgLocator{root: f.root},
		tunnel.GrantDB{Name: "shop", Host: "/var/run/postgresql", Port: 5432, PoolHost: "127.0.0.1", PoolPort: 6432}, "a@b", console)
	if !st.Found || !st.Through || st.Port != 6432 || !st.Running {
		t.Fatalf("stats = %+v", st)
	}
	if as != me.Username || asked.Socket != "/var/run/postgresql" || asked.Port != 6432 {
		t.Fatalf("console read as %q with %+v", as, asked)
	}
	if st.SettingsFrom != "console" || st.Version != "PgBouncer 1.23.1" || st.Pools == nil || st.LiveError != "" {
		t.Fatalf("stats = %+v", st)
	}
	if st.DatabasePools["shop"]["pool_size"] != "40" || st.DatabasePools["shop"]["pool_mode"] != "session" {
		t.Fatalf("per-database pools = %+v", st.DatabasePools)
	}
	noSecrets(t, st)
}

// When the console cannot be read the file still answers, and says so.
func TestPoolerStatsFallsBackToTheFile(t *testing.T) {
	f := pgFakeBouncer(t, poolerTestIni)
	console := func(context.Context, string, PoolerConsoleRequest, string) PoolerConsoleResult {
		return PoolerConsoleResult{Error: "FATAL: not allowed"}
	}
	st := poolerStatsFor(context.Background(), pgLocator{root: f.root},
		tunnel.GrantDB{Name: "shop", Host: "/var/run/postgresql", Port: 5432}, "a@b", console)
	if !st.Found || st.Through {
		t.Fatalf("an entry that does not name the pooler went through it: %+v", st)
	}
	if st.SettingsFrom != "file" || st.Settings["pool_mode"] != "transaction" || st.Settings["default_pool_size"] != "20" {
		t.Fatalf("settings = %+v from %s", st.Settings, st.SettingsFrom)
	}
	if _, leaked := st.Settings["auth_file"]; leaked {
		t.Fatal("auth_file left the server")
	}
	if !strings.Contains(st.LiveError, "not allowed") {
		t.Fatalf("live error = %q", st.LiveError)
	}
	noSecrets(t, st)
}

func TestPoolerStatsWithoutASocket(t *testing.T) {
	f := pgFakeBouncer(t, strings.Replace(poolerTestIni, "unix_socket_dir = /var/run/postgresql", "unix_socket_dir =", 1))
	called := false
	console := func(context.Context, string, PoolerConsoleRequest, string) PoolerConsoleResult {
		called = true
		return PoolerConsoleResult{}
	}
	st := poolerStatsFor(context.Background(), pgLocator{root: f.root},
		tunnel.GrantDB{Name: "shop", Host: "127.0.0.1", Port: 6432}, "a@b", console)
	if called || !st.Through || !strings.Contains(st.LiveError, "unix_socket_dir") {
		t.Fatalf("called=%v stats=%+v", called, st)
	}
}

func TestPoolerStatsNoPgBouncer(t *testing.T) {
	f := newPGFakeHost(t)
	st := poolerStatsFor(context.Background(), pgLocator{root: f.root}, tunnel.GrantDB{Name: "x", Host: "/tmp", Port: 5432}, "", nil)
	if st.Found {
		t.Fatalf("stats = %+v", st)
	}
}

func TestPoolerConsoleRefusesOddSockets(t *testing.T) {
	for _, s := range []string{"", "tmp", "/tmp/../etc", "/tmp dbname=postgres", "/tmp'x"} {
		in, _ := json.Marshal(PoolerConsoleRequest{Socket: s, Port: 6432})
		if res := PoolerConsoleMain(context.Background(), in); res.Error == "" {
			t.Fatalf("socket %q was used", s)
		}
	}
}

// A real PgBouncer's admin console, as a stats user over TCP. It needs no
// Postgres behind it: SHOW answers from PgBouncer's own state.
func TestReadPoolerConsole(t *testing.T) {
	bin, err := exec.LookPath("pgbouncer")
	if err != nil {
		t.Skip("pgbouncer is not installed")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	dir, err := os.MkdirTemp("/var/tmp", "argus-pgbouncer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ini := fmt.Sprintf(`[databases]
shop = host=127.0.0.1 port=1 pool_size=3

[pgbouncer]
listen_addr = 127.0.0.1
listen_port = %d
unix_socket_dir =
auth_type = trust
auth_file = %s
stats_users = watcher
pool_mode = transaction
default_pool_size = 7
max_client_conn = 77
logfile = %s
pidfile = %s
`, port, filepath.Join(dir, "userlist.txt"), filepath.Join(dir, "pgbouncer.log"), filepath.Join(dir, "pgbouncer.pid"))
	if err := os.WriteFile(filepath.Join(dir, "pgbouncer.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "userlist.txt"), []byte(`"watcher" ""`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-q", filepath.Join(dir, "pgbouncer.ini"))
	if os.Geteuid() == 0 {
		u, err := user.Lookup("postgres")
		if err != nil {
			u, err = user.Lookup("nobody")
		}
		if err != nil {
			t.Skip("no unprivileged account to run pgbouncer as")
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		for _, p := range []string{dir, filepath.Join(dir, "pgbouncer.ini"), filepath.Join(dir, "userlist.txt")} {
			if err := os.Chown(p, uid, gid); err != nil {
				t.Fatal(err)
			}
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	conn := fmt.Sprintf("host=127.0.0.1 port=%d user=watcher dbname=pgbouncer sslmode=disable connect_timeout=2", port)
	var res PoolerConsoleResult
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res = readPoolerConsole(context.Background(), conn)
		if res.Error == "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if res.Error != "" {
		log, _ := os.ReadFile(filepath.Join(dir, "pgbouncer.log"))
		t.Fatalf("console: %s\n%s", res.Error, log)
	}
	if !strings.HasPrefix(res.Version, "PgBouncer") {
		t.Fatalf("version = %q", res.Version)
	}
	if res.Settings["default_pool_size"] != "7" || res.Settings["max_client_conn"] != "77" || res.Settings["pool_mode"] != "transaction" {
		t.Fatalf("settings = %+v", res.Settings)
	}
	if _, ok := res.Settings["auth_file"]; ok {
		t.Fatal("auth_file was passed on")
	}
	if res.Pools == nil || res.Stats == nil || res.Databases == nil {
		t.Fatalf("tables = %+v", res)
	}
	has := func(tb *tunnel.PoolerTable, col string) bool {
		for _, c := range tb.Cols {
			if c == col {
				return true
			}
		}
		return false
	}
	if !has(res.Pools, "cl_waiting") || !has(res.Pools, "sv_active") || !has(res.Stats, "avg_query_time") {
		t.Fatalf("columns: pools %v stats %v", res.Pools.Cols, res.Stats.Cols)
	}
}

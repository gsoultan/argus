package agent

import (
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
	"github.com/jackc/pgx/v5/pgconn"
)

/*
A server where applications already connect through PgBouncer, and one where
an application has roles and grants of its own. Argus must fit in without
changing anything for them: these tests run a real PgBouncer in transaction
mode when one is installed, and check that what Argus leaves behind on the
server is exactly its own roles and nothing else.
*/

// startPgBouncer runs a PgBouncer in transaction mode in front of the test
// server, authenticating with Argus's lookup, and returns its port.
func startPgBouncer(t *testing.T, dsn string) int {
	t.Helper()
	bin, err := exec.LookPath("pgbouncer")
	if err != nil {
		t.Skip("pgbouncer is not installed")
	}
	cfg, _ := pgconn.ParseConfig(dsn)
	host := cfg.Host
	if strings.HasPrefix(host, "/") {
		host = "127.0.0.1"
	}
	// The one-time setup an admin runs, exactly as the console shows it.
	setup := tunnel.PoolerAuthSetupSQL()
	superExec(t, dsn, "DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'argus_people') "+
		"THEN CREATE ROLE argus_people NOLOGIN; END IF; END $$")
	superExec(t, dsn, setup...)
	superExec(t, dsn,
		"DROP ROLE IF EXISTS argus_test_pgb_auth",
		"CREATE ROLE argus_test_pgb_auth LOGIN PASSWORD 'pgb-test'",
		"GRANT "+tunnel.PoolerAuthRole+" TO argus_test_pgb_auth")
	t.Cleanup(func() { superExec(t, dsn, "DROP ROLE IF EXISTS argus_test_pgb_auth") })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	// Not t.TempDir: another account must be able to reach it.
	dir, err := os.MkdirTemp("/var/tmp", "argus-pgbouncer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ini := fmt.Sprintf(`[databases]
* = host=%s port=%d

[pgbouncer]
listen_addr = 127.0.0.1
listen_port = %d
unix_socket_dir = %s
auth_type = scram-sha-256
auth_file = %s
auth_user = argus_test_pgb_auth
auth_query = %s
pool_mode = transaction
max_client_conn = 50
default_pool_size = 5
logfile = %s
pidfile = %s
`, host, cfg.Port, port, dir, filepath.Join(dir, "userlist.txt"), tunnel.PoolerAuthQuery,
		filepath.Join(dir, "pgbouncer.log"), filepath.Join(dir, "pgbouncer.pid"))
	if err := os.WriteFile(filepath.Join(dir, "pgbouncer.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "userlist.txt"), []byte(`"argus_test_pgb_auth" "pgb-test"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-q", filepath.Join(dir, "pgbouncer.ini"))
	if os.Geteuid() == 0 {
		// PgBouncer refuses to run as root; hand it to an ordinary account.
		u, err := user.Lookup("postgres")
		if err != nil {
			u, err = user.Lookup("nobody")
		}
		if err != nil {
			t.Skip("no unprivileged account to run pgbouncer as")
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		for _, f := range []string{dir, filepath.Join(dir, "pgbouncer.ini"), filepath.Join(dir, "userlist.txt")} {
			if err := os.Chown(f, uid, gid); err != nil {
				t.Fatal(err)
			}
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			log, _ := os.ReadFile(filepath.Join(dir, "pgbouncer.log"))
			t.Logf("pgbouncer log:\n%s", log)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return port
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pgbouncer did not start")
	return 0
}

func TestPostgresPooledSession(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	port := startPgBouncer(t, dsn)
	superExec(t, dsn,
		"DROP TABLE IF EXISTS argus_t_pool",
		"CREATE TABLE argus_t_pool (id int PRIMARY KEY, v text)",
		"INSERT INTO argus_t_pool SELECT g, 'v' || g FROM generate_series(1, 500) g",
		"DROP ROLE IF EXISTS ana_sari")
	pooled := func(db *tunnel.GrantDB) { db.PoolHost = "127.0.0.1"; db.PoolPort = port; db.StatementTimeoutMS = 7000 }

	// Read level: through the pooler, read-only, limits set per statement.
	h := openPG(t, tunnel.LevelRead, pooled)
	if h.ready.Via != "pgbouncer" {
		t.Fatalf("the session must have gone through PgBouncer, not %q", h.ready.Via)
	}
	_, rows, final := h.run(1, "SELECT current_user, current_setting('transaction_read_only'), "+
		"current_setting('statement_timeout'), application_name FROM pg_stat_activity WHERE pid = pg_backend_pid()", 0)
	if final.Type != "done" || len(rows) != 1 {
		t.Fatalf("who am I: %+v", final)
	}
	if *rows[0][0] != "ana_sari" || *rows[0][1] != "on" || *rows[0][2] != "7s" || !strings.HasPrefix(*rows[0][3], "argus:") {
		t.Fatalf("pooled read session state: %q %q %q %q", *rows[0][0], *rows[0][1], *rows[0][2], *rows[0][3])
	}
	// A result cut at the row limit, then the connection is still good.
	_, rows, final = h.run(2, "SELECT id FROM argus_t_pool ORDER BY id", 10)
	if final.Type != "done" || !final.Truncated || len(rows) != 10 {
		t.Fatalf("truncated through the pooler: %+v (%d rows)", final, len(rows))
	}
	_, rows, final = h.run(3, "SELECT count(*) FROM argus_t_pool", 0)
	if final.Type != "done" || *rows[0][0] != "500" {
		t.Fatalf("after a truncated result: %+v %v", final, rows)
	}
	// A write at read level is refused by Postgres, and the next statement
	// still runs: our transaction was rolled back, not left aborted.
	if _, _, final = h.run(4, "DELETE FROM argus_t_pool", 0); final.Type != "error" {
		t.Fatalf("a read session must not write: %+v", final)
	}
	if _, rows, final = h.run(5, "SELECT 1", 0); final.Type != "done" || *rows[0][0] != "1" {
		t.Fatalf("after a refused statement: %+v", final)
	}
	// Nothing Argus set is left on the pooled server connection: as the
	// pooler's auth user, which shares no pool with ana_sari, and as
	// superuser, no backend of ana_sari's is inside a transaction.
	for _, row := range superQuery(t, dsn, "SELECT state, coalesce(query, '') FROM pg_stat_activity WHERE usename = 'ana_sari'") {
		if row[0] == "idle in transaction" || row[0] == "idle in transaction (aborted)" {
			t.Fatalf("a pooled connection was left in a transaction: %v", row)
		}
	}

	// Write level: a transaction spanning requests holds its server
	// connection through the pooler; a rollback keeps nothing.
	h.close()
	h = openPG(t, tunnel.LevelWrite, pooled)
	if h.ready.Via != "pgbouncer" {
		t.Fatalf("via %q", h.ready.Via)
	}
	for i, s := range []string{"BEGIN", "DELETE FROM argus_t_pool WHERE id <= 100", "SELECT count(*) FROM argus_t_pool", "ROLLBACK"} {
		_, rows, final := h.run(uint32(10+i), s, 0)
		if final.Type != "done" {
			t.Fatalf("%s: %+v", s, final)
		}
		if i == 2 && *rows[0][0] != "400" {
			t.Fatalf("inside the transaction the delete must show: %v", rows)
		}
	}
	if got := superQuery(t, dsn, "SELECT count(*) FROM argus_t_pool"); got[0][0] != "500" {
		t.Fatalf("rollback through the pooler: %v", got)
	}
	// An ordinary write commits on its own.
	if _, _, final = h.run(20, "UPDATE argus_t_pool SET v = 'x' WHERE id = 1", 0); final.Type != "done" || final.Rows != 1 {
		t.Fatalf("update: %+v", final)
	}
	if got := superQuery(t, dsn, "SELECT v FROM argus_t_pool WHERE id = 1"); got[0][0] != "x" {
		t.Fatalf("the update must be committed: %v", got)
	}

	// DBA level: the level role is set per statement, so what a DBA creates
	// belongs to the level, through a pooler that forgets session state.
	h.close()
	h = openPG(t, tunnel.LevelDBA, pooled)
	if h.ready.Via != "pgbouncer" {
		t.Fatalf("via %q", h.ready.Via)
	}
	_, rows, final = h.run(30, "SELECT current_user", 0)
	if final.Type != "done" || !strings.HasPrefix(*rows[0][0], "argus_dba_") {
		t.Fatalf("a pooled DBA statement must run as the level role: %+v %v", final, rows)
	}
	superExec(t, dsn, "DROP TABLE IF EXISTS argus_t_pool_dba")
	if _, _, final = h.run(31, "CREATE TABLE argus_t_pool_dba (id int)", 0); final.Type != "done" {
		t.Fatalf("create: %+v", final)
	}
	if got := superQuery(t, dsn, "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE relname = 'argus_t_pool_dba'"); !strings.HasPrefix(got[0][0], "argus_dba_") {
		t.Fatalf("owner of what a DBA made: %v", got)
	}
	superExec(t, dsn, "DROP TABLE argus_t_pool_dba")
}

func TestPostgresDBAManagesApplicationRoles(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	superExec(t, dsn,
		"DROP ROLE IF EXISTS ana_sari",
		"DROP ROLE IF EXISTS argus_t_app",
		"CREATE ROLE argus_t_app LOGIN")
	t.Cleanup(func() { superExec(t, dsn, "DROP ROLE IF EXISTS argus_t_app") })

	// A DBA gives an application's own role a level, sees it, takes it back.
	h := openPG(t, tunnel.LevelDBA, nil)
	read := levelRole(tunnel.LevelRead, dbname)
	if _, _, final := h.run(1, "GRANT "+pgx(read)+" TO argus_t_app", 0); final.Type != "done" {
		t.Fatalf("a DBA must be able to grant a level to an application role: %+v", final)
	}
	if got := superQuery(t, dsn, "SELECT pg_has_role('argus_t_app', '"+read+"', 'MEMBER')"); got[0][0] != "t" {
		t.Fatalf("membership: %v", got)
	}
	if _, _, final := h.run(2, "REVOKE "+pgx(read)+" FROM argus_t_app", 0); final.Type != "done" {
		t.Fatalf("revoke: %+v", final)
	}
	if got := superQuery(t, dsn, "SELECT pg_has_role('argus_t_app', '"+read+"', 'MEMBER')"); got[0][0] != "f" {
		t.Fatalf("membership after revoke: %v", got)
	}
	// What the console's Activity panel runs: statistics and the right to
	// signal backends, without superuser.
	_, rows, final := h.run(3, "SELECT count(*) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND query IS NOT NULL", 0)
	if final.Type != "done" || len(rows) != 1 {
		t.Fatalf("pg_stat_activity as a DBA: %+v", final)
	}
	_, rows, final = h.run(4, "SELECT pg_has_role(current_user, 'pg_signal_backend', 'USAGE')", 0)
	if final.Type != "done" || *rows[0][0] != "t" {
		t.Fatalf("a DBA must be able to cancel or end a backend: %+v %v", final, rows)
	}

	// An owner cannot change who holds a level.
	h.close()
	h = openPG(t, tunnel.LevelOwner, nil)
	if _, _, final := h.run(5, "GRANT "+pgx(read)+" TO argus_t_app", 0); final.Type != "error" || final.Code != "42501" {
		t.Fatalf("an owner must not hand out levels: %+v", final)
	}
}

func TestPostgresLeavesOtherRolesAlone(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	// An application with its own role, table and grants, as a DBA set it up
	// before Argus arrived.
	superExec(t, dsn,
		"DROP ROLE IF EXISTS ana_sari",
		"DROP TABLE IF EXISTS argus_t_theirs",
		"DROP ROLE IF EXISTS argus_t_app_owner", "DROP ROLE IF EXISTS argus_t_app_reader",
		"CREATE ROLE argus_t_app_owner LOGIN PASSWORD 'theirs' CREATEDB",
		"CREATE ROLE argus_t_app_reader LOGIN",
		"GRANT argus_t_app_reader TO argus_t_app_owner",
		"CREATE TABLE argus_t_theirs (id int)",
		"ALTER TABLE argus_t_theirs OWNER TO argus_t_app_owner",
		"GRANT SELECT ON argus_t_theirs TO argus_t_app_reader",
		"ALTER DEFAULT PRIVILEGES FOR ROLE argus_t_app_owner GRANT SELECT ON TABLES TO argus_t_app_reader")
	t.Cleanup(func() {
		superExec(t, dsn, "DROP TABLE IF EXISTS argus_t_theirs",
			"ALTER DEFAULT PRIVILEGES FOR ROLE argus_t_app_owner REVOKE SELECT ON TABLES FROM argus_t_app_reader",
			"DROP ROLE IF EXISTS argus_t_app_owner", "DROP ROLE IF EXISTS argus_t_app_reader")
	})
	snapshot := func() [][]string {
		return superQuery(t, dsn, `SELECT r.rolname, r.rolcanlogin, r.rolcreatedb, r.rolsuper, r.rolpassword IS NOT NULL,
			(SELECT string_agg(g.rolname, ',' ORDER BY g.rolname) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid),
			(SELECT string_agg(a.grantee::regrole::text || '=' || a.privilege_type, ',' ORDER BY 1)
			   FROM pg_class c, aclexplode(c.relacl) a
			   WHERE c.relname = 'argus_t_theirs' AND a.grantee::regrole::text NOT LIKE 'argus\_%'),
			(SELECT count(*) FROM pg_default_acl d WHERE d.defaclrole = r.oid)
			FROM pg_authid r WHERE r.rolname LIKE 'argus\_t\_app\_%' ORDER BY 1`)
	}
	before := snapshot()

	// Sessions at every level, including a DBA who creates a table, then
	// everyone leaves.
	for _, level := range []string{tunnel.LevelRead, tunnel.LevelWrite, tunnel.LevelOwner, tunnel.LevelDBA} {
		h := openPG(t, level, nil)
		if _, _, final := h.run(1, "SELECT count(*) FROM argus_t_theirs", 0); final.Type != "done" {
			t.Fatalf("%s: %+v", level, final)
		}
		h.close()
	}

	after := snapshot()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("Argus changed something that was not its own:\nbefore %v\nafter  %v", before, after)
	}
	// Argus's own additions are additive grants to its own level roles,
	// which the snapshot leaves out; the application's table is still
	// readable by its own reader and owned by its owner.
	if got := superQuery(t, dsn, "SELECT pg_get_userbyid(relowner), has_table_privilege('argus_t_app_reader', 'argus_t_theirs', 'SELECT') FROM pg_class WHERE relname = 'argus_t_theirs'"); got[0][0] != "argus_t_app_owner" || got[0][1] != "t" {
		t.Fatalf("the application's own access changed: %v", got)
	}
}

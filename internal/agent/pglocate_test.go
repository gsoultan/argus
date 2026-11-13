//go:build linux

package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// pgFakeHost is a made-up server root for pgLocator.
type pgFakeHost struct {
	t    *testing.T
	root string
}

func newPGFakeHost(t *testing.T) *pgFakeHost {
	t.Helper()
	// Short: a socket's path must fit in sun_path.
	dir, err := os.MkdirTemp("", "pgl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return &pgFakeHost{t: t, root: dir}
}

func (f *pgFakeHost) file(path, body string) {
	f.t.Helper()
	p := filepath.Join(f.root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// socket binds a real unix socket, which is what socketExists looks for.
func (f *pgFakeHost) socket(dir string, port string) {
	f.t.Helper()
	d := filepath.Join(f.root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		f.t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(d, ".s.PGSQL."+port))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = ln.Close() })
}

func (f *pgFakeHost) process(pid string, args ...string) {
	f.t.Helper()
	f.file("/proc/"+pid+"/cmdline", strings.Join(args, "\x00")+"\x00")
}

func (f *pgFakeHost) fd(pid, fd, inode string) {
	f.t.Helper()
	d := filepath.Join(f.root, "proc", pid, "fd")
	if err := os.MkdirAll(d, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink("socket:["+inode+"]", filepath.Join(d, fd)); err != nil {
		f.t.Fatal(err)
	}
}

func TestLocateReadsPostmasterPID(t *testing.T) {
	f := newPGFakeHost(t)
	// Postgres built from source: socket in /tmp, port 5433, data directory
	// given with -D. Its children's rewritten names are not postmasters.
	f.process("812", "/usr/local/pgsql/bin/postgres", "-D", "/srv/pg")
	f.process("813", "postgres: checkpointer")
	f.file("/srv/pg/postmaster.pid", "812\n/srv/pg\n1731500000\n5433\n/tmp\nlocalhost\n  5433001  0\nready\n")
	f.socket("/tmp", "5433")
	loc := pgLocator{root: f.root}.locate()
	if len(loc.Places) == 0 || loc.Places[0].Host != "/tmp" || loc.Places[0].Port != 5433 {
		t.Fatalf("places = %+v", loc.Places)
	}
	found := false
	for _, p := range loc.Places {
		if p.Host == "127.0.0.1" && p.Port == 5433 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no TCP way from listen_addresses: %+v", loc.Places)
	}
}

func TestLocateFollowsConfigIncludes(t *testing.T) {
	f := newPGFakeHost(t)
	f.process("900", "/usr/lib/postgresql/16/bin/postgres", "-D", "/var/lib/postgresql/16/main",
		"-c", "config_file=/etc/postgresql/16/main/postgresql.conf")
	f.file("/etc/postgresql/16/main/postgresql.conf", `
port = 5432				# overridden below
unix_socket_directories = '/var/run/postgresql' # comment
include_dir 'conf.d'
include_if_exists 'missing.conf'
`)
	f.file("/etc/postgresql/16/main/conf.d/10-port.conf", "port 5434\n")
	f.file("/var/lib/postgresql/16/main/postgresql.auto.conf", "unix_socket_directories = '/run/pg, /tmp'\n")
	f.socket("/run/pg", "5434")
	loc := pgLocator{root: f.root}.locate()
	if len(loc.Places) == 0 || loc.Places[0].Host != "/run/pg" || loc.Places[0].Port != 5434 {
		t.Fatalf("places = %+v", loc.Places)
	}
}

func TestLocateDebianClusters(t *testing.T) {
	f := newPGFakeHost(t)
	f.file("/etc/postgresql/14/main/postgresql.conf", "port = 5432\n")
	f.file("/etc/postgresql/16/main/postgresql.conf", "port = 5433\n")
	f.socket("/var/run/postgresql", "5433")
	loc := pgLocator{root: f.root}.locate()
	// The running cluster sorts first.
	if len(loc.Places) < 2 || loc.Places[0].Label != "16/main" || loc.Places[0].Port != 5433 {
		t.Fatalf("places = %+v", loc.Places)
	}
}

func TestLocateSkipsPgBouncerSockets(t *testing.T) {
	f := newPGFakeHost(t)
	f.process("700", "/usr/sbin/pgbouncer", "-d", "/etc/pgbouncer/pgbouncer.ini")
	f.fd("700", "5", "4242")
	f.file("/etc/pgbouncer/pgbouncer.ini", `
[databases]
app = host=10.0.0.5 port=5432 dbname=app_prod
"Orders" = host=db-02.internal
local = port=5433

[pgbouncer]
listen_port = 6432
unix_socket_dir = /var/run/postgresql
`)
	f.socket("/var/run/postgresql", "6432")
	f.file("/proc/net/unix", "Num       RefCount Protocol Flags    Type St Inode Path\n"+
		"0000000000000000: 00000002 00000000 00010000 0001 01 4242 /var/run/postgresql/.s.PGSQL.6432\n")
	loc := pgLocator{root: f.root}.locate()
	if len(loc.Places) != 0 {
		t.Fatalf("PgBouncer's socket was taken for Postgres: %+v", loc.Places)
	}
	if len(loc.Bouncers) != 1 || loc.Bouncers[0].Port != 6432 {
		t.Fatalf("bouncers = %+v", loc.Bouncers)
	}
	want := []tunnel.PoolerTarget{
		{Name: "app", Host: "10.0.0.5", Port: 5432, DBName: "app_prod"},
		{Name: "Orders", Host: "db-02.internal", Port: 5432},
		{Name: "local", Port: 5433},
	}
	got := loc.Bouncers[0].Targets
	if len(got) != len(want) {
		t.Fatalf("targets = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The case that started it: PgBouncer on this server, Postgres on another.
// The answer names where Postgres is instead of "no such file".
func TestDiscoverPgBouncerOnly(t *testing.T) {
	f := newPGFakeHost(t)
	f.file("/etc/pgbouncer/pgbouncer.ini", "[databases]\napp = host=10.0.0.5 port=5432\nbilling = host=10.0.0.5 dbname=bill\n")
	var asked []PGAdminRequest
	cfg := DiscoverDBConfig{PGRoot: f.root, Admin: func(_ context.Context, r PGAdminRequest) PGAdminResult {
		asked = append(asked, r)
		return PGAdminResult{Error: "could not sign in to Postgres as postgres over " + r.Socket +
			" (port 5432): failed to connect: dial error: connect: no such file or directory"}
	}}
	res := discoverPostgres(context.Background(), cfg,
		tunnel.GrantDiscover{Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432}, "ana@corp.example")
	if res.OK {
		t.Fatal("listed databases with no Postgres")
	}
	for _, w := range []string{"only PgBouncer", "10.0.0.5:5432 for app, billing (bill)"} {
		if !strings.Contains(res.Error, w) {
			t.Fatalf("error %q lacks %q", res.Error, w)
		}
	}
	if len(res.Poolers) != 1 || len(res.Poolers[0].Targets) != 2 {
		t.Fatalf("poolers = %+v", res.Poolers)
	}
	// The socket asked for, TCP on localhost, then the Postgres behind
	// PgBouncer, once.
	if len(asked) != 3 || asked[1].Socket != "127.0.0.1" || asked[2].Socket != "10.0.0.5" {
		t.Fatalf("asked = %+v", asked)
	}
}

// A Postgres on another port: the listing signs in there and says so.
func TestDiscoverFindsOtherPort(t *testing.T) {
	f := newPGFakeHost(t)
	f.file("/etc/postgresql/16/main/postgresql.conf", "port = 5433\n")
	f.socket("/var/run/postgresql", "5433")
	var asked []PGAdminRequest
	cfg := DiscoverDBConfig{PGRoot: f.root, Admin: func(_ context.Context, r PGAdminRequest) PGAdminResult {
		asked = append(asked, r)
		return PGAdminResult{OK: true, Version: "16.4", Found: []tunnel.Discovered{{Name: "orders"}}}
	}}
	res := discoverPostgres(context.Background(), cfg,
		tunnel.GrantDiscover{Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432}, "ana@corp.example")
	if !res.OK || res.Port != 5433 || res.Host != "/var/run/postgresql" {
		t.Fatalf("res = %+v", res)
	}
	if len(asked) != 1 || asked[0].Port != 5433 {
		t.Fatalf("asked = %+v", asked)
	}
}

// When every way fails, the error says each one and why.
func TestDiscoverSaysWhatWasTried(t *testing.T) {
	f := newPGFakeHost(t)
	f.file("/etc/postgresql/16/main/postgresql.conf", "port = 5433\n")
	f.socket("/var/run/postgresql", "5433")
	cfg := DiscoverDBConfig{PGRoot: f.root, Admin: func(_ context.Context, r PGAdminRequest) PGAdminResult {
		return PGAdminResult{Error: "could not sign in to Postgres as postgres over /var/run/postgresql (port 5433): " +
			`FATAL: Peer authentication failed for user "postgres" (SQLSTATE 28000)`}
	}}
	res := discoverPostgres(context.Background(), cfg,
		tunnel.GrantDiscover{Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432}, "ana@corp.example")
	for _, w := range []string{"(port 5432): no socket there", "(port 5433): Postgres refused peer sign-in"} {
		if !strings.Contains(res.Error, w) {
			t.Fatalf("error %q lacks %q", res.Error, w)
		}
	}
}

func TestConfValue(t *testing.T) {
	for in, want := range map[string]string{
		`'/tmp, /run/pg'  # two`: "/tmp, /run/pg",
		`5433 # port`:            "5433",
		`'it''s'`:                "it's",
	} {
		if got := confValue(in); got != want {
			t.Errorf("confValue(%q) = %q, want %q", in, got, want)
		}
	}
}

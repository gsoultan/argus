//go:build linux

package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/caduceus/wire"

	"github.com/gsoultan/argus/internal/tunnel"
)

func TestUserlist(t *testing.T) {
	f := newPGFakeHost(t)
	f.file("/etc/pgbouncer/userlist.txt", `"postgres" "s3cret ""quoted"""
"app" "md5d41d8cd98f00b204e9800998ecf8427e"
"scram" "SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy"
;comment
`)
	u := pgLocator{root: f.root}.userlist("/etc/pgbouncer/userlist.txt")
	if u["postgres"] != `s3cret "quoted"` || len(u) != 3 {
		t.Fatalf("userlist = %q", u)
	}
	if pgHashedSecret(u["postgres"]) || !pgHashedSecret(u["app"]) || !pgHashedSecret(u["scram"]) {
		t.Fatal("hashes misread")
	}
}

// The agent signs in to another server only where its own PgBouncer sends,
// and only with a password, never a hash.
func TestPGRemoteAdminRefuses(t *testing.T) {
	f := newPGFakeHost(t)
	f.file("/etc/pgbouncer/pgbouncer.ini", "[databases]\napp = host=10.0.0.5 port=5432\n[pgbouncer]\nauth_file = userlist.txt\n")
	f.file("/etc/pgbouncer/userlist.txt", `"postgres" "md5d41d8cd98f00b204e9800998ecf8427e"`+"\n")
	l := pgLocator{root: f.root}
	for _, c := range []struct {
		host string
		port int
		want string
	}{
		{"10.0.0.6", 5432, "is not one this server's PgBouncer sends to"},
		{"10.0.0.5", 5433, "is not one this server's PgBouncer sends to"},
		{"10.0.0.5", 5432, "has only a hash of postgres's password"},
	} {
		r := pgRemoteAdmin(context.Background(), l, PGAdminRequest{Op: "discover-databases", Database: "postgres",
			Socket: c.host, Port: c.port})
		if r.OK || !strings.Contains(r.Error, c.want) {
			t.Fatalf("%s:%d: %+v", c.host, c.port, r)
		}
	}
	r := pgRemoteAdmin(context.Background(), l, PGAdminRequest{Op: "discover-databases", Database: "postgres",
		Socket: "10.0.0.5", Port: 5432, AdminUser: "nobody"})
	if r.OK || !strings.Contains(r.Error, "has no password for nobody") {
		t.Fatalf("%+v", r)
	}
}

// An entry that names this server, on a server with no Postgres of its own,
// opens where this server's PgBouncer sends that database, and nowhere else.
func TestPGSentOn(t *testing.T) {
	f := newPGFakeHost(t)
	f.file("/etc/pgbouncer/pgbouncer.ini", "[databases]\n"+
		"athena_db = host=10.0.3.7 port=5432\n"+
		"alias = host=10.0.3.7 port=5433 dbname=real\n"+
		"local = host=/var/run/postgresql port=5432\n"+
		"* = host=10.0.3.9\n"+
		"[pgbouncer]\nlisten_port = 6432\n")
	l := pgLocator{root: f.root}
	sock := "/var/run/postgresql"
	for _, c := range []struct {
		db   tunnel.GrantDB
		want string // host:port/dbname, or "" for left alone
	}{
		{tunnel.GrantDB{Name: "athena_db", Host: sock, Port: 5432}, "10.0.3.7:5432/athena_db"},
		{tunnel.GrantDB{Name: "alias", Host: sock, Port: 5432}, "10.0.3.7:5433/real"},
		{tunnel.GrantDB{Name: "other", Host: sock, Port: 5432}, "10.0.3.9:5432/other"},
		// Named and kept on this server: "*" is not where it goes.
		{tunnel.GrantDB{Name: "local", Host: sock, Port: 5432}, ""},
		// PgBouncer's own port on loopback is PgBouncer, not Postgres.
		{tunnel.GrantDB{Name: "athena_db", Host: "127.0.0.1", Port: 6432}, "10.0.3.7:5432/athena_db"},
		// Any other loopback port may be a Postgres on TCP only.
		{tunnel.GrantDB{Name: "athena_db", Host: "127.0.0.1", Port: 5432}, ""},
		// An entry already naming another server is not this check's.
		{tunnel.GrantDB{Name: "athena_db", Host: "10.0.3.7", Port: 5432}, ""},
	} {
		got := ""
		if tg, ok := l.sentOn(c.db); ok {
			got = fmt.Sprintf("%s:%d/%s", tg.Host, tg.Port, tg.DBName)
		}
		if got != c.want {
			t.Errorf("%+v: got %q, want %q", c.db, got, c.want)
		}
	}

	// "local" goes to a socket here, and no "*" covers it twice over: it
	// is left alone rather than sent somewhere PgBouncer does not send it.
	f2 := newPGFakeHost(t)
	f2.file("/etc/pgbouncer/pgbouncer.ini", "[databases]\nlocal = host=/var/run/postgresql\n")
	if _, ok := (pgLocator{root: f2.root}).sentOn(tunnel.GrantDB{Name: "local", Host: sock, Port: 5432}); ok {
		t.Error("a database PgBouncer keeps on this server was sent elsewhere")
	}

	// Two PgBouncers that disagree: no guess.
	f3 := newPGFakeHost(t)
	f3.file("/etc/pgbouncer/pgbouncer.ini", "[databases]\nathena_db = host=10.0.3.7\n")
	f3.file("/etc/pgbouncer.ini", "[databases]\nathena_db = host=10.0.3.8\n")
	if _, ok := (pgLocator{root: f3.root}).sentOn(tunnel.GrantDB{Name: "athena_db", Host: sock, Port: 5432}); ok {
		t.Error("guessed between two PgBouncers")
	}

	// A Postgres running here is opened as before.
	f.socket(sock, "5432")
	if tg, ok := l.sentOn(tunnel.GrantDB{Name: "athena_db", Host: sock, Port: 5432}); ok {
		t.Errorf("sent to %+v with a Postgres running here", tg)
	}
}

// End to end against a real Postgres, reached as another server would be:
// by an address that is not loopback's usual name, through PgBouncer's
// configuration and userlist.txt.
func TestPGRemoteDiscoverThroughPgBouncer(t *testing.T) {
	dsn := os.Getenv("ARGUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ARGUS_TEST_DATABASE_URL is not set")
	}
	pc, err := wire.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// 127.0.0.2 is this machine, but not a name the agent treats as local.
	host := "127.0.0.2"
	f := newPGFakeHost(t)
	f.file("/etc/pgbouncer/pgbouncer.ini", fmt.Sprintf("[databases]\n%s = host=%s port=%d\n[pgbouncer]\nlisten_port = 6432\nauth_file = /etc/pgbouncer/userlist.txt\n",
		pc.Database, host, pc.Port))
	f.file("/etc/pgbouncer/userlist.txt", fmt.Sprintf("%q %q\n", pc.User, pc.Password))

	cfg := DiscoverDBConfig{PGRoot: f.root, Admin: withPooledRemote(f.root,
		func(context.Context, PGAdminRequest) PGAdminResult {
			return PGAdminResult{Error: "could not sign in to Postgres as postgres over /var/run/postgresql (port 5432): no such file or directory"}
		})}
	// Asked at the default, it finds the Postgres behind PgBouncer.
	res := discoverPostgres(context.Background(), cfg, tunnel.GrantDiscover{Engine: tunnel.EnginePostgres,
		Host: "/var/run/postgresql", Port: 5432, AdminUser: pc.User}, "ana@corp.example")
	if !res.OK || res.Host != host || res.Port != int(pc.Port) {
		t.Fatalf("res = %+v", res)
	}
	found := false
	for _, d := range res.Databases {
		found = found || d.Name == pc.Database
	}
	if !found {
		t.Fatalf("%s not listed: %+v", pc.Database, res.Databases)
	}
	// Asked there directly, as an entry added from it would be.
	res = discoverPostgres(context.Background(), cfg, tunnel.GrantDiscover{Engine: tunnel.EnginePostgres,
		Host: host, Port: int(pc.Port), AdminUser: pc.User}, "ana@corp.example")
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
}

// A session on a Postgres behind this server's PgBouncer: provisioned with
// the password in userlist.txt, opened as the person's role on that server.
func TestPostgresSessionBehindPgBouncer(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	pc, err := wire.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if pc.Password == "" {
		t.Skip("ARGUS_TEST_PG_SUPER_DSN has no password to put in userlist.txt")
	}
	host := "127.0.0.2"
	f := newPGFakeHost(t)
	f.file("/etc/pgbouncer/pgbouncer.ini", fmt.Sprintf("[databases]\n* = host=%s port=%d\n[pgbouncer]\nauth_file = /etc/pgbouncer/userlist.txt\n", host, pc.Port))
	f.file("/etc/pgbouncer/userlist.txt", fmt.Sprintf("%q %q\n", pc.User, pc.Password))
	superExec(t, dsn, "DROP ROLE IF EXISTS ana_sari")

	key, _ := tunnel.GrantKeyFromSecret(strings.Repeat("s", 32))
	svc := Postgres(PostgresConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"}, RecordDir: t.TempDir(),
		PGRoot: f.root, AdminUser: pc.User,
		Admin: withPooledRemote(f.root, func(context.Context, PGAdminRequest) PGAdminResult {
			t.Fatal("a Postgres on another server went to the local helper")
			return PGAdminResult{}
		})})
	open := func(h string) tunnel.Answer {
		g := tunnel.NewGrant("ana.sari@corp.example", "db-01", "ana_sari", "")
		g.Service = tunnel.PostgresService
		g.DB = &tunnel.GrantDB{ID: "db1", Name: dbname, Host: h, Port: int(pc.Port), Level: tunnel.LevelRead}
		tok, _ := tunnel.SignGrant(key, g)
		a, b := net.Pipe()
		done := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer close(done)
			svc(ctx, &Call{Header: tunnel.Header{Service: tunnel.PostgresService, Grant: tok},
				Stream: pipeStream{a}, GrantKey: key.Public().(ed25519.PublicKey)})
		}()
		defer func() { cancel(); _ = a.Close(); _ = b.Close(); <-done }()
		var ans tunnel.Answer
		if err := tunnel.ReadJSON(b, &ans); err != nil {
			t.Fatal(err)
		}
		if ans.OK {
			var ready tunnel.PGAnswer
			fr := tunnel.NewFrameReader(b)
			for {
				typ, p, err := fr.Next()
				if err != nil {
					t.Fatal(err)
				}
				if typ == tunnel.FramePGAnswer {
					_ = json.Unmarshal(p, &ready)
					break
				}
			}
			if ready.Type != "ready" {
				t.Fatalf("not ready: %+v", ready)
			}
		}
		return ans
	}
	if ans := open(host); !ans.OK {
		t.Fatalf("refused: %s", ans.Error)
	}
	// An entry that kept the default socket, as one added before the agent
	// could reach another server did: opened where PgBouncer sends it.
	if ans := open("/var/run/postgresql"); !ans.OK {
		t.Fatalf("the default socket was not sent through PgBouncer: %s", ans.Error)
	}
	// Any other server is refused before anything is dialled.
	if ans := open("127.0.0.3"); ans.OK || !strings.Contains(ans.Error, "behind its PgBouncer") {
		t.Fatalf("another server was opened: %+v", ans)
	}
}

// With no password for postgres, the agent tries the other accounts PgBouncer
// can sign in with: its auth_user, a [databases] line's user=, then the rest
// of userlist.txt. Hashes are said, not tried.
func TestPGRemoteLogins(t *testing.T) {
	f := newPGFakeHost(t)
	f.file("/etc/pgbouncer/pgbouncer.ini", "[databases]\n"+
		"* = host=pg.example.id port=5432\n"+
		"app = host=pg.example.id port=5432 user=app_rw password=inline\n"+
		"[pgbouncer]\nauth_file = userlist.txt\nauth_user = pgbouncer\n")
	f.file("/etc/pgbouncer/userlist.txt", `"zed" "z"`+"\n"+
		`"hashed" "md5d41d8cd98f00b204e9800998ecf8427e"`+"\n"+
		`"pgbouncer" "pb"`+"\n")
	l := pgLocator{root: f.root}
	b, ok := l.behindPooler("pg.example.id", 5432)
	if !ok {
		t.Fatal("not behind the pooler")
	}
	got, missing := l.remoteLogins(b, PGAdminRequest{Socket: "pg.example.id", Port: 5432})
	var users []string
	for _, lg := range got {
		users = append(users, lg.User+"="+lg.Password)
	}
	if strings.Join(users, ",") != "pgbouncer=pb,app_rw=inline,zed=z" {
		t.Fatalf("logins = %v", users)
	}
	if m := strings.Join(missing, "; "); !strings.Contains(m, "has no password for postgres") ||
		!strings.Contains(m, "has only a hash of hashed's password") {
		t.Fatalf("missing = %q", m)
	}
}

// End to end: userlist.txt has no postgres, only the application's account,
// yet the databases behind PgBouncer are listed with it.
func TestPGRemoteDiscoverWithAppAccount(t *testing.T) {
	dsn, _, _, dbname := pgTest(t)
	pc, err := wire.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	superExec(t, dsn, "DROP ROLE IF EXISTS argus_app_t")
	superExec(t, dsn, "CREATE ROLE argus_app_t LOGIN PASSWORD 'app-pw'")
	t.Cleanup(func() { superExec(t, dsn, "DROP ROLE IF EXISTS argus_app_t") })
	host := "127.0.0.2"
	f := newPGFakeHost(t)
	f.file("/etc/pgbouncer/pgbouncer.ini", fmt.Sprintf("[databases]\n* = host=%s port=%d\n[pgbouncer]\nauth_file = /etc/pgbouncer/userlist.txt\n", host, pc.Port))
	f.file("/etc/pgbouncer/userlist.txt", `"argus_app_t" "app-pw"`+"\n")
	cfg := DiscoverDBConfig{PGRoot: f.root, Admin: withPooledRemote(f.root,
		func(context.Context, PGAdminRequest) PGAdminResult {
			return PGAdminResult{Error: "could not sign in to Postgres as postgres over /var/run/postgresql (port 5432): no such file or directory"}
		})}
	res := discoverPostgres(context.Background(), cfg, tunnel.GrantDiscover{Engine: tunnel.EnginePostgres,
		Host: "/var/run/postgresql", Port: 5432}, "ana@corp.example")
	if !res.OK || res.Host != host {
		t.Fatalf("res = %+v", res)
	}
	found := false
	for _, d := range res.Databases {
		found = found || d.Name == dbname
	}
	if !found {
		t.Fatalf("%s not listed: %+v", dbname, res.Databases)
	}
}

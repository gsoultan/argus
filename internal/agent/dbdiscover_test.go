package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

// askDiscover presents grant g to svc and returns the answer and, when the
// stream was accepted, the result.
func askDiscover(t *testing.T, svc TunnelService, key ed25519.PrivateKey, g tunnel.Grant) (tunnel.Answer, tunnel.DiscoverResult) {
	t.Helper()
	tok, err := tunnel.SignGrant(key, g)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc(context.Background(), &Call{Header: tunnel.Header{Service: tunnel.DiscoverService, Grant: tok},
			Stream: pipeStream{a}, GrantKey: key.Public().(ed25519.PublicKey)})
	}()
	defer func() { _ = b.Close(); <-done }()
	var ans tunnel.Answer
	if err := tunnel.ReadJSON(b, &ans); err != nil {
		t.Fatal(err)
	}
	var res tunnel.DiscoverResult
	if !ans.OK {
		return ans, res
	}
	typ, p, err := tunnel.NewFrameReader(b).Next()
	if err != nil || typ != tunnel.FrameDiscoverResult {
		t.Fatalf("no result: %v %q", err, typ)
	}
	if err := json.Unmarshal(p, &res); err != nil {
		t.Fatal(err)
	}
	return ans, res
}

func discoverGrant(d tunnel.GrantDiscover) tunnel.Grant {
	g := tunnel.NewGrant("ana@corp.example", "db-01", "discover", "")
	g.Service = tunnel.DiscoverService
	g.Discover = &d
	return g
}

func fakeDiscover(t *testing.T) (TunnelService, ed25519.PrivateKey, *[]PGAdminRequest) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	var asked []PGAdminRequest
	svc := DiscoverDatabases(DiscoverDBConfig{
		Verifier: &tunnel.GrantVerifier{Host: "db-01"},
		Admin: func(_ context.Context, req PGAdminRequest) PGAdminResult {
			asked = append(asked, req)
			return PGAdminResult{OK: true, Version: "16.4", Found: []tunnel.Discovered{
				{Name: "analytics", Owner: "etl", Bytes: 640 << 20}, {Name: "orders", Owner: "app", Bytes: 1 << 30}}}
		},
		Ctl: func(_ context.Context, args []string, _ string) ([]byte, error) {
			if args[0] != "list_vhosts" {
				return nil, fmt.Errorf("unexpected rabbitmqctl %v", args)
			}
			return []byte(`[{"name":"orders"},{"name":"/"},{"name":"bad\nname"}]`), nil
		},
		Redis: func(context.Context, tunnel.GrantDiscover) tunnel.DiscoverResult {
			return tunnel.DiscoverResult{OK: true, Configured: 2,
				Databases: []tunnel.Discovered{{Name: "0", Keys: 5}, {Name: "1"}}}
		},
	})
	return svc, key, &asked
}

func TestDiscoverListsEachEngine(t *testing.T) {
	svc, key, asked := fakeDiscover(t)

	ans, res := askDiscover(t, svc, key, discoverGrant(tunnel.GrantDiscover{
		Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5433}))
	if !ans.OK || !res.OK || len(res.Databases) != 2 || res.Databases[1].Name != "orders" {
		t.Fatalf("postgres: %+v %+v", ans, res)
	}
	if len(*asked) != 1 || (*asked)[0].Op != "discover-databases" || (*asked)[0].Port != 5433 ||
		(*asked)[0].Database != "postgres" {
		t.Fatalf("the helper was asked %+v", *asked)
	}

	_, res = askDiscover(t, svc, key, discoverGrant(tunnel.GrantDiscover{
		Engine: tunnel.EngineRabbitMQ, Host: "127.0.0.1", Port: 15672}))
	if !res.OK || len(res.Databases) != 2 || res.Databases[0].Name != "/" || res.Databases[1].Name != "orders" {
		t.Fatalf("rabbitmq: %+v", res)
	}

	_, res = askDiscover(t, svc, key, discoverGrant(tunnel.GrantDiscover{
		Engine: tunnel.EngineRedis, Host: "127.0.0.1", Port: 6379}))
	if !res.OK || res.Configured != 2 || res.Databases[0].Keys != 5 {
		t.Fatalf("redis: %+v", res)
	}
}

// A discover grant reads names on this server only, and no other grant can
// be spent on it.
func TestDiscoverRefusesOtherGrantsAndOtherHosts(t *testing.T) {
	svc, key, asked := fakeDiscover(t)

	g := tunnel.NewGrant("ana@corp.example", "db-01", "ana", "")
	g.Service = tunnel.PostgresService
	g.DB = &tunnel.GrantDB{ID: "db-x", Name: "orders", Host: "/var/run/postgresql", Port: 5432, Level: "dba"}
	if ans, _ := askDiscover(t, svc, key, g); ans.OK || !strings.Contains(ans.Error, "not for listing") {
		t.Fatalf("a workspace grant listed databases: %+v", ans)
	}
	for _, d := range []tunnel.GrantDiscover{
		{Engine: tunnel.EnginePostgres, Host: "10.0.0.9", Port: 5432},
		{Engine: tunnel.EngineRedis, Host: "redis.internal", Port: 6379},
		{Engine: tunnel.EngineRabbitMQ, Host: "/var/run", Port: 15672},
		{Engine: "mysql", Host: "127.0.0.1", Port: 3306},
		{Engine: tunnel.EnginePostgres, Host: "127.0.0.1", Port: 0},
	} {
		if ans, _ := askDiscover(t, svc, key, discoverGrant(d)); ans.OK {
			t.Fatalf("listed %+v", d)
		}
	}
	if len(*asked) != 0 {
		t.Fatalf("the helper ran for a refused grant: %+v", *asked)
	}

	// And a discover grant opens no workspace.
	pg := Postgres(PostgresConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"}, RecordDir: t.TempDir(),
		Admin: func(context.Context, PGAdminRequest) PGAdminResult { t.Fatal("provisioned"); return PGAdminResult{} }})
	tok, _ := tunnel.SignGrant(key, discoverGrant(tunnel.GrantDiscover{Engine: tunnel.EnginePostgres,
		Host: "/var/run/postgresql", Port: 5432}))
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pg(context.Background(), &Call{Header: tunnel.Header{Service: tunnel.PostgresService, Grant: tok},
			Stream: pipeStream{a}, GrantKey: key.Public().(ed25519.PublicKey)})
	}()
	var ans tunnel.Answer
	_ = tunnel.ReadJSON(b, &ans)
	_ = b.Close()
	<-done
	if ans.OK {
		t.Fatal("a discover grant opened a workspace")
	}
}

func TestParseKeyspace(t *testing.T) {
	got := parseKeyspace("# Keyspace\r\ndb0:keys=48120,expires=3,avg_ttl=0\r\ndb4:keys=12,expires=0,avg_ttl=0\r\nnoise\r\n")
	if got[0] != 48120 || got[4] != 12 || len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

// The real query, on a real server when one is configured.
func TestPGDiscoverListsDatabases(t *testing.T) {
	dsn := os.Getenv("ARGUS_TEST_PG_SUPER_DSN")
	if dsn == "" {
		t.Skip("ARGUS_TEST_PG_SUPER_DSN is not set")
	}
	conn, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	res := RunPGAdmin(context.Background(), conn, PGAdminRequest{Op: "discover-databases", Database: "postgres"})
	if !res.OK {
		t.Fatal(res.Error)
	}
	seen := map[string]bool{}
	for _, d := range res.Found {
		seen[d.Name] = true
		if d.Owner == "" {
			t.Fatalf("no owner for %+v", d)
		}
	}
	if !seen["postgres"] || seen["template0"] || seen["template1"] {
		t.Fatalf("found %v", seen)
	}
}

//go:build !windows

package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

func askCreate(t *testing.T, svc TunnelService, key ed25519.PrivateKey, g tunnel.Grant) (tunnel.Answer, tunnel.CreateResult) {
	t.Helper()
	tok, err := tunnel.SignGrant(key, g)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc(context.Background(), &Call{Header: tunnel.Header{Service: tunnel.CreateService, Grant: tok},
			Stream: pipeStream{a}, GrantKey: key.Public().(ed25519.PublicKey)})
	}()
	defer func() { _ = b.Close(); <-done }()
	var ans tunnel.Answer
	if err := tunnel.ReadJSON(b, &ans); err != nil {
		t.Fatal(err)
	}
	var res tunnel.CreateResult
	if !ans.OK {
		return ans, res
	}
	typ, p, err := tunnel.NewFrameReader(b).Next()
	if err != nil || typ != tunnel.FrameCreateResult {
		t.Fatalf("no result: %v %q", err, typ)
	}
	if err := json.Unmarshal(p, &res); err != nil {
		t.Fatal(err)
	}
	return ans, res
}

func createGrant(c tunnel.GrantCreate) tunnel.Grant {
	g := tunnel.NewGrant("ana@corp.example", "db-01", "create", "")
	g.Service = tunnel.CreateService
	g.Create = &c
	return g
}

// The service checks the grant before anything runs, takes the Postgres
// steps in order (the database, then inside it), and never runs over a
// vhost that is there.
func TestCreateServiceOrderAndRefusals(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	var asked []PGAdminRequest
	var ctl [][]string
	svc := CreateDatabases(CreateDBConfig{
		Verifier: &tunnel.GrantVerifier{Host: "db-01"},
		Admin: func(_ context.Context, req PGAdminRequest) PGAdminResult {
			asked = append(asked, req)
			switch req.Op {
			case "create-database":
				return PGAdminResult{OK: true, Steps: []tunnel.CreateStep{{Text: "Created the database invoices", OK: true}}}
			case "prepare-new-database":
				return PGAdminResult{OK: true, Steps: []tunnel.CreateStep{{Text: "Enabled pgcrypto", OK: true}}}
			}
			return PGAdminResult{Error: "unexpected " + req.Op}
		},
		Ctl: func(_ context.Context, args []string, _ string) ([]byte, error) {
			ctl = append(ctl, args)
			if args[0] == "list_vhosts" {
				return []byte(`[{"name":"/"},{"name":"/orders"}]`), nil
			}
			return nil, nil
		},
	})

	pg := tunnel.GrantCreate{Op: tunnel.CreateOpCreate, Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432,
		Name: "invoices", Postgres: &tunnel.PGCreate{Extensions: []string{"pgcrypto"}}}
	ans, res := askCreate(t, svc, key, createGrant(pg))
	if !ans.OK || !res.OK || res.Error != "" {
		t.Fatalf("%+v %+v", ans, res)
	}
	if len(asked) != 2 || asked[0].Op != "create-database" || asked[0].Database != "postgres" ||
		asked[1].Op != "prepare-new-database" || asked[1].Database != "invoices" || asked[0].Owner != "ana@corp.example" {
		t.Fatalf("the helper was asked %+v", asked)
	}
	if len(res.Statements) != 2 || !strings.HasPrefix(res.Statements[0], "CREATE DATABASE invoices OWNER argus_owner_invoices") ||
		res.Statements[1] != "CREATE EXTENSION IF NOT EXISTS pgcrypto" {
		t.Fatalf("statements %q", res.Statements)
	}

	// Refused before anything runs: another service's grant, a remote
	// host, a name Validate refuses, Redis.
	asked = nil
	g := tunnel.NewGrant("ana@corp.example", "db-01", "discover", "")
	g.Service = tunnel.DiscoverService
	g.Discover = &tunnel.GrantDiscover{Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432}
	if ans, _ := askCreate(t, svc, key, g); ans.OK || !strings.Contains(ans.Error, "not for creating") {
		t.Fatalf("a discover grant created: %+v", ans)
	}
	for _, c := range []tunnel.GrantCreate{
		{Op: "create", Engine: tunnel.EnginePostgres, Host: "10.0.0.9", Port: 5432, Name: "x"},
		{Op: "create", Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: `x"; DROP DATABASE y; --`},
		{Op: "create", Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: "argus_test_x"},
		{Op: "create", Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: "x",
			Postgres: &tunnel.PGCreate{Owner: "pg_write_server_files"}},
		{Op: "create", Engine: tunnel.EngineRedis, Host: "127.0.0.1", Port: 6379, Name: "3"},
		{Op: "drop", Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432, Name: "x"},
	} {
		if ans, _ := askCreate(t, svc, key, createGrant(c)); ans.OK {
			t.Fatalf("accepted %+v", c)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("the helper ran for a refused grant: %+v", asked)
	}

	// RabbitMQ: a vhost that is there is reported, not made again.
	_, res = askCreate(t, svc, key, createGrant(tunnel.GrantCreate{Op: "create", Engine: tunnel.EngineRabbitMQ,
		Host: "127.0.0.1", Port: 15672, Name: "/orders"}))
	if res.OK || !res.Exists {
		t.Fatalf("an existing vhost: %+v", res)
	}
	for _, a := range ctl {
		if a[0] == "add_vhost" {
			t.Fatalf("add_vhost ran over an existing vhost: %v", ctl)
		}
	}
	ctl = nil
	_, res = askCreate(t, svc, key, createGrant(tunnel.GrantCreate{Op: "create", Engine: tunnel.EngineRabbitMQ,
		Host: "127.0.0.1", Port: 15672, Name: "/payments",
		Rabbit: &tunnel.RabbitCreate{Description: "Payment events", QueueType: "quorum", MaxQueues: 50}}))
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	want := []string{"add_vhost /payments --description Payment events --default-queue-type quorum",
		`set_vhost_limits -p /payments {"max-queues":50}`}
	var got []string
	for _, a := range ctl[1:] {
		got = append(got, strings.Join(a, " "))
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rabbitmqctl got %q, want %q", got, want)
	}
	if len(res.Statements) != 2 {
		t.Fatalf("statements %q", res.Statements)
	}
}

// The helper's create operations against a real Postgres: the database is
// made owned by Argus's Owner level, the extension enabled inside it, a taken
// name refused, and the owner changed.
func TestPostgresCreateDatabaseHelper(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	const name = "argus_c_invoices"
	owner := tunnel.PGLevelRole(tunnel.LevelOwner, name)
	cleanup := func() {
		superTry(t, dsn, "DROP DATABASE IF EXISTS "+name, "DROP ROLE IF EXISTS argus_c_app")
	}
	cleanup()
	t.Cleanup(cleanup)
	superExec(t, dsn, "CREATE ROLE argus_c_app NOLOGIN")

	run := func(op, database string, c tunnel.GrantCreate) PGAdminResult {
		t.Helper()
		cfg, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if database != "" {
			cfg.Database = database
		}
		conn, err := pgconn.ConnectConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		return RunPGAdmin(context.Background(), conn, PGAdminRequest{Op: op, Create: &c})
	}
	c := tunnel.GrantCreate{Op: "create", Engine: tunnel.EnginePostgres, Host: "/var/run/postgresql", Port: 5432,
		Name: name, Postgres: &tunnel.PGCreate{Encoding: "UTF8", Extensions: []string{"pgcrypto"}}}

	o := run("create-options", "", c)
	if !o.OK || o.Options == nil || o.Options.Encoding == "" || len(o.Options.Extensions) == 0 {
		t.Fatalf("options %+v", o)
	}
	r := run("create-database", "", c)
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	if got := superQuery(t, dsn, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '"+name+"'"); len(got) != 1 || got[0][0] != owner {
		t.Fatalf("owner %v, want %s", got, owner)
	}
	p := run("prepare-new-database", name, c)
	if !p.OK {
		t.Fatalf("%+v", p)
	}
	cfg, _ := pgconn.ParseConfig(dsn)
	cfg.Database = name
	conn, err := pgconn.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := conn.Exec(context.Background(), `SELECT extname FROM pg_extension WHERE extname = 'pgcrypto'`).ReadAll()
	conn.Close(context.Background())
	if err != nil || len(res[0].Rows) != 1 {
		t.Fatalf("pgcrypto not enabled: %v %v", err, res)
	}

	// Taken: refused, nothing changed.
	if r := run("create-database", "", c); r.OK || !r.Exists {
		t.Fatalf("a taken name: %+v", r)
	}
	// A role that is not there cannot own it.
	c.Postgres.Owner = "argus_c_nobody"
	if r := run("create-database", "", tunnel.GrantCreate{Op: "create", Engine: c.Engine, Host: c.Host, Port: c.Port,
		Name: name + "_2", Postgres: c.Postgres}); r.OK || !strings.Contains(r.Error, "no role named") {
		t.Fatalf("a missing owner: %+v", r)
	}
	// Change the owner to an application's role, and back.
	so := tunnel.GrantCreate{Op: "set-owner", Engine: c.Engine, Host: c.Host, Port: c.Port, Name: name,
		Postgres: &tunnel.PGCreate{Owner: "argus_c_app"}}
	if r := run("set-owner", "", so); !r.OK {
		t.Fatalf("%+v", r)
	}
	if got := superQuery(t, dsn, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '"+name+"'"); got[0][0] != "argus_c_app" {
		t.Fatalf("owner %v", got)
	}
	so.Postgres.Owner = tunnel.PGOwnerArgus
	if r := run("set-owner", "", so); !r.OK {
		t.Fatalf("%+v", r)
	}
	if got := superQuery(t, dsn, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = '"+name+"'"); got[0][0] != owner {
		t.Fatalf("owner %v", got)
	}
}

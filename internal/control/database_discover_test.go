package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The add form's database list, through a real tunnel to a real agent loop
whose pg-admin helper is a fake: who may ask, what the answer marks as
already added, and how an offline or old agent reads.
*/

func TestDiscoverDatabasesThroughTheAgent(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	ctx := context.Background()
	admin, _ := newAccount(t, e.store, "admin")
	op, _ := newAccount(t, e.store, "operator")
	e.api.UserTokens = map[string]string{"admin-token": admin, "op-token": op}

	host := unique("discover")
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: host, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset.ID) })
	if _, err := e.store.CreateDatabase(ctx, Database{AssetID: asset.ID, Name: "orders"}, admin); err != nil {
		t.Fatal(err)
	}
	url := "/api/v1/assets/" + asset.ID + "/databases/discover?engine=postgres&port=5432"

	// Offline first: no agent yet.
	var ans DiscoverAnswer
	if w := do(t, e.h, http.MethodGet, url, "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	} else if _ = json.Unmarshal(w.Body.Bytes(), &ans); ans.State != "offline" {
		t.Fatalf("no agent read as %+v", ans)
	}

	// This server runs a PgBouncer whose databases are on another server in
	// the inventory: the answer names that server.
	pgHost := unique("discover-pg")
	pgAsset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: pgHost + ".corp.example", Address: "10.9.8.7", Port: 22,
		Protocol: "ssh", Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), pgAsset.ID) })
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/pgbouncer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/pgbouncer/pgbouncer.ini"),
		[]byte("[databases]\nappdb = host=10.9.8.7 port=5432\nother = host=192.0.2.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var asked []agent.PGAdminRequest
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.DiscoverDBService] = agent.DiscoverDatabases(agent.DiscoverDBConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, Log: log, PGRoot: root,
		Admin: func(_ context.Context, req agent.PGAdminRequest) agent.PGAdminResult {
			asked = append(asked, req)
			return agent.PGAdminResult{OK: true, Version: "16.4", Found: []tunnel.Discovered{
				{Name: "orders", Owner: "app", Bytes: 1 << 30}, {Name: "warehouse", Owner: "etl"}}}
		},
	})
	actx, cancel := context.WithCancel(ctx)
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")

	// An operator cannot ask, and the agent is not reached.
	if w := do(t, e.h, http.MethodGet, url, "op-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator listed databases: %d %s", w.Code, w.Body)
	}
	// Nowhere but the server itself, or a Postgres its PgBouncer sends to:
	// the agent refuses any other address without asking anything.
	if w := do(t, e.h, http.MethodGet, url+"&host=10.0.0.5", "admin-token", ""); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "not one this server's PgBouncer sends to") {
		t.Fatalf("a remote host was accepted: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodGet, url+"&host=/tmp/../etc", "admin-token", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("a bad host was accepted: %d %s", w.Code, w.Body)
	}
	if len(asked) != 0 {
		t.Fatalf("the helper ran for a refused request: %+v", asked)
	}

	w := do(t, e.h, http.MethodGet, url, "admin-token", "")
	ans = DiscoverAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if w.Code != http.StatusOK || ans.State != "ok" || len(ans.Databases) != 2 || ans.Version != "16.4" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !ans.Databases[0].Added || ans.Databases[1].Added {
		t.Fatalf("already-added is wrong: %+v", ans.Databases)
	}
	if len(ans.Poolers) != 1 || len(ans.Poolers[0].Targets) != 2 ||
		ans.Poolers[0].Targets[0].AssetID != pgAsset.ID || ans.Poolers[0].Targets[1].AssetID != "" {
		t.Fatalf("PgBouncer's Postgres was not matched to its server: %+v", ans.Poolers)
	}
	if len(asked) != 1 || asked[0].Op != "discover-databases" || asked[0].Socket != "/var/run/postgresql" {
		t.Fatalf("the helper was asked %+v", asked)
	}

	// An agent from before the service: "too old", so the form asks for a name.
	cancel()
	old := unique("discover-old")
	cred2, _ := enrolled(t, e.store, e.h, old)
	asset2, err := e.store.CreateAsset(ctx, AssetInput{Hostname: old, Address: old, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset2.ID) })
	e.runAgent(t, cred2, old)
	e.waitTunnel(t, old, "")
	w = do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset2.ID+"/databases/discover?engine=redis", "admin-token", "")
	ans = DiscoverAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.State != "unsupported" {
		t.Fatalf("an old agent read as %d %s", w.Code, w.Body)
	}
}

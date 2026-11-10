package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Create new, through a real tunnel to a real agent loop whose pg-admin helper
is a fake: who may create, that the entry is added with the server's rules,
that Activity says what ran, and how a taken name, an offline agent and an
old agent read.
*/

func TestCreateDatabaseThroughTheAgent(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	ctx := context.Background()
	admin, _ := newAccount(t, e.store, "admin")
	op, _ := newAccount(t, e.store, "operator")
	e.api.UserTokens = map[string]string{"admin-token": admin, "op-token": op}

	host := unique("create")
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: host, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset.ID) })
	url := "/api/v1/assets/" + asset.ID + "/databases/create"
	body := `{"engine":"postgres","port":5432,"name":"invoices","label":"staging","postgres":{"extensions":["pgcrypto"]}}`

	// Offline: nothing made, said why, recorded as a failure.
	var ans CreateAnswer
	w := do(t, e.h, http.MethodPost, url, "admin-token", body)
	if _ = json.Unmarshal(w.Body.Bytes(), &ans); w.Code != http.StatusOK || ans.State != "offline" {
		t.Fatalf("no agent read as %d %s", w.Code, w.Body)
	}

	var asked []agent.PGAdminRequest
	taken := map[string]bool{"billing": true}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.CreateDBService] = agent.CreateDatabases(agent.CreateDBConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, Log: log,
		Admin: func(_ context.Context, req agent.PGAdminRequest) agent.PGAdminResult {
			asked = append(asked, req)
			switch req.Op {
			case "create-database":
				if taken[req.Create.Name] {
					return agent.PGAdminResult{Exists: true, Error: "this server already has a database named " + req.Create.Name}
				}
				taken[req.Create.Name] = true
				return agent.PGAdminResult{OK: true, Steps: []tunnel.CreateStep{{Text: "Created the database", OK: true}}}
			case "prepare-new-database":
				return agent.PGAdminResult{OK: true, Steps: []tunnel.CreateStep{{Text: "Enabled pgcrypto", OK: true}}}
			case "set-owner":
				return agent.PGAdminResult{OK: true}
			case "create-options":
				return agent.PGAdminResult{OK: true, Options: &tunnel.CreateOptions{Extensions: []string{"pgcrypto"}, Taken: []string{"billing"}}}
			}
			return agent.PGAdminResult{Error: "unexpected " + req.Op}
		},
	})
	actx, cancel := context.WithCancel(ctx)
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")

	// Only an admin, and nothing is asked of the agent otherwise.
	if w := do(t, e.h, http.MethodPost, url, "op-token", body); w.Code != http.StatusForbidden {
		t.Fatalf("an operator created a database: %d %s", w.Code, w.Body)
	}
	// Nothing to create in Redis; nowhere but the server itself.
	if w := do(t, e.h, http.MethodPost, url, "admin-token", `{"engine":"redis","name":"3"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("redis: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, url, "admin-token",
		`{"engine":"postgres","host":"10.0.0.5","name":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a remote host was accepted: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, url, "admin-token",
		`{"engine":"postgres","name":"x","postgres":{"extensions":["pgcrypto; DROP TABLE t"]}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a bad extension was accepted: %d %s", w.Code, w.Body)
	}
	if len(asked) != 0 {
		t.Fatalf("the helper ran for a refused request: %+v", asked)
	}

	// A dry run says what would run and changes nothing.
	w = do(t, e.h, http.MethodPost, url, "admin-token", strings.Replace(body, `{"engine"`, `{"dryRun":true,"engine"`, 1))
	ans = CreateAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.State != "ok" || len(ans.Statements) != 2 || !strings.HasPrefix(ans.Statements[0], "CREATE DATABASE invoices OWNER argus_owner_invoices") || len(asked) != 0 {
		t.Fatalf("dry run: %s (asked %d)", w.Body, len(asked))
	}

	// Options come from the agent.
	w = do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/databases/create-options?engine=postgres&port=5432", "admin-token", "")
	var opts CreateOptionsAnswer
	if _ = json.Unmarshal(w.Body.Bytes(), &opts); opts.State != "ok" || opts.Options == nil || opts.Options.Taken[0] != "billing" {
		t.Fatalf("options: %s", w.Body)
	}

	// Created, added with its label, and in Activity with what ran.
	asked = nil
	w = do(t, e.h, http.MethodPost, url, "admin-token", body)
	ans = CreateAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if w.Code != http.StatusOK || ans.State != "ok" || ans.Database == nil || ans.Database.Name != "invoices" ||
		ans.Database.Label != "staging" {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if len(asked) != 2 || asked[0].Op != "create-database" || asked[1].Op != "prepare-new-database" || asked[1].Database != "invoices" {
		t.Fatalf("the helper was asked %+v", asked)
	}
	dbs, _ := e.store.Databases(ctx, asset.ID)
	if len(dbs) != 1 || dbs[0].Name != "invoices" {
		t.Fatalf("entries %+v", dbs)
	}
	events, err := e.store.AuditEvents(ctx, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var created, added bool
	for _, ev := range events {
		if ev.Action == "db.created" && ev.ActorEmail == admin && ev.Target == host &&
			strings.Contains(ev.Detail, "CREATE DATABASE invoices") && strings.Contains(ev.Detail, "pgcrypto") {
			created = true
		}
		if ev.Action == "db.added" && strings.Contains(ev.Detail, "invoices") {
			added = true
		}
	}
	if !created || !added {
		t.Fatalf("activity is missing the create (%v) or the add (%v)", created, added)
	}

	// A taken name: said so, nothing added.
	w = do(t, e.h, http.MethodPost, url, "admin-token", `{"engine":"postgres","port":5432,"name":"billing"}`)
	ans = CreateAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.State != "exists" || ans.Database != nil {
		t.Fatalf("taken: %s", w.Body)
	}
	if dbs, _ := e.store.Databases(ctx, asset.ID); len(dbs) != 1 {
		t.Fatalf("a taken name was added: %+v", dbs)
	}

	// The owner, later.
	w = do(t, e.h, http.MethodPost, "/api/v1/databases/"+dbs[0].ID+"/owner", "op-token", `{"owner":"app"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an operator changed an owner: %d", w.Code)
	}
	w = do(t, e.h, http.MethodPost, "/api/v1/databases/"+dbs[0].ID+"/owner", "admin-token", `{"owner":"app"}`)
	ans = CreateAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.State != "ok" || len(ans.Statements) != 1 || ans.Statements[0] != "ALTER DATABASE invoices OWNER TO app" {
		t.Fatalf("owner: %s", w.Body)
	}
	if last := asked[len(asked)-1]; last.Op != "set-owner" || last.Create.Postgres.Owner != "app" {
		t.Fatalf("the helper was asked %+v", last)
	}
	events, _ = e.store.AuditEvents(ctx, 50, 0)
	var owned bool
	for _, ev := range events {
		owned = owned || (ev.Action == "db.owner" && strings.Contains(ev.Detail, "to app"))
	}
	if !owned {
		t.Fatal("activity is missing the owner change")
	}

	// A Kafka topic an entry's scope covers adds nothing (dry run: no agent
	// needed to say so).
	if _, err := e.store.CreateDatabase(ctx, Database{AssetID: asset.ID, Engine: tunnel.EngineKafka, Name: "orders."}, admin); err != nil {
		t.Fatal(err)
	}
	w = do(t, e.h, http.MethodPost, url, "admin-token", `{"dryRun":true,"engine":"kafka","name":"orders.refunds","kafka":{"partitions":3}}`)
	ans = CreateAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.CoveredBy == nil || ans.CoveredBy.Name != "orders." {
		t.Fatalf("kafka coverage: %s", w.Body)
	}

	// An agent from before the service: "too old".
	cancel()
	old := unique("create-old")
	cred2, _ := enrolled(t, e.store, e.h, old)
	asset2, err := e.store.CreateAsset(ctx, AssetInput{Hostname: old, Address: old, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset2.ID) })
	e.runAgent(t, cred2, old)
	e.waitTunnel(t, old, "")
	w = do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset2.ID+"/databases/create", "admin-token", body)
	ans = CreateAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.State != "unsupported" {
		t.Fatalf("an old agent read as %d %s", w.Code, w.Body)
	}
}

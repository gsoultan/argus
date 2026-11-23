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
Create new with a new owner user, through a real tunnel to a real agent
loop whose pg-admin helper is a fake: the password reaches the helper only
as a SCRAM verifier, appears in no answer and no Activity line, the answer
carries the connection details once, and an agent from before owners says
"update the agent" instead of making the database without its owner.
*/

func TestCreateDatabaseWithANewOwner(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	ctx := context.Background()
	admin, _ := newAccount(t, e.store, "admin")
	e.api.UserTokens = map[string]string{"admin-token": admin}

	host := unique("owner")
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: host, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset.ID) })

	const pw = "Tr0ub4dor-and-3-more"
	var asked []agent.PGAdminRequest
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	create := agent.CreateDatabases(agent.CreateDBConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, Log: log,
		Admin: func(_ context.Context, req agent.PGAdminRequest) agent.PGAdminResult {
			asked = append(asked, req)
			switch req.Op {
			case "create-database", "prepare-new-database", "set-password":
				return agent.PGAdminResult{OK: true}
			}
			return agent.PGAdminResult{Error: "unexpected " + req.Op}
		},
	})
	services[agent.CreateDBService] = create
	services[agent.CreateDBOwnedService] = create
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")

	url := "/api/v1/assets/" + asset.ID + "/databases/create"
	body := `{"engine":"postgres","port":5432,"name":"shop","owner":{"kind":"new","name":"shop_app"},"password":"` + pw + `"}`

	// Refused before the agent is asked: a short password, an Argus name.
	for _, bad := range []string{
		strings.Replace(body, pw, "short", 1),
		strings.Replace(body, `"shop_app"`, `"argus_x"`, 1),
		strings.Replace(body, `"shop_app"`, `"postgres"`, 1),
	} {
		if w := do(t, e.h, http.MethodPost, url, "admin-token", bad); w.Code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d %s", bad, w.Code, w.Body)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("the helper ran for a refused request: %+v", asked)
	}

	w := do(t, e.h, http.MethodPost, url, "admin-token", body)
	if strings.Contains(w.Body.String(), pw) {
		t.Fatalf("the answer carries the password: %s", w.Body)
	}
	var ans CreateAnswer
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.Database == nil || ans.Connection == nil || ans.Connection.User != "shop_app" || ans.Connection.Database != "shop" {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if len(ans.Statements) == 0 || !strings.Contains(ans.Statements[0], "CREATE ROLE shop_app LOGIN PASSWORD "+tunnel.PasswordRedacted) {
		t.Fatalf("statements: %v", ans.Statements)
	}
	if len(asked) == 0 || asked[0].Op != "create-database" || !strings.HasPrefix(asked[0].Verifier, "SCRAM-SHA-256$") ||
		strings.Contains(asked[0].Verifier, pw) {
		t.Fatalf("the helper was asked %+v", asked)
	}
	events, err := e.store.AuditEvents(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var made bool
	for _, ev := range events {
		if strings.Contains(ev.Detail, pw) {
			t.Fatalf("Activity carries the password: %+v", ev)
		}
		made = made || (ev.Action == "db.user.created" && strings.Contains(ev.Detail, "shop_app"))
	}
	if !made {
		t.Fatal("Activity is missing the new user")
	}

	// A new password, later: the same rules, the same silence.
	asked = nil
	w = do(t, e.h, http.MethodPost, "/api/v1/databases/"+ans.Database.ID+"/owner/password", "admin-token",
		`{"name":"shop_app","password":"`+pw+`-2"}`)
	if strings.Contains(w.Body.String(), pw) {
		t.Fatalf("the answer carries the password: %s", w.Body)
	}
	ans = CreateAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.State != "ok" || len(asked) != 1 || asked[0].Op != "set-password" || !strings.HasPrefix(asked[0].Verifier, "SCRAM-SHA-256$") {
		t.Fatalf("new password: %s (asked %+v)", w.Body, asked)
	}

	// An agent from before owners: "update the agent", nothing made.
	cancel()
	old := unique("owner-old")
	cred2, _ := enrolled(t, e.store, e.h, old)
	asset2, err := e.store.CreateAsset(ctx, AssetInput{Hostname: old, Address: old, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset2.ID) })
	oldServices := agent.DefaultTunnelServices()
	oldServices[agent.CreateDBService] = create
	octx, ocancel := context.WithCancel(ctx)
	defer ocancel()
	go agent.RunTunnel(octx, agent.TunnelConfig{Dial: e.dial(cred2, old), Services: oldServices, Log: log})
	e.waitTunnel(t, old, "")
	asked = nil
	w = do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset2.ID+"/databases/create", "admin-token", body)
	ans = CreateAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.State != "unsupported" || !strings.Contains(ans.Error, "owner user") || len(asked) != 0 {
		t.Fatalf("an old agent read as %d %s", w.Code, w.Body)
	}
}

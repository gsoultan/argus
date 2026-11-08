package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Who is signed in on a Windows server, through a real tunnel to an agent
whose Windows is a fake: who may ask, who may sign out, and that the agent's
refusal of a session in use stands whatever the console asks.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Logons
*/

type fakeWindowsLogons struct {
	mu        sync.Mutex
	logons    []tunnel.Logon
	signedOut []uint32
}

func (f *fakeWindowsLogons) List(context.Context) (tunnel.LogonsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return tunnel.LogonsResult{Platform: "windows", Edition: "server", RemoteLimit: 2,
		Logons: append([]tunnel.Logon(nil), f.logons...)}, nil
}

func (f *fakeWindowsLogons) SignOut(_ context.Context, s uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signedOut = append(f.signedOut, s)
	kept := f.logons[:0]
	for _, l := range f.logons {
		if l.Session != s {
			kept = append(kept, l)
		}
	}
	f.logons = kept
	return nil
}

func TestLogonsThroughTheAgent(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	ctx := context.Background()
	admin, _ := newAccount(t, e.store, "admin")
	op, _ := newAccount(t, e.store, "operator")
	stranger, _ := newAccount(t, e.store, "operator")
	auditor, _ := newAccount(t, e.store, "auditor")
	e.api.UserTokens = map[string]string{"admin-token": admin, "op-token": op,
		"stranger-token": stranger, "auditor-token": auditor}

	host := unique("logons") + ".example"
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.31", Port: 3389,
		Protocol: "rdp", OS: "Windows Server 2022", Principals: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset.ID) })
	if err := e.store.SetAssignment(ctx, asset.ID, op, []string{"ops"}, admin); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/assets/" + asset.ID + "/logons"

	get := func(token string) (int, LogonsAnswer) {
		t.Helper()
		w := do(t, e.h, http.MethodGet, base, token, "")
		var ans LogonsAnswer
		_ = json.Unmarshal(w.Body.Bytes(), &ans)
		return w.Code, ans
	}

	// No agent yet: an answer that says so, not an error.
	if code, ans := get("admin-token"); code != http.StatusOK || ans.State != "offline" {
		t.Fatalf("offline: %d %+v", code, ans)
	}

	fake := &fakeWindowsLogons{logons: []tunnel.Logon{
		{Session: 1, Account: `SRV\admin`, State: tunnel.LogonActive, Console: true},
		{Session: 2, Account: `SRV\ana`, State: tunnel.LogonDisconnected, Remote: true, IdleSeconds: 7200},
		{Session: 3, Account: `SRV\bo`, State: tunnel.LogonActive, Remote: true},
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.LogonsService] = agent.Logons(agent.LogonsConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, Log: log, Host: fake})
	actx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")

	// Whoever may connect sees who is there; only an admin may sign out.
	code, ans := get("op-token")
	if code != http.StatusOK || ans.State != "ok" || len(ans.Logons) != 3 || ans.RemoteLimit != 2 || ans.CanSignOut {
		t.Fatalf("operator: %d %+v", code, ans)
	}
	if code, ans := get("auditor-token"); code != http.StatusOK || ans.State != "ok" || ans.CanSignOut {
		t.Fatalf("auditor: %d %+v", code, ans)
	}
	if code, ans := get("admin-token"); code != http.StatusOK || !ans.CanSignOut {
		t.Fatalf("admin: %d %+v", code, ans)
	}
	// Someone with no way onto the server learns nothing, not even that it is there.
	if code, _ := get("stranger-token"); code != http.StatusNotFound {
		t.Fatalf("a stranger asked: %d", code)
	}

	signOut := func(token, body string) (int, LogonsAnswer) {
		t.Helper()
		w := do(t, e.h, http.MethodPost, base+"/signout", token, body)
		var ans LogonsAnswer
		_ = json.Unmarshal(w.Body.Bytes(), &ans)
		return w.Code, ans
	}
	if code, _ := signOut("op-token", `{"session":2,"account":"SRV\\ana"}`); code != http.StatusForbidden {
		t.Fatalf("an operator signed someone out: %d", code)
	}
	if code, _ := signOut("admin-token", `{"session":0,"account":"SRV\\ana"}`); code != http.StatusBadRequest {
		t.Fatalf("session 0: %d", code)
	}
	// In use: the agent refuses, whatever the console asked.
	if code, ans := signOut("admin-token", `{"session":3,"account":"SRV\\bo"}`); code != http.StatusOK ||
		ans.State != "failed" || ans.Error == "" {
		t.Fatalf("in use: %d %+v", code, ans)
	}
	if code, ans := signOut("admin-token", `{"session":2,"account":"SRV\\ana"}`); code != http.StatusOK ||
		ans.State != "ok" || ans.SignedOut != 2 {
		t.Fatalf("idle: %d %+v", code, ans)
	}
	fake.mu.Lock()
	if len(fake.signedOut) != 1 || fake.signedOut[0] != 2 {
		t.Fatalf("signed out %v; want only session 2", fake.signedOut)
	}
	fake.mu.Unlock()
	if n, _ := e.store.AuditCountFor(ctx, "asset.session_signed_out", host); n != 1 {
		t.Errorf("sign-outs audited: %d", n)
	}
	if n, _ := e.store.AuditCountFor(ctx, "asset.session_sign_out_refused", host); n != 1 {
		t.Errorf("refusals audited: %d", n)
	}
}

package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

// A reach check runs on the client's server through its agent, for admins
// only, and lands in the audit log.
func TestReachCheckFromAnotherServer(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	ctx := context.Background()
	admin, _ := newAccount(t, e.store, "admin")
	op, _ := newAccount(t, e.store, "operator")
	e.api.UserTokens = map[string]string{"admin-token": admin, "op-token": op}

	host := unique("hermod")
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: host, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset.ID) })

	// Something listening, standing in for a Redis that asks for a password.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 64)
			_, _ = c.Read(buf)
			_, _ = c.Write([]byte("-NOAUTH Authentication required.\r\n"))
			_ = c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	services := agent.DefaultTunnelServices()
	services[agent.ReachCheckService] = agent.ReachCheck(agent.ReachCheckConfig{Verifier: &tunnel.GrantVerifier{Host: host}, Log: log})
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")

	url := "/api/v1/assets/" + asset.ID + "/reach-check"
	body := `{"host":"127.0.0.1","port":` + strconv.Itoa(port) + `,"engine":"redis"}`
	if w := do(t, e.h, http.MethodPost, url, "op-token", body); w.Code != http.StatusForbidden {
		t.Fatalf("an operator checked: %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, http.MethodPost, url, "admin-token", `{"host":"cache-01","port":6379}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a name was accepted: %d %s", w.Code, w.Body)
	}
	w := do(t, e.h, http.MethodPost, url, "admin-token", body)
	var ans ReachCheckAnswer
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if w.Code != http.StatusOK || ans.State != "ok" || !ans.Connected || ans.Answer != "signin" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !auditHas(t, e.store, "asset.reach_check") {
		t.Fatal("the check is not in the audit log")
	}
	_ = l.Close()
	w = do(t, e.h, http.MethodPost, url, "admin-token", body)
	ans = ReachCheckAnswer{}
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if ans.State != "ok" || ans.Connected || !strings.Contains(ans.Detail, "connection refused") {
		t.Fatalf("closed: %s", w.Body)
	}
}

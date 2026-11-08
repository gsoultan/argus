package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A workstation's desktop opens only after the person using it says yes; a
server's opens without asking anyone. The question itself is the Windows
drill's; here it is a fake that answers as told.
*/

type answeringAsker struct {
	mu    sync.Mutex
	allow bool
	asks  []tunnel.GrantAsk
}

func (a *answeringAsker) InUse(context.Context) ([]tunnel.Logon, error) {
	return []tunnel.Logon{{Session: 1, Account: `PC\sam`, State: tunnel.LogonActive, Console: true}}, nil
}
func (a *answeringAsker) Ask(context.Context, tunnel.Logon, string, string, int) (bool, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.allow, true, nil
}
func (a *answeringAsker) Tell(context.Context, tunnel.Logon, string, string) error { return nil }

func TestAWorkstationDesktopAsksThePersonUsingIt(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	open := func(kind string, allow bool) (frame []byte, msgs []terminalMessage, asked []tunnel.GrantAsk) {
		t.Helper()
		host := unique(kind)
		cred, _ := enrolled(t, e.store, e.h, host)
		if _, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Protocol: "rdp", Kind: kind,
			Principals: []string{"ana"}}); err != nil {
			t.Fatal(err)
		}
		asker := &answeringAsker{allow: allow}
		var mu sync.Mutex
		services := agent.DefaultTunnelServices()
		services[agent.DesktopService] = agent.Desktop(agent.DesktopConfig{
			Asker:     asker,
			Collector: agent.NewCollector(t.TempDir(), log), Verifier: &tunnel.GrantVerifier{Host: host},
			Hostname: host, Log: log,
			Connect: func(_ agent.DesktopConfig, g tunnel.Grant, _ tunnel.DesktopOpen,
				_ *slog.Logger) (agent.DesktopClient, agent.DesktopInfo, error) {
				mu.Lock()
				if g.Ask != nil {
					asked = append(asked, *g.Ask)
				}
				mu.Unlock()
				if g.Ask == nil && kind == "workstation" {
					return nil, agent.DesktopInfo{}, errors.New("a workstation grant without a question")
				}
				return newFakeDesktop(), agent.DesktopInfo{Protocol: rdp.ProtocolHybrid}, nil
			},
		})
		actx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
		e.waitTunnel(t, host, "")

		ticket, _ := signer.IssueTicket("ana@example.com", host, "ana", time.Minute)
		c := e.openDesktop(t, host, "ana", ticket)
		t.Cleanup(func() { c.CloseNow() })
		b, _ := json.Marshal(map[string]any{"type": "logon", "password": "pw", "width": 1024, "height": 768})
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
		frame = nextBinary(pump(c), &msgs, 10*time.Second)
		if n, _ := e.store.AuditCountFor(ctx, "session.declined_at_machine", host); (n == 1) != (kind == "workstation" && !allow) {
			t.Errorf("%s allow=%v: declines audited %d", kind, allow, n)
		}
		return frame, msgs, asked
	}

	// A server: nobody asked.
	frame, msgs, asked := open("server", false)
	if len(frame) == 0 || frame[0] != rdp.FrameReady || len(asked) != 0 {
		t.Fatalf("server: %v %+v asked %v", frame, msgs, asked)
	}
	// A workstation, allowed.
	frame, msgs, asked = open("workstation", true)
	if len(frame) == 0 || frame[0] != rdp.FrameReady || len(asked) != 1 ||
		asked[0].Mode != tunnel.AskFirst || asked[0].Who != "ana@example.com" || asked[0].Seconds != workstationAskSeconds {
		t.Fatalf("allowed: %v %+v asked %+v", frame, msgs, asked)
	}
	// A workstation, declined: the browser hears who said no, and nothing opens.
	frame, msgs, asked = open("workstation", false)
	if len(frame) == 0 || frame[0] != rdp.FrameClosed || len(msgs) == 0 ||
		!strings.Contains(msgs[0].Data, "sam said no") || len(asked) != 0 {
		t.Fatalf("declined: %v %+v asked %v", frame, msgs, asked)
	}
}

func TestConnectRulesPerTypeAndPerAsset(t *testing.T) {
	e := newTunnelEnv(t)
	ctx := context.Background()
	admin, _ := newAccount(t, e.store, "admin")
	auditor, _ := newAccount(t, e.store, "auditor")
	e.api.UserTokens = map[string]string{"admin-token": admin, "auditor-token": auditor}
	t.Cleanup(func() { _ = e.store.SetConnectRules(context.Background(), DefaultConnectRules(), "test") })
	_ = e.store.SetConnectRules(ctx, DefaultConnectRules(), "test")

	mk := func(kind string, own *string) string {
		t.Helper()
		host := unique(kind)
		if _, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Protocol: "rdp", Kind: kind,
			Principals: []string{"ana"}, ConnectAsk: own}); err != nil {
			t.Fatal(err)
		}
		return host
	}
	mode := func(host string, desktop bool) string {
		t.Helper()
		ask, err := e.api.askFor(ctx, host, "ana@example.com", desktop)
		if err != nil {
			t.Fatal(err)
		}
		if ask == nil {
			return ConnectNever
		}
		return ask.Mode
	}
	never, ask := ConnectNever, ConnectAsk
	srv, ws, kiosk, askedServer := mk("server", nil), mk("workstation", nil), mk("workstation", &never), mk("server", &ask)

	for _, c := range []struct {
		host    string
		desktop bool
		want    string
	}{
		{srv, true, ConnectNever}, {srv, false, ConnectNever},
		{ws, true, tunnel.AskFirst}, {ws, false, tunnel.TellOnly},
		{kiosk, true, ConnectNever}, {kiosk, false, ConnectNever},
		// Asking is never a terminal's rule: it tells instead.
		{askedServer, true, tunnel.AskFirst}, {askedServer, false, tunnel.TellOnly},
	} {
		if got := mode(c.host, c.desktop); got != c.want {
			t.Errorf("%s desktop=%v: %s, want %s", c.host, c.desktop, got, c.want)
		}
	}

	// A device has no setting of its own here.
	bad := "ask"
	if _, err := e.store.CreateAsset(ctx, AssetInput{Hostname: unique("phone"), Kind: "android", ConnectAsk: &bad}); err == nil {
		t.Error("a device took a before-connecting setting")
	}

	// The rules through the API: an auditor reads, only an admin writes.
	w := do(t, e.h, http.MethodGet, "/api/v1/settings/asset-types", "auditor-token", "")
	var got ConnectRules
	if _ = json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || got.Workstation.Desktop != ConnectAsk {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	body := `{"server":{"desktop":"tell","terminal":"never","seconds":60},"workstation":{"desktop":"ask","terminal":"tell","seconds":30}}`
	if w := do(t, e.h, http.MethodPut, "/api/v1/settings/asset-types", "auditor-token", body); w.Code != http.StatusForbidden {
		t.Fatalf("an auditor wrote: %d", w.Code)
	}
	for _, b := range []string{
		`{"server":{"desktop":"ask","terminal":"ask","seconds":60},"workstation":{"desktop":"ask","terminal":"tell","seconds":30}}`,
		`{"server":{"desktop":"never","terminal":"never","seconds":5},"workstation":{"desktop":"ask","terminal":"tell","seconds":30}}`,
	} {
		if w := do(t, e.h, http.MethodPut, "/api/v1/settings/asset-types", "admin-token", b); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", b, w.Code)
		}
	}
	if w := do(t, e.h, http.MethodPut, "/api/v1/settings/asset-types", "admin-token", body); w.Code != http.StatusOK {
		t.Fatalf("write: %d %s", w.Code, w.Body)
	}
	if got := mode(srv, true); got != tunnel.TellOnly {
		t.Errorf("a server after the rule changed: %s", got)
	}
	if a, _ := e.api.askFor(ctx, ws, "ana@example.com", true); a == nil || a.Seconds != 30 {
		t.Errorf("a workstation's wait after the rule changed: %+v", a)
	}
	if n, _ := e.store.AuditCountFor(ctx, "settings.connect_rules", "asset types"); n < 1 {
		t.Error("the change was not audited")
	}
}

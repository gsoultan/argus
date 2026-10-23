package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
Console-only: the deployment's rule that every session opens from the
console. The control plane holds it (gateway_policy.allow_native_clients) and
hands it to agents on the heartbeat, which is where a direct sshd login is
refused. These hold the refusal side: closed by default, carried to the agent,
and a refused login written into the audit chain.
*/

func heartbeatConsoleOnly(t *testing.T, h http.Handler, credential, hostname, applied string) *bool {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", credential,
		`{"hostname":"`+hostname+`","version":"1.0.0","active_sessions":0,"console_only":`+applied+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out struct {
		ConsoleOnly *bool `json:"console_only"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.ConsoleOnly
}

func TestConsoleOnlyIsTheDefaultAndReachesTheAgent(t *testing.T) {
	s, h, _ := scopeAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	// The policy row is shared by every test on this database: start from
	// the default and leave it there.
	if _, err := s.SaveGatewayPolicy(context.Background(), DefaultGatewayPolicy(), "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.SaveGatewayPolicy(context.Background(), DefaultGatewayPolicy(), "test")
	})
	if got := heartbeatConsoleOnly(t, h, cred, asset.Hostname, "true"); got == nil || !*got {
		t.Fatalf("the agent was not told to refuse direct logins: %v", got)
	}
	a, _ := s.AssetByID(context.Background(), asset.ID)
	if a.AgentConsoleOnly == nil || !*a.AgentConsoleOnly {
		t.Errorf("the console cannot see the agent applied it: %v", a.AgentConsoleOnly)
	}

	p := DefaultGatewayPolicy()
	p.AllowNativeClients = true
	if _, err := s.SaveGatewayPolicy(context.Background(), p, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := heartbeatConsoleOnly(t, h, cred, asset.Hostname, "false"); got == nil || *got {
		t.Errorf("after allowing native clients the agent was told %v", got)
	}
}

// An agent older than console-only says nothing, and that is unconfirmed --
// never shown as applied.
func TestAnAgentThatDoesNotSayIsUnconfirmed(t *testing.T) {
	s, h, _ := scopeAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	heartbeatScope(t, h, cred, asset.Hostname, ScopeEveryLogin)
	a, _ := s.AssetByID(context.Background(), asset.ID)
	if a.AgentConsoleOnly != nil {
		t.Errorf("an agent that never said is shown as %v", *a.AgentConsoleOnly)
	}
}

func TestARefusedDirectLoginIsInTheAuditChain(t *testing.T) {
	s, h, _ := scopeAPI(t)
	if _, err := s.SaveGatewayPolicy(context.Background(), DefaultGatewayPolicy(), "test"); err != nil {
		t.Fatal(err)
	}
	asset, cred := enrolledAsset(t, s, h)

	w := do(t, h, http.MethodPost, "/api/v1/report/login", cred,
		`{"hostname":"`+asset.Hostname+`","principal":"ops","client_addr":"203.0.113.7",`+
			`"command":"scp -t /tmp","origin":"direct","scope":"console_only","refused":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	e := lastAuditOn(t, s, asset.Hostname)
	if e.Action != "session.direct_refused" || e.Severity != "warning" {
		t.Errorf("audit = %s/%s", e.Action, e.Severity)
	}
	for _, want := range []string{"ops", "203.0.113.7", "scp -t /tmp", "console-only"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("entry does not carry %q: %s", want, e.Detail)
		}
	}
	if strings.Contains(e.Detail, "not following") {
		t.Errorf("a refusal under console-only is reported as disobeying it: %s", e.Detail)
	}
}

// Opening native clients is a loosening, filed where an investigator looks.
func TestAllowingNativeClientsIsALoosening(t *testing.T) {
	before := DefaultGatewayPolicy()
	after := before
	after.AllowNativeClients = true
	changes := DiffGatewayPolicy(before, after)
	if len(changes) != 1 || !changes[0].Loosening {
		t.Fatalf("changes = %+v, want one loosening", changes)
	}
}

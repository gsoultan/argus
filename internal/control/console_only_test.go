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
console, with no setting to turn it off. The control plane hands it to agents
on the heartbeat, which is where a direct sshd login is refused. These hold
the refusal side: always on, carried to the agent, and a refused login written
into the audit chain.
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

func TestConsoleOnlyAlwaysReachesTheAgent(t *testing.T) {
	s, h, _ := scopeAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	if got := heartbeatConsoleOnly(t, h, cred, asset.Hostname, "true"); got == nil || !*got {
		t.Fatalf("the agent was not told to refuse direct logins: %v", got)
	}
	a, _ := s.AssetByID(context.Background(), asset.ID)
	if a.AgentConsoleOnly == nil || !*a.AgentConsoleOnly {
		t.Errorf("the console cannot see the agent applied it: %v", a.AgentConsoleOnly)
	}

	// An agent that last heard otherwise, from an older control plane, is
	// told to refuse again.
	if got := heartbeatConsoleOnly(t, h, cred, asset.Hostname, "false"); got == nil || !*got {
		t.Errorf("an agent reporting console-only off was told %v", got)
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
}

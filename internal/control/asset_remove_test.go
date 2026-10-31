package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
Removing a server from Argus takes its agent off it too.

Archiving the asset used to leave the agent running with sshd's hook in place:
direct logins stayed refused and the console could no longer open the server,
so nothing but its own console reached it.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Removed
*/

func removedHeartbeat(t *testing.T, h http.Handler, cred, host string) map[string]any {
	t.Helper()
	w := heartbeatAs(t, h, cred, host)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestARemovedServersAgentIsToldToUninstall(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	if r := removedHeartbeat(t, h, cred, asset.Hostname); r["uninstall"] != nil {
		t.Fatalf("an agent was told to uninstall before anyone removed its server: %v", r)
	}
	w := do(t, h, http.MethodDelete, "/api/v1/assets/"+asset.ID, "admin-token", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"agent":"uninstalling"`) {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "asset.archived" ||
		!strings.Contains(e.Detail, "installing the agent again") {
		t.Errorf("the removal should say the agent goes and must be installed again: %s %s",
			e.Action, e.Detail)
	}
	// The asset is gone, so this agent is now unmatched; it still hears it.
	if r := removedHeartbeat(t, h, cred, asset.Hostname); r["uninstall"] != true {
		t.Fatalf("the agent of a removed server was not told to uninstall: %v", r)
	}

	// The agent finishes by revoking itself, saying who can still get in.
	w = do(t, h, http.MethodPost, "/api/v1/report/deregister", cred,
		`{"hostname":"`+asset.Hostname+`","ran_by":"the Argus console (server removed)",`+
			`"ssh_ways":["ana with 1 key(s)"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("deregister: %d %s", w.Code, w.Body)
	}
	e := lastAuditOn(t, s, asset.Hostname)
	if e.Action != "agent.uninstalled" || !strings.Contains(e.Detail, "plain SSH works for: ana with 1 key(s)") {
		t.Errorf("audit = %s: %s", e.Action, e.Detail)
	}
	if w := heartbeatAs(t, h, cred, asset.Hostname); w.Code != http.StatusUnauthorized {
		t.Errorf("an uninstalled agent still reports: %d", w.Code)
	}
}

// A server reporting that nobody can SSH in any more is said loudly.
func TestAnUninstallThatLeavesNoWayInSaysSo(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	w := do(t, h, http.MethodPost, "/api/v1/report/deregister", cred,
		`{"hostname":"`+asset.Hostname+`","ssh_ways":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("deregister: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); !strings.Contains(e.Detail, "nobody can log in") {
		t.Errorf("detail = %s", e.Detail)
	}
}

// Only admins and owners remove a server, and so only they can make an agent
// take itself off.
func TestAnOperatorCannotRemoveAServerOrItsAgent(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	if w := do(t, h, http.MethodDelete, "/api/v1/assets/"+asset.ID, "operator-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("an operator removed a server: %d %s", w.Code, w.Body)
	}
	if r := removedHeartbeat(t, h, cred, asset.Hostname); r["uninstall"] != nil {
		t.Errorf("a refused removal still told the agent to uninstall: %v", r)
	}
}

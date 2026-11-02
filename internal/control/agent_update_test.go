package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
Updating an agent from the console: asked on the server page, carried on the
heartbeat, cleared and audited once the agent says how it went.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run AgentUpdate
*/

func updateAPI(t *testing.T) (*Store, http.Handler, *API) {
	t.Helper()
	a, s, h, _ := enrollAPI(t)
	operator, _ := newAccount(t, s, "operator")
	a.UserTokens["operator-token"] = operator
	a.Version = "v1.1.0"
	a.AgentReleasesURL = "https://mirror.example/argus/"
	return s, h, a
}

// beatWith sends a heartbeat carrying an update status and returns the reply.
func beatWith(t *testing.T, h http.Handler, cred, host, version, update string) map[string]any {
	t.Helper()
	body := `{"hostname":"` + host + `","version":"` + version + `","active_sessions":0`
	if update != "" {
		body += `,"update":` + update
	}
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestAgentUpdateRidesTheHeartbeatUntilTheAgentAnswers(t *testing.T) {
	s, h, _ := updateAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/agent/update"

	if r := beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`); r["update"] != nil {
		t.Fatalf("an agent was told to update before anyone asked: %v", r)
	}
	w := do(t, h, http.MethodPost, path, "admin-token", `{}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}
	var req AgentUpdateRequest
	_ = json.Unmarshal(w.Body.Bytes(), &req)
	if req.Version != "1.1.0" || req.ID == "" || req.Now {
		t.Fatalf("the update should default to this control plane's version, when idle: %+v", req)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "agent.update_requested" ||
		!strings.Contains(e.Detail, "from 1.0.0 to 1.1.0, once nothing is open") {
		t.Errorf("audit = %s: %s", e.Action, e.Detail)
	}

	r := beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`)
	u, _ := r["update"].(map[string]any)
	if u["id"] != req.ID || u["version"] != "1.1.0" || u["now"] != false ||
		u["url"] != "https://mirror.example/argus" {
		t.Fatalf("the heartbeat reply should carry the request: %v", r)
	}

	// Asking again for the same version keeps the request the agent is
	// working on, and "now" only adds urgency.
	w = do(t, h, http.MethodPost, path, "admin-token", `{"now":true}`)
	var again AgentUpdateRequest
	_ = json.Unmarshal(w.Body.Bytes(), &again)
	if again.ID != req.ID || !again.Now {
		t.Errorf("asking now should keep the request and add urgency: %+v", again)
	}

	// While it waits, it is still asked; the status is kept for the page.
	waiting := `{"supported":true,"id":"` + req.ID + `","state":"waiting","sessions":2,` +
		`"from":"1.0.0","target":"1.1.0","message":"waiting for 2 open session(s) to end"}`
	if r := beatWith(t, h, cred, asset.Hostname, "1.0.0", waiting); r["update"] == nil {
		t.Fatalf("a waiting update should still be asked for: %v", r)
	}
	w = do(t, h, http.MethodGet, path, "admin-token", "")
	var view AgentUpdateView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.Status == nil || view.Status.State != "waiting" || view.Status.Sessions != 2 ||
		view.Request == nil || view.Latest != "1.1.0" || view.AgentVersion != "1.0.0" {
		t.Fatalf("view = %+v (status %+v)", view, view.Status)
	}

	// The new version reports it is done: the request is cleared, once.
	done := `{"supported":true,"id":"` + req.ID + `","state":"done","from":"1.0.0",` +
		`"target":"1.1.0","message":"updated from 1.0.0 to 1.1.0; back in 3s"}`
	if r := beatWith(t, h, cred, asset.Hostname, "1.1.0", done); r["update"] != nil {
		t.Fatalf("a finished update was asked for again: %v", r)
	}
	e := lastAuditOn(t, s, asset.Hostname)
	if e.Action != "agent.updated" || !strings.Contains(e.Detail, "back in 3s") {
		t.Errorf("audit = %s: %s", e.Action, e.Detail)
	}
	beatWith(t, h, cred, asset.Hostname, "1.1.0", done)
	if again := lastAuditOn(t, s, asset.Hostname); again.Detail != e.Detail || again.Action != e.Action {
		t.Errorf("an outcome repeated on the next heartbeat was audited again: %s", again.Detail)
	}
	var n int
	_ = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events
		WHERE target = $1 AND action = 'agent.updated'`, asset.Hostname).Scan(&n)
	if n != 1 {
		t.Errorf("agent.updated audited %d times, want 1", n)
	}
}

func TestAgentUpdateThatWentBackIsAuditedAsAWarning(t *testing.T) {
	s, h, _ := updateAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`)
	w := do(t, h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/agent/update", "admin-token", `{}`)
	var req AgentUpdateRequest
	_ = json.Unmarshal(w.Body.Bytes(), &req)

	back := `{"supported":true,"id":"` + req.ID + `","state":"rolled_back",` +
		`"message":"1.1.0 started but did not reconnect to Argus within 1m30s; went back to 1.0.0"}`
	if r := beatWith(t, h, cred, asset.Hostname, "1.0.0", back); r["update"] != nil {
		t.Fatalf("a rolled back update was asked for again, which would loop: %v", r)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "agent.update_rolled_back" ||
		e.Severity != "warn" || !strings.Contains(e.Detail, "went back to 1.0.0") {
		t.Errorf("audit = %s %s: %s", e.Action, e.Severity, e.Detail)
	}
}

// An agent that cannot update itself -- too old to say, or on a server it
// cannot update -- is not asked, and the console is told why.
func TestAgentUpdateIsRefusedWhenTheAgentCannotDoIt(t *testing.T) {
	s, h, _ := updateAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/agent/update"

	heartbeatAs(t, h, cred, asset.Hostname) // an agent older than updates
	w := do(t, h, http.MethodPost, path, "admin-token", `{}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "too old to update itself") {
		t.Errorf("old agent: %d %s", w.Code, w.Body)
	}

	beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":false,"reason":"this server does not run systemd"}`)
	w = do(t, h, http.MethodPost, path, "admin-token", `{}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "does not run systemd") {
		t.Errorf("unsupported: %d %s", w.Code, w.Body)
	}

	beatWith(t, h, cred, asset.Hostname, "1.1.0", `{"supported":true}`)
	w = do(t, h, http.MethodPost, path, "admin-token", `{}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already runs 1.1.0") {
		t.Errorf("current: %d %s", w.Code, w.Body)
	}

	w = do(t, h, http.MethodPost, path, "admin-token", `{"version":"1.2.0; rm -rf /"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a version that is not a release was accepted: %d %s", w.Code, w.Body)
	}
	if r := beatWith(t, h, cred, asset.Hostname, "1.1.0", `{"supported":true}`); r["update"] != nil {
		t.Errorf("a refused request still reached the agent: %v", r)
	}
}

func TestAnOperatorCannotUpdateAnAgent(t *testing.T) {
	s, h, _ := updateAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`)
	path := "/api/v1/assets/" + asset.ID + "/agent/update"
	if w := do(t, h, http.MethodPost, path, "operator-token", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("an operator updated an agent: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodGet, path, "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator read the agent card: %d", w.Code)
	}
	if r := beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`); r["update"] != nil {
		t.Errorf("a refused request still reached the agent: %v", r)
	}
}

func TestAgentUpdateCanBeTakenBack(t *testing.T) {
	s, h, _ := updateAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`)
	path := "/api/v1/assets/" + asset.ID + "/agent/update"
	do(t, h, http.MethodPost, path, "admin-token", `{"version":"1.2.0"}`)
	if w := do(t, h, http.MethodDelete, path, "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if r := beatWith(t, h, cred, asset.Hostname, "1.0.0", `{"supported":true}`); r["update"] != nil {
		t.Errorf("a cancelled update was still asked for: %v", r)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "agent.update_cancelled" {
		t.Errorf("audit = %s", e.Action)
	}
	if w := do(t, h, http.MethodDelete, path, "admin-token", ""); w.Code != http.StatusConflict {
		t.Errorf("cancelling nothing: %d", w.Code)
	}
}

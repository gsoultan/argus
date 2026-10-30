package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
Direct logins, as the control plane hears of them.

Every server is console-only, so an agent reports a direct login either as
refused (console_only_test.go) or, if it is not following that, as let through
-- and the second is written down at the highest severity, by the agent of that
host and no other.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Login
*/

func reportAPI(t *testing.T) (*Store, http.Handler, string) {
	t.Helper()
	a, s, h, admin := enrollAPI(t)
	operator, _ := newAccount(t, s, "operator")
	a.UserTokens["operator-token"] = operator
	return s, h, admin
}

// enrolledAsset enrolls an agent for a new asset. Enrolment first: it clears
// any asset already holding the hostname.
func enrolledAsset(t *testing.T, s *Store, h http.Handler) (Asset, string) {
	t.Helper()
	host := unique("login") + ".example"
	cred, _ := enrolled(t, s, h, host)
	asset, err := s.CreateAsset(context.Background(), AssetInput{
		Hostname: host, Address: "192.0.2.10", Principals: []string{"ops"},
		CredentialRef: "injected",
	})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	return asset, cred
}

func lastAuditOn(t *testing.T, s *Store, target string) AuditEvent {
	t.Helper()
	var e AuditEvent
	err := s.pool.QueryRow(context.Background(), `
		SELECT action, severity, actor_email, target, detail FROM audit_events
		 WHERE target = $1 ORDER BY seq DESC LIMIT 1`, target).
		Scan(&e.Action, &e.Severity, &e.ActorEmail, &e.Target, &e.Detail)
	if err != nil {
		t.Fatalf("no audit entry on %s: %v", target, err)
	}
	return e
}

// An agent that let a direct login through is not following console-only, and
// the entry says so.
func TestALoginLetThroughIsCriticalInTheAuditChain(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	w := do(t, h, http.MethodPost, "/api/v1/report/login", cred,
		`{"hostname":"`+asset.Hostname+`","principal":"ops","client_addr":"203.0.113.7",`+
			`"origin":"direct","scope":"argus_only"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	e := lastAuditOn(t, s, asset.Hostname)
	if e.Action != "session.direct_unrecorded" || e.Severity != "critical" {
		t.Errorf("audit = %s/%s", e.Action, e.Severity)
	}
	for _, want := range []string{"ops", "203.0.113.7", "no replay", "not following"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("entry does not carry %q: %s", want, e.Detail)
		}
	}
}

func TestAnAgentReportsLoginsOnlyForItsOwnHost(t *testing.T) {
	s, h, _ := reportAPI(t)
	_, cred := enrolledAsset(t, s, h)
	other := testAsset(t, s, "ops")

	w := do(t, h, http.MethodPost, "/api/v1/report/login", cred,
		`{"hostname":"`+other.Hostname+`","principal":"ops","origin":"direct"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an agent wrote a login against another host: %d", w.Code)
	}
}

// An agent older than the removal still sends the scope it applies. Its
// heartbeat is accepted, and the reply no longer carries a scope -- which that
// agent reads as every_login.
func TestAnOlderAgentsHeartbeatIsStillAccepted(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred,
		`{"hostname":"`+asset.Hostname+`","version":"1.0.0","active_sessions":0,"recording_scope":"argus_only"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if _, ok := out["recording_scope"]; ok {
		t.Errorf("the reply still carries a recording scope: %v", out)
	}
	if out["console_only"] != true {
		t.Errorf("the reply does not say console-only: %v", out)
	}
}

// The route the console used to change the setting is gone.
func TestTheRecordingScopeRouteIsGone(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, _ := enrolledAsset(t, s, h)
	w := do(t, h, http.MethodPut, "/api/v1/assets/"+asset.ID+"/recording-scope", "admin-token",
		`{"recordingScope":"argus_only"}`)
	if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT recording-scope = %d, want it gone", w.Code)
	}
}

// A login sshd let in while the agent was down is reported as exactly that,
// not as an agent ignoring its setting.
func TestALoginWhileTheAgentWasDownIsCriticalAndSaysWhy(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	w := do(t, h, http.MethodPost, "/api/v1/report/login", cred,
		`{"hostname":"`+asset.Hostname+`","principal":"ana","client_addr":"203.0.113.9",`+
			`"origin":"direct","agent_down":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	e := lastAuditOn(t, s, asset.Hostname)
	if e.Action != "session.direct_unrecorded" || e.Severity != "critical" {
		t.Errorf("audit = %s/%s", e.Action, e.Severity)
	}
	for _, want := range []string{"ana", "203.0.113.9", "was not running", "no replay"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("entry does not carry %q: %s", want, e.Detail)
		}
	}
	if strings.Contains(e.Detail, "not following") {
		t.Errorf("an agent that was down is not one ignoring its setting: %s", e.Detail)
	}
}

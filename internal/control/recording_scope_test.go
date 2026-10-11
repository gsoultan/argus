package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
The recording scope, as the control plane holds it.

The refusals: only an administrator moves a host to "Argus sessions only", the
move is in the audit chain with who made it, an agent the console has said
nothing to is told every_login, and a login the agent let through is written
down -- by the agent of that host, and no other.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Scope
*/

func scopeAPI(t *testing.T) (*Store, http.Handler, string) {
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
	host := unique("scope") + ".example"
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

func setScope(t *testing.T, h http.Handler, token, id, scope string) int {
	t.Helper()
	return do(t, h, http.MethodPut, "/api/v1/assets/"+id+"/recording-scope", token,
		`{"recordingScope":"`+scope+`"}`).Code
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

func heartbeatScope(t *testing.T, h http.Handler, credential, hostname, applied string) string {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", credential,
		`{"hostname":"`+hostname+`","version":"1.0.0","active_sessions":0,"recording_scope":"`+applied+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out struct {
		RecordingScope string `json:"recording_scope"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.RecordingScope
}

func TestOnlyAnAdministratorChangesWhatAHostRecords(t *testing.T) {
	s, h, _ := scopeAPI(t)
	asset := testAsset(t, s, "ops")

	if code := setScope(t, h, "operator-token", asset.ID, ScopeArgusOnly); code != http.StatusForbidden {
		t.Fatalf("an operator changed the scope: %d", code)
	}
	if code := setScope(t, h, "admin-token", asset.ID, "nothing"); code != http.StatusBadRequest {
		t.Errorf("an unknown scope was accepted: %d", code)
	}
	got, _ := s.AssetByID(context.Background(), asset.ID)
	if got.RecordingScope != ScopeEveryLogin {
		t.Errorf("scope = %q after two refusals, want every_login", got.RecordingScope)
	}
}

func TestLooseningTheScopeIsAudited(t *testing.T) {
	s, h, admin := scopeAPI(t)
	asset := testAsset(t, s, "ops")

	if code := setScope(t, h, "admin-token", asset.ID, ScopeArgusOnly); code != http.StatusOK {
		t.Fatalf("set: %d", code)
	}
	e := lastAuditOn(t, s, asset.Hostname)
	if e.Action != "asset.recording_scope_changed" || e.Severity != "warning" {
		t.Errorf("audit = %s/%s, want asset.recording_scope_changed/warning", e.Action, e.Severity)
	}
	if e.ActorEmail != admin {
		t.Errorf("audit actor = %q, want %q", e.ActorEmail, admin)
	}
	if !strings.Contains(e.Detail, "unrecorded") {
		t.Errorf("the entry does not say what the change gives up: %s", e.Detail)
	}
}

func TestTheHeartbeatCarriesTheScope(t *testing.T) {
	s, h, _ := scopeAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	if got := heartbeatScope(t, h, cred, asset.Hostname, ""); got != ScopeEveryLogin {
		t.Fatalf("an unchanged host was told %q", got)
	}
	// An agent too old to say what it applies is unconfirmed, not applied.
	a, _ := s.AssetByID(context.Background(), asset.ID)
	if a.AgentRecordingScope != nil {
		t.Errorf("agent scope = %q from an agent that did not say", *a.AgentRecordingScope)
	}

	setScope(t, h, "admin-token", asset.ID, ScopeArgusOnly)
	if got := heartbeatScope(t, h, cred, asset.Hostname, ScopeEveryLogin); got != ScopeArgusOnly {
		t.Fatalf("after the change the agent was told %q", got)
	}
	heartbeatScope(t, h, cred, asset.Hostname, ScopeArgusOnly)
	a, _ = s.AssetByID(context.Background(), asset.ID)
	if a.AgentRecordingScope == nil || *a.AgentRecordingScope != ScopeArgusOnly {
		t.Errorf("the console cannot see the agent applied it: %v", a.AgentRecordingScope)
	}
}

// A short name matching two assets that disagree gets the strict one.
func TestAnAmbiguousHostRecordsEveryLogin(t *testing.T) {
	if got := effectiveScope([]string{ScopeArgusOnly, ScopeEveryLogin}); got != ScopeEveryLogin {
		t.Errorf("disagreeing assets gave %q", got)
	}
	if got := effectiveScope(nil); got != ScopeEveryLogin {
		t.Errorf("no asset gave %q", got)
	}
	if got := effectiveScope([]string{ScopeArgusOnly, ScopeArgusOnly}); got != ScopeArgusOnly {
		t.Errorf("agreeing assets gave %q", got)
	}
}

func TestAnUnrecordedLoginIsInTheAuditChain(t *testing.T) {
	s, h, _ := scopeAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setScope(t, h, "admin-token", asset.ID, ScopeArgusOnly)

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
	for _, want := range []string{"ops", "203.0.113.7", "no replay"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("entry does not carry %q: %s", want, e.Detail)
		}
	}
}

// The console says every login; the agent says it skipped one anyway.
func TestALoginSkippedAgainstTheSettingSaysSo(t *testing.T) {
	s, h, _ := scopeAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	do(t, h, http.MethodPost, "/api/v1/report/login", cred,
		`{"hostname":"`+asset.Hostname+`","principal":"ops","origin":"direct"}`)
	if e := lastAuditOn(t, s, asset.Hostname); !strings.Contains(e.Detail, "not following the current setting") {
		t.Errorf("the disagreement is not in the entry: %s", e.Detail)
	}
}

func TestAnAgentReportsLoginsOnlyForItsOwnHost(t *testing.T) {
	s, h, _ := scopeAPI(t)
	_, cred := enrolledAsset(t, s, h)
	other := testAsset(t, s, "ops")

	w := do(t, h, http.MethodPost, "/api/v1/report/login", cred,
		`{"hostname":"`+other.Hostname+`","principal":"ops","origin":"direct"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an agent wrote a login against another host: %d", w.Code)
	}
}

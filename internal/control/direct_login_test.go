package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

/*
Direct SSH logins: refused, held for approval, or allowed and recorded. These
hold the closed side of each: the default refuses, an approval is needed from
an admin and only counts once, a timed-out login is refused and audited, and an
agent's claim that a session was admitted is checked rather than believed.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run DirectLogin
*/

type heartbeatReply struct {
	ConsoleOnly *bool   `json:"console_only"`
	DirectSSH   *string `json:"direct_ssh"`
}

func heartbeatDirect(t *testing.T, h http.Handler, cred, host string) heartbeatReply {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred,
		`{"hostname":"`+host+`","version":"1.0.0","active_sessions":0,"direct_ssh":"refuse"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out heartbeatReply
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func setServerDirectSSH(t *testing.T, h http.Handler, id, mode string) {
	t.Helper()
	w := do(t, h, http.MethodPut, "/api/v1/assets/"+id+"/direct-ssh", "admin-token",
		`{"mode":"`+mode+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set direct ssh %q: %d %s", mode, w.Code, w.Body)
	}
}

// keepGlobalDirectSSH restores the deployment's setting after a test that
// changes it, since every test shares the one policy row.
func keepGlobalDirectSSH(t *testing.T, s *Store) {
	t.Helper()
	before, err := s.GatewayPolicy(context.Background())
	if err != nil {
		t.Fatalf("GatewayPolicy: %v", err)
	}
	t.Cleanup(func() { _, _ = s.SaveGatewayPolicy(context.Background(), before, "test") })
}

type directLoginReply struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	DecidedBy string `json:"decidedBy"`
}

func askDirect(t *testing.T, h http.Handler, cred, host string) directLoginReply {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/report/direct-login", cred,
		`{"hostname":"`+host+`","principal":"ops","client_addr":"198.51.100.7","command":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("direct-login: %d %s", w.Code, w.Body)
	}
	var out directLoginReply
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func waitDirect(t *testing.T, h http.Handler, cred, id string) (int, directLoginReply) {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/report/direct-login/"+id+"/wait", cred, `{}`)
	var out directLoginReply
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func shortPolls(t *testing.T) {
	t.Helper()
	poll, long := directLoginPoll, directLoginLongPoll
	directLoginPoll, directLoginLongPoll = 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { directLoginPoll, directLoginLongPoll = poll, long })
}

// Upgrading changes nothing: a server with no choice of its own follows the
// deployment, which refuses, and an agent asking is refused without a row.
func TestDirectLoginRefusedByDefault(t *testing.T) {
	s, h, _ := reportAPI(t)
	keepGlobalDirectSSH(t, s)
	p, _ := s.GatewayPolicy(context.Background())
	p.DirectSSH = DirectSSHRefuse
	if _, err := s.SaveGatewayPolicy(context.Background(), p, "test"); err != nil {
		t.Fatal(err)
	}
	asset, cred := enrolledAsset(t, s, h)

	got := heartbeatDirect(t, h, cred, asset.Hostname)
	if got.DirectSSH == nil || *got.DirectSSH != DirectSSHRefuse || got.ConsoleOnly == nil || !*got.ConsoleOnly {
		t.Fatalf("heartbeat reply %+v, want refuse and console_only", got)
	}
	if r := askDirect(t, h, cred, asset.Hostname); r.State != "denied" || r.ID != "" {
		t.Fatalf("a refused server held a login: %+v", r)
	}
}

// A server's own choice wins over the deployment's, and an agent older than
// the setting is told console_only under "approve", since it cannot hold a
// login for anyone.
func TestDirectLoginServerOverride(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	for _, c := range []struct {
		mode        string
		consoleOnly bool
	}{
		{DirectSSHApprove, true},
		{DirectSSHAllow, false},
		{DirectSSHRefuse, true},
	} {
		setServerDirectSSH(t, h, asset.ID, c.mode)
		got := heartbeatDirect(t, h, cred, asset.Hostname)
		if got.DirectSSH == nil || *got.DirectSSH != c.mode {
			t.Errorf("%s: agent told %v", c.mode, got.DirectSSH)
		}
		if got.ConsoleOnly == nil || *got.ConsoleOnly != c.consoleOnly {
			t.Errorf("%s: console_only %v, want %v", c.mode, got.ConsoleOnly, c.consoleOnly)
		}
	}

	a, _ := s.AssetByID(context.Background(), asset.ID)
	if a.AgentDirectSSH == nil || *a.AgentDirectSSH != DirectSSHRefuse {
		t.Errorf("the console cannot see what the agent applied: %v", a.AgentDirectSSH)
	}

	// Opening a server up is audited as a loosening.
	setServerDirectSSH(t, h, asset.ID, DirectSSHAllow)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "asset.direct_ssh_changed" || e.Severity != "warning" {
		t.Errorf("loosening audited as %s/%s", e.Action, e.Severity)
	}
}

func TestDirectLoginNeedsAnAdminAndIsDecidedOnce(t *testing.T) {
	shortPolls(t)
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHApprove)

	r := askDirect(t, h, cred, asset.Hostname)
	if r.State != "pending" || r.ID == "" {
		t.Fatalf("not held for approval: %+v", r)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_requested" {
		t.Errorf("request audited as %s", e.Action)
	}

	// Waiting with nobody deciding comes back still pending.
	if code, w := waitDirect(t, h, cred, r.ID); code != http.StatusOK || w.State != "pending" {
		t.Fatalf("wait: %d %+v", code, w)
	}

	// An operator cannot approve it.
	if w := do(t, h, http.MethodPost, "/api/v1/direct-logins/"+r.ID+"/decision", "operator-token",
		`{"approve":true}`); w.Code != http.StatusForbidden {
		t.Fatalf("operator decided a direct login: %d %s", w.Code, w.Body)
	}

	w := do(t, h, http.MethodPost, "/api/v1/direct-logins/"+r.ID+"/decision", "admin-token",
		`{"approve":true,"note":"on call"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	e := lastAuditOn(t, s, asset.Hostname)
	if e.Action != "session.direct_approved" || e.ActorEmail == "" || !strings.Contains(e.Detail, "on call") {
		t.Errorf("approval audited as %+v", e)
	}

	code, got := waitDirect(t, h, cred, r.ID)
	if code != http.StatusOK || got.State != "approved" || got.DecidedBy == "" {
		t.Fatalf("agent told %d %+v", code, got)
	}

	// Once decided, it stays decided.
	if w := do(t, h, http.MethodPost, "/api/v1/direct-logins/"+r.ID+"/decision", "admin-token",
		`{"approve":false}`); w.Code != http.StatusConflict {
		t.Errorf("a decided login was decided again: %d", w.Code)
	}
}

// Nobody deciding in time is a refusal, written to the audit log, and too late
// to approve.
func TestDirectLoginExpires(t *testing.T) {
	shortPolls(t)
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHApprove)
	r := askDirect(t, h, cred, asset.Hostname)

	if _, err := s.pool.Exec(context.Background(),
		`UPDATE direct_login_requests SET expires_at = now() - interval '1 second' WHERE id::text = $1`,
		r.ID); err != nil {
		t.Fatal(err)
	}
	if code, got := waitDirect(t, h, cred, r.ID); code != http.StatusOK || got.State != "expired" {
		t.Fatalf("an expired login came back %d %+v", code, got)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_expired" {
		t.Errorf("expiry audited as %s", e.Action)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/direct-logins/"+r.ID+"/decision", "admin-token",
		`{"approve":true}`); w.Code != http.StatusConflict {
		t.Errorf("an expired login was approved: %d", w.Code)
	}
}

// An agent may only wait on its own host's logins.
func TestDirectLoginWaitIsBoundToTheHost(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHApprove)
	r := askDirect(t, h, cred, asset.Hostname)

	_, other := enrolledAsset(t, s, h)
	if code, _ := waitDirect(t, h, other, r.ID); code != http.StatusForbidden {
		t.Fatalf("another host's agent read the decision: %d", code)
	}
}

// One host cannot fill the console with waiting logins.
func TestDirectLoginPendingIsBounded(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHApprove)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM direct_login_requests WHERE hostname = $1`, asset.Hostname)
	})
	for i := 0; i < maxPendingDirectLogins; i++ {
		if r := askDirect(t, h, cred, asset.Hostname); r.State != "pending" {
			t.Fatalf("login %d: %+v", i, r)
		}
	}
	if r := askDirect(t, h, cred, asset.Hostname); r.State != "denied" {
		t.Fatalf("login past the bound was held: %+v", r)
	}
}

func reportDirectSession(t *testing.T, h http.Handler, cred, host, extra string) {
	t.Helper()
	// Distinct start times: the sessions list orders by them, and ties make
	// other tests' ordering checks flaky.
	start := time.Now().UTC()
	body := `{"id":"` + newUUID(t) + `","userEmail":"ops@` + host + `","assetHostname":"` + host +
		`","principal":"ops","protocol":"ssh","origin":"direct","state":"closed",` +
		`"startedAt":"` + start.Format(time.RFC3339Nano) + `","endedAt":"` +
		start.Add(time.Minute).Format(time.RFC3339Nano) + `","exitCode":0` + extra + `}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred, body); w.Code != http.StatusOK {
		t.Fatalf("report session: %d %s", w.Code, w.Body)
	}
}

// An agent saying a direct session was approved is believed only when the
// approval exists, is for this host and account, and was granted.
func TestDirectSessionApprovalIsChecked(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHApprove)

	r := askDirect(t, h, cred, asset.Hostname)
	// Not yet approved: still a bypass.
	reportDirectSession(t, h, cred, asset.Hostname,
		`,"directApproval":"approved","directLoginId":"`+r.ID+`"`)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_detected" || e.Severity != "critical" {
		t.Errorf("an unapproved claim was believed: %s/%s", e.Action, e.Severity)
	}

	if w := do(t, h, http.MethodPost, "/api/v1/direct-logins/"+r.ID+"/decision", "admin-token",
		`{"approve":true}`); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	reportDirectSession(t, h, cred, asset.Hostname,
		`,"directApproval":"approved","directLoginId":"`+r.ID+`"`)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_recorded" || e.Severity != "notice" {
		t.Errorf("an approved session audited as %s/%s", e.Action, e.Severity)
	}

	// "Allowed" from a server that is not set to allow is a bypass.
	reportDirectSession(t, h, cred, asset.Hostname, `,"directApproval":"allowed"`)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_detected" {
		t.Errorf("an allowed claim on an approve server was believed: %s", e.Action)
	}
	setServerDirectSSH(t, h, asset.ID, DirectSSHAllow)
	reportDirectSession(t, h, cred, asset.Hostname, `,"directApproval":"allowed"`)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_recorded" {
		t.Errorf("an allowed session audited as %s", e.Action)
	}
}

// Changing the deployment's setting is audited, and opening it is a loosening.
func TestDirectSSHPolicyChangeIsALoosening(t *testing.T) {
	s, h, _ := reportAPI(t)
	keepGlobalDirectSSH(t, s)
	p, _ := s.GatewayPolicy(context.Background())
	p.DirectSSH = DirectSSHRefuse
	if _, err := s.SaveGatewayPolicy(context.Background(), p, "test"); err != nil {
		t.Fatal(err)
	}
	p.DirectSSH = DirectSSHApprove
	b, _ := json.Marshal(p)
	w := do(t, h, http.MethodPost, "/api/v1/policy", "admin-token", string(b))
	if w.Code != http.StatusOK {
		t.Fatalf("policy: %d %s", w.Code, w.Body)
	}
	e := lastAuditOn(t, s, "gateway")
	if e.Action != "policy.change" || e.Severity != "warning" || !strings.Contains(e.Detail, "ask for approval") {
		t.Errorf("policy change audited as %+v", e)
	}

	p.DirectSSH = "sometimes"
	b, _ = json.Marshal(p)
	if w := do(t, h, http.MethodPost, "/api/v1/policy", "admin-token", string(b)); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown mode was saved: %d", w.Code)
	}
}

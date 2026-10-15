package control

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

/*
Per-host agent credentials.

Every agent used to report with the token every gateway holds, so root on one
managed host could report as any other host, or as a gateway. These tests hold
the refusals that make a per-host credential worth more than that: a token
works once, for a while, for the host it names; the credential it buys speaks
only for that host, only on the agent's routes, and stops the moment it is
revoked.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Enrol
*/

const testReporterToken = "shared-reporter-token-for-tests"

// enrollAPI is the full router, so the gates are the ones a request meets.
func enrollAPI(t *testing.T) (*API, *Store, http.Handler, string) {
	t.Helper()
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.ReporterToken = testReporterToken
	admin, _ := newAccount(t, s, "admin")
	a.UserTokens = map[string]string{"admin-token": admin}
	return a, s, a.Handler(), admin
}

func do(t *testing.T, h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// mint makes a token through the console route.
func mint(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/agents/enroll-tokens", "admin-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", w.Code, w.Body)
	}
	var out struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Token, enrollTokenPrefix) {
		t.Fatalf("minted token %q", out.Token)
	}
	return out.Token
}

// enroll exchanges a token and returns the response.
func enroll(t *testing.T, h http.Handler, token, hostname string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"token": token, "hostname": hostname})
	return do(t, h, http.MethodPost, "/api/v1/agents/enroll", "", string(b))
}

// enrolled mints, enrolls and returns the credential, cleaning the host up.
func enrolled(t *testing.T, s *Store, h http.Handler, hostname string) (credential, id string) {
	t.Helper()
	cleanEnrollment(t, s, hostname)
	w := enroll(t, h, mint(t, h, ""), hostname)
	if w.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", w.Code, w.Body)
	}
	var out struct{ Credential, CredentialID string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.Credential, out.CredentialID
}

func cleanEnrollment(t *testing.T, s *Store, hostname string) {
	t.Helper()
	cleanHost(t, s, hostname)
	drop := func() {
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM agent_credentials WHERE lower(hostname) = lower($1)`, hostname)
	}
	drop()
	t.Cleanup(drop)
}

func heartbeatAs(t *testing.T, h http.Handler, credential, hostname string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, http.MethodPost, "/api/v1/report/heartbeat", credential,
		`{"hostname":"`+hostname+`","version":"1.0.0","active_sessions":0}`)
}

/* ── The token ───────────────────────────────────────────────────────────── */

func TestAnEnrollmentTokenWorksOnce(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	host := unique("enr-once")
	cleanEnrollment(t, s, host)
	tok := mint(t, h, "")

	if w := enroll(t, h, tok, host); w.Code != http.StatusOK {
		t.Fatalf("first use refused: %d %s", w.Code, w.Body)
	}
	other := unique("enr-once-b")
	cleanEnrollment(t, s, other)
	w := enroll(t, h, tok, other)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a used token enrolled a second host: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "already been used") {
		t.Errorf("the refusal should say why: %s", w.Body)
	}
}

func TestTwoAgentsRacingOneTokenEnrollOne(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	tok := mint(t, h, "")
	var (
		wg sync.WaitGroup
		mu sync.Mutex
		ok int
	)
	for i := range 8 {
		host := unique("enr-race") + "-" + string(rune('a'+i))
		cleanEnrollment(t, s, host)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if enroll(t, h, tok, host).Code == http.StatusOK {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d hosts enrolled with one single-use token", ok)
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	host := unique("enr-exp")
	cleanEnrollment(t, s, host)
	tok := mint(t, h, "")
	_, _ = s.pool.Exec(context.Background(),
		`UPDATE agent_enrollment_tokens SET expires_at = now() - interval '1 second'
		  WHERE token_hash = $1`, hashSecret(tok))

	w := enroll(t, h, tok, host)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("an expired token: %d %s", w.Code, w.Body)
	}
}

func TestATokenForOneHostCannotEnrollAnother(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	named, other := unique("enr-named"), unique("enr-other")
	cleanEnrollment(t, s, named)
	cleanEnrollment(t, s, other)
	tok := mint(t, h, `{"hostname":"`+named+`"}`)

	if w := enroll(t, h, tok, other); w.Code != http.StatusUnauthorized {
		t.Fatalf("a token made for %s enrolled %s: %d %s", named, other, w.Code, w.Body)
	}
	// Refused without being spent, so the right host can still use it.
	if w := enroll(t, h, tok, strings.ToUpper(named)); w.Code != http.StatusOK {
		t.Fatalf("the host it was made for was refused: %d %s", w.Code, w.Body)
	}
}

func TestAnUnknownTokenIsRefusedAndCounted(t *testing.T) {
	_, _, h, _ := enrollAPI(t)
	w := enroll(t, h, enrollTokenPrefix+"not-a-real-token", unique("enr-unknown"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown token: %d %s", w.Code, w.Body)
	}
}

// The console used to tell people to paste the shared token into every agent.
// Someone will paste it here; the answer has to say what to do instead.
func TestTheSharedReporterTokenIsNotAnEnrollmentToken(t *testing.T) {
	_, _, h, _ := enrollAPI(t)
	w := enroll(t, h, testReporterToken, unique("enr-shared"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Add a server") {
		t.Fatalf("the shared token as an enrollment token: %d %s", w.Code, w.Body)
	}
}

func TestOnlyAdminsMakeEnrollmentTokens(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	op, _ := newAccount(t, s, "operator")
	a.UserTokens["operator-token"] = op
	w := do(t, h, http.MethodPost, "/api/v1/agents/enroll-tokens", "operator-token", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("an operator made an enrollment token: %d %s", w.Code, w.Body)
	}
}

// A second credential for an enrolled host would let anyone with an unbound
// token take over that host's identity. Refused, and the token survives so
// the operator can revoke the old one and retry.
func TestAnEnrolledHostCannotBeEnrolledAgain(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	host := unique("enr-twice")
	_, id := enrolled(t, s, h, host)

	tok := mint(t, h, "")
	w := enroll(t, h, tok, strings.ToUpper(host))
	if w.Code != http.StatusConflict {
		t.Fatalf("a second credential for %s: %d %s", host, w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/agents/"+id+"/revoke", "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := enroll(t, h, tok, host); w.Code != http.StatusOK {
		t.Fatalf("the refused token was spent anyway: %d %s", w.Code, w.Body)
	}
}

/* ── The credential ──────────────────────────────────────────────────────── */

func TestACredentialReportsOnlyForItsOwnHost(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	host, victim := unique("enr-own"), unique("enr-victim")
	cred, _ := enrolled(t, s, h, host)
	cleanHost(t, s, victim)

	if w := heartbeatAs(t, h, cred, host); w.Code != http.StatusOK {
		t.Fatalf("its own heartbeat was refused: %d %s", w.Code, w.Body)
	}
	if w := heartbeatAs(t, h, cred, victim); w.Code != http.StatusForbidden {
		t.Fatalf("heartbeat for another host: %d %s", w.Code, w.Body)
	}
	w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred,
		`{"hostname":"`+victim+`","os":"forged"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("facts for another host: %d %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodPost, "/api/v1/report/session", cred,
		`{"id":"00000000-0000-4000-8000-000000000001","assetHostname":"`+victim+`","principal":"root","state":"closed"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a session on another host: %d %s", w.Code, w.Body)
	}
}

// The rest of the reporter surface is a gateway's.
func TestACredentialCannotUseTheGatewayRoutes(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	cred, _ := enrolled(t, s, h, unique("enr-routes"))
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/report/audit", `{"action":"forged"}`},
		{http.MethodGet, "/api/v1/gateway/inventory", ""},
		{http.MethodGet, "/api/v1/gateway/policy", ""},
		{http.MethodPost, "/api/v1/terminal/redeem", `{}`},
		{http.MethodPost, "/api/v1/report/asset", `{"hostname":"x"}`},
	} {
		if w := do(t, h, rt.method, rt.path, cred, rt.body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with an agent credential: %d", rt.method, rt.path, w.Code)
		}
	}
}

// Which credential vouched for a session is the control plane's to say.
func TestASessionNamesTheCredentialThatReportedIt(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	host := unique("enr-sess")
	cred, _ := enrolled(t, s, h, host)
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id::text = $1`, id)
	})
	w := do(t, h, http.MethodPost, "/api/v1/report/session", cred,
		`{"id":"`+id+`","assetHostname":"`+host+`","principal":"root","state":"closed",`+
			`"reportedBy":"gateway:trusted-bastion"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("report session: %d %s", w.Code, w.Body)
	}
	got, err := s.Session(context.Background(), id)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.ReportedBy != "agent:"+host {
		t.Errorf("ReportedBy = %q; the reporter chose it", got.ReportedBy)
	}
}

// A Windows shell that fails to start exits with an NTSTATUS such as
// 0xC0000142, past the INT column. The insert failed with a 500 the agent
// retried for ever; it is now refused as the reporter's mistake, and the
// signed form is stored.
func TestAnExitCodePastThirtyTwoBitsIsRefusedNotAServerError(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	host := unique("enr-exit")
	cred, _ := enrolled(t, s, h, host)
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id::text = $1`, id)
	})
	body := func(code string) string {
		return `{"id":"` + id + `","assetHostname":"` + host + `","principal":"ana",` +
			`"state":"closed","exitCode":` + code + `}`
	}
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred, body("3221225794")); w.Code != http.StatusBadRequest {
		t.Fatalf("unsigned NTSTATUS: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred, body("-1073741502")); w.Code != http.StatusOK {
		t.Fatalf("signed NTSTATUS: %d %s", w.Code, w.Body)
	}
	got, err := s.Session(context.Background(), id)
	if err != nil || got.ExitCode == nil || *got.ExitCode != -1073741502 {
		t.Fatalf("stored %+v, %v", got.ExitCode, err)
	}
}

func TestARevokedCredentialIsRefusedAtOnce(t *testing.T) {
	_, s, h, admin := enrollAPI(t)
	host := unique("enr-revoke")
	cred, id := enrolled(t, s, h, host)

	// Warm the cache, so the refusal below is not just a cold lookup.
	if w := heartbeatAs(t, h, cred, host); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/agents/"+id+"/revoke", "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := heartbeatAs(t, h, cred, host); w.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked credential still reports: %d", w.Code)
	}

	action, actor, target := lastAgentAudit(t, s, host)
	if action != "agent.revoked" || actor != admin || target != host {
		t.Errorf("audit = %s by %s on %s", action, actor, target)
	}
}

// The client certificate stands in for a host identity the shared token lacks.
// A per-host credential is one, so it is accepted without -- and the shared
// token still is not.
func TestOnlyTheSharedTokenStillNeedsAClientCertificate(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	a.RequirePeerCert = true
	host := unique("enr-mtls")
	cred, _ := enrolled(t, s, h, host)

	tlsHeartbeat := func(bearer string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/report/heartbeat",
			strings.NewReader(`{"hostname":"`+host+`","version":"1"}`))
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := tlsHeartbeat(cred); code != http.StatusOK {
		t.Errorf("an agent credential without a certificate: %d", code)
	}
	if code := tlsHeartbeat(testReporterToken); code != http.StatusUnauthorized {
		t.Errorf("the shared token without a certificate: %d", code)
	}
}

/* ── The record ──────────────────────────────────────────────────────────── */

func TestEnrollmentIsAudited(t *testing.T) {
	_, s, h, admin := enrollAPI(t)
	host := unique("enr-audit")
	enrolled(t, s, h, host)

	action, actor, target := lastAgentAudit(t, s, host)
	if action != "agent.enrolled" || actor != admin || target != host {
		t.Errorf("audit = %s by %s on %s; want agent.enrolled by %s on %s",
			action, actor, target, admin, host)
	}
	var minted int
	_ = s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		 WHERE action = 'agent.enroll_token.created' AND actor_email = $1`, admin).Scan(&minted)
	if minted == 0 {
		t.Error("minting a token left no audit entry")
	}
}

func lastAgentAudit(t *testing.T, s *Store, host string) (action, actor, target string) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(), `
		SELECT action, actor_email, target FROM audit_events
		 WHERE target = $1 AND action LIKE 'agent.%'
		 ORDER BY seq DESC LIMIT 1`, host).Scan(&action, &actor, &target)
	if err != nil {
		t.Fatalf("no agent audit entry for %s: %v", host, err)
	}
	return
}

/* ── The cache ───────────────────────────────────────────────────────────── */

func TestTheCredentialCacheIsBounded(t *testing.T) {
	var c agentCredCache
	now := time.Now()
	for i := range agentCredCacheMax + 10 {
		c.put(hashSecret(string(rune(i))+"-k"), agentCredEntry{id: "x", checked: now})
	}
	if n := len(c.m); n > agentCredCacheMax {
		t.Fatalf("cache holds %d entries, bound is %d", n, agentCredCacheMax)
	}
	if _, ok := c.get("absent", now); ok {
		t.Error("an absent key was found")
	}
	c.put("stale", agentCredEntry{checked: now.Add(-2 * agentCredCacheTTL)})
	if _, ok := c.get("stale", now); ok {
		t.Error("an entry older than the TTL was trusted")
	}
}

func TestEnrollRejectsAnythingWithoutThePrefix(t *testing.T) {
	s := testStore(t)
	_, err := s.Enroll(context.Background(), "plain", "h")
	if !errors.Is(err, ErrEnrollTokenInvalid) {
		t.Fatalf("err = %v", err)
	}
}

package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 5: just-in-time console access, against the fake provider. The
account is Production, so every grant waits for someone else.
*/

func (e changeEnv) askAccess(t *testing.T, token, body string, want int) CloudAccess {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/access", token, body)
	if w.Code != want {
		t.Fatalf("access: %d %s, want %d", w.Code, w.Body, want)
	}
	var g CloudAccess
	_ = json.Unmarshal(w.Body.Bytes(), &g)
	return g
}

func (e changeEnv) accessBody(level string, minutes int, reason string) string {
	b, _ := json.Marshal(map[string]any{"accountId": e.acct.ID, "level": level, "minutes": minutes, "reason": reason})
	return string(b)
}

func (e changeEnv) accessCall(t *testing.T, token, id, what string, want int) []byte {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/access/"+id+"/"+what, token, `{}`)
	if w.Code != want {
		t.Fatalf("%s: %d %s, want %d", what, w.Code, w.Body, want)
	}
	return w.Body.Bytes()
}

func TestCloudAccessGrantAndSessions(t *testing.T) {
	e := changeAPI(t)
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM cloud_access WHERE account_id::text = $1`, e.acct.ID)
	})
	g := e.askAccess(t, "admin-token", e.accessBody("read", 60, "look at the ALB logs for INC-42"), http.StatusAccepted)
	if g.State != "awaiting_approval" || g.Environment != "production" {
		t.Fatalf("asked: %+v", g)
	}
	// Nothing opens before a decision; nobody decides their own.
	e.accessCall(t, "admin-token", g.ID, "console", http.StatusConflict)
	e.accessCall(t, "admin-token", g.ID, "approve", http.StatusForbidden)
	// A second request for the same access waits behind the first.
	e.askAccess(t, "admin-token", e.accessBody("read", 30, "the same thing again"), http.StatusConflict)

	var ok CloudAccess
	_ = json.Unmarshal(e.accessCall(t, "approver-token", g.ID, "approve", http.StatusOK), &ok)
	if ok.State != "active" || ok.EndsAt == nil || ok.DecidedBy == nil || *ok.DecidedBy != e.approve {
		t.Fatalf("approved: %+v", ok)
	}
	// Only its holder opens sessions with it.
	e.accessCall(t, "approver-token", g.ID, "console", http.StatusForbidden)
	var link cloud.ConsoleLink
	_ = json.Unmarshal(e.accessCall(t, "admin-token", g.ID, "console", http.StatusOK), &link)
	if !strings.HasPrefix(link.URL, "https://signin.fake/") || !strings.Contains(link.URL, "region="+e.acct.Regions[0]) {
		t.Fatalf("link: %+v", link)
	}
	var keys cloud.Credentials
	_ = json.Unmarshal(e.accessCall(t, "admin-token", g.ID, "credentials", http.StatusOK), &keys)
	if keys.AccessKeyID == "" || keys.SessionToken == "" {
		t.Fatalf("keys: %+v", keys)
	}
	// Each session is the holder's own, at most an hour.
	var sessions int
	for _, c := range e.fake.CallLog() {
		if strings.HasPrefix(c, "console read "+e.admin+" ") || strings.HasPrefix(c, "credentials read "+e.admin+" ") {
			sessions++
			if !strings.HasSuffix(c, "m") && !strings.HasSuffix(c, "s") {
				t.Fatalf("session length: %q", c)
			}
		}
	}
	if sessions != 2 {
		t.Fatalf("sessions: %v", e.fake.CallLog())
	}
	after, _ := e.s.CloudAccessGrant(context.Background(), g.ID)
	if after.Sessions != 2 || after.LastSessionAt == nil {
		t.Fatalf("counted: %+v", after)
	}
	for _, act := range []string{"cloud.access.requested", "cloud.access.approved", "cloud.access.console", "cloud.access.credentials"} {
		if !accessAudited(t, e.s, act, g.ID) {
			t.Fatalf("no %s in the audit log", act)
		}
	}

	// Ending it stops new sessions.
	e.accessCall(t, "admin-token", g.ID, "end", http.StatusOK)
	e.accessCall(t, "admin-token", g.ID, "console", http.StatusConflict)
	e.accessCall(t, "admin-token", g.ID, "end", http.StatusConflict)
}

func TestCloudAccessWhoMayAsk(t *testing.T) {
	e := changeAPI(t)
	t.Cleanup(func() {
		_, _ = e.s.pool.Exec(context.Background(), `DELETE FROM cloud_access WHERE account_id::text = $1`, e.acct.ID)
	})
	auditor, _ := newAccount(t, e.s, "auditor")
	e.a.UserTokens["auditor-token"] = auditor
	e.askAccess(t, "auditor-token", e.accessBody("operate", 60, "fix the broken thing"), http.StatusForbidden)
	e.askAccess(t, "approver-token", e.accessBody("read", 60, "fix the broken thing"), http.StatusForbidden)
	r := e.askAccess(t, "auditor-token", e.accessBody("read", 60, "quarterly access review"), http.StatusAccepted)
	// Bad lengths, levels and reasons.
	e.askAccess(t, "admin-token", e.accessBody("read", 45, "a good reason"), http.StatusBadRequest)
	e.askAccess(t, "admin-token", e.accessBody("root", 60, "a good reason"), http.StatusBadRequest)
	e.askAccess(t, "admin-token", e.accessBody("read", 60, "no"), http.StatusBadRequest)
	// The asker withdraws; nobody else can.
	e.accessCall(t, "admin-token", r.ID, "cancel", http.StatusConflict)
	e.accessCall(t, "auditor-token", r.ID, "cancel", http.StatusOK)
	// A declined one never opens.
	d := e.askAccess(t, "admin-token", e.accessBody("operate", 120, "rotate the ALB certificate"), http.StatusAccepted)
	e.accessCall(t, "second-token", d.ID, "decline", http.StatusOK)
	e.accessCall(t, "admin-token", d.ID, "console", http.StatusConflict)

	// Everyone who may see cloud access sees all of it.
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/access", "auditor-token", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), d.ID) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
}

// An account whose stack predates console roles is told to update it.
func TestCloudAccessNeedsTheConsoleRole(t *testing.T) {
	e := changeAPI(t)
	e.fake.Missing = []string{"role:ArgusConsoleOperate"}
	e.resync(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/access", "admin-token", e.accessBody("operate", 60, "rotate the ALB certificate"))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "ArgusConsoleOperate") {
		t.Fatalf("old stack: %d %s", w.Code, w.Body)
	}
}

func TestCloudAccessApprovalRule(t *testing.T) {
	for _, c := range []struct {
		env   string
		level cloud.ConsoleLevel
		waits bool
	}{
		{"production", cloud.ConsoleRead, true}, {"production", cloud.ConsoleOperate, true},
		{"staging", cloud.ConsoleRead, false}, {"staging", cloud.ConsoleOperate, true},
		{"development", cloud.ConsoleOperate, false},
	} {
		if got := cloudAccessNeedsApproval(c.env, c.level); got != c.waits {
			t.Errorf("%s %s: waits %v", c.env, c.level, got)
		}
	}
}

func accessAudited(t *testing.T, s *Store, action, id string) bool {
	t.Helper()
	events, err := s.AuditEvents(context.Background(), 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Action == action && strings.Contains(ev.Detail, id) {
			return true
		}
	}
	return false
}

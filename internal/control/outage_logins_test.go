package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

/*
Logins while a server cannot reach Argus (outage_logins.go).

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5432/argus_test?sslmode=disable \
	  go test ./internal/control/ -run OutageLogin
*/

// keepOutageLogins puts the organization's setting back after a test.
func keepOutageLogins(t *testing.T, s *Store) {
	t.Helper()
	before, err := s.OutageLogins(context.Background())
	if err != nil {
		t.Fatalf("OutageLogins: %v", err)
	}
	t.Cleanup(func() { _, _ = s.SetOutageLogins(context.Background(), before, "test") })
}

func heartbeatOutage(t *testing.T, h http.Handler, cred, host string) *struct {
	After  int      `json:"after_minutes"`
	Groups []string `json:"groups"`
} {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred,
		`{"hostname":"`+host+`","version":"1.0.0","active_sessions":0}`)
	var got struct {
		Outage *struct {
			After  int      `json:"after_minutes"`
			Groups []string `json:"groups"`
		} `json:"outage_logins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Outage
}

// Off unless turned on; the setting rides the heartbeat; a server can turn
// it off for itself; each change is audited, and turning it on is a warning.
func TestOutageLoginsRideTheHeartbeatAndAreAudited(t *testing.T) {
	s, h, _ := reportAPI(t)
	keepOutageLogins(t, s)
	asset, cred := enrolledAsset(t, s, h)
	if _, err := s.SetOutageLogins(context.Background(),
		OutageLogins{AfterMinutes: 0, Groups: []string{"sudo", "wheel", "admin"}}, "test"); err != nil {
		t.Fatal(err)
	}
	if o := heartbeatOutage(t, h, cred, asset.Hostname); o == nil || o.After != 0 {
		t.Fatalf("off: %+v", o)
	}

	w := do(t, h, http.MethodPut, "/api/v1/policy/outage-logins", "admin-token",
		`{"afterMinutes":15,"groups":["sudo"," oncall","sudo"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, "servers"); e.Action != "policy.outage_logins_changed" || e.Severity != "warning" {
		t.Errorf("audit: %+v", e)
	}
	o := heartbeatOutage(t, h, cred, asset.Hostname)
	if o == nil || o.After != 15 || len(o.Groups) != 2 || o.Groups[1] != "oncall" {
		t.Fatalf("on: %+v", o)
	}

	if w := do(t, h, http.MethodPut, "/api/v1/assets/"+asset.ID+"/outage-logins", "admin-token",
		`{"off":true}`); w.Code != http.StatusOK {
		t.Fatalf("server off: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "asset.outage_logins_changed" {
		t.Errorf("audit: %+v", e)
	}
	if o := heartbeatOutage(t, h, cred, asset.Hostname); o == nil || o.After != 0 {
		t.Fatalf("a server that turned it off is told %+v", o)
	}
	w = do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/outage-logins", "admin-token", "")
	var view assetOutageLogins
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if !view.Off || view.AfterMinutes != 15 {
		t.Errorf("server view: %+v", view)
	}
}

// Only what can be chosen is stored, and only admins and owners choose.
func TestOutageLoginsAreChecked(t *testing.T) {
	s, h, _ := reportAPI(t)
	keepOutageLogins(t, s)
	for _, bad := range []string{
		`{"afterMinutes":7,"groups":["sudo"]}`,
		`{"afterMinutes":15,"groups":[]}`,
		`{"afterMinutes":15,"groups":["sudo; rm -rf /"]}`,
	} {
		if w := do(t, h, http.MethodPut, "/api/v1/policy/outage-logins", "admin-token", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := do(t, h, http.MethodPut, "/api/v1/policy/outage-logins", "operator-token",
		`{"afterMinutes":15,"groups":["sudo"]}`); w.Code != http.StatusForbidden {
		t.Errorf("an ordinary account changed it: %d", w.Code)
	}
}

// A session the agent says it let in during an outage is believed only when
// the setting is on for its server; then it is audited and put in the bell.
func TestAnOutageLoginIsCheckedAuditedAndRaised(t *testing.T) {
	s, h, _ := reportAPI(t)
	keepOutageLogins(t, s)
	asset, cred := enrolledAsset(t, s, h)
	if _, err := s.SetOutageLogins(context.Background(),
		OutageLogins{AfterMinutes: 0, Groups: []string{"sudo"}}, "test"); err != nil {
		t.Fatal(err)
	}
	reportDirectSession(t, h, cred, asset.Hostname, `,"directApproval":"outage"`)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_detected" {
		t.Errorf("an outage claim with the setting off was believed: %s", e.Action)
	}

	if _, err := s.SetOutageLogins(context.Background(),
		OutageLogins{AfterMinutes: 15, Groups: []string{"sudo"}}, "test"); err != nil {
		t.Fatal(err)
	}
	reportDirectSession(t, h, cred, asset.Hostname, `,"directApproval":"outage"`)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.outage_login" || e.Severity != "warning" {
		t.Errorf("an outage login audited as %s/%s", e.Action, e.Severity)
	}
	var title string
	if err := s.pool.QueryRow(context.Background(), `SELECT title FROM console_alerts
		WHERE title LIKE '%' || $1 || ' while Argus was unreachable' ORDER BY seq DESC LIMIT 1`,
		asset.Hostname).Scan(&title); err != nil {
		t.Fatalf("no alert: %v", err)
	}
}

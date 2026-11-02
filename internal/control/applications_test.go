package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
What runs on a server reaches the server page for those who see the whole
fleet and no one else, a report from an older agent leaves it alone, and an
oversized report is cut rather than stored whole.
*/

func TestApplicationsReachTheServerPage(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/applications"

	w := do(t, h, http.MethodGet, path, "admin-token", "")
	var got ServerApplications
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.Reported {
		t.Fatalf("before a report: %d %s", w.Code, w.Body)
	}

	facts := `{"hostname":"` + asset.Hostname + `","os":"Ubuntu","applications":{` +
		`"checked_at":"2026-11-02T05:00:00Z",` +
		`"applications":[{"kind":"postgres","name":"PostgreSQL","version":"16.4","running":true,` +
		`"listen":[{"address":"127.0.0.1","port":5432},{"address":"0.0.0.0","port":5433}],` +
		`"process":"postgres","unit":"postgresql@16-main.service","package":"postgresql-16","evidence":["package","process"]}],` +
		`"listeners":[{"address":"127.0.0.1","port":5432,"process":"postgres","kind":"postgres"},` +
		`{"address":"0.0.0.0","port":70000}],` +
		`"containers":[],"services":["postgresql@16-main.service"],"sources":["sockets","dpkg"]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, facts); w.Code != http.StatusOK {
		t.Fatalf("facts: %d %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodGet, path, "admin-token", "")
	got = ServerApplications{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Reported || got.CheckedAt == nil || len(got.Applications) != 1 {
		t.Fatalf("after a report: %s", w.Body)
	}
	pg := got.Applications[0]
	if pg.Kind != "postgres" || pg.Version != "16.4" || len(pg.Listen) != 2 ||
		!pg.Listen[0].LocalOnly || pg.Listen[1].LocalOnly {
		t.Errorf("postgres = %+v", pg)
	}
	// A port no socket can have is dropped, not stored.
	if len(got.Listeners) != 1 {
		t.Errorf("listeners = %+v", got.Listeners)
	}

	// An agent older than discovery sends facts without applications: what
	// the row holds stays.
	old := `{"hostname":"` + asset.Hostname + `","os":"Ubuntu"}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, old); w.Code != http.StatusOK {
		t.Fatalf("old facts: %d %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodGet, path, "admin-token", "")
	got = ServerApplications{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Reported || len(got.Applications) != 1 {
		t.Errorf("an old agent's facts erased the inventory: %s", w.Body)
	}

	if w := do(t, h, http.MethodGet, path, "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator read what runs on a host: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/assets/00000000-0000-0000-0000-000000000000/applications", "admin-token", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown asset: %d", w.Code)
	}
}

func TestApplicationReportIsBounded(t *testing.T) {
	inv := &AppInventory{Services: make([]string, maxServices+50)}
	for i := 0; i < maxListeners+50; i++ {
		inv.Listeners = append(inv.Listeners, AppListener{Address: "0.0.0.0", Port: 1 + i, Process: strings.Repeat("x", 5000)})
	}
	b := inv.bounded()
	if len(b.Listeners) != maxListeners || len(b.Services) != maxServices || len(b.Listeners[0].Process) != maxAppField {
		t.Errorf("listeners %d services %d process %d", len(b.Listeners), len(b.Services), len(b.Listeners[0].Process))
	}
	if (*AppInventory)(nil).bounded() != nil {
		t.Error("nil inventory became a report")
	}
}

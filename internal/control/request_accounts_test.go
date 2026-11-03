package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

/*
The Ask for access form offered deploy, ops and root on every server. These
hold that what it offers comes from the server, and that a request for an
account the server does not have is refused rather than queued for an
approval nobody can use.
*/

func TestRequestableAccountsComeFromTheServer(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE assets SET principals = $2 WHERE id = $1::uuid`,
		asset.ID, []string{"ops", "deploy", "root"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM access_requests WHERE $1 = ANY(asset_hostnames) OR 'nowhere-01.example' = ANY(asset_hostnames)`,
			asset.Hostname)
	})

	accounts := func() RequestableHost {
		t.Helper()
		w := do(t, h, http.MethodGet, "/api/v1/requests/accounts?host="+url.QueryEscape(asset.Hostname),
			"operator-token", "")
		var got []RequestableHost
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got) != 1 {
			t.Fatalf("accounts: %d %s", w.Code, w.Body)
		}
		return got[0]
	}
	byName := func(h RequestableHost) map[string]RequestableAccount {
		m := map[string]RequestableAccount{}
		for _, a := range h.Accounts {
			m[a.Name] = a
		}
		return m
	}

	// Before the agent reports, the list stands as the administrator wrote it.
	before := accounts()
	if !before.Known || before.AgentReported || len(before.Accounts) != 3 {
		t.Fatalf("before a report: %+v", before)
	}
	for _, a := range before.Accounts {
		if !a.OnServer {
			t.Errorf("%s marked missing before any report", a.Name)
		}
	}

	// The agent finds ops and root, not deploy.
	body := `{"hostname":"` + asset.Hostname + `","version":"1.0.0","active_sessions":0,` +
		`"posture":{"rating":"monitored","unmanaged_keys":[],"shim_installed":true,"drift":[],` +
		`"login_accounts":[{"user":"ops","password":"set","keys":1,"can_login":true},` +
		`{"user":"root","password":"set","keys":0,"can_login":false}]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	after := byName(accounts())
	if !after["ops"].OnServer || after["deploy"].OnServer || !after["root"].OnServer {
		t.Errorf("after a report: %+v", after)
	}
	if !after["root"].Elevated || after["ops"].Elevated {
		t.Errorf("elevated wrong: %+v", after)
	}

	ask := func(host, principal string) (int, string) {
		t.Helper()
		w := do(t, h, http.MethodPost, "/api/v1/requests", "operator-token",
			`{"assetHostnames":["`+host+`"],"principal":"`+principal+`",`+
				`"justification":"checking the queue depth after the deploy","durationMinutes":60}`)
		return w.Code, w.Body.String()
	}
	if code, body := ask(asset.Hostname, "deploy"); code != http.StatusBadRequest || !strings.Contains(body, "did not find") {
		t.Errorf("deploy, missing on the server, was %d: %s", code, body)
	}
	if code, body := ask(asset.Hostname, "ubuntu"); code != http.StatusBadRequest || !strings.Contains(body, "only ops, deploy, root") {
		t.Errorf("ubuntu, not on the list, was %d: %s", code, body)
	}
	if code, body := ask(asset.Hostname, "ops"); code != http.StatusOK {
		t.Errorf("ops was refused: %d %s", code, body)
	}
	// A server not in the inventory is the approver's call, as before.
	if code, body := ask("nowhere-01.example", "deploy"); code != http.StatusOK {
		t.Errorf("an unknown server was refused: %d %s", code, body)
	}
}

func TestRequestableAccountsAreBounded(t *testing.T) {
	_, h, _ := reportAPI(t)
	q := url.Values{}
	for i := range maxRequestHosts + 1 {
		q.Add("host", "h"+strings.Repeat("x", i))
	}
	if w := do(t, h, http.MethodGet, "/api/v1/requests/accounts?"+q.Encode(), "operator-token", ""); w.Code != http.StatusBadRequest {
		t.Errorf("%d servers at once: %d", maxRequestHosts+1, w.Code)
	}
}

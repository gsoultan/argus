package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
Who can log in to a server, and how a direct login signed in. These hold that
the list the agent sends reaches the console for those who see the fleet and
no one else, and that the key a login used is on the approval an admin
decides and in the audit entry.
*/

func TestLoginAccountsReachTheConsole(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	// Before the agent says anything, nothing is reported.
	w := do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/login-accounts", "admin-token", "")
	var got ServerLoginAccounts
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.Reported {
		t.Fatalf("before a report: %d %s", w.Code, w.Body)
	}

	body := `{"hostname":"` + asset.Hostname + `","version":"1.0.0","active_sessions":0,` +
		`"posture":{"rating":"monitored","unmanaged_keys":[],"shim_installed":true,"drift":[],` +
		`"login_accounts":[{"user":"staging","password":"set","keys":1,"can_login":true},` +
		`{"user":"sys-admin","password":"set","keys":0,"can_login":true}],` +
		`"sshd_auth":{"passwords":true,"keys":true,"expose_auth_info":true}}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body); w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/login-accounts", "admin-token", "")
	got = ServerLoginAccounts{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Reported || len(got.Accounts) != 2 || !got.ExposeAuthInfo || !got.Passwords {
		t.Fatalf("after a report: %+v", got)
	}
	if a := got.Accounts[1]; a.User != "sys-admin" || a.Password != "set" || a.Keys != 0 || !a.CanLogin {
		t.Errorf("sys-admin = %+v", a)
	}

	if w := do(t, h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/login-accounts", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator read who can log in: %d", w.Code)
	}
}

// The key a held login used is on the request an admin approves, and in the
// audit entry that says it is waiting.
func TestADirectLoginSaysWhichKey(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHApprove)

	w := do(t, h, http.MethodPost, "/api/v1/report/direct-login", cred,
		`{"hostname":"`+asset.Hostname+`","principal":"sys-admin","client_addr":"198.51.100.7","command":"",`+
			`"auth":{"method":"publickey","key_fingerprint":"SHA256:abc","key_type":"ssh-ed25519",`+
			`"key_comment":"gembit@laptop","key_found":true}}`)
	var r directLoginReply
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if r.State != "pending" {
		t.Fatalf("not held: %s", w.Body)
	}
	d, err := s.DirectLoginByID(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.AuthMethod != "publickey" || d.KeyFingerprint != "SHA256:abc" || d.KeyComment != "gembit@laptop" || !d.KeyFound {
		t.Errorf("held login %+v", d)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_requested" ||
		!strings.Contains(e.Detail, "SHA256:abc") || !strings.Contains(e.Detail, "gembit@laptop") {
		t.Errorf("audit does not name the key: %s", e.Detail)
	}

	// A password login says so in the allowed entry.
	setServerDirectSSH(t, h, asset.ID, DirectSSHAllow)
	if w := do(t, h, http.MethodPost, "/api/v1/report/login", cred,
		`{"hostname":"`+asset.Hostname+`","principal":"staging","allowed":true,"auth":{"method":"password"}}`); w.Code != http.StatusOK {
		t.Fatalf("login report: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_allowed" ||
		!strings.Contains(e.Detail, "signed in with password") {
		t.Errorf("allowed entry: %s / %s", e.Action, e.Detail)
	}
}

package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
Direct SSH logins per login account. These hold that a named account's mode
is what the control plane decides on, in both directions; that an agent too
old to read the accounts is told the strictest mode, so a named account is
never more open there; and that naming or clearing an account is audited,
with opening one up as a loosening.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run DirectSSHAccount
*/

func setAccountDirectSSH(t *testing.T, h http.Handler, id, account, mode string) {
	t.Helper()
	w := do(t, h, http.MethodPut, "/api/v1/assets/"+id+"/direct-ssh/accounts/"+account, "admin-token",
		`{"mode":"`+mode+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set direct ssh for %s to %q: %d %s", account, mode, w.Code, w.Body)
	}
}

func askDirectAs(t *testing.T, h http.Handler, cred, host, account string) directLoginReply {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/v1/report/direct-login", cred,
		`{"hostname":"`+host+`","principal":"`+account+`","client_addr":"198.51.100.7","command":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("direct-login: %d %s", w.Code, w.Body)
	}
	var out directLoginReply
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// The user's example: on a server that allows direct logins, sys-admin waits
// for an approver and staging is let in; on one that refuses, staging is
// still let in and everyone else is refused.
func TestDirectSSHAccountDecidesTheLogin(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)

	setServerDirectSSH(t, h, asset.ID, DirectSSHAllow)
	setAccountDirectSSH(t, h, asset.ID, "sys-admin", DirectSSHApprove)
	if r := askDirectAs(t, h, cred, asset.Hostname, "sys-admin"); r.State != "pending" || r.ID == "" {
		t.Fatalf("sys-admin on an open server: %+v, want held", r)
	}
	if r := askDirectAs(t, h, cred, asset.Hostname, "staging"); r.State != "approved" || r.ID != "" {
		t.Fatalf("staging on an open server: %+v, want allowed with no row", r)
	}

	setServerDirectSSH(t, h, asset.ID, DirectSSHRefuse)
	setAccountDirectSSH(t, h, asset.ID, "staging", DirectSSHAllow)
	if r := askDirectAs(t, h, cred, asset.Hostname, "staging"); r.State != "approved" {
		t.Fatalf("staging on a closed server: %+v, want allowed", r)
	}
	if r := askDirectAs(t, h, cred, asset.Hostname, "ops"); r.State != "denied" || r.ID != "" {
		t.Fatalf("ops on a closed server: %+v, want refused", r)
	}

	// An agent's "allowed" is believed for the account that is allowed, not
	// for another on the same server.
	reportDirectSession(t, h, cred, asset.Hostname, `,"directApproval":"allowed"`) // principal ops
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_detected" || e.Severity != "critical" {
		t.Errorf("an allowed claim for a refused account was believed: %s/%s", e.Action, e.Severity)
	}
	setAccountDirectSSH(t, h, asset.ID, "ops", DirectSSHAllow)
	reportDirectSession(t, h, cred, asset.Hostname, `,"directApproval":"allowed"`)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_recorded" {
		t.Errorf("an allowed session for an allowed account audited as %s", e.Action)
	}
}

// The heartbeat carries the accounts. direct_ssh is the strictest mode any
// account gets, for an agent that cannot read them, and direct_ssh_default
// is what every other account gets.
func TestDirectSSHAccountRidesTheHeartbeat(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHAllow)
	setAccountDirectSSH(t, h, asset.ID, "sys-admin", DirectSSHApprove)
	setAccountDirectSSH(t, h, asset.ID, "staging", DirectSSHAllow)

	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred,
		`{"hostname":"`+asset.Hostname+`","version":"1.0.0","active_sessions":0}`)
	var got struct {
		ConsoleOnly *bool             `json:"console_only"`
		DirectSSH   *string           `json:"direct_ssh"`
		Default     *string           `json:"direct_ssh_default"`
		Accounts    map[string]string `json:"direct_ssh_accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DirectSSH == nil || *got.DirectSSH != DirectSSHApprove || got.ConsoleOnly == nil || !*got.ConsoleOnly {
		t.Errorf("an old agent is told %v / console_only %v, want approve and on", got.DirectSSH, got.ConsoleOnly)
	}
	if got.Default == nil || *got.Default != DirectSSHAllow {
		t.Errorf("default %v, want allow", got.Default)
	}
	if got.Accounts["sys-admin"] != DirectSSHApprove || got.Accounts["staging"] != DirectSSHAllow || len(got.Accounts) != 2 {
		t.Errorf("accounts %v", got.Accounts)
	}

	a, err := s.AssetByID(context.Background(), asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.DirectSSHAccounts["sys-admin"] != DirectSSHApprove {
		t.Errorf("the console cannot see the accounts: %v", a.DirectSSHAccounts)
	}
}

// Naming an account is audited; opening one up is a loosening, and so is
// clearing a strict account back to an open server.
func TestDirectSSHAccountChangesAreAudited(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, _ := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHAllow)

	setAccountDirectSSH(t, h, asset.ID, "sys-admin", DirectSSHApprove)
	e := lastAuditOn(t, s, asset.Hostname)
	if e.Action != "asset.direct_ssh_account_changed" || e.Severity != "notice" ||
		!strings.Contains(e.Detail, "sys-admin") || e.ActorEmail == "" {
		t.Errorf("tightening audited as %+v", e)
	}
	setAccountDirectSSH(t, h, asset.ID, "sys-admin", "")
	if e := lastAuditOn(t, s, asset.Hostname); e.Severity != "warning" {
		t.Errorf("clearing a strict account on an open server audited as %s", e.Severity)
	}
	a, _ := s.AssetByID(context.Background(), asset.ID)
	if _, ok := a.DirectSSHAccounts["sys-admin"]; ok {
		t.Errorf("cleared account still named: %v", a.DirectSSHAccounts)
	}
}

// Only an admin may name an account, and a name that cannot be a login or a
// mode that is not one of the three is refused.
func TestDirectSSHAccountIsGuarded(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, _ := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/direct-ssh/accounts/"
	if w := do(t, h, http.MethodPut, path+"staging", "operator-token", `{"mode":"allow"}`); w.Code != http.StatusForbidden {
		t.Errorf("an operator set an account: %d", w.Code)
	}
	if w := do(t, h, http.MethodPut, path+"staging", "admin-token", `{"mode":"sometimes"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown mode: %d", w.Code)
	}
	if w := do(t, h, http.MethodPut, path+"-rf", "admin-token", `{"mode":"allow"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a name that cannot be a login: %d", w.Code)
	}
	a, _ := s.AssetByID(context.Background(), asset.ID)
	if len(a.DirectSSHAccounts) != 0 {
		t.Errorf("a refused change was kept: %v", a.DirectSSHAccounts)
	}
}

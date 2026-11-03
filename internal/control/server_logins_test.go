package control

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

/*
Login accounts an admin makes on a server from the console. What is held:
only an admin or owner can make, lock or delete one; each step is an audit
entry; the agent is told to keep or delete exactly those; a removal locks
rather than deletes; and deleting for good needs a locked account and its
name typed again.
*/

func heartbeatLogins(t *testing.T, h http.Handler, cred, host, states string) ServerLoginsWanted {
	t.Helper()
	body := `{"hostname":"` + host + `","version":"1.0.0","active_sessions":0,"personal_accounts":` + states + `}`
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, body)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var reply struct {
		ServerLogins *ServerLoginsWanted `json:"server_logins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.ServerLogins == nil {
		t.Fatalf("no server_logins in the reply: %s", w.Body)
	}
	return *reply.ServerLogins
}

func TestAdminsMakeAndRemoveServerLogins(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/server-logins"

	// Only an admin or owner may.
	if w := do(t, h, http.MethodPost, path, "operator-token", `{"name":"deploy"}`); w.Code != http.StatusForbidden {
		t.Fatalf("an operator made a login: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{"root", "Deploy", "a b", "ops"} {
		// ops is already on the list: Argus does not adopt an account it did not make.
		if w := do(t, h, http.MethodPost, path, "admin-token", `{"name":"`+bad+`"}`); w.Code != http.StatusBadRequest {
			t.Errorf("made %q: %d %s", bad, w.Code, w.Body)
		}
	}

	w := do(t, h, http.MethodPost, path, "admin-token", `{"name":"deploy"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("make deploy: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "server_login.created" ||
		e.ActorEmail == "" || !strings.Contains(e.Detail, "deploy") {
		t.Errorf("audit: %+v", e)
	}
	got, _ := s.AssetByID(context.Background(), asset.ID)
	if !slices.Contains(got.Principals, "deploy") {
		t.Errorf("deploy is not on the login accounts list: %v", got.Principals)
	}
	if want := heartbeatLogins(t, h, cred, asset.Hostname, `[]`); !slices.Equal(want.Keep, []string{"deploy"}) || len(want.Delete) != 0 {
		t.Errorf("agent told %+v", want)
	}

	// Deleting for good needs it locked first, and the name typed again.
	if w := do(t, h, http.MethodDelete, path+"/deploy?forGood=true&confirm=deploy", "admin-token", ""); w.Code != http.StatusBadRequest {
		t.Errorf("deleted an active login: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodDelete, path+"/deploy", "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator removed a login: %d", w.Code)
	}
	if w := do(t, h, http.MethodDelete, path+"/deploy", "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("lock deploy: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "server_login.locked" {
		t.Errorf("audit: %+v", e)
	}
	got, _ = s.AssetByID(context.Background(), asset.ID)
	if slices.Contains(got.Principals, "deploy") {
		t.Errorf("a locked login is still offered: %v", got.Principals)
	}
	if want := heartbeatLogins(t, h, cred, asset.Hostname, `[]`); len(want.Keep) != 0 || len(want.Delete) != 0 {
		t.Errorf("after locking, agent told %+v", want)
	}

	if w := do(t, h, http.MethodDelete, path+"/deploy?forGood=true&confirm=nope", "admin-token", ""); w.Code != http.StatusBadRequest {
		t.Errorf("deleted without the name typed: %d", w.Code)
	}
	if w := do(t, h, http.MethodDelete, path+"/deploy?forGood=true&confirm=deploy", "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("delete deploy: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "server_login.delete_requested" || e.Severity != "critical" {
		t.Errorf("audit: %+v", e)
	}
	if want := heartbeatLogins(t, h, cred, asset.Hostname, `[]`); !slices.Equal(want.Delete, []string{"deploy"}) {
		t.Errorf("agent not told to delete: %+v", want)
	}

	// The agent says it is gone: the row closes and the audit log says so.
	heartbeatLogins(t, h, cred, asset.Hostname, `[{"name":"deploy","owner":"shared login","state":"deleted"}]`)
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "server_login.deleted" || !strings.HasPrefix(e.ActorEmail, "agent:") {
		t.Errorf("audit: %+v", e)
	}
	logins, err := s.ServerLogins(context.Background(), asset.ID)
	if err != nil || len(logins) != 1 || logins[0].State != "deleted" {
		t.Fatalf("after delete: %+v %v", logins, err)
	}
	if want := heartbeatLogins(t, h, cred, asset.Hostname, `[]`); len(want.Delete) != 0 {
		t.Errorf("still told to delete: %+v", want)
	}

	// Making it again starts a new account.
	if w := do(t, h, http.MethodPost, path, "admin-token", `{"name":"deploy"}`); w.Code != http.StatusOK {
		t.Errorf("remake deploy: %d %s", w.Code, w.Body)
	}
}

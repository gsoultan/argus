package control

import (
	"context"
	"net/http"
	"testing"
)

// The person waiting at their own terminal can take the login back or
// remind the approvers, and only for a login that is theirs.
func TestMyWaitingDirectLoginCanBeWithdrawnOrReminded(t *testing.T) {
	shortPolls(t)
	a, s, h, _ := enrollAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	setServerDirectSSH(t, h, asset.ID, DirectSSHApprove)
	budi, _ := newAccount(t, s, "operator")
	citra, _ := newAccount(t, s, "operator")
	a.UserTokens["budi-token"] = budi
	a.UserTokens["citra-token"] = citra
	if w := do(t, h, http.MethodPut, "/api/v1/assets/"+asset.ID+"/assignments", "admin-token",
		`{"email":"`+budi+`","principals":[],"personalAccounts":true,"directSsh":"approve"}`); w.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", w.Code, w.Body)
	}
	acct, err := s.ServerAccountOf(context.Background(), budi)
	if err != nil || acct.LoginName == "" {
		t.Fatalf("no login name for budi: %+v %v", acct, err)
	}

	r := askDirectAs(t, h, cred, asset.Hostname, acct.LoginName)
	if r.State != "pending" {
		t.Fatalf("not held: %+v", r)
	}
	path := "/api/v1/direct-logins/" + r.ID

	// Someone else's login is not theirs to touch, and says nothing.
	for _, op := range []string{"/withdraw", "/remind"} {
		if w := do(t, h, http.MethodPost, path+op, "citra-token", `{}`); w.Code != http.StatusNotFound {
			t.Errorf("citra %s: %d %s", op, w.Code, w.Body)
		}
		if w := do(t, h, http.MethodPost, path+op, "", `{}`); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d", op, w.Code)
		}
	}

	if w := do(t, h, http.MethodPost, path+"/remind", "budi-token", `{}`); w.Code != http.StatusOK {
		t.Fatalf("remind: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, path+"/remind", "budi-token", `{}`); w.Code != http.StatusTooManyRequests {
		t.Errorf("a second reminder straight away: %d", w.Code)
	}

	if w := do(t, h, http.MethodPost, path+"/withdraw", "budi-token", `{}`); w.Code != http.StatusOK {
		t.Fatalf("withdraw: %d %s", w.Code, w.Body)
	}
	if e := lastAuditOn(t, s, asset.Hostname); e.Action != "session.direct_withdrawn" || e.ActorEmail != budi {
		t.Errorf("withdrawal audited as %+v", e)
	}
	// The terminal is told no at once.
	if code, got := waitDirect(t, h, cred, r.ID); code != http.StatusOK || got.State != "denied" {
		t.Fatalf("agent told %d %+v", code, got)
	}
	if w := do(t, h, http.MethodPost, path+"/withdraw", "budi-token", `{}`); w.Code != http.StatusConflict {
		t.Errorf("withdrawn twice: %d", w.Code)
	}
}

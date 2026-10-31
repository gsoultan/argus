package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// getMyServerAccount is the signed-in person's own server account: its name
// on servers and the public key they reach it directly with.
func (a *API) getMyServerAccount(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	acct, err := a.store.ServerAccountOf(r.Context(), sess.Email)
	if errors.Is(err, ErrNoAccount) {
		// A static token has no row to hold a key.
		writeErr(w, http.StatusNotFound, "this sign-in has no account of its own to hold a key")
		return
	}
	if err != nil {
		a.fail(w, "server account", err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

// putMySSHKey sets or clears the signed-in person's own public key. Nobody
// sets another person's: a key put on someone's behalf is a way in that is
// theirs only on paper.
func (a *API) putMySSHKey(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	fp, err := a.store.SetSSHKey(r.Context(), sess.Email, in.Key)
	switch {
	case err == nil:
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "this sign-in has no account of its own to hold a key")
		return
	case errors.Is(err, ErrNoDirectSSH):
		writeErr(w, http.StatusForbidden, err.Error())
		return
	case errors.As(err, new(ErrInvalidAsset)):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	default:
		a.fail(w, "set ssh key", err)
		return
	}
	detail := sess.Email + " removed their SSH key. Direct SSH to their personal accounts stops at the next heartbeat."
	if fp != "" {
		detail = fmt.Sprintf("%s set their SSH key to %s. It is written into their personal "+
			"accounts on servers where a team of theirs may SSH directly.", sess.Email, fp)
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "account.ssh_key_changed", Severity: "notice", ActorEmail: sess.Email,
		Target: sess.Email, Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	a.getMyServerAccount(w, r)
}

// putLoginName renames someone's personal server account, for a name that is
// already taken on a server by an account Argus did not make.
func (a *API) putLoginName(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "renaming a server account", "admin", "owner") {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	before, err := a.store.SetLoginName(r.Context(), email, in.Name)
	switch {
	case err == nil:
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	case errors.Is(err, ErrLoginNameTaken):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case errors.As(err, new(ErrInvalidAsset)):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	default:
		a.fail(w, "rename server account", err)
		return
	}
	name := strings.TrimSpace(in.Name)
	if before != name {
		detail := fmt.Sprintf("Server account of %s named %s.", email, name)
		if before != "" {
			detail = fmt.Sprintf("Server account of %s renamed from %s to %s. %s is locked on every "+
				"server at its next heartbeat and %s made in its place; %s's history stays as it was.",
				email, before, name, before, name, before)
		}
		if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
			Action: "account.login_name_changed", Severity: "notice", ActorEmail: sess.Email,
			Target: email, Detail: detail,
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email, "loginName": name})
}

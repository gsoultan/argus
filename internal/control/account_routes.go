package control

import (
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/notify"
)

// Accounts from the console: the same acts as `argus-control users add`,
// `disable` and `enable`, for an administrator who is signed in rather than
// on the host. Each is audited under the administrator who did it.

type accountInput struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Password    string `json:"password"`
}

func (a *API) postAccount(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "adding an account requires the admin or owner role")
		return
	}
	var in accountInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	email := normaliseEmail(in.Email)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		writeErr(w, http.StatusBadRequest, "enter an email address, such as you@example.com")
		return
	}
	switch in.Role {
	case "admin", "approver", "operator", "auditor":
	case "owner":
		// An administrator who could make an owner could make themselves one.
		if sess.Role != "owner" {
			writeErr(w, http.StatusForbidden, "only an owner can add another owner")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown role %q", in.Role))
		return
	}
	// The first password is a temporary one whoever chose it: generated here
	// unless the request names one (the API, a script), and changed by the
	// person at their first sign-in either way.
	password := in.Password
	if password == "" {
		pw, err := auth.NewTemporaryPassword()
		if err != nil {
			a.fail(w, "generate password", err)
			return
		}
		password = pw
	} else if len([]rune(password)) < auth.MinPasswordLength {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("the password must be at least %d characters", auth.MinPasswordLength))
		return
	}
	// CreateAccount would overwrite an existing account's password and role,
	// which from here would be a way to take one over. A row with no password
	// is not an account yet, and giving it one is what this is for.
	existing, err := a.store.Account(r.Context(), email)
	if err == nil && existing.Shared() {
		// Nor is a person shared in from elsewhere given a second password here.
		writeErr(w, http.StatusConflict, email+" is already here as a shared person")
		return
	} else if err == nil && existing.PasswordHash != "" {
		writeErr(w, http.StatusConflict, email+" already has an account")
		return
	} else if err != nil && !errors.Is(err, ErrNoAccount) {
		a.fail(w, "look up account", err)
		return
	}
	until := time.Now().Add(a.temporaryPasswordTTL()).UTC().Truncate(time.Second)
	name := strings.TrimSpace(in.DisplayName)
	if _, err := a.store.CreateAccountWith(r.Context(), NewAccount{Email: email,
		DisplayName: name, Role: in.Role, Password: password, TemporaryUntil: &until}); err != nil {
		if errors.Is(err, ErrSharedAccount) {
			writeErr(w, http.StatusConflict, email+" is already here as a shared person")
			return
		}
		a.fail(w, "create account", err)
		return
	}
	var out temporaryPasswordOutcome
	if in.Password != "" {
		// The administrator chose it and has it; there is nothing to send.
		out = temporaryPasswordOutcome{Email: email, Role: in.Role, ExpiresAt: until}
	} else {
		out = a.deliverTemporaryPassword(r.Context(), notify.Welcome{
			Email: email, Name: name, Role: in.Role, Password: password,
			ExpiresAt: until, By: sess.Email,
		})
	}
	detail := "Local account created from the console with role " + in.Role + ". "
	if in.Password != "" {
		detail += "The administrator set its first password; it must be changed at the first sign-in."
	} else {
		detail += out.auditText()
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "user.created", Severity: "notice", ActorEmail: sess.Email,
		Target: email, Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) postAccountDisabled(disabled bool) http.HandlerFunc {
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if sess.Role != "admin" && sess.Role != "owner" {
			writeErr(w, http.StatusForbidden, verb+" an account requires the admin or owner role")
			return
		}
		email := normaliseEmail(r.PathValue("email"))
		if disabled && strings.EqualFold(email, sess.Email) {
			writeErr(w, http.StatusBadRequest, "you cannot disable your own account; ask another administrator")
			return
		}
		// An admin may not change an owner's role, so may not take an owner
		// out either: disabling every owner was a way round that rule.
		if !a.mayManage(w, r, sess.Role, email, verb+" an owner") {
			return
		}
		switch err := a.store.SetAccountDisabledBy(r.Context(), email, disabled, sess.Email); {
		case err == nil:
		case errors.Is(err, ErrLastAdmin):
			writeErr(w, http.StatusConflict,
				"refusing: "+err.Error()+". Give another account the admin or owner role first.")
			return
		case errors.Is(err, ErrAlreadyInState):
			writeJSON(w, http.StatusOK, map[string]string{"email": email})
			return
		case errors.Is(err, ErrNoAccount):
			writeErr(w, http.StatusNotFound, "no account for "+email)
			return
		default:
			a.fail(w, verb+" account", err)
			return
		}
		if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
			Action: "account." + verb + "d", Severity: "notice", ActorEmail: sess.Email,
			Target: email,
			Detail: fmt.Sprintf("Account %s %sd from the console.", email, verb),
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
		writeJSON(w, http.StatusOK, map[string]string{"email": email})
	}
}

// putAccountRole changes someone's role from the People page.
//
// Nobody changes their own: an administrator who could would need no one's
// agreement to become an owner, and one who demoted themselves by mistake
// could not undo it. Only an owner makes or unmakes an owner.
func (a *API) putAccountRole(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "changing a role requires the admin or owner role")
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	if strings.EqualFold(email, sess.Email) {
		writeErr(w, http.StatusBadRequest, "you cannot change your own role; ask another administrator")
		return
	}
	switch in.Role {
	case "admin", "approver", "operator", "auditor", "owner":
	default:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown role %q", in.Role))
		return
	}
	acct, err := a.store.Account(r.Context(), email)
	if errors.Is(err, ErrNoAccount) {
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	}
	if err != nil {
		a.fail(w, "look up account", err)
		return
	}
	if (in.Role == "owner" || acct.Role == "owner") && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "only an owner can make or change an owner")
		return
	}
	before, err := a.store.SetAccountRole(r.Context(), email, in.Role)
	switch {
	case err == nil:
	case errors.Is(err, ErrAlreadyInState):
		writeJSON(w, http.StatusOK, map[string]string{"email": email, "role": in.Role})
		return
	case errors.Is(err, ErrLastAdmin):
		writeErr(w, http.StatusConflict,
			"refusing: "+err.Error()+". Give another account the admin or owner role first.")
		return
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	default:
		a.fail(w, "change role", err)
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "account.role_changed", Severity: "notice", ActorEmail: sess.Email,
		Target: email,
		Detail: fmt.Sprintf("Role of %s changed from %s to %s from the console.", email, before, in.Role),
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email, "role": in.Role})
}

// mayManage refuses an admin acting on an owner. Writes the response itself
// and returns false when it refuses.
func (a *API) mayManage(w http.ResponseWriter, r *http.Request, actorRole, email, what string) bool {
	if actorRole == "owner" {
		return true
	}
	target, err := a.store.Account(r.Context(), email)
	switch {
	case errors.Is(err, ErrNoAccount):
		return true // the caller reports the missing account its own way
	case err != nil:
		a.fail(w, "look up account", err)
		return false
	case target.Role == "owner":
		writeErr(w, http.StatusForbidden, "only an owner can "+what)
		return false
	}
	return true
}

// postResetMFA removes someone's second factor, for a person who has lost
// their authenticator and their recovery codes.
//
// Admin or owner, never on yourself (a stolen session must not be able to
// strip its own account's second factor), and only an owner may reset an
// owner's. The account is signed out everywhere and must enrol again before
// it can do anything else, when its role requires a second factor.
func (a *API) postResetMFA(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "resetting a second factor requires the admin or owner role")
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	if strings.EqualFold(email, sess.Email) {
		writeErr(w, http.StatusBadRequest,
			"you cannot reset your own second factor; set up a new one from your account, "+
				"or ask another administrator")
		return
	}
	if !a.mayManage(w, r, sess.Role, email, "reset an owner's second factor") {
		return
	}
	switch err := a.store.ResetMFA(r.Context(), email); {
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	case err != nil:
		a.fail(w, "reset second factor", err)
		return
	}
	detail := fmt.Sprintf("Second factor and recovery codes of %s removed from the console by %s. "+
		"The account was signed out everywhere and must enrol again.", email, sess.Email)
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "auth.mfa_reset", Severity: "warning", ActorEmail: sess.Email,
		Target: email, Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	a.Alerts.SecurityEvent("Argus: second factor of "+email+" was reset", detail)
	a.alertSecurity(r.Context(), sess.Email, "Second factor of "+email+" was reset", detail)
	writeJSON(w, http.StatusOK, map[string]string{"email": email})
}

// postSignOutAccount ends every session someone has, for a cookie that may
// have been stolen. Admin or owner; only an owner may sign out an owner.
func (a *API) postSignOutAccount(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "signing someone out requires the admin or owner role")
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	if !a.mayManage(w, r, sess.Role, email, "sign out an owner") {
		return
	}
	switch err := a.store.RevokeAllSessions(r.Context(), email); {
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	case err != nil:
		a.fail(w, "revoke sessions", err)
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "auth.sessions_revoked", Severity: "notice", ActorEmail: sess.Email,
		Target: email,
		Detail: fmt.Sprintf("Every session of %s was signed out from the console.", email),
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email})
}

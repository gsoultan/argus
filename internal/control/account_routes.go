package control

import (
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/gsoultan/argus/internal/auth"
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
	if len([]rune(in.Password)) < auth.MinPasswordLength {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("the password must be at least %d characters", auth.MinPasswordLength))
		return
	}
	// CreateAccount would overwrite an existing account's password and role,
	// which from here would be a way to take one over. A row with no password
	// is not an account yet, and giving it one is what this is for.
	if existing, err := a.store.Account(r.Context(), email); err == nil && existing.PasswordHash != "" {
		writeErr(w, http.StatusConflict, email+" already has an account")
		return
	} else if err != nil && !errors.Is(err, ErrNoAccount) {
		a.fail(w, "look up account", err)
		return
	}
	if _, err := a.store.CreateAccount(r.Context(), email,
		strings.TrimSpace(in.DisplayName), in.Role, in.Password); err != nil {
		a.fail(w, "create account", err)
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "user.created", Severity: "notice", ActorEmail: sess.Email,
		Target: email,
		Detail: "Local account created from the console with role " + in.Role + ".",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"email": email, "role": in.Role})
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
		switch err := a.store.SetAccountDisabled(r.Context(), email, disabled); {
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

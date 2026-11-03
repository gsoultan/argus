package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/notify"
)

/*
Temporary passwords: how a person added from the console first signs in.

Nobody picks the first password. Argus generates one, emails it to the person
when email is set up, and otherwise shows it once to the administrator who
added them, saying it could not be emailed. Either way it is temporary:

  - It must be changed at the first sign-in, before anything else -- the gate
    holds the session to /auth/password/change (session_gate.go). So the
    password an administrator could have read, or that sat in a mailbox, is
    never the one in use.
  - It stops working after TemporaryPasswordTTL if it is not used, so an
    unanswered invitation is not a standing credential.
  - The new password must pass auth.CheckPasswordStrength.
  - Then, with require_mfa at its default, the person must set up an
    authenticator before the console shows them anything.

An administrator can send a new one ("Send a new temporary password"): to a
person who never signed in and let theirs run out, or who forgot theirs. With
email set up it goes only to the person, never to the administrator. Without
email it is shown to the administrator once, and only for someone who has not
yet chosen a password of their own: for anyone else that would let the
administrator sign in as them. The person's second factor is left in place.
*/

// DefaultTemporaryPasswordTTL is how long a temporary password works unused.
const DefaultTemporaryPasswordTTL = 72 * time.Hour

func (a *API) temporaryPasswordTTL() time.Duration {
	if a.TemporaryPasswordTTL > 0 {
		return a.TemporaryPasswordTTL
	}
	return DefaultTemporaryPasswordTTL
}

// canEmailPeople reports whether a temporary password can be emailed.
func (a *API) canEmailPeople() bool { return a.SignInAlerts.EmailOn() }

// temporaryPasswordOutcome is what the administrator is told: either it was
// emailed, or here it is, and why it was not.
type temporaryPasswordOutcome struct {
	Email     string    `json:"email"`
	Role      string    `json:"role,omitempty"`
	ExpiresAt time.Time `json:"temporaryPasswordExpiresAt"`
	Emailed   bool      `json:"emailed"`
	// EmailError is why it could not be emailed, when it was tried.
	EmailError string `json:"emailError,omitempty"`
	// TemporaryPassword is set only when it was not emailed, for the
	// administrator to hand over. Shown once; stored only as a hash.
	TemporaryPassword string `json:"temporaryPassword,omitempty"`
}

// deliverTemporaryPassword emails pw to the person, or returns it in the
// outcome when it cannot be emailed.
func (a *API) deliverTemporaryPassword(ctx context.Context, w notify.Welcome) temporaryPasswordOutcome {
	out := temporaryPasswordOutcome{Email: w.Email, Role: w.Role, ExpiresAt: w.ExpiresAt}
	w.Org = orgLabel(ctx)
	w.MFARequired = a.mfaRequiredFor(w.Role)
	err := a.SignInAlerts.SendWelcome(ctx, w)
	switch {
	case err == nil:
		out.Emailed = true
	case errors.Is(err, notify.ErrNoEmail):
		out.TemporaryPassword = w.Password
	default:
		a.log.Error("could not email a temporary password", "email", w.Email, "error", err)
		out.EmailError = "the mail server refused it: " + err.Error()
		out.TemporaryPassword = w.Password
	}
	return out
}

func (o temporaryPasswordOutcome) auditText() string {
	if o.Emailed {
		return "The temporary password was emailed to " + o.Email + "; it works until " +
			o.ExpiresAt.UTC().Format(time.RFC3339) + " and must be changed at the first sign-in."
	}
	why := "email is not set up"
	if o.EmailError != "" {
		why = "emailing it failed"
	}
	return "The temporary password was shown to the administrator, because " + why +
		"; it works until " + o.ExpiresAt.UTC().Format(time.RFC3339) +
		" and must be changed at the first sign-in."
}

// postTemporaryPassword sends someone a new temporary password.
func (a *API) postTemporaryPassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "sending a temporary password requires the admin or owner role")
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	if strings.EqualFold(email, sess.Email) {
		writeErr(w, http.StatusBadRequest,
			"you cannot reset your own password from here; change it from your account menu")
		return
	}
	if !a.mayManage(w, r, sess.Role, email, "send an owner a temporary password") {
		return
	}
	acct, err := a.store.Account(r.Context(), email)
	switch {
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	case err != nil:
		a.fail(w, "look up account", err)
		return
	case acct.Shared():
		writeErr(w, http.StatusConflict, email+
			" signs in through another organization; their password is reset there")
		return
	case acct.Disabled:
		writeErr(w, http.StatusConflict, email+" is disabled; enable the account first")
		return
	case !a.canEmailPeople() && !acct.PasswordChangeRequired:
		// Shown to the administrator, a new password for someone who has
		// one of their own would let the administrator sign in as them.
		// Before they ever chose one, it is only a fresh invitation.
		writeErr(w, http.StatusConflict, "set up email in Settings, Alerts first: "+email+
			" has chosen their own password, so a new one can only be sent to them, not shown here")
		return
	}
	pw, err := auth.NewTemporaryPassword()
	if err != nil {
		a.fail(w, "generate password", err)
		return
	}
	until := time.Now().Add(a.temporaryPasswordTTL()).UTC().Truncate(time.Second)
	if err := a.store.SetTemporaryPassword(r.Context(), email, pw, until); err != nil {
		if errors.Is(err, ErrNoAccount) {
			writeErr(w, http.StatusNotFound, "no account for "+email)
			return
		}
		a.fail(w, "set temporary password", err)
		return
	}
	out := a.deliverTemporaryPassword(r.Context(), notify.Welcome{
		Email: acct.Email, Name: acct.DisplayName, Role: acct.Role,
		Password: pw, ExpiresAt: until, By: sess.Email, Reset: true,
	})
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "user.temporary_password", Severity: "warning", ActorEmail: sess.Email,
		Target: email,
		Detail: "Sent " + email + " a new temporary password from the console. Their old password " +
			"stopped working and every session on the account was signed out; their second factor " +
			"is unchanged. " + out.auditText(),
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// checkNewPassword is what a password a person chooses must pass.
func checkNewPassword(pw, email, name string) error {
	if err := auth.CheckPasswordStrength(pw, auth.PasswordContext{Email: email, Name: name}); err != nil {
		return fmt.Errorf("choose a stronger password: %w", err)
	}
	return nil
}

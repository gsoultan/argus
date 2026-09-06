package control

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

// Sign-in for accounts Argus holds itself.
//
// Two steps when a second factor is enrolled: the password proves what someone
// knows, and only then does the console learn that a code is owed. The
// intermediate state is a signed, short-lived token rather than a server-side
// record, so a half-finished sign-in costs nothing to keep and expires whether
// or not the browser comes back.
//
// Every outcome is audited. A refused password is the event that matters most
// -- one is a typo, forty is an attack -- and it is the one an investigator
// will look for.

// mfaChallengeTTL is how long a password remains sufficient to present a code.
//
// Long enough to open an authenticator app and read one, short enough that a
// token captured from a log is worthless by the time anyone finds it.
const mfaChallengeTTL = 5 * time.Minute

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handlePasswordLogin verifies an email and password.
func (a *API) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if a.signer == nil {
		writeErr(w, http.StatusNotImplemented,
			"this control plane has no signing secret, so it cannot issue a session")
		return
	}
	var in loginRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	in.Email = strings.TrimSpace(in.Email)
	if in.Email == "" || in.Password == "" {
		writeErr(w, http.StatusBadRequest, "email and password are required")
		return
	}

	acct, err := a.store.Authenticate(r.Context(), in.Email, in.Password)
	if err != nil {
		// Deliberately the same message whatever went wrong. Telling someone
		// the address exists but the password is wrong hands them half the
		// answer, and hands a scanner the user list.
		a.log.Warn("password sign-in refused", "email", in.Email,
			"client", clientOf(r))
		a.RecordAuthFailure(r)
		a.auditAuth(r, "auth.password_failed", "warning", in.Email,
			"A password sign-in was refused.")
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	// The password was right. If a second factor is enrolled it is owed now,
	// and the account is not signed in until it is presented.
	if acct.MFAEnrolled {
		challenge, err := a.signer.IssueMFAChallenge(acct.Email, mfaChallengeTTL)
		if err != nil {
			a.fail(w, "issue challenge", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mfaRequired": true,
			"challenge":   challenge,
		})
		return
	}

	a.completeSignIn(w, r, auth.Session{
		Email: acct.Email, Name: acct.DisplayName, Role: acct.Role,
	}, "password")
}

type mfaRequest struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
	// Recovery is set when the code is one of the printed single-use codes
	// rather than one from an authenticator app.
	Recovery bool `json:"recovery"`
}

// handleMFAVerify completes a sign-in that owed a second factor.
func (a *API) handleMFAVerify(w http.ResponseWriter, r *http.Request) {
	if a.signer == nil {
		writeErr(w, http.StatusNotImplemented, "no signing secret configured")
		return
	}
	var in mfaRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}

	email, err := a.signer.RedeemMFAChallenge(in.Challenge)
	if err != nil {
		a.RecordAuthFailure(r)
		// Expired is worth saying, because the remedy differs: start again
		// rather than check the code.
		writeErr(w, http.StatusUnauthorized,
			"this sign-in expired or is not valid; start again")
		return
	}

	acct, err := a.store.Account(r.Context(), email)
	if err != nil || acct.Disabled {
		a.RecordAuthFailure(r)
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	verified := false
	switch {
	case in.Recovery:
		ok, err := a.store.ConsumeRecoveryCode(r.Context(), acct.ID, in.Code)
		if err != nil {
			a.fail(w, "consume recovery code", err)
			return
		}
		verified = ok
		if ok {
			left, _ := a.store.RemainingRecoveryCodes(r.Context(), acct.ID)
			// A recovery code means the usual second factor was unavailable,
			// which is either a lost device or somebody else's sign-in.
			a.auditAuth(r, "auth.recovery_code_used", "warning", acct.Email,
				"Signed in with a single-use recovery code. "+
					itoa(left)+" remain. If this was not the account holder, "+
					"treat the account as compromised.")
		}
	default:
		verified = auth.VerifyTOTP(acct.TOTPSecret, in.Code, time.Now())
	}

	if !verified {
		a.log.Warn("second factor refused", "email", acct.Email, "client", clientOf(r))
		a.RecordAuthFailure(r)
		a.auditAuth(r, "auth.mfa_failed", "warning", acct.Email,
			"A second factor was refused after a correct password.")
		writeErr(w, http.StatusUnauthorized, "that code is not correct")
		return
	}

	a.completeSignIn(w, r, auth.Session{
		Email: acct.Email, Name: acct.DisplayName, Role: acct.Role,
	}, "password+mfa")
}

// completeSignIn issues the session cookie and records the event.
func (a *API) completeSignIn(w http.ResponseWriter, r *http.Request, sess auth.Session, how string) {
	token, err := a.signer.IssueSession(sess, a.sessionTTL())
	if err != nil {
		a.fail(w, "issue session", err)
		return
	}
	auth.SetCookie(w, auth.SessionCookieName, token, a.sessionTTL(), a.secureCookies)

	_, _ = a.store.pool.Exec(r.Context(),
		`UPDATE users SET last_seen_at = now() WHERE lower(email) = lower($1)`, sess.Email)

	a.auditAuth(r, "auth.login", "info", sess.Email,
		"Signed in via "+how+" as role "+sess.Role+".")
	a.RecordAuthSuccess(r)
	a.log.Info("user signed in", "email", sess.Email, "role", sess.Role, "method", how)

	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"email":         sess.Email,
		"displayName":   sess.Name,
		"role":          sess.Role,
	})
}

/* ── Enrolment ───────────────────────────────────────────────────────────── */

// handleMFABegin starts enrolment for the signed-in account.
func (a *API) handleMFABegin(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	secret, err := a.store.BeginMFAEnrolment(r.Context(), sess.Email)
	if err != nil {
		a.fail(w, "begin enrolment", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret": secret,
		"uri":    auth.TOTPEnrolmentURI("Argus", sess.Email, secret),
	})
}

// handleMFAConfirm proves the app holds the same secret, then issues recovery
// codes. They are shown exactly once, because they are stored only as digests.
func (a *API) handleMFAConfirm(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	recovery, err := a.store.ConfirmMFAEnrolment(r.Context(), sess.Email, in.Code)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.auditAuth(r, "auth.mfa_enrolled", "notice", sess.Email,
		"Enrolled a second factor and was issued "+itoa(len(recovery))+" recovery codes.")
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": recovery})
}

// handleChangePassword lets the signed-in account replace its own password.
func (a *API) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	// The current password is required even though the session is already
	// authenticated: a borrowed, unlocked laptop must not be enough to lock the
	// owner out of their own account.
	if _, err := a.store.Authenticate(r.Context(), sess.Email, in.Current); err != nil {
		a.RecordAuthFailure(r)
		writeErr(w, http.StatusUnauthorized, "your current password is not correct")
		return
	}
	if err := a.store.SetPassword(r.Context(), sess.Email, in.New); err != nil {
		if errors.Is(err, ErrNoAccount) {
			writeErr(w, http.StatusNotFound, "no such account")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.auditAuth(r, "auth.password_changed", "notice", sess.Email, "Changed their own password.")
	writeJSON(w, http.StatusOK, map[string]string{"status": "password changed"})
}

/* ── Helpers ─────────────────────────────────────────────────────────────── */

func (a *API) auditAuth(r *http.Request, action, severity, actor, detail string) {
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: action, Severity: severity, ActorEmail: actor,
		Target: "console", Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
}

func clientOf(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

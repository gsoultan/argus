package control

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

// SetAuth enables session signing and terminal tickets.
func (a *API) SetAuth(signer *auth.Signer, consoleURL string, secureCookies bool) {
	a.signer = signer
	a.consoleURL = consoleURL
	a.secureCookies = secureCookies
}

// authenticate resolves the caller.
//
// Order matters: a session cookie is the real credential, and static tokens are
// a development fallback that must never shadow it.
//
// The static path is disabled outright once the deployment has a real way in.
// Gating it on anything narrower was the wrong test: a deployment signing
// people in with a password and a second factor still honoured a bearer token
// from the config file, and handed it admin regardless of whose address it
// named. Every control the login page enforces was one header away from being
// skipped.
//
// StaticTokensDisabled is decided at start-up, where the deployment can be
// refused outright rather than quietly downgraded.
func (a *API) authenticate(r *http.Request) (auth.Session, bool) {
	if a.signer != nil {
		if c, err := r.Cookie(auth.SessionCookieName); err == nil {
			if sess, err := a.signer.VerifySession(c.Value); err == nil {
				return sess, true
			}
		}
	}
	// Setup finishing in this process counts too. The check that refuses to
	// start with tokens alongside an account runs at start-up, and the account
	// the setup page just made did not exist then.
	if a.StaticTokensDisabled || a.setup.done.Load() {
		return auth.Session{}, false
	}
	email, ok := a.UserTokens[bearer(r)]
	if !ok {
		return auth.Session{}, false
	}
	// The role comes from the account, not from the fact that a token was
	// presented. Minting admin for whoever holds a string made the token more
	// powerful than any account it could name.
	role := "viewer"
	if acct, err := a.store.Account(r.Context(), email); err == nil && acct.Role != "" {
		role = acct.Role
	}
	return auth.Session{Email: email, Name: email, Role: role}, true
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess, ok := a.authenticate(r); ok {
		if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
			Action:     "auth.logout",
			Severity:   "info",
			ActorEmail: sess.Email,
			Target:     "console",
			Detail:     "Signed out.",
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
	}
	auth.ClearCookie(w, auth.SessionCookieName, a.secureCookies)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// handleMe reports the current identity, and whether login is even available.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		// The console needs to know which doors exist before it can draw one.
		// passwordEnabled is what makes the sign-in form appear; without it the
		// console shows no way in at all, which is the state this product spent
		// its whole life in.
		// accountsExist distinguishes "sign in" from "nobody can yet". On a
		// fresh install the form cannot succeed, and saying so beats letting
		// someone retype a password they never set.
		accounts := false
		if a.signer != nil {
			accounts, _ = a.store.AnyAccountExists(r.Context())
		}
		// setupAvailable sends a fresh install to /setup instead of to a
		// command, when this process is offering it.
		setup := false
		if a.signer != nil && !accounts {
			if st, err := a.setupState(r.Context()); err == nil {
				setup = st.Available
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"authenticated":   false,
			"passwordEnabled": a.signer != nil,
			"accountsExist":   accounts,
			"setupAvailable":  setup,
		})
		return
	}
	// mfaEnrolled drives the console's prompt to set one up. Looked up rather
	// than carried in the session, so enrolling takes effect immediately
	// instead of at the next sign-in.
	mfa := false
	if acct, err := a.store.Account(r.Context(), sess.Email); err == nil {
		mfa = acct.MFAEnrolled
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"email":         sess.Email,
		"displayName":   sess.Name,
		"role":          sess.Role,
		"expiresAt":     sess.ExpiresAt,
		"mfaEnrolled":   mfa,
	})
}

/* ── Terminal tickets ────────────────────────────────────────────────────── */

// handleTicket issues a single-use ticket for one terminal session.
//
// This is where authorisation happens: the console asks for permission to open
// a specific shell or desktop, the control plane decides, and the agent only
// has to verify the grant it is handed. Without it every agent would need its
// own copy of the policy, which is a second place for the rules to drift.
func (a *API) handleTicket(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var in struct {
		Target    string `json:"target"`
		Principal string `json:"principal"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Target == "" || in.Principal == "" {
		writeErr(w, http.StatusBadRequest, "target and principal are required")
		return
	}

	// An auditor may read every recording and open none. Enforced here rather
	// than in the console, because a UI that hides a button is not a control.
	if sess.Role == "auditor" {
		a.log.Warn("terminal denied by role",
			"email", sess.Email, "role", sess.Role, "target", in.Target)
		writeErr(w, http.StatusForbidden,
			"your role can review sessions but not open them")
		return
	}

	// Assignment decides who may reach a host at all.
	//
	// The principal list on an asset says which accounts Argus can broker
	// there; it never said *for whom*, so until assignments existed every
	// signed-in account could open a session on every host in the inventory by
	// naming it. Admins and owners are exempt for the same reason they are
	// exempt from the grant check below.
	if !a.assignmentPermits(w, r, sess.Role, sess.Email, in.Target, in.Principal) {
		return
	}

	// Elevated principals require a grant, so root cannot be reached by typing
	// it into a form field, and the kernel-evidence policy applies to them.
	// Decided by authorizePrincipal, which audits the decision either way:
	// "who held root on this host, and under which approval" is the question
	// an audit opens with. Admins are exempt from the grant, because someone
	// has to be able to act when the approval chain itself is broken, and the
	// exemption is written down rather than left implicit.
	if isElevated(in.Principal) {
		auth, err := a.authorizePrincipal(r, sess.Email, in.Target, in.Principal)
		if err != nil {
			a.log.Error("refusing a session: elevation could not be checked",
				"email", sess.Email, "target", in.Target, "principal", in.Principal, "error", err)
			writeErr(w, http.StatusServiceUnavailable,
				"the control plane could not check your access; nothing was opened")
			return
		}
		if !auth.Allowed {
			writeErr(w, http.StatusForbidden, auth.Reason)
			return
		}
	}

	ticket, err := a.signer.IssueTicket(sess.Email, in.Target, in.Principal, a.ticketTTL())
	if err != nil {
		a.fail(w, "issue ticket", err)
		return
	}

	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "terminal.authorized",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     in.Target,
		Detail:     "Browser terminal authorised as " + in.Principal + ".",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":    ticket,
		"expiresIn": int(a.ticketTTL().Seconds()),
	})
}

/* ── Helpers ─────────────────────────────────────────────────────────────── */

// isElevated lists principals that never come for free.
// ElevatedPrincipals is the one definition of "needs an approval first".
func ElevatedPrincipals() []string {
	return []string{"root", "admin", "administrator"}
}

func isElevated(principal string) bool {
	for _, e := range ElevatedPrincipals() {
		if strings.EqualFold(e, principal) {
			return true
		}
	}
	return false
}

func (a *API) sessionTTL() time.Duration {
	if a.SessionTTL > 0 {
		return a.SessionTTL
	}
	return 8 * time.Hour
}

// ticketTTL is short by design: long enough to open a WebSocket, short enough
// that a ticket leaked through a URL is worthless by the time anyone finds it.
func (a *API) ticketTTL() time.Duration {
	if a.TicketTTL > 0 {
		return a.TicketTTL
	}
	return 60 * time.Second
}

// ErrNoAuth is returned when a request carries no usable credential.
var ErrNoAuth = errors.New("not authenticated")

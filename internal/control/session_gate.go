package control

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

/*
Everything a console cookie has to pass besides its signature.

A signature proves the control plane issued the cookie. It cannot say whether
the account behind it has since been disabled, demoted, had its password
changed, or signed out -- so until this existed every one of those left the
cookie working, with the role it was issued with, for up to eight hours. A
stolen cookie could not be killed at all short of rotating the signing secret,
which signs everyone out and re-keys every agent.

So a cookie is resolved against the account on every request (see
Store.SessionState), once per request, and the answer is kept on the request's
context so the handlers' own calls to authenticate do not repeat the query.
*/

// sessionCtxKey carries the resolved console session on a request.
const sessionCtxKey ctxKey = "argus.session"

// resolvedSession is a request's cookie, after the database has had its say.
type resolvedSession struct {
	sess auth.Session
	ok   bool
	// mfaSetupRequired is set when the account's role must have a second
	// factor and has none, so only enrolment is open to it.
	mfaSetupRequired bool
}

// MFAPolicy says which accounts must enrol a second factor before they may do
// anything else in the console.
type MFAPolicy string

const (
	// MFAPrivileged requires it of owners, admins and approvers: the roles
	// that change who can reach what, or approve it. The default.
	MFAPrivileged MFAPolicy = "privileged"
	// MFAAll requires it of everyone.
	MFAAll MFAPolicy = "all"
	// MFAOff requires it of no one. Enrolment is still offered.
	MFAOff MFAPolicy = "off"
)

// ValidMFAPolicy reports whether p is a policy this build knows.
func ValidMFAPolicy(p string) bool {
	switch MFAPolicy(p) {
	case "", MFAPrivileged, MFAAll, MFAOff:
		return true
	}
	return false
}

func (a *API) mfaRequiredFor(role string) bool {
	switch a.RequireMFA {
	case MFAOff:
		return false
	case MFAAll:
		return true
	default:
		return role == "owner" || role == "admin" || role == "approver"
	}
}

// resolveSession verifies the cookie and checks it against the account.
//
// Fails closed: a database that cannot be asked is a cookie that cannot be
// checked, and an unchecked cookie is how a disabled account keeps working.
func (a *API) resolveSession(r *http.Request) resolvedSession {
	if a.signer == nil {
		return resolvedSession{}
	}
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return resolvedSession{}
	}
	// The key of the organization the cookie claims; scope resolved it.
	sess, err := a.signerFor(r).VerifySession(c.Value)
	if err != nil {
		return resolvedSession{}
	}
	st, err := a.store.SessionState(r.Context(), sess.Email, sess.ID)
	if err != nil {
		if err != ErrNoAccount {
			a.log.Error("cannot check a console session; refusing it", "error", err)
		}
		return resolvedSession{}
	}
	if st.Disabled || st.Revoked {
		return resolvedSession{}
	}
	if st.ValidAfter != nil && sess.IssuedAt.Before(*st.ValidAfter) {
		return resolvedSession{}
	}
	// The role is the account's now, not the one the cookie was issued with:
	// a demotion applies to the next request, as the People page says.
	sess.Email, sess.Role = st.Email, st.Role
	if st.DisplayName != "" {
		sess.Name = st.DisplayName
	}
	out := resolvedSession{sess: sess, ok: true}
	out.mfaSetupRequired = a.mfaRequiredFor(st.Role) && !st.MFAEnrolled
	return out
}

// sessionOf returns the request's resolved session, resolving it if the gate
// did not run (a handler called directly in a test).
func (a *API) sessionOf(r *http.Request) resolvedSession {
	if v, ok := r.Context().Value(sessionCtxKey).(*resolvedSession); ok && v != nil {
		return *v
	}
	return a.resolveSession(r)
}

// mfaSetupPaths are what an account that owes a second factor may still reach:
// enough to see who it is, enrol, change its password, and leave.
var mfaSetupPaths = map[string]bool{
	"/auth/me":              true,
	"/auth/logout":          true,
	"/auth/mfa/enrol":       true,
	"/auth/mfa/confirm":     true,
	"/auth/password/change": true,
	"/auth/sessions/revoke": true,
}

// sessionGate resolves the cookie once per request, refuses cross-site
// writes, and holds an account that owes a second factor to enrolment.
func (a *API) sessionGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.sameOriginWrite(r) {
			a.log.Warn("refused a cross-site request",
				"path", r.URL.Path, "origin", r.Header.Get("Origin"),
				"fetch_site", r.Header.Get("Sec-Fetch-Site"))
			writeErr(w, http.StatusForbidden,
				"this request came from another site; open the console directly and try again")
			return
		}
		// A console request body is at most 1 MiB of JSON, so thirty seconds
		// is generous; without a deadline a client can trickle one byte a
		// minute and hold the connection and its goroutine for ever.
		// Agents' uploads (/api/v1/report/) are large and paced by the
		// network, and set no deadline here.
		if r.ContentLength != 0 && r.Method != http.MethodGet &&
			!strings.HasPrefix(r.URL.Path, "/api/v1/report/") {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
		}
		rs := a.resolveSession(r)
		if rs.ok && rs.mfaSetupRequired && !mfaSetupPaths[r.URL.Path] &&
			(strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/")) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "your role must have a second factor before it can do anything else; " +
					"set one up from the prompt in the console",
				"mfaSetupRequired": true,
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey, &rs)))
	})
}

/* ── Cross-site request forgery ──────────────────────────────────────────── */

// sameOriginWrite reports whether a state-changing request came from the
// console itself.
//
// The cookie is SameSite=Lax, which keeps it off a cross-site POST, but not off
// a request from a *same-site* origin: a sibling subdomain, or anything an
// attacker can host under the same registrable domain. Browsers say where a
// request came from in Origin and Sec-Fetch-Site, which a page cannot forge,
// so a write is accepted only when those name this host or a configured
// console origin. A request with neither header is not from a modern browser
// (an agent, curl, a test) and carries no ambient cookie to abuse.
func (a *API) sameOriginWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// Except a WebSocket handshake, which is a GET that opens a channel.
		// Each one also demands a single-use ticket and checks Origin itself;
		// this is the same check made before any work is done.
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			return true
		}
	}
	// The browser's own statement that the page making this request is this
	// origin. Checked first because a proxy in front (a Cloudflare Tunnel, a
	// load balancer) may rewrite Host, so comparing Origin with Host alone
	// would refuse the console's own writes behind one.
	if r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			return origin == ""
		default:
			return false
		}
	}
	for _, o := range a.AllowedOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if a.consoleURL != "" {
		if c, err := url.Parse(a.consoleURL); err == nil && strings.EqualFold(c.Host, u.Host) {
			return true
		}
	}
	return false
}

/* ── Known devices ───────────────────────────────────────────────────────── */

// deviceTTL is how long a browser stays known after a sign-in.
const deviceTTL = 90 * 24 * time.Hour

// knownDevice reports whether this browser has signed in as email before.
func (a *API) knownDevice(r *http.Request, email string) bool {
	c, err := r.Cookie(auth.DeviceCookieName)
	if err != nil {
		return false
	}
	return a.signerFor(r).DeviceKnownFor(c.Value, email)
}

func (a *API) rememberDevice(w http.ResponseWriter, r *http.Request, email string) {
	tok, err := a.signerFor(r).IssueDeviceToken(email, deviceTTL)
	if err != nil {
		a.log.Warn("cannot issue a device cookie", "error", err)
		return
	}
	auth.SetCookie(w, auth.DeviceCookieName, tok, deviceTTL, a.secureCookies)
}

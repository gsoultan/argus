package auth

import (
	"net/http"
	"time"
)

// SessionCookieName is the console's session.
const SessionCookieName = "argus_session"

// SetCookie writes a hardened cookie.
func SetCookie(w http.ResponseWriter, name, value string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:  name,
		Value: value,
		Path:  "/",
		// HttpOnly: the console never needs to read this from JavaScript, and
		// keeping it out of reach means an XSS bug does not become a session
		// theft.
		HttpOnly: true,
		Secure:   secure,
		// Lax rather than Strict: Strict would drop the cookie on a top-level
		// navigation into the console, so a bookmarked deep link would land on
		// the sign-in form despite a valid session.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearCookie expires a cookie.
func ClearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

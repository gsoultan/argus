package control

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/notify"
)

/*
A cookie is bound to the browser it was issued to.

Revocation (session_gate.go) kills a stolen cookie once somebody knows it was
stolen. This catches the theft itself, in the common case: a cookie lifted by
malware, a malicious extension or a proxy log and replayed from the attacker's
own tooling. Each cookie names the browser and operating system that signed in
("Firefox on macOS", no version, so an update is not a change), and a request
carrying it from anything else is refused, the cookie revoked, and the account
holder's administrators told.

Bound to the browser and not the address: people move between Wi-Fi and
mobile data all day, and a cookie that died with every new address would sign
phones out every few minutes. A thief who copies the User-Agent too gets past
this, which is why it sits beside revocation, sign-in alerts and the second
factor rather than replacing any of them.

Cost per request: one User-Agent parse, which is a few substring searches. No
database round trip; the binding is inside the signed cookie.
*/

// browserFamily is the binding value for a request.
func browserFamily(r *http.Request) string {
	_, family := notify.DescribeUserAgent(r.UserAgent())
	return family
}

// browserMatches reports whether the cookie may be used from this request's
// browser, and deals with one that may not.
func (a *API) browserMatches(r *http.Request, sess auth.Session) bool {
	if a.AllowBrowserChange || sess.Browser == "" {
		return true
	}
	got := browserFamily(r)
	if got == sess.Browser {
		return true
	}
	// Once per cookie: every request a replayed cookie makes would
	// otherwise be its own audit entry and alert.
	if !a.browserMismatch.first(sess.ID, time.Now()) {
		return false
	}
	ctx := context.WithoutCancel(r.Context())
	if err := a.store.RevokeSession(ctx, sess.ID, sess.ExpiresAt); err != nil {
		a.log.Error("could not revoke a cookie used from another browser", "error", err)
	}
	ip := a.clientAddr(r)
	detail := "A session cookie issued to " + sess.Browser + " was presented by " + got +
		" from " + orUnknownValue(ip) + ". Argus refused it and signed that session out. " +
		"Someone may have copied the cookie out of the browser; if the account holder did not " +
		"just switch browsers, treat the account as compromised."
	a.log.Warn("refused a session cookie from a different browser",
		"email", sess.Email, "issued_to", sess.Browser, "presented_by", got, "client", ip)
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: "auth.session_hijack_refused", Severity: "critical", ActorEmail: sess.Email,
		Target: "console", Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	a.alertSecurity(ctx, "", "A copied session cookie of "+sess.Email+" was refused", detail)
	a.Alerts.SecurityEvent("Argus: a copied session cookie of "+sess.Email+" was refused", detail)
	return false
}

// reportedOnce remembers keys for a while, so an event is acted on once per
// key per window. Bounded: keys are cookie ids and host names, which a caller
// can repeat but only mint by signing in or enrolling.
type reportedOnce struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const (
	reportedOnceWindow = time.Hour
	reportedOnceKeys   = 10000
)

// first reports whether key was not seen in the last window, and records it.
func (o *reportedOnce) first(key string, now time.Time) bool {
	return o.firstWithin(key, now, reportedOnceWindow)
}

func (o *reportedOnce) firstWithin(key string, now time.Time, window time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen = map[string]time.Time{}
	}
	if t, ok := o.seen[key]; ok && now.Sub(t) < window {
		return false
	}
	if len(o.seen) >= reportedOnceKeys {
		for k, t := range o.seen {
			if now.Sub(t) >= reportedOnceWindow {
				delete(o.seen, k)
			}
		}
		if len(o.seen) >= reportedOnceKeys {
			o.seen = map[string]time.Time{}
		}
	}
	o.seen[key] = now
	return true
}

package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/notify"
)

// Sign-in alerts: when someone signs in to the console, the organization's
// administrators are told by email (and, for the default organization, on the
// Telegram chats), and the person who signed in is told by email with a link
// to say it was not them.
//
// Only through channels that are set up: with no email and no Telegram in
// Settings, SignInAlerts is nil and nothing here does anything but record the
// device. Only successful sign-ins are announced, so guessing at passwords
// cannot make Argus send mail; and each account's alerts are bounded, so
// neither can signing in over and over.

// SignInAlertPolicy is what the operator chose in Settings › Alerts.
type SignInAlertPolicy struct {
	// Admins tells the organization's administrators.
	Admins bool
	// Person emails the person who signed in.
	Person bool
	// Every announces every sign-in. Off, only a new device or address, or a
	// recovery code, is announced.
	Every bool
}

// DefaultSignInAlertPolicy is on for both audiences, new devices only.
var DefaultSignInAlertPolicy = SignInAlertPolicy{Admins: true, Person: true}

const (
	// signInDeviceMemory is how long a device stays known without signing in.
	signInDeviceMemory = 90 * 24 * time.Hour
	// signInReportTTL is how long the "this wasn't me" link works.
	signInReportTTL = 24 * time.Hour

	// An account's routine alerts come at most once per signInAlertQuiet, and
	// no account produces more than signInAlertsPerHour alerts of any kind.
	signInAlertQuiet    = 10 * time.Minute
	signInAlertsPerHour = 5
	// signInAlertKeys bounds the limiter's map.
	signInAlertKeys = 10000
)

// signInMethod maps completeSignIn's "how" to what an alert says.
func signInMethod(how string) string {
	switch how {
	case "password+recovery":
		return notify.MethodRecoveryCode
	case "password+mfa":
		return notify.MethodAuthenticator
	default:
		return notify.MethodPasswordOnly
	}
}

// announceSignIn records the device and queues whatever alerts are owed. It
// never fails the sign-in: by the time it runs the session is issued.
func (a *API) announceSignIn(r *http.Request, sess auth.Session, how string) {
	ctx := r.Context()
	ip := a.clientAddr(r)
	device, family := notify.DescribeUserAgent(r.UserAgent())
	isNew, err := a.store.RememberSignInDevice(ctx, sess.Email, signInFingerprint(ip, family))
	if err != nil {
		// Unknown is announced as new: an extra alert is the safe mistake.
		a.log.Error("could not record the sign-in device", "email", sess.Email, "error", err)
		isNew = true
	}

	method := signInMethod(how)
	// The bell is told of every sign-in; each administrator chooses whether
	// to see only new devices. Not rate limited like mail: a row costs
	// nothing, and the sign-in itself had to succeed.
	a.alertSignIn(ctx, notify.SignIn{
		Email: sess.Email, Name: sess.Name, Role: sess.Role,
		ClientIP: ip, Device: device, Method: method, NewDevice: isNew,
	})

	n := a.SignInAlerts
	p := a.SignInPolicy
	if n == nil || (!p.Admins && !p.Person) {
		return
	}
	important := isNew || method == notify.MethodRecoveryCode
	if !important && !p.Every {
		return
	}
	key := tenantIDOf(ctx) + "\x00" + strings.ToLower(sess.Email)
	if !a.signInLimiter.allow(key, important, time.Now()) {
		a.log.Info("sign-in alert held back; this account was announced recently", "email", sess.Email)
		return
	}

	e := notify.SignIn{
		Email: sess.Email, Name: sess.Name, Role: sess.Role, Org: orgLabel(ctx),
		ClientIP: ip, Device: device, Method: method, NewDevice: isNew,
		At: time.Now().UTC(),
	}
	personTold := p.Person && n.EmailOn()
	if personTold {
		n.Person(e, a.signInReportURL(r, e))
	}
	if p.Admins {
		admins, err := a.store.AdministratorEmails(ctx)
		if err != nil {
			a.log.Error("could not list administrators for a sign-in alert", "error", err)
		}
		if personTold {
			// An administrator signing in is told once, as the person.
			admins = withoutFold(admins, sess.Email)
		}
		n.Admins(e, admins, tenantIDOf(ctx) == DefaultTenantID)
	}
}

// signInAlertsTellAdmins reports whether administrators hear about sign-ins
// here, so a recovery-code sign-in is not also sent as a security alert.
func (a *API) signInAlertsTellAdmins() bool {
	return a.SignInAlerts != nil && a.SignInPolicy.Admins
}

// signInReportURL is the "this wasn't me" link, or empty when there is no
// console address to put in it or nothing to sign it with.
func (a *API) signInReportURL(r *http.Request, e notify.SignIn) string {
	base := a.SignInAlerts.ConsoleURL()
	if base == "" || a.signer == nil {
		return ""
	}
	tok, err := a.signerFor(r).IssueSignInReport(auth.SignInReport{
		Account: e.Email, At: e.At, ClientIP: e.ClientIP, Device: e.Device, Method: e.Method,
	}, signInReportTTL)
	if err != nil {
		a.log.Error("could not sign a sign-in report link", "error", err)
		return ""
	}
	return strings.TrimRight(base, "/") + "/auth/sign-in-report?ticket=" + url.QueryEscape(tok)
}

// orgLabel names the organization in an alert, or nothing for the default
// one: an install with one organization has no use for its name.
func orgLabel(ctx context.Context) string {
	if t := TenantOf(ctx); t != nil && !t.IsDefault() {
		return t.Name
	}
	return ""
}

// signInFingerprint identifies a browser at an address. An IPv6 address is
// taken by its /64, which is what one household or office is usually given
// and within which a device picks new addresses for privacy.
func signInFingerprint(ip, family string) string {
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		ip = (&net.IPNet{IP: p.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
	}
	h := sha256.Sum256([]byte(ip + "|" + family))
	return hex.EncodeToString(h[:16])
}

func withoutFold(list []string, drop string) []string {
	out := list[:0:0]
	for _, s := range list {
		if !strings.EqualFold(s, drop) {
			out = append(out, s)
		}
	}
	return out
}

/* ── Store ──────────────────────────────────────────────────────────────── */

// RememberSignInDevice records that fingerprint signed in to email now, and
// reports whether it had not in the last 90 days. The account's devices not
// seen for that long are forgotten first, which is what keeps the table
// bounded.
func (s *Store) RememberSignInDevice(ctx context.Context, email, fingerprint string) (isNew bool, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	db := s.db(ctx)
	if _, err := db.Exec(ctx,
		`DELETE FROM sign_in_devices WHERE email = $1 AND last_seen_at < now() - $2::interval`,
		email, signInDeviceMemory.String()); err != nil {
		return false, err
	}
	// xmax is zero on a row this statement inserted, and set on one it
	// updated: one round trip, and no window between a read and a write.
	err = db.QueryRow(ctx, `
		INSERT INTO sign_in_devices (email, fingerprint) VALUES ($1, $2)
		ON CONFLICT (email, fingerprint) DO UPDATE SET last_seen_at = now()
		RETURNING xmax = 0`, email, fingerprint).Scan(&isNew)
	return isNew, err
}

// AdministratorEmails is every enabled owner and administrator of the
// organization on ctx.
func (s *Store) AdministratorEmails(ctx context.Context) ([]string, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT email FROM users
		 WHERE role IN ('owner','admin') AND disabled_at IS NULL
		 ORDER BY email LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

/* ── Limiter ────────────────────────────────────────────────────────────── */

// signInAlertLimiter bounds how often one account is announced. Keyed by
// accounts that signed in successfully, which a caller cannot invent; bounded
// all the same.
type signInAlertLimiter struct {
	mu   sync.Mutex
	sent map[string][]time.Time
}

func (l *signInAlertLimiter) allow(key string, important bool, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sent == nil {
		l.sent = map[string][]time.Time{}
	}
	if len(l.sent) >= signInAlertKeys {
		for k, ts := range l.sent {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > time.Hour {
				delete(l.sent, k)
			}
		}
		if len(l.sent) >= signInAlertKeys {
			// Forgetting sends a few extra alerts; refusing would let one
			// busy hour silence every account after it.
			l.sent = map[string][]time.Time{}
		}
	}
	recent := l.sent[key][:0:0]
	for _, t := range l.sent[key] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	ok := len(recent) < signInAlertsPerHour
	if ok && !important && len(recent) > 0 && now.Sub(recent[len(recent)-1]) < signInAlertQuiet {
		ok = false
	}
	if ok {
		recent = append(recent, now)
	}
	if len(recent) == 0 {
		delete(l.sent, key)
	} else {
		l.sent[key] = recent
	}
	return ok
}

/* ── "This wasn't me" ───────────────────────────────────────────────────── */

// handleSignInReport serves the page the link in a sign-in email opens.
//
// GET only shows the sign-in and a button; the report is a POST from that
// page. Mail scanners and link previews fetch every link in a message, and a
// GET that acted would report -- and sign out -- every account whose mail is
// scanned.
func (a *API) handleSignInReport(w http.ResponseWriter, r *http.Request) {
	var tok string
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			a.reportPage(w, http.StatusBadRequest, reportView{Title: "That link is not valid",
				Body: "Open the link from the email again."})
			return
		}
		tok = r.PostForm.Get("ticket")
	} else {
		tok = r.URL.Query().Get("ticket")
	}
	ctx, signer, err := a.reportScope(r.Context(), tok)
	if err != nil || signer == nil {
		a.reportPage(w, http.StatusBadRequest, reportView{Title: "That link is not valid",
			Body: "It may have been cut short. Open the link from the email again, or tell your Argus administrator."})
		return
	}

	if r.Method != http.MethodPost {
		rep, err := signer.PeekSignInReport(tok)
		if err != nil {
			a.reportInvalid(w, err)
			return
		}
		a.reportPage(w, http.StatusOK, reportView{
			Title: "Wasn't you?",
			Body: "Report this sign-in to " + rep.Account + ". Argus signs out every session on the account, " +
				"this one included, and tells your administrators at once. Then change your password.",
			Facts:  reportFacts(rep),
			Ticket: tok, Confirm: true,
		})
		return
	}

	rep, err := signer.RedeemSignInReport(ctx, tok)
	switch {
	case errors.Is(err, auth.ErrUsed):
		a.reportPage(w, http.StatusOK, reportView{Title: "Already reported",
			Body: "This sign-in was reported already, and your administrators were told."})
		return
	case err != nil:
		a.reportInvalid(w, err)
		return
	}

	// Every session on the account ends, the reported one included. Safe to
	// hand whoever holds the link: it can only sign people out.
	signedOut := true
	if err := a.store.RevokeAllSessions(ctx, rep.Account); err != nil {
		signedOut = false
		a.log.Error("could not sign out a reported account", "email", rep.Account, "error", err)
	}
	detail := "The account holder reported that a sign-in at " +
		rep.At.UTC().Format(time.RFC3339) + " from " + orUnknownValue(rep.ClientIP) + " (" + orUnknownValue(rep.Device) +
		") was not them, using the link in their sign-in email. "
	if signedOut {
		detail += "Every session on the account was signed out. "
	} else {
		detail += "Signing the account out failed; its sessions may still be open. "
	}
	detail += "Treat the account as compromised until checked."
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: "auth.sign_in_reported", Severity: "warning", ActorEmail: rep.Account,
		Target: "console", Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	a.alertSecurity(ctx, "", rep.Account+" says a sign-in was not them", detail)
	a.log.Warn("a sign-in was reported as not the account holder",
		"email", rep.Account, "client", rep.ClientIP, "reporter", a.clientAddr(r), "signed_out", signedOut)

	if n := a.SignInAlerts; n != nil {
		admins, err := a.store.AdministratorEmails(ctx)
		if err != nil {
			a.log.Error("could not list administrators for a reported sign-in", "error", err)
		}
		n.Reported(notify.SignInReport{
			SignIn: notify.SignIn{
				Email: rep.Account, Org: orgLabel(ctx), ClientIP: rep.ClientIP,
				Device: rep.Device, Method: rep.Method, At: rep.At,
			},
			ReportedAt: time.Now().UTC(),
			SignedOut:  signedOut,
		}, withoutFold(admins, rep.Account), tenantIDOf(ctx) == DefaultTenantID)
	}

	body := "Argus recorded your report"
	if a.SignInAlerts != nil {
		body += " and told your administrators"
	}
	body += ". "
	if signedOut {
		body += "Every session on your account is signed out, including the one you reported. "
	}
	body += "Sign in again and change your password now, and if you think your phone is lost, ask an administrator to reset your authenticator."
	a.reportPage(w, http.StatusOK, reportView{Title: "Reported", Body: body, Done: true})
}

// reportScope is the organization the link was minted in, with its signer.
// The link says which, the way a ticket does; a browser signed in to another
// organization must not make it unreadable.
func (a *API) reportScope(ctx context.Context, tok string) (context.Context, *auth.Signer, error) {
	if tok == "" || a.signer == nil {
		return ctx, nil, auth.ErrInvalid
	}
	id := auth.PeekTenant(tok)
	if id == "" || id == DefaultTenantID {
		return WithTenant(ctx, defaultTenant()), a.signer, nil
	}
	if !validTenantID(id) || a.store == nil {
		return ctx, nil, auth.ErrInvalid
	}
	t, err := a.store.TenantByID(ctx, id)
	if err != nil || t.Disabled() {
		return ctx, nil, auth.ErrInvalid
	}
	return WithTenant(ctx, t), a.signer.ForTenant(id), nil
}

func (a *API) reportInvalid(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrExpired) {
		a.reportPage(w, http.StatusGone, reportView{Title: "This link has expired",
			Body: "Links in sign-in emails work for 24 hours. If a sign-in wasn't you, tell your Argus administrator now, and change your password."})
		return
	}
	a.reportPage(w, http.StatusBadRequest, reportView{Title: "That link is not valid",
		Body: "Open the link from the email again, or tell your Argus administrator."})
}

func reportFacts(r auth.SignInReport) [][2]string {
	f := [][2]string{{"Account", r.Account}, {"When", r.At.UTC().Format("Mon 2 Jan 2006, 15:04 MST")}}
	if r.ClientIP != "" {
		f = append(f, [2]string{"From", r.ClientIP})
	}
	if r.Device != "" {
		f = append(f, [2]string{"Device", r.Device})
	}
	return f
}

func orUnknownValue(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

type reportView struct {
	Title   string
	Body    string
	Facts   [][2]string
	Ticket  string
	Confirm bool
	Done    bool
}

// reportTemplate is a page of its own rather than a console route: whoever
// opens it may not be signed in, and the console's sign-in would be in the
// way. No script; html/template escapes everything.
var reportTemplate = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer"><title>Argus · {{.Title}}</title>
<style>
body{margin:0;padding:40px 16px;background:#f6f7f9;font:14px/1.55 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#101828}
main{max-width:520px;margin:0 auto;background:#fff;border:1px solid #e4e7ec;border-radius:10px;padding:28px}
h1{font-size:20px;margin:0 0 8px}p{color:#344054;margin:0 0 14px}
dl{border:1px solid #e4e7ec;border-radius:8px;margin:16px 0}dl div{display:flex;padding:8px 14px;border-top:1px solid #e4e7ec}
dl div:first-child{border-top:0}dt{width:90px;color:#667085;flex:none}dd{margin:0;font-weight:500;word-break:break-all}
button{background:#d92d20;color:#fff;border:0;border-radius:8px;padding:10px 18px;font-weight:600;font-size:14px;cursor:pointer}
.ok{color:#039855;font-weight:600}.logo{font-weight:700;margin-bottom:18px}
@media (prefers-color-scheme:dark){body{background:#0b1220;color:#e4e7ec}main{background:#111827;border-color:#1f2937}p{color:#cbd5e1}dl,dl div{border-color:#1f2937}}
</style></head><body><main>
<div class="logo">Argus</div>
<h1>{{if .Done}}<span class="ok">✓</span> {{end}}{{.Title}}</h1>
<p>{{.Body}}</p>
{{if .Facts}}<dl>{{range .Facts}}<div><dt>{{index . 0}}</dt><dd>{{index . 1}}</dd></div>{{end}}</dl>{{end}}
{{if .Confirm}}<form method="post" action="/auth/sign-in-report"><input type="hidden" name="ticket" value="{{.Ticket}}">
<button type="submit">This wasn't me, report it</button></form>{{end}}
</main></body></html>
`))

func (a *API) reportPage(w http.ResponseWriter, status int, v reportView) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// The API's policy allows nothing at all; this page needs its own styles
	// and one form that posts back here. Still no script.
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(status)
	if err := reportTemplate.Execute(w, v); err != nil {
		a.log.Error("render sign-in report page", "error", err)
	}
}

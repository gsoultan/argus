package notify

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"html/template"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Sign-in alerts tell people that somebody signed in to the console.
//
// Two audiences, told differently. Administrators of the organization hear
// about it by email at their own address and, for the default organization,
// on the configured Telegram chats as well. The person who signed in hears
// about it by email only, at the address on their account, with a link to say
// it was not them.
//
// A channel that is not set up is simply not used: there is no fallback, and
// nothing is queued for later. That is the operator's choice to make in
// Settings, not something to discover from a mail that never arrived.

// Second-step methods, as a sign-in alert names them.
const (
	MethodPasswordOnly  = "password only"
	MethodAuthenticator = "authenticator app"
	MethodRecoveryCode  = "recovery code"
)

// SignIn is what an alert says about one sign-in.
type SignIn struct {
	Email string
	Name  string
	Role  string
	// Org is the organization's display name.
	Org string
	// ClientIP is the address the request came from, as the rate limiter
	// resolved it: forwarding headers count only from trusted proxies.
	ClientIP string
	// Location is a place for ClientIP, or empty when none is known.
	Location string
	// Device is a browser and operating system read from the User-Agent,
	// which the browser sets: a description, not evidence.
	Device string
	// Method is the second step: MethodAuthenticator, MethodRecoveryCode or
	// MethodPasswordOnly.
	Method string
	// NewDevice is set when this browser and address have not signed in to
	// the account recently.
	NewDevice bool
	At        time.Time
}

// SignInReport is the account holder saying a sign-in was not them.
type SignInReport struct {
	SignIn
	ReportedAt time.Time
	// SignedOut is whether the report ended every session on the account.
	// False on a control plane that cannot end sessions before they expire.
	SignedOut bool
}

// Rendered is one alert in every form a channel needs.
type Rendered struct {
	Subject string
	Text    string
	HTML    string
	// Telegram is Telegram's HTML subset.
	Telegram  string
	LinkLabel string
	Link      string
}

func (e SignIn) who() string {
	if e.Name != "" && !strings.EqualFold(e.Name, e.Email) {
		return e.Name
	}
	return e.Email
}

func (e SignIn) firstName() string {
	if e.Name == "" || strings.EqualFold(e.Name, e.Email) {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(e.Name), " ")
	return first
}

func (e SignIn) when() string { return e.At.UTC().Format("Mon 2 Jan 2006, 15:04 MST") }

func (e SignIn) from() string {
	if e.Location == "" {
		return e.ClientIP
	}
	return e.ClientIP + " (" + e.Location + ", approximate)"
}

func (e SignIn) method() string {
	switch e.Method {
	case MethodAuthenticator:
		return "Authenticator app"
	case MethodRecoveryCode:
		return "Recovery code (single use)"
	default:
		return "None, password only"
	}
}

type fact struct{ K, V string }

func (e SignIn) facts(withPerson bool) []fact {
	var f []fact
	if withPerson {
		p := e.Email
		if e.who() != e.Email {
			p = e.who() + " · " + e.Email
		}
		if e.Role != "" {
			p += " (" + e.Role + ")"
		}
		f = append(f, fact{"Person", p})
	}
	f = append(f, fact{"When", e.when()})
	if e.ClientIP != "" {
		f = append(f, fact{"From", e.from()})
	}
	if e.Device != "" {
		f = append(f, fact{"Device", e.Device})
	}
	f = append(f, fact{"Second step", e.method()})
	if withPerson && e.Org != "" {
		f = append(f, fact{"Organization", e.Org})
	}
	return f
}

func consoleLink(consoleURL, path string) string {
	if consoleURL == "" {
		return ""
	}
	return strings.TrimRight(consoleURL, "/") + path
}

func textFacts(b *strings.Builder, facts []fact) {
	for _, f := range facts {
		fmt.Fprintf(b, "%-13s %s\n", f.K+":", f.V)
	}
}

// SignInAdminMessage is the alert administrators get.
func SignInAdminMessage(e SignIn, consoleURL string) Rendered {
	var tag string
	switch {
	case e.Method == MethodRecoveryCode:
		tag = "Recovery code used"
	case e.NewDevice:
		tag = "New device"
	}
	subject := "Argus: " + e.who() + " signed in"
	switch {
	case e.Method == MethodRecoveryCode:
		subject += " with a recovery code"
	case e.NewDevice:
		subject += " from a new device"
	}
	lead := e.Email
	if e.Role != "" || e.Org != "" {
		lead += " (" + strings.Trim(e.Role+", "+e.Org, ", ") + ")"
	}
	switch {
	case e.Method == MethodRecoveryCode:
		lead += " signed in with a single-use recovery code, which means their usual second step was not available. " +
			"Either they lost their device, or somebody else has their password and a code."
	case e.NewDevice:
		lead += " signed in from a browser or address Argus hasn't seen for this account recently."
	default:
		lead += " signed in to Argus."
	}
	link := consoleLink(consoleURL, "/audit")

	var t strings.Builder
	t.WriteString(subject[len("Argus: "):] + ".\n\n")
	t.WriteString(lead + "\n\n")
	textFacts(&t, e.facts(true))
	if link != "" {
		fmt.Fprintf(&t, "\nReview it in Argus: %s\n", link)
	}
	t.WriteString("\nYou get this because you're an administrator of " + orgOrArgus(e.Org) +
		" and sign-in alerts are on. Change them in Settings, Alerts.\n")

	return Rendered{
		Subject: subject,
		Text:    t.String(),
		HTML: renderMail(mailData{
			Tag: tag, TagRose: e.Method == MethodRecoveryCode, Title: subject[len("Argus: "):],
			Lead: lead, Facts: e.facts(true), Button: "Review in Argus", Link: link,
			Footer: "You get this because you're an administrator of " + orgOrArgus(e.Org) +
				" and sign-in alerts are on. Change who is told, and when, in Settings › Alerts.",
			Host: hostOf(consoleURL),
		}),
		Telegram:  telegramMessage("🔐", subject[len("Argus: "):], tag, "", e.facts(true)),
		LinkLabel: "Review in Argus",
		Link:      link,
	}
}

// SignInPersonMessage is the email the person who signed in gets. reportURL is
// the single-use "this wasn't me" link; empty leaves the button out.
func SignInPersonMessage(e SignIn, consoleURL, reportURL string) Rendered {
	subject := "New sign-in to your Argus account"
	greet := "Your account"
	if n := e.firstName(); n != "" {
		greet = "Hi " + n + ", your account"
	}
	lead := greet + " " + e.Email + " was just used to sign in to Argus."
	notMe := "Wasn't you? Use the button below. Argus signs out every session on your account, " +
		"including this one, and tells your administrators at once. " +
		"Then change your password and, if you think your phone is lost, ask an administrator to reset your authenticator."
	if reportURL == "" {
		notMe = "Wasn't you? Tell your Argus administrator now, and change your password."
	}

	var t strings.Builder
	t.WriteString(lead + "\n\n")
	textFacts(&t, e.facts(false))
	t.WriteString("\nWas this you? Then you don't need to do anything.\n\n")
	t.WriteString(notMe + "\n")
	if reportURL != "" {
		fmt.Fprintf(&t, "\nThis wasn't me: %s\n(The link works once, for 24 hours. It can only sign you out; it can't sign anyone in.)\n", reportURL)
	}
	t.WriteString("\nArgus sends this to the person who signed in. Your administrator decides whether these are sent.\n")

	return Rendered{
		Subject: subject,
		Text:    t.String(),
		HTML: renderMail(mailData{
			Title: subject, Lead: lead, Facts: e.facts(false),
			After: "Was this you? Then you don't need to do anything.", Warn: notMe,
			Button: "This wasn't me", Link: reportURL, Danger: true,
			Note:   noteIf(reportURL != "", "The link works once and for 24 hours. It can only sign you out; it can't sign anyone in."),
			Footer: "Argus sends this to the person who signed in. Your administrator decides whether these are sent.",
			Host:   hostOf(consoleURL),
		}),
	}
}

// SignInReportedMessage is the alert administrators get when the account
// holder says a sign-in was not them. It is not subject to the sign-in alert
// switches: an account holder who reports a sign-in expects someone to hear.
func SignInReportedMessage(r SignInReport, consoleURL string) Rendered {
	e := r.SignIn
	subject := "Argus: " + e.who() + " reported a sign-in that wasn't them"
	var state string
	switch {
	case r.SignedOut:
		state = "Argus has already signed out every session on " + e.Email + ". The account is still enabled."
	default:
		state = "Sessions on " + e.Email + " are still signed in, and the account is still enabled."
	}
	lead := e.who() + " used the link in their sign-in email. " + state
	advice := "Before they sign in again: check what that session did, and consider disabling the account or resetting their authenticator."
	facts := append([]fact{{"Person", e.Email}, {"Sign-in", e.when()}}, e.facts(false)[1:]...)
	facts = append(facts, fact{"Reported", r.ReportedAt.UTC().Format("Mon 2 Jan 2006, 15:04 MST")})
	if e.Org != "" {
		facts = append(facts, fact{"Organization", e.Org})
	}
	link := consoleLink(consoleURL, "/audit")

	var t strings.Builder
	t.WriteString(lead + "\n\n")
	textFacts(&t, facts)
	t.WriteString("\n" + advice + "\n")
	if link != "" {
		fmt.Fprintf(&t, "\nSee what happened: %s\n", link)
	}
	return Rendered{
		Subject: subject,
		Text:    t.String(),
		HTML: renderMail(mailData{
			Tag: "Reported by the account owner", TagRose: true, Title: subject[len("Argus: "):],
			Lead: lead, Facts: facts, Warn: advice, WarnAmber: true,
			Button: "See what happened", Link: link,
			Footer: "You get this because you're an administrator of " + orgOrArgus(e.Org) +
				". This alert is sent whenever email or Telegram is set up.",
			Host: hostOf(consoleURL),
		}),
		Telegram:  telegramMessage("🚨", subject[len("Argus: "):], "", lead, facts) + "\n\n" + html.EscapeString(advice),
		LinkLabel: "See what happened",
		Link:      link,
	}
}

func noteIf(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}

func orgOrArgus(org string) string {
	if org == "" {
		return "Argus"
	}
	return org
}

func hostOf(consoleURL string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(consoleURL, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	return h
}

func escapeHTML(s string) string { return html.EscapeString(s) }

func telegramMessage(icon, title, tag, lead string, facts []fact) string {
	var b strings.Builder
	b.WriteString(icon + " <b>" + escapeHTML(title) + "</b>")
	if tag != "" {
		b.WriteString("\n<i>" + escapeHTML(tag) + "</i>")
	}
	if lead != "" {
		b.WriteString("\n\n" + escapeHTML(lead))
	}
	b.WriteString("\n")
	for _, f := range facts {
		v := escapeHTML(f.V)
		if f.K == "From" {
			v = "<code>" + v + "</code>"
		}
		b.WriteString("\n" + escapeHTML(f.K) + ": " + v)
	}
	return b.String()
}

/* ── HTML email ─────────────────────────────────────────────────────────── */

type mailData struct {
	Tag       string
	TagRose   bool
	Title     string
	Lead      string
	Facts     []fact
	After     string
	Warn      string
	WarnAmber bool
	Button    string
	Link      string
	Danger    bool
	Note      string
	Footer    string
	Host      string
	// Code is a value to copy, shown large in a monospace box: a
	// temporary password.
	CodeLabel string
	Code      string
	// Steps is a numbered list after the facts.
	Steps []string
}

// Inline styles and a table layout, because that is what mail clients render.
// html/template escapes every value: names, hostnames and addresses come from
// accounts and requests.
var mailTemplate = template.Must(template.New("mail").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"></head>
<body style="margin:0;padding:24px 12px;background:#f6f7f9;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#101828">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center">
<table role="presentation" width="600" cellpadding="0" cellspacing="0" style="max-width:600px;width:100%;background:#ffffff;border:1px solid #e4e7ec;border-radius:10px">
<tr><td style="padding:18px 28px;border-bottom:1px solid #e4e7ec;font-weight:700;font-size:15px">
<span style="display:inline-block;width:22px;height:22px;border-radius:6px;background:#2563eb;vertical-align:middle;margin-right:8px"></span>Argus
{{if .Host}}<span style="float:right;font-weight:400;font-size:12px;color:#667085;line-height:22px">{{.Host}}</span>{{end}}</td></tr>
<tr><td style="padding:24px 28px 8px">
{{if .Tag}}<span style="display:inline-block;font-size:11px;font-weight:600;padding:3px 8px;border-radius:6px;{{if .TagRose}}background:#fef3f2;color:#d92d20{{else}}background:#fffaeb;color:#b54708{{end}}">{{.Tag}}</span>{{end}}
<h1 style="font-size:20px;line-height:1.3;margin:10px 0 0">{{.Title}}</h1>
<p style="font-size:14px;line-height:1.55;color:#344054;margin:10px 0 0">{{.Lead}}</p>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin:18px 0 6px;border:1px solid #e4e7ec;border-radius:8px;border-collapse:separate">
{{range $i, $f := .Facts}}<tr><td style="padding:9px 14px;font-size:13.5px;color:#667085;width:120px;{{if $i}}border-top:1px solid #e4e7ec{{end}}">{{$f.K}}</td><td style="padding:9px 14px;font-size:13.5px;font-weight:500;{{if $i}}border-top:1px solid #e4e7ec{{end}}">{{$f.V}}</td></tr>
{{end}}</table>
{{if .Code}}<p style="font-size:12.5px;color:#667085;margin:14px 0 4px">{{.CodeLabel}}</p>
<div style="font-family:SFMono-Regular,Menlo,Consolas,monospace;font-size:18px;letter-spacing:1px;font-weight:600;background:#f2f4f7;border:1px solid #e4e7ec;border-radius:8px;padding:12px 14px;word-break:break-all">{{.Code}}</div>{{end}}
{{if .Steps}}<ol style="font-size:14px;line-height:1.6;color:#344054;margin:14px 0 0;padding-left:20px">{{range .Steps}}<li>{{.}}</li>{{end}}</ol>{{end}}
{{if .After}}<p style="font-size:14px;line-height:1.55;color:#344054;margin:12px 0 0">{{.After}}</p>{{end}}
{{if .Warn}}<div style="margin-top:14px;border-radius:8px;padding:11px 14px;font-size:13px;line-height:1.45;{{if .WarnAmber}}background:#fffaeb;color:#7a2e0e;border:1px solid #fedf89{{else}}background:#fef3f2;color:#7a271a;border:1px solid #fecdca{{end}}">{{.Warn}}</div>{{end}}
{{if .Link}}<p style="margin:20px 0 0"><a href="{{.Link}}" style="display:inline-block;background:{{if .Danger}}#d92d20{{else}}#2563eb{{end}};color:#ffffff;border-radius:8px;padding:10px 18px;font-weight:600;font-size:14px;text-decoration:none">{{.Button}}</a></p>{{end}}
{{if .Note}}<p style="font-size:12.5px;color:#667085;margin:12px 0 0">{{.Note}}</p>{{end}}
</td></tr>
<tr><td style="padding:16px 28px 22px;font-size:12px;color:#667085;line-height:1.5;border-top:1px solid #e4e7ec">{{.Footer}}<br>Sent by Argus{{if .Host}} at {{.Host}}{{end}}. Don't reply; nobody reads this mailbox.</td></tr>
</table></td></tr></table></body></html>
`))

func renderMail(d mailData) string {
	var b bytes.Buffer
	if err := mailTemplate.Execute(&b, d); err != nil {
		// The template is fixed and the data is strings; an error here is a
		// bug, and the plain-text part still carries the whole message.
		return ""
	}
	return b.String()
}

/* ── User-Agent ─────────────────────────────────────────────────────────── */

// DescribeUserAgent names a browser and operating system. label carries the
// browser's major version for people to read; family leaves it out, so a
// browser updating itself is not a new device.
func DescribeUserAgent(ua string) (label, family string) {
	browser, version := "", ""
	pick := func(name, token string) bool {
		i := strings.Index(ua, token)
		if i < 0 {
			return false
		}
		browser = name
		v := ua[i+len(token):]
		if j := strings.IndexAny(v, ". ;)"); j >= 0 {
			v = v[:j]
		}
		version = v
		return true
	}
	switch {
	case pick("Edge", "Edg/"), pick("Opera", "OPR/"), pick("Firefox", "Firefox/"),
		pick("Chrome", "CriOS/"), pick("Firefox", "FxiOS/"), pick("Chrome", "Chrome/"):
	case strings.Contains(ua, "Safari/") && pick("Safari", "Version/"):
	case pick("curl", "curl/"):
	}
	os := ""
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		os = "iOS"
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "CrOS"):
		os = "ChromeOS"
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		os = "macOS"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}
	if browser == "" {
		browser = "An unknown browser"
	}
	family = browser
	if os != "" {
		family += " on " + os
	}
	label = browser
	if version != "" && isDigits(version) {
		label += " " + version
	}
	if os != "" {
		label += " on " + os
	}
	return label, family
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

/* ── Delivery ───────────────────────────────────────────────────────────── */

// SignInNotifier queues sign-in alerts and sends them from one worker, so a
// slow mail server never holds up a sign-in.
//
// Email and Telegram are the channels set up in Settings; either may be nil,
// and a nil one is never used. With neither, NewSignInNotifier returns nil,
// and a nil notifier accepts and discards everything.
type SignInNotifier struct {
	email      *Email
	telegram   *Telegram
	consoleURL string
	log        *slog.Logger
	timeout    time.Duration

	queue chan signInJob
	done  chan struct{}
	once  sync.Once
}

type signInJob struct {
	what string
	send func(ctx context.Context) error
}

// NewSignInNotifier starts the worker.
func NewSignInNotifier(email *Email, telegram *Telegram, consoleURL string, log *slog.Logger) *SignInNotifier {
	if email == nil && telegram == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	n := &SignInNotifier{
		email: email, telegram: telegram, consoleURL: consoleURL, log: log,
		timeout: 15 * time.Second,
		queue:   make(chan signInJob, queueSize),
		done:    make(chan struct{}),
	}
	go n.run()
	return n
}

// EmailOn reports whether email is set up.
func (n *SignInNotifier) EmailOn() bool { return n != nil && n.email != nil }

// TelegramOn reports whether Telegram is set up.
func (n *SignInNotifier) TelegramOn() bool { return n != nil && n.telegram != nil }

// ConsoleURL is where links in alerts point.
func (n *SignInNotifier) ConsoleURL() string {
	if n == nil {
		return ""
	}
	return n.consoleURL
}

// Admins tells administrators: an email to each address in to, one message per
// address so they do not learn each other's. With platform set -- a sign-in to
// the default organization -- the configured email recipients and Telegram
// chats are told as well; another organization's sign-ins never reach them.
func (n *SignInNotifier) Admins(e SignIn, to []string, platform bool) {
	if n == nil {
		return
	}
	n.toAdmins(SignInAdminMessage(e, n.consoleURL), to, platform, "sign-in of "+e.Email)
}

// Reported tells administrators that the account holder disowned a sign-in.
func (n *SignInNotifier) Reported(r SignInReport, to []string, platform bool) {
	if n == nil {
		return
	}
	n.toAdmins(SignInReportedMessage(r, n.consoleURL), to, platform, "reported sign-in of "+r.Email)
}

// Person emails the person who signed in. Never Telegram: the chats are the
// administrators'.
func (n *SignInNotifier) Person(e SignIn, reportURL string) {
	if n == nil || n.email == nil {
		return
	}
	m := SignInPersonMessage(e, n.consoleURL, reportURL)
	to := e.Email
	n.enqueue(signInJob{what: "sign-in email to " + to, send: func(ctx context.Context) error {
		return n.email.SendTo(ctx, []string{to}, m.Subject, m.Text, m.HTML)
	}})
}

func (n *SignInNotifier) toAdmins(m Rendered, to []string, platform bool, what string) {
	if n.email != nil {
		recipients := to
		if platform {
			recipients = append(append([]string(nil), to...), n.email.Recipients()...)
		}
		for _, addr := range dedupeFold(recipients) {
			addr := addr
			n.enqueue(signInJob{what: what + " to " + addr, send: func(ctx context.Context) error {
				return n.email.SendTo(ctx, []string{addr}, m.Subject, m.Text, m.HTML)
			}})
		}
	}
	if n.telegram != nil && platform {
		n.enqueue(signInJob{what: what + " to telegram", send: func(ctx context.Context) error {
			return n.telegram.SendHTML(ctx, m.Telegram, m.LinkLabel, m.Link)
		}})
	}
}

func dedupeFold(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, a := range in {
		a = strings.TrimSpace(a)
		k := strings.ToLower(a)
		if a == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out
}

func (n *SignInNotifier) enqueue(j signInJob) {
	defer func() {
		// Close raced us; the alert is dropped as it would be when full.
		if recover() != nil {
			n.log.Warn("sign-in alert dropped during shutdown", "about", j.what)
		}
	}()
	select {
	case n.queue <- j:
	default:
		n.log.Warn("alert queue full; dropping a sign-in alert", "about", j.what,
			"detail", "the alert channels are not keeping up; the sign-in itself is audited")
	}
}

// Close stops accepting alerts and waits for the queued ones, up to ctx's
// deadline.
func (n *SignInNotifier) Close(ctx context.Context) {
	if n == nil {
		return
	}
	n.once.Do(func() { close(n.queue) })
	select {
	case <-n.done:
	case <-ctx.Done():
		n.log.Warn("shutting down with sign-in alerts still unsent", "queued", len(n.queue))
	}
}

func (n *SignInNotifier) run() {
	defer close(n.done)
	for j := range n.queue {
		ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
		err := j.send(ctx)
		cancel()
		if err != nil {
			n.log.Error("sign-in alert not delivered", "about", j.what, "error", err)
			continue
		}
		n.log.Info("sign-in alert sent", "about", j.what)
	}
}

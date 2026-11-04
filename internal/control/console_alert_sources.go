package control

import (
	"context"
	"strings"

	"github.com/gsoultan/argus/internal/notify"
)

// What raises a console alert, in one place, so the wording of the bell is
// read and changed together. Each is called beside the audit entry for the
// same event; see console_alerts.go.

// describeAsk is what a request asks for, in words.
func describeAsk(principal string, hosts []string) string {
	on := joinHosts(hosts)
	switch {
	case principal == "db:restore":
		return "a restore of a database on " + on
	case principal == statementPrincipal:
		return "approval to run a statement on a production database on " + on
	case strings.HasPrefix(principal, "db:"):
		return strings.TrimPrefix(principal, "db:") + " access to a database on " + on
	default:
		return principal + " on " + on
	}
}

// alertRequestWaiting tells whoever may decide it that a request is waiting.
func (a *API) alertRequestWaiting(ctx context.Context, req AccessRequest) {
	sev, title := "warning", req.RequesterEmail+" asks for "+describeAsk(req.Principal, req.AssetHostnames)
	if req.BreakGlass {
		sev, title = "critical", "Break-glass: "+title
	}
	a.raiseAlert(ctx, ConsoleAlert{
		Kind: AlertApproval, Severity: sev, Audience: AudienceDeciders, Actor: req.RequesterEmail,
		Title: title, Detail: req.Justification, Link: "/approvals",
	})
}

// alertRequestDecided tells the person who asked.
func (a *API) alertRequestDecided(ctx context.Context, req AccessRequest, decision, by, note string) {
	what := describeAsk(req.Principal, req.AssetHostnames)
	al := ConsoleAlert{Kind: AlertApproval, Audience: AudiencePerson, Recipient: req.RequesterEmail,
		Actor: by, Link: "/approvals"}
	if decision == "approved" {
		al.Title = "Your request for " + what + " was approved"
		al.Detail = "Approved by " + by + "."
		if req.ExpiresAt != nil {
			al.Detail = "Approved by " + by + ", until " + req.ExpiresAt.UTC().Format("Jan 2, 15:04 UTC") + "."
		}
	} else {
		al.Severity = "warning"
		al.Title = "Your request for " + what + " was denied"
		al.Detail = by + ": " + note
	}
	a.raiseAlert(ctx, al)
}

// alertDirectLoginWaiting tells deciders a login is being held for them.
func (a *API) alertDirectLoginWaiting(ctx context.Context, d DirectLogin) {
	a.raiseAlert(ctx, ConsoleAlert{
		Kind: AlertApproval, Severity: "warning", Audience: AudienceDeciders,
		Title:  "Direct login waiting on " + d.Hostname + " as " + d.Principal,
		Detail: describeDirectLogin(d) + ". The server holds it until someone decides.",
		Link:   "/approvals",
	})
}

// alertSignIn tells the organization's administrators someone signed in.
// A new device, or a recovery code, is a warning; anything else is routine
// and is what "only new devices" leaves out.
func (a *API) alertSignIn(ctx context.Context, e notify.SignIn) {
	sev, title := "info", e.Email+" signed in"
	if e.NewDevice {
		sev, title = "warning", e.Email+" signed in from a new device"
	}
	kind := AlertSignIn
	if e.Method == notify.MethodRecoveryCode {
		// The usual second factor was unavailable: a lost phone, or
		// someone else. Not something an administrator may mute.
		kind, sev, title = AlertSecurity, "critical", e.Email+" signed in with a recovery code"
	}
	from := e.ClientIP
	if from != "" && e.Location != "" {
		from += " (" + e.Location + ")"
	}
	parts := []string{}
	for _, p := range []string{e.Device, from, e.Method} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	a.raiseAlert(ctx, ConsoleAlert{
		Kind: kind, Severity: sev, Audience: AudienceAdmins, Actor: e.Email,
		Title: title, Detail: strings.Join(parts, " · "), Link: "/audit",
	})
}

// alertSessionStarted tells administrators a session opened. A login that
// did not go through Argus is a warning.
func (a *API) alertSessionStarted(ctx context.Context, s notify.SessionStarted) {
	who := s.UserEmail
	if who == "" {
		who = "Someone"
	}
	proto := strings.ToUpper(s.Protocol)
	if proto == "" {
		proto = "a session"
	}
	al := ConsoleAlert{Kind: AlertSession, Audience: AudienceAdmins, Actor: s.UserEmail,
		Title: who + " opened " + proto + " on " + s.Host + " as " + s.Principal}
	if s.Origin == "direct" {
		al.Severity = "warning"
		al.Title = "Direct login on " + s.Host + " as " + s.Principal
		al.Detail = "Not through Argus. The agent recorded it."
	}
	if s.ClientIP != "" {
		if al.Detail != "" {
			al.Detail += " "
		}
		al.Detail += "From " + s.ClientIP + "."
	}
	if s.ID != "" {
		al.Link = "/sessions/" + s.ID
	}
	a.raiseAlert(ctx, al)
}

// alertSecurity tells administrators of something they must look at.
func (a *API) alertSecurity(ctx context.Context, actor, title, detail string) {
	a.raiseAlert(ctx, ConsoleAlert{
		Kind: AlertSecurity, Severity: "critical", Audience: AudienceAdmins, Actor: actor,
		Title: strings.TrimPrefix(title, "Argus: "), Detail: detail, Link: "/audit",
	})
}

// alertServer tells administrators about a server joining or leaving.
func (a *API) alertServer(ctx context.Context, actor, title, detail string) {
	a.raiseAlert(ctx, ConsoleAlert{
		Kind: AlertServer, Audience: AudienceAdmins, Actor: actor,
		Title: title, Detail: detail, Link: "/assets",
	})
}

// alertBackup tells administrators a backup or a test restore of db needs
// them, in the bell and, as a security alert, on their channels: a backup
// that cannot be restored is found out now or on the worst day.
func (a *API) alertBackup(ctx context.Context, db Database, severity, title, detail string) {
	a.raiseAlert(ctx, ConsoleAlert{
		Kind: AlertBackup, Severity: severity, Audience: AudienceAdmins, Actor: "",
		Title: title, Detail: detail, Link: "/databases/" + db.ID,
	})
	if a.Alerts != nil {
		a.Alerts.SecurityEvent("Argus: "+title, detail)
	}
}

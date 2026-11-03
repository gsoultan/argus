package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Account emails: a person being given a temporary password, and
// administrators being told a server connected.

// Welcome is a person being sent a temporary password: when they are added,
// or when an administrator sends them a new one.
type Welcome struct {
	Email string
	Name  string
	Role  string
	// Org is the organization's display name, empty for the default one.
	Org string
	// Password is the temporary password. It goes in this one email and
	// nowhere else: not the audit log, not the console's response.
	Password  string
	ExpiresAt time.Time
	// By is the administrator who added them or sent it.
	By string
	// Reset is a new temporary password for someone who already had an
	// account, rather than a first one.
	Reset bool
	// MFARequired says the person must set up an authenticator app after
	// choosing their password.
	MFARequired bool
}

// ErrNoEmail means email is not set up, so nothing can be sent to a person.
var ErrNoEmail = errors.New("email is not set up in Settings, Alerts")

// WelcomeMessage is the email with a temporary password in it.
func WelcomeMessage(w Welcome, consoleURL string) Rendered {
	first := ""
	if w.Name != "" && !strings.EqualFold(w.Name, w.Email) {
		first, _, _ = strings.Cut(strings.TrimSpace(w.Name), " ")
	}
	greet := "Hello"
	if first != "" {
		greet = "Hi " + first
	}
	where := orgOrArgus(w.Org)
	subject := "Your Argus account is ready"
	lead := greet + ", " + orUnknown(w.By, "an administrator") + " added you to " + where
	if r := roleName(w.Role); r != "" {
		lead += " as " + article(r) + " " + r
	}
	lead += ". Sign in with the temporary password below."
	if w.Reset {
		subject = "A new temporary password for Argus"
		lead = greet + ", " + orUnknown(w.By, "an administrator") + " sent you a new temporary password for " +
			where + ". Your old password no longer works, and every session on your account was signed out."
	}
	expires := w.ExpiresAt.UTC().Format("Mon 2 Jan 2006, 15:04 MST")
	steps := []string{
		"Open Argus and sign in as " + w.Email + " with the temporary password.",
		"Choose a password of your own. Argus asks for one before anything else, and the temporary one stops working.",
	}
	if w.MFARequired {
		steps = append(steps, "Set up an authenticator app (such as Google Authenticator, 1Password or Authy) "+
			"and keep the recovery codes Argus shows you.")
	}
	facts := []fact{{"Account", w.Email}, {"Works until", expires}}
	if w.Org != "" {
		facts = append(facts, fact{"Organization", w.Org})
	}
	link := consoleLink(consoleURL, "/")
	warn := "Nobody from Argus will ask you for this password. If you weren't expecting this email, " +
		"don't sign in; tell " + orUnknown(w.By, "your administrator") + "."

	var t strings.Builder
	t.WriteString(lead + "\n\n")
	textFacts(&t, facts)
	fmt.Fprintf(&t, "\nTemporary password: %s\n\n", w.Password)
	for i, st := range steps {
		fmt.Fprintf(&t, "%d. %s\n", i+1, st)
	}
	if link != "" {
		fmt.Fprintf(&t, "\nSign in: %s\n", link)
	}
	t.WriteString("\n" + warn + "\n")
	return Rendered{
		Subject: subject,
		Text:    t.String(),
		HTML: renderMail(mailData{
			Title: subject, Lead: lead, Facts: facts,
			CodeLabel: "Temporary password", Code: w.Password, Steps: steps,
			Warn: warn, WarnAmber: true,
			Button: "Sign in to Argus", Link: link,
			Note: "The password works once, until " + expires +
				". If it runs out, ask an administrator to send you a new one.",
			Footer: "Sent because an administrator of " + where + " added you, or reset your password.",
			Host:   hostOf(consoleURL),
		}),
	}
}

func orUnknown(s, otherwise string) string {
	if strings.TrimSpace(s) == "" {
		return otherwise
	}
	return s
}

// roleName is a role as the console names it.
func roleName(role string) string {
	switch role {
	case "operator":
		return "member"
	case "auditor":
		return "viewer"
	}
	return role
}

func article(word string) string {
	if word != "" && strings.ContainsRune("aeiouAEIOU", rune(word[0])) {
		return "an"
	}
	return "a"
}

// SendWelcome emails a temporary password and waits for the mail server to
// take it, so the administrator who sent it is told whether it went. Never
// Telegram: the chats are the administrators', and this is the person's.
func (n *SignInNotifier) SendWelcome(ctx context.Context, w Welcome) error {
	if n == nil || n.email == nil {
		return ErrNoEmail
	}
	m := WelcomeMessage(w, n.consoleURL)
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	return n.email.SendTo(ctx, []string{w.Email}, m.Subject, m.Text, m.HTML)
}

// ServerConnected is a server's agent connecting to the control plane.
type ServerConnected struct {
	Host string
	Org  string
	// First is the agent's first connection since it enrolled; otherwise it
	// is back after being disconnected.
	First bool
	// Transport is "quic" or "https", RemoteAddr where it connected from.
	Transport  string
	RemoteAddr string
	At         time.Time
}

// ServerConnectedMessage is the alert administrators get.
func ServerConnectedMessage(e ServerConnected, consoleURL string) Rendered {
	title := e.Host + " connected to Argus"
	lead := "The agent on " + e.Host + " connected for the first time. The server is ready: " +
		"assign it to the people and teams who should reach it."
	tag := "New server"
	if !e.First {
		title = e.Host + " is connected again"
		lead = "The agent on " + e.Host + " reconnected to the control plane after being disconnected."
		tag = ""
	}
	facts := []fact{{"Server", e.Host}, {"When", e.At.UTC().Format("Mon 2 Jan 2006, 15:04 MST")}}
	if e.RemoteAddr != "" {
		facts = append(facts, fact{"From", e.RemoteAddr})
	}
	if e.Transport != "" {
		facts = append(facts, fact{"Connection", strings.ToUpper(e.Transport)})
	}
	if e.Org != "" {
		facts = append(facts, fact{"Organization", e.Org})
	}
	link := consoleLink(consoleURL, "/assets")

	var t strings.Builder
	t.WriteString(lead + "\n\n")
	textFacts(&t, facts)
	if link != "" {
		fmt.Fprintf(&t, "\nOpen it in Argus: %s\n", link)
	}
	t.WriteString("\nYou get this because you're an administrator of " + orgOrArgus(e.Org) +
		" and server alerts are on. Change them in Settings, Alerts.\n")
	return Rendered{
		Subject: "Argus: " + title,
		Text:    t.String(),
		HTML: renderMail(mailData{
			Tag: tag, Title: title, Lead: lead, Facts: facts,
			Button: "Open in Argus", Link: link,
			Footer: "You get this because you're an administrator of " + orgOrArgus(e.Org) +
				" and server alerts are on. Change them in Settings › Alerts.",
			Host: hostOf(consoleURL),
		}),
		Telegram:  telegramMessage("🖥️", title, tag, "", facts),
		LinkLabel: "Open in Argus",
		Link:      link,
	}
}

// ServerConnected tells administrators a server connected: an email to each
// of to and, with platform set, the configured recipients and Telegram chats.
func (n *SignInNotifier) ServerConnected(e ServerConnected, to []string, platform bool) {
	if n == nil {
		return
	}
	n.toAdmins(ServerConnectedMessage(e, n.consoleURL), to, platform, "server "+e.Host+" connected")
}

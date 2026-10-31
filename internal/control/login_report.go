package control

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// postLogin is an agent reporting a direct login: one that reached sshd without
// coming through Argus.
//
// It is the whole of what Argus will know about that attempt, so it goes into
// the audit chain -- the record an auditor reads and the one that cannot be
// quietly edited afterwards.
func (a *API) postLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hostname   string `json:"hostname"`
		Principal  string `json:"principal"`
		ClientAddr string `json:"client_addr"`
		Command    string `json:"command"`
		Origin     string `json:"origin"`
		Scope      string `json:"scope"`
		// Refused is a login the agent turned away because the deployment
		// is console-only. Absent from an agent older than that.
		Refused bool `json:"refused"`
		// Allowed is a login let in because the server is set to allow and
		// record direct SSH logins. Believed only if it is.
		Allowed bool `json:"allowed"`
		// Owner is set when the login used a personal account its owner may
		// reach directly. Believed only if the control plane agrees.
		Owner string    `json:"owner"`
		At    time.Time `json:"at"`
	}
	if err := decodeReport(r, &in, a.log, "report/login"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Hostname == "" || in.Principal == "" {
		writeErr(w, http.StatusBadRequest, "hostname and principal are required")
		return
	}
	if !a.mayReportFor(w, r, in.Hostname) {
		return
	}
	if in.At.IsZero() {
		in.At = time.Now().UTC()
	}

	what := "an interactive session"
	if in.Command != "" {
		what = fmt.Sprintf("the command %q", in.Command)
	}
	from := in.ClientAddr
	if from == "" {
		from = "an unknown address"
	}
	if in.Refused {
		// Not a bypass that happened: one that was tried and stopped. Still
		// in the chain, because someone holding a working key to sshd on a
		// console-only fleet is exactly what an auditor needs to find.
		detail := fmt.Sprintf("%s tried to log in to %s from %s at %s to run %s without "+
			"the Argus console. The deployment is console-only, so the agent refused the "+
			"login before anything ran. Whoever holds that key or password can still "+
			"reach sshd directly; remove it if it is not meant to exist.",
			in.Principal, in.Hostname, from, in.At.UTC().Format(time.RFC3339), what)
		if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
			Action:   "session.direct_refused",
			Severity: "warning",
			Target:   in.Hostname,
			Detail:   detail,
		}); err != nil {
			a.fail(w, "report login", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}

	if in.Allowed && in.Owner != "" {
		if p, ok, err := a.store.PersonalOwner(r.Context(), in.Hostname, in.Principal); err == nil && ok &&
			p.Direct == DirectSSHAllow && strings.EqualFold(p.Owner, in.Owner) {
			if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
				Action:     "session.direct_allowed",
				Severity:   "notice",
				ActorEmail: p.Owner,
				Target:     in.Hostname,
				Detail: fmt.Sprintf("%s logged in to %s as their personal account %s from %s at %s to "+
					"run %s without the Argus console. A team of theirs may SSH directly to this "+
					"server, so nobody approved it; the session is being recorded in their name.",
					p.Owner, in.Hostname, in.Principal, from, in.At.UTC().Format(time.RFC3339), what),
			}); err != nil {
				a.fail(w, "report login", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
			return
		}
	}
	if in.Allowed {
		if mode, err := a.store.EffectiveDirectSSH(r.Context(), in.Hostname); err == nil &&
			mode == DirectSSHAllow {
			if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
				Action:   "session.direct_allowed",
				Severity: "warning",
				Target:   in.Hostname,
				Detail: fmt.Sprintf("%s logged in to %s from %s at %s to run %s without the "+
					"Argus console. The server is set to allow and record direct SSH logins, "+
					"so nobody approved it; the session is being recorded and is attributed "+
					"to the server account, not to a person.",
					in.Principal, in.Hostname, from, in.At.UTC().Format(time.RFC3339), what),
			}); err != nil {
				a.fail(w, "report login", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
			return
		}
	}

	// An agent only lets a direct login through when it was approved or the
	// server is set to allow it. A report of one otherwise is an agent that is
	// not following the setting -- an old one, a misconfigured one, or a report that is not what it claims
	// to be -- and the auditor is told, with the highest severity.
	detail := fmt.Sprintf("%s logged in to %s from %s at %s and ran %s without "+
		"passing through Argus, and the agent reports it let the login through "+
		"unrecorded: there is no replay, and no approval, time window or principal "+
		"check was applied. The server is not set to allow direct logins, so the "+
		"agent was not following its setting; check the agent on this host.",
		in.Principal, in.Hostname, from, in.At.UTC().Format(time.RFC3339), what)

	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:   "session.direct_unrecorded",
		Severity: "critical",
		Target:   in.Hostname,
		Detail:   detail,
	}); err != nil {
		a.fail(w, "report login", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

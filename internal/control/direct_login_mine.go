package control

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The person whose own terminal is waiting at "Waiting for approval" can do
// two things from the console while it waits: take the login back, or nudge
// the people who decide. Neither decides anything about anyone else's login.

// DirectLoginRemindEvery is how soon the same waiting login may be reminded
// about again, so a held-down button is one reminder, not a flood.
const DirectLoginRemindEvery = 30 * time.Second

// directLoginWithdrawnNote is the decision note a withdrawn login carries, so
// the approvals list and the audit log say why it was refused.
const directLoginWithdrawnNote = "Withdrawn by the person logging in"

var directLoginReminded sync.Map // id -> time.Time

// myWaitingDirectLogin finds a direct login that is this person's own: it
// logs in as their personal account, on a server where they hold one. Any
// other login is answered 404, so the route says nothing about logins that
// are not theirs.
func (a *API) myWaitingDirectLogin(w http.ResponseWriter, r *http.Request, email string) (DirectLogin, bool) {
	a.expireAndAudit(r.Context())
	d, err := a.store.DirectLoginByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrDirectLoginNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return d, false
	}
	if err != nil {
		a.fail(w, "direct login", err)
		return d, false
	}
	acct, err := a.store.ServerAccountOf(r.Context(), email)
	if errors.Is(err, ErrNoAccount) || (err == nil && (acct.LoginName == "" || acct.LoginName != d.Principal)) {
		writeErr(w, http.StatusNotFound, ErrDirectLoginNotFound.Error())
		return d, false
	}
	if err != nil {
		a.fail(w, "server account", err)
		return d, false
	}
	servers, err := a.store.PersonServers(r.Context(), normaliseEmail(email))
	if err != nil {
		a.fail(w, "my servers", err)
		return d, false
	}
	mine := false
	for _, s := range servers {
		if s.PersonalAccount && strings.EqualFold(s.Hostname, d.Hostname) {
			mine = true
			break
		}
	}
	if !mine {
		writeErr(w, http.StatusNotFound, ErrDirectLoginNotFound.Error())
		return d, false
	}
	if d.State != "pending" {
		writeErr(w, http.StatusConflict, "this login is no longer waiting: it was "+d.State)
		return d, false
	}
	return d, true
}

// postDirectLoginWithdraw refuses the person's own waiting login. Their
// terminal is told no at once instead of waiting out the two minutes.
func (a *API) postDirectLoginWithdraw(w http.ResponseWriter, r *http.Request, email string) {
	if _, ok := a.myWaitingDirectLogin(w, r, email); !ok {
		return
	}
	d, err := a.store.DecideDirectLogin(r.Context(), r.PathValue("id"), false, normaliseEmail(email), directLoginWithdrawnNote)
	switch {
	case errors.Is(err, ErrDirectLoginExpired), errors.Is(err, ErrAlreadyDecided):
		writeErr(w, http.StatusConflict, "this login is no longer waiting")
		return
	case err != nil:
		a.fail(w, "withdraw direct login", err)
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action:     "session.direct_withdrawn",
		Severity:   "info",
		ActorEmail: normaliseEmail(email),
		Target:     d.Hostname,
		Detail:     normaliseEmail(email) + " withdrew their own direct login, " + describeDirectLogin(d) + ", before anyone decided.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	a.closeAlerts(r.Context(), "direct-login:"+d.ID, "Withdrawn by "+normaliseEmail(email))
	directLoginReminded.Delete(d.ID)
	writeJSON(w, http.StatusOK, d)
}

// postDirectLoginRemind raises the waiting login again for the people who
// decide it, at most once every DirectLoginRemindEvery.
func (a *API) postDirectLoginRemind(w http.ResponseWriter, r *http.Request, email string) {
	d, ok := a.myWaitingDirectLogin(w, r, email)
	if !ok {
		return
	}
	now := time.Now()
	if last, ok := directLoginReminded.Load(d.ID); ok {
		if wait := DirectLoginRemindEvery - now.Sub(last.(time.Time)); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, "approvers were reminded a moment ago")
			return
		}
	}
	directLoginReminded.Store(d.ID, now)
	left := time.Until(d.ExpiresAt).Round(time.Second)
	a.raiseAlert(r.Context(), ConsoleAlert{
		Kind: AlertApproval, Severity: "warning", Audience: AudienceDeciders,
		Actor:  normaliseEmail(email),
		Title:  "Reminder: direct login waiting on " + d.Hostname + " as " + d.Principal,
		Detail: normaliseEmail(email) + " is waiting at their terminal. " + describeDirectLogin(d) + ". Refused in " + left.String() + " if nobody decides.",
		Link:   "/approvals", Subject: "direct-login:" + d.ID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"reminded": true, "nextAt": now.Add(DirectLoginRemindEvery)})
}

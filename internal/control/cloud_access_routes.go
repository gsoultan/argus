package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 5: just-in-time access to an account's AWS console and command
line.

Who asks: read access, administrators, owners and auditors; operate access
(everything but IAM), administrators and owners. Who decides: an approver,
administrator or owner who did not ask. When: on Production both levels
need a decision, on Staging operate does, on Development neither.

A grant lasts what was asked, 30 minutes to 12 hours, from when it is
approved. While it lasts, its holder (and only they) opens console sessions
or gets command-line keys, each made fresh as them and never longer than
an hour or than the grant has left. Every grant, decision and session is in
the audit log; AWS's own log names the person as the source identity.

Ending a grant early stops new sessions. One already open runs out on its
own, within the hour: AWS has no way to end a session from outside short
of changing the role, which the manage role may not do.
*/

// cloudAccessMinutes are the lengths a grant may be.
var cloudAccessMinutes = []int{30, 60, 120, 240, 480, 720}

func (a *API) registerCloudAccessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/cloud/access", a.getCloudAccess)
	mux.HandleFunc("POST /api/v1/cloud/access", a.postCloudAccess)
	mux.HandleFunc("POST /api/v1/cloud/access/{id}/approve", a.postCloudAccessDecision(true))
	mux.HandleFunc("POST /api/v1/cloud/access/{id}/decline", a.postCloudAccessDecision(false))
	mux.HandleFunc("POST /api/v1/cloud/access/{id}/cancel", a.postCloudAccessCancel)
	mux.HandleFunc("POST /api/v1/cloud/access/{id}/end", a.postCloudAccessEnd)
	mux.HandleFunc("POST /api/v1/cloud/access/{id}/console", a.postCloudAccessSession(false))
	mux.HandleFunc("POST /api/v1/cloud/access/{id}/credentials", a.postCloudAccessSession(true))
}

// cloudAccessNeedsApproval is the rule by environment.
func cloudAccessNeedsApproval(env string, level cloud.ConsoleLevel) bool {
	switch env {
	case "production":
		return true
	case "staging":
		return level == cloud.ConsoleOperate
	}
	return false
}

func mayAskCloudAccess(role string, level cloud.ConsoleLevel) bool {
	if level == cloud.ConsoleOperate {
		return role == "admin" || role == "owner"
	}
	return seesWholeFleet(role)
}

func decidesCloud(role string) bool { return role == "admin" || role == "owner" || role == "approver" }

func (a *API) auditCloudAccess(r *http.Request, g CloudAccess, what, actor, detail string) {
	sev := "notice"
	if g.Level == cloud.ConsoleOperate || what == "credentials" {
		sev = "warn"
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.access." + what, Severity: sev, ActorEmail: actor,
		Target: "aws " + g.AccountName + " (" + g.CloudAccount + ")", Detail: detail + " (access " + g.ID + ")"})
}

func accessWords(g CloudAccess) string {
	return fmt.Sprintf("%s access to %s for %s", g.Level, g.AccountName, minutesWords(g.Minutes))
}

func minutesWords(m int) string {
	if m < 60 {
		return fmt.Sprintf("%d minutes", m)
	}
	if m == 60 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", m/60)
}

func (a *API) getCloudAccess(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) && sess.Role != "approver" {
		writeErr(w, http.StatusForbidden, "cloud access is for administrators, owners, auditors and approvers")
		return
	}
	// Everyone who may see cloud access sees all of it: who holds which
	// account's console is the question an auditor asks.
	list, err := a.store.CloudAccessList(r.Context(), "", r.URL.Query().Get("waiting") == "1", 200)
	if err != nil {
		a.fail(w, "cloud access", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"grants": list, "minutes": cloudAccessMinutes})
}

type cloudAccessIn struct {
	AccountID string             `json:"accountId"`
	Level     cloud.ConsoleLevel `json:"level"`
	Minutes   int                `json:"minutes"`
	Reason    string             `json:"reason"`
}

// consoleMissing refuses a level whose role the account lacks.
func consoleMissing(c CloudAccount, provider cloud.Provider, level cloud.ConsoleLevel) error {
	con, ok := provider.(cloud.Console)
	if !ok {
		return errors.New(c.Provider + " has no console access through Argus yet")
	}
	role := con.ConsoleRole(c.AccountID, level)
	name := role[strings.LastIndex(role, "/")+1:]
	if slices.Contains(c.ManageMissing, "role:"+name) {
		return fmt.Errorf("%s has no %s role yet. Update the role from the account's row on the Cloud overview: deploy the template it shows over the argus-access stack, and Argus sees the change at its next read", c.Name, name)
	}
	return nil
}

func (a *API) postCloudAccess(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in cloudAccessIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !cloud.ValidConsoleLevel(in.Level) {
		writeErr(w, http.StatusBadRequest, "pick read or operate")
		return
	}
	if !mayAskCloudAccess(sess.Role, in.Level) {
		if in.Level == cloud.ConsoleOperate {
			writeErr(w, http.StatusForbidden, "operate access is for administrators and owners; ask for read access instead")
		} else {
			writeErr(w, http.StatusForbidden, "console access is for administrators, owners and auditors")
		}
		return
	}
	if !slices.Contains(cloudAccessMinutes, in.Minutes) {
		writeErr(w, http.StatusBadRequest, "pick 30 minutes, or 1, 2, 4, 8 or 12 hours")
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) < 5 || len(in.Reason) > 500 {
		writeErr(w, http.StatusBadRequest, "say why, in 5 to 500 characters; it is kept with the access in the audit log")
		return
	}
	m, code, err := a.cloudManagedAccount(r.Context(), in.AccountID)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	p := m.provider
	if err := consoleMissing(m.acct, p, in.Level); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	waits := cloudAccessNeedsApproval(m.acct.Environment, in.Level)
	g := CloudAccess{AccountID: m.acct.ID, Level: in.Level, Environment: m.acct.Environment, Minutes: in.Minutes,
		Reason: in.Reason, State: "active", RequestedBy: sess.Email}
	if waits {
		g.State = "awaiting_approval"
	}
	g, err = a.store.CreateCloudAccess(r.Context(), g)
	if errors.Is(err, ErrCloudAccessOpen) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud access", err)
		return
	}
	detail := "Asked for " + accessWords(g) + ". Reason: " + g.Reason
	if waits {
		detail += ". Waits for approval (" + g.Environment + ")"
	} else {
		detail += ". In force at once (" + g.Environment + " needs no approval for it)"
	}
	a.auditCloudAccess(r, g, "requested", sess.Email, detail)
	code = http.StatusOK
	if waits {
		code = http.StatusAccepted
	}
	writeJSON(w, code, g)
}

func (a *API) postCloudAccessDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !decidesCloud(sess.Role) {
			writeErr(w, http.StatusForbidden, "deciding cloud access requires the approver, admin or owner role")
			return
		}
		g, err := a.store.CloudAccessGrant(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrCloudAccessNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			a.fail(w, "cloud access", err)
			return
		}
		if strings.EqualFold(g.RequestedBy, sess.Email) {
			writeErr(w, http.StatusForbidden, "someone other than the person who asked decides their access")
			return
		}
		g, err = a.store.DecideCloudAccess(r.Context(), g.ID, sess.Email, approve)
		if errors.Is(err, ErrCloudAccessSettled) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			a.fail(w, "cloud access", err)
			return
		}
		if approve {
			a.auditCloudAccess(r, g, "approved", sess.Email, "Approved "+accessWords(g)+" for "+g.RequestedBy+", until "+g.EndsAt.UTC().Format(time.RFC3339))
		} else {
			a.auditCloudAccess(r, g, "declined", sess.Email, "Declined "+accessWords(g)+" for "+g.RequestedBy)
		}
		writeJSON(w, http.StatusOK, g)
	}
}

func (a *API) postCloudAccessCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	g, err := a.store.CancelCloudAccess(r.Context(), r.PathValue("id"), sess.Email)
	if errors.Is(err, ErrCloudAccessNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, ErrCloudAccessSettled) {
		writeErr(w, http.StatusConflict, "only the person who asked can withdraw it, while it waits")
		return
	}
	if err != nil {
		a.fail(w, "cloud access", err)
		return
	}
	a.auditCloudAccess(r, g, "cancelled", sess.Email, "Withdrew the request for "+accessWords(g))
	writeJSON(w, http.StatusOK, g)
}

// postCloudAccessEnd ends a grant early: its holder may, and so may anyone
// who decides access.
func (a *API) postCloudAccessEnd(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	g, err := a.store.CloudAccessGrant(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrCloudAccessNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud access", err)
		return
	}
	if !strings.EqualFold(g.RequestedBy, sess.Email) && !decidesCloud(sess.Role) {
		writeErr(w, http.StatusForbidden, "only its holder, or an approver, admin or owner, ends someone's access")
		return
	}
	g, err = a.store.EndCloudAccess(r.Context(), g.ID, sess.Email)
	if errors.Is(err, ErrCloudAccessSettled) {
		writeErr(w, http.StatusConflict, "this access is not in force")
		return
	}
	if err != nil {
		a.fail(w, "cloud access", err)
		return
	}
	a.auditCloudAccess(r, g, "ended", sess.Email, "Ended "+g.RequestedBy+"'s "+accessWords(g)+" early. Sessions already open run out within the hour")
	writeJSON(w, http.StatusOK, g)
}

// postCloudAccessSession opens a console session, or gives command-line
// keys, under a grant in force, for its holder only.
func (a *API) postCloudAccessSession(keys bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var in struct {
			Region string `json:"region"`
		}
		_ = decode(r, &in)
		g, err := a.store.CloudAccessGrant(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrCloudAccessNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			a.fail(w, "cloud access", err)
			return
		}
		if !strings.EqualFold(g.RequestedBy, sess.Email) {
			writeErr(w, http.StatusForbidden, "only the person it was granted to opens sessions with it")
			return
		}
		now := time.Now()
		if !g.Live(now) {
			writeErr(w, http.StatusConflict, "this access is not in force; ask again")
			return
		}
		left := g.EndsAt.Sub(now)
		if left < cloud.MinConsoleSession {
			writeErr(w, http.StatusConflict, fmt.Sprintf("this access has %d minutes left, and AWS's shortest session is 15; ask again", int(left.Minutes())))
			return
		}
		// A role still being who it should be is checked again here, not
		// only when it was granted.
		if !mayAskCloudAccess(sess.Role, g.Level) {
			writeErr(w, http.StatusForbidden, "your role no longer allows "+string(g.Level)+" access")
			return
		}
		m, code, err := a.cloudManagedAccount(r.Context(), g.AccountID)
		if err != nil {
			writeErr(w, code, err.Error())
			return
		}
		p := m.provider
		if err := consoleMissing(m.acct, p, g.Level); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		con := p.(cloud.Console)
		region := in.Region
		if region == "" && len(m.acct.Regions) > 0 {
			region = m.acct.Regions[0]
		}
		if region != "" && !hasRegion(m.acct, region) {
			writeErr(w, http.StatusBadRequest, "pick one of the account's regions")
			return
		}
		d := min(left, cloud.MaxConsoleSession)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		var out any
		var until time.Time
		if keys {
			c, err := con.Credentials(ctx, m.target, cloud.Actor(sess.Email), g.Level, d, region)
			if err != nil {
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
			out, until = c, c.Expires
		} else {
			link, err := con.ConsoleLink(ctx, m.target, cloud.Actor(sess.Email), g.Level, d, consoleIssuer(r), region)
			if err != nil {
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
			out, until = link, link.Expires
		}
		if err := a.store.NoteCloudAccessSession(r.Context(), g.ID); err != nil {
			a.log.Error("cloud access: cannot count the session", "access", g.ID, "error", err)
		}
		what, words := "console", "Opened an AWS console session"
		if keys {
			what, words = "credentials", "Took AWS command-line keys"
		}
		a.auditCloudAccess(r, g, what, sess.Email, fmt.Sprintf("%s (%s) in %s, until %s", words, g.Level, region, until.UTC().Format(time.RFC3339)))
		writeJSON(w, http.StatusOK, out)
	}
}

// consoleIssuer is where AWS sends someone whose session ended: back to
// Argus, when the request says where that is.
func consoleIssuer(r *http.Request) string {
	if o := r.Header.Get("Origin"); strings.HasPrefix(o, "https://") && !strings.ContainsAny(o, " \"<>") {
		return o + "/cloud?tab=access"
	}
	return "https://" + r.Host + "/cloud?tab=access"
}

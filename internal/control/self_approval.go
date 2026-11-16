package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

/*
Approving your own request, when nobody else could.

Every approval in Argus means a second person agreed, and the person who
asked never decides their own request. That rule leaves an organization with
one person who may decide a kind of request with requests nobody can ever
approve: a Production cloud change, a restore in place, an update run on a
server whose rule asks for a second admin. They sit in Approvals until the
person who asked takes them back.

So the person who asked may approve their own request on one condition:
nobody else holds a role that decides it. Roles are counted from the
organization's enabled accounts each time, so the moment a second account
that could decide exists, the normal rule applies again, to requests already
waiting too. They type a phrase to confirm, and the approval is recorded at
warning severity beside the usual entry, so the audit log says plainly that
nobody else agreed.

	GET /api/v1/approvals/self   whether the reader decides alone, by kind

Who decides each kind:

	requests  access requests, database changes and restores, cloud changes,
	          cloud schedules and cloud access: approvers, admins and owners
	          (and a database's editors, where its editors approve changes)
	runs      update and software runs, disk grows and log cleanups: admins
	          and owners

Script review has its own form of the same exception (script_routes.go),
older than this one, which counts admins the same way.
*/

// SelfApprovalPhrase is what the person who asked types to approve their own
// request.
const SelfApprovalPhrase = "approve my own request"

var (
	// requestDeciders decide access requests and cloud changes.
	requestDeciders = []string{"approver", "admin", "owner"}
	// runDeciders decide package runs, disk grows and log cleanups.
	runDeciders = []string{"admin", "owner"}
)

// ErrOwnRequest is someone deciding a request they made. A store refusing
// it returns an error that is also ErrRunState or ErrSelfApproval, so the
// handlers that answer those answer it the same way.
var ErrOwnRequest = errors.New("this is your own request")

// errOwnRun is a run's store refusing the person who asked for it.
type errOwnRun struct{ why string }

func (e errOwnRun) Error() string { return ErrRunState.Error() + ": " + e.why }

func (e errOwnRun) Is(t error) bool { return t == ErrRunState || t == ErrOwnRequest }

// SoleDecider is whether email is the only enabled account holding one of
// roles. False when email holds none of them, however many others do.
func (s *Store) SoleDecider(ctx context.Context, email string, roles []string) (bool, error) {
	var self, others int
	err := s.db(ctx).QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE lower(email) = lower($1)),
		       count(*) FILTER (WHERE lower(email) <> lower($1))
		  FROM users WHERE role = ANY($2) AND disabled_at IS NULL`, email, roles).Scan(&self, &others)
	return self > 0 && others == 0, err
}

// approveOwn answers someone deciding their own request. It returns true
// when they may approve it, having recorded that they did; otherwise it has
// answered with why not, refused being the usual refusal.
//
// others is whether someone outside roles could decide it too (another
// editor of the database a change is for).
func (a *API) approveOwn(w http.ResponseWriter, r *http.Request, email string, approve bool,
	roles []string, others bool, confirm, refused, what, target string) bool {
	if !approve {
		writeErr(w, http.StatusForbidden, refused)
		return false
	}
	sole := false
	if !others {
		var err error
		if sole, err = a.store.SoleDecider(r.Context(), email, roles); err != nil {
			a.fail(w, "self approval", err)
			return false
		}
	}
	if !sole {
		writeErr(w, http.StatusForbidden, refused)
		return false
	}
	if strings.TrimSpace(confirm) != SelfApprovalPhrase {
		writeErr(w, http.StatusConflict, `nobody else can approve this, so you may approve it yourself: type "`+
			SelfApprovalPhrase+`" to confirm`)
		return false
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "approval.self_approved", Severity: "warning", ActorEmail: email, Target: target,
		Detail: "Approved their own " + what + ": nobody else holds a role that can decide it (" +
			humanRoles(roles) + ").",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	return true
}

// readConfirmSelf is the phrase from a decision's body, for the decisions
// that otherwise take none. An empty body is no phrase.
func readConfirmSelf(r *http.Request) (string, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return "", nil
	}
	var in struct {
		ConfirmSelf string `json:"confirmSelf"`
	}
	if err := decode(r, &in); err != nil {
		if strings.Contains(err.Error(), "EOF") {
			return "", nil
		}
		return "", err
	}
	return in.ConfirmSelf, nil
}

// SelfApproval is whether the reader is the only one who decides each kind,
// so the console offers them their own requests and says so when they ask.
type SelfApproval struct {
	Requests bool   `json:"requests"`
	Runs     bool   `json:"runs"`
	Phrase   string `json:"phrase"`
}

// GET /api/v1/approvals/self
func (a *API) getSelfApproval(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	out := SelfApproval{Phrase: SelfApprovalPhrase}
	var err error
	if out.Requests, err = a.store.SoleDecider(r.Context(), sess.Email, requestDeciders); err != nil {
		a.fail(w, "self approval", err)
		return
	}
	if out.Runs, err = a.store.SoleDecider(r.Context(), sess.Email, runDeciders); err != nil {
		a.fail(w, "self approval", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

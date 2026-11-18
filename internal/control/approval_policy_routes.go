package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

/*
The approval policy's routes (approval_policy.go).

	GET  /api/v1/approval-policy                        everyone signed in
	PUT  /api/v1/approval-policy                        owners
	POST /api/v1/approval-policy/changes/{id}/approve   another owner
	POST /api/v1/approval-policy/changes/{id}/deny      another owner
	POST /api/v1/approval-policy/changes/{id}/cancel    the owner who asked
	GET  /api/v1/platform/approval-locks                platform owners
	PUT  /api/v1/platform/approval-locks                platform owners

Owners change the policy; an organization with no enabled owner lets its
admins, since first-run setup makes the first account an admin. A change
that asks for more approval applies at once. One that lets more people act
directly is itself an approval: another owner decides it, or, where there is
no other, the owner types PolicySelfPhrase and Activity records that nobody
else agreed, as for approving one's own request (self_approval.go).
*/

// PolicySelfPhrase is what the only owner types to loosen the policy alone.
const PolicySelfPhrase = "loosen our approval policy"

func (a *API) registerApprovalPolicyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/approval-policy", a.getApprovalPolicy)
	mux.HandleFunc("PUT /api/v1/approval-policy", a.putApprovalPolicy)
	mux.HandleFunc("POST /api/v1/approval-policy/changes/{id}/approve", a.postPolicyChangeDecision("approve"))
	mux.HandleFunc("POST /api/v1/approval-policy/changes/{id}/deny", a.postPolicyChangeDecision("deny"))
	mux.HandleFunc("POST /api/v1/approval-policy/changes/{id}/cancel", a.postPolicyChangeDecision("cancel"))
	mux.HandleFunc("GET /api/v1/platform/approval-locks", a.getApprovalLocks)
	mux.HandleFunc("PUT /api/v1/platform/approval-locks", a.putApprovalLocks)
}

// approvalPolicyView is the settings page's whole read.
type approvalPolicyView struct {
	Kinds   []ApprovalKindInfo    `json:"kinds"`
	Policy  ApprovalPolicy        `json:"policy"`
	Pending *ApprovalPolicyChange `json:"pending"`
	// Keepers is how many may change the policy; CanChange whether the
	// reader is one of them.
	Keepers   int    `json:"keepers"`
	CanChange bool   `json:"canChange"`
	Phrase    string `json:"phrase"`
	// Direct30 is how many actions went straight through in 30 days.
	Direct30 int `json:"direct30"`
	// Mine is, per kind, the environments where the reader acts directly,
	// so the console can say so where they would otherwise ask.
	Mine map[ApprovalKind][]string `json:"mine"`
}

func (a *API) approvalPolicyView(ctx context.Context, email string) (approvalPolicyView, error) {
	v := approvalPolicyView{Phrase: PolicySelfPhrase}
	var err error
	if v.Policy, err = a.store.ApprovalPolicy(ctx); err != nil {
		return v, err
	}
	locks, err := a.store.ApprovalLocks(ctx)
	if err != nil {
		return v, err
	}
	for _, k := range ApprovalKinds {
		k.Locked = nil
		for _, env := range k.Envs {
			if l := lockFor(locks, k.Kind, env); l != nil {
				if k.Locked == nil {
					k.Locked = map[string]string{}
				}
				k.Locked[env] = l.Reason
			}
		}
		v.Kinds = append(v.Kinds, k)
	}
	if v.Pending, err = a.store.PendingApprovalPolicyChange(ctx); err != nil {
		return v, err
	}
	if v.Pending != nil {
		v.Pending.Changes = v.Policy.Changes(v.Pending.Proposal)
		v.Pending.Loosens = v.Policy.Loosens(v.Pending.Proposal)
	}
	keepers, err := a.store.policyKeepers(ctx)
	if err != nil {
		return v, err
	}
	v.Mine = map[ApprovalKind][]string{}
	for _, k := range ApprovalKinds {
		for _, env := range k.Envs {
			if a.actsDirectly(ctx, k.Kind, env, email) != nil {
				v.Mine[k.Kind] = append(v.Mine[k.Kind], env)
			}
		}
	}
	v.Keepers = len(keepers)
	for _, k := range keepers {
		if strings.EqualFold(k, email) {
			v.CanChange = true
		}
	}
	err = a.store.db(ctx).QueryRow(ctx, `SELECT count(*) FROM audit_events
		WHERE action = 'approval.direct' AND at > now() - interval '30 days'`).Scan(&v.Direct30)
	return v, err
}

// GET /api/v1/approval-policy
func (a *API) getApprovalPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	v, err := a.approvalPolicyView(r.Context(), sess.Email)
	if err != nil {
		a.fail(w, "approval policy", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// policyKeeper authenticates someone who may change the policy.
func (a *API) policyKeeper(w http.ResponseWriter, r *http.Request) (string, []string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", nil, false
	}
	keepers, err := a.store.policyKeepers(r.Context())
	if err != nil {
		a.fail(w, "approval policy", err)
		return "", nil, false
	}
	for _, k := range keepers {
		if strings.EqualFold(k, sess.Email) {
			return sess.Email, keepers, true
		}
	}
	writeErr(w, http.StatusForbidden, "only owners change the approval policy")
	return "", nil, false
}

// lockedLoosening is the first locked cell next loosens, in words, or "".
func lockedLoosening(cur, next ApprovalPolicy, locks []ApprovalLock) string {
	for _, l := range locks {
		if choiceRank(next.choice(l.Kind, l.Env)) > choiceRank(cur.choice(l.Kind, l.Env)) {
			info, _ := kindInfo(l.Kind)
			why := envWords(l.Env) + " · " + info.Title + " always needs a second person on this platform"
			if l.Reason != "" {
				why += ": " + l.Reason
			}
			return why
		}
	}
	return ""
}

// PUT /api/v1/approval-policy
func (a *API) putApprovalPolicy(w http.ResponseWriter, r *http.Request) {
	email, keepers, ok := a.policyKeeper(w, r)
	if !ok {
		return
	}
	var in struct {
		ApprovalPolicy
		Reason      string `json:"reason"`
		ConfirmSelf string `json:"confirmSelf"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	next := in.ApprovalPolicy
	if err := next.normalise(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > 500 {
		writeErr(w, http.StatusBadRequest, "keep the reason under 500 characters")
		return
	}
	ctx := r.Context()
	cur, err := a.store.ApprovalPolicy(ctx)
	if err != nil {
		a.fail(w, "approval policy", err)
		return
	}
	locks, err := a.store.ApprovalLocks(ctx)
	if err != nil {
		a.fail(w, "approval policy", err)
		return
	}
	if why := lockedLoosening(cur, next, locks); why != "" {
		writeErr(w, http.StatusForbidden, why)
		return
	}
	changes := cur.Changes(next)
	if len(changes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"applied": true, "policy": cur})
		return
	}
	loosens := cur.Loosens(next)
	if len(loosens) == 0 {
		// More approval, or the same: nobody needs to agree to that.
		p, err := a.store.SetApprovalPolicy(ctx, next, email, "")
		if err != nil {
			a.fail(w, "approval policy", err)
			return
		}
		a.auditPolicy(ctx, "approval_policy.changed", "notice", email,
			"Changed the approval policy: "+strings.Join(changes, "; ")+"."+reasonWords(in.Reason))
		writeJSON(w, http.StatusOK, map[string]any{"applied": true, "policy": p})
		return
	}
	if len(in.Reason) < 5 {
		writeErr(w, http.StatusBadRequest, "say why, in a few words: letting more people act directly is recorded with its reason")
		return
	}
	others := 0
	for _, k := range keepers {
		if !strings.EqualFold(k, email) {
			others++
		}
	}
	if others == 0 {
		// The only owner: they loosen it alone, typing the phrase, and the
		// record says nobody else agreed.
		if strings.TrimSpace(in.ConfirmSelf) != PolicySelfPhrase {
			writeErr(w, http.StatusConflict, `nobody else can approve this, so you may make the change yourself: type "`+
				PolicySelfPhrase+`" to confirm`)
			return
		}
		p, err := a.store.SetApprovalPolicy(ctx, next, email, cur.Version())
		if errors.Is(err, ErrPolicyMoved) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			a.fail(w, "approval policy", err)
			return
		}
		a.auditPolicy(ctx, "approval_policy.changed", "warning", email,
			"Loosened the approval policy alone: nobody else can change it. "+strings.Join(changes, "; ")+"."+
				reasonWords(in.Reason))
		writeJSON(w, http.StatusOK, map[string]any{"applied": true, "policy": p})
		return
	}
	c, err := a.store.AskApprovalPolicyChange(ctx, next, email, in.Reason, cur.Version())
	if errors.Is(err, ErrPolicyChangePending) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "approval policy", err)
		return
	}
	c.Changes, c.Loosens = changes, loosens
	a.auditPolicy(ctx, "approval_policy.requested", "warning", email,
		"Asked to loosen the approval policy: "+strings.Join(changes, "; ")+". Waits for another owner."+
			reasonWords(in.Reason))
	for _, k := range keepers {
		if strings.EqualFold(k, email) {
			continue
		}
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Severity: "info", Audience: AudiencePerson, Recipient: k,
			Actor: email, Title: email + " asks to loosen the approval policy",
			Detail: strings.Join(loosens, "; ") + "." + reasonWords(in.Reason),
			Link:   "/settings?section=approval-policy", Subject: "policy-change:" + c.ID})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"applied": false, "pending": c})
}

func reasonWords(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return " Why: " + strings.TrimSpace(reason)
}

func (a *API) auditPolicy(ctx context.Context, action, severity, actor, detail string) {
	if _, err := a.store.AppendAudit(ctx, AuditEvent{Action: action, Severity: severity, ActorEmail: actor,
		Target: "approval policy", Detail: detail}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
}

// POST /api/v1/approval-policy/changes/{id}/{approve,deny,cancel}
func (a *API) postPolicyChangeDecision(what string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email, _, ok := a.policyKeeper(w, r)
		if !ok {
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		if r.ContentLength > 0 {
			if err := decode(r, &in); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		in.Note = strings.TrimSpace(in.Note)
		ctx := r.Context()
		pending, err := a.store.PendingApprovalPolicyChange(ctx)
		if err != nil {
			a.fail(w, "approval policy", err)
			return
		}
		id := r.PathValue("id")
		if pending == nil || pending.ID != id {
			writeErr(w, http.StatusNotFound, ErrPolicyChangeNotFound.Error())
			return
		}
		own := strings.EqualFold(pending.RequestedBy, email)
		switch {
		case what == "cancel" && !own:
			writeErr(w, http.StatusForbidden, "only the owner who asked can take it back; deny it instead")
			return
		case what != "cancel" && own:
			writeErr(w, http.StatusForbidden, "another owner decides a change you asked for")
			return
		case what == "deny" && len(in.Note) < 5:
			writeErr(w, http.StatusBadRequest, "a denial must say why")
			return
		}
		cur, err := a.store.ApprovalPolicy(ctx)
		if err != nil {
			a.fail(w, "approval policy", err)
			return
		}
		changes := cur.Changes(pending.Proposal)
		if what == "approve" {
			locks, err := a.store.ApprovalLocks(ctx)
			if err != nil {
				a.fail(w, "approval policy", err)
				return
			}
			if why := lockedLoosening(cur, pending.Proposal, locks); why != "" {
				writeErr(w, http.StatusForbidden, why)
				return
			}
			// Applied first, under the version check, then marked decided:
			// a policy that moved meanwhile is not overwritten.
			if _, err := a.store.SetApprovalPolicy(ctx, pending.Proposal, pending.RequestedBy, pending.Base); err != nil {
				if errors.Is(err, ErrPolicyMoved) {
					writeErr(w, http.StatusConflict, err.Error())
					return
				}
				a.fail(w, "approval policy", err)
				return
			}
		}
		state := map[string]string{"approve": "approved", "deny": "denied", "cancel": "cancelled"}[what]
		c, err := a.store.CloseApprovalPolicyChange(ctx, id, state, email, in.Note)
		if errors.Is(err, ErrPolicyChangeNotFound) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			a.fail(w, "approval policy", err)
			return
		}
		switch what {
		case "approve":
			a.auditPolicy(ctx, "approval_policy.changed", "warning", email, "Approved the approval policy change asked by "+
				pending.RequestedBy+": "+strings.Join(changes, "; ")+"."+reasonWords(pending.Reason))
			a.closeAlerts(ctx, "policy-change:"+id, "Approved by "+email)
		case "deny":
			a.auditPolicy(ctx, "approval_policy.denied", "notice", email, "Denied the approval policy change asked by "+
				pending.RequestedBy+": "+in.Note)
			a.closeAlerts(ctx, "policy-change:"+id, "Denied by "+email)
		default:
			a.auditPolicy(ctx, "approval_policy.cancelled", "notice", email, "Took back their change to the approval policy.")
			a.closeAlerts(ctx, "policy-change:"+id, "Taken back by "+email)
		}
		if what != "cancel" {
			a.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Severity: "info", Audience: AudiencePerson,
				Recipient: pending.RequestedBy, Actor: email,
				Title:  email + " " + state + " your change to the approval policy",
				Detail: strings.Join(changes, "; ") + "." + reasonWords(in.Note), Link: "/settings?section=approval-policy"})
		}
		writeJSON(w, http.StatusOK, c)
	}
}

// GET /api/v1/platform/approval-locks
func (a *API) getApprovalLocks(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	locks, err := a.store.ApprovalLocks(r.Context())
	if err != nil {
		a.fail(w, "approval locks", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": ApprovalKinds, "locks": locks})
}

// PUT /api/v1/platform/approval-locks
func (a *API) putApprovalLocks(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	var in struct {
		Locks []ApprovalLock `json:"locks"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := a.store.ApprovalLocks(r.Context())
	if err != nil {
		a.fail(w, "approval locks", err)
		return
	}
	if err := a.store.SetApprovalLocks(r.Context(), in.Locks, sess.Email); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	words := func(ls []ApprovalLock) string {
		if len(ls) == 0 {
			return "none"
		}
		var out []string
		for _, l := range ls {
			info, _ := kindInfo(l.Kind)
			out = append(out, envWords(l.Env)+" · "+info.Title)
		}
		return strings.Join(out, ", ")
	}
	a.auditPolicy(r.Context(), "approval_policy.locks_changed", "notice", sess.Email,
		"Changed what no organization may loosen, from "+words(before)+" to "+words(in.Locks)+".")
	locks, err := a.store.ApprovalLocks(r.Context())
	if err != nil {
		a.fail(w, "approval locks", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": ApprovalKinds, "locks": locks, "at": time.Now().UTC()})
}

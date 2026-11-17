package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/gsoultan/argus/internal/auth"
)

// Routes for people shared across organizations; see shared_people.go.

// postSharedAccount adds a person who already signs in to another
// organization. No password: they keep the one they have.
//
// Where the caller is also an admin or owner of the person's home, naming
// that home allows this organization there and adds them here in one step.
// Otherwise the home must already have named this organization, and every
// refusal about the address itself is the same 404, whether nobody uses it,
// its home has not allowed this organization, or the account is disabled.
func (a *API) postSharedAccount(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "adding a person requires the admin or owner role")
		return
	}
	var in struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		Role        string `json:"role"`
		HomeOrg     string `json:"homeOrg"`
		TeamID      string `json:"teamId"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	email := normaliseEmail(in.Email)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		writeErr(w, http.StatusBadRequest, "enter an email address, such as you@example.com")
		return
	}
	if msg, code := roleRefusal(sess.Role, in.Role); code != 0 {
		writeErr(w, code, msg)
		return
	}
	here := TenantOf(r.Context())
	if in.HomeOrg != "" {
		slug := strings.ToLower(strings.TrimSpace(in.HomeOrg))
		if !ValidTenantSlug(slug) {
			writeErr(w, http.StatusNotFound, ErrNoShareable.Error())
			return
		}
		// One step for someone who runs the home too: allow here, there.
		if home, err := a.store.TenantBySlug(r.Context(), slug); err == nil && !home.Disabled() {
			if home.IsDefault() {
				home = defaultTenantFrom(home)
			}
			if _, err := a.store.ManagesOrganization(r.Context(), sess.Email, home); err == nil {
				if _, err := a.allowShare(r.Context(), WithTenant(r.Context(), home), sess.Email, email, here,
					"while adding them to "+here.Name); err != nil && !errors.Is(err, ErrNoAccount) &&
					!errors.Is(err, ErrSharedAccount) && !errors.Is(err, ErrNoShareable) {
					a.fail(w, "allow sharing", err)
					return
				}
			}
		}
	}
	acct, home, err := a.store.AddSharedMember(r.Context(), SharedMember{
		Email: email, DisplayName: in.DisplayName, Role: in.Role, HomeSlug: in.HomeOrg,
	})
	switch {
	case errors.Is(err, ErrAccountExists):
		writeErr(w, http.StatusConflict, email+" "+err.Error())
		return
	case errors.Is(err, ErrNoShareable):
		writeErr(w, http.StatusNotFound,
			"no account that can be added here uses this address. Their own organization has to allow "+
				here.Name+" first, under Teams, People, Organizations")
		return
	case errors.Is(err, ErrAmbiguousHome):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		a.fail(w, "add shared person", err)
		return
	}
	a.auditSharedIn(r.Context(), home, sess.Email, email, in.Role)
	team := ""
	if in.TeamID != "" {
		if err := a.store.AddTeamMember(r.Context(), in.TeamID, email, sess.Email); err != nil {
			// The person is added; the team is the part that did not happen,
			// and the console says so rather than reporting one outcome.
			team = "not added to the team: " + err.Error()
		} else {
			team = "added"
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"email": acct.Email, "role": acct.Role, "displayName": acct.DisplayName, "shared": true, "team": team,
	})
}

// roleRefusal is why the caller may not give role, or code 0.
func roleRefusal(callerRole, role string) (string, int) {
	switch role {
	case "admin", "approver", "operator", "auditor":
		return "", 0
	case "owner":
		if callerRole != "owner" {
			return "only an owner can add another owner", http.StatusForbidden
		}
		return "", 0
	}
	return fmt.Sprintf("unknown role %q", role), http.StatusBadRequest
}

// auditSharedIn records an addition in both chains: here, who was added and
// with what; at home, where its person's account is now used.
func (a *API) auditSharedIn(ctx context.Context, home *Tenant, actor, email, role string) {
	here := TenantOf(ctx)
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: "user.shared_in", Severity: "notice", ActorEmail: actor, Target: email,
		Detail: "Added " + email + ", who signs in through another organization, with role " +
			role + ". Their password and second factor are not held here.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	if _, err := a.store.AppendAudit(WithTenant(ctx, home), AuditEvent{
		Action: "user.shared_out", Severity: "notice", ActorEmail: actor, Target: email,
		Detail: email + "'s account is now also used by organization " + here.Name +
			" (" + here.Slug + "), with role " + role + " there. Taking that organization away " +
			"under Organizations, or disabling the account here, refuses them there.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
}

// allowShare names target as allowed in hctx's organization for email, and
// audits it there and in target. why ends the home's sentence.
func (a *API) allowShare(ctx, hctx context.Context, actor, email string, target *Tenant, why string) (bool, error) {
	added, err := a.store.AllowShare(hctx, actor, email, target)
	if err != nil || !added {
		return added, err
	}
	home := TenantOf(hctx)
	if _, err := a.store.AppendAudit(hctx, AuditEvent{
		Action: "user.share_allowed", Severity: "notice", ActorEmail: actor, Target: email,
		Detail: "Allowed organization " + target.Name + " (" + target.Slug + ") to have " + email + " " + why + ".",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	if _, err := a.store.AppendAudit(WithTenant(ctx, target), AuditEvent{
		Action: "user.share_allowed", Severity: "info", ActorEmail: actor, Target: email,
		Detail: "Organization " + home.Name + " allowed " + email + " to be added here.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	return true, nil
}

// targetOrg resolves an organization named in a request from ctx's
// organization. Unknown, disabled and this one are all the same 404.
func (a *API) targetOrg(ctx context.Context, slug string) (*Tenant, bool) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if !ValidTenantSlug(slug) {
		return nil, false
	}
	t, err := a.store.TenantBySlug(ctx, slug)
	if err != nil || t.Disabled() {
		return nil, false
	}
	if t.IsDefault() {
		t = defaultTenantFrom(t)
	}
	if t.ID == tenantIDOf(ctx) {
		return nil, false
	}
	return t, true
}

// adminSession is the caller when they are an admin or owner here.
func (a *API) adminSession(w http.ResponseWriter, r *http.Request, what string) (sessionInfo, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return sessionInfo{}, false
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, what+" requires the admin or owner role")
		return sessionInfo{}, false
	}
	return sessionInfo{Email: sess.Email, Role: sess.Role}, true
}

type sessionInfo struct{ Email, Role string }

// GET /api/v1/users/{email}/organizations: where one of this organization's
// own people may work, and the organizations the caller could add them to.
func (a *API) getPersonOrganizations(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.adminSession(w, r, "seeing where someone works")
	if !ok {
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	allowed, err := a.store.ShareAllowances(r.Context(), email)
	switch {
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	case errors.Is(err, ErrSharedAccount):
		writeErr(w, http.StatusBadRequest,
			email+" signs in through another organization; only that organization decides where else they work")
		return
	case err != nil:
		a.fail(w, "where they work", err)
		return
	}
	acct, _ := a.store.Account(r.Context(), email)
	taken := map[string]bool{}
	for _, al := range allowed {
		taken[al.Slug] = true
	}
	canAdd := []map[string]any{}
	if mine, err := a.store.OrganizationsFor(r.Context(), sess.Email); err == nil {
		for _, m := range mine {
			if m.ID == tenantIDOf(r.Context()) || taken[m.Slug] || (m.Role != "admin" && m.Role != "owner") {
				continue
			}
			canAdd = append(canAdd, map[string]any{"slug": m.Slug, "name": m.Name, "yourRole": m.Role})
		}
	}
	here := TenantOf(r.Context())
	role := ""
	legacy := false
	if acct != nil {
		role, legacy = acct.Role, acct.Shareable
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"home":          map[string]any{"slug": here.Slug, "name": here.Name, "role": role},
		"organizations": allowed,
		"canAddTo":      canAdd,
		// wasShareable: marked "shareable with any organization" before
		// organizations were chosen one by one, and none chosen since.
		"wasShareable": legacy && len(allowed) == 0,
	})
}

// POST /api/v1/users/{email}/organizations {org, role}: allow an organization
// to have one of this organization's people. When the caller is an admin or
// owner there too and gives a role, they are added there in the same step.
func (a *API) postPersonOrganization(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.adminSession(w, r, "choosing where someone works")
	if !ok {
		return
	}
	var in struct {
		Org  string `json:"org"`
		Role string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	target, ok := a.targetOrg(r.Context(), in.Org)
	if !ok {
		writeErr(w, http.StatusNotFound, "no organization "+in.Org+" to share with")
		return
	}
	var there *Account
	if in.Role != "" {
		var err error
		if there, err = a.store.ManagesOrganization(r.Context(), sess.Email, target); err != nil {
			writeErr(w, http.StatusForbidden,
				"you are not an admin or owner of "+target.Name+"; allow it without a role and its admin adds them")
			return
		}
		if msg, code := roleRefusal(there.Role, in.Role); code != 0 {
			writeErr(w, code, msg+" in "+target.Name)
			return
		}
	}
	why := "for its admin to add them"
	if there != nil {
		why = "and added them there"
	}
	if _, err := a.allowShare(r.Context(), r.Context(), sess.Email, email, target, why); err != nil {
		switch {
		case errors.Is(err, ErrNoAccount), errors.Is(err, ErrNoShareable):
			writeErr(w, http.StatusNotFound, "no enabled account for "+email+" signs in here")
		case errors.Is(err, ErrSharedAccount):
			writeErr(w, http.StatusBadRequest,
				email+" signs in through another organization; only that organization decides where else they work")
		default:
			a.fail(w, "allow sharing", err)
		}
		return
	}
	out := map[string]any{"org": target.Slug, "allowed": true, "added": false}
	if there != nil {
		tctx := WithTenant(r.Context(), target)
		_, home, err := a.store.AddSharedMember(tctx, SharedMember{
			Email: email, Role: in.Role, HomeSlug: TenantOf(r.Context()).Slug,
		})
		switch {
		case errors.Is(err, ErrAccountExists):
			// Already there: allowing it again is all that was needed.
			out["added"] = true
			out["already"] = true
		case err != nil:
			// Allowed but not added: say both halves.
			a.log.Error("add shared person after allowing", "error", err)
			writeErr(w, http.StatusInternalServerError, target.Name+" is allowed, but adding "+email+
				" there failed; add them from "+target.Name+"'s Add person")
			return
		default:
			a.auditSharedIn(tctx, home, sess.Email, email, in.Role)
			out["added"] = true
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /api/v1/users/{email}/organizations/{org}: take an organization
// away. The person is refused there at their next request.
func (a *API) deletePersonOrganization(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.adminSession(w, r, "choosing where someone works")
	if !ok {
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	target, ok := a.targetOrg(r.Context(), r.PathValue("org"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no organization "+r.PathValue("org"))
		return
	}
	err := a.store.RevokeShare(r.Context(), email, target)
	switch {
	case errors.Is(err, ErrAlreadyInState):
		w.WriteHeader(http.StatusNoContent)
		return
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	case errors.Is(err, ErrSharedAccount):
		writeErr(w, http.StatusBadRequest,
			email+" signs in through another organization; only that organization decides where else they work")
		return
	case err != nil:
		a.fail(w, "take organization away", err)
		return
	}
	here := TenantOf(r.Context())
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "user.share_revoked", Severity: "notice", ActorEmail: sess.Email, Target: email,
		Detail: "Took organization " + target.Name + " (" + target.Slug + ") away from " + email +
			". They can no longer sign in there.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	if _, err := a.store.AppendAudit(WithTenant(r.Context(), target), AuditEvent{
		Action: "user.share_revoked", Severity: "notice", ActorEmail: sess.Email, Target: email,
		Detail: "Organization " + here.Name + ", where " + email + " signs in, no longer allows them here. " +
			"Their sessions here end at their next request.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/v1/users/from-organizations: people of the other organizations the
// caller runs who are not here yet, for Add person.
func (a *API) getFromOrganizations(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.adminSession(w, r, "adding a person")
	if !ok {
		return
	}
	list, err := a.store.ManagedOrganizations(r.Context(), sess.Email)
	if err != nil {
		a.fail(w, "other organizations", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"organizations": list})
}

// handleSwitchOrg moves a signed-in session to another organization the same
// identity is a member of, by issuing that organization's cookie in place of
// this one's.
//
// No password is asked again: the person proved who they are at sign-in,
// and the target organization chose to have them. What the target's row says
// -- role, display name, disabled -- is what the new session carries, and
// the target refuses anyone whose credentials are not the ones it added.
func (a *API) handleSwitchOrg(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if a.signer == nil {
		writeErr(w, http.StatusNotImplemented, "this control plane cannot issue a session")
		return
	}
	var in struct {
		Org string `json:"org"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	from := TenantOf(r.Context())
	target, err := a.store.TenantBySlug(r.Context(), in.Org)
	if errors.Is(err, ErrUnknownTenant) || (err == nil && target.Disabled()) {
		// Same answer as not being a member: the switcher only ever offers
		// organizations the person is in, so anything else is a guess.
		writeErr(w, http.StatusNotFound, ErrNotMember.Error())
		return
	}
	if err != nil {
		a.fail(w, "resolve organization", err)
		return
	}
	if target.IsDefault() {
		target = defaultTenantFrom(target)
	}
	if target.ID == from.ID {
		writeJSON(w, http.StatusOK, map[string]any{"org": tenantSummary(r.Context(), from)})
		return
	}
	acct, err := a.store.MemberOf(r.Context(), sess.Email, target)
	if errors.Is(err, ErrNotMember) {
		a.RecordAuthFailure(r)
		if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
			Action: "auth.switch_refused", Severity: "warning", ActorEmail: sess.Email,
			Target: "console", Detail: "Asked to switch to an organization they are not a member of.",
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
		writeErr(w, http.StatusNotFound, ErrNotMember.Error())
		return
	}
	if err != nil {
		a.fail(w, "check membership", err)
		return
	}

	// Recorded in the organization being left before the cookie changes, so
	// a session that ends here has an ending in this chain.
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "auth.switch", Severity: "info", ActorEmail: sess.Email, Target: "console",
		Detail: "Switched to organization " + target.Name + " (" + target.Slug + ").",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	// From here on the request is the target's: its signer, its tables, its
	// audit chain. completeSignIn does the rest as for any sign-in.
	tr := r.WithContext(WithTenant(r.Context(), target))
	a.completeSignIn(w, tr, auth.Session{
		Email: acct.Email, Name: acct.DisplayName, Role: acct.Role,
	}, "switch from "+from.Slug)
}

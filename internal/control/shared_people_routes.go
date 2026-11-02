package control

import (
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
// Every refusal about the address itself is the same 404, whether nobody uses
// it, its home has not allowed sharing, or the account there is disabled.
// Only an address already in this organization is told apart, since the
// caller can see that in their own list.
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
	switch in.Role {
	case "admin", "approver", "operator", "auditor":
	case "owner":
		if sess.Role != "owner" {
			writeErr(w, http.StatusForbidden, "only an owner can add another owner")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown role %q", in.Role))
		return
	}
	if in.HomeOrg != "" && !ValidTenantSlug(strings.ToLower(strings.TrimSpace(in.HomeOrg))) {
		writeErr(w, http.StatusNotFound, ErrNoShareable.Error())
		return
	}
	acct, home, err := a.store.AddSharedMember(r.Context(), SharedMember{
		Email: email, DisplayName: in.DisplayName, Role: in.Role, HomeSlug: in.HomeOrg,
	})
	switch {
	case errors.Is(err, ErrAccountExists):
		writeErr(w, http.StatusConflict, email+" "+err.Error())
		return
	case errors.Is(err, ErrNoShareable):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrAmbiguousHome):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		a.fail(w, "add shared person", err)
		return
	}
	here := TenantOf(r.Context())
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "user.shared_in", Severity: "notice", ActorEmail: sess.Email, Target: email,
		Detail: "Added " + email + ", who signs in through another organization, with role " +
			in.Role + ". Their password and second factor are not held here.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	// The home organization's chain records where its person's account is
	// now used. Its people can see this organization's name; the inviting
	// organization is told nothing about the home beyond that one exists.
	if _, err := a.store.AppendAudit(WithTenant(r.Context(), home), AuditEvent{
		Action: "user.shared_out", Severity: "notice", ActorEmail: sess.Email, Target: email,
		Detail: email + "'s account is now also used by organization " + here.Name +
			" (" + here.Slug + "), with role " + in.Role + " there. Disabling the account here " +
			"refuses them there too.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"email": acct.Email, "role": acct.Role, "displayName": acct.DisplayName, "shared": true,
	})
}

// putAccountShareable sets whether other organizations may add one of this
// organization's people.
func (a *API) putAccountShareable(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "changing who may be shared requires the admin or owner role")
		return
	}
	var in struct {
		Shareable bool `json:"shareable"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	email := normaliseEmail(r.PathValue("email"))
	err := a.store.SetAccountShareable(r.Context(), email, in.Shareable)
	switch {
	case errors.Is(err, ErrAlreadyInState):
		writeJSON(w, http.StatusOK, map[string]any{"email": email, "shareable": in.Shareable})
		return
	case errors.Is(err, ErrNoAccount):
		writeErr(w, http.StatusNotFound, "no account for "+email)
		return
	case errors.Is(err, ErrSharedAccount):
		writeErr(w, http.StatusBadRequest,
			email+" signs in through another organization; only that organization decides who may share them")
		return
	case err != nil:
		a.fail(w, "change sharing", err)
		return
	}
	verb, detail := "account.shareable", "Other organizations may now add "+email+" without a new password."
	if !in.Shareable {
		verb, detail = "account.not_shareable", "Other organizations may no longer add "+email+
			". Those that already have them keep them."
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: verb, Severity: "notice", ActorEmail: sess.Email, Target: email, Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"email": email, "shareable": in.Shareable})
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

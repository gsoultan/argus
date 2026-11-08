package control

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"sync"

	"github.com/gsoultan/argus/internal/auth"
)

// Which organization a request is about, decided once, before any handler.
//
// Every route runs inside scope, which puts exactly one organization on the
// request context. Handlers and the store never choose: a cookie, a ticket, an
// agent credential or a sign-in form names an organization, scope looks it up
// in the registry, and the store runs the request on that organization's
// connection. A request that names an organization the registry does not
// have, or one that is disabled, is answered here and reaches nothing.

// OrgHeader is how an unauthenticated request -- a sign-in, an enrolment --
// says which organization it means. Anything authenticated carries the
// organization inside its credential instead, and the header is ignored.
const OrgHeader = "X-Argus-Org"

// scope resolves the request's organization.
func (a *API) scope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, slug, ok := claimedTenant(r)
		if !ok {
			// A credential shaped like Argus's but naming an organization no
			// id could. Not looked up, so the registry is not an oracle.
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var (
			t   *Tenant
			err error
		)
		switch {
		case a.store == nil:
			// No database at all, as in the handler tests: there is only
			// the default organization to be in.
			t = defaultTenant()
		case id != "" && id != DefaultTenantID:
			t, err = a.store.TenantByID(r.Context(), id)
		case slug != "" && slug != DefaultTenantID:
			t, err = a.store.TenantBySlug(r.Context(), slug)
		default:
			t = a.store.DefaultTenant(r.Context())
		}
		switch {
		case errors.Is(err, ErrUnknownTenant) && slug != "":
			// Typed by a person at the sign-in form: say so.
			writeErr(w, http.StatusNotFound, "no organization is called "+slug)
			return
		case errors.Is(err, ErrUnknownTenant):
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		case err != nil:
			a.fail(w, "resolve organization", err)
			return
		}
		if t.Disabled() {
			writeErr(w, http.StatusForbidden, ErrTenantDisabled.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(WithTenant(r.Context(), t)))
	})
}

// claimedTenant reads which organization a request says it is about.
//
// In order: an agent credential or enrolment token (its prefix), a signed
// value (cookie, ticket, challenge: its org claim), then the header a sign-in
// sends. The claim picks what to verify against; it is believed only once the
// credential that carried it has been.
func claimedTenant(r *http.Request) (id, slug string, ok bool) {
	tok := bearer(r)
	if tok == "" {
		tok = r.URL.Query().Get("ticket")
	}
	if strings.HasPrefix(tok, agentCredentialPrefix) || strings.HasPrefix(tok, enrollTokenPrefix) {
		id, ok = secretTenant(tok)
		return id, "", ok
	}
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		if id = auth.PeekTenant(c.Value); id != "" {
			return id, "", validTenantID(id)
		}
	}
	if tok != "" {
		if id = auth.PeekTenant(tok); id != "" {
			return id, "", validTenantID(id)
		}
	}
	if h := strings.ToLower(strings.TrimSpace(r.Header.Get(OrgHeader))); h != "" {
		return "", h, true
	}
	return "", "", true
}

// scopeSecret is scope for a connection that is not an HTTP request: the
// QUIC tunnel, which carries its credential in its hello. Same rule, same
// registry, same refusals.
func (a *API) scopeSecret(ctx context.Context, secret string) (context.Context, bool) {
	id, ok := secretTenant(secret)
	if !ok {
		return ctx, false
	}
	if id == DefaultTenantID {
		return WithTenant(ctx, defaultTenant()), true
	}
	t, err := a.store.TenantByID(ctx, id)
	if err != nil || t.Disabled() {
		return ctx, false
	}
	return WithTenant(ctx, t), true
}

// secretTenant reads the organization prefix off an agent credential or an
// enrolment token: "argus-agent-<id>.<secret>". No prefix means the default
// organization, which is what every secret minted before there were several
// looks like; the secret part has no '.' in it, so the two cannot be confused.
func secretTenant(secret string) (string, bool) {
	rest := secret
	for _, p := range []string{agentCredentialPrefix, enrollTokenPrefix} {
		if strings.HasPrefix(secret, p) {
			rest = secret[len(p):]
			break
		}
	}
	id, _, found := strings.Cut(rest, ".")
	if !found {
		return DefaultTenantID, true
	}
	if !validTenantID(id) || id == DefaultTenantID {
		return "", false
	}
	return id, true
}

// secretPrefixFor is the routing prefix a secret minted in ctx's organization
// carries, so the control plane knows where to look it up.
func secretPrefixFor(id string) string {
	if id == "" || id == DefaultTenantID {
		return ""
	}
	return id + "."
}

// signer returns the signer for the request's organization.
func (a *API) signerFor(r *http.Request) *auth.Signer {
	if a.signer == nil {
		return nil
	}
	id := tenantIDOf(r.Context())
	if id == DefaultTenantID {
		return a.signer
	}
	a.signers.mu.Lock()
	defer a.signers.mu.Unlock()
	if s, ok := a.signers.m[id]; ok {
		return s
	}
	if a.signers.m == nil {
		a.signers.m = map[string]*auth.Signer{}
	}
	s := a.signer.ForTenant(id)
	a.signers.m[id] = s
	return s
}

// tenantSigners caches derived signers. Bounded by the registry, which only
// an administrator adds to.
type tenantSigners struct {
	mu sync.Mutex
	m  map[string]*auth.Signer
}

/* ── Platform routes ─────────────────────────────────────────────────────── */

// platformOwner gates what only the platform's own administrators may do:
// an owner signed in to the default organization. Nobody else, whatever their
// role where they are, can see that other organizations exist.
func (a *API) platformOwner(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return sess, false
	}
	if tenantIDOf(r.Context()) != DefaultTenantID || !managesPlatform(sess.Role) {
		// Not found, not forbidden: an organization's own admin learns nothing
		// about whether a platform exists above them.
		writeErr(w, http.StatusNotFound, "not found")
		return sess, false
	}
	return sess, true
}

// managesPlatform is the role check behind platformOwner. Admins too, not
// owners only: first-run setup makes the install's first account an admin,
// and a platform whose own administrator cannot see its organizations has
// shipped a feature nobody can reach.
func managesPlatform(role string) bool { return role == "owner" || role == "admin" }

type tenantView struct {
	Tenant
	Default bool          `json:"default"`
	Counts  *TenantCounts `json:"counts,omitempty"`
}

func (a *API) getTenants(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	list, err := a.store.Tenants(r.Context())
	if err != nil {
		a.fail(w, "list organizations", err)
		return
	}
	out := make([]tenantView, 0, len(list))
	for i := range list {
		v := tenantView{Tenant: list[i], Default: list[i].IsDefault()}
		if !list[i].Disabled() {
			if c, err := a.store.CountsFor(r.Context(), &list[i]); err == nil {
				v.Counts = &c
			} else {
				a.log.Warn("organization counts unavailable", "tenant", list[i].ID, "error", err)
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) postTenant(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	var in struct {
		Slug          string `json:"slug"`
		Name          string `json:"name"`
		OwnerEmail    string `json:"ownerEmail"`
		OwnerName     string `json:"ownerName"`
		OwnerPassword string `json:"ownerPassword"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The first owner is optional. Left out, the administrator creating the
	// organization is its owner, signing in with the account they already
	// have, and no second password exists. Named, a new account is made for
	// that person, with a password the administrator passes on.
	email := normaliseEmail(in.OwnerEmail)
	if email != "" {
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
			writeErr(w, http.StatusBadRequest, "enter the owner's email address, such as you@example.com")
			return
		}
		if len([]rune(in.OwnerPassword)) < auth.MinPasswordLength {
			writeErr(w, http.StatusBadRequest,
				"the owner's password must be at least "+itoa(auth.MinPasswordLength)+" characters")
			return
		}
	}
	t, err := a.store.CreateTenant(r.Context(), NewTenant{
		Slug: in.Slug, Name: in.Name, OwnerEmail: email,
		OwnerName: strings.TrimSpace(in.OwnerName), OwnerPassword: in.OwnerPassword,
	}, sess.Email)
	switch {
	case errors.Is(err, ErrSlugTaken):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil && t == nil:
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		a.fail(w, "create organization", err)
		return
	}
	// The administrator who created it becomes one of its owners, signing in
	// with the account they already have, so it appears in their organization
	// menu at once. When nobody else was named they are its only owner, so
	// failing to add them is an error rather than a warning.
	if err := a.joinCreatedTenant(r, sess, t); err != nil {
		if email == "" {
			a.fail(w, "add you as the new organization's owner", err)
			return
		}
		a.log.Warn("creator not added to the new organization", "tenant", t.ID, "error", err)
	}
	// The platform's own chain records it too, so both sides can show who
	// created the organization and when.
	detail := "Organization " + t.Name + " (" + t.Slug + ") created, with " + sess.Email + " as its owner."
	if email != "" {
		detail = "Organization " + t.Name + " (" + t.Slug + ") created, with " + email +
			" as its first owner."
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "tenant.created", Severity: "notice", ActorEmail: sess.Email, Target: t.Slug,
		Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	writeJSON(w, http.StatusCreated, t)
}

// postTenantName renames an organization. Only its display name: the short
// name people sign in with stays, so nothing anyone has saved stops working.
func (a *API) postTenantName(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	before, err := a.tenantForRename(r, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, ErrUnknownTenant.Error())
		return
	}
	t, err := a.store.RenameTenant(r.Context(), id, in.Name)
	switch {
	case errors.Is(err, errInvalidTenantName):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ErrUnknownTenant):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		a.fail(w, "rename organization", err)
		return
	}
	if before.Name != t.Name {
		ev := AuditEvent{
			Action: "tenant.renamed", Severity: "notice", ActorEmail: sess.Email, Target: t.Slug,
			Detail: "Organization " + before.Name + " (" + t.Slug + ") renamed to " + t.Name + ".",
		}
		// The platform's chain, and the organization's own, so its people
		// can see why the name in their console changed.
		if _, err := a.store.AppendAudit(r.Context(), ev); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
		if t.ID != DefaultTenantID {
			if _, err := a.store.AppendAudit(WithTenant(r.Context(), t), ev); err != nil {
				a.log.Error("audit append failed", "tenant", t.ID, "error", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, t)
}

// tenantForRename reads an organization as it is before a rename, the
// default one included.
func (a *API) tenantForRename(r *http.Request, id string) (*Tenant, error) {
	if id == DefaultTenantID {
		return a.store.DefaultTenant(r.Context()), nil
	}
	return a.store.TenantByID(r.Context(), id)
}

func (a *API) postTenantDisabled(disabled bool) http.HandlerFunc {
	verb, action := "enabled", "tenant.enabled"
	if disabled {
		verb, action = "disabled", "tenant.disabled"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.platformOwner(w, r)
		if !ok {
			return
		}
		t, err := a.store.SetTenantDisabled(r.Context(), r.PathValue("id"), disabled, sess.Email)
		switch {
		case errors.Is(err, ErrUnknownTenant):
			writeErr(w, http.StatusNotFound, err.Error())
			return
		case err != nil && strings.Contains(err.Error(), "cannot be disabled"):
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			a.fail(w, "update organization", err)
			return
		}
		if disabled && a.Tunnels != nil {
			// Its agents are closed now rather than at the next ping; a
			// disabled organization's servers should not stay reachable for
			// up to a minute.
			a.Tunnels.closeTenant(t.ID)
		}
		if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
			Action: action, Severity: "warning", ActorEmail: sess.Email, Target: t.Slug,
			Detail: "Organization " + t.Name + " (" + t.Slug + ") " + verb + ".",
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
		writeJSON(w, http.StatusOK, t)
	}
}

// joinCreatedTenant adds the creating administrator to t as an owner, shared
// from the default organization.
func (a *API) joinCreatedTenant(r *http.Request, sess auth.Session, t *Tenant) error {
	ctx := r.Context()
	if err := a.store.SetAccountShareable(ctx, sess.Email, true); err != nil &&
		!errors.Is(err, ErrAlreadyInState) {
		return err
	}
	tctx := WithTenant(ctx, t)
	if _, _, err := a.store.AddSharedMember(tctx, SharedMember{
		Email: sess.Email, DisplayName: sess.Name, Role: "owner", HomeSlug: DefaultTenantID,
	}); err != nil && !errors.Is(err, ErrAccountExists) {
		return err
	}
	_, err := a.store.AppendAudit(tctx, AuditEvent{
		Action: "user.shared_in", Severity: "notice", ActorEmail: sess.Email, Target: sess.Email,
		Detail: sess.Email + ", who created this organization on the platform, was added as an owner. " +
			"They sign in with their platform account; remove them here to revoke that.",
	})
	return err
}

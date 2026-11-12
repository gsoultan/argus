package control

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
The identities argus-control starts from in a cloud, before it steps into the
role in a connected account. Read plans/cloud-identities-multi/design.md in
the project files.

They come from the config file, never the database: the secrets stay on the
control host. Each has its own provider, so the assumed-role sessions it
caches are its own. Each account names the identity it was connected with
and keeps it until someone moves it; changing the default moves nothing.
An organization may only use the identities made available to it, and an
account whose identity is gone or no longer available is not reached at all
rather than with another one.
*/

// CloudIdentity is one identity for one provider.
type CloudIdentity struct {
	ID   string
	Name string
	// Default is the one a new connection starts with.
	Default bool
	// Organizations may use it, by id; empty is every organization.
	Organizations []string
	Provider      cloud.Provider
}

// allows reports whether an organization may use the identity.
func (i CloudIdentity) allows(tenantID string) bool {
	return len(i.Organizations) == 0 || slices.Contains(i.Organizations, tenantID)
}

// DefaultCloudIdentity is the id an install's one identity had before there
// could be several, and the one every account connected then uses.
const DefaultCloudIdentity = "main"

// SetCloudIdentities makes a cloud available under Cloud with these
// identities, in the order the console lists them. Identities with a nil
// provider are left out, since a nil provider is never stored.
func (a *API) SetCloudIdentities(kind string, list []CloudIdentity) {
	keep := make([]CloudIdentity, 0, len(list))
	for _, i := range list {
		if i.Provider != nil && i.ID != "" && len(keep) < MaxCloudIdentities {
			keep = append(keep, i)
		}
	}
	a.cloud.mu.Lock()
	defer a.cloud.mu.Unlock()
	if a.cloud.identities == nil {
		a.cloud.identities = map[string][]CloudIdentity{}
	}
	if len(keep) == 0 {
		delete(a.cloud.identities, kind)
		return
	}
	a.cloud.identities[kind] = keep
}

// SetCloudProvider makes a cloud available with one identity, Main, that
// every organization may use. A nil provider is never stored.
func (a *API) SetCloudProvider(p cloud.Provider) {
	if p == nil {
		return
	}
	a.SetCloudIdentities(p.Kind(), []CloudIdentity{{ID: DefaultCloudIdentity, Name: "Main", Default: true, Provider: p}})
}

// cloudSupports reports whether a provider is configured at all.
func (a *API) cloudSupports(kind string) bool {
	a.cloud.mu.Lock()
	defer a.cloud.mu.Unlock()
	return len(a.cloud.identities[kind]) > 0
}

// cloudIdentitiesFor is the identities of a provider the organization on
// ctx may use, in order.
func (a *API) cloudIdentitiesFor(ctx context.Context, kind string) []CloudIdentity {
	org := tenantIDOf(ctx)
	a.cloud.mu.Lock()
	defer a.cloud.mu.Unlock()
	var out []CloudIdentity
	for _, i := range a.cloud.identities[kind] {
		if i.allows(org) {
			out = append(out, i)
		}
	}
	return out
}

// allCloudIdentities is every identity of a provider, for the platform's
// own view.
func (a *API) allCloudIdentities(kind string) []CloudIdentity {
	a.cloud.mu.Lock()
	defer a.cloud.mu.Unlock()
	return slices.Clone(a.cloud.identities[kind])
}

// errCloudIdentity is an identity the organization cannot use, or none to
// choose from.
type errCloudIdentity struct {
	code int
	msg  string
}

func (e *errCloudIdentity) Error() string { return e.msg }

// identityStatus is the HTTP status for an error from cloudIdentity.
func identityStatus(err error) int {
	var e *errCloudIdentity
	if errors.As(err, &e) {
		return e.code
	}
	return http.StatusInternalServerError
}

// cloudIdentity is the identity id names, for the organization on ctx. An
// empty id is the organization's default: the platform's default when the
// organization may use it, else the only one it may use. An identity the
// organization may not use is answered the same as one that does not
// exist, so an organization learns nothing about the others'.
func (a *API) cloudIdentity(ctx context.Context, kind, id string) (CloudIdentity, error) {
	if !a.cloudSupports(kind) {
		return CloudIdentity{}, &errCloudIdentity{http.StatusNotImplemented,
			"this control plane was started without " + kind + " support"}
	}
	list := a.cloudIdentitiesFor(ctx, kind)
	if id != "" {
		for _, i := range list {
			if i.ID == id {
				return i, nil
			}
		}
		return CloudIdentity{}, &errCloudIdentity{http.StatusForbidden,
			"there is no identity \"" + id + "\" this organization may use; pick one of those listed"}
	}
	for _, i := range list {
		if i.Default {
			return i, nil
		}
	}
	switch len(list) {
	case 0:
		return CloudIdentity{}, &errCloudIdentity{http.StatusConflict,
			"no " + kind + " identity is available to this organization yet. The platform's owner makes one available from the default organization's Settings › Cloud identities"}
	case 1:
		return list[0], nil
	}
	return CloudIdentity{}, &errCloudIdentity{http.StatusBadRequest, "choose which identity Argus connects as"}
}

// cloudProviderFor is the provider for the identity an account uses. An
// identity that is gone, or no longer available to the organization, is an
// error: the account is never reached with another identity instead.
func (a *API) cloudProviderFor(ctx context.Context, c CloudAccount) (cloud.Provider, error) {
	id := c.Identity
	if id == "" {
		id = DefaultCloudIdentity
	}
	i, err := a.cloudIdentity(ctx, c.Provider, id)
	if err != nil {
		if identityStatus(err) == http.StatusForbidden {
			return nil, errors.New("the identity " + c.Name + " uses (" + id + ") is no longer configured for this organization. " +
				"Move the account to another identity with Change identity on its row under Cloud")
		}
		return nil, err
	}
	return i.Provider, nil
}

// cloudBase is what a provider says about one identity, cached for a few
// minutes: finding no identity can mean waiting on the instance metadata
// service, which is not something every page load should do.
func (a *API) cloudBase(ctx context.Context, kind string, i CloudIdentity) CloudBase {
	key := kind + ":" + i.ID
	a.cloud.mu.Lock()
	c, ok := a.cloud.bases[key]
	a.cloud.mu.Unlock()
	if ok && time.Since(c.at) < cloudBaseTTL {
		c.base.Default = i.Default
		c.base.Name = i.Name
		return c.base
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := CloudBase{Provider: kind, Identity: i.ID, Name: i.Name, Default: i.Default}
	if b, err := i.Provider.Base(ctx); err != nil {
		out.Error = err.Error()
	} else {
		out.Principal, out.Source = b.Principal, b.Source
	}
	a.cloud.mu.Lock()
	if a.cloud.bases == nil {
		a.cloud.bases = map[string]cachedBase{}
	}
	a.cloud.bases[key] = cachedBase{base: out, at: time.Now()}
	a.cloud.mu.Unlock()
	return out
}

// cloudBases asks about every identity in list at once, in list's order.
// The list is at most MaxCloudIdentities long.
func (a *API) cloudBases(ctx context.Context, kind string, list []CloudIdentity) []CloudBase {
	out := make([]CloudBase, len(list))
	var wg sync.WaitGroup
	for n, i := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[n] = a.cloudBase(ctx, kind, i)
		}()
	}
	wg.Wait()
	return out
}

// CloudIdentityUse is how many accounts in one organization use an
// identity, for the platform's owner deciding whether it can go.
type CloudIdentityUse struct {
	Organization string `json:"organization"`
	Name         string `json:"name"`
	Accounts     int    `json:"accounts"`
}

// CloudIdentityStatus is one identity as the platform's settings page shows
// it: what AWS says about it, and who uses it.
type CloudIdentityStatus struct {
	CloudBase
	Use []CloudIdentityUse `json:"use"`
}

// cloudIdentityUse counts the accounts on each identity in every
// organization. Counts and organization names only: no account of one
// organization is read into another's view.
func (a *API) cloudIdentityUse(ctx context.Context, kind string) (map[string][]CloudIdentityUse, error) {
	out := map[string][]CloudIdentityUse{}
	var ferr error
	err := a.store.ForEachTenant(PlatformContext(ctx), func(tctx context.Context, t *Tenant) {
		counts, err := a.store.CloudAccountsByIdentity(tctx, kind)
		if err != nil {
			ferr = err
			return
		}
		for id, n := range counts {
			out[id] = append(out[id], CloudIdentityUse{Organization: t.ID, Name: t.Name, Accounts: n})
		}
	})
	if err == nil {
		err = ferr
	}
	return out, err
}

// getCloudIdentityStatus is every identity with its answer from AWS and
// how many accounts in each organization use it: the platform's view, so
// only the default organization's administrators see it.
func (a *API) getCloudIdentityStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.admin(w, r, "reading the cloud identities"); !ok {
		return
	}
	if !platformScope(r.Context()) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	kind := r.PathValue("provider")
	list := a.allCloudIdentities(kind)
	use, err := a.cloudIdentityUse(r.Context(), kind)
	if err != nil {
		a.fail(w, "cloud identity use", err)
		return
	}
	bases := a.cloudBases(r.Context(), kind, list)
	out := make([]CloudIdentityStatus, 0, len(list))
	for n, b := range bases {
		u := use[list[n].ID]
		if u == nil {
			u = []CloudIdentityUse{}
		}
		out = append(out, CloudIdentityStatus{CloudBase: b, Use: u})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// refuseCloudIdentity audits a request that named an identity this
// organization may not use. Other refusals (none configured, none chosen)
// are the person's to fix and are not audited.
func (a *API) refuseCloudIdentity(r *http.Request, actor, target, id string, err error) {
	if identityStatus(err) != http.StatusForbidden {
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.identity.refused", Severity: "notice", ActorEmail: actor, Target: target,
		Detail: "Asked for the identity \"" + id + "\", which this organization may not use."})
}

type cloudMoveIn struct {
	Identity string `json:"identity"`
}

// cloudMove checks that Argus gets into an account's roles with another
// identity. It returns the identity and who Argus is in the account then.
func (a *API) cloudMove(r *http.Request, actor string) (CloudAccount, CloudIdentity, cloud.Identity, int, error) {
	c, err := a.store.CloudAccount(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrCloudAccountNotFound) {
		return c, CloudIdentity{}, cloud.Identity{}, http.StatusNotFound, err
	}
	if err != nil {
		return c, CloudIdentity{}, cloud.Identity{}, http.StatusInternalServerError, err
	}
	var in cloudMoveIn
	if err := decode(r, &in); err != nil {
		return c, CloudIdentity{}, cloud.Identity{}, http.StatusBadRequest, err
	}
	if in.Identity == "" {
		return c, CloudIdentity{}, cloud.Identity{}, http.StatusBadRequest, errors.New("choose the identity to move to")
	}
	if in.Identity == c.Identity {
		return c, CloudIdentity{}, cloud.Identity{}, http.StatusBadRequest, errors.New(c.Name + " already uses that identity")
	}
	ident, err := a.cloudIdentity(r.Context(), c.Provider, in.Identity)
	if err != nil {
		a.refuseCloudIdentity(r, actor, cloudTarget(c.Provider, c.AccountID, c.Name), in.Identity, err)
		return c, CloudIdentity{}, cloud.Identity{}, identityStatus(err), err
	}
	ext, err := a.cloudExternalID(r.Context(), c.Provider, c.AccountID)
	if err != nil {
		return c, ident, cloud.Identity{}, http.StatusInternalServerError, err
	}
	vctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	acct := cloud.Account{Provider: c.Provider, AccountID: c.AccountID, RoleARN: c.RoleARN, ExternalID: ext}
	who, err := ident.Provider.Verify(vctx, acct, cloud.Actor(actor))
	if err != nil {
		return c, ident, cloud.Identity{}, http.StatusConflict, errors.New("the read role does not let " + ident.Name + " in yet: " + err.Error())
	}
	if c.ManageRoleARN != "" {
		acct.RoleARN = c.ManageRoleARN
		if _, err := ident.Provider.Verify(vctx, acct, cloud.Actor(actor)); err != nil {
			return c, ident, cloud.Identity{}, http.StatusConflict, errors.New("the read role works; the manage role does not let " + ident.Name + " in yet: " + err.Error())
		}
	}
	return c, ident, who, http.StatusOK, nil
}

// postCloudMoveCheck says whether an account's roles let another identity
// in yet, without moving anything. The page asks every few seconds while
// the person updates the stack, so nothing is recorded.
func (a *API) postCloudMoveCheck(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "changing a cloud account's identity")
	if !ok {
		return
	}
	_, _, _, code, err := a.cloudMove(r, actor)
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case code == http.StatusConflict:
		writeJSON(w, http.StatusOK, CloudCheck{Detail: err.Error()})
	case err != nil:
		writeErr(w, code, err.Error())
	default:
		writeJSON(w, http.StatusOK, CloudCheck{Ready: true})
	}
}

// putCloudIdentity moves an account to another identity, only once Argus
// has got into its roles with it: a move never leaves an account Argus
// cannot reach.
func (a *API) putCloudIdentity(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "changing a cloud account's identity")
	if !ok {
		return
	}
	c, ident, who, code, err := a.cloudMove(r, actor)
	if err != nil {
		if code == http.StatusConflict {
			a.auditAsset(r, AuditEvent{Action: "cloud.account.refused", Severity: "notice", ActorEmail: actor,
				Target: cloudTarget(c.Provider, c.AccountID, c.Name),
				Detail: "Could not move to the identity " + ident.Name + " (" + ident.ID + "): " + err.Error() + ". It stays on " + c.Identity + "."})
		}
		writeErr(w, code, err.Error())
		return
	}
	from := c.Identity
	c, err = a.store.SetCloudAccountIdentity(r.Context(), c.ID, ident.ID, who.ARN)
	if err != nil {
		a.fail(w, "move cloud account", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.account.identity_changed", Severity: "notice", ActorEmail: actor,
		Target: cloudTarget(c.Provider, c.AccountID, c.Name),
		Detail: "Moved from the identity " + from + " to " + ident.Name + " (" + ident.ID + "), reading as " + who.ARN +
			". The roles may still trust the old identity until their stack is updated with the template for " + ident.Name + " alone."})
	a.syncCloudAccountSoon(r.Context(), c)
	writeJSON(w, http.StatusOK, c)
}

// cloudIdentitiesInUse refuses a save that would take an identity away from
// accounts that use it: removing it, or no longer letting an organization
// with accounts on it use it. It answers with what to do, or "".
func (a *API) cloudIdentitiesInUse(ctx context.Context, before, next *CloudSettings) (string, error) {
	if before == nil || next == nil {
		return "", nil
	}
	use, err := a.cloudIdentityUse(ctx, "aws")
	if err != nil {
		return "", err
	}
	kept := map[string]AWSIdentitySettings{}
	for _, i := range next.AWS.Identities {
		if i.ID != "" {
			kept[i.ID] = i
		}
	}
	names := map[string]string{}
	for _, i := range before.AWS.Identities {
		names[i.ID] = i.Name
	}
	var problems []string
	for _, i := range before.AWS.Identities {
		var lost []string
		k, ok := kept[i.ID]
		for _, u := range use[i.ID] {
			if !ok || (len(k.Organizations) > 0 && !slices.Contains(k.Organizations, u.Organization)) {
				lost = append(lost, fmt.Sprintf("%s (%d)", u.Name, u.Accounts))
			}
		}
		if len(lost) == 0 {
			continue
		}
		what := "remove " + i.Name
		if ok {
			what = "take " + i.Name + " away from " + strings.Join(lost, ", ")
		}
		problems = append(problems, fmt.Sprintf("cannot %s: accounts in %s still use it", what, strings.Join(lost, ", ")))
	}
	if len(problems) == 0 {
		return "", nil
	}
	return strings.Join(problems, "; ") + ". Move those accounts to another identity first, with Change identity on each account's row under Cloud in its organization.", nil
}

// auditCloudIdentities records what a save did to the identities, one
// event each, so the chain says which identity changed and not only that
// the settings did.
func (a *API) auditCloudIdentities(r *http.Request, actor string, before, next *CloudSettings) {
	if before == nil || next == nil || next.AWS.Identities == nil {
		return
	}
	old := map[string]AWSIdentitySettings{}
	for _, i := range before.AWS.Identities {
		old[i.ID] = i
	}
	seen := map[string]bool{}
	ev := func(action, target, detail string) {
		a.auditAsset(r, AuditEvent{Action: action, Severity: "notice", ActorEmail: actor, Target: target, Detail: detail})
	}
	for _, i := range next.AWS.Identities {
		seen[i.ID] = true
		target := "cloud identity aws/" + cmp.Or(i.ID, i.Name)
		p, ok := old[i.ID]
		switch {
		case !ok:
			ev("cloud.identity.added", target, "Added the AWS identity "+i.Name+" ("+describeIdentity(i)+").")
		case p.Name != i.Name || p.Source != i.Source || p.AccessKeyID != i.AccessKeyID || i.SecretAccessKey != nil ||
			p.Profile != i.Profile || p.CredentialsFile != i.CredentialsFile || p.HomeRegion != i.HomeRegion ||
			!slices.Equal(p.Organizations, i.Organizations):
			ev("cloud.identity.changed", target, "Changed the AWS identity "+i.Name+": now "+describeIdentity(i)+".")
		}
	}
	for _, p := range before.AWS.Identities {
		if !seen[p.ID] {
			ev("cloud.identity.removed", "cloud identity aws/"+p.ID, "Removed the AWS identity "+p.Name+"; no account used it.")
		}
	}
	if before.AWS.Default != next.AWS.Default && next.AWS.Default != "" {
		ev("cloud.identity.default_changed", "cloud identity aws/"+next.AWS.Default,
			"New AWS connections now start with "+next.AWS.Default+" instead of "+before.AWS.Default+". No account moved.")
	}
}

// describeIdentity is where an identity comes from and who may use it, in
// words, with nothing secret.
func describeIdentity(i AWSIdentitySettings) string {
	var from string
	switch i.Source {
	case IdentityKey:
		from = "the access key " + i.AccessKeyID
	case IdentityFile:
		from = strings.TrimSpace("the profile " + i.Profile + " " + i.CredentialsFile)
	default:
		from = "where argus-control runs"
	}
	who := "every organization"
	if len(i.Organizations) > 0 {
		who = "only " + strings.Join(i.Organizations, ", ")
	}
	return "from " + from + ", for " + who
}

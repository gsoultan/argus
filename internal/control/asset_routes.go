package control

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gsoultan/argus/internal/auth"
)

/*
The inventory, as an administrator edits it.

Creating an asset and assigning it are changes to what Argus manages and to who
can reach it, so both are admin-or-owner and both are in the audit chain. An
ordinary account reads this surface and sees only what it was assigned: the
filtering is done here, on the server, because a console that fetched the fleet
and hid most of it would be handing every host to anyone with a debugger.
*/

// privileged answers the request itself when the role is not allowed to make
// this change, and reports whether the handler may continue.
//
// The refusal names the action rather than the route, so the console can show
// it verbatim and the person reading it knows what to ask for.
func (a *API) privileged(
	w http.ResponseWriter, sess auth.Session, action string, roles ...string,
) bool {
	for _, role := range roles {
		if sess.Role == role {
			return true
		}
	}
	writeErr(w, http.StatusForbidden, action+" requires the "+humanRoles(roles)+" role")
	return false
}

func humanRoles(roles []string) string {
	switch len(roles) {
	case 0:
		return ""
	case 1:
		return roles[0]
	default:
		return strings.Join(roles[:len(roles)-1], ", ") + " or " + roles[len(roles)-1]
	}
}

// getAssets serves the inventory, filtered to what the caller may see.
//
// An administrator, an owner or an auditor sees the fleet. Everyone else sees
// the assets assigned to them, each one carrying only the principals they may
// actually assume — so the Connect page offers accounts that will work rather
// than every account the host has.
func (a *API) getAssets(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var (
		assets []Asset
		err    error
	)
	if seesWholeFleet(sess.Role) {
		assets, err = a.store.Assets(r.Context())
	} else {
		assets, err = a.store.AssetsForUser(r.Context(), sess.Email)
	}
	if err != nil {
		a.fail(w, "assets", err)
		return
	}
	writeJSON(w, http.StatusOK, assets)
}

// seesWholeFleet is the read rule, in one place.
//
// An auditor is included deliberately: their job is to answer "who can reach
// what", and an auditor who could only see their own assignments could not do
// it. They cannot change any of it — every write below refuses them.
func seesWholeFleet(role string) bool {
	return role == "admin" || role == "owner" || role == "auditor"
}

func (a *API) postAssetCreate(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "adding an asset", "admin", "owner") {
		return
	}

	var in AssetInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	asset, err := a.store.CreateAsset(r.Context(), in)
	if err != nil {
		a.failAsset(w, "create asset", err)
		return
	}

	a.auditAsset(r, AuditEvent{
		Action:     "asset.created",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail: "Added " + asset.Hostname + " (" + asset.Protocol + ") to the inventory as " +
			describePrincipals(asset.Principals) + ". Nobody is assigned to it yet.",
	})
	a.log.Info("asset created", "hostname", asset.Hostname, "actor", sess.Email,
		"protocol", asset.Protocol, "credentialMode", asset.CredentialMode)
	writeJSON(w, http.StatusOK, asset)
}

func (a *API) patchAsset(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "editing an asset", "admin", "owner") {
		return
	}

	id := r.PathValue("id")
	before, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}

	var in AssetInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	asset, err := a.store.UpdateAsset(r.Context(), id, in)
	if err != nil {
		a.failAsset(w, "update asset", err)
		return
	}

	// Principals are the part of an edit that changes who can do what, so the
	// audit entry says what they were as well as what they are. Reconstructing
	// that from a sequence of "asset updated" entries is not possible.
	detail := "Edited " + asset.Hostname + "."
	if !sameStrings(before.Principals, asset.Principals) {
		detail = "Changed the principals on " + asset.Hostname + " from " +
			describePrincipals(before.Principals) + " to " +
			describePrincipals(asset.Principals) + "."
	}
	a.auditAsset(r, AuditEvent{
		Action:     "asset.updated",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail:     detail,
	})
	writeJSON(w, http.StatusOK, asset)
}

func (a *API) deleteAsset(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "retiring an asset", "admin", "owner") {
		return
	}

	id := r.PathValue("id")
	asset, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	if err := a.store.ArchiveAsset(r.Context(), id); err != nil {
		a.failAsset(w, "archive asset", err)
		return
	}

	a.auditAsset(r, AuditEvent{
		Action:     "asset.archived",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail: "Retired " + asset.Hostname + ". It leaves the inventory and every " +
			"gateway at the next sync; its sessions and audit history are kept.",
	})
	a.log.Warn("asset archived", "hostname", asset.Hostname, "actor", sess.Email)
	writeJSON(w, http.StatusOK, map[string]string{"status": "archived"})
}

func (a *API) getAssignments(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing who is assigned to a host requires the admin, owner or auditor role")
		return
	}

	list, err := a.store.Assignments(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "assignments", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// putAssignment gives a person access to an asset, or takes it away.
//
// An empty principal list removes the assignment rather than recording an
// assignment that permits nothing — the two would look the same in the console
// and mean different things in an audit.
func (a *API) putAssignment(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "assigning an asset", "admin", "owner") {
		return
	}

	id := r.PathValue("id")
	asset, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}

	var in struct {
		Email      string   `json:"email"`
		Principals []string `json:"principals"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))

	if err := a.store.SetAssignment(r.Context(), id, in.Email, in.Principals, sess.Email); err != nil {
		a.failAsset(w, "assign asset", err)
		return
	}

	action, detail := "asset.assigned", in.Email+" may now open a session on "+
		asset.Hostname+" as "+describePrincipals(in.Principals)+"."
	if len(in.Principals) == 0 {
		action, detail = "asset.unassigned", in.Email+" no longer has access to "+asset.Hostname+"."
	}
	a.auditAsset(r, AuditEvent{
		Action:     action,
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail:     detail,
	})
	a.log.Info("assignment changed", "hostname", asset.Hostname,
		"subject", in.Email, "actor", sess.Email, "principals", in.Principals)

	list, err := a.store.Assignments(r.Context(), id)
	if err != nil {
		a.failAsset(w, "assignments", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) deleteAssignment(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "removing an assignment", "admin", "owner") {
		return
	}

	id := r.PathValue("id")
	email := strings.ToLower(r.PathValue("email"))
	asset, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	if err := a.store.RemoveAssignment(r.Context(), id, email); err != nil {
		a.failAsset(w, "remove assignment", err)
		return
	}

	a.auditAsset(r, AuditEvent{
		Action:     "asset.unassigned",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     asset.Hostname,
		Detail:     email + " no longer has access to " + asset.Hostname + ".",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// getInventory serves what a gateway brokers, and for whom.
//
// Machine credential only. It carries the credential *names* a gateway resolves
// against its own vault, never credential material: a control plane that could
// hand out keys would be a vault, and this one deliberately is not.
func (a *API) getInventory(w http.ResponseWriter, r *http.Request) {
	inv, err := a.store.InventorySnapshot(r.Context())
	if err != nil {
		// An error rather than an empty inventory. A gateway keeps the last
		// inventory it successfully fetched, and handing it a zero-valued one
		// on a database fault would read as "every asset was deleted" — which
		// would take the whole fleet offline on a transient failure.
		a.fail(w, "inventory", err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

// assignmentPermits reports whether this person may open this session, and
// answers the request itself when they may not.
//
// Three outcomes worth telling apart, because they need three different next
// steps: the host is not in the inventory at all, the host is there and this
// person has no assignment on it, or the question could not be answered.
//
// The last one refuses. A database that cannot be read is not permission, and
// the moment the store is unavailable is exactly when an unassigned session
// must not open.
//
// Refusals are logged rather than written to the audit chain: the chain is
// append-only and hash-linked, so anything a caller can trigger at will does
// not belong in it.
func (a *API) assignmentPermits(
	w http.ResponseWriter, r *http.Request, role, email, target, principal string,
) bool {
	// Admins and owners are exempt, as they are for the grant check. Someone
	// has to be able to act when the approval chain itself is broken, and their
	// sessions are recorded and attributed like everyone else's.
	if role == "admin" || role == "owner" {
		return true
	}

	allowed, err := a.store.AssignmentAllows(r.Context(), email, target, principal)
	if err != nil {
		a.log.Error("refusing a session: the assignment could not be checked",
			"email", email, "target", target, "principal", principal, "error", err)
		writeErr(w, http.StatusServiceUnavailable,
			"the control plane could not check whether you are assigned this host; "+
				"nothing was opened")
		return false
	}
	if allowed {
		return true
	}

	// An approved access request is the other way in, and it has to be: a
	// product whose answer to "I need this host for an hour" is "ask an
	// administrator to assign it to you permanently" has no just-in-time access
	// at all. A grant is narrower than an assignment — one principal, one host,
	// and it expires — so honouring it here does not widen anything.
	granted, expires, gerr := a.store.ActiveGrant(r.Context(), email, target, principal)
	if gerr != nil {
		a.log.Error("refusing a session: the grant could not be checked",
			"email", email, "target", target, "principal", principal, "error", gerr)
		writeErr(w, http.StatusServiceUnavailable,
			"the control plane could not check your access; nothing was opened")
		return false
	}
	if granted {
		a.log.Info("session authorised by grant rather than assignment",
			"email", email, "target", target, "principal", principal, "expires", expires)
		return true
	}

	managed, err := a.store.AssetIsManaged(r.Context(), target)
	if err != nil {
		a.log.Error("assignment refused and the inventory could not be read",
			"target", target, "error", err)
	}
	a.log.Warn("session refused: not assigned",
		"email", email, "target", target, "principal", principal, "managed", managed)

	if !managed {
		writeErr(w, http.StatusForbidden,
			target+" is not in the inventory; Argus does not broker sessions to it")
		return false
	}
	writeErr(w, http.StatusForbidden,
		"you are not assigned "+target+" as "+principal+
			"; ask an administrator to assign it, or request access if you need it temporarily")
	return false
}

/* ── Shared bits ─────────────────────────────────────────────────────────── */

// failAsset maps the store's errors onto status codes.
//
// A rejected input is the administrator's to fix and says how; a missing asset
// is a 404; anything else is ours and is logged rather than explained.
func (a *API) failAsset(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidAsset
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, ErrAssetNotFound):
		writeErr(w, http.StatusNotFound, "no such asset")
	default:
		a.fail(w, what, err)
	}
}

func (a *API) auditAsset(r *http.Request, e AuditEvent) {
	if _, err := a.store.AppendAudit(r.Context(), e); err != nil {
		a.log.Error("audit append failed", "error", err, "action", e.Action)
	}
}

func describePrincipals(ps []string) string {
	if len(ps) == 0 {
		return "no principals"
	}
	return strings.Join(ps, ", ")
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

/* ── Accounts, for deciding who to assign ────────────────────────────────── */

// consoleUser is an account as the console sees it.
//
// A deliberate projection rather than the store's Account: that type carries a
// password hash and a TOTP secret, and the day one is marshalled straight onto
// the wire is the day the console serves both.
type consoleUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	MFAEnrolled bool   `json:"mfaEnrolled"`
	Disabled    bool   `json:"disabled"`
	// LastSeenAt is null: the column exists but nothing writes it yet, and a
	// console that invented a plausible time would be worse than one that says
	// it does not know.
	LastSeenAt *string `json:"lastSeenAt"`
}

// getUsers lists the accounts an administrator can assign a host to.
//
// Admin, owner and auditor only. Enumerating who holds an account is a
// reconnaissance step, and an operator picking a host to connect to has no need
// of it.
func (a *API) getUsers(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"listing accounts requires the admin, owner or auditor role")
		return
	}

	accounts, err := a.store.Accounts(r.Context())
	if err != nil {
		a.fail(w, "users", err)
		return
	}
	out := make([]consoleUser, 0, len(accounts))
	for _, acct := range accounts {
		out = append(out, consoleUser{
			ID:          acct.ID,
			Email:       acct.Email,
			DisplayName: acct.DisplayName,
			Role:        acct.Role,
			MFAEnrolled: acct.MFAEnrolled,
			Disabled:    acct.Disabled,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

/* ── Reading someone else's session ──────────────────────────────────────── */

// mayReadSession reports whether the caller may read this session and its
// artefacts.
//
// The lists are filtered by role, and a filtered list with an unfiltered detail
// endpoint is not a control: the id is in every recording link, and a session
// recording is the most sensitive artefact this product holds. Admins, owners
// and auditors read any of them -- reviewing sessions is what an auditor is
// for. Everyone else reads their own.
func (a *API) mayReadSession(r *http.Request, sess *Session) bool {
	auth, ok := a.authenticate(r)
	if !ok || sess == nil {
		return false
	}
	return seesWholeFleet(auth.Role) || strings.EqualFold(sess.UserEmail, auth.Email)
}

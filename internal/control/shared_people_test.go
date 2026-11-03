package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
)

// People shared across organizations: one sign-in, a row per organization.

func TestSharedPersonSignsInWithHomeCredentialsOnlyWhereAllowed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	home, _, _ := newTenant(t, s)
	other, _, _ := newTenant(t, s)
	hctx, octx := WithTenant(ctx, home), WithTenant(ctx, other)

	// Alice belongs to home. Until home allows it, other cannot have her,
	// and the refusal is the one every unknown address gets.
	alice := strings.ToLower(unique("alice")) + "@northwind.id"
	if _, err := s.CreateAccount(hctx, alice, "Alice", "operator", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.AddSharedMember(octx, SharedMember{Email: alice, Role: "approver"})
	if err != ErrNoShareable {
		t.Fatalf("sharing a person whose home has not allowed it: %v, want ErrNoShareable", err)
	}
	if _, _, err := s.AddSharedMember(octx, SharedMember{Email: "nobody@northwind.id", Role: "approver"}); err != ErrNoShareable {
		t.Fatalf("sharing an unknown address: %v, want the same ErrNoShareable", err)
	}

	if err := s.SetAccountShareable(hctx, alice, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountShareable(octx, alice, true); err != ErrNoAccount {
		t.Errorf("another organization set shareable on a person it does not have: %v", err)
	}
	row, h, err := s.AddSharedMember(octx, SharedMember{Email: alice, Role: "approver"})
	if err != nil {
		t.Fatalf("AddSharedMember: %v", err)
	}
	if h.ID != home.ID || row.HomeTenant != home.ID || row.Role != "approver" || row.PasswordHash != "" {
		t.Errorf("shared row = %+v (home %s), want a passwordless approver pointing at %s", row, h.ID, home.ID)
	}
	// Adding her again, or giving her a password here, is refused.
	if _, _, err := s.AddSharedMember(octx, SharedMember{Email: alice, Role: "owner"}); err != ErrAccountExists {
		t.Errorf("sharing twice: %v", err)
	}
	if _, err := s.CreateAccount(octx, alice, "Alice", "owner", "another password entirely"); err != ErrSharedAccount {
		t.Errorf("creating a local account over a shared person: %v", err)
	}
	// The shared-in organization cannot decide who may share her.
	if err := s.SetAccountShareable(octx, alice, false); err != ErrSharedAccount {
		t.Errorf("the guest organization changed shareable: %v", err)
	}

	// Her home password signs her in to both, with each organization's role.
	a, err := s.Authenticate(octx, alice, "correct horse battery staple")
	if err != nil {
		t.Fatalf("home password refused in the other organization: %v", err)
	}
	if a.Role != "approver" {
		t.Errorf("role in the other organization = %q, want approver", a.Role)
	}
	if a, err := s.Authenticate(hctx, alice, "correct horse battery staple"); err != nil || a.Role != "operator" {
		t.Errorf("at home: %v, role %q", err, a.Role)
	}
	// Her second factor is enrolled once, at home, whichever organization she
	// enrols from.
	secret, err := s.BeginMFAEnrolment(octx, alice)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := auth.TOTPCode(secret, time.Now())
	if _, err := s.ConfirmMFAEnrolment(octx, alice, code); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Account(hctx, alice); got.TOTPSecret != secret || !got.MFAEnrolled {
		t.Errorf("enrolment started from the other organization did not land at home")
	}
	if got, _ := s.Account(octx, alice); got.TOTPSecret != "" {
		t.Errorf("the other organization holds a TOTP secret")
	}
	// A password change from the other organization changes the one at home.
	if err := s.SetPassword(octx, alice, "a new and different passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(hctx, alice, "a new and different passphrase"); err != nil {
		t.Errorf("new password not accepted at home: %v", err)
	}
	if got, _ := s.Account(octx, alice); got.PasswordHash != "" {
		t.Errorf("the other organization holds a password hash")
	}

	// She is a member of both; disabling her in one keeps the other.
	orgs, err := s.OrganizationsFor(octx, alice)
	if err != nil || len(orgs) != 2 {
		t.Fatalf("OrganizationsFor = %v, %v; want two", orgs, err)
	}
	if err := s.SetAccountDisabled(octx, alice, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(octx, alice, "a new and different passphrase"); err == nil {
		t.Errorf("disabled in the other organization and still signed in there")
	}
	if _, err := s.Authenticate(hctx, alice, "a new and different passphrase"); err != nil {
		t.Errorf("disabled elsewhere and refused at home: %v", err)
	}
	if orgs, _ := s.OrganizationsFor(hctx, alice); len(orgs) != 1 || orgs[0].ID != home.ID {
		t.Errorf("after disabling in the other organization, memberships = %v", orgs)
	}
	// Disabled at home refuses her everywhere.
	_ = s.SetAccountDisabled(octx, alice, false)
	if _, err := s.CreateAccount(hctx, "second-admin-"+alice, "Second", "owner", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountDisabled(hctx, alice, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(octx, alice, "a new and different passphrase"); err == nil {
		t.Errorf("disabled at home and still signed in to the other organization")
	}
}

func TestSameAddressInTwoOrganizationsIsTwoIdentities(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	x, _, _ := newTenant(t, s)
	y, _, _ := newTenant(t, s)
	z, _, _ := newTenant(t, s)
	xctx, yctx, zctx := WithTenant(ctx, x), WithTenant(ctx, y), WithTenant(ctx, z)

	// X and Y each made their own account for the same address, as any two
	// organizations may. Neither can switch into the other.
	bob := strings.ToLower(unique("bob")) + "@northwind.id"
	if _, err := s.CreateAccount(xctx, bob, "Bob at X", "admin", "x-password-for-bob-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(yctx, bob, "Bob at Y", "admin", "y-password-for-bob-2"); err != nil {
		t.Fatal(err)
	}
	if orgs, _ := s.OrganizationsFor(xctx, bob); len(orgs) != 1 || orgs[0].ID != x.ID {
		t.Errorf("X's bob sees organizations %v", orgs)
	}
	if _, err := s.MemberOf(xctx, bob, y); err != ErrNotMember {
		t.Errorf("X's bob is a member of Y: %v", err)
	}
	// Z wants to share bob. Both X and Y allow it: Z must say which.
	_ = s.SetAccountShareable(xctx, bob, true)
	_ = s.SetAccountShareable(yctx, bob, true)
	if _, _, err := s.AddSharedMember(zctx, SharedMember{Email: bob, Role: "operator"}); err != ErrAmbiguousHome {
		t.Fatalf("two shareable homes: %v, want ErrAmbiguousHome", err)
	}
	row, home, err := s.AddSharedMember(zctx, SharedMember{Email: bob, Role: "operator", HomeSlug: y.Slug})
	if err != nil {
		t.Fatal(err)
	}
	if home.ID != y.ID || row.HomeTenant != y.ID {
		t.Errorf("shared from %s, want %s", home.Slug, y.Slug)
	}
	// Y's password works in Z; X's does not, and X's bob is not a member of Z.
	if _, err := s.Authenticate(zctx, bob, "y-password-for-bob-2"); err != nil {
		t.Errorf("Y's password refused in Z: %v", err)
	}
	if _, err := s.Authenticate(zctx, bob, "x-password-for-bob-1"); err == nil {
		t.Errorf("X's password accepted in Z")
	}
	if _, err := s.MemberOf(xctx, bob, z); err != ErrNotMember {
		t.Errorf("X's bob may switch to Z: %v", err)
	}
	if _, err := s.MemberOf(yctx, bob, z); err != nil {
		t.Errorf("Y's bob may not switch to Z: %v", err)
	}
	// Z's listing says bob is shared and reads his second-factor state from Y.
	list, err := s.Accounts(zctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range list {
		if strings.EqualFold(a.Email, bob) {
			found = true
			if !a.Shared() || a.MFAEnrolled {
				t.Errorf("Z lists bob as %+v", a)
			}
		}
	}
	if !found {
		t.Errorf("Z does not list bob")
	}
}

func TestSwitchingOrganizationsOverHTTP(t *testing.T) {
	h := newTenantHarness(t)
	home, owner, pw := newTenant(t, h.store)
	other, otherOwner, otherPw := newTenant(t, h.store)
	ctx := context.Background()

	// The owner of home allows sharing of their own account; other's owner
	// adds them as an auditor through the console route.
	in := h.signIn(t, home.Slug, owner, pw)
	if code, body := h.call(t, in, "", http.MethodPut, "/api/v1/users/"+owner+"/shareable", `{"shareable":true}`); code != http.StatusOK {
		t.Fatalf("shareable = %d: %s", code, body)
	}
	otherIn := h.signIn(t, other.Slug, otherOwner, otherPw)
	if code, body := h.call(t, otherIn, "", http.MethodPost, "/api/v1/users/shared",
		`{"email":"`+owner+`","role":"auditor"}`); code != http.StatusCreated {
		t.Fatalf("add shared = %d: %s", code, body)
	}
	// An address that cannot be shared is a 404, however it fails.
	if code, _ := h.call(t, otherIn, "", http.MethodPost, "/api/v1/users/shared",
		`{"email":"stranger@northwind.id","role":"auditor"}`); code != http.StatusNotFound {
		t.Errorf("unknown address = %d, want 404", code)
	}

	// /auth/me lists both; switching lands in other with other's role.
	_, me := h.call(t, in, "", http.MethodGet, "/auth/me", "")
	var id struct {
		Role string           `json:"role"`
		Org  map[string]any   `json:"org"`
		Orgs []map[string]any `json:"orgs"`
	}
	_ = json.Unmarshal([]byte(me), &id)
	if len(id.Orgs) != 2 || id.Role != "owner" {
		t.Fatalf("/auth/me at home = %s", me)
	}
	if code, body := h.call(t, in, "", http.MethodPost, "/auth/switch", `{"org":"`+other.Slug+`"}`); code != http.StatusOK {
		t.Fatalf("switch = %d: %s", code, body)
	}
	_, me = h.call(t, in, "", http.MethodGet, "/auth/me", "")
	_ = json.Unmarshal([]byte(me), &id)
	if id.Org["slug"] != other.Slug || id.Role != "auditor" {
		t.Errorf("after switching: %s", me)
	}
	// Both chains have it: home records the leaving, other the arrival.
	for _, c := range []struct {
		t      *Tenant
		action string
	}{{home, "auth.switch"}, {other, "auth.login"}, {home, "user.shared_out"}, {other, "user.shared_in"}} {
		var n int
		if err := h.store.db(WithTenant(ctx, c.t)).QueryRow(ctx,
			`SELECT count(*) FROM audit_events WHERE action = $1 AND target IN ($2, 'console')`,
			c.action, owner).Scan(&n); err != nil || n == 0 {
			t.Errorf("%s has no %s event (%v)", c.t.Slug, c.action, err)
		}
	}
	// Switching to an organization they are not in, or to the default one
	// where they have no account, is refused the same way.
	if code, _ := h.call(t, in, "", http.MethodPost, "/auth/switch", `{"org":"default"}`); code != http.StatusNotFound {
		t.Errorf("switch to the default organization = %d, want 404", code)
	}
	if code, _ := h.call(t, in, "", http.MethodPost, "/auth/switch", `{"org":"no-such-org"}`); code != http.StatusNotFound {
		t.Errorf("switch to an unknown organization = %d, want 404", code)
	}
	// Other's owner, who only has their own account, has one organization and
	// cannot reach home even though a person of home's is now in theirs.
	_, me = h.call(t, otherIn, "", http.MethodGet, "/auth/me", "")
	_ = json.Unmarshal([]byte(me), &id)
	if len(id.Orgs) != 1 {
		t.Errorf("other's owner sees organizations: %s", me)
	}
	if code, _ := h.call(t, otherIn, "", http.MethodPost, "/auth/switch", `{"org":"`+home.Slug+`"}`); code != http.StatusNotFound {
		t.Errorf("other's owner switched into home: %d", code)
	}
}

func TestAnAdminCreatesAnOrganizationAndCanSwitchToIt(t *testing.T) {
	h := newTenantHarness(t)
	admin, pw := newAccount(t, h.store, "admin")
	in := h.signIn(t, "", admin, pw)

	// An admin of the main organization, as first-run setup makes, manages
	// organizations; nobody below admin does.
	_, me := h.call(t, in, "", http.MethodGet, "/auth/me", "")
	if !strings.Contains(me, `"platformOwner":true`) {
		t.Fatalf("an admin of the main organization is not offered organizations: %s", me)
	}
	op, opPw := newAccount(t, h.store, "operator")
	if code, _ := h.call(t, h.signIn(t, "", op, opPw), "", http.MethodGet,
		"/api/v1/platform/organizations", ""); code != http.StatusNotFound {
		t.Errorf("an operator reached the platform routes: %d", code)
	}

	slug := strings.ToLower(unique("acme"))
	code, body := h.call(t, in, "", http.MethodPost, "/api/v1/platform/organizations",
		`{"slug":"`+slug+`","name":"Acme","ownerEmail":"owner-`+slug+`@acme.id","ownerPassword":"correct horse battery staple"}`)
	if code != http.StatusCreated {
		t.Fatalf("create organization = %d: %s", code, body)
	}
	var created Tenant
	_ = json.Unmarshal([]byte(body), &created)
	t.Cleanup(func() {
		h.store.closeTenants()
		_, _ = h.store.pool.Exec(context.Background(), `DELETE FROM argus_platform.memberships WHERE tenant_id = $1`, created.ID)
		_, _ = h.store.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+created.Schema+`" CASCADE`)
		_, _ = h.store.pool.Exec(context.Background(), `DELETE FROM argus_platform.tenants WHERE id = $1`, created.ID)
	})

	// It is in their organization menu straight away, and choosing it moves
	// the session there as an owner.
	_, me = h.call(t, in, "", http.MethodGet, "/auth/me", "")
	var id struct {
		Role string           `json:"role"`
		Org  map[string]any   `json:"org"`
		Orgs []map[string]any `json:"orgs"`
	}
	_ = json.Unmarshal([]byte(me), &id)
	if len(id.Orgs) != 2 {
		t.Fatalf("after creating, the menu lists %d organizations: %s", len(id.Orgs), me)
	}
	if code, body := h.call(t, in, "", http.MethodPost, "/auth/switch", `{"org":"`+slug+`"}`); code != http.StatusOK {
		t.Fatalf("switch = %d: %s", code, body)
	}
	_, me = h.call(t, in, "", http.MethodGet, "/auth/me", "")
	_ = json.Unmarshal([]byte(me), &id)
	if id.Org["slug"] != slug || id.Role != "owner" {
		t.Errorf("after switching: %s", me)
	}
}

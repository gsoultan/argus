package control

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// An admin of two organizations adds one of the first's people to the second
// in one step; the person sees it in their menu, switches, and is refused
// there as soon as their home takes it away.
func TestAnAdminOfBothAddsSomeoneInOneStepAndTheHomeCanTakeItBack(t *testing.T) {
	h := newTenantHarness(t)
	ctx := context.Background()
	home, boss, bossPw := newTenant(t, h.store)
	other, otherOwner, otherPw := newTenant(t, h.store)
	hctx, octx := WithTenant(ctx, home), WithTenant(ctx, other)

	// boss runs home and is an owner of other too.
	if _, err := h.store.AllowShare(hctx, boss, boss, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.AddSharedMember(octx, SharedMember{Email: boss, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	citra := strings.ToLower(unique("citra")) + "@northwind.id"
	if _, err := h.store.CreateAccount(hctx, citra, "Citra", "operator", "citra's own long passphrase"); err != nil {
		t.Fatal(err)
	}

	in := h.signIn(t, home.Slug, boss, bossPw)
	code, body := h.call(t, in, "", http.MethodGet, "/api/v1/users/"+citra+"/organizations", "")
	if code != http.StatusOK || !strings.Contains(body, `"slug":"`+other.Slug+`"`) {
		t.Fatalf("where citra works = %d %s; want %s offered", code, body, other.Slug)
	}
	code, body = h.call(t, in, "", http.MethodPost, "/api/v1/users/"+citra+"/organizations",
		`{"org":"`+other.Slug+`","role":"operator"}`)
	if code != http.StatusOK || !strings.Contains(body, `"added":true`) {
		t.Fatalf("add to other = %d %s", code, body)
	}

	// Citra signs in at home as always; other is in her menu, and switching
	// lands her there as an operator with her own access list.
	ci := h.signIn(t, home.Slug, citra, "citra's own long passphrase")
	var me struct {
		Role string           `json:"role"`
		Org  map[string]any   `json:"org"`
		Orgs []map[string]any `json:"orgs"`
	}
	_, body = h.call(t, ci, "", http.MethodGet, "/auth/me", "")
	_ = json.Unmarshal([]byte(body), &me)
	if len(me.Orgs) != 2 {
		t.Fatalf("citra's menu = %s", body)
	}
	if code, body := h.call(t, ci, "", http.MethodPost, "/auth/switch", `{"org":"`+other.Slug+`"}`); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	_, body = h.call(t, ci, "", http.MethodGet, "/auth/me", "")
	_ = json.Unmarshal([]byte(body), &me)
	if me.Org["slug"] != other.Slug || me.Role != "operator" {
		t.Fatalf("after switching: %s", body)
	}
	oi := h.signIn(t, other.Slug, otherOwner, otherPw)
	code, body = h.call(t, oi, "", http.MethodGet, "/api/v1/users/"+citra+"/access", "")
	var sections []AccessSection
	_ = json.Unmarshal([]byte(body), &sections)
	if code != http.StatusOK || len(sections) != len(accessKinds) {
		t.Fatalf("citra's access in other = %d %s", code, body)
	}

	// Home takes other away: her cookie there stops working at once.
	if code, body := h.call(t, in, "", http.MethodDelete, "/api/v1/users/"+citra+"/organizations/"+other.Slug, ""); code != http.StatusNoContent {
		t.Fatalf("take away = %d %s", code, body)
	}
	if code, _ := h.call(t, ci, "", http.MethodGet, "/api/v1/databases", ""); code != http.StatusUnauthorized {
		t.Errorf("citra still works in other after home took it away: %d", code)
	}
	for _, c := range []struct {
		t      *Tenant
		action string
	}{{home, "user.share_allowed"}, {other, "user.shared_in"}, {home, "user.share_revoked"}, {other, "user.share_revoked"}} {
		var n int
		if err := h.store.db(WithTenant(ctx, c.t)).QueryRow(ctx,
			`SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2 AND actor_email = $3`,
			c.action, citra, boss).Scan(&n); err != nil || n == 0 {
			t.Errorf("%s has no %s by %s (%v)", c.t.Slug, c.action, boss, err)
		}
	}
}

// From the other side: Add person lists the people of organizations the
// caller runs, and adds one without their home having been visited first.
// Someone who does not run the home still needs it to have named them.
func TestAddPersonFromAnOrganizationYouRun(t *testing.T) {
	h := newTenantHarness(t)
	ctx := context.Background()
	home, boss, bossPw := newTenant(t, h.store)
	other, otherOwner, otherPw := newTenant(t, h.store)
	hctx, octx := WithTenant(ctx, home), WithTenant(ctx, other)
	if _, err := h.store.AllowShare(hctx, boss, boss, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.AddSharedMember(octx, SharedMember{Email: boss, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	dewi := strings.ToLower(unique("dewi")) + "@northwind.id"
	eko := strings.ToLower(unique("eko")) + "@northwind.id"
	for _, e := range []string{dewi, eko} {
		if _, err := h.store.CreateAccount(hctx, e, "Someone", "operator", "a long enough passphrase"); err != nil {
			t.Fatal(err)
		}
	}

	// other's owner alone runs only other: home has not named other for eko.
	oi := h.signIn(t, other.Slug, otherOwner, otherPw)
	if code, body := h.call(t, oi, "", http.MethodPost, "/api/v1/users/shared",
		`{"email":"`+eko+`","role":"operator","homeOrg":"`+home.Slug+`"}`); code != http.StatusNotFound {
		t.Fatalf("added without home's say = %d %s", code, body)
	}
	_, body := h.call(t, oi, "", http.MethodGet, "/api/v1/users/from-organizations", "")
	if strings.Contains(body, dewi) {
		t.Errorf("other's owner sees home's people: %s", body)
	}

	// boss, signed in to other, runs home too: dewi is offered and added.
	bi := h.signIn(t, other.Slug, boss, bossPw)
	_, body = h.call(t, bi, "", http.MethodGet, "/api/v1/users/from-organizations", "")
	if !strings.Contains(body, dewi) || strings.Contains(body, `"`+boss+`"`) {
		t.Fatalf("from-organizations = %s; want dewi and not boss, who is here", body)
	}
	if code, body := h.call(t, bi, "", http.MethodPost, "/api/v1/users/shared",
		`{"email":"`+dewi+`","role":"auditor","homeOrg":"`+home.Slug+`"}`); code != http.StatusCreated {
		t.Fatalf("one-step add = %d %s", code, body)
	}
	allowed, err := h.store.ShareAllowances(hctx, dewi)
	if err != nil || len(allowed) != 1 || allowed[0].Slug != other.Slug || !allowed[0].Added || allowed[0].Role != "auditor" {
		t.Errorf("home's record of dewi = %+v %v", allowed, err)
	}
}

// Every table that gives a person or a team something must be read by an
// access kind, or the "what can they do" window and the step after adding
// someone would not show it. Add the new kind to accessKinds, or say in
// notGrantTables why the table grants nothing.
func TestEveryGrantTableHasAnAccessKind(t *testing.T) {
	read := map[string]bool{}
	for _, k := range accessKinds {
		for _, tb := range k.tables {
			read[tb] = true
		}
	}
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations: %v", err)
	}
	create := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+)\s*\((.*?)\n\);`)
	alter := regexp.MustCompile(`ALTER TABLE (?:IF EXISTS )?(\w+)\s+ADD COLUMN (?:IF NOT EXISTS )?(user_email|team_id)\b`)
	grantCol := regexp.MustCompile(`(?m)^\s*(user_email|team_id)\b`)
	found := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range create.FindAllStringSubmatch(string(b), -1) {
			if grantCol.MatchString(m[2]) {
				found[m[1]] = filepath.Base(f)
			}
		}
		for _, m := range alter.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = filepath.Base(f)
		}
	}
	if len(found) < 4 {
		t.Fatalf("found only %v; the scan is broken", found)
	}
	for tb, f := range found {
		if !read[tb] && notGrantTables[tb] == "" {
			t.Errorf("%s (%s) names people or teams, but no access kind reads it and notGrantTables does not say why", tb, f)
		}
	}
	for _, k := range accessKinds {
		for _, tb := range k.tables {
			if _, ok := found[tb]; !ok && tb != "cloud_access" {
				t.Errorf("access kind %s reads %s, which no migration creates with a person or team", k.kind, tb)
			}
		}
	}
}

// Upgrading to per-organization sharing keeps everyone where they are: each
// organization a person was already shared into becomes an allowance, and
// the blanket flag on its own allows nothing. Later starts never re-fill it.
func TestUpgradeCarriesExistingSharingOverAndNothingMore(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	home, _, _ := newTenant(t, s)
	other, _, _ := newTenant(t, s)
	third, _, _ := newTenant(t, s)
	hctx, octx := WithTenant(ctx, home), WithTenant(ctx, other)
	in := strings.ToLower(unique("in")) + "@northwind.id"
	flagged := strings.ToLower(unique("flagged")) + "@northwind.id"
	for _, e := range []string{in, flagged} {
		if _, err := s.CreateAccount(hctx, e, "Someone", "operator", "a long enough passphrase"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AllowShare(hctx, "x", in, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddSharedMember(octx, SharedMember{Email: in, Role: "operator"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountShareable(hctx, flagged, true); err != nil {
		t.Fatal(err)
	}

	// The version before: no allowances table at all.
	if _, err := s.pool.Exec(ctx, `DROP TABLE argus_platform.share_allowances`); err != nil {
		t.Fatal(err)
	}
	if err := s.migratePlatform(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.shareAllowed(ctx, in, home.ID, other.ID); !ok {
		t.Errorf("a person already in other lost it at the upgrade")
	}
	if _, err := s.Authenticate(octx, in, "a long enough passphrase"); err != nil {
		t.Errorf("signed out of other by the upgrade: %v", err)
	}
	for _, tn := range []*Tenant{other, third} {
		if ok, _ := s.shareAllowed(ctx, flagged, home.ID, tn.ID); ok {
			t.Errorf("the blanket flag became an allowance for %s", tn.Slug)
		}
	}
	// A taken-away allowance stays away across starts.
	if err := s.RevokeShare(hctx, in, other); err != nil {
		t.Fatal(err)
	}
	if err := s.migratePlatform(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.shareAllowed(ctx, in, home.ID, other.ID); ok {
		t.Errorf("a start re-filled an allowance home took away")
	}
}

// An organization whose people all sign in elsewhere is not a fresh install:
// someone signed out of it is offered the sign-in form, not first-run setup.
func TestAnOrganizationOfSharedPeopleHasAccounts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	home, owner, _ := newTenant(t, s)
	other, otherOwner, _ := newTenant(t, s)
	octx := WithTenant(ctx, other)
	if _, err := s.AllowShare(WithTenant(ctx, home), owner, owner, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddSharedMember(octx, SharedMember{Email: owner, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	// other's own owner leaves; only the shared one is left.
	if _, err := s.db(octx).Exec(ctx, `UPDATE users SET disabled_at = now() WHERE lower(email) = lower($1)`, otherOwner); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AnyAccountExists(octx); err != nil || !ok {
		t.Errorf("an organization of shared people has no accounts: %v %v", ok, err)
	}
}

package control

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Gembit's case: other gave Citra an account of its own (her own password
// there), then removed her. Her home now allows other and adds her: the
// removed account becomes the shared one, so she signs in with her home
// password and works in other again. Before, every path answered "already
// has an account in this organization", and the one-step add said "added".
func TestARemovedAccountOfTheirOwnBecomesTheSharedOne(t *testing.T) {
	h := newTenantHarness(t)
	ctx := context.Background()
	home, boss, bossPw := newTenant(t, h.store)
	other, _, _ := newTenant(t, h.store)
	hctx, octx := WithTenant(ctx, home), WithTenant(ctx, other)
	if _, err := h.store.AllowShare(hctx, boss, boss, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.AddSharedMember(octx, SharedMember{Email: boss, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	citra := strings.ToLower(unique("citra")) + "@northwind.id"
	if _, err := h.store.CreateAccount(hctx, citra, "Citra", "operator", "citra's home passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateAccount(octx, citra, "Citra (other)", "viewer", "citra's other passphrase"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountDisabled(octx, citra, true); err != nil {
		t.Fatal(err)
	}

	in := h.signIn(t, home.Slug, boss, bossPw)
	// Removed people are offered again in other's Add person list.
	oi := h.signIn(t, other.Slug, boss, bossPw)
	if code, body := h.call(t, oi, "", http.MethodGet, "/api/v1/users/from-organizations", ""); code != http.StatusOK || !strings.Contains(body, citra) {
		t.Fatalf("from-organizations = %d %s; want %s offered", code, body, citra)
	}

	code, body := h.call(t, in, "", http.MethodPost, "/api/v1/users/"+citra+"/organizations",
		`{"org":"`+other.Slug+`","role":"operator"}`)
	if code != http.StatusOK || !strings.Contains(body, `"added":true`) || strings.Contains(body, `"already"`) {
		t.Fatalf("add to other = %d %s", code, body)
	}
	a, err := h.store.Account(octx, citra)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Shared() || a.Disabled || a.PasswordHash != "" || a.Role != "operator" {
		t.Fatalf("citra in other: shared=%v disabled=%v ownPassword=%v role=%s", a.Shared(), a.Disabled, a.PasswordHash != "", a.Role)
	}
	// Her home password opens other; the old one from other does not.
	ci := h.signIn(t, home.Slug, citra, "citra's home passphrase")
	if code, body := h.call(t, ci, "", http.MethodPost, "/auth/switch", `{"org":"`+other.Slug+`"}`); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	if code, _ := h.call(t, &http.Client{}, other.Slug, http.MethodPost, "/auth/password",
		`{"email":"`+citra+`","password":"citra's other passphrase"}`); code == http.StatusOK {
		t.Error("the removed account's own password still signs in to other")
	}
}

// The sibling case: other's own account for the address is still in use.
// Nothing is taken over; the answer says what to do instead.
func TestAnActiveAccountOfTheirOwnIsNotTakenOver(t *testing.T) {
	h := newTenantHarness(t)
	ctx := context.Background()
	home, boss, bossPw := newTenant(t, h.store)
	other, _, _ := newTenant(t, h.store)
	hctx, octx := WithTenant(ctx, home), WithTenant(ctx, other)
	if _, err := h.store.AllowShare(hctx, boss, boss, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.AddSharedMember(octx, SharedMember{Email: boss, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	citra := strings.ToLower(unique("citra")) + "@northwind.id"
	if _, err := h.store.CreateAccount(hctx, citra, "Citra", "operator", "citra's home passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateAccount(octx, citra, "Citra (other)", "viewer", "citra's other passphrase"); err != nil {
		t.Fatal(err)
	}

	in := h.signIn(t, home.Slug, boss, bossPw)
	code, body := h.call(t, in, "", http.MethodPost, "/api/v1/users/"+citra+"/organizations",
		`{"org":"`+other.Slug+`","role":"operator"}`)
	if code != http.StatusConflict || !strings.Contains(body, "Remove") || strings.Contains(body, `"added":true`) {
		t.Fatalf("add over an active account = %d %s; want 409 saying to remove it first", code, body)
	}
	a, err := h.store.Account(octx, citra)
	if err != nil || a.Shared() || a.PasswordHash == "" {
		t.Fatalf("other's own account was changed: %+v %v", a, err)
	}
	oi := h.signIn(t, other.Slug, boss, bossPw)
	code, body = h.call(t, oi, "", http.MethodPost, "/api/v1/users/shared",
		`{"email":"`+citra+`","displayName":"Citra","role":"operator","homeOrg":"`+home.Slug+`"}`)
	if code != http.StatusConflict || !strings.Contains(body, "Remove") {
		t.Fatalf("add from other = %d %s; want 409 saying to remove it first", code, body)
	}
}

package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseUsername(t *testing.T) {
	ok := map[string]Request{
		"ops:pay-01":                       {Principal: "ops", Target: "pay-01"},
		"deploy+core-02":                   {Principal: "deploy", Target: "core-02"},
		"root/db-01":                       {Principal: "root", Target: "db-01"},
		"ops:pay-01.payments.northwind.id": {Principal: "ops", Target: "pay-01.payments.northwind.id"},
		"  ops : pay-01 ":                  {Principal: "ops", Target: "pay-01"},
	}
	for input, want := range ok {
		got, err := ParseUsername(input)
		if err != nil {
			t.Errorf("ParseUsername(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseUsername(%q) = %+v, want %+v", input, got, want)
		}
	}
}

// Guessing at a missing principal would mean a typo silently opens a session as
// the wrong account, which is exactly the ambiguity a PAM tool must not have.
func TestAmbiguousUsernamesAreRejected(t *testing.T) {
	bad := []string{
		"",           // nothing at all
		"ops",        // no target: do not default it
		":pay-01",    // no principal
		"ops:",       // no target
		"ops:pay:01", // two separators — which is the split?
		"ops:pay+01", // mixed separators, same ambiguity
		"   ",        // whitespace only
		"ops:   ",    // target is whitespace
	}
	for _, input := range bad {
		if got, err := ParseUsername(input); err == nil {
			t.Errorf("ParseUsername(%q) = %+v, want an error", input, got)
		}
	}
}

// The error has to tell the user the syntax, since it is the first thing they
// hit and there is no other prompt to explain it.
func TestMissingTargetErrorExplainsTheSyntax(t *testing.T) {
	_, err := ParseUsername("ops")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "principal:host@gateway") {
		t.Errorf("error does not show the expected form: %v", err)
	}
}

/* ── Inventory ───────────────────────────────────────────────────────────── */

func writeInventory(t *testing.T, assets []Asset) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	data, err := json.Marshal(assets)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInventoryResolvesFullAndShortNames(t *testing.T) {
	path := writeInventory(t, []Asset{{
		Hostname: "pay-01.payments.northwind.id", Address: "10.0.0.1",
		Principals: []string{"ops"},
	}})
	inv, err := LoadInventory(path)
	if err != nil {
		t.Fatalf("LoadInventory: %v", err)
	}

	for _, name := range []string{"pay-01.payments.northwind.id", "pay-01"} {
		if _, err := inv.Resolve(name); err != nil {
			t.Errorf("Resolve(%q): %v", name, err)
		}
	}
	if _, err := inv.Resolve("nope"); err == nil {
		t.Error("resolved a host that is not in the inventory")
	}
}

// An error on an unknown target must not enumerate the fleet for someone
// probing it.
func TestUnknownTargetErrorDoesNotListHosts(t *testing.T) {
	path := writeInventory(t, []Asset{
		{Hostname: "secret-prod-db.internal", Address: "10.0.0.9"},
		{Hostname: "another-host.internal", Address: "10.0.0.10"},
	})
	inv, _ := LoadInventory(path)

	_, err := inv.Resolve("guess")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, leaked := range []string{"secret-prod-db", "another-host"} {
		if contains(err.Error(), leaked) {
			t.Errorf("the error enumerates the inventory: %v", err)
		}
	}
}

// The inventory, not the target's /etc/passwd, decides who may be assumed. A
// credential that happens to work is not authorisation.
func TestPrincipalAllowlist(t *testing.T) {
	a := Asset{Principals: []string{"ops", "deploy"}}
	if !a.AllowsPrincipal("ops") {
		t.Error("ops should be permitted")
	}
	if a.AllowsPrincipal("root") {
		t.Error("root is not in the list and must be refused")
	}
	if (Asset{}).AllowsPrincipal("ops") {
		t.Error("an asset with no principals must permit none")
	}
}

func TestCredentialMode(t *testing.T) {
	if !(Asset{CredentialMode: "ca-certificate"}).UsesCertificate() {
		t.Error("ca-certificate should use certificate auth")
	}
	// Anything unset falls back to injected keys rather than silently
	// requiring a CA that may not be configured.
	for _, mode := range []string{"", "injected-key", "injected-password"} {
		if (Asset{CredentialMode: mode}).UsesCertificate() {
			t.Errorf("mode %q should not use certificate auth", mode)
		}
	}
}

func TestAssetAddrDefaultsToPort22(t *testing.T) {
	if got := (Asset{Address: "10.0.0.1"}).Addr(); got != "10.0.0.1:22" {
		t.Errorf("Addr() = %q, want 10.0.0.1:22", got)
	}
	if got := (Asset{Address: "10.0.0.1", Port: 2222}).Addr(); got != "10.0.0.1:2222" {
		t.Errorf("Addr() = %q", got)
	}
}

func TestCorruptInventoryIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInventory(path); err == nil {
		t.Error("loaded a corrupt inventory instead of failing")
	}
	if _, err := LoadInventory(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("loaded a nonexistent inventory")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

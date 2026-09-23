package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/reporter"
)

/*
What the gateway does with an inventory the control plane sent it.

Two properties are worth more than the rest:

  - A credential is a name resolved inside the vault directory. An
    administrator with a console must not be able to make this process read a
    file outside it.

  - An assigned session opens without asking anyone. Everything else asks the
    control plane, and an unanswerable question is a refusal — losing contact
    must never be the thing that opens a host.
*/

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

/* ── The credential is a name ────────────────────────────────────────────── */

func TestVaultPathRefusesAnythingButAName(t *testing.T) {
	for _, ref := range []string{
		"../../etc/shadow", "/etc/shadow", "keys/injected", "..", ".", "",
		`..\windows`, "sub/dir",
	} {
		if got, err := vaultPath("/var/lib/argus/vault", ref); err == nil {
			t.Errorf("credential %q resolved to %q", ref, got)
		}
	}
}

func TestVaultPathResolvesInsideTheVault(t *testing.T) {
	got, err := vaultPath("/var/lib/argus/vault", "pay-01")
	if err != nil {
		t.Fatalf("a plain name was refused: %v", err)
	}
	if got != "/var/lib/argus/vault/pay-01" {
		t.Errorf("resolved to %q", got)
	}
}

// With no vault configured there is nowhere safe to resolve a name, so the
// asset is dropped rather than resolved somewhere surprising.
func TestAnAssetIsDroppedWhenItsCredentialCannotBeResolved(t *testing.T) {
	inv := NewInventory()
	applyInventory(inv, reporter.Inventory{
		Assets: []reporter.InventoryAsset{
			{Hostname: "safe.example", Address: "192.0.2.1", Port: 22,
				Principals: []string{"ops"}, CredentialRef: "ok"},
			{Hostname: "unsafe.example", Address: "192.0.2.2", Port: 22,
				Principals: []string{"ops"}, CredentialRef: "../../etc/shadow"},
		},
	}, t.TempDir(), "control plane", quiet())

	if _, err := inv.Resolve("unsafe.example"); err == nil {
		t.Error("an asset whose credential leaves the vault was loaded anyway")
	}
	if _, err := inv.Resolve("safe.example"); err != nil {
		t.Errorf("the well-formed asset was dropped too: %v", err)
	}
}

// One name, resolved by protocol: SSH injects a key file, Remote Desktop reads
// a directory holding one password per principal.
func TestTheCredentialResolvesByProtocol(t *testing.T) {
	vault := t.TempDir()
	inv := NewInventory()
	applyInventory(inv, reporter.Inventory{
		Assets: []reporter.InventoryAsset{
			{Hostname: "pay-01.example", Address: "192.0.2.1", Port: 22, Protocol: "ssh",
				Principals: []string{"ops"}, CredentialRef: "pay-01"},
			{Hostname: "win-01.example", Address: "192.0.2.2", Port: 3389, Protocol: "rdp",
				Principals: []string{"rdptest"}, CredentialRef: "win-01"},
		},
	}, vault, "control plane", quiet())

	ssh, err := inv.Resolve("pay-01.example")
	if err != nil {
		t.Fatal(err)
	}
	if ssh.KeyPath != filepath.Join(vault, "pay-01") || ssh.CredentialDir != "" {
		t.Errorf("ssh asset resolved to key=%q dir=%q", ssh.KeyPath, ssh.CredentialDir)
	}

	rdp, err := inv.Resolve("win-01.example")
	if err != nil {
		t.Fatal(err)
	}
	if rdp.CredentialDir != filepath.Join(vault, "win-01") || rdp.KeyPath != "" {
		t.Errorf("rdp asset resolved to key=%q dir=%q", rdp.KeyPath, rdp.CredentialDir)
	}
}

/* ── Assignment ──────────────────────────────────────────────────────────── */

func syncedInventory(t *testing.T) *Inventory {
	t.Helper()
	inv := NewInventory()
	inv.UnderControlPlane()
	applyInventory(inv, reporter.Inventory{
		Assets: []reporter.InventoryAsset{{
			Hostname: "pay-01.northwind.example", Address: "192.0.2.1", Port: 22,
			Principals: []string{"ops", "deploy"}, CredentialRef: "pay-01",
			Assignments: []reporter.InventoryAssignment{
				{Email: "lin@northwind.example", Principals: []string{"ops"}},
			},
		}},
		Unrestricted: []string{"admin@northwind.example"},
	}, t.TempDir(), "control plane", quiet())
	return inv
}

func TestAssignedIsBoundedByTheAssetsOwnList(t *testing.T) {
	inv := syncedInventory(t)

	if !inv.Assigned("pay-01.northwind.example", "lin@northwind.example", "ops") {
		t.Error("an assigned principal was not recognised")
	}
	if inv.Assigned("pay-01.northwind.example", "lin@northwind.example", "deploy") {
		t.Error("a principal nobody assigned was recognised")
	}
	if inv.Assigned("pay-01.northwind.example", "someone@northwind.example", "ops") {
		t.Error("an unassigned person was recognised")
	}
}

// `ssh ops:pay-01@gw` has to find the assignment made against the full name, or
// the short form is a way round it.
func TestAssignedResolvesTheShortName(t *testing.T) {
	inv := syncedInventory(t)
	if !inv.Assigned("pay-01", "lin@northwind.example", "ops") {
		t.Error("the short name did not match the assignment")
	}
}

func TestAssignedIsCaseInsensitiveOnTheAddress(t *testing.T) {
	inv := syncedInventory(t)
	if !inv.Assigned("pay-01.northwind.example", "LIN@Northwind.Example", "ops") {
		t.Error("the same address in another case did not match")
	}
}

func TestUnrestrictedAccountsAreExempt(t *testing.T) {
	inv := syncedInventory(t)
	if !inv.Unrestricted("Admin@Northwind.Example") {
		t.Error("an admin was not recognised as exempt")
	}
	if inv.Unrestricted("lin@northwind.example") {
		t.Error("an ordinary account was treated as exempt")
	}
}

/* ── What Dial does with it ──────────────────────────────────────────────── */

// The fast path: an assigned, ordinary session opens with no control plane
// involved at all. The server here has no reporter, so anything that reached
// for one would fail.
func TestAnAssignedSessionNeedsNoControlPlane(t *testing.T) {
	srv := testServer(t, t.TempDir())
	srv.log = quiet()
	srv.cfg.Inventory = syncedInventory(t)
	h := &PolicyHolder{}
	h.Set(DefaultPolicy())
	srv.cfg.Policy = h

	if _, err := srv.authorize("lin@northwind.example", "ops", "pay-01.northwind.example"); err != nil {
		t.Fatalf("an assigned session was refused: %v", err)
	}
	if _, err := srv.authorize("admin@northwind.example", "ops", "pay-01.northwind.example"); err != nil {
		t.Fatalf("an exempt account was refused: %v", err)
	}
}

// And the other half: not assigned, nothing to ask, so nothing opens. An
// outage must not be the thing that lets someone onto a host.
func TestAnUnassignedSessionIsRefusedWithNoControlPlane(t *testing.T) {
	srv := testServer(t, t.TempDir())
	srv.log = quiet()
	srv.cfg.Inventory = syncedInventory(t)
	h := &PolicyHolder{}
	h.Set(DefaultPolicy())
	srv.cfg.Policy = h

	_, err := srv.authorize("stranger@northwind.example", "ops", "pay-01.northwind.example")
	if err == nil {
		t.Fatal("an unassigned session opened while the control plane was unreachable")
	}
	if !strings.Contains(err.Error(), "not assigned") {
		t.Errorf("the refusal should say why: %v", err)
	}
}

// A gateway with no control plane at all keeps working the way it always has:
// the principal list is the whole control. This is a real reduction, which is
// why cmd/argus-gateway says so at start-up — but it must not be a refusal.
func TestAFileInventoryStillBrokersOrdinarySessions(t *testing.T) {
	srv := testServer(t, t.TempDir())
	srv.log = quiet()
	inv := NewInventory()
	applyInventory(inv, reporter.Inventory{
		Assets: []reporter.InventoryAsset{{
			Hostname: "pay-01.example", Address: "192.0.2.1", Port: 22,
			Principals: []string{"ops"},
		}},
	}, t.TempDir(), "file", quiet())
	srv.cfg.Inventory = inv
	h := &PolicyHolder{}
	h.Set(DefaultPolicy())
	srv.cfg.Policy = h

	if _, err := srv.authorize("anyone@northwind.example", "ops", "pay-01.example"); err != nil {
		t.Fatalf("a file-inventory gateway refused an ordinary session: %v", err)
	}
}

/* ── Surviving a restart ─────────────────────────────────────────────────── */

// Without the cache, a gateway that restarts during a control-plane outage
// knows of no assignments and has to refuse everything: the outage becomes a
// lockout rather than a delay.
func TestTheCachedInventoryIsRestoredAtStartUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory-cache.json")

	if err := writeInventoryCache(path, reporter.Inventory{
		Assets: []reporter.InventoryAsset{{
			Hostname: "pay-01.example", Address: "192.0.2.1", Port: 22,
			Principals: []string{"ops"}, CredentialRef: "pay-01",
			Assignments: []reporter.InventoryAssignment{
				{Email: "lin@northwind.example", Principals: []string{"ops"}},
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the cached fleet map is mode %o", mode)
	}

	inv := NewInventory()
	inv.UnderControlPlane()
	if !LoadInventoryCache(inv, path, dir, quiet()) {
		t.Fatal("the cache was not restored")
	}
	if !inv.Assigned("pay-01.example", "lin@northwind.example", "ops") {
		t.Error("the restored inventory lost its assignments")
	}
}

// A missing cache is the ordinary first start, not a failure.
func TestAMissingCacheIsNotAnError(t *testing.T) {
	inv := NewInventory()
	if LoadInventoryCache(inv, filepath.Join(t.TempDir(), "absent.json"), "", quiet()) {
		t.Error("a missing cache reported as restored")
	}
}

/* ── Failing safe ────────────────────────────────────────────────────────── */

type failingFetcher struct{ calls int }

func (f *failingFetcher) Enabled() bool { return true }
func (f *failingFetcher) Inventory(_ context.Context) (*reporter.Inventory, error) {
	f.calls++
	return nil, errors.New("control plane unreachable")
}

// A failed fetch keeps the inventory already in force. Emptying it would read
// as "every asset was deleted" and take the fleet offline on a network blip.
func TestAFailedSyncKeepsTheInventoryInForce(t *testing.T) {
	inv := syncedInventory(t)
	before := inv.Count()

	ctx, cancel := context.WithCancel(context.Background())
	f := &failingFetcher{}
	go func() {
		SyncInventory(ctx, inv, f, t.TempDir(), "", quiet())
	}()
	// One fetch happens immediately; cancel before the ticker fires again.
	for i := 0; i < 100 && f.calls == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	cancel()

	if f.calls == 0 {
		t.Fatal("the sync never asked")
	}
	if inv.Count() != before {
		t.Errorf("a failed fetch changed the inventory: %d assets, was %d",
			inv.Count(), before)
	}
	if !inv.Assigned("pay-01.northwind.example", "lin@northwind.example", "ops") {
		t.Error("a failed fetch lost the assignments")
	}
}

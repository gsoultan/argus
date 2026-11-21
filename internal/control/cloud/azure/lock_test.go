package azure

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

const lockPath = "/providers/Microsoft.Authorization/locks/" + LockName

func TestProtectingAVMPutsArgussDeleteLockOnIt(t *testing.T) {
	p, m, a := removable(t)
	ctx := context.Background()
	res, err := p.Protect(ctx, a, "ada@example.com", "eastus", web1, true)
	if err != nil || res.ResourceID != web1 || res.RequestID == "" {
		t.Fatalf("protect = %+v, %v", res, err)
	}
	got := m.changed()
	if len(got) != 1 || !strings.HasPrefix(got[0], "PUT "+lockPath+" ") {
		t.Fatalf("calls = %v", got)
	}
	// A lock that stops deletes and nothing else: ReadOnly would also stop
	// the VM being stopped or resized.
	if !strings.Contains(got[0], `"level":"CanNotDelete"`) || strings.Contains(got[0], "ReadOnly") || !strings.Contains(got[0], "Argus") {
		t.Errorf("lock body = %s", got[0])
	}
	// It now stands in the way of a remove, from Argus too.
	if plan, err := p.PlanRemove(ctx, a, "ada@example.com", "eastus", web1); err != nil || !plan.Protected {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if _, err := p.Remove(ctx, a, "ada@example.com", "eastus", web1, false); !errors.Is(err, ErrLocked) {
		t.Fatalf("remove = %v", err)
	}
	// Turning it on again is no error and leaves one lock.
	if _, err := p.Protect(ctx, a, "ada@example.com", "eastus", web1, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.locks, []string{LockName}) {
		t.Fatalf("locks = %v", m.locks)
	}
	// Off again, and the VM can be removed.
	if _, err := p.Protect(ctx, a, "ada@example.com", "eastus", web1, false); err != nil {
		t.Fatal(err)
	}
	if len(m.locks) != 0 {
		t.Fatalf("locks = %v", m.locks)
	}
	if plan, err := p.PlanRemove(ctx, a, "ada@example.com", "eastus", web1); err != nil || plan.Protected {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}

func TestArgusTakesOffOnlyItsOwnLock(t *testing.T) {
	p, m, a := removable(t)
	ctx := context.Background()
	// Somebody else's lock, and none of Argus's: nothing is asked of Azure.
	m.locks = []string{"finance-hold"}
	_, err := p.Protect(ctx, a, "ada@example.com", "eastus", web1, false)
	if err == nil || !strings.Contains(err.Error(), "finance-hold") || !strings.Contains(err.Error(), "portal") {
		t.Fatalf("err = %v", err)
	}
	if got := m.changed(); len(got) != 0 {
		t.Fatalf("somebody else's lock was touched: %v", got)
	}
	// Both: Argus's goes, the other stays, and the answer says so.
	m.locks = []string{"finance-hold", LockName}
	_, err = p.Protect(ctx, a, "ada@example.com", "eastus", web1, false)
	if err == nil || !strings.Contains(err.Error(), "finance-hold") {
		t.Fatalf("err = %v", err)
	}
	if got := m.changed(); len(got) != 1 || !strings.HasPrefix(got[0], "DELETE "+lockPath+" ") {
		t.Fatalf("calls = %v", got)
	}
	if !slices.Equal(m.locks, []string{"finance-hold"}) {
		t.Fatalf("locks = %v", m.locks)
	}
	// No lock at all: already off, and nothing is asked.
	m.locks = nil
	if _, err := p.Protect(ctx, a, "ada@example.com", "eastus", web1, false); err != nil {
		t.Fatal(err)
	}
	if got := m.changed(); len(got) != 1 {
		t.Fatalf("calls = %v", got)
	}
}

func TestProtectSaysWhatAzureRefused(t *testing.T) {
	p, m, a := removable(t)
	m.refuse = true
	if _, err := p.Protect(context.Background(), a, "ada@example.com", "eastus", web1, true); err == nil {
		t.Fatal("a refused lock must be an error")
	}
	if len(m.locks) != 0 {
		t.Fatalf("locks = %v", m.locks)
	}
}

func TestTheManageRoleHoldsOnlyLocksOfAuthorization(t *testing.T) {
	for _, act := range ManageActions {
		if strings.HasPrefix(act, "Microsoft.Authorization/") && !strings.HasPrefix(act, "Microsoft.Authorization/locks/") {
			t.Errorf("the manage role must hold no right to grant access: %s", act)
		}
		if strings.Contains(act, "*") {
			t.Errorf("no wildcard: %s", act)
		}
	}
	p := New(Options{})
	for _, a := range []cloud.Action{cloud.ActionProtect, cloud.ActionUnprotect} {
		if !p.Supports(a) || len(p.Needs(a)) != 1 || !slices.Contains(ManageActions, p.Needs(a)[0]) {
			t.Errorf("%s: supported %v, needs %v", a, p.Supports(a), p.Needs(a))
		}
	}
}

// lockOf is what the inventory says of web-1's locks.
func lockOf(t *testing.T, p *Provider, a cloud.Account) map[string]string {
	t.Helper()
	snap, err := p.Inventory(context.Background(), a, "eastus", cloud.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Resources {
		if r.Kind == cloud.KindInstance && r.Name == "web-1" {
			return r.Details
		}
	}
	t.Fatal("no web-1 in the inventory")
	return nil
}

func TestTheInventorySaysWhichVMsAreLocked(t *testing.T) {
	p, m, a := removable(t)
	ctx := context.Background()
	if d := lockOf(t, p, a); d["lock"] != "none" || d["lockNames"] != "" || d["lockAbove"] != "" {
		t.Fatalf("no lock: %v", d)
	}
	// Argus's own lock shows at once, not when the read in hand runs out.
	if _, err := p.Protect(ctx, a, "ada@example.com", "eastus", web1, true); err != nil {
		t.Fatal(err)
	}
	if d := lockOf(t, p, a); d["lock"] != "argus" || d["lockNames"] != "" {
		t.Fatalf("Argus's lock: %v", d)
	}
	// Somebody else's beside it is named, and so is one on the group.
	m.mu.Lock()
	m.locks = append(m.locks, "finance-hold")
	m.groupLocks = []string{"rg-freeze"}
	m.mu.Unlock()
	p.stale(a.AccountID)
	if d := lockOf(t, p, a); d["lock"] != "both" || d["lockNames"] != "finance-hold" || d["lockAbove"] != "resource group" {
		t.Fatalf("both: %v", d)
	}
	if _, err := p.Protect(ctx, a, "ada@example.com", "eastus", web1, false); err == nil {
		t.Fatal("the other lock is still there, and the answer must say so")
	}
	if d := lockOf(t, p, a); d["lock"] != "other" || d["lockNames"] != "finance-hold" {
		t.Fatalf("the other's lock: %v", d)
	}
}

func TestAnInventoryWithoutTheLocksSaysNothingOfThem(t *testing.T) {
	p, m, a := removable(t)
	m.noLockList = true
	d := lockOf(t, p, a)
	if _, said := d["lock"]; said || d["resourceGroup"] != "Web-RG" {
		t.Fatalf("locks that could not be read must not read as none: %v", d)
	}
}

func TestLocksAreSortedByWhatTheyAreOn(t *testing.T) {
	const sub = "/subscriptions/S"
	vm := sub + "/resourceGroups/Web-RG/providers/Microsoft.Compute/virtualMachines/web-1"
	idx := lockIndex([]raw{
		{ID: sub + "/providers/Microsoft.Authorization/locks/all", Name: "all"},
		{ID: sub + "/resourcegroups/web-rg/providers/Microsoft.Authorization/locks/rg", Name: "rg"},
		{ID: vm + "/providers/Microsoft.Authorization/locks/" + LockName, Name: LockName},
		{ID: sub + "/resourceGroups/Other/providers/Microsoft.Authorization/locks/elsewhere", Name: "elsewhere"},
		{ID: "not a lock", Name: "x"},
	})
	d := map[string]string{}
	idx.mark(d, vm)
	if d["lock"] != "argus" || d["lockAbove"] != "resource group and subscription" {
		t.Fatalf("details = %v", d)
	}
	d = map[string]string{}
	idx.mark(d, sub+"/resourceGroups/Third/providers/Microsoft.Compute/virtualMachines/api-1")
	if d["lock"] != "none" || d["lockAbove"] != "subscription" {
		t.Fatalf("details = %v", d)
	}
}

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

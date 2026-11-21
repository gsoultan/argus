package azure

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Protecting a VM. Azure has no switch on a VM that stops it being deleted;
what does that is a resource lock of level CanNotDelete, put on the VM
itself. While it is there nobody deletes the VM, from Argus, the portal or
anywhere else, and the VM can still be stopped, started, resized and
tagged.

Argus puts on one lock, always under the same name, and takes off only
that one. A lock somebody else put on the VM stays where it is: Argus
names it and leaves it to the portal.

Locks sit under Microsoft.Authorization, so the manage role holds two
actions there, locks/write and locks/delete, and nothing else of it: not
role assignments, not role definitions. Azure does not let a role be
limited to locks of one name, so the role could take off any lock in the
subscription; Argus only ever asks for its own.
*/

/*
The inventory says which VMs are locked. One read lists every lock in the
subscription, on whatever it sits; a VM's details then say whether Argus's
lock is on it ("argus"), somebody else's ("other"), both, or none, name
the others, and say when a lock on its resource group or the subscription
stands over it. Either level of lock, CanNotDelete or ReadOnly, stops a
delete, so the level is not told apart. When the locks could not be read,
the details say nothing of locks, which is not the same as "none".
*/

// lockScopes is the names of the locks on each scope, by the scope's path
// in lower case.
type lockScopes map[string][]string

const lockMarker = "/providers/microsoft.authorization/locks/"

// lockIndex sorts the subscription's locks by what they are on.
func lockIndex(locks []raw) lockScopes {
	out := lockScopes{}
	for _, l := range locks {
		id := strings.ToLower(l.ID)
		at := strings.LastIndex(id, lockMarker)
		if at < 0 || l.Name == "" {
			continue
		}
		out[id[:at]] = append(out[id[:at]], l.Name)
	}
	return out
}

// mark writes what the locks say of the VM at path vm into its details.
func (s lockScopes) mark(details map[string]string, vm string) {
	vm = strings.ToLower(strings.TrimRight(vm, "/"))
	mine, others := false, []string{}
	for _, name := range s[vm] {
		if strings.EqualFold(name, LockName) {
			mine = true
		} else {
			others = append(others, name)
		}
	}
	switch {
	case mine && len(others) > 0:
		details["lock"] = "both"
	case mine:
		details["lock"] = "argus"
	case len(others) > 0:
		details["lock"] = "other"
	default:
		details["lock"] = "none"
	}
	if len(others) > 0 {
		sort.Strings(others)
		details["lockNames"] = strings.Join(others, ", ")
	}
	// The scopes above a VM: its resource group, then the subscription.
	above := []string{}
	if at := strings.Index(vm, "/providers/"); at > 0 && len(s[vm[:at]]) > 0 {
		above = append(above, "resource group")
	}
	if at := strings.Index(vm, "/resourcegroups/"); at > 0 && len(s[vm[:at]]) > 0 {
		above = append(above, "subscription")
	}
	if len(above) > 0 {
		details["lockAbove"] = strings.Join(above, " and ")
	}
}

// LockName is the name of the lock Argus puts on a VM.
const LockName = "argus-delete-lock"

// lockNotes is what the lock says of itself in the portal.
const lockNotes = "Put on by Argus so this VM cannot be deleted. Turn its protection off in Argus to take it off."

// lock is one resource lock as Azure lists it.
type lock struct {
	Name string `json:"name"`
}

// locksOn is the locks on a VM itself.
func (p *Provider) locksOn(ctx context.Context, t token, actor cloud.Actor, m vm) ([]lock, error) {
	var out struct {
		Value []lock `json:"value"`
	}
	if err := p.get(ctx, t, actor, p.ep.arm+m.Path+"/providers/Microsoft.Authorization/locks?api-version="+lockAPI, &out); err != nil {
		return nil, fmt.Errorf("reading the locks on %s: %w", m.Name, err)
	}
	return out.Value, nil
}

// stale drops what was read of a subscription, so the next inventory
// reads it again: a lock just put on or taken off shows at once, not when
// the read in hand runs out.
func (p *Provider) stale(subscription string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.inv {
		if strings.HasPrefix(k, subscription+"|") {
			delete(p.inv, k)
		}
	}
}

// Protect puts Argus's delete lock on a VM, or takes it off.
func (p *Provider) Protect(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, on bool) (cloud.Result, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	m, err := p.locateVM(ctx, t, actor, a.AccountID, region, id)
	if err != nil {
		return cloud.Result{}, err
	}
	u := p.ep.arm + m.Path + "/providers/Microsoft.Authorization/locks/" + LockName + "?api-version=" + lockAPI
	if on {
		body := map[string]any{"properties": map[string]string{"level": "CanNotDelete", "notes": lockNotes}}
		req, err := p.send(ctx, t, actor, http.MethodPut, u, body, nil)
		if err != nil {
			return cloud.Result{}, err
		}
		p.stale(a.AccountID)
		return cloud.Result{ResourceID: id, RequestID: req}, nil
	}
	locks, err := p.locksOn(ctx, t, actor, m)
	if err != nil {
		return cloud.Result{}, err
	}
	mine, others := false, []string{}
	for _, l := range locks {
		if strings.EqualFold(l.Name, LockName) {
			mine = true
		} else {
			others = append(others, l.Name)
		}
	}
	if !mine {
		if len(others) > 0 {
			return cloud.Result{}, fmt.Errorf("the lock on %s (%s) was not put on by Argus; take it off in the Azure portal", m.Name, strings.Join(others, ", "))
		}
		return cloud.Result{ResourceID: id}, nil
	}
	req, err := p.send(ctx, t, actor, http.MethodDelete, u, nil, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	p.stale(a.AccountID)
	if len(others) > 0 {
		return cloud.Result{ResourceID: id, RequestID: req}, fmt.Errorf("Argus's lock is off %s, but it is still locked by %s, which Argus did not put on; take that off in the Azure portal", m.Name, strings.Join(others, ", "))
	}
	return cloud.Result{ResourceID: id, RequestID: req}, nil
}

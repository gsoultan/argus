package azure

import (
	"context"
	"fmt"
	"net/http"
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
	if len(others) > 0 {
		return cloud.Result{ResourceID: id, RequestID: req}, fmt.Errorf("Argus's lock is off %s, but it is still locked by %s, which Argus did not put on; take that off in the Azure portal", m.Name, strings.Join(others, ", "))
	}
	return cloud.Result{ResourceID: id, RequestID: req}, nil
}

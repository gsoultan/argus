package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Connecting an account without copying anything back. The roles have fixed
names, so Argus knows where they will be and watches for them (check); in
the account argus-control's own identity is in, it can also make the stack
itself when that identity is allowed to (deploy). Either way the roles are
the same, trust only this Argus and require its external id: Argus never
asks for the person's keys.
*/

type cloudConnectIn struct {
	Provider  string `json:"provider"`
	AccountID string `json:"accountId"`
	Manage    bool   `json:"manage"`
	// Name is what the account will be called, for the audit log.
	Name string `json:"name"`
}

// CloudCheck says whether the roles are there and work yet.
type CloudCheck struct {
	Ready bool `json:"ready"`
	// Detail is why not, in AWS's words.
	Detail string `json:"detail,omitempty"`
	// Stack is the argus-access stack, when Argus can see it: only in the
	// account its own identity is in.
	Stack *cloud.StackState `json:"stack,omitempty"`
}

// postCloudCheck assumes the roles the template makes, as the person
// asking, without keeping anything. The connect page asks every few
// seconds while the person makes the stack, and connects once it is ready.
// Nothing is recorded: a role not there yet is the expected answer, and
// connecting is audited when it happens.
func (a *API) postCloudCheck(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "connecting a cloud account")
	if !ok {
		return
	}
	var in cloudConnectIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.AccountID = strings.TrimSpace(in.AccountID)
	prep, code, err := a.cloudPrepare(r.Context(), in.Provider, in.AccountID, in.Manage)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	p, _ := a.cloudProvider(in.Provider)
	out := CloudCheck{}
	if d, ok := p.(cloud.Deployer); ok && prep.CanDeploy {
		sctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		if st, err := d.AccessStack(sctx, in.AccountID); err == nil && st.Status != "" {
			out.Stack = &st
		}
		cancel()
	}
	acct := cloud.Account{Provider: in.Provider, AccountID: in.AccountID, RoleARN: prep.ReadRoleARN, ExternalID: prep.ExternalID}
	vctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if _, err := p.Verify(vctx, acct, cloud.Actor(actor)); err != nil {
		out.Detail = "The read role does not let Argus in yet: " + err.Error()
	} else if in.Manage {
		acct.RoleARN = prep.ManageRoleARN
		if _, err := p.Verify(vctx, acct, cloud.Actor(actor)); err != nil {
			out.Detail = "The read role works; the manage role does not let Argus in yet: " + err.Error()
		} else {
			out.Ready = true
		}
	} else {
		out.Ready = true
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// postCloudDeploy makes the argus-access stack with argus-control's own
// identity, in the one account that identity is in. Whether it is allowed
// to is up to whoever gave it its permissions; when it is not, the person
// gets the steps to do it themselves.
func (a *API) postCloudDeploy(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "connecting a cloud account")
	if !ok {
		return
	}
	var in cloudConnectIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.AccountID, in.Name = strings.TrimSpace(in.AccountID), strings.TrimSpace(in.Name)
	if len(in.Name) > 64 {
		in.Name = in.Name[:64]
	}
	prep, code, err := a.cloudPrepare(r.Context(), in.Provider, in.AccountID, in.Manage)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	p, _ := a.cloudProvider(in.Provider)
	d, ok := p.(cloud.Deployer)
	if !ok || !prep.CanDeploy {
		writeErr(w, http.StatusConflict, "argus-control's own identity ("+prep.Principal+") is not in account "+in.AccountID+
			", so it cannot make the roles there. Make them with the command or the template instead.")
		return
	}
	roles := prep.ReadRoleARN
	if in.Manage {
		roles += " and " + prep.ManageRoleARN
	}
	target := cloudTarget(in.Provider, in.AccountID, in.Name)
	dctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	st, err := d.DeployAccess(dctx, in.AccountID, prep.Template, cloud.Actor(actor))
	cancel()
	if err != nil {
		a.auditAsset(r, AuditEvent{Action: "cloud.account.stack_refused", Severity: "notice", ActorEmail: actor, Target: target,
			Detail: "Argus could not make the argus-access stack with its own identity " + prep.Principal + ": " + err.Error()})
		status := http.StatusUnprocessableEntity
		if errors.Is(err, cloud.ErrNotHomeAccount) {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.account.stack_made", Severity: "notice", ActorEmail: actor, Target: target,
		Detail: "Argus asked CloudFormation for the argus-access stack (" + roles + ") with its own identity " + prep.Principal +
			", for " + actor + ". The stack is tagged argus:requested-by=" + actor + "."})
	writeJSON(w, http.StatusAccepted, st)
}

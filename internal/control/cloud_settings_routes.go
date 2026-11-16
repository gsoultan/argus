package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
An instance's settings: stop protection, whether its metadata service needs
a token (IMDSv2), and a burstable instance's credit mode.

They are read from the provider when the dialog opens, since the inventory
does not carry all of them, and changed as one "settings" change through
the same plan, approval and audit as any other. The change names only what
differs from now, so an approver reads exactly what will move.
*/

// cloudSettingsSpec is a settings change as asked and as planned: To is
// what is set, From what it was when planned.
type cloudSettingsSpec struct {
	To   cloud.InstanceSettings `json:"to"`
	From cloud.InstanceConfig   `json:"from"`
}

// settingsNeeds is the permission each setting needs, so a role made
// before a setting existed is refused only for that setting.
func settingsNeeds(s cloud.InstanceSettings) []string {
	var out []string
	if s.StopProtect != nil {
		out = append(out, "ec2:ModifyInstanceAttribute")
	}
	if s.RequireTokens != nil {
		out = append(out, "ec2:ModifyInstanceMetadataOptions")
	}
	if s.CreditMode != "" {
		out = append(out, "ec2:ModifyInstanceCreditSpecification")
	}
	return out
}

func (a *API) planSettings(r *http.Request, m cloudManaged, in *cloudChangeIn, inst cloud.Resource, ch CloudChange) (CloudChange, int, error) {
	cf, ok := m.manager.(cloud.Configurer)
	if !ok {
		return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot change instance settings yet")
	}
	var spec cloudSettingsSpec
	if err := json.Unmarshal(in.Spec, &spec.To); err != nil {
		return ch, http.StatusBadRequest, errors.New("settings are {stopProtect, requireTokens, creditMode}")
	}
	to := &spec.To
	if to.CreditMode != "" && to.CreditMode != "standard" && to.CreditMode != "unlimited" {
		return ch, http.StatusBadRequest, errors.New("credit mode is standard or unlimited")
	}
	if to.CreditMode != "" && !cloud.Burstable(inst.Size) {
		return ch, http.StatusBadRequest, errors.New(inst.Name + " is " + inst.Size + ", which has no credit mode; only t2, t3, t3a and t4g sizes do")
	}
	cur, err := cf.InstanceConfig(r.Context(), m.target, cloud.Actor(ch.RequestedBy), inst.Region, inst.ID)
	if err != nil {
		return ch, http.StatusBadGateway, errors.New("Argus could not read the instance's settings: " + err.Error())
	}
	// Keep only what changes.
	if to.StopProtect != nil && *to.StopProtect == cur.StopProtect {
		to.StopProtect = nil
	}
	if to.RequireTokens != nil && *to.RequireTokens == cur.RequireTokens {
		to.RequireTokens = nil
	}
	if to.CreditMode == cur.CreditMode {
		to.CreditMode = ""
	}
	if to.Empty() {
		return ch, http.StatusBadRequest, errors.New("nothing to change; " + inst.Name + " is already set that way")
	}
	if err := a.missingFor(m, settingsNeeds(*to)); err != nil {
		return ch, http.StatusConflict, err
	}
	spec.From = cur
	ch.Spec, _ = json.Marshal(spec)
	ch.Summary = settingsSummary(inst.Name, *to)
	return ch, http.StatusOK, nil
}

func settingsSummary(name string, s cloud.InstanceSettings) string {
	var parts []string
	if s.StopProtect != nil {
		parts = append(parts, "turn "+onOff(*s.StopProtect)+" stop protection")
	}
	if s.RequireTokens != nil {
		if *s.RequireTokens {
			parts = append(parts, "require tokens for instance metadata (IMDSv2)")
		} else {
			parts = append(parts, "allow instance metadata without a token (IMDSv1)")
		}
	}
	if s.CreditMode != "" {
		parts = append(parts, "set the credit mode to "+s.CreditMode)
	}
	out := strings.Join(parts, ", ")
	return strings.ToUpper(out[:1]) + out[1:] + " on " + name
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func applySettings(ctx context.Context, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	cf, ok := m.manager.(cloud.Configurer)
	if !ok {
		return cloud.Result{}, errors.New(m.acct.Provider + " cannot change instance settings yet")
	}
	var spec cloudSettingsSpec
	if err := json.Unmarshal(ch.Spec, &spec); err != nil {
		return cloud.Result{}, fmt.Errorf("the change's settings could not be read: %w", err)
	}
	return cf.Configure(ctx, m.target, cloud.Actor(ch.RequestedBy), ch.Region, ch.ResourceID, spec.To)
}

// missingFor refuses when the manage role lacks any of needs, saying how
// to add them.
func (a *API) missingFor(m cloudManaged, needs []string) error {
	if m.acct.ManageCheckedAt == nil {
		return nil
	}
	var lacks []string
	for _, need := range needs {
		if slices.Contains(m.acct.ManageMissing, need) {
			lacks = append(lacks, need)
		}
	}
	if len(lacks) == 0 {
		return nil
	}
	return fmt.Errorf("the manage role in %s was made before Argus could do this and lacks %s. "+
		"Update the role from the account's row on the Cloud overview: deploy the template it shows over the argus-access stack, and Argus sees the change at its next read",
		m.acct.Name, strings.Join(lacks, ", "))
}

// getCloudInstanceSettings reads an instance's settings for the dialog.
func (a *API) getCloudInstanceSettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudChanger(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	m, code, err := a.cloudManagedAccount(r.Context(), q.Get("accountId"))
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	cf, ok := m.manager.(cloud.Configurer)
	if !ok {
		writeErr(w, http.StatusNotImplemented, m.acct.Provider+" cannot change instance settings yet")
		return
	}
	inst, err := a.store.CloudInstance(r.Context(), m.acct.ID, q.Get("region"), q.Get("resourceId"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	cur, err := cf.InstanceConfig(r.Context(), m.target, cloud.Actor(actor), inst.Region, inst.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Argus could not read the instance's settings: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, cur)
}

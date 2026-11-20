package azure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Changes in a subscription go through a second role assignment. Azure gives
argus-control one identity, so there is no second account to switch to as
there is in AWS and Google Cloud; what stands in for it is a custom role,
"Argus manage", made in the subscription by the same setup command when the
person allows changes, and assigned to argus-control with this Argus's
external id in the assignment's description. The role lists only the
actions Argus takes, and nothing under Microsoft.Authorization, so it
cannot give itself more.

Argus makes a change only when that assignment is there and carries the
external id, checked the same way as the Reader one. Taking the assignment
away in the portal stops changes at once; the Reader assignment and the
inventory stay.

A VM is known to Argus by its vmId, which Azure does not take in a path, so
each change first lists the subscription's VMs to find the one with that id
and builds the path from its resource group and name.

Calls return as soon as Azure has accepted the operation, as AWS's and
Google's do. The correlation id is kept as the request id: the
subscription's Activity log finds the operation by it.
*/

// ManageRoleName is what the custom role is called in the portal, before
// the subscription id that keeps the name unique in its directory.
const ManageRoleName = "Argus manage"

// ManageActions are the Azure actions the manage role allows: what Argus
// does in a subscription so far, and no more.
var ManageActions = []string{
	"Microsoft.Compute/virtualMachines/read",
	"Microsoft.Compute/virtualMachines/start/action",
	"Microsoft.Compute/virtualMachines/deallocate/action",
	"Microsoft.Compute/virtualMachines/restart/action",
	"Microsoft.Compute/virtualMachines/write",
	"Microsoft.Compute/virtualMachines/vmSizes/read",
	"Microsoft.Resources/tags/read",
	"Microsoft.Resources/tags/write",
	"Microsoft.Resources/tags/delete",
	// Disks and snapshots. Azure checks beginGetAccess on what a copy is
	// made from: on the disk a snapshot is taken of, and on the snapshot a
	// disk is made from. The same two actions also let their holder ask
	// for a download link to that disk or snapshot, which Argus never does.
	"Microsoft.Compute/virtualMachines/attachDetachDataDisks/action",
	"Microsoft.Compute/disks/read",
	"Microsoft.Compute/disks/write",
	"Microsoft.Compute/disks/delete",
	"Microsoft.Compute/disks/beginGetAccess/action",
	"Microsoft.Compute/snapshots/read",
	"Microsoft.Compute/snapshots/write",
	"Microsoft.Compute/snapshots/delete",
	"Microsoft.Compute/snapshots/beginGetAccess/action",
}

// ManageRoleID is the id of a subscription's manage role: a GUID, as Azure
// requires, worked out from the subscription so the command and the
// control plane agree on it without anything being copied back.
func ManageRoleID(subscription string) string {
	sum := sha256.Sum256([]byte("argus manage role:" + subscription))
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ManageDescription is what the manage assignment's description says.
func ManageDescription(externalID string) string {
	return "Argus makes changes in this subscription through this assignment. " + ExternalIDMarker + externalID
}

// supported are the changes Argus makes in an Azure subscription so far.
var supported = map[cloud.Action]bool{
	cloud.ActionStart:  true,
	cloud.ActionStop:   true,
	cloud.ActionReboot: true,
	cloud.ActionResize: true,
	cloud.ActionTags:   true,

	cloud.ActionVolumeCreate: true, cloud.ActionVolumeGrow: true, cloud.ActionVolumeAttach: true,
	cloud.ActionVolumeDetach: true, cloud.ActionVolumeDelete: true,
	cloud.ActionSnapshotCreate: true, cloud.ActionSnapshotDelete: true,
}

// TaggableKinds are the kinds Argus tags in Azure so far.
var TaggableKinds = []cloud.Kind{cloud.KindInstance, cloud.KindVolume, cloud.KindSnapshot}

// Supports reports whether Argus can make change a in a subscription yet.
func (p *Provider) Supports(a cloud.Action) bool { return supported[a] }

// SupportedActions is every action Supports allows, for the console.
func SupportedActions() []cloud.Action {
	return []cloud.Action{cloud.ActionStart, cloud.ActionStop, cloud.ActionReboot, cloud.ActionResize, cloud.ActionTags,
		cloud.ActionVolumeCreate, cloud.ActionVolumeGrow, cloud.ActionVolumeAttach, cloud.ActionVolumeDetach, cloud.ActionVolumeDelete,
		cloud.ActionSnapshotCreate, cloud.ActionSnapshotDelete}
}

// errNotYet refuses a change Argus does not make in Azure yet.
func errNotYet(what string) error {
	return fmt.Errorf("%w: Argus cannot %s in Azure yet; do it in the Azure portal", cloud.ErrUnsupported, what)
}

var (
	reVMSize = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(_[A-Za-z0-9-]+)+$`)
	// Azure's own rule for the names inside a resource path Argus builds.
	reGroupName = regexp.MustCompile(`^[-\w._()]{1,90}$`)
	reVMName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ValidSize reports whether s looks like a VM size, such as Standard_B2s
// or Standard_D4s_v5.
func ValidSize(s string) bool { return len(s) <= 64 && reVMSize.MatchString(s) }

// MaxTags is the most tags Azure keeps on one resource.
const MaxTags = 50

// TagProblem says why k=v cannot be a tag, or "" when it can. Azure keeps
// names starting with microsoft, azure and windows for itself.
func TagProblem(k, v string) string {
	switch {
	case k == "" || len(k) > 512:
		return "a tag name holds 1 to 512 characters"
	case strings.ContainsAny(k, `<>%&\?/`) || strings.ContainsFunc(k, func(r rune) bool { return r < 0x20 }):
		return `a tag name cannot hold < > % & \ ? or /`
	case len(v) > 256:
		return "a tag value holds up to 256 characters"
	}
	low := strings.ToLower(k)
	for _, pre := range []string{"microsoft", "azure", "windows"} {
		if strings.HasPrefix(low, pre) {
			return "Azure keeps tag names starting with " + pre
		}
	}
	return ""
}

// vm is one virtual machine as a change needs it.
type vm struct {
	Name     string
	Path     string // /subscriptions/…/virtualMachines/name, built by Argus
	Location string
	Tags     map[string]string
}

// locateVM finds the VM with vmId id in the region the console named. The
// path is built from the resource group and name Azure listed, each
// checked, never taken whole from Azure's answer.
func (p *Provider) locateVM(ctx context.Context, t token, actor cloud.Actor, subscription, region, id string) (vm, error) {
	if !ValidGUID(id) {
		return vm{}, fmt.Errorf("%q is not a VM id", id)
	}
	var found *raw
	err := p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Compute/virtualMachines?api-version="+computeAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for i := range items {
				if strings.EqualFold(items[i].Properties.VMID, id) {
					found = &items[i]
				}
			}
			return nil
		})
	if err != nil {
		return vm{}, err
	}
	if found == nil {
		return vm{}, fmt.Errorf("subscription %s has no VM with id %s", subscription, id)
	}
	if got := strings.ToLower(found.Location); got != region {
		return vm{}, fmt.Errorf("%s is in %s, not %s", found.Name, got, region)
	}
	rg := group(found.ID)
	if !reGroupName.MatchString(rg) || !reVMName.MatchString(found.Name) {
		return vm{}, fmt.Errorf("Azure named VM %s in a way Argus cannot address", id)
	}
	return vm{Name: found.Name, Location: region, Tags: found.Tags,
		Path: SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(rg) + "/providers/Microsoft.Compute/virtualMachines/" + url.PathEscape(found.Name)}, nil
}

// send makes a changing call and returns Azure's id for it once Azure has
// accepted it. v, when not nil, takes the answer's body.
func (p *Provider) send(ctx context.Context, t token, actor cloud.Actor, method, u string, body, v any) (string, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+t.value)
	req.Header.Set("User-Agent", userAgent(actor))
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
	default:
		return "", azureError(resp.StatusCode, b)
	}
	if v != nil && len(b) > 0 {
		if err := json.Unmarshal(b, v); err != nil {
			return "", fmt.Errorf("Azure's answer: %w", err)
		}
	}
	id := resp.Header.Get("x-ms-correlation-request-id")
	if id == "" {
		id = resp.Header.Get("x-ms-request-id")
	}
	if len(id) > 64 {
		id = id[:64]
	}
	return id, nil
}

// manage opens the session a change runs in and finds its VM.
func (p *Provider) manage(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string) (token, vm, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return token{}, vm{}, err
	}
	m, err := p.locateVM(ctx, t, actor, a.AccountID, region, id)
	return t, m, err
}

// Power starts, stops or restarts a VM. A stop deallocates it, as the
// portal's Stop does: Azure goes on charging for a VM that is only shut
// down, and stops once it is deallocated.
func (p *Provider) Power(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, act cloud.Action) (cloud.Result, error) {
	verb := map[cloud.Action]string{cloud.ActionStart: "start", cloud.ActionStop: "deallocate", cloud.ActionReboot: "restart"}[act]
	if verb == "" {
		return cloud.Result{}, errNotYet(string(act) + " a VM")
	}
	t, m, err := p.manage(ctx, a, actor, region, id)
	if err != nil {
		return cloud.Result{}, err
	}
	req, err := p.send(ctx, t, actor, http.MethodPost, p.ep.arm+m.Path+"/"+verb+"?api-version="+computeAPI, nil, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: req}, nil
}

// Resize changes a VM's size.
func (p *Provider) Resize(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id, size string) (cloud.Result, error) {
	if !ValidSize(size) {
		return cloud.Result{}, fmt.Errorf("%q is not a VM size, like Standard_B2s", size)
	}
	t, m, err := p.manage(ctx, a, actor, region, id)
	if err != nil {
		return cloud.Result{}, err
	}
	body := map[string]any{"properties": map[string]any{"hardwareProfile": map[string]string{"vmSize": size}}}
	req, err := p.send(ctx, t, actor, http.MethodPatch, p.ep.arm+m.Path+"?api-version="+computeAPI, body, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: req}, nil
}

// Sizes are the sizes a VM can change to where it is now, as Azure lists
// them for that VM.
func (p *Provider) Sizes(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string) ([]string, error) {
	t, m, err := p.manage(ctx, a, actor, region, id)
	if err != nil {
		return nil, err
	}
	var out []string
	err = p.pages(ctx, t, actor, p.ep.arm+m.Path+"/vmSizes?api-version="+computeAPI, func(v json.RawMessage) error {
		var items []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(v, &items); err != nil {
			return err
		}
		for _, s := range items {
			if ValidSize(s.Name) {
				out = append(out, s.Name)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

// SetTags changes a VM's tags.
func (p *Provider) SetTags(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, set map[string]string, remove []string) (cloud.Result, error) {
	return p.SetTagsOf(ctx, a, actor, region, cloud.KindInstance, id, set, remove)
}

// SetTagsOf changes the tags of a resource of kind. Azure merges and
// deletes tags by name on its side, so tags someone else is changing at
// the same moment are not overwritten.
func (p *Provider) SetTagsOf(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, kind cloud.Kind, id string, set map[string]string, remove []string) (cloud.Result, error) {
	if !slices.Contains(TaggableKinds, kind) {
		return cloud.Result{}, errNotYet("tag a " + string(kind))
	}
	for k, v := range set {
		if why := TagProblem(k, v); why != "" {
			return cloud.Result{}, fmt.Errorf("%s=%s: %s", k, v, why)
		}
	}
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	var m vm
	switch kind {
	case cloud.KindInstance:
		m, err = p.locateVM(ctx, t, actor, a.AccountID, region, id)
	case cloud.KindVolume, cloud.KindSnapshot:
		var h held
		h, err = p.locateHeld(ctx, t, actor, a.AccountID, region, map[cloud.Kind]string{cloud.KindVolume: "disks", cloud.KindSnapshot: "snapshots"}[kind], id)
		m = vm{Name: h.Name, Path: h.Path, Location: h.Location, Tags: h.Tags}
	}
	if err != nil {
		return cloud.Result{}, err
	}
	// Azure matches tag names whatever their case, and deletes a tag by
	// its name and the value it has now.
	now := map[string]string{}
	for k := range m.Tags {
		now[strings.ToLower(k)] = k
	}
	gone := map[string]string{}
	for _, k := range remove {
		if has, ok := now[strings.ToLower(k)]; ok {
			if _, again := set[k]; !again {
				gone[has] = m.Tags[has]
			}
		}
	}
	added := 0
	for k := range set {
		if _, ok := now[strings.ToLower(k)]; !ok {
			added++
		}
	}
	if n := len(m.Tags) - len(gone) + added; n > MaxTags {
		return cloud.Result{}, fmt.Errorf("%s would have %d tags; Azure keeps at most %d", m.Name, n, MaxTags)
	}
	u := p.ep.arm + m.Path + "/providers/Microsoft.Resources/tags/default?api-version=" + tagsAPI
	req := ""
	if len(gone) > 0 {
		if req, err = p.send(ctx, t, actor, http.MethodPatch, u, tagPatch("Delete", gone), nil); err != nil {
			return cloud.Result{}, err
		}
	}
	if len(set) > 0 {
		if req, err = p.send(ctx, t, actor, http.MethodPatch, u, tagPatch("Merge", set), nil); err != nil {
			if len(gone) > 0 {
				err = fmt.Errorf("the tags to remove were removed, but the new ones were not set: %w", err)
			}
			return cloud.Result{}, err
		}
	}
	return cloud.Result{ResourceID: id, RequestID: req}, nil
}

// tagsAPI is the Microsoft.Resources version that merges and deletes tags.
const tagsAPI = "2021-04-01"

func tagPatch(op string, tags map[string]string) map[string]any {
	return map[string]any{"operation": op, "properties": map[string]any{"tags": tags}}
}

// The rest of what a Manager does comes to Azure later; Supports says so
// before any of these is asked.

func (p *Provider) Images(context.Context, cloud.Account, cloud.Actor, string) ([]cloud.Image, error) {
	return nil, errNotYet("launch a VM")
}

func (p *Provider) Launch(context.Context, cloud.Account, cloud.Actor, cloud.LaunchSpec) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("launch a VM")
}

func (p *Provider) Protect(context.Context, cloud.Account, cloud.Actor, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("lock a VM against deletion")
}

func (p *Provider) PlanRemove(context.Context, cloud.Account, cloud.Actor, string, string) (cloud.RemovePlan, error) {
	return cloud.RemovePlan{}, errNotYet("remove a VM")
}

func (p *Provider) Remove(context.Context, cloud.Account, cloud.Actor, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("remove a VM")
}

var (
	_ cloud.Manager    = (*Provider)(nil)
	_ cloud.KindTagger = (*Provider)(nil)
	_ cloud.SizeLister = (*Provider)(nil)
	_ cloud.Supporter  = (*Provider)(nil)
)

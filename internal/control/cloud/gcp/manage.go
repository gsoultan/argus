package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Changes in a project go through a second service account, argus-manage,
made by the same setup command when the person allows changes. It is held
to the same rule as argus-read: Argus never has a key for it, every token
is minted with argus-control's own identity, and a token is used only when
argus-manage's description carries this Argus's external id.

Google takes a Compute Engine resource's numeric id wherever it takes its
name, so the ids the inventory stored are used as they are; only the zone
(or, for a regional disk, the region) is looked up, since the inventory's
region is not enough to name a zonal resource.

Calls return as soon as Google has accepted the operation, as AWS's do.
Its name is kept as the request id: Cloud Audit Logs and the Operations
list find it by that.
*/

// ManageAccountName is the service account changes are made through.
const ManageAccountName = "argus-manage"

// supported are the changes Argus makes in a Google Cloud project so far.
var supported = map[cloud.Action]bool{
	cloud.ActionStart:      true,
	cloud.ActionStop:       true,
	cloud.ActionReboot:     true,
	cloud.ActionVolumeGrow: true,
}

// Supports reports whether Argus can make change a in a Google Cloud
// project yet.
func (p *Provider) Supports(a cloud.Action) bool { return supported[a] }

// SupportedActions is every action Supports allows, for the console.
func SupportedActions() []cloud.Action {
	var out []cloud.Action
	for _, a := range []cloud.Action{cloud.ActionStart, cloud.ActionStop, cloud.ActionReboot, cloud.ActionVolumeGrow} {
		if supported[a] {
			out = append(out, a)
		}
	}
	return out
}

// errNotYet refuses a change Argus does not make in Google Cloud yet.
func errNotYet(what string) error {
	return fmt.Errorf("%w: Argus cannot %s in Google Cloud yet; do it in the Google Cloud console", cloud.ErrUnsupported, what)
}

var reID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// where is a resource's place: its zone, or the region of a regional disk.
type where struct {
	Name   string
	Zone   string
	Region string
}

func (w where) path(project, kind string) string {
	if w.Zone != "" {
		return "/compute/v1/projects/" + project + "/zones/" + w.Zone + "/" + kind + "/"
	}
	return "/compute/v1/projects/" + project + "/regions/" + w.Region + "/" + kind + "/"
}

// locate finds where the resource of kind ("instances" or "disks") with
// id is, in the region the console named.
func (p *Provider) locate(ctx context.Context, t token, actor cloud.Actor, project, region, kind, id string) (where, error) {
	if !reID.MatchString(id) {
		return where{}, fmt.Errorf("%q is not a Compute Engine id", id)
	}
	q := url.Values{"filter": {"id = " + id}, "fields": {"items/*/" + kind + "(id,name,zone,region)"}}
	// Each scope holds the list under kind, or a warning when it is empty.
	var out struct {
		Items map[string]map[string]json.RawMessage `json:"items"`
	}
	if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+project+"/aggregated/"+kind+"?"+q.Encode(), &out); err != nil {
		return where{}, err
	}
	for _, scope := range out.Items {
		var list []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Zone   string `json:"zone"`
			Region string `json:"region"`
		}
		if raw, ok := scope[kind]; !ok || json.Unmarshal(raw, &list) != nil {
			continue
		}
		for _, r := range list {
			if r.ID != id {
				continue
			}
			w := where{Name: r.Name, Zone: last(r.Zone), Region: last(r.Region)}
			got := w.Region
			if w.Zone != "" {
				got = regionOfZone(w.Zone)
			}
			if got != region {
				return where{}, fmt.Errorf("%s is in %s, not %s", r.Name, got, region)
			}
			return w, nil
		}
	}
	return where{}, fmt.Errorf("project %s has no %s with id %s", project, kind[:len(kind)-1], id)
}

// operation is the part of a Compute Engine operation Argus reads.
type operation struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  *struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

func (o operation) err() error {
	if o.Error == nil || len(o.Error.Errors) == 0 {
		return nil
	}
	return errors.New(o.Error.Errors[0].Message)
}

// post starts an operation and returns it once Google has accepted it.
func (p *Provider) post(ctx context.Context, t token, actor cloud.Actor, u string, body any) (operation, error) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, rd)
	if err != nil {
		return operation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.value)
	req.Header.Set("User-Agent", userAgent(actor))
	var op operation
	if err := p.do(req, &op); err != nil {
		return operation{}, err
	}
	return op, op.err()
}

// Power starts, stops or resets an instance. Google has no gentle reboot:
// a reset is the power button, so the console says so before it asks.
func (p *Provider) Power(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, act cloud.Action) (cloud.Result, error) {
	verb := map[cloud.Action]string{cloud.ActionStart: "start", cloud.ActionStop: "stop", cloud.ActionReboot: "reset"}[act]
	if verb == "" {
		return cloud.Result{}, errNotYet(string(act) + " an instance")
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	w, err := p.locate(ctx, t, actor, a.AccountID, region, "instances", id)
	if err != nil {
		return cloud.Result{}, err
	}
	if w.Zone == "" {
		return cloud.Result{}, fmt.Errorf("%s has no zone", w.Name)
	}
	op, err := p.post(ctx, t, actor, p.ep.compute+w.path(a.AccountID, "instances")+id+"/"+verb, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: op.Name}, nil
}

// ModifyVolume grows a disk. Google changes neither a disk's type nor its
// performance through a resize, so a change asks for size alone.
func (p *Provider) ModifyVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, c cloud.VolumeChange) (cloud.Result, error) {
	if c.Type != "" || c.IOPS != 0 || c.Throughput != 0 {
		return cloud.Result{}, errNotYet("change a disk's type or performance")
	}
	if c.SizeGB <= 0 {
		return cloud.Result{}, errors.New("say the disk's new size")
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	w, err := p.locate(ctx, t, actor, a.AccountID, region, "disks", id)
	if err != nil {
		return cloud.Result{}, err
	}
	op, err := p.post(ctx, t, actor, p.ep.compute+w.path(a.AccountID, "disks")+id+"/resize",
		map[string]any{"sizeGb": fmt.Sprint(c.SizeGB)})
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: op.Name}, nil
}

// The rest of cloud.Manager and cloud.Storage: refused until each is built.

func (p *Provider) Images(context.Context, cloud.Account, cloud.Actor, string) ([]cloud.Image, error) {
	return nil, errNotYet("list images to launch from")
}
func (p *Provider) Launch(context.Context, cloud.Account, cloud.Actor, cloud.LaunchSpec) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("launch an instance")
}
func (p *Provider) Resize(context.Context, cloud.Account, cloud.Actor, string, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("change an instance's machine type")
}
func (p *Provider) SetTags(context.Context, cloud.Account, cloud.Actor, string, string, map[string]string, []string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("change labels")
}
func (p *Provider) Protect(context.Context, cloud.Account, cloud.Actor, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("change deletion protection")
}
func (p *Provider) PlanRemove(context.Context, cloud.Account, cloud.Actor, string, string) (cloud.RemovePlan, error) {
	return cloud.RemovePlan{}, errNotYet("remove an instance")
}
func (p *Provider) Remove(context.Context, cloud.Account, cloud.Actor, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("remove an instance")
}
func (p *Provider) CreateVolume(context.Context, cloud.Account, cloud.Actor, string, cloud.VolumeSpec) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("make a disk")
}
func (p *Provider) AttachVolume(context.Context, cloud.Account, cloud.Actor, string, string, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("attach a disk")
}
func (p *Provider) DetachVolume(context.Context, cloud.Account, cloud.Actor, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("detach a disk")
}
func (p *Provider) DeleteVolume(context.Context, cloud.Account, cloud.Actor, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("delete a disk")
}
func (p *Provider) CreateSnapshot(context.Context, cloud.Account, cloud.Actor, string, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("take a snapshot")
}
func (p *Provider) DeleteSnapshot(context.Context, cloud.Account, cloud.Actor, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("delete a snapshot")
}
func (p *Provider) CreateImage(context.Context, cloud.Account, cloud.Actor, string, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("make an image")
}
func (p *Provider) DeleteImage(context.Context, cloud.Account, cloud.Actor, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("delete an image")
}
func (p *Provider) AllocateAddress(context.Context, cloud.Account, cloud.Actor, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("reserve an address")
}
func (p *Provider) AssociateAddress(context.Context, cloud.Account, cloud.Actor, string, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("assign an address")
}
func (p *Provider) DisassociateAddress(context.Context, cloud.Account, cloud.Actor, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("unassign an address")
}
func (p *Provider) ReleaseAddress(context.Context, cloud.Account, cloud.Actor, string, string) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("release an address")
}

var (
	_ cloud.Manager   = (*Provider)(nil)
	_ cloud.Storage   = (*Provider)(nil)
	_ cloud.Supporter = (*Provider)(nil)
)

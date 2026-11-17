package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

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
	cloud.ActionResize:     true,
	cloud.ActionTags:       true,
	cloud.ActionProtect:    true,
	cloud.ActionUnprotect:  true,

	cloud.ActionVolumeCreate:   true,
	cloud.ActionVolumeAttach:   true,
	cloud.ActionVolumeDetach:   true,
	cloud.ActionVolumeDelete:   true,
	cloud.ActionSnapshotCreate: true,
	cloud.ActionSnapshotDelete: true,

	cloud.ActionAddressAllocate:     true,
	cloud.ActionAddressAssociate:    true,
	cloud.ActionAddressDisassociate: true,
	cloud.ActionAddressRelease:      true,

	cloud.ActionRemove: true,
	cloud.ActionLaunch: true,
}

// TaggableKinds are the kinds Argus labels in Google Cloud so far.
var TaggableKinds = []cloud.Kind{cloud.KindInstance, cloud.KindVolume}

// Supports reports whether Argus can make change a in a Google Cloud
// project yet.
func (p *Provider) Supports(a cloud.Action) bool { return supported[a] }

// SupportedActions is every action Supports allows, for the console.
func SupportedActions() []cloud.Action {
	var out []cloud.Action
	for _, a := range []cloud.Action{cloud.ActionStart, cloud.ActionStop, cloud.ActionReboot,
		cloud.ActionResize, cloud.ActionTags, cloud.ActionProtect, cloud.ActionUnprotect, cloud.ActionVolumeGrow,
		cloud.ActionVolumeCreate, cloud.ActionVolumeAttach, cloud.ActionVolumeDetach, cloud.ActionVolumeDelete,
		cloud.ActionSnapshotCreate, cloud.ActionSnapshotDelete,
		cloud.ActionAddressAllocate, cloud.ActionAddressAssociate, cloud.ActionAddressDisassociate, cloud.ActionAddressRelease,
		cloud.ActionRemove, cloud.ActionLaunch} {
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

// locate finds where the resource of kind ("instances", "disks" or "addresses") with
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
	return where{}, fmt.Errorf("project %s has no %s with id %s", project, map[string]string{"instances": "instance", "disks": "disk", "addresses": "address"}[kind], id)
}

// operation is the part of a Compute Engine operation Argus reads.
type operation struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	SelfLink string `json:"selfLink"`
	TargetID string `json:"targetId"`
	Error    *struct {
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

var (
	reMachineType = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)+$`)
	reLabelKey    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	reLabelValue  = regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
)

// ValidMachineType reports whether s looks like a Compute Engine machine
// type, such as e2-medium, n2-standard-4 or n2-custom-4-16384.
func ValidMachineType(s string) bool { return len(s) <= 63 && reMachineType.MatchString(s) }

// MaxLabels is the most labels Google keeps on one resource.
const MaxLabels = 64

// LabelProblem says why k=v cannot be a label, or "" when it can. Google
// keeps labels lowercase, and its own start with goog-.
func LabelProblem(k, v string) string {
	switch {
	case !reLabelKey.MatchString(k):
		return "a label key starts with a lowercase letter and holds up to 63 lowercase letters, digits, _ and -"
	case strings.HasPrefix(k, "goog-"):
		return "Google keeps labels starting with goog-"
	case !reLabelValue.MatchString(v):
		return "a label value holds up to 63 lowercase letters, digits, _ and -"
	}
	return ""
}

// Resize changes a stopped instance's machine type.
func (p *Provider) Resize(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id, size string) (cloud.Result, error) {
	if !ValidMachineType(size) {
		return cloud.Result{}, fmt.Errorf("%q is not a machine type, like e2-medium", size)
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
	op, err := p.post(ctx, t, actor, p.ep.compute+w.path(a.AccountID, "instances")+id+"/setMachineType",
		map[string]any{"machineType": "zones/" + w.Zone + "/machineTypes/" + size})
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: op.Name}, nil
}

// Sizes are the machine types an instance can change to: those of its
// zone that Google has not deprecated.
func (p *Provider) Sizes(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string) ([]string, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return nil, err
	}
	w, err := p.locate(ctx, t, actor, a.AccountID, region, "instances", id)
	if err != nil {
		return nil, err
	}
	if w.Zone == "" {
		return nil, fmt.Errorf("%s has no zone", w.Name)
	}
	var out []string
	next := ""
	for page := 0; page < 20; page++ {
		q := url.Values{"fields": {"items(name,deprecated),nextPageToken"}, "maxResults": {"500"}}
		if next != "" {
			q.Set("pageToken", next)
		}
		var list struct {
			Items []struct {
				Name       string          `json:"name"`
				Deprecated json.RawMessage `json:"deprecated"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+a.AccountID+"/zones/"+w.Zone+"/machineTypes?"+q.Encode(), &list); err != nil {
			return nil, err
		}
		for _, m := range list.Items {
			if len(m.Deprecated) == 0 {
				out = append(out, m.Name)
			}
		}
		if next = list.NextPageToken; next == "" {
			break
		}
	}
	slices.Sort(out)
	return out, nil
}

// SetTags changes an instance's labels.
func (p *Provider) SetTags(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, set map[string]string, remove []string) (cloud.Result, error) {
	return p.SetTagsOf(ctx, a, actor, region, cloud.KindInstance, id, set, remove)
}

// SetTagsOf changes the labels of an instance or a disk. Google replaces
// a resource's labels as a whole, guarded by a fingerprint, so the labels
// are read, changed, and written back: a label someone else set between
// the two is not lost, it makes Google refuse, and the change can be
// asked for again.
func (p *Provider) SetTagsOf(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, kind cloud.Kind, id string, set map[string]string, remove []string) (cloud.Result, error) {
	coll := map[cloud.Kind]string{cloud.KindInstance: "instances", cloud.KindVolume: "disks"}[kind]
	if coll == "" {
		return cloud.Result{}, errNotYet("label a " + string(kind))
	}
	for k, v := range set {
		if why := LabelProblem(k, v); why != "" {
			return cloud.Result{}, fmt.Errorf("%s=%s: %s", k, v, why)
		}
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	w, err := p.locate(ctx, t, actor, a.AccountID, region, coll, id)
	if err != nil {
		return cloud.Result{}, err
	}
	base := p.ep.compute + w.path(a.AccountID, coll) + id
	var cur struct {
		Labels           map[string]string `json:"labels"`
		LabelFingerprint string            `json:"labelFingerprint"`
	}
	if err := p.get(ctx, t, actor, base+"?fields=labels,labelFingerprint", &cur); err != nil {
		return cloud.Result{}, err
	}
	labels := map[string]string{}
	maps.Copy(labels, cur.Labels)
	for _, k := range remove {
		delete(labels, k)
	}
	maps.Copy(labels, set)
	if len(labels) > MaxLabels {
		return cloud.Result{}, fmt.Errorf("%s would have %d labels; Google keeps at most %d", w.Name, len(labels), MaxLabels)
	}
	op, err := p.post(ctx, t, actor, base+"/setLabels", map[string]any{"labels": labels, "labelFingerprint": cur.LabelFingerprint})
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: op.Name}, nil
}

// Protect turns an instance's deletion protection on or off.
func (p *Provider) Protect(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, on bool) (cloud.Result, error) {
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
	q := url.Values{"deletionProtection": {fmt.Sprint(on)}}
	op, err := p.post(ctx, t, actor, p.ep.compute+w.path(a.AccountID, "instances")+id+"/setDeletionProtection?"+q.Encode(), nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: id, RequestID: op.Name}, nil
}

// reName is a Compute Engine resource name: what a disk or snapshot
// Argus makes is called.
var reName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidName reports whether n can name a disk or snapshot.
func ValidName(n string) bool { return reName.MatchString(n) }

// DiskTypes are the disk types Argus makes.
var DiskTypes = []string{"pd-balanced", "pd-ssd", "pd-standard"}

// Disk sizes Argus makes, in GB: Google's persistent disk limits.
const (
	MinDiskGB = 10
	MaxDiskGB = 65536
)

// send makes one call that answers with an operation.
func (p *Provider) send(ctx context.Context, t token, actor cloud.Actor, method, u string, body any) (operation, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return operation{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+t.value)
	req.Header.Set("User-Agent", userAgent(actor))
	var op operation
	if err := p.do(req, &op); err != nil {
		return operation{}, err
	}
	return op, op.err()
}

// wait waits for an operation to finish, for a step that needs what it
// made: a disk is attached only once it exists.
func (p *Provider) wait(ctx context.Context, t token, actor cloud.Actor, op operation) error {
	for delay := 500 * time.Millisecond; op.Status != "DONE"; delay = min(delay*2, 5*time.Second) {
		if op.SelfLink == "" {
			return errors.New("Google gave no operation to wait for")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Google had not finished %s: %w", op.Name, ctx.Err())
		case <-time.After(delay):
		}
		var next operation
		if err := p.get(ctx, t, actor, p.ep.compute+trimHost(op.SelfLink), &next); err != nil {
			return err
		}
		op = next
	}
	return op.err()
}

// trimHost is a Compute Engine link's path, so a call goes where the
// provider's endpoint says.
func trimHost(link string) string {
	if i := strings.Index(link, "/compute/v1/"); i >= 0 {
		return link[i:]
	}
	return link
}

// labelsOf are tags as Google labels: the change id Argus marks what it
// makes with, as argus-change, and the rest as they are (planning has
// already checked them).
func labelsOf(tags map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range tags {
		if k == "argus:change" {
			k, v = "argus-change", strings.ToLower(v)
		}
		if LabelProblem(k, v) == "" {
			out[k] = v
		}
	}
	return out
}

// CreateVolume makes a disk in a zone, empty or from a snapshot, and
// attaches it when asked to, once Google has made it.
func (p *Provider) CreateVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region string, s cloud.VolumeSpec) (cloud.Result, error) {
	if !ValidName(s.Name) {
		return cloud.Result{}, fmt.Errorf("%q cannot name a disk: lowercase letters, digits and -, up to 63, starting with a letter", s.Name)
	}
	if !slices.Contains(DiskTypes, s.Type) {
		return cloud.Result{}, errors.New("pick a disk type: " + strings.Join(DiskTypes, ", "))
	}
	if regionOfZone(s.Zone) != region {
		return cloud.Result{}, fmt.Errorf("%s is not a zone of %s", s.Zone, region)
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	body := map[string]any{"name": s.Name, "type": "zones/" + s.Zone + "/diskTypes/" + s.Type, "labels": labelsOf(s.Tags)}
	if s.SizeGB > 0 {
		body["sizeGb"] = fmt.Sprint(s.SizeGB)
	}
	if s.Snapshot != "" {
		if !reID.MatchString(s.Snapshot) {
			return cloud.Result{}, fmt.Errorf("%q is not a Compute Engine id", s.Snapshot)
		}
		var snap struct {
			SelfLink string `json:"selfLink"`
		}
		if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+a.AccountID+"/global/snapshots/"+s.Snapshot+"?fields=selfLink", &snap); err != nil {
			return cloud.Result{}, err
		}
		body["sourceSnapshot"] = trimHost(snap.SelfLink)
	}
	op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+"/compute/v1/projects/"+a.AccountID+"/zones/"+s.Zone+"/disks", body)
	if err != nil {
		return cloud.Result{}, err
	}
	res := cloud.Result{ResourceID: op.TargetID, RequestID: op.Name}
	if s.AttachTo == "" {
		return res, nil
	}
	if err := p.wait(ctx, t, actor, op); err != nil {
		return res, fmt.Errorf("the disk was asked for, but Argus could not attach it: %w", err)
	}
	_, at, err := p.attach(ctx, t, actor, a.AccountID, region, where{Name: s.Name, Zone: s.Zone}, s.AttachTo, s.Device)
	if err != nil {
		return res, fmt.Errorf("%s was made, but not attached: %w", s.Name, err)
	}
	res.Detail = "Made " + s.Name + " and attached it to " + at.Name + " as " + s.Device
	return res, nil
}

// AttachVolume attaches a disk to an instance in its zone, under device:
// the server sees it as /dev/disk/by-id/google-<device>.
func (p *Provider) AttachVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume, instance, device string) (cloud.Result, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locate(ctx, t, actor, a.AccountID, region, "disks", volume)
	if err != nil {
		return cloud.Result{}, err
	}
	op, _, err := p.attach(ctx, t, actor, a.AccountID, region, d, instance, device)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: volume, RequestID: op.Name}, nil
}

// attach attaches the disk at d to the instance with id instance.
func (p *Provider) attach(ctx context.Context, t token, actor cloud.Actor, project, region string, d where, instance, device string) (operation, where, error) {
	if !ValidName(device) {
		return operation{}, where{}, fmt.Errorf("%q cannot be a device name", device)
	}
	in, err := p.locate(ctx, t, actor, project, region, "instances", instance)
	if err != nil {
		return operation{}, where{}, err
	}
	if in.Zone == "" || (d.Zone != "" && d.Zone != in.Zone) {
		return operation{}, where{}, fmt.Errorf("%s is in %s and %s in %s; a disk attaches only in its own zone", d.Name, d.Zone, in.Name, in.Zone)
	}
	source := strings.TrimPrefix(d.path(project, "disks"), "/compute/v1/") + d.Name
	op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+in.path(project, "instances")+instance+"/attachDisk",
		map[string]any{"source": source, "deviceName": device, "mode": "READ_WRITE"})
	return op, in, err
}

// DetachVolume detaches a disk from the instance using it. Google detaches
// at once, used or not, so the console asks for it to be unmounted first.
func (p *Provider) DetachVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume string) (cloud.Result, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locate(ctx, t, actor, a.AccountID, region, "disks", volume)
	if err != nil {
		return cloud.Result{}, err
	}
	var disk struct {
		Users []string `json:"users"`
	}
	if err := p.get(ctx, t, actor, p.ep.compute+d.path(a.AccountID, "disks")+volume+"?fields=users", &disk); err != nil {
		return cloud.Result{}, err
	}
	if len(disk.Users) != 1 {
		return cloud.Result{}, fmt.Errorf("%s is used by %d instances; Argus detaches a disk from one", d.Name, len(disk.Users))
	}
	user := trimHost(disk.Users[0])
	var inst struct {
		Disks []struct {
			Source     string `json:"source"`
			DeviceName string `json:"deviceName"`
		} `json:"disks"`
	}
	if err := p.get(ctx, t, actor, p.ep.compute+user+"?fields=disks(source,deviceName)", &inst); err != nil {
		return cloud.Result{}, err
	}
	device := ""
	for _, x := range inst.Disks {
		if strings.HasSuffix(x.Source, "/disks/"+d.Name) {
			device = x.DeviceName
		}
	}
	if device == "" {
		return cloud.Result{}, fmt.Errorf("%s is not attached to %s any more", d.Name, last(user))
	}
	q := url.Values{"deviceName": {device}}
	op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+user+"/detachDisk?"+q.Encode(), nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: volume, RequestID: op.Name}, nil
}

// DeleteVolume deletes a disk no instance uses.
func (p *Provider) DeleteVolume(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume string, finalSnapshot bool) (cloud.Result, error) {
	if finalSnapshot {
		return cloud.Result{}, errNotYet("take a final snapshot while deleting a disk (take a snapshot first, then delete it)")
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locate(ctx, t, actor, a.AccountID, region, "disks", volume)
	if err != nil {
		return cloud.Result{}, err
	}
	op, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.compute+d.path(a.AccountID, "disks")+volume, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: volume, RequestID: op.Name}, nil
}

// CreateSnapshot snapshots a disk, zonal or regional, under name. Google
// copies it while it stays in use; the snapshot is ready in the background.
func (p *Provider) CreateSnapshot(ctx context.Context, a cloud.Account, actor cloud.Actor, region, volume, name string) (cloud.Result, error) {
	if !ValidName(name) {
		return cloud.Result{}, fmt.Errorf("%q cannot name a snapshot: lowercase letters, digits and -, up to 63, starting with a letter", name)
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	d, err := p.locate(ctx, t, actor, a.AccountID, region, "disks", volume)
	if err != nil {
		return cloud.Result{}, err
	}
	op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+d.path(a.AccountID, "disks")+volume+"/createSnapshot",
		map[string]any{"name": name})
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: name, RequestID: op.Name}, nil
}

// DeleteSnapshot deletes a snapshot. Snapshots are global in Google Cloud.
func (p *Provider) DeleteSnapshot(ctx context.Context, a cloud.Account, actor cloud.Actor, _ string, snapshot string) (cloud.Result, error) {
	if !reID.MatchString(snapshot) {
		return cloud.Result{}, fmt.Errorf("%q is not a Compute Engine id", snapshot)
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	op, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.compute+"/compute/v1/projects/"+a.AccountID+"/global/snapshots/"+snapshot, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: snapshot, RequestID: op.Name}, nil
}

// managedBy names what owns a VM Google made for something else: a managed
// instance group puts a deleted VM back, and GKE breaks without its node.
func managedBy(labels map[string]string, metadata map[string]string) string {
	if c := metadata["created-by"]; strings.Contains(c, "/instanceGroupManagers/") {
		return "managed instance group " + last(c)
	}
	if v := labels["goog-gke-node"]; v != "" || labels["goog-k8s-cluster-name"] != "" {
		if n := labels["goog-k8s-cluster-name"]; n != "" {
			return "GKE cluster " + n
		}
		return "a GKE cluster"
	}
	if v := labels["goog-dataproc-cluster-name"]; v != "" {
		return "Dataproc cluster " + v
	}
	return ""
}

// vmForRemove is what removing a VM reads.
type vmForRemove struct {
	where
	Name               string            `json:"name"`
	Status             string            `json:"status"`
	MachineType        string            `json:"machineType"`
	DeletionProtection bool              `json:"deletionProtection"`
	Labels             map[string]string `json:"labels"`
	Metadata           struct {
		Items []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"items"`
	} `json:"metadata"`
	Disks []struct {
		Source     string `json:"source"`
		DeviceName string `json:"deviceName"`
		Boot       bool   `json:"boot"`
		AutoDelete bool   `json:"autoDelete"`
		DiskSizeGb string `json:"diskSizeGb"`
	} `json:"disks"`
	NetworkInterfaces []struct {
		AccessConfigs []accessConfig `json:"accessConfigs"`
	} `json:"networkInterfaces"`
}

func (p *Provider) planRemove(ctx context.Context, t token, actor cloud.Actor, project, region, id string) (cloud.RemovePlan, vmForRemove, error) {
	w, err := p.locate(ctx, t, actor, project, region, "instances", id)
	if err != nil {
		return cloud.RemovePlan{}, vmForRemove{}, err
	}
	var vm vmForRemove
	if err := p.get(ctx, t, actor, p.ep.compute+w.path(project, "instances")+id, &vm); err != nil {
		return cloud.RemovePlan{}, vmForRemove{}, err
	}
	vm.where = w
	meta := map[string]string{}
	for _, it := range vm.Metadata.Items {
		meta[it.Key] = it.Value
	}
	plan := cloud.RemovePlan{ID: id, Name: vm.Name, State: instanceState(vm.Status), Size: last(vm.MachineType),
		Protected: vm.DeletionProtection, ManagedBy: managedBy(vm.Labels, meta), Volumes: []cloud.Volume{}}
	for _, d := range vm.Disks {
		v := cloud.Volume{ID: last(d.Source), Device: d.DeviceName, Deleted: d.AutoDelete}
		var disk struct {
			ID     string `json:"id"`
			SizeGb string `json:"sizeGb"`
		}
		// The disk's id is what the console knows it by; its name when it cannot be read.
		if p.get(ctx, t, actor, p.ep.compute+trimHost(d.Source)+"?fields=id,sizeGb", &disk) == nil && disk.ID != "" {
			v.ID = disk.ID
			d.DiskSizeGb = disk.SizeGb
		}
		var gb int64
		fmt.Sscan(d.DiskSizeGb, &gb)
		v.SizeGB = int32(gb)
		v.Description = last(d.Source)
		if d.Boot {
			v.Description += " (boot)"
		}
		plan.Volumes = append(plan.Volumes, v)
	}
	for _, nic := range vm.NetworkInterfaces {
		for _, ac := range nic.AccessConfigs {
			if ac.NatIP == "" || plan.PublicIP != "" || plan.ElasticIP != "" {
				continue
			}
			// A static address stays reserved, and billed, after the VM goes.
			var found struct {
				Items []struct {
					Address string `json:"address"`
				} `json:"items"`
			}
			q := url.Values{"filter": {`address = "` + ac.NatIP + `"`}, "fields": {"items(address)"}}
			if p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+project+"/regions/"+region+"/addresses?"+q.Encode(), &found) == nil && len(found.Items) > 0 {
				plan.ElasticIP = ac.NatIP
			} else {
				plan.PublicIP = ac.NatIP
			}
		}
	}
	return plan, vm, nil
}

// PlanRemove reads what deleting a VM takes with it: the disks Google
// deletes with it (auto-delete) and its ephemeral public address.
func (p *Provider) PlanRemove(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string) (cloud.RemovePlan, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.RemovePlan{}, err
	}
	plan, _, err := p.planRemove(ctx, t, actor, a.AccountID, region, id)
	return plan, err
}

// finalName is a final snapshot's name: the disk's, -final and the time,
// in Compute Engine's 63 characters.
func finalName(disk string, now time.Time) string {
	tail := "-final-" + now.UTC().Format("20060102-1504")
	if len(disk) > 63-len(tail) {
		disk = strings.TrimRight(disk[:63-len(tail)], "-")
	}
	return disk + tail
}

// Remove deletes a VM and the disks Google deletes with it. A final
// snapshot of each of those disks is taken first, when asked, and Argus
// waits for each to finish before anything is deleted.
func (p *Provider) Remove(ctx context.Context, a cloud.Account, actor cloud.Actor, region, id string, finalSnapshots bool) (cloud.Result, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	plan, vm, err := p.planRemove(ctx, t, actor, a.AccountID, region, id)
	if err != nil {
		return cloud.Result{}, err
	}
	if plan.Protected {
		return cloud.Result{}, cloud.ErrProtected
	}
	out := cloud.Result{ResourceID: id}
	if finalSnapshots {
		out.Snapshots = map[string]string{}
		for i, d := range vm.Disks {
			if !d.AutoDelete {
				continue
			}
			name := finalName(last(d.Source), time.Now())
			op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+trimHost(d.Source)+"/createSnapshot",
				map[string]any{"name": name, "description": "Final snapshot of " + plan.Name + " (" + id + ") before Argus removed it"})
			if err == nil {
				err = p.wait(ctx, t, actor, op)
			}
			if err != nil {
				return out, fmt.Errorf("nothing was removed: the final snapshot of %s failed: %w", last(d.Source), err)
			}
			out.Snapshots[plan.Volumes[i].ID] = name
		}
	}
	op, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.compute+vm.path(a.AccountID, "instances")+id, nil)
	if err != nil {
		if len(out.Snapshots) > 0 {
			names := make([]string, 0, len(out.Snapshots))
			for _, n := range out.Snapshots {
				names = append(names, n)
			}
			slices.Sort(names)
			return out, fmt.Errorf("the final snapshots were taken (%s), but the VM was not removed: %w", strings.Join(names, ", "), err)
		}
		return out, err
	}
	out.RequestID = op.Name
	return out, nil
}

// imageFamilies are the public images Argus offers to launch from: each
// family's latest image, so a VM starts patched.
var imageFamilies = []struct{ project, family, label string }{
	{"debian-cloud", "debian-12", "Debian 12"},
	{"ubuntu-os-cloud", "ubuntu-2404-lts-amd64", "Ubuntu 24.04 LTS"},
	{"ubuntu-os-cloud", "ubuntu-2204-lts", "Ubuntu 22.04 LTS"},
	{"rocky-linux-cloud", "rocky-linux-9", "Rocky Linux 9"},
	{"windows-cloud", "windows-2022", "Windows Server 2022"},
}

// Images are the image families Argus offers, those Google still has. An
// image's id is its family's path, which Google resolves to the latest
// image when the VM is made.
func (p *Provider) Images(ctx context.Context, a cloud.Account, actor cloud.Actor, _ string) ([]cloud.Image, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return nil, err
	}
	var out []cloud.Image
	var last error
	for _, f := range imageFamilies {
		var img struct {
			Name string `json:"name"`
		}
		path := "projects/" + f.project + "/global/images/family/" + f.family
		if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/"+path+"?fields=name", &img); err != nil {
			last = err
			continue
		}
		out = append(out, cloud.Image{Family: f.family, Label: f.label, ID: path, Name: img.Name, Linux: f.project != "windows-cloud"})
	}
	if len(out) == 0 && last != nil {
		return nil, last
	}
	return out, nil
}

// Zones are the zones of a region that are up, for a launch.
func (p *Provider) Zones(ctx context.Context, a cloud.Account, actor cloud.Actor, region string) ([]string, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return nil, err
	}
	var r struct {
		Zones []string `json:"zones"`
	}
	if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+a.AccountID+"/regions/"+region+"?fields=zones", &r); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.Zones))
	for _, z := range r.Zones {
		out = append(out, last(z))
	}
	slices.Sort(out)
	return out, nil
}

// LaunchSizes are the machine types the launch form offers in Google Cloud.
var LaunchSizes = []string{"e2-micro", "e2-small", "e2-medium", "e2-standard-2", "e2-standard-4", "e2-standard-8",
	"e2-highmem-2", "e2-highmem-4", "n2-standard-2", "n2-standard-4", "n2-standard-8", "n2-highmem-4",
	"n2d-standard-2", "n2d-standard-4", "c3-standard-4", "t2d-standard-2"}

// firstBoot makes a startup script run once: Google runs it at every boot,
// AWS user data only at the first, and an enrollment token is single use.
func firstBoot(script string) string {
	body := strings.TrimPrefix(script, "#!/bin/sh\n")
	return "#!/bin/sh\n" +
		"# Argus: run once, at the first boot.\n" +
		"[ -e /var/lib/argus-first-boot ] && exit 0\n" +
		"mkdir -p /var/lib && touch /var/lib/argus-first-boot\n" + body
}

var reNotLabel = regexp.MustCompile(`[^a-z0-9_-]+`)

// launchLabels are a launch's tags as labels: who launched it, as a label
// value can hold an email, and the change.
func launchLabels(tags map[string]string) map[string]string {
	in := map[string]string{}
	for k, v := range tags {
		if k == "argus:launched-by" {
			k, v = "argus-launched-by", strings.Trim(reNotLabel.ReplaceAllString(strings.ToLower(v), "_"), "_")
			if len(v) > 63 {
				v = v[:63]
			}
		}
		in[k] = v
	}
	return labelsOf(in)
}

// Launch makes one VM in a zone: from an image family, with a boot disk and
// any extra disks Google deletes with it, on a subnet, with the network
// tags of the firewall rules picked, and an ephemeral public address when
// asked. It runs with no service account, so it holds no Google
// credentials and Argus needs no right to hand one out.
func (p *Provider) Launch(ctx context.Context, a cloud.Account, actor cloud.Actor, s cloud.LaunchSpec) (cloud.Result, error) {
	if !ValidName(s.Name) {
		return cloud.Result{}, fmt.Errorf("%q cannot name a VM: lowercase letters, digits and -, up to 63, starting with a letter", s.Name)
	}
	if regionOfZone(s.Zone) != s.Region {
		return cloud.Result{}, fmt.Errorf("pick a zone of %s", s.Region)
	}
	if !ValidMachineType(s.Size) {
		return cloud.Result{}, fmt.Errorf("%q is not a machine type", s.Size)
	}
	known := false
	for _, f := range imageFamilies {
		known = known || s.Image == "projects/"+f.project+"/global/images/family/"+f.family
	}
	if !known {
		return cloud.Result{}, errors.New("pick one of the images offered")
	}
	if s.CapacityReservation != "" {
		return cloud.Result{}, errNotYet("launch into a reservation")
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	project := a.AccountID
	var subnet struct {
		SelfLink string `json:"selfLink"`
		Network  string `json:"network"`
	}
	if !reID.MatchString(s.Subnet) {
		return cloud.Result{}, fmt.Errorf("%q is not a Compute Engine id", s.Subnet)
	}
	if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+project+"/regions/"+s.Region+"/subnetworks/"+s.Subnet+"?fields=selfLink,network", &subnet); err != nil {
		return cloud.Result{}, err
	}
	// A firewall rule reaches a VM by its network tags.
	var tags []string
	for _, f := range s.Firewalls {
		if !reID.MatchString(f) {
			return cloud.Result{}, fmt.Errorf("%q is not a Compute Engine id", f)
		}
		var fw struct {
			Name       string   `json:"name"`
			Network    string   `json:"network"`
			TargetTags []string `json:"targetTags"`
		}
		if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+project+"/global/firewalls/"+f+"?fields=name,network,targetTags", &fw); err != nil {
			return cloud.Result{}, err
		}
		if last(fw.Network) != last(subnet.Network) {
			return cloud.Result{}, fmt.Errorf("%s is in another network than the subnet", fw.Name)
		}
		for _, tg := range fw.TargetTags {
			if !slices.Contains(tags, tg) {
				tags = append(tags, tg)
			}
		}
	}
	disks := []map[string]any{{"boot": true, "autoDelete": true, "deviceName": s.Name,
		"initializeParams": map[string]any{"sourceImage": s.Image, "diskSizeGb": fmt.Sprint(s.DiskGB),
			"diskType": "zones/" + s.Zone + "/diskTypes/pd-balanced"}}}
	for i, gb := range s.ExtraDisks {
		dev := fmt.Sprintf("%s-data-%d", s.Name, i+1)
		if len(dev) > 63 {
			dev = fmt.Sprintf("data-%d", i+1)
		}
		disks = append(disks, map[string]any{"autoDelete": true, "deviceName": dev,
			"initializeParams": map[string]any{"diskSizeGb": fmt.Sprint(gb), "diskType": "zones/" + s.Zone + "/diskTypes/pd-balanced"}})
	}
	nic := map[string]any{"subnetwork": trimHost(subnet.SelfLink)}
	if s.PublicIP {
		nic["accessConfigs"] = []map[string]any{{"name": "external-nat", "type": "ONE_TO_ONE_NAT"}}
	}
	body := map[string]any{
		"name":               s.Name,
		"machineType":        "zones/" + s.Zone + "/machineTypes/" + s.Size,
		"disks":              disks,
		"networkInterfaces":  []map[string]any{nic},
		"labels":             launchLabels(s.Tags),
		"deletionProtection": s.Protect,
		"serviceAccounts":    []any{},
	}
	if len(tags) > 0 {
		body["tags"] = map[string]any{"items": tags}
	}
	if s.UserData != "" {
		body["metadata"] = map[string]any{"items": []map[string]string{{"key": "startup-script", "value": firstBoot(s.UserData)}}}
	}
	if s.Spot {
		body["scheduling"] = map[string]any{"provisioningModel": "SPOT", "instanceTerminationAction": "DELETE", "automaticRestart": false, "onHostMaintenance": "TERMINATE"}
	}
	op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+"/compute/v1/projects/"+project+"/zones/"+s.Zone+"/instances", body)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: op.TargetID, RequestID: op.Name}, nil
}

// The rest of cloud.Manager and cloud.Storage: refused until each is built.

func (p *Provider) CreateImage(context.Context, cloud.Account, cloud.Actor, string, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("make an image")
}
func (p *Provider) DeleteImage(context.Context, cloud.Account, cloud.Actor, string, string, bool) (cloud.Result, error) {
	return cloud.Result{}, errNotYet("delete an image")
}

// AllocateAddress reserves a static external address in region under
// name. Google bills it while nothing uses it, as AWS does.
func (p *Provider) AllocateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, name string) (cloud.Result, error) {
	if !ValidName(name) {
		return cloud.Result{}, fmt.Errorf("%q cannot name an address: lowercase letters, digits and -, up to 63, starting with a letter", name)
	}
	if region == "" || region == GlobalRegion {
		return cloud.Result{}, errors.New("an address for a VM is reserved in its region")
	}
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	base := p.ep.compute + "/compute/v1/projects/" + a.AccountID + "/regions/" + region + "/addresses"
	op, err := p.send(ctx, t, actor, http.MethodPost, base, map[string]any{"name": name, "addressType": "EXTERNAL"})
	if err != nil {
		return cloud.Result{}, err
	}
	res := cloud.Result{ResourceID: op.TargetID, RequestID: op.Name}
	// The address itself is known once Google has reserved it.
	if p.wait(ctx, t, actor, op) == nil {
		var got struct {
			Address string `json:"address"`
		}
		if p.get(ctx, t, actor, base+"/"+name+"?fields=address", &got) == nil && got.Address != "" {
			res.Detail = "Address " + got.Address
		}
	}
	return res, nil
}

// addressAt is a regional address and who uses it.
type addressAt struct {
	where
	IP    string
	Users []string
}

func (p *Provider) addressOf(ctx context.Context, t token, actor cloud.Actor, project, region, id string) (addressAt, error) {
	w, err := p.locate(ctx, t, actor, project, region, "addresses", id)
	if err != nil {
		return addressAt{}, err
	}
	if w.Region == "" {
		return addressAt{}, fmt.Errorf("%s is a global address, for load balancers; Argus assigns regional ones to VMs", w.Name)
	}
	var got struct {
		Address string   `json:"address"`
		Users   []string `json:"users"`
	}
	if err := p.get(ctx, t, actor, p.ep.compute+w.path(project, "addresses")+id+"?fields=address,users", &got); err != nil {
		return addressAt{}, err
	}
	return addressAt{where: w, IP: got.Address, Users: got.Users}, nil
}

// accessConfig is the external address a VM's network interface has.
type accessConfig struct {
	Name  string `json:"name"`
	NatIP string `json:"natIP"`
}

// nicsOf are a VM's network interfaces with their external addresses.
func (p *Provider) nicsOf(ctx context.Context, t token, actor cloud.Actor, instance string) ([]struct {
	Name          string         `json:"name"`
	AccessConfigs []accessConfig `json:"accessConfigs"`
}, error) {
	var got struct {
		NetworkInterfaces []struct {
			Name          string         `json:"name"`
			AccessConfigs []accessConfig `json:"accessConfigs"`
		} `json:"networkInterfaces"`
	}
	err := p.get(ctx, t, actor, p.ep.compute+instance+"?fields=networkInterfaces(name,accessConfigs(name,natIP))", &got)
	return got.NetworkInterfaces, err
}

// AssociateAddress gives a VM the static address as its external one, on
// its first network interface. An ephemeral address it had is let go
// first: Google allows one external address on an interface.
func (p *Provider) AssociateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address, instance string) (cloud.Result, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	ad, err := p.addressOf(ctx, t, actor, a.AccountID, region, address)
	if err != nil {
		return cloud.Result{}, err
	}
	if len(ad.Users) > 0 {
		return cloud.Result{}, fmt.Errorf("%s is in use by %s; unassign it first", ad.IP, last(ad.Users[0]))
	}
	in, err := p.locate(ctx, t, actor, a.AccountID, region, "instances", instance)
	if err != nil {
		return cloud.Result{}, err
	}
	vm := in.path(a.AccountID, "instances") + instance
	nics, err := p.nicsOf(ctx, t, actor, vm)
	if err != nil {
		return cloud.Result{}, err
	}
	if len(nics) == 0 {
		return cloud.Result{}, fmt.Errorf("%s has no network interface", in.Name)
	}
	nic := nics[0]
	res := cloud.Result{ResourceID: address}
	for _, ac := range nic.AccessConfigs {
		q := url.Values{"accessConfig": {ac.Name}, "networkInterface": {nic.Name}}
		op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+vm+"/deleteAccessConfig?"+q.Encode(), nil)
		if err == nil {
			err = p.wait(ctx, t, actor, op)
		}
		if err != nil {
			return res, fmt.Errorf("%s keeps its address %s: %w", in.Name, ac.NatIP, err)
		}
		if ac.NatIP != "" {
			res.Detail = "Replaced " + ac.NatIP
		}
	}
	q := url.Values{"networkInterface": {nic.Name}}
	op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+vm+"/addAccessConfig?"+q.Encode(),
		map[string]any{"name": "external-nat", "type": "ONE_TO_ONE_NAT", "natIP": ad.IP})
	if err != nil {
		if res.Detail != "" {
			return res, fmt.Errorf("%s lost its old address but did not get %s: %w", in.Name, ad.IP, err)
		}
		return res, err
	}
	res.RequestID = op.Name
	return res, nil
}

// DisassociateAddress takes the static address off the VM using it. The
// VM has no external address afterwards until it is given one.
func (p *Provider) DisassociateAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address string) (cloud.Result, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	ad, err := p.addressOf(ctx, t, actor, a.AccountID, region, address)
	if err != nil {
		return cloud.Result{}, err
	}
	var vm string
	for _, u := range ad.Users {
		if strings.Contains(u, "/instances/") {
			vm = trimHost(u)
		}
	}
	if vm == "" {
		return cloud.Result{}, fmt.Errorf("%s is not assigned to a VM", ad.IP)
	}
	nics, err := p.nicsOf(ctx, t, actor, vm)
	if err != nil {
		return cloud.Result{}, err
	}
	for _, nic := range nics {
		for _, ac := range nic.AccessConfigs {
			if ac.NatIP != ad.IP {
				continue
			}
			q := url.Values{"accessConfig": {ac.Name}, "networkInterface": {nic.Name}}
			op, err := p.send(ctx, t, actor, http.MethodPost, p.ep.compute+vm+"/deleteAccessConfig?"+q.Encode(), nil)
			if err != nil {
				return cloud.Result{}, err
			}
			return cloud.Result{ResourceID: address, RequestID: op.Name}, nil
		}
	}
	return cloud.Result{}, fmt.Errorf("%s does not have %s any more", last(vm), ad.IP)
}

// ReleaseAddress gives a static address nothing uses back to Google.
func (p *Provider) ReleaseAddress(ctx context.Context, a cloud.Account, actor cloud.Actor, region, address string) (cloud.Result, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	ad, err := p.addressOf(ctx, t, actor, a.AccountID, region, address)
	if err != nil {
		return cloud.Result{}, err
	}
	if len(ad.Users) > 0 {
		return cloud.Result{}, fmt.Errorf("%s is in use by %s; unassign it first", ad.IP, last(ad.Users[0]))
	}
	op, err := p.send(ctx, t, actor, http.MethodDelete, p.ep.compute+ad.path(a.AccountID, "addresses")+address, nil)
	if err != nil {
		return cloud.Result{}, err
	}
	return cloud.Result{ResourceID: address, RequestID: op.Name}, nil
}

var (
	_ cloud.Manager    = (*Provider)(nil)
	_ cloud.Storage    = (*Provider)(nil)
	_ cloud.Supporter  = (*Provider)(nil)
	_ cloud.KindTagger = (*Provider)(nil)
	_ cloud.SizeLister = (*Provider)(nil)
)

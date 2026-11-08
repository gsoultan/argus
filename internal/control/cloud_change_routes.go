package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 2: launch, change and remove instances.

Who: an administrator or owner asks for a change. Whether it needs a second
person depends on the instance's environment (its Environment tag, else the
account's): on Production, launching, resizing and termination protection
(change) and removing (remove) need approval; on Staging, removing does;
starting, stopping, rebooting and tags (operate) never do. An approver,
administrator or owner who did not ask decides. A change runs as the person
who asked, so CloudTrail names them, and runs only the plan that was
approved: a remove whose plan no longer matches goes back for a decision.

Every change is two audit rows at least: cloud.change.requested, then
.applied or .failed (with .approved, .declined or .cancelled between when a
decision was needed).
*/

// CloudSizes are the instance types the launch form offers. Any valid type
// is accepted; these are the ones people pick most.
var CloudSizes = []string{"t3.micro", "t3.small", "t3.medium", "t3.large", "t3.xlarge",
	"m7i.large", "m7i.xlarge", "m7i.2xlarge", "c7i.large", "c7i.xlarge", "r7i.large", "r7i.xlarge"}

// cloudApplyTimeout bounds one change's calls to the provider.
const cloudApplyTimeout = 90 * time.Second

func (a *API) registerCloudChangeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/cloud/launch-options", a.getCloudLaunchOptions)
	mux.HandleFunc("GET /api/v1/cloud/remove-plan", a.getCloudRemovePlan)
	mux.HandleFunc("GET /api/v1/cloud/changes", a.getCloudChanges)
	mux.HandleFunc("POST /api/v1/cloud/changes", a.postCloudChange)
	mux.HandleFunc("GET /api/v1/cloud/changes/{id}", a.getCloudChange)
	mux.HandleFunc("POST /api/v1/cloud/changes/{id}/approve", a.postCloudChangeDecision(true))
	mux.HandleFunc("POST /api/v1/cloud/changes/{id}/decline", a.postCloudChangeDecision(false))
	mux.HandleFunc("POST /api/v1/cloud/changes/{id}/cancel", a.postCloudChangeCancel)
}

// cloudEnvOf is the environment a resource is in: its Environment tag when
// that names one, else its account's.
func cloudEnvOf(tags map[string]string, account string) string {
	for k, v := range tags {
		if !strings.EqualFold(k, "environment") && !strings.EqualFold(k, "env") {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "production", "prod", "prd", "live":
			return "production"
		case "staging", "stage", "stg", "uat", "preprod":
			return "staging"
		case "development", "dev", "test", "testing", "sandbox":
			return "development"
		}
	}
	return account
}

// cloudNeedsApproval is the rule by environment.
func cloudNeedsApproval(env string, level cloud.Level) bool {
	switch env {
	case "production":
		return level == cloud.LevelChange || level == cloud.LevelRemove
	case "staging":
		return level == cloud.LevelRemove
	}
	return false
}

// cloudChanger authenticates someone who may ask for changes.
func (a *API) cloudChanger(w http.ResponseWriter, r *http.Request) (string, bool) {
	return a.cloudAdmin(w, r, "changing cloud resources")
}

// cloudManaged is an account Argus may change, with its provider.
type cloudManaged struct {
	acct    CloudAccount
	manager cloud.Manager
	target  cloud.Account
}

func (a *API) cloudManagedAccount(ctx context.Context, id string) (cloudManaged, int, error) {
	c, err := a.store.CloudAccount(ctx, id)
	if errors.Is(err, ErrCloudAccountNotFound) {
		return cloudManaged{}, http.StatusNotFound, err
	}
	if err != nil {
		return cloudManaged{}, http.StatusInternalServerError, err
	}
	if c.ManageRoleARN == "" {
		return cloudManaged{}, http.StatusConflict, errors.New(c.Name + " is connected read only; allow changes on its row under Cloud first")
	}
	p, ok := a.cloudProvider(c.Provider)
	if !ok {
		return cloudManaged{}, http.StatusNotImplemented, errors.New("this control plane was started without " + c.Provider + " support")
	}
	m, ok := p.(cloud.Manager)
	if !ok {
		return cloudManaged{}, http.StatusNotImplemented, errors.New(c.Provider + " cannot make changes yet")
	}
	ext, err := a.cloudExternalID(ctx, c.Provider, c.AccountID)
	if err != nil {
		return cloudManaged{}, http.StatusInternalServerError, err
	}
	return cloudManaged{acct: c, manager: m,
		target: cloud.Account{Provider: c.Provider, AccountID: c.AccountID, RoleARN: c.ManageRoleARN, ExternalID: ext}}, http.StatusOK, nil
}

func hasRegion(c CloudAccount, region string) bool {
	for _, r := range c.Regions {
		if r == region {
			return true
		}
	}
	return false
}

// CloudLaunchOptions is what the launch form offers in one region.
type CloudLaunchOptions struct {
	Images        []cloud.Image `json:"images"`
	Sizes         []string      `json:"sizes"`
	Subnets       []CloudItem   `json:"subnets"`
	Firewalls     []CloudItem   `json:"firewalls"`
	Environment   string        `json:"environment"`
	NeedsApproval bool          `json:"needsApproval"`
	// AgentProblem says why the agent cannot enroll by itself, when it
	// cannot.
	AgentProblem string `json:"agentProblem,omitempty"`
}

func (a *API) getCloudLaunchOptions(w http.ResponseWriter, r *http.Request) {
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
	region := q.Get("region")
	if !hasRegion(m.acct, region) {
		writeErr(w, http.StatusBadRequest, "pick one of the account's regions")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	images, err := m.manager.Images(ctx, m.target, cloud.Actor(actor), region)
	cancel()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Argus could not list images: "+err.Error())
		return
	}
	out := CloudLaunchOptions{Images: images, Sizes: CloudSizes, Subnets: []CloudItem{}, Firewalls: []CloudItem{},
		Environment: m.acct.Environment, NeedsApproval: cloudNeedsApproval(m.acct.Environment, cloud.LevelChange)}
	for _, kind := range []cloud.Kind{cloud.KindSubnet, cloud.KindFirewall} {
		items, err := a.store.CloudResources(r.Context(), kind)
		if err != nil {
			a.fail(w, "launch options", err)
			return
		}
		for _, it := range items {
			if it.AccountID != m.acct.ID || it.Resource.Region != region {
				continue
			}
			if kind == cloud.KindSubnet {
				out.Subnets = append(out.Subnets, it)
			} else {
				out.Firewalls = append(out.Firewalls, it)
			}
		}
	}
	if _, problem := a.agentUserData(r, "check", "x"); problem != "" {
		out.AgentProblem = problem
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// agentUserData is the first-boot script that names the server, installs
// the agent and enrolls it, the same one-line install the Add-a-server page
// shows. It returns why it cannot, when the control plane's address for
// servers is unknown.
func (a *API) agentUserData(r *http.Request, hostname, token string) (string, string) {
	base := strings.TrimRight(a.enrolAddress(r), "/")
	u, err := CheckURL(base, "the address servers use", true)
	if err != nil || base == "" {
		return "", "Argus does not know the address new servers reach it at. Set it under Settings, then the agent can enroll by itself."
	}
	base = strings.TrimRight(u.String(), "/")
	if !reShellSafe.MatchString(base) {
		return "", "the address servers use has characters a shell would read"
	}
	pin, _, problem := a.enrolPin(r)
	if problem != "" {
		return "", problem
	}
	q := url.Values{"token": {token}, "version": {"latest"}, "url": {base}}
	curl := "curl -fsSL "
	if cp := curlPin(pin); pin != "" && cp != "" {
		curl += "-k --pinnedpubkey '" + cp + "' "
	} else {
		q.Set("pin", "off")
	}
	return "#!/bin/sh\n" +
		"# Written by Argus: name this server, then install and enroll its agent.\n" +
		"hostnamectl set-hostname '" + hostname + "' 2>/dev/null || hostname '" + hostname + "'\n" +
		curl + "'" + base + "/api/v1/install/agent.sh?" + q.Encode() + "' | sh\n", ""
}

// cloudChangeIn is a request for a change.
type cloudChangeIn struct {
	AccountID  string          `json:"accountId"`
	Region     string          `json:"region"`
	Action     cloud.Action    `json:"action"`
	ResourceID string          `json:"resourceId"`
	Spec       json.RawMessage `json:"spec"`
	Reason     string          `json:"reason"`
	// Confirm is the instance's name, typed, for a remove.
	Confirm string `json:"confirm"`
}

// cloudLaunchSpec is what a launch asks for, as stored.
type cloudLaunchSpec struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	ImageLabel string            `json:"imageLabel"`
	Size       string            `json:"size"`
	DiskGB     int32             `json:"diskGb"`
	Subnet     string            `json:"subnet"`
	Firewalls  []string          `json:"firewalls"`
	Protect    bool              `json:"protect"`
	PublicIP   bool              `json:"publicIp"`
	Agent      bool              `json:"agent"`
	Tags       map[string]string `json:"tags,omitempty"`
}

type cloudResizeSpec struct {
	Size string `json:"size"`
	From string `json:"from"`
}

type cloudTagsSpec struct {
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
}

type cloudRemoveSpec struct {
	FinalSnapshots bool `json:"finalSnapshots"`
}

// cloudRemovePlanView is what the remove dialog shows.
type cloudRemovePlanView struct {
	Plan          cloud.RemovePlan `json:"plan"`
	Hash          string           `json:"hash"`
	Environment   string           `json:"environment"`
	NeedsApproval bool             `json:"needsApproval"`
	// Server is the Argus server retired with it, when Argus launched it.
	Server  string `json:"server,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

func (a *API) removePlan(ctx context.Context, m cloudManaged, actor, region string, inst cloud.Resource) (cloudRemovePlanView, error) {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	plan, err := m.manager.PlanRemove(pctx, m.target, cloud.Actor(actor), region, inst.ID)
	cancel()
	if err != nil {
		return cloudRemovePlanView{}, err
	}
	env := cloudEnvOf(inst.Tags, m.acct.Environment)
	v := cloudRemovePlanView{Plan: plan, Hash: plan.Hash(), Environment: env,
		NeedsApproval: cloudNeedsApproval(env, cloud.LevelRemove)}
	if name, ok := a.store.CloudLaunchedServer(ctx, m.acct.ID, inst.ID); ok {
		v.Server = name
	}
	switch {
	case plan.Protected:
		v.Refusal = cloud.ErrProtected.Error()
	case plan.ManagedBy != "":
		v.Refusal = "it belongs to " + plan.ManagedBy + "; remove it there, or the stack will put it back or break"
	case plan.State == "shutting-down" || plan.State == "terminated":
		v.Refusal = "it is already being removed"
	}
	return v, nil
}

func (a *API) getCloudRemovePlan(w http.ResponseWriter, r *http.Request) {
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
	inst, err := a.store.CloudInstance(r.Context(), m.acct.ID, q.Get("region"), q.Get("resourceId"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	v, err := a.removePlan(r.Context(), m, actor, inst.Region, inst)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Argus could not read what removing it would take: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, v)
}

func (a *API) getCloudChanges(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) && sess.Role != "approver" {
		writeErr(w, http.StatusForbidden, "cloud changes are for administrators, owners, auditors and approvers")
		return
	}
	q := r.URL.Query()
	list, err := a.store.CloudChanges(r.Context(), q.Get("pending") == "1", q.Get("accountId"), q.Get("resourceId"), 200)
	if err != nil {
		a.fail(w, "cloud changes", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"changes": list})
}

func (a *API) getCloudChange(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) && sess.Role != "approver" {
		writeErr(w, http.StatusForbidden, "cloud changes are for administrators, owners, auditors and approvers")
		return
	}
	c, err := a.store.CloudChange(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrCloudChangeNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud change", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// planChange checks a request and works out what it will do, without
// doing it.
func (a *API) planChange(r *http.Request, m cloudManaged, actor string, in *cloudChangeIn) (CloudChange, int, error) {
	ch := CloudChange{AccountID: m.acct.ID, Region: in.Region, Action: in.Action, Level: cloud.LevelOf(in.Action),
		Reason: strings.TrimSpace(in.Reason), RequestedBy: actor}
	if ch.Level == "" {
		return ch, http.StatusBadRequest, fmt.Errorf("%q is not a change Argus makes", in.Action)
	}
	if len(ch.Reason) > 500 {
		return ch, http.StatusBadRequest, errors.New("keep the reason under 500 characters")
	}
	if !hasRegion(m.acct, in.Region) {
		return ch, http.StatusBadRequest, errors.New("pick one of the account's regions")
	}
	if in.Action == cloud.ActionLaunch {
		return a.planLaunch(r, m, actor, in, ch)
	}
	inst, err := a.store.CloudInstance(r.Context(), m.acct.ID, in.Region, in.ResourceID)
	if err != nil {
		return ch, http.StatusNotFound, err
	}
	ch.ResourceID, ch.ResourceName = inst.ID, inst.Name
	ch.Environment = cloudEnvOf(inst.Tags, m.acct.Environment)
	switch in.Action {
	case cloud.ActionStart:
		ch.Summary = "Start " + inst.Name
	case cloud.ActionStop:
		ch.Summary = "Stop " + inst.Name
	case cloud.ActionReboot:
		ch.Summary = "Reboot " + inst.Name
	case cloud.ActionProtect:
		ch.Summary = "Turn on termination protection for " + inst.Name
	case cloud.ActionUnprotect:
		ch.Summary = "Turn off termination protection for " + inst.Name
	case cloud.ActionResize:
		var s cloudResizeSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil || !cloud.ValidSize(s.Size) {
			return ch, http.StatusBadRequest, errors.New("pick an instance type, like t3.medium")
		}
		if s.Size == inst.Size {
			return ch, http.StatusBadRequest, errors.New(inst.Name + " is already " + s.Size)
		}
		if inst.State != "stopped" {
			return ch, http.StatusConflict, errors.New("stop " + inst.Name + " first; AWS changes the type of a stopped instance only")
		}
		s.From = inst.Size
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Change " + inst.Name + " from " + inst.Size + " to " + s.Size
	case cloud.ActionTags:
		var s cloudTagsSpec
		if err := json.Unmarshal(in.Spec, &s); err != nil {
			return ch, http.StatusBadRequest, errors.New("tags are {set: {key: value}, remove: [key]}")
		}
		if len(s.Set) == 0 && len(s.Remove) == 0 {
			return ch, http.StatusBadRequest, errors.New("nothing to change")
		}
		if len(s.Set)+len(s.Remove) > 50 {
			return ch, http.StatusBadRequest, errors.New("change at most 50 tags at once")
		}
		for k, v := range s.Set {
			if !cloud.ValidTagKey(k) || len(v) > 256 {
				return ch, http.StatusBadRequest, fmt.Errorf("%q cannot be a tag here: keys are up to 128 characters and cannot start with aws:, values up to 256", k)
			}
		}
		for _, k := range s.Remove {
			if !cloud.ValidTagKey(k) {
				return ch, http.StatusBadRequest, fmt.Errorf("%q cannot be removed from here", k)
			}
		}
		ch.Spec, _ = json.Marshal(s)
		ch.Summary = "Change tags on " + inst.Name + ": " + tagWords(s)
		// Moving an instance out of Production by retagging it is a change
		// to the rule that guards it, so it needs what Production needs.
		if _, ok := s.Set["Environment"]; ok || slices.Contains(s.Remove, "Environment") {
			ch.Level = cloud.LevelChange
		}
	case cloud.ActionRemove:
		var s cloudRemoveSpec
		if len(in.Spec) > 0 {
			if err := json.Unmarshal(in.Spec, &s); err != nil {
				return ch, http.StatusBadRequest, errors.New("remove takes {finalSnapshots: true|false}")
			}
		}
		if strings.TrimSpace(in.Confirm) != inst.Name {
			return ch, http.StatusBadRequest, errors.New("type the instance's name, " + inst.Name + ", to remove it")
		}
		v, err := a.removePlan(r.Context(), m, actor, in.Region, inst)
		if err != nil {
			return ch, http.StatusBadGateway, errors.New("Argus could not read what removing it would take: " + err.Error())
		}
		if v.Refusal != "" {
			return ch, http.StatusConflict, errors.New("Argus will not remove " + inst.Name + ": " + v.Refusal)
		}
		ch.Spec, _ = json.Marshal(s)
		ch.Plan, _ = json.Marshal(v)
		ch.PlanHash = v.Hash
		ch.Summary = "Remove " + inst.Name + removeWords(v, s.FinalSnapshots)
	}
	return ch, http.StatusOK, nil
}

func (a *API) planLaunch(r *http.Request, m cloudManaged, actor string, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	var s cloudLaunchSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a launch needs a name, image, size, disk, subnet and firewalls")
	}
	s.Name = strings.ToLower(strings.TrimSpace(s.Name))
	if !cloud.ValidHostname(s.Name) {
		return ch, http.StatusBadRequest, errors.New("the name becomes the server's hostname: lowercase letters, digits and dashes, up to 63")
	}
	if !cloud.ValidSize(s.Size) {
		return ch, http.StatusBadRequest, errors.New("pick an instance type, like t3.medium")
	}
	if s.DiskGB == 0 {
		s.DiskGB = 20
	}
	if s.DiskGB < 8 || s.DiskGB > 2048 {
		return ch, http.StatusBadRequest, errors.New("the disk is 8 to 2048 GB")
	}
	if len(s.Firewalls) == 0 || len(s.Firewalls) > 5 {
		return ch, http.StatusBadRequest, errors.New("pick one to five security groups")
	}
	for k, v := range s.Tags {
		if !cloud.ValidTagKey(k) || len(v) > 256 || k == "Name" {
			return ch, http.StatusBadRequest, fmt.Errorf("%q cannot be a tag here", k)
		}
	}
	if len(s.Tags) > 40 {
		return ch, http.StatusBadRequest, errors.New("at most 40 tags")
	}
	// The subnet and security groups must be ones Argus has read in this
	// account and region, and in one network.
	subnet, err := a.cloudResource(r.Context(), m.acct.ID, in.Region, cloud.KindSubnet, s.Subnet)
	if err != nil {
		return ch, http.StatusBadRequest, errors.New("pick a subnet in " + in.Region)
	}
	for _, f := range s.Firewalls {
		fw, err := a.cloudResource(r.Context(), m.acct.ID, in.Region, cloud.KindFirewall, f)
		if err != nil {
			return ch, http.StatusBadRequest, errors.New("pick security groups in " + in.Region)
		}
		if fw.Network != subnet.Network {
			return ch, http.StatusBadRequest, errors.New(fw.Name + " is in another network than the subnet")
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	images, err := m.manager.Images(ctx, m.target, cloud.Actor(actor), in.Region)
	cancel()
	if err != nil {
		return ch, http.StatusBadGateway, errors.New("Argus could not list images: " + err.Error())
	}
	var img *cloud.Image
	for i := range images {
		if images[i].ID == s.Image {
			img = &images[i]
		}
	}
	if img == nil {
		return ch, http.StatusBadRequest, errors.New("pick one of the images offered")
	}
	s.ImageLabel = img.Label
	if s.Agent && !img.Linux {
		return ch, http.StatusBadRequest, errors.New("the agent enrolls by itself on Linux images; add " + img.Label + " servers from Servers › Add a server")
	}
	if s.Agent {
		if _, problem := a.agentUserData(r, s.Name, "check"); problem != "" {
			return ch, http.StatusConflict, errors.New(problem)
		}
	}
	env := m.acct.Environment
	if e := cloudEnvOf(s.Tags, ""); e != "" {
		env = e
	}
	ch.Environment = env
	ch.ResourceName = s.Name
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = fmt.Sprintf("Launch %s: %s, %s, %d GB in %s", s.Name, img.Label, s.Size, s.DiskGB, subnet.Zone)
	if s.Agent {
		ch.Summary += ", with the Argus agent"
	}
	return ch, http.StatusOK, nil
}

func (a *API) cloudResource(ctx context.Context, accountID, region string, kind cloud.Kind, id string) (cloud.Resource, error) {
	items, err := a.store.CloudResources(ctx, kind)
	if err != nil {
		return cloud.Resource{}, err
	}
	for _, it := range items {
		if it.AccountID == accountID && it.Resource.Region == region && it.Resource.ID == id {
			return it.Resource, nil
		}
	}
	return cloud.Resource{}, errors.New("not found")
}

func tagWords(s cloudTagsSpec) string {
	var parts []string
	keys := make([]string, 0, len(s.Set))
	for k := range s.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+s.Set[k])
	}
	for _, k := range s.Remove {
		parts = append(parts, "remove "+k)
	}
	return strings.Join(parts, ", ")
}

func removeWords(v cloudRemovePlanView, snaps bool) string {
	var gb int32
	n := 0
	for _, d := range v.Plan.Volumes {
		if d.Deleted {
			gb += d.SizeGB
			n++
		}
	}
	out := fmt.Sprintf(" (%s) and %d disk%s, %d GB", v.Plan.ID, n, plural(n), gb)
	if snaps && n > 0 {
		out += ", after a final snapshot of each"
	}
	if v.Server != "" {
		out += "; retire the Argus server " + v.Server
	}
	return out
}

func (a *API) postCloudChange(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudChanger(w, r)
	if !ok {
		return
	}
	var in cloudChangeIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	m, code, err := a.cloudManagedAccount(r.Context(), in.AccountID)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	ch, code, err := a.planChange(r, m, actor, &in)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	waits := cloudNeedsApproval(ch.Environment, ch.Level)
	ch.State = "applying"
	if waits {
		ch.State = "awaiting_approval"
	}
	ch, err = a.store.CreateCloudChange(r.Context(), ch)
	if errors.Is(err, ErrCloudChangeBusy) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud change", err)
		return
	}
	detail := ch.Summary + "."
	if waits {
		detail += " Waits for approval (" + ch.Environment + ")."
	}
	if ch.Reason != "" {
		detail += " Reason: " + ch.Reason
	}
	a.auditCloudChange(r, ch, "requested", actor, detail)
	if waits {
		writeJSON(w, http.StatusAccepted, ch)
		return
	}
	ch = a.applyCloudChange(r, m, ch)
	writeJSON(w, http.StatusOK, ch)
}

func (a *API) auditCloudChange(r *http.Request, ch CloudChange, what, actor, detail string) {
	sev := "notice"
	if what == "failed" || ch.Action == cloud.ActionRemove {
		sev = "warn"
	}
	target := m2target(ch)
	a.auditAsset(r, AuditEvent{Action: "cloud.change." + what, Severity: sev, ActorEmail: actor, Target: target,
		Detail: detail + " (change " + ch.ID + ")"})
}

func m2target(ch CloudChange) string {
	name := ch.ResourceName
	if ch.ResourceID != "" && ch.ResourceID != name {
		name += " (" + ch.ResourceID + ")"
	}
	return "aws " + ch.AccountName + " " + ch.Region + " " + name
}

// applyCloudChange runs an applying change as the person who asked, and
// records the outcome.
func (a *API) applyCloudChange(r *http.Request, m cloudManaged, ch CloudChange) CloudChange {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), cloudApplyTimeout)
	defer cancel()
	actor := cloud.Actor(ch.RequestedBy)
	var res cloud.Result
	var err error
	switch ch.Action {
	case cloud.ActionStart, cloud.ActionStop, cloud.ActionReboot:
		res, err = m.manager.Power(ctx, m.target, actor, ch.Region, ch.ResourceID, ch.Action)
	case cloud.ActionProtect, cloud.ActionUnprotect:
		res, err = m.manager.Protect(ctx, m.target, actor, ch.Region, ch.ResourceID, ch.Action == cloud.ActionProtect)
	case cloud.ActionResize:
		var s cloudResizeSpec
		_ = json.Unmarshal(ch.Spec, &s)
		res, err = m.manager.Resize(ctx, m.target, actor, ch.Region, ch.ResourceID, s.Size)
	case cloud.ActionTags:
		var s cloudTagsSpec
		_ = json.Unmarshal(ch.Spec, &s)
		res, err = m.manager.SetTags(ctx, m.target, actor, ch.Region, ch.ResourceID, s.Set, s.Remove)
	case cloud.ActionLaunch:
		res, err = a.applyLaunch(ctx, r, m, ch)
	case cloud.ActionRemove:
		res, err = a.applyRemove(ctx, r, m, ch)
		if errors.Is(err, errPlanChanged) {
			return a.mustReread(r, ch)
		}
	default:
		err = fmt.Errorf("%q is not a change Argus makes", ch.Action)
	}
	cause := ""
	if err != nil {
		cause = err.Error()
	}
	done, ferr := a.store.FinishCloudChange(context.WithoutCancel(r.Context()), ch.ID, err == nil, res, cause)
	if ferr != nil {
		a.log.Error("cloud change: cannot record the outcome", "change", ch.ID, "error", ferr)
		done = ch
	}
	if err != nil {
		a.auditCloudChange(r, done, "failed", ch.RequestedBy, ch.Summary+" failed: "+cause)
	} else {
		detail := ch.Summary + " done"
		if res.RequestID != "" {
			detail += "; AWS request " + res.RequestID
		}
		if res.ResourceID != "" && ch.Action == cloud.ActionLaunch {
			detail += "; instance " + res.ResourceID
		}
		if len(res.Snapshots) > 0 {
			detail += "; final snapshots " + strings.Join(sortedMapValues(res.Snapshots), ", ")
		}
		if res.Detail != "" {
			detail += ". " + res.Detail
		}
		a.auditCloudChange(r, done, "applied", ch.RequestedBy, detail+".")
	}
	a.syncCloudAccountSoon(r.Context(), m.acct)
	return done
}

var errPlanChanged = errors.New("the instance changed since this was approved")

func (a *API) mustReread(r *http.Request, ch CloudChange) CloudChange {
	c, err := a.store.CloudChange(r.Context(), ch.ID)
	if err != nil {
		return ch
	}
	return c
}

func sortedMapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (a *API) applyLaunch(ctx context.Context, r *http.Request, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	var s cloudLaunchSpec
	if err := json.Unmarshal(ch.Spec, &s); err != nil {
		return cloud.Result{}, err
	}
	spec := cloud.LaunchSpec{Region: ch.Region, Name: s.Name, Image: s.Image, Size: s.Size, DiskGB: s.DiskGB,
		Subnet: s.Subnet, Firewalls: s.Firewalls, Protect: s.Protect, PublicIP: s.PublicIP,
		Tags: map[string]string{"argus:launched-by": ch.RequestedBy, "argus:change": ch.ID}}
	for k, v := range s.Tags {
		spec.Tags[k] = v
	}
	detail := ""
	if s.Agent {
		// One hour, single use, and only for this server's name: the token
		// sits in the instance's user data, which anyone who can describe
		// its attributes can read.
		tok, err := a.store.CreateEnrollToken(ctx, ch.RequestedBy, s.Name, time.Hour)
		if err != nil {
			return cloud.Result{}, fmt.Errorf("could not make the agent's enrollment token: %w", err)
		}
		ud, problem := a.agentUserData(r, s.Name, tok.Token)
		if problem != "" {
			return cloud.Result{}, errors.New(problem)
		}
		spec.UserData = ud
		a.auditAsset(r, AuditEvent{Action: "agent.enroll_token.created", Severity: "info", ActorEmail: ch.RequestedBy,
			Target: s.Name, Detail: "Made a single-use agent enrollment token for " + s.Name + " only, valid for an hour, " +
				"for the instance launched by cloud change " + ch.ID + " (token " + tok.ID + ")."})
		detail = "The agent enrolls as " + s.Name + " when the instance first boots"
	}
	res, err := m.manager.Launch(ctx, m.target, cloud.Actor(ch.RequestedBy), spec)
	if err == nil && detail != "" {
		res.Detail = detail
	}
	return res, err
}

func (a *API) applyRemove(ctx context.Context, r *http.Request, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	var s cloudRemoveSpec
	_ = json.Unmarshal(ch.Spec, &s)
	inst, err := a.store.CloudInstance(ctx, m.acct.ID, ch.Region, ch.ResourceID)
	if err != nil {
		return cloud.Result{}, err
	}
	v, err := a.removePlan(ctx, m, ch.RequestedBy, ch.Region, inst)
	if err != nil {
		return cloud.Result{}, err
	}
	if v.Refusal != "" {
		return cloud.Result{}, errors.New("Argus did not remove it: " + v.Refusal)
	}
	if v.Hash != ch.PlanHash {
		// Something about it changed since the approver looked. Show them
		// the new plan rather than run one nobody saw.
		plan, _ := json.Marshal(v)
		summary := "Remove " + inst.Name + removeWords(v, s.FinalSnapshots)
		if _, err := a.store.ReplanCloudChange(ctx, ch.ID, plan, v.Hash, summary); err != nil {
			return cloud.Result{}, err
		}
		a.auditCloudChange(r, ch, "replanned", ch.RequestedBy, "Not run: "+inst.Name+" changed since the plan was approved. It waits for a decision on the new plan")
		return cloud.Result{}, errPlanChanged
	}
	res, err := m.manager.Remove(ctx, m.target, cloud.Actor(ch.RequestedBy), ch.Region, ch.ResourceID, s.FinalSnapshots)
	if err != nil {
		return res, err
	}
	if v.Server != "" {
		if asset, ok := a.assetByHostname(ctx, v.Server); ok {
			if aerr := a.store.ArchiveAsset(ctx, asset.ID); aerr == nil {
				res.Detail = "Retired the Argus server " + v.Server + "; its sessions and recordings stay readable"
				a.auditAsset(r, AuditEvent{Action: "asset.archived", Severity: "notice", ActorEmail: ch.RequestedBy,
					Target: v.Server, Detail: "Retired because its instance " + ch.ResourceID + " was removed (cloud change " + ch.ID + ")."})
			}
		}
	}
	return res, nil
}

func (a *API) assetByHostname(ctx context.Context, host string) (Asset, bool) {
	list, err := a.store.Assets(ctx)
	if err != nil {
		return Asset{}, false
	}
	for _, s := range list {
		if strings.EqualFold(s.Hostname, host) {
			return s, true
		}
	}
	return Asset{}, false
}

// postCloudChangeDecision approves (and runs) or declines a waiting change.
// The person who asked cannot decide their own.
func (a *API) postCloudChangeDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !a.privileged(w, sess, "deciding a cloud change", "approver", "admin", "owner") {
			return
		}
		ch, err := a.store.CloudChange(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrCloudChangeNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			a.fail(w, "cloud change", err)
			return
		}
		if strings.EqualFold(ch.RequestedBy, sess.Email) {
			writeErr(w, http.StatusForbidden, "someone other than the person who asked decides a change")
			return
		}
		m, code, err := a.cloudManagedAccount(r.Context(), ch.AccountID)
		if approve && err != nil {
			writeErr(w, code, err.Error())
			return
		}
		ch, err = a.store.DecideCloudChange(r.Context(), ch.ID, sess.Email, approve)
		if errors.Is(err, ErrCloudChangeSettled) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			a.fail(w, "cloud change", err)
			return
		}
		if !approve {
			a.auditCloudChange(r, ch, "declined", sess.Email, "Declined: "+ch.Summary+".")
			writeJSON(w, http.StatusOK, ch)
			return
		}
		a.auditCloudChange(r, ch, "approved", sess.Email, "Approved: "+ch.Summary+", asked by "+ch.RequestedBy+".")
		ch = a.applyCloudChange(r, m, ch)
		writeJSON(w, http.StatusOK, ch)
	}
}

func (a *API) postCloudChangeCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ch, err := a.store.CancelCloudChange(r.Context(), r.PathValue("id"), sess.Email)
	if errors.Is(err, ErrCloudChangeNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, ErrCloudChangeSettled) {
		writeErr(w, http.StatusConflict, "only the person who asked can withdraw a change, while it waits")
		return
	}
	if err != nil {
		a.fail(w, "cloud change", err)
		return
	}
	a.auditCloudChange(r, ch, "cancelled", sess.Email, "Withdrawn: "+ch.Summary+".")
	writeJSON(w, http.StatusOK, ch)
}

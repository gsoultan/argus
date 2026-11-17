package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Scale groups, the API: list and read them, the wizard's answers worked out
from a server, and the four changes (make a group, change it, add a server,
take one away). Every change goes through the usual plan, approval and
apply path (cloud_change_routes.go), so a group is made and changed the way
any cloud change is, and Argus's own adds and take-aways are changes too:
asked each time, or run by themselves within the limits (scaler.go).
*/

func (a *API) registerScaleGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/cloud/scale-groups", a.getScaleGroups)
	mux.HandleFunc("GET /api/v1/cloud/scale-groups/draft", a.getScaleGroupDraft)
	mux.HandleFunc("GET /api/v1/cloud/scale-groups/{id}", a.getScaleGroup)
}

// scaleSettings prefixes a group's id for a change to its settings.
const scaleSettings = "settings:"

// scaleGroupIDOf is the group a scale change is about, or "" for one that
// makes a group.
func scaleGroupIDOf(ch CloudChange) string {
	if strings.HasPrefix(ch.ResourceID, "scale:") {
		return ""
	}
	return strings.TrimPrefix(ch.ResourceID, scaleSettings)
}

var reScaleName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// scaleCreateSpec is what making a group asks for, and what the plan adds.
type scaleCreateSpec struct {
	Name string `json:"name"`
	// From is the instance the group is made from; it is its first member.
	From string `json:"from"`
	// Image is an existing image to launch from; empty makes one of From.
	Image  string `json:"image"`
	Reboot bool   `json:"reboot"`
	// Size is the members' instance type; From's when empty.
	Size        string            `json:"size"`
	TargetGroup string            `json:"targetGroup"`
	Subnets     []string          `json:"subnets"`
	Limits      cloud.ScaleLimits `json:"limits"`
	Rules       cloud.ScaleRules  `json:"rules"`
	// Mode is ask, auto, or aws (AWS Auto Scaling decides, keeping Track).
	Mode  string         `json:"mode"`
	Track cloud.AWSTrack `json:"track"`

	// Worked out when planned.
	Template   scaleTemplate `json:"template"`
	TargetName string        `json:"targetName"`
	Balancer   string        `json:"balancer"`
	Port       int32         `json:"port"`
	EnrollBase string        `json:"enrollBase"`
}

// scaleChangeSpec changes a group; a nil field is left as it is.
type scaleChangeSpec struct {
	Limits *cloud.ScaleLimits `json:"limits,omitempty"`
	Rules  *cloud.ScaleRules  `json:"rules,omitempty"`
	Mode   string             `json:"mode,omitempty"`
	// Track is what AWS keeps the group near when it decides.
	Track *cloud.AWSTrack `json:"track,omitempty"`
	// Pause pauses (true) or resumes (false) the group.
	Pause *bool `json:"pause,omitempty"`
}

// scaleAddSpec is one server added to a group.
type scaleAddSpec struct {
	Hostname string `json:"hostname"`
	Subnet   string `json:"subnet"`
	Zone     string `json:"zone"`
	// Keep is an add to bring the group back to its minimum.
	Keep bool `json:"keep,omitempty"`
}

// scaleRemoveSpec is one server taken out of a group.
type scaleRemoveSpec struct {
	Instance string `json:"instance"`
	Hostname string `json:"hostname"`
}

// scalePlan is what an add or take-away was asked on: the rule, the chart
// that met it, and the cost.
type scalePlan struct {
	Why       string       `json:"why"`
	Signal    string       `json:"signal"`
	Threshold float64      `json:"threshold"`
	Series    []scalePoint `json:"series"`
	Monthly   *float64     `json:"monthly"`
	After     int          `json:"after"`
	Min       int          `json:"min"`
	Max       int          `json:"max"`
	GroupCost *float64     `json:"groupCost"`
	CostCap   float64      `json:"costCap"`
	Automatic bool         `json:"automatic"`
	AutoBy    string       `json:"autoBy,omitempty"`
}

type scalePoint struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// planScale plans one of the scale group changes.
func (a *API) planScale(r *http.Request, m cloudManaged, actor string, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	if _, ok := m.manager.(cloud.Network); !ok {
		return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot run scale groups from Argus yet")
	}
	if in.Action == cloud.ActionScaleGroupCreate {
		return a.planScaleCreate(r, m, in, ch)
	}
	g, err := a.store.ScaleGroup(r.Context(), in.ResourceID)
	if errors.Is(err, ErrScaleGroupNotFound) || (err == nil && (g.AccountID != m.acct.ID || g.Region != in.Region)) {
		return ch, http.StatusNotFound, ErrScaleGroupNotFound
	}
	if err != nil {
		return ch, http.StatusInternalServerError, err
	}
	ch.ResourceID, ch.ResourceName, ch.Environment = g.ID, g.Name, g.Environment
	switch in.Action {
	case cloud.ActionScaleGroupChange:
		// A change to the settings has its own slot, so a question Argus
		// asked about adding never blocks a person changing the rules.
		ch.ResourceID = scaleSettings + g.ID
		return a.planScaleChange(r.Context(), g, in, ch)
	case cloud.ActionScaleAdd:
		return a.planScaleAdd(r.Context(), g, ch, false)
	}
	return a.planScaleRemove(r.Context(), g, ch)
}

func (a *API) planScaleCreate(r *http.Request, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	ctx := r.Context()
	if _, ok := m.manager.(cloud.Storage); !ok {
		return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot make images from Argus yet")
	}
	var s scaleCreateSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a scale group takes {name, from, targetGroup, subnets, limits, rules, mode}")
	}
	s.Name = strings.ToLower(strings.TrimSpace(s.Name))
	if !reScaleName.MatchString(s.Name) {
		return ch, http.StatusBadRequest, errors.New("name the group with lowercase letters, digits and dashes, up to 32; its servers are named after it")
	}
	if err := s.Limits.Check(); err != nil {
		return ch, http.StatusBadRequest, err
	}
	if err := s.Rules.Check(); err != nil {
		return ch, http.StatusBadRequest, err
	}
	if s.Mode == "" {
		s.Mode = cloud.ScaleAsk
	}
	if s.Mode != cloud.ScaleAsk && s.Mode != cloud.ScaleAuto && s.Mode != cloud.ScaleAWS {
		return ch, http.StatusBadRequest, errors.New("who decides is ask (each time), auto (within the limits) or aws (AWS Auto Scaling)")
	}
	if s.Mode == cloud.ScaleAWS {
		if _, ok := m.manager.(cloud.Floors); !ok {
			return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot make Auto Scaling groups from Argus yet")
		}
		if s.Track.Metric == "" {
			s.Track = cloud.DefaultAWSTrack()
		}
	} else {
		s.Track = cloud.AWSTrack{}
	}
	src, err := a.store.CloudInstance(ctx, m.acct.ID, in.Region, s.From)
	if err != nil {
		return ch, http.StatusBadRequest, errors.New("pick the server to make the group from")
	}
	if src.State != "running" {
		return ch, http.StatusConflict, errors.New(src.Name + " is " + src.State + "; a group is made from a running server")
	}
	if g, _ := a.scaleGroupOf(ctx, m.acct.ID, src.ID); g != nil {
		return ch, http.StatusConflict, errors.New(src.Name + " is already in the scale group " + g.Name)
	}
	if s.Size == "" {
		s.Size = src.Size
	}
	if !cloud.ValidSize(s.Size) {
		return ch, http.StatusBadRequest, errors.New("pick an instance type, like m6i.large")
	}
	tg, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindTargetGroup, s.TargetGroup)
	if err != nil {
		return ch, http.StatusBadRequest, errors.New("pick the target group traffic comes in through, or make one first under Network")
	}
	if t := tg.Details["targetType"]; t != "" && t != "instance" {
		return ch, http.StatusBadRequest, errors.New(tg.Name + " sends traffic to " + t + " targets, not servers")
	}
	if tg.Network != "" && src.Network != "" && tg.Network != src.Network {
		return ch, http.StatusBadRequest, errors.New(tg.Name + " is in another network than " + src.Name)
	}
	port, _ := strconv.Atoi(tg.Details["port"])
	if len(s.Subnets) == 0 {
		for _, l := range src.Links {
			if l.Kind == cloud.KindSubnet {
				s.Subnets = []string{l.ID}
			}
		}
	}
	if len(s.Subnets) == 0 || len(s.Subnets) > 6 {
		return ch, http.StatusBadRequest, errors.New("pick 1 to 6 subnets for the group's servers; one in each zone spreads them")
	}
	zones := map[string]bool{}
	for i, id := range s.Subnets {
		if slices.Contains(s.Subnets[:i], id) {
			return ch, http.StatusBadRequest, errors.New("pick each subnet once")
		}
		sub, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindSubnet, id)
		if err != nil || (src.Network != "" && sub.Network != src.Network) {
			return ch, http.StatusBadRequest, errors.New("pick subnets in " + src.Name + "'s network")
		}
		zones[sub.Zone] = true
	}
	if s.Image != "" {
		img, err := a.store.CloudResourceOf(ctx, m.acct.ID, in.Region, cloud.KindImage, s.Image)
		if err != nil {
			return ch, http.StatusBadRequest, errors.New("pick an image in " + in.Region + ", or let Argus make one of " + src.Name)
		}
		s.Template.ImageLabel = img.Name
	}
	if problem := a.scaleCanEnroll(r); problem != "" {
		return ch, http.StatusConflict, errors.New("a new server's agent could not enroll by itself: " + problem)
	}
	if s.Mode == cloud.ScaleAuto && s.Limits.CostCap == 0 {
		return ch, http.StatusBadRequest, errors.New("automatic needs a cost cap, so the limits say what it may spend")
	}
	s.Template.Image, s.Template.Size, s.Template.Subnets = s.Image, s.Size, s.Subnets
	s.Template.DiskGB = a.rootDiskGB(ctx, m.acct.ID, in.Region, src)
	s.Template.From, s.Template.FromName = src.ID, src.Name
	for _, l := range src.Links {
		if l.Kind == cloud.KindFirewall {
			s.Template.Firewalls = append(s.Template.Firewalls, l.ID)
		}
	}
	s.Template.Tags = map[string]string{}
	for k, v := range src.Tags {
		if k != "Name" && !strings.HasPrefix(k, "aws:") && !strings.HasPrefix(k, "argus:") && k != cloud.ScheduleTag {
			s.Template.Tags[k] = v
		}
	}
	s.TargetName, s.Port, s.Balancer = tg.Name, int32(port), lbOf(tg)
	if s.Mode == cloud.ScaleAWS {
		if err := awsModeProblem(s.Track, s.Limits, s.Size, a.requestLabelOf(tg)); err != nil {
			return ch, http.StatusBadRequest, err
		}
	}
	s.EnrollBase = strings.TrimRight(a.enrolAddress(r), "/")
	ch.Environment = cloudEnvOf(src.Tags, m.acct.Environment)
	ch.ResourceID, ch.ResourceName = "scale:"+s.Name, s.Name
	if a.scaleNameTaken(ctx, m.acct.ID, in.Region, s.Name) {
		return ch, http.StatusConflict, ErrScaleGroupTaken
	}
	ch.Spec, _ = json.Marshal(s)
	what := "a new image of " + src.Name
	if s.Image == "" && !s.Reboot {
		what += " (no reboot)"
	} else if s.Image == "" {
		what += " (rebooted for a consistent disk)"
	} else {
		what = "image " + s.Template.ImageLabel
	}
	ch.Summary = fmt.Sprintf("Make scale group %s from %s: %s, %s in %d zone%s, in %s behind %s, %d to %d servers%s, %s",
		s.Name, src.Name, what, s.Size, len(zones), plural(len(zones)), tg.Name, s.Balancer, s.Limits.Min, s.Limits.Max,
		costRange(s.Size, s.Limits), modeWords(s.Mode))
	if s.Mode == cloud.ScaleAWS {
		ch.Summary += ", keeping " + s.Track.Words()
	}
	return ch, http.StatusOK, nil
}

// requestLabelOf is CloudWatch's name for a target group behind an
// application load balancer, or "".
func (a *API) requestLabelOf(tg cloud.Resource) string {
	for _, l := range tg.Links {
		if l.Kind == cloud.KindLoadBalancer {
			if label := cloud.RequestLabel(l.ID, tg.ID); label != "" {
				return label
			}
		}
	}
	return ""
}

// awsModeProblem says why AWS cannot decide for a group with these
// settings, or nil. AWS knows nothing of Argus's cost cap, so the maximum
// has to keep under it.
func awsModeProblem(t cloud.AWSTrack, l cloud.ScaleLimits, size, label string) error {
	if err := t.Check(); err != nil {
		return err
	}
	if t.Metric == cloud.TrackRequests && label == "" {
		return errors.New("AWS tracks requests per server only behind an application load balancer; track the processor instead")
	}
	if p, ok := monthly(size); ok && l.CostCap > 0 && p*float64(l.Max) > l.CostCap {
		fit := int(l.CostCap / p)
		return fmt.Errorf("AWS does not know the cost cap, so the maximum has to keep under it: %d %s are about %s a month, over %s; lower the maximum to %d or raise the cap",
			l.Max, size, money(p*float64(l.Max)), money(l.CostCap), fit)
	}
	return nil
}

// costRange is " (about $122 to $366 a month, cap $600)" for a group's
// size, or "" when the price is not known.
func costRange(size string, l cloud.ScaleLimits) string {
	p, ok := monthly(size)
	if !ok {
		return ""
	}
	out := " (about " + money(p*float64(l.Min)) + " to " + money(p*float64(l.Max)) + " a month"
	if l.CostCap > 0 {
		out += ", cap " + money(l.CostCap)
	}
	return out + ")"
}

func modeWords(mode string) string {
	switch mode {
	case cloud.ScaleAuto:
		return "Argus adds and takes away by itself within the limits"
	case cloud.ScaleAWS:
		return "AWS Auto Scaling decides, even when Argus is down"
	}
	return "ask each time"
}

func (a *API) planScaleChange(ctx context.Context, g ScaleGroup, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	var s scaleChangeSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a group change takes {limits, rules, mode, pause}")
	}
	var parts []string
	if s.Pause != nil {
		if s.Limits != nil || s.Rules != nil || s.Mode != "" || s.Track != nil {
			return ch, http.StatusBadRequest, errors.New("pause or resume on its own")
		}
		if *s.Pause == (g.State == "paused") {
			return ch, http.StatusBadRequest, errors.New(g.Name + " is already " + g.State)
		}
		// Pausing only stops Argus acting, so it never waits for anyone;
		// resuming lets it act again within limits already approved.
		ch.Level = cloud.LevelOperate
		if *s.Pause && g.Mode == cloud.ScaleAWS {
			ch.Summary = "Pause scale group " + g.Name + ": AWS stops adding and taking away, and keeps the servers it has"
		} else if *s.Pause {
			ch.Summary = "Pause scale group " + g.Name + ": Argus stops adding and taking away"
		} else {
			ch.Summary = "Resume scale group " + g.Name
		}
		ch.Spec, _ = json.Marshal(s)
		return ch, http.StatusOK, nil
	}
	if s.Limits != nil {
		if err := s.Limits.Check(); err != nil {
			return ch, http.StatusBadRequest, err
		}
		l, o := *s.Limits, g.Limits
		if l.Min != o.Min || l.Max != o.Max {
			parts = append(parts, fmt.Sprintf("%d to %d servers (was %d to %d)", l.Min, l.Max, o.Min, o.Max))
		}
		if l.CostCap != o.CostCap {
			parts = append(parts, "cap "+capWords(l.CostCap)+" (was "+capWords(o.CostCap)+")")
		}
		if l.AddWait != o.AddWait || l.RemoveWait != o.RemoveWait {
			parts = append(parts, fmt.Sprintf("wait %d min after adding and %d after taking away", l.AddWait, l.RemoveWait))
		}
	}
	if s.Rules != nil {
		if err := s.Rules.Check(); err != nil {
			return ch, http.StatusBadRequest, err
		}
		parts = append(parts, "add when "+ruleList(s.Rules.Add, " or ")+"; take away when "+ruleList(s.Rules.Remove, " and "))
	}
	limits, mode := g.Limits, g.Mode
	if s.Limits != nil {
		limits = *s.Limits
	}
	if s.Mode != "" {
		mode = s.Mode
	}
	if s.Mode != "" && s.Mode != g.Mode {
		if s.Mode != cloud.ScaleAsk && s.Mode != cloud.ScaleAuto && s.Mode != cloud.ScaleAWS {
			return ch, http.StatusBadRequest, errors.New("who decides is ask, auto or aws")
		}
		if s.Mode == cloud.ScaleAuto && limits.CostCap == 0 {
			return ch, http.StatusBadRequest, errors.New("automatic needs a cost cap, so the limits say what it may spend")
		}
		if s.Mode == cloud.ScaleAWS {
			// AWS takes over from the floor up; what Argus launched itself
			// above it is Argus's to take away first.
			if extra := g.argusCopies(); len(extra) > 0 {
				return ch, http.StatusConflict, fmt.Errorf("Argus launched %s itself; take %s away first, then AWS can decide",
					extra[0].Hostname, map[bool]string{true: "it", false: "them"}[len(extra) == 1])
			}
		}
		words := modeWords(s.Mode)
		if g.Mode == cloud.ScaleAWS && g.AWSGroup != "" {
			if d, ok := a.floorDesired(ctx, g); ok && d > floorCopies(g) {
				words += fmt.Sprintf("; AWS ends the %d server%s it added above the minimum", d-floorCopies(g), plural(int(d-floorCopies(g))))
			}
		}
		parts = append(parts, words)
	}
	if mode == cloud.ScaleAWS {
		t := g.AWSTrack
		if s.Track != nil {
			t = *s.Track
		}
		if t.Metric == "" {
			t = cloud.DefaultAWSTrack()
		}
		label := ""
		if tg, err := a.store.CloudResourceOf(ctx, g.AccountID, g.Region, cloud.KindTargetGroup, g.TargetGroup); err == nil {
			label = a.requestLabelOf(tg)
		}
		if err := awsModeProblem(t, limits, g.Template.Size, label); err != nil {
			return ch, http.StatusBadRequest, err
		}
		if s.Track != nil || s.Mode == cloud.ScaleAWS {
			s.Track = &t
			if t != g.AWSTrack || s.Mode == cloud.ScaleAWS {
				parts = append(parts, "AWS keeps "+t.Words())
			}
		}
	} else {
		s.Track = nil
	}
	if len(parts) == 0 {
		return ch, http.StatusBadRequest, errors.New("nothing to change")
	}
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = "Change scale group " + g.Name + ": " + strings.Join(parts, "; ")
	return ch, http.StatusOK, nil
}

func capWords(c float64) string {
	if c == 0 {
		return "none"
	}
	return money(c) + " a month"
}

func ruleList(rs []cloud.ScaleRule, sep string) string {
	if len(rs) == 0 {
		return "never"
	}
	w := make([]string, len(rs))
	for i, r := range rs {
		w[i] = r.Words()
	}
	return strings.Join(w, sep)
}

// active is the group's servers now: starting, serving or draining.
func (g ScaleGroup) active() []ScaleMember {
	var out []ScaleMember
	for _, m := range g.Members {
		if m.Active() {
			out = append(out, m)
		}
	}
	return out
}

// serving is the group's servers that count towards its size: not being
// taken away.
func (g ScaleGroup) serving() []ScaleMember {
	var out []ScaleMember
	for _, m := range g.Members {
		if m.State == "joining" || m.State == "member" {
			out = append(out, m)
		}
	}
	return out
}

// planScaleAdd plans one more server; keep is an add to get back to the
// minimum.
func (a *API) planScaleAdd(ctx context.Context, g ScaleGroup, ch CloudChange, keep bool) (CloudChange, int, error) {
	if g.Mode == cloud.ScaleAWS {
		return ch, http.StatusConflict, errors.New("AWS decides how many servers " + g.Name + " has; change who decides first")
	}
	n := len(g.serving())
	if n >= g.Limits.Max {
		return ch, http.StatusConflict, fmt.Errorf("%s is at its maximum of %d; raise it first, which is its own change", g.Name, g.Limits.Max)
	}
	price, priced := monthly(g.Template.Size)
	if g.Limits.CostCap > 0 && priced && price*float64(n+1) > g.Limits.CostCap {
		return ch, http.StatusConflict, fmt.Errorf("one more %s would make %s about %s a month, over its cap of %s",
			g.Template.Size, g.Name, money(price*float64(n+1)), money(g.Limits.CostCap))
	}
	if g.Template.Image == "" {
		return ch, http.StatusConflict, errors.New(g.Name + " has no image to launch from yet")
	}
	img, err := a.store.CloudResourceOf(ctx, g.AccountID, g.Region, cloud.KindImage, g.Template.Image)
	if err == nil && img.State != "" && img.State != "available" {
		return ch, http.StatusConflict, fmt.Errorf("the image %s is %s; AWS takes a few minutes to make one", g.Template.Image, img.State)
	}
	s := scaleAddSpec{Keep: keep}
	s.Subnet, s.Zone = a.scaleSubnet(ctx, g)
	s.Hostname = a.scaleHostname(ctx, g)
	if s.Hostname == "" {
		return ch, http.StatusConflict, errors.New("Argus could not find a free name for a new server in " + g.Name)
	}
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = fmt.Sprintf("Add %s to %s: launch it from %s (%s", s.Hostname, g.Name, imageWords(g.Template), g.Template.Size)
	if s.Zone != "" {
		ch.Summary += ", " + s.Zone
	}
	ch.Summary += "), register it in " + g.TargetName + " when it runs"
	if priced {
		ch.Summary += ", +" + money(price) + " a month"
	}
	return ch, http.StatusOK, nil
}

func imageWords(t scaleTemplate) string {
	if t.ImageLabel != "" {
		return t.ImageLabel
	}
	return t.Image
}

// planScaleRemove plans taking the newest server away that can be.
func (a *API) planScaleRemove(ctx context.Context, g ScaleGroup, ch CloudChange) (CloudChange, int, error) {
	if g.Mode == cloud.ScaleAWS {
		return ch, http.StatusConflict, errors.New("AWS decides how many servers " + g.Name + " has; change who decides first")
	}
	serving := g.serving()
	if len(serving) <= g.Limits.Min {
		return ch, http.StatusConflict, fmt.Errorf("%s is at its minimum of %d", g.Name, g.Limits.Min)
	}
	var pick *ScaleMember
	for i := len(serving) - 1; i >= 0; i-- {
		m := serving[i]
		if m.State != "member" {
			continue
		}
		// The server the group was made from goes last: it is usually the
		// one people know, and protected from termination.
		if m.InstanceID == g.Template.From {
			continue
		}
		// What AWS started is the floor it keeps; ending one, AWS would
		// start another.
		if m.InAWS {
			continue
		}
		pick = &m
		break
	}
	if pick == nil {
		return ch, http.StatusConflict, errors.New("every server " + g.Name + " could take away is still starting, or kept by AWS")
	}
	s := scaleRemoveSpec{Instance: pick.InstanceID, Hostname: pick.Hostname}
	ch.Spec, _ = json.Marshal(s)
	ch.Summary = fmt.Sprintf("Take %s out of %s: drain it from %s for %d s, then terminate it", pick.Hostname, g.Name, g.TargetName,
		int(cloud.ScaleDrain.Seconds()))
	if p, ok := monthly(g.Template.Size); ok {
		ch.Summary += ", -" + money(p) + " a month"
	}
	return ch, http.StatusOK, nil
}

// scaleSubnet is the group's subnet with fewest servers, so they spread
// across zones.
func (a *API) scaleSubnet(ctx context.Context, g ScaleGroup) (string, string) {
	count := map[string]int{}
	for _, m := range g.serving() {
		count[m.Subnet]++
	}
	best, zone := "", ""
	for _, id := range g.Template.Subnets {
		if best == "" || count[id] < count[best] {
			best = id
		}
	}
	if sub, err := a.store.CloudResourceOf(ctx, g.AccountID, g.Region, cloud.KindSubnet, best); err == nil {
		zone = sub.Zone
	}
	return best, zone
}

// scaleHostname is the next free name in the group: web-01, web-02, …
func (a *API) scaleHostname(ctx context.Context, g ScaleGroup) string {
	used := map[string]bool{}
	for _, m := range g.Members {
		used[m.Hostname] = true
	}
	if items, err := a.store.CloudResources(ctx, cloud.KindInstance); err == nil {
		for _, it := range items {
			used[strings.ToLower(it.Resource.Name)] = true
		}
	}
	if list, err := a.store.Assets(ctx); err == nil {
		for _, s := range list {
			used[strings.ToLower(s.Hostname)] = true
		}
	}
	for i := 1; i < 1000; i++ {
		n := fmt.Sprintf("%s-%02d", g.Name, i)
		if !used[n] && cloud.ValidHostname(n) {
			return n
		}
	}
	return ""
}

// rootDiskGB is the size of an instance's root disk, from its volume.
func (a *API) rootDiskGB(ctx context.Context, acct, region string, inst cloud.Resource) int32 {
	root := inst.Details["rootDeviceName"]
	for _, l := range inst.Links {
		if l.Kind != cloud.KindVolume || (root != "" && l.Name != root) {
			continue
		}
		if v, err := a.store.CloudResourceOf(ctx, acct, region, cloud.KindVolume, l.ID); err == nil {
			if gb, _ := volumeGB(v); gb > 0 {
				return gb
			}
		}
	}
	return 20
}

// scaleCanEnroll says why a new server's agent could not enroll by itself,
// or "".
func (a *API) scaleCanEnroll(r *http.Request) string {
	_, problem := a.agentUserData(r, "check", "check")
	return problem
}

func (a *API) scaleNameTaken(ctx context.Context, acct, region, name string) bool {
	list, err := a.store.ScaleGroups(ctx)
	if err != nil {
		return false
	}
	for _, g := range list {
		if g.AccountID == acct && g.Region == region && g.Name == name {
			return true
		}
	}
	return false
}

// scaleGroupOf is the group an instance is in now, or nil.
func (a *API) scaleGroupOf(ctx context.Context, acct, instance string) (*ScaleGroup, error) {
	list, err := a.store.ScaleGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range list {
		if g.AccountID != acct {
			continue
		}
		for _, m := range g.active() {
			if m.InstanceID == instance {
				return &g, nil
			}
		}
	}
	return nil, nil
}

// applyScale runs a scale group change. actor is who it runs as: the person
// who asked, or argus-scale for Argus's own.
func (a *API) applyScale(ctx context.Context, r *http.Request, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	actor := cloud.Actor(ch.RequestedBy)
	nw := m.manager.(cloud.Network)
	switch ch.Action {
	case cloud.ActionScaleGroupCreate:
		var s scaleCreateSpec
		if err := json.Unmarshal(ch.Spec, &s); err != nil {
			return cloud.Result{}, err
		}
		res := cloud.Result{}
		if s.Template.Image == "" {
			st := m.manager.(cloud.Storage)
			img, err := st.CreateImage(ctx, m.target, actor, ch.Region, s.Template.From,
				"argus-"+s.Name+"-"+time.Now().UTC().Format("20060102-1504"), s.Reboot)
			if err != nil {
				return img, fmt.Errorf("making the image of %s: %w", s.Template.FromName, err)
			}
			s.Template.Image, s.Template.ImageLabel = img.ResourceID, "image of "+s.Template.FromName
			res.RequestID = img.RequestID
			res.Detail = "Made image " + img.ResourceID + " of " + s.Template.FromName + "; new servers start from it once AWS has finished it, in a few minutes"
		}
		g := ScaleGroup{AccountID: m.acct.ID, Region: ch.Region, Name: s.Name, Environment: ch.Environment,
			TargetGroup: s.TargetGroup, TargetName: s.TargetName, Port: s.Port, Balancer: s.Balancer, Template: s.Template,
			Limits: s.Limits, Rules: s.Rules, Mode: s.Mode, AWSTrack: s.Track, EnrollBase: s.EnrollBase, CreatedBy: ch.RequestedBy}
		if s.Mode == cloud.ScaleAuto {
			now := time.Now().UTC()
			g.AutoBy, g.AutoAt = ch.RequestedBy, &now
		}
		g, err := a.store.CreateScaleGroup(ctx, g)
		if err != nil {
			return res, err
		}
		res.ResourceID = g.ID
		src, _ := a.store.CloudInstance(ctx, m.acct.ID, ch.Region, s.Template.From)
		member := ScaleMember{InstanceID: s.Template.From, Hostname: strings.ToLower(s.Template.FromName), Zone: src.Zone,
			State: "member", AddedBy: ch.RequestedBy}
		for _, l := range src.Links {
			if l.Kind == cloud.KindSubnet {
				member.Subnet = l.ID
			}
		}
		if host := a.hostOfInstance(ctx, m.acct.ID, s.Template.From); host != "" {
			member.Hostname = host
		}
		now := time.Now().UTC()
		member.RegisteredAt = &now
		if !a.inTargetGroup(ctx, m.acct.ID, ch.Region, s.TargetGroup, s.Template.From) {
			if _, err := nw.RegisterTarget(ctx, m.target, actor, ch.Region, s.TargetGroup, s.Template.From, s.Port); err != nil {
				_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "failed", Actor: ch.RequestedBy, ChangeID: ch.ID,
					Detail: "Could not put " + s.Template.FromName + " in " + s.TargetName + ": " + err.Error()})
			}
		}
		if err := a.store.AddScaleMember(ctx, g.ID, member); err != nil {
			return res, err
		}
		detail := "Made " + g.Name + " from " + s.Template.FromName + ", " + modeWords(g.Mode)
		if ch.DecidedBy != nil {
			detail += "; approved by " + *ch.DecidedBy
		}
		_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "created", Actor: ch.RequestedBy, ChangeID: ch.ID, Detail: detail})
		return res, nil

	case cloud.ActionScaleGroupChange:
		g, err := a.store.ScaleGroup(ctx, scaleGroupIDOf(ch))
		if err != nil {
			return cloud.Result{}, err
		}
		var s scaleChangeSpec
		_ = json.Unmarshal(ch.Spec, &s)
		kind := "changed"
		if s.Pause != nil {
			g.State, g.PausedReason, kind = "active", "", "resumed"
			if *s.Pause {
				g.State, g.PausedReason, kind = "paused", "Paused by "+ch.RequestedBy, "paused"
			} else {
				_ = a.store.SetScaleFailedStarts(ctx, g.ID, 0)
			}
		}
		if s.Limits != nil {
			g.Limits = *s.Limits
		}
		if s.Rules != nil {
			g.Rules = *s.Rules
		}
		was := g.Mode
		if s.Mode != "" && s.Mode != g.Mode {
			g.Mode = s.Mode
			if s.Mode == cloud.ScaleAuto {
				now := time.Now().UTC()
				g.AutoBy, g.AutoAt = ch.RequestedBy, &now
			}
		}
		if s.Track != nil {
			g.AWSTrack = *s.Track
		}
		// AWS first: if it refuses, nothing changes in Argus either.
		if err := a.floorChange(ctx, m, actor, g, was); err != nil {
			return cloud.Result{}, err
		}
		if err := a.store.SetScaleGroupSettings(ctx, g); err != nil {
			return cloud.Result{}, err
		}
		detail := strings.TrimPrefix(ch.Summary, "Change scale group "+g.Name+": ")
		if ch.DecidedBy != nil {
			detail += "; approved by " + *ch.DecidedBy
		}
		_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: kind, Actor: ch.RequestedBy, ChangeID: ch.ID, Detail: detail})
		return cloud.Result{ResourceID: g.ID}, nil

	case cloud.ActionScaleAdd:
		g, err := a.store.ScaleGroup(ctx, ch.ResourceID)
		if err != nil {
			return cloud.Result{}, err
		}
		var s scaleAddSpec
		_ = json.Unmarshal(ch.Spec, &s)
		if len(g.serving()) >= g.Limits.Max {
			return cloud.Result{}, fmt.Errorf("%s reached its maximum of %d before this ran", g.Name, g.Limits.Max)
		}
		spec := cloud.LaunchSpec{Region: ch.Region, Name: s.Hostname, Image: g.Template.Image, Size: g.Template.Size,
			DiskGB: g.Template.DiskGB, Subnet: s.Subnet, Firewalls: g.Template.Firewalls,
			Tags: map[string]string{"argus:launched-by": ch.RequestedBy, "argus:change": ch.ID, "argus:scale-group": g.Name}}
		for k, v := range g.Template.Tags {
			spec.Tags[k] = v
		}
		tok, err := a.store.CreateEnrollToken(ctx, ch.RequestedBy, s.Hostname, time.Hour)
		if err != nil {
			return cloud.Result{}, fmt.Errorf("could not make the agent's enrollment token: %w", err)
		}
		ud, problem := a.agentUserData(r, s.Hostname, tok.Token)
		if problem != "" {
			return cloud.Result{}, errors.New(problem)
		}
		spec.UserData = ud
		res, err := m.manager.Launch(ctx, m.target, actor, spec)
		if err != nil {
			return res, err
		}
		now := time.Now().UTC()
		if err := a.store.AddScaleMember(ctx, g.ID, ScaleMember{InstanceID: res.ResourceID, Hostname: s.Hostname,
			Zone: s.Zone, Subnet: s.Subnet, State: "joining", AddedBy: ch.RequestedBy}); err != nil {
			return res, err
		}
		_ = a.store.SetScaleGroupStamp(ctx, g.ID, "add", now)
		res.Detail = s.Hostname + " joins " + g.TargetName + " once it runs; its agent enrolls when it first boots"
		_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "added", Actor: ch.RequestedBy, ChangeID: ch.ID,
			Detail: "Launched " + s.Hostname + " (" + res.ResourceID + ")" + whoLetIt(g, ch)})
		return res, nil
	}
	// Take one away: out of the target group now, terminated once drained.
	g, err := a.store.ScaleGroup(ctx, ch.ResourceID)
	if err != nil {
		return cloud.Result{}, err
	}
	var s scaleRemoveSpec
	_ = json.Unmarshal(ch.Spec, &s)
	if p, err := m.manager.PlanRemove(ctx, m.target, actor, ch.Region, s.Instance); err != nil {
		return cloud.Result{}, err
	} else if p.Protected {
		return cloud.Result{}, errors.New(s.Hostname + " is protected from termination; turn that off first, which is its own change")
	}
	res, err := nw.DeregisterTarget(ctx, m.target, actor, ch.Region, g.TargetGroup, s.Instance, g.Port)
	if err != nil {
		return res, err
	}
	now := time.Now().UTC()
	if err := a.store.SetScaleMember(ctx, g.ID, s.Instance, "draining", now.Add(cloud.ScaleDrain)); err != nil {
		return res, err
	}
	_ = a.store.SetScaleGroupStamp(ctx, g.ID, "remove", now)
	res.Detail = fmt.Sprintf("Draining %s for %d s, then Argus terminates it", s.Hostname, int(cloud.ScaleDrain.Seconds()))
	_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "removed", Actor: ch.RequestedBy, ChangeID: ch.ID,
		Detail: "Took " + s.Hostname + " out of " + g.TargetName + "; draining" + whoLetIt(g, ch)})
	return res, nil
}

// whoLetIt says who let an add or take-away happen: the approver, or the
// person who turned automatic on.
func whoLetIt(g ScaleGroup, ch CloudChange) string {
	if ch.DecidedBy != nil {
		return "; approved by " + *ch.DecidedBy
	}
	if ch.RequestedBy == string(cloud.ScaleActor) && g.AutoBy != "" {
		return "; automatic, turned on by " + g.AutoBy
	}
	return ""
}

func (a *API) inTargetGroup(ctx context.Context, acct, region, tg, instance string) bool {
	r, err := a.store.CloudResourceOf(ctx, acct, region, cloud.KindTargetGroup, tg)
	if err != nil {
		return false
	}
	for _, l := range r.Links {
		if l.Kind == cloud.KindInstance && l.ID == instance {
			return true
		}
	}
	return false
}

// targetHealth is each instance's health in a target group.
func (a *API) targetHealth(ctx context.Context, acct, region, tg string) map[string]string {
	out := map[string]string{}
	if r, err := a.store.CloudResourceOf(ctx, acct, region, cloud.KindTargetGroup, tg); err == nil {
		for _, l := range r.Links {
			if l.Kind == cloud.KindInstance {
				out[l.ID] = l.Name
			}
		}
	}
	return out
}

// hostOfInstance is the hostname of the Argus server matched to an
// instance, or "".
func (a *API) hostOfInstance(ctx context.Context, acct, instance string) string {
	items, err := a.store.CloudResources(ctx, cloud.KindInstance)
	if err != nil {
		return ""
	}
	for _, it := range items {
		if it.AccountID == acct && it.Resource.ID == instance && it.Server != nil {
			return it.Server.Hostname
		}
	}
	return ""
}

// scaleGroupView is a group as the console shows it.
type scaleGroupView struct {
	ScaleGroup
	// Now is the group's average processor over the last 10 minutes.
	Now     *float64             `json:"now"`
	Monthly *float64             `json:"monthly"`
	Each    *float64             `json:"each"`
	Last    *ScaleActivity       `json:"last"`
	Open    *CloudChange         `json:"open"`
	Health  map[string]string    `json:"health"`
	Usage   map[string]memberNow `json:"usage"`
	// AWS is the Auto Scaling group under the group, as last read.
	AWS awsSide `json:"aws"`
}

// awsSide is the Auto Scaling group in words, and its size when read.
type awsSide struct {
	Words   string `json:"words"`
	Min     *int   `json:"min"`
	Max     *int   `json:"max"`
	Desired *int   `json:"desired"`
}

type memberNow struct {
	CPU *float64 `json:"cpu"`
	Mem *float64 `json:"mem"`
}

func (a *API) scaleView(ctx context.Context, g ScaleGroup, now time.Time) scaleGroupView {
	v := scaleGroupView{ScaleGroup: g, Health: a.targetHealth(ctx, g.AccountID, g.Region, g.TargetGroup), Usage: map[string]memberNow{}}
	var hosts []string
	for _, m := range g.active() {
		hosts = append(hosts, m.Hostname)
	}
	if p, ok := monthly(g.Template.Size); ok {
		total := cents(p * float64(len(g.serving())))
		v.Monthly, v.Each = &total, &p
	}
	if samples, err := a.store.scaleSamples(ctx, hosts, now.Add(-10*time.Minute)); err == nil {
		sum, n := 0.0, 0
		for h, ss := range samples {
			var u memberNow
			cs, cn, ms := 0.0, 0, -1.0
			for _, s := range ss {
				if x, ok := s.Values[cloud.SignalCPU]; ok {
					cs += x
					cn++
				}
				if x, ok := s.Values[cloud.SignalMemory]; ok {
					ms = 100 - x
				}
			}
			if cn > 0 {
				c := cs / float64(cn)
				u.CPU = &c
				sum += c
				n++
			}
			if ms >= 0 {
				u.Mem = &ms
			}
			v.Usage[h] = u
		}
		if n > 0 {
			avg := sum / float64(n)
			v.Now = &avg
		}
	}
	if acts, err := a.store.ScaleActivityOf(ctx, g.ID, 1); err == nil && len(acts) > 0 {
		v.Last = &acts[0]
	}
	v.Open, _ = a.store.OpenScaleChange(ctx, g.AccountID, g.ID)
	v.AWS.Words = floorWords(g)
	if g.AWSGroup != "" {
		if r, err := a.store.CloudResourceOf(ctx, g.AccountID, g.Region, cloud.KindScalingGroup, g.AWSGroup); err == nil {
			num := func(k string) *int {
				if n, err := strconv.Atoi(r.Details[k]); err == nil {
					return &n
				}
				return nil
			}
			v.AWS.Min, v.AWS.Max, v.AWS.Desired = num("min"), num("max"), num("desired")
		}
	}
	return v
}

func (a *API) getScaleGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	list, err := a.store.ScaleGroups(r.Context())
	if err != nil {
		a.fail(w, "scale groups", err)
		return
	}
	now := time.Now().UTC()
	out := make([]scaleGroupView, 0, len(list))
	for _, g := range list {
		out = append(out, a.scaleView(r.Context(), g, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

// scaleGroupDetail is one group's page: the view, its activity, and each
// member's processor for the chart.
type scaleGroupDetail struct {
	scaleGroupView
	Activity []ScaleActivity         `json:"activity"`
	Series   map[string][]scalePoint `json:"series"`
}

func (a *API) getScaleGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	g, err := a.store.ScaleGroup(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrScaleGroupNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "scale group", err)
		return
	}
	now := time.Now().UTC()
	d := scaleGroupDetail{scaleGroupView: a.scaleView(r.Context(), g, now), Series: map[string][]scalePoint{}}
	if d.Activity, err = a.store.ScaleActivityOf(r.Context(), g.ID, 50); err != nil {
		a.fail(w, "scale group", err)
		return
	}
	var hosts []string
	for _, m := range g.Members {
		hosts = append(hosts, m.Hostname)
	}
	samples, err := a.store.scaleSamples(r.Context(), hosts, now.Add(-8*time.Hour))
	if err != nil {
		a.fail(w, "scale group", err)
		return
	}
	for h, ss := range samples {
		d.Series[h] = seriesOf(ss, cloud.SignalCPU)
	}
	writeJSON(w, http.StatusOK, d)
}

func seriesOf(ss []cloud.ScaleSample, signal string) []scalePoint {
	out := []scalePoint{}
	for _, s := range ss {
		if x, ok := s.Values[signal]; ok {
			out = append(out, scalePoint{At: s.At, Value: x})
		}
	}
	return out
}

// groupSeries is the group's average of signal per bucket up to now.
func groupSeries(samples map[string][]cloud.ScaleSample, signal string, now time.Time) []scalePoint {
	sum, n := map[time.Time]float64{}, map[time.Time]int{}
	for _, ss := range samples {
		for _, s := range ss {
			if x, ok := s.Values[signal]; ok && !s.At.After(now) {
				sum[s.At] += x
				n[s.At]++
			}
		}
	}
	out := make([]scalePoint, 0, len(sum))
	for at, v := range sum {
		out = append(out, scalePoint{At: at, Value: v / float64(n[at])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// scaleDraft is the wizard's answers, worked out from one server.
type scaleDraft struct {
	From         CloudItem          `json:"from"`
	Name         string             `json:"name"`
	Size         string             `json:"size"`
	DiskGB       int32              `json:"diskGb"`
	Each         *float64           `json:"each"`
	Subnets      []scaleDraftOption `json:"subnets"`
	TargetGroups []scaleDraftOption `json:"targetGroups"`
	Images       []scaleDraftOption `json:"images"`
	// TargetGroup is the one it is already in, if any.
	TargetGroup string            `json:"targetGroup"`
	Rules       cloud.ScaleRules  `json:"rules"`
	Limits      cloud.ScaleLimits `json:"limits"`
	Environment string            `json:"environment"`
	// Problem is why a group cannot be made from it, when it cannot.
	Problem string `json:"problem"`
}

type scaleDraftOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Zone  string `json:"zone,omitempty"`
	// Note is a short fact: the load balancer in front, or the zone.
	Note string `json:"note,omitempty"`
}

var reTrailingDigits = regexp.MustCompile(`-?\d+$`)

func (a *API) getScaleGroupDraft(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	acct, region, id := q.Get("accountId"), q.Get("region"), q.Get("from")
	items, err := a.store.CloudResources(ctx, cloud.KindInstance)
	if err != nil {
		a.fail(w, "scale group draft", err)
		return
	}
	var d scaleDraft
	found := false
	for _, it := range items {
		if it.AccountID == acct && it.Resource.Region == region && it.Resource.ID == id {
			d.From, found = it, true
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "no such server in that account and region")
		return
	}
	src := d.From.Resource
	d.Environment = cloudEnvOf(src.Tags, d.From.Environment)
	base := strings.ToLower(src.Name)
	if d.From.Server != nil {
		base = strings.ToLower(strings.Split(d.From.Server.Hostname, ".")[0])
	}
	d.Name = strings.Trim(reTrailingDigits.ReplaceAllString(base, ""), "-")
	if len(d.Name) > 32 || !reScaleName.MatchString(d.Name) {
		d.Name = ""
	}
	d.Size, d.DiskGB = src.Size, a.rootDiskGB(ctx, acct, region, src)
	if p, ok := monthly(src.Size); ok {
		d.Each = &p
	}
	d.Rules = cloud.DefaultScaleRules()
	d.Limits = cloud.ScaleLimits{Min: 2, Max: 4, AddWait: 10, RemoveWait: 30}
	if d.Each != nil {
		d.Limits.CostCap = float64(int(*d.Each*4/50+1)) * 50
	}
	if subs, err := a.store.CloudResources(ctx, cloud.KindSubnet); err == nil {
		for _, s := range subs {
			if s.AccountID == acct && s.Resource.Region == region && (src.Network == "" || s.Resource.Network == src.Network) {
				d.Subnets = append(d.Subnets, scaleDraftOption{Value: s.Resource.ID, Label: s.Resource.Name, Zone: s.Resource.Zone,
					Note: s.Resource.Size})
			}
		}
	}
	if tgs, err := a.store.CloudResources(ctx, cloud.KindTargetGroup); err == nil {
		for _, t := range tgs {
			r := t.Resource
			if t.AccountID != acct || r.Region != region || (src.Network != "" && r.Network != "" && r.Network != src.Network) {
				continue
			}
			if tt := r.Details["targetType"]; tt != "" && tt != "instance" {
				continue
			}
			d.TargetGroups = append(d.TargetGroups, scaleDraftOption{Value: r.ID, Label: r.Name, Note: lbOf(r)})
			for _, l := range r.Links {
				if l.Kind == cloud.KindInstance && l.ID == src.ID && d.TargetGroup == "" {
					d.TargetGroup = r.ID
				}
			}
		}
	}
	if imgs, err := a.store.CloudResources(ctx, cloud.KindImage); err == nil {
		for _, im := range imgs {
			if im.AccountID != acct || im.Resource.Region != region || im.Resource.State != "available" {
				continue
			}
			for _, l := range im.Resource.Links {
				if l.Kind == cloud.KindInstance && l.ID == src.ID {
					d.Images = append(d.Images, scaleDraftOption{Value: im.Resource.ID, Label: im.Resource.Name})
				}
			}
		}
	}
	switch {
	case src.State != "running":
		d.Problem = src.Name + " is " + src.State + "; a group is made from a running server."
	case len(d.TargetGroups) == 0:
		d.Problem = "There is no target group in " + src.Name + "'s network. Make a load balancer and target group under Cloud › Network first."
	}
	if g, _ := a.scaleGroupOf(ctx, acct, src.ID); g != nil {
		d.Problem = src.Name + " is already in the scale group " + g.Name + "."
	}
	writeJSON(w, http.StatusOK, d)
}

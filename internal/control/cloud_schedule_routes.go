package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 3: scheduled start and stop.

Who: administrators and owners make, change, pause and delete schedules;
auditors and approvers see them. A schedule that reaches Production (an
account in its scope, or an instance it matches) waits for an approver,
administrator or owner who did not ask, both when it is made and when it
is changed; the version in effect keeps running meanwhile. Keep running
needs no approval: it only ever leaves an instance as it is.

A schedule runs as its owner: the person whose version is in effect.
*/

func (a *API) registerCloudScheduleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/cloud/schedules", a.getCloudSchedules)
	mux.HandleFunc("POST /api/v1/cloud/schedules", a.postCloudSchedule)
	mux.HandleFunc("POST /api/v1/cloud/schedules/preview", a.postCloudSchedulePreview)
	mux.HandleFunc("PUT /api/v1/cloud/schedules/{id}", a.putCloudSchedule)
	mux.HandleFunc("DELETE /api/v1/cloud/schedules/{id}", a.deleteCloudSchedule)
	mux.HandleFunc("GET /api/v1/cloud/schedules/{id}/preview", a.getCloudSchedulePreview)
	mux.HandleFunc("GET /api/v1/cloud/schedules/{id}/runs", a.getCloudScheduleRuns)
	mux.HandleFunc("POST /api/v1/cloud/schedules/{id}/approve", a.postCloudScheduleDecision(true))
	mux.HandleFunc("POST /api/v1/cloud/schedules/{id}/decline", a.postCloudScheduleDecision(false))
	mux.HandleFunc("POST /api/v1/cloud/schedules/{id}/pause", a.postCloudSchedulePause)
	mux.HandleFunc("POST /api/v1/cloud/schedules/{id}/resume", a.postCloudScheduleResume)
	mux.HandleFunc("PUT /api/v1/cloud/instances/{account}/{region}/{id}/keep-running", a.putCloudKeepRunning)
	mux.HandleFunc("DELETE /api/v1/cloud/instances/{account}/{region}/{id}/keep-running", a.deleteCloudKeepRunning)
}

// cloudScheduleReader authenticates someone who may see schedules.
func (a *API) cloudScheduleReader(w http.ResponseWriter, r *http.Request) bool {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if !seesWholeFleet(sess.Role) && sess.Role != "approver" {
		writeErr(w, http.StatusForbidden, "schedules are for administrators, owners, auditors and approvers")
		return false
	}
	return true
}

// CloudSaving is what a schedule saves a month, at on-demand prices.
type CloudSaving struct {
	Monthly float64 `json:"monthly"`
	// Priced is how many matched instances the estimate knows the price
	// of; Unpriced, how many it does not.
	Priced   int `json:"priced"`
	Unpriced int `json:"unpriced"`
}

// CloudScheduleInstance is one instance a schedule matches.
type CloudScheduleInstance struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Region      string `json:"region"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Size        string `json:"size"`
	Environment string `json:"environment"`
	// Note is why the schedule would leave it alone now, if it would.
	Note string `json:"note,omitempty"`
}

// CloudScheduleView is a schedule with what it means now.
type CloudScheduleView struct {
	CloudSchedule
	Next          []cloud.Transition `json:"next"`
	Matched       int                `json:"matched"`
	Running       int                `json:"running"`
	HoursPerWeek  float64            `json:"hoursPerWeek"`
	Saving        CloudSaving        `json:"saving"`
	NeedsApproval bool               `json:"needsApproval"`
	LastRun       *CloudScheduleRun  `json:"lastRun,omitempty"`
}

// CloudSchedulePreview is what a version would do.
type CloudSchedulePreview struct {
	Instances     []CloudScheduleInstance `json:"instances"`
	Transitions   []cloud.Transition      `json:"transitions"`
	HoursPerWeek  float64                 `json:"hoursPerWeek"`
	Saving        CloudSaving             `json:"saving"`
	NeedsApproval bool                    `json:"needsApproval"`
	RunningNow    bool                    `json:"runningNow"`
}

// weeksPerMonth is 365.25 / 12 / 7.
const weeksPerMonth = 4.348

// matchSchedule lists what v matches and whether it reaches Production.
func matchSchedule(v CloudScheduleVersion, w scheduleWorld) ([]CloudScheduleInstance, bool) {
	out := []CloudScheduleInstance{}
	prod := false
	for _, c := range w.accounts {
		if c.Environment == "production" && (len(v.AccountIDs) == 0 || containsFold(v.AccountIDs, c.ID)) {
			prod = true
		}
	}
	for _, it := range w.instances {
		if !inScope(v, it) {
			continue
		}
		r := it.Resource
		acct := w.accounts[it.AccountID]
		env := cloudEnvOf(r.Tags, acct.Environment)
		if env == "production" {
			prod = true
		}
		key := it.AccountID + "/" + r.ID
		inst := CloudScheduleInstance{AccountID: it.AccountID, AccountName: it.AccountName, Region: r.Region, ID: r.ID,
			Name: r.Name, State: r.State, Size: r.Size, Environment: env}
		switch {
		case acct.ManageRoleARN == "":
			inst.Note = "its account is connected read only"
		case w.open[key]:
			inst.Note = "a change to it is waiting or running"
		default:
			if k, ok := w.keep[key]; ok {
				inst.Note = "kept running until " + k.Until.UTC().Format("Mon 2 Jan 15:04 MST") + " by " + k.SetBy
			} else if why := cloud.StopRefusal(r); why != "" {
				inst.Note = "never stopped: " + why
			}
		}
		out = append(out, inst)
	}
	return out, prod
}

func estimate(insts []CloudScheduleInstance, hoursPerWeek float64) CloudSaving {
	var s CloudSaving
	off := 168 - hoursPerWeek
	if off < 0 {
		off = 0
	}
	for _, in := range insts {
		if strings.HasPrefix(in.Note, "never stopped") {
			continue
		}
		p, ok := cloud.HourlyPrice(in.Size)
		if !ok {
			s.Unpriced++
			continue
		}
		s.Priced++
		s.Monthly += p * off * weeksPerMonth
	}
	s.Monthly = float64(int(s.Monthly*100+0.5)) / 100
	return s
}

func (a *API) viewSchedule(s CloudSchedule, w scheduleWorld, now time.Time) CloudScheduleView {
	v := CloudScheduleView{CloudSchedule: s, Next: []cloud.Transition{}}
	insts, prod := matchSchedule(s.CloudScheduleVersion, w)
	v.Matched, v.NeedsApproval = len(insts), prod
	for _, in := range insts {
		if in.State == "running" || in.State == "pending" {
			v.Running++
		}
	}
	if plan, err := s.Plan(); err == nil {
		if ts := plan.Transitions(now, now.AddDate(0, 0, 8)); len(ts) > 0 {
			v.Next = ts[:min(len(ts), 4)]
		}
		v.HoursPerWeek = plan.HoursPerWeek(now)
		v.Saving = estimate(insts, v.HoursPerWeek)
	}
	return v
}

func (a *API) getCloudSchedules(w http.ResponseWriter, r *http.Request) {
	if !a.cloudScheduleReader(w, r) {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	list, err := a.store.CloudSchedules(ctx)
	if err != nil {
		a.fail(w, "cloud schedules", err)
		return
	}
	world, err := a.readScheduleWorld(ctx, now)
	if err != nil {
		a.fail(w, "cloud schedules", err)
		return
	}
	out := make([]CloudScheduleView, 0, len(list))
	for _, s := range list {
		v := a.viewSchedule(s, world, now)
		if runs, err := a.store.CloudScheduleRuns(ctx, s.ID, 1); err == nil && len(runs) > 0 {
			v.LastRun = &runs[0]
		}
		out = append(out, v)
	}
	keep := make([]CloudKeepRunning, 0, len(world.keep))
	for _, k := range world.keep {
		keep = append(keep, k)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].Until.Before(keep[j].Until) })
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out, "keepRunning": keep})
}

var (
	reScheduleName = regexp.MustCompile(`^[A-Za-z0-9-]{1,63}$`)
	reRegion       = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)
)

type cloudScheduleIn struct {
	CloudScheduleVersion
	Reason string `json:"reason"`
}

// checkVersion cleans up a version and says what is wrong with it.
func (a *API) checkVersion(ctx context.Context, v *CloudScheduleVersion) error {
	v.Name = strings.TrimSpace(v.Name)
	if !reScheduleName.MatchString(v.Name) {
		return errors.New("the name is what instances are tagged with: letters, digits and dashes, up to 63")
	}
	if v.Mode == "" {
		v.Mode = "transitions"
	}
	if v.Mode != "transitions" && v.Mode != "enforce" {
		return errors.New("the mode is transitions or enforce")
	}
	plan, err := v.Plan()
	if err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	slices.Sort(v.SkipDates)
	v.SkipDates = slices.Compact(v.SkipDates)
	if len(v.Regions) > 30 {
		return errors.New("at most 30 regions")
	}
	for _, rg := range v.Regions {
		if !reRegion.MatchString(rg) {
			return fmt.Errorf("%q is not a region like ap-southeast-3", rg)
		}
	}
	if len(v.AccountIDs) > 0 {
		accts, err := a.store.CloudAccounts(ctx)
		if err != nil {
			return err
		}
		for _, id := range v.AccountIDs {
			if !slices.ContainsFunc(accts, func(c CloudAccount) bool { return c.ID == id }) {
				return errors.New("pick accounts that are connected")
			}
		}
	}
	*v = v.norm()
	return nil
}

func (a *API) previewVersion(ctx context.Context, v CloudScheduleVersion) (CloudSchedulePreview, error) {
	now := time.Now().UTC()
	world, err := a.readScheduleWorld(ctx, now)
	if err != nil {
		return CloudSchedulePreview{}, err
	}
	insts, prod := matchSchedule(v, world)
	p := CloudSchedulePreview{Instances: insts, Transitions: []cloud.Transition{}, NeedsApproval: prod}
	if plan, err := v.Plan(); err == nil && plan.Validate() == nil {
		p.Transitions = append(p.Transitions, plan.Transitions(now, now.AddDate(0, 0, 14))...)
		p.HoursPerWeek = plan.HoursPerWeek(now)
		p.RunningNow = plan.Running(now)
		p.Saving = estimate(insts, p.HoursPerWeek)
	}
	return p, nil
}

func (a *API) postCloudSchedulePreview(w http.ResponseWriter, r *http.Request) {
	if !a.cloudScheduleReader(w, r) {
		return
	}
	var in cloudScheduleIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := a.previewVersion(r.Context(), in.CloudScheduleVersion.norm())
	if err != nil {
		a.fail(w, "schedule preview", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) getCloudSchedulePreview(w http.ResponseWriter, r *http.Request) {
	if !a.cloudScheduleReader(w, r) {
		return
	}
	s, ok := a.scheduleOr404(w, r)
	if !ok {
		return
	}
	p, err := a.previewVersion(r.Context(), s.CloudScheduleVersion)
	if err != nil {
		a.fail(w, "schedule preview", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, p)
}

func (a *API) scheduleOr404(w http.ResponseWriter, r *http.Request) (CloudSchedule, bool) {
	s, err := a.store.CloudSchedule(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrCloudScheduleNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return s, false
	}
	if err != nil {
		a.fail(w, "cloud schedule", err)
		return s, false
	}
	return s, true
}

func describeVersion(v CloudScheduleVersion) string {
	days := []string{"", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	var ws []string
	for _, x := range v.Windows {
		var ds []string
		for _, d := range x.Days {
			if d >= 1 && d <= 7 {
				ds = append(ds, days[d])
			}
		}
		s := strings.Join(ds, ",") + " " + x.Start + "-"
		if x.StopDay >= 1 && x.StopDay <= 7 {
			s += days[x.StopDay] + " "
		}
		ws = append(ws, s+x.Stop)
	}
	out := v.Timezone + ", " + strings.Join(ws, "; ") + ", mode " + v.Mode
	if len(v.SkipDates) > 0 {
		out += fmt.Sprintf(", %d skip date%s", len(v.SkipDates), plural(len(v.SkipDates)))
	}
	if len(v.AccountIDs) > 0 {
		out += fmt.Sprintf(", %d account%s", len(v.AccountIDs), plural(len(v.AccountIDs)))
	}
	if len(v.Regions) > 0 {
		out += ", regions " + strings.Join(v.Regions, ", ")
	}
	return out
}

func (a *API) postCloudSchedule(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "making a schedule")
	if !ok {
		return
	}
	var in cloudScheduleIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.checkVersion(r.Context(), &in.CloudScheduleVersion); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > 500 {
		writeErr(w, http.StatusBadRequest, "keep the reason under 500 characters")
		return
	}
	p, err := a.previewVersion(r.Context(), in.CloudScheduleVersion)
	if err != nil {
		a.fail(w, "cloud schedule", err)
		return
	}
	state := "active"
	if p.NeedsApproval {
		state = "awaiting_approval"
	}
	s, err := a.store.CreateCloudSchedule(r.Context(), in.CloudScheduleVersion, actor, state, in.Reason)
	if errors.Is(err, ErrCloudScheduleExists) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud schedule", err)
		return
	}
	detail := "New schedule " + s.Name + ": " + describeVersion(s.CloudScheduleVersion) +
		fmt.Sprintf("; matches %d instance%s now", len(p.Instances), plural(len(p.Instances)))
	if in.Reason != "" {
		detail += ". Reason: " + in.Reason
	}
	if p.NeedsApproval {
		a.auditSchedule(r.Context(), "requested", actor, s.Name, detail+". Waits for approval (production).")
		a.askScheduleDecision(r.Context(), s, actor, "made")
		writeJSON(w, http.StatusAccepted, s)
		return
	}
	a.auditSchedule(r.Context(), "applied", actor, s.Name, detail+".")
	writeJSON(w, http.StatusCreated, s)
}

func (a *API) askScheduleDecision(ctx context.Context, s CloudSchedule, actor, verb string) {
	a.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Severity: "info", Audience: AudienceDeciders, Actor: actor,
		Title:  actor + " " + verb + " schedule " + s.Name,
		Detail: "It starts and stops Production instances, so someone other than " + actor + " approves it.",
		Link:   "/cloud?tab=schedules", Subject: "schedule:" + s.ID})
}

func (a *API) putCloudSchedule(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "changing a schedule")
	if !ok {
		return
	}
	cur, ok := a.scheduleOr404(w, r)
	if !ok {
		return
	}
	var in cloudScheduleIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.checkVersion(r.Context(), &in.CloudScheduleVersion); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) > 500 {
		writeErr(w, http.StatusBadRequest, "keep the reason under 500 characters")
		return
	}
	p, err := a.previewVersion(r.Context(), in.CloudScheduleVersion)
	if err != nil {
		a.fail(w, "cloud schedule", err)
		return
	}
	// The version in effect reaching Production counts too: narrowing a
	// Production schedule is a change to it.
	old, err := a.previewVersion(r.Context(), cur.CloudScheduleVersion)
	if err != nil {
		a.fail(w, "cloud schedule", err)
		return
	}
	waits := p.NeedsApproval || old.NeedsApproval
	detail := "Change " + cur.Name + " to " + describeVersion(in.CloudScheduleVersion)
	if in.Reason != "" {
		detail += ". Reason: " + in.Reason
	}
	var s CloudSchedule
	switch {
	case cur.State == "awaiting_approval" && strings.EqualFold(cur.UpdatedBy, actor):
		// Still waiting: the person who asked reworks their own request.
		s, err = a.store.UpdateCloudSchedule(r.Context(), cur.ID, in.CloudScheduleVersion, actor, in.Reason)
	case cur.State == "awaiting_approval":
		writeErr(w, http.StatusConflict, "this schedule waits for a decision on what "+cur.UpdatedBy+" asked; decide it first")
		return
	case waits:
		s, err = a.store.SetCloudSchedulePending(r.Context(), cur.ID, in.CloudScheduleVersion, actor, in.Reason)
	default:
		s, err = a.store.UpdateCloudSchedule(r.Context(), cur.ID, in.CloudScheduleVersion, actor, in.Reason)
	}
	switch {
	case errors.Is(err, ErrCloudScheduleExists), errors.Is(err, ErrCloudSchedulePending):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		a.fail(w, "cloud schedule", err)
		return
	}
	if s.Waiting() {
		a.auditSchedule(r.Context(), "requested", actor, cur.Name, detail+". Waits for approval (production); the version in effect keeps running.")
		a.askScheduleDecision(r.Context(), s, actor, "changed")
		writeJSON(w, http.StatusAccepted, s)
		return
	}
	a.auditSchedule(r.Context(), "applied", actor, s.Name, detail+".")
	writeJSON(w, http.StatusOK, s)
}

func (a *API) postCloudScheduleDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !a.privileged(w, sess, "deciding a schedule", "approver", "admin", "owner") {
			return
		}
		cur, ok := a.scheduleOr404(w, r)
		if !ok {
			return
		}
		if !cur.Waiting() {
			writeErr(w, http.StatusConflict, ErrCloudScheduleSettled.Error())
			return
		}
		asker := cur.Requester()
		if strings.EqualFold(asker, sess.Email) {
			writeErr(w, http.StatusForbidden, "someone other than the person who asked decides a schedule")
			return
		}
		s, kept, err := a.store.DecideCloudSchedule(r.Context(), cur.ID, approve)
		switch {
		case errors.Is(err, ErrCloudScheduleSettled), errors.Is(err, ErrCloudScheduleExists):
			writeErr(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			a.fail(w, "cloud schedule", err)
			return
		}
		verb := "Declined"
		if approve {
			verb = "Approved"
		}
		a.closeAlerts(r.Context(), "schedule:"+cur.ID, verb+" by "+sess.Email)
		if !approve {
			what := "the change to " + cur.Name
			if !kept {
				what = "the new schedule " + cur.Name + ", which is removed"
			}
			a.auditSchedule(r.Context(), "declined", sess.Email, cur.Name, "Declined "+what+", asked by "+asker+".")
			writeJSON(w, http.StatusOK, map[string]any{"schedule": s, "kept": kept})
			return
		}
		a.auditSchedule(r.Context(), "approved", sess.Email, s.Name, "Approved "+s.Name+", asked by "+asker+".")
		a.auditSchedule(r.Context(), "applied", asker, s.Name, s.Name+" is in effect: "+describeVersion(s.CloudScheduleVersion)+"; it runs as "+s.Owner+".")
		writeJSON(w, http.StatusOK, map[string]any{"schedule": s, "kept": true})
	}
}

func (a *API) postCloudSchedulePause(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "pausing a schedule")
	if !ok {
		return
	}
	cur, ok := a.scheduleOr404(w, r)
	if !ok {
		return
	}
	s, err := a.store.PauseCloudSchedule(r.Context(), cur.ID, "paused by "+actor)
	if errors.Is(err, ErrCloudScheduleSettled) {
		writeErr(w, http.StatusConflict, "only an active schedule can be paused")
		return
	}
	if err != nil {
		a.fail(w, "cloud schedule", err)
		return
	}
	a.auditSchedule(r.Context(), "paused", actor, s.Name, "Paused "+s.Name+"; its instances stay as they are.")
	writeJSON(w, http.StatusOK, s)
}

func (a *API) postCloudScheduleResume(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "resuming a schedule")
	if !ok {
		return
	}
	cur, ok := a.scheduleOr404(w, r)
	if !ok {
		return
	}
	s, err := a.store.ResumeCloudSchedule(r.Context(), cur.ID, actor)
	if errors.Is(err, ErrCloudScheduleSettled) {
		writeErr(w, http.StatusConflict, "only a paused schedule can be resumed")
		return
	}
	if err != nil {
		a.fail(w, "cloud schedule", err)
		return
	}
	a.closeAlerts(r.Context(), "schedule:"+cur.ID, "Resumed by "+actor)
	a.auditSchedule(r.Context(), "resumed", actor, s.Name, "Resumed "+s.Name+"; it runs as "+actor+" from now on, from the next transition.")
	writeJSON(w, http.StatusOK, s)
}

func (a *API) deleteCloudSchedule(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "deleting a schedule")
	if !ok {
		return
	}
	cur, ok := a.scheduleOr404(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteCloudSchedule(r.Context(), cur.ID); err != nil {
		a.fail(w, "cloud schedule", err)
		return
	}
	a.closeAlerts(r.Context(), "schedule:"+cur.ID, "Deleted by "+actor)
	a.auditSchedule(r.Context(), "deleted", actor, cur.Name, "Deleted "+cur.Name+"; instances still tagged "+
		cloud.ScheduleTag+"="+cur.Name+" stay as they are.")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) getCloudScheduleRuns(w http.ResponseWriter, r *http.Request) {
	if !a.cloudScheduleReader(w, r) {
		return
	}
	s, ok := a.scheduleOr404(w, r)
	if !ok {
		return
	}
	runs, err := a.store.CloudScheduleRuns(r.Context(), s.ID, 50)
	if err != nil {
		a.fail(w, "schedule runs", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// cloudKeepMax is the longest an instance can be kept running at once.
const cloudKeepMax = 7 * 24 * time.Hour

type keepRunningIn struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

func (a *API) putCloudKeepRunning(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "keeping an instance running")
	if !ok {
		return
	}
	var in keepRunningIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	in.Reason = strings.TrimSpace(in.Reason)
	switch {
	case !in.Until.After(now):
		writeErr(w, http.StatusBadRequest, "pick a time in the future")
		return
	case in.Until.Sub(now) > cloudKeepMax+time.Minute:
		writeErr(w, http.StatusBadRequest, "keep it running for at most 7 days at a time")
		return
	case in.Reason == "":
		writeErr(w, http.StatusBadRequest, "say why, so whoever wonders why it is still running knows")
		return
	case len(in.Reason) > 300:
		writeErr(w, http.StatusBadRequest, "keep the reason under 300 characters")
		return
	}
	acct, region, id := r.PathValue("account"), r.PathValue("region"), r.PathValue("id")
	inst, err := a.store.CloudInstance(r.Context(), acct, region, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	k := CloudKeepRunning{AccountID: acct, InstanceID: inst.ID, Until: in.Until.UTC(), Reason: in.Reason, SetBy: actor}
	if err := a.store.SetCloudKeepRunning(r.Context(), k); err != nil {
		a.fail(w, "keep running", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.keep_running.set", Severity: "notice", ActorEmail: actor,
		Target: "aws " + region + " " + inst.Name + " (" + inst.ID + ")",
		Detail: "Schedules leave " + inst.Name + " running until " + k.Until.Format("Mon 2 Jan 15:04 MST") + ". Reason: " + in.Reason + "."})
	k.SetAt = now
	writeJSON(w, http.StatusOK, k)
}

func (a *API) deleteCloudKeepRunning(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "keeping an instance running")
	if !ok {
		return
	}
	acct, region, id := r.PathValue("account"), r.PathValue("region"), r.PathValue("id")
	k, found, err := a.store.ClearCloudKeepRunning(r.Context(), acct, id)
	if err != nil {
		a.fail(w, "keep running", err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "this instance is not being kept running")
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.keep_running.cleared", Severity: "notice", ActorEmail: actor,
		Target: "aws " + region + " " + id,
		Detail: "Schedules act on " + id + " again (it was kept running until " + k.Until.Format("Mon 2 Jan 15:04 MST") + " by " + k.SetBy + ")."})
	w.WriteHeader(http.StatusNoContent)
}

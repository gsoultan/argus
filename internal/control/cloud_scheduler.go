package control

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
The schedule loop. Every minute, for every organization:

  - each active schedule's starts and stops since it last looked are
    claimed (one row per transition; the first control host to insert it
    runs it) and run against the instances tagged with its name;
  - a transition missed by more than two hours (the control plane was
    down) is not made up: it is recorded as skipped and admins are told;
  - in enforce mode, every fifteen minutes, instances in the wrong state
    are put right, which catches someone starting one by hand at night;
  - runs waiting on something are looked at again: an instance with a live
    session is stopped once the session ends, or after an hour with a
    notice to the person fifteen minutes before; a failed call is tried
    once more after two minutes.

Every call runs as the schedule's owner, so CloudTrail names a person. An
owner who is no longer an administrator or owner pauses the schedule.
*/

const (
	cloudScheduleCatchUp = 2 * time.Hour
	cloudScheduleRetry   = 2 * time.Minute
	cloudSessionGrace    = time.Hour
	cloudSessionNotice   = 15 * time.Minute
	cloudEnforceEvery    = 15 * time.Minute
)

// RunCloudSchedules runs every organization's schedules each interval until
// ctx ends.
func (a *API) RunCloudSchedules(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			a.TickCloudSchedules(tctx, time.Now().UTC())
		})
		if err != nil {
			a.log.Error("cloud schedules", "error", err)
		}
	}
}

// TickCloudSchedules does one minute's work for the organization on ctx.
func (a *API) TickCloudSchedules(ctx context.Context, now time.Time) {
	list, err := a.store.CloudSchedules(ctx)
	if err != nil {
		a.log.Error("cloud schedules: cannot list", "error", err)
		return
	}
	for _, s := range list {
		if s.State == "active" {
			a.tickSchedule(ctx, s, now)
		}
	}
	runs, err := a.store.TakeDueCloudScheduleRuns(ctx, now, 20)
	if err != nil {
		a.log.Error("cloud schedules: cannot read waiting runs", "error", err)
		return
	}
	for _, r := range runs {
		a.continueRun(ctx, r, now)
	}
}

func (a *API) tickSchedule(ctx context.Context, s CloudSchedule, now time.Time) {
	plan, err := s.Plan()
	if err != nil {
		a.pauseSchedule(ctx, s, "its time zone "+s.Timezone+" is not known on this control host")
		return
	}
	role, err := a.store.UserRole(ctx, s.Owner)
	if err != nil {
		a.log.Error("cloud schedules: cannot read the owner's role", "schedule", s.Name, "error", err)
		return
	}
	if role != "admin" && role != "owner" {
		a.pauseSchedule(ctx, s, s.Owner+", who runs it, is no longer an administrator or owner; resume it to run it as yourself")
		return
	}
	from := now.Add(-time.Minute)
	if s.CheckedAt != nil {
		from = *s.CheckedAt
	}
	if !now.After(from) {
		return
	}
	if edge := now.Add(-cloudScheduleCatchUp); from.Before(edge) {
		missed := plan.Transitions(from, edge)
		if n := len(missed); n > 0 {
			a.recordMissed(ctx, s, missed[n-1], n)
		}
		from = edge
	}
	for _, tr := range plan.Transitions(from, now) {
		run, ok, err := a.store.ClaimCloudScheduleRun(ctx, s.ID, tr.At, tr.Action)
		if err != nil {
			a.log.Error("cloud schedules: cannot claim a run", "schedule", s.Name, "error", err)
			return
		}
		if ok {
			a.startRun(ctx, s, run, now)
		}
	}
	if s.Mode == "enforce" {
		slot := now.Truncate(cloudEnforceEvery)
		if slot.After(from) {
			act := cloud.ActionStop
			if plan.Running(slot) {
				act = cloud.ActionStart
			}
			run, ok, err := a.store.ClaimCloudScheduleRun(ctx, s.ID, slot, act)
			if err == nil && ok {
				a.startRun(ctx, s, run, now)
			}
		}
	}
	if err := a.store.SetCloudScheduleChecked(ctx, s.ID, now); err != nil {
		a.log.Error("cloud schedules: cannot record the check", "schedule", s.Name, "error", err)
	}
}

func (a *API) pauseSchedule(ctx context.Context, s CloudSchedule, why string) {
	if _, err := a.store.PauseCloudSchedule(ctx, s.ID, why); err != nil {
		a.log.Error("cloud schedules: cannot pause", "schedule", s.Name, "error", err)
		return
	}
	a.auditSchedule(ctx, "paused", "argus", s.Name, "Paused "+s.Name+": "+why+".")
	a.raiseAlert(ctx, ConsoleAlert{Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
		Title: "Schedule " + s.Name + " is paused", Detail: "Its instances are no longer started and stopped: " + why + ".",
		Link: "/cloud?tab=schedules", Subject: "schedule:" + s.ID})
}

// recordMissed notes the latest transition the loop slept through, without
// acting on it, and tells admins.
func (a *API) recordMissed(ctx context.Context, s CloudSchedule, tr cloud.Transition, n int) {
	run, ok, err := a.store.ClaimCloudScheduleRun(ctx, s.ID, tr.At, tr.Action)
	if err != nil || !ok {
		return
	}
	verb := "start"
	if tr.Action == cloud.ActionStop {
		verb = "stop"
	}
	run.State = "skipped"
	run.Detail = fmt.Sprintf("Missed: the %s due %s came more than two hours ago, while Argus was not running schedules, so it was not made up", verb, tr.At.UTC().Format("Mon 2 Jan 15:04 MST"))
	if n > 1 {
		run.Detail += fmt.Sprintf(" (%d transitions were missed; this was the last)", n)
	}
	done := time.Now().UTC()
	run.FinishedAt = &done
	if err := a.store.SaveCloudScheduleRun(ctx, run); err != nil {
		a.log.Error("cloud schedules: cannot record a missed run", "schedule", s.Name, "error", err)
	}
	a.auditSchedule(ctx, "run", s.Owner, s.Name, run.Detail+".")
	a.raiseAlert(ctx, ConsoleAlert{Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
		Title: "Schedule " + s.Name + " missed a " + verb, Detail: run.Detail + ". Its instances may be in the wrong state until the next transition.",
		Link: "/cloud?tab=schedules"})
}

// scheduleWorld is what deciding a run's targets needs, read once.
type scheduleWorld struct {
	instances []CloudItem
	accounts  map[string]CloudAccount
	keep      map[string]CloudKeepRunning
	open      map[string]bool
	// sessions is who has a live session on the server each instance
	// matches, by account and instance.
	sessions map[string][]string
}

func (a *API) readScheduleWorld(ctx context.Context, now time.Time) (scheduleWorld, error) {
	var w scheduleWorld
	var err error
	if w.instances, err = a.store.CloudResources(ctx, cloud.KindInstance); err != nil {
		return w, err
	}
	accts, err := a.store.CloudAccounts(ctx)
	if err != nil {
		return w, err
	}
	w.accounts = map[string]CloudAccount{}
	for _, c := range accts {
		w.accounts[c.ID] = c
	}
	if w.keep, err = a.store.CloudKeepRunningNow(ctx, now); err != nil {
		return w, err
	}
	if w.open, err = a.store.cloudOpenChanges(ctx); err != nil {
		return w, err
	}
	live, err := a.store.liveSessionUsers(ctx)
	if err != nil {
		return w, err
	}
	w.sessions = map[string][]string{}
	if len(live) > 0 {
		assets, err := a.store.Assets(ctx)
		if err != nil {
			return w, err
		}
		matched := append([]CloudItem(nil), w.instances...)
		matchServers(matched, assets)
		for _, it := range matched {
			if it.Server != nil && len(live[it.Server.ID]) > 0 {
				w.sessions[it.AccountID+"/"+it.Resource.ID] = live[it.Server.ID]
			}
		}
	}
	return w, nil
}

// inScope reports whether an instance is on schedule s.
func inScope(s CloudScheduleVersion, it CloudItem) bool {
	if !strings.EqualFold(cloud.ScheduleOf(it.Resource.Tags), s.Name) {
		return false
	}
	if len(s.AccountIDs) > 0 && !containsFold(s.AccountIDs, it.AccountID) {
		return false
	}
	if len(s.Regions) > 0 && !containsFold(s.Regions, it.Resource.Region) {
		return false
	}
	return true
}

func containsFold(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

// planTargets decides, for each instance on the schedule, whether the run
// acts on it now, waits, or skips it and why. Instances already where the
// run would put them are left out.
func planTargets(s CloudSchedule, act cloud.Action, w scheduleWorld, due, now time.Time) []CloudRunTarget {
	var out []CloudRunTarget
	for _, it := range w.instances {
		if !inScope(s.CloudScheduleVersion, it) {
			continue
		}
		r := it.Resource
		state := r.State
		if act == cloud.ActionStart && (state == "running" || state == "pending") {
			continue
		}
		if act == cloud.ActionStop && (state == "stopped" || state == "stopping") {
			continue
		}
		if state == "terminated" || state == "shutting-down" {
			continue
		}
		t := CloudRunTarget{AccountID: it.AccountID, AccountName: it.AccountName, Region: r.Region, ID: r.ID, Name: r.Name}
		key := it.AccountID + "/" + r.ID
		acct := w.accounts[it.AccountID]
		switch {
		case acct.ManageRoleARN == "":
			t.Status, t.Reason = "skipped", "its account is connected read only"
		case w.open[key]:
			t.Status, t.Reason = "skipped", "a change to it is waiting or running"
		case act == cloud.ActionStart && state != "stopped":
			t.Status, t.Reason = "skipped", "it is "+state
		case act == cloud.ActionStop:
			if k, ok := w.keep[key]; ok {
				t.Status, t.Reason = "skipped", "kept running until "+k.Until.UTC().Format("Mon 2 Jan 15:04 MST")+" by "+k.SetBy
			} else if why := cloud.StopRefusal(r); why != "" {
				t.Status, t.Reason = "skipped", why
			} else if users := w.sessions[key]; len(users) > 0 {
				until := due.Add(cloudSessionGrace)
				t.Status, t.Reason, t.WaitUntil = "waiting", "a session is open on it ("+strings.Join(users, ", ")+")", &until
				if !now.Before(until) {
					t.Status, t.Reason, t.WaitUntil = "", "", nil
				}
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AccountName != out[j].AccountName {
			return out[i].AccountName < out[j].AccountName
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (a *API) startRun(ctx context.Context, s CloudSchedule, run CloudScheduleRun, now time.Time) {
	w, err := a.readScheduleWorld(ctx, now)
	if err != nil {
		a.log.Error("cloud schedules: cannot read the inventory", "schedule", s.Name, "error", err)
		retry := now.Add(cloudScheduleRetry)
		run.NextAt = &retry
		_ = a.store.SaveCloudScheduleRun(ctx, run)
		return
	}
	run.Targets = planTargets(s, run.Action, w, run.DueAt, now)
	run.Attempts = 1
	a.act(ctx, s, &run, w, func(t CloudRunTarget) bool { return t.Status == "" })
	a.settleRun(ctx, s, run, now)
}

// continueRun takes up a run with something left: instances waiting for a
// session, or a failed call to try again.
func (a *API) continueRun(ctx context.Context, run CloudScheduleRun, now time.Time) {
	s, err := a.store.CloudSchedule(ctx, run.ScheduleID)
	if err != nil {
		return
	}
	if s.State != "active" {
		for i := range run.Targets {
			if t := &run.Targets[i]; t.Status == "waiting" || (t.Status == "failed" && t.Tries < 2) {
				t.Status, t.Reason, t.WaitUntil = "skipped", "the schedule was paused", nil
			}
		}
		a.settleRun(ctx, s, run, now)
		return
	}
	if run.Attempts == 0 {
		// The inventory could not be read when it was due: start over,
		// unless it is now as late as a missed run.
		if now.Sub(run.DueAt) > cloudScheduleCatchUp {
			run.Attempts, run.State, run.NextAt, run.FinishedAt = 1, "skipped", nil, &now
			run.Detail = "Skipped: Argus could not read the inventory for two hours after it was due"
			_ = a.store.SaveCloudScheduleRun(ctx, run)
			a.auditSchedule(ctx, "run", s.Owner, s.Name, s.Name+": "+run.Detail+".")
			return
		}
		a.startRun(ctx, s, run, now)
		return
	}
	w, err := a.readScheduleWorld(ctx, now)
	if err != nil {
		retry := now.Add(cloudScheduleRetry)
		run.NextAt = &retry
		_ = a.store.SaveCloudScheduleRun(ctx, run)
		return
	}
	retrying := false
	for i := range run.Targets {
		t := &run.Targets[i]
		key := t.AccountID + "/" + t.ID
		switch {
		case t.Status == "failed" && t.Tries < 2:
			t.Status, t.Reason = "", ""
			retrying = true
		case t.Status == "waiting":
			if k, ok := w.keep[key]; ok {
				t.Status, t.Reason, t.WaitUntil = "skipped", "kept running until "+k.Until.UTC().Format("Mon 2 Jan 15:04 MST")+" by "+k.SetBy, nil
				continue
			}
			users := w.sessions[key]
			switch {
			case len(users) == 0:
				t.Status, t.Reason, t.WaitUntil = "", "", nil
			case t.WaitUntil != nil && !now.Before(*t.WaitUntil):
				t.Status, t.Reason, t.WaitUntil = "", "", nil
			case t.WaitUntil != nil && !t.Noticed && !now.Before(t.WaitUntil.Add(-cloudSessionNotice)):
				t.Noticed = true
				for _, u := range users {
					a.raiseAlert(ctx, ConsoleAlert{Kind: AlertSession, Severity: "warning", Audience: AudiencePerson, Recipient: u,
						Title: t.Name + " stops at " + t.WaitUntil.UTC().Format("15:04 MST"),
						Detail: "Schedule " + s.Name + " stops " + t.Name + " in " + fmt.Sprint(int(t.WaitUntil.Sub(now).Round(time.Minute).Minutes())) +
							" minutes, with your session open on it. Save your work, or ask an administrator to keep it running.",
						Link: "/cloud?tab=instances"})
				}
			}
		}
	}
	if retrying {
		run.Attempts++
	}
	a.act(ctx, s, &run, w, func(t CloudRunTarget) bool { return t.Status == "" })
	a.settleRun(ctx, s, run, now)
}

// act calls the provider for the targets pick selects, batched by account
// and region, as the schedule's owner.
func (a *API) act(ctx context.Context, s CloudSchedule, run *CloudScheduleRun, w scheduleWorld, pick func(CloudRunTarget) bool) {
	groups := map[string][]int{}
	var keys []string
	for i, t := range run.Targets {
		if !pick(t) {
			continue
		}
		k := t.AccountID + "|" + t.Region
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], i)
	}
	actor := cloud.Actor(s.Owner)
	touched := map[string]CloudAccount{}
	for _, k := range keys {
		idx := groups[k]
		first := run.Targets[idx[0]]
		m, _, err := a.cloudManagedAccount(ctx, first.AccountID)
		if err != nil {
			for _, i := range idx {
				run.Targets[i].Status, run.Targets[i].Reason = "failed", err.Error()
				run.Targets[i].Tries++
			}
			continue
		}
		touched[m.acct.ID] = m.acct
		for start := 0; start < len(idx); start += cloud.MaxBulkPower {
			chunk := idx[start:min(start+cloud.MaxBulkPower, len(idx))]
			ctx, cancel := context.WithTimeout(ctx, cloudApplyTimeout)
			if bulk, ok := m.manager.(cloud.BulkPower); ok {
				ids := make([]string, len(chunk))
				for n, i := range chunk {
					ids[n] = run.Targets[i].ID
				}
				res, err := bulk.PowerMany(ctx, m.target, actor, first.Region, ids, run.Action)
				if err == nil {
					for _, i := range chunk {
						run.Targets[i].Status, run.Targets[i].RequestID = "done", res.RequestID
						run.Targets[i].Tries++
					}
					if res.RequestID != "" {
						run.RequestIDs = append(run.RequestIDs, res.RequestID)
					}
					cancel()
					continue
				}
				// One bad instance fails the whole call: try them one by
				// one, so each records its own outcome.
			}
			for _, i := range chunk {
				t := &run.Targets[i]
				t.Tries++
				res, err := m.manager.Power(ctx, m.target, actor, t.Region, t.ID, run.Action)
				if err != nil {
					t.Status, t.Reason = "failed", err.Error()
					continue
				}
				t.Status, t.RequestID, t.Reason = "done", res.RequestID, ""
				if res.RequestID != "" {
					run.RequestIDs = append(run.RequestIDs, res.RequestID)
				}
			}
			cancel()
		}
	}
	for _, c := range touched {
		a.syncCloudAccountSoon(ctx, c)
	}
}

// settleRun works out where a run stands, when to look at it again, and,
// once nothing is left, records it in the audit chain.
func (a *API) settleRun(ctx context.Context, s CloudSchedule, run CloudScheduleRun, now time.Time) {
	var done, failed, skipped, waiting, retry int
	var next *time.Time
	soon := func(t time.Time) {
		if next == nil || t.Before(*next) {
			next = &t
		}
	}
	for _, t := range run.Targets {
		switch t.Status {
		case "done":
			done++
		case "failed":
			failed++
			if t.Tries < 2 {
				retry++
				soon(now.Add(cloudScheduleRetry))
			}
		case "skipped":
			skipped++
		case "waiting":
			waiting++
			soon(now.Add(time.Minute))
		}
	}
	run.NextAt = next
	verb := map[cloud.Action]string{cloud.ActionStart: "Started", cloud.ActionStop: "Stopped"}[run.Action]
	parts := []string{fmt.Sprintf("%s %d of %d instance%s", verb, done, len(run.Targets), plural(len(run.Targets)))}
	if len(run.Targets) == 0 {
		parts = []string{"No instance needed it"}
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting for a session to end", waiting))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	if failed > 0 {
		if retry > 0 {
			parts = append(parts, fmt.Sprintf("%d failed, tried again in two minutes", failed))
		} else {
			parts = append(parts, fmt.Sprintf("%d failed", failed))
		}
	}
	run.Detail = strings.Join(parts, "; ")
	switch {
	case next != nil:
		run.State = "running"
	case failed > 0 && done == 0:
		run.State = "failed"
	case failed > 0:
		run.State = "partial"
	default:
		run.State = "done"
	}
	if next == nil {
		t := now
		run.FinishedAt = &t
	}
	if err := a.store.SaveCloudScheduleRun(ctx, run); err != nil {
		a.log.Error("cloud schedules: cannot record a run", "schedule", s.Name, "error", err)
	}
	if next != nil {
		return
	}
	detail := fmt.Sprintf("%s, due %s, as %s: %s", s.Name, run.DueAt.UTC().Format("Mon 2 Jan 15:04 MST"), s.Owner, run.Detail)
	for _, t := range run.Targets {
		if t.Status == "failed" || t.Status == "skipped" {
			detail += "; " + t.Name + " (" + t.ID + ") " + t.Status + ": " + t.Reason
		}
	}
	if len(run.RequestIDs) > 0 {
		detail += "; AWS requests " + strings.Join(run.RequestIDs, ", ")
	}
	a.auditSchedule(ctx, "run", s.Owner, s.Name, clip(detail, 3000)+".")
	if failed > 0 {
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
			Title:  fmt.Sprintf("Schedule %s: %d instance%s did not %s", s.Name, failed, plural(failed), run.Action),
			Detail: run.Detail + ".", Link: "/cloud?tab=schedules"})
	}
}

// auditSchedule records cloud.schedule.<what>.
func (a *API) auditSchedule(ctx context.Context, what, actor, name, detail string) {
	sev := "notice"
	if what == "paused" || what == "deleted" {
		sev = "warn"
	}
	if _, err := a.store.AppendAudit(ctx, AuditEvent{Action: "cloud.schedule." + what, Severity: sev, ActorEmail: actor,
		Target: "schedule " + name, Detail: detail}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "cloud.schedule."+what)
	}
}

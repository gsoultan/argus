package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
The scaler. Every minute, for every organization:

  - Argus's own questions nobody answered in 30 minutes lapse, and the group
    says so ("Asked at 14:02, nobody answered");
  - each group's servers are looked after: a new one goes in the target
    group once it runs and becomes a member when its health check passes;
    one that does not within 15 minutes is a failed start, and two in a
    row pause the group, so a broken image does not launch twenty broken
    servers; one taken away is terminated once drained;
  - an active group with nothing waiting reads its rules against the
    members' metrics. Below its minimum it adds without asking (making the
    group approved that). Otherwise, in "Ask each time" it puts the add or
    take-away in Approvals; in automatic it runs it as argus-scale, naming
    the person who turned automatic on, up to 3 adds a day, after which it
    asks.

Never above the maximum or the cost cap, never twice inside a wait.

Under each group sits an AWS Auto Scaling group (scale_floor.go), made once
its image is ready, that keeps the minimum even when argus-control is
down; what AWS starts there joins the group as AWS's, and Argus fills
below the minimum only what AWS does not hold. When AWS decides, Argus
only records what AWS did.
*/

// RunScaleGroups follows every organization's scale groups each interval
// until ctx ends.
func (a *API) RunScaleGroups(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			a.TickScaleGroups(tctx, time.Now().UTC())
		})
		if err != nil {
			a.log.Error("scale groups", "error", err)
		}
	}
}

// TickScaleGroups does one minute's work for the organization on ctx.
func (a *API) TickScaleGroups(ctx context.Context, now time.Time) {
	lapsed, err := a.store.LapseScaleAsks(ctx, now.Add(-cloud.ScaleAskLapse))
	if err != nil {
		a.log.Error("scale groups: cannot lapse questions", "error", err)
	}
	for _, ch := range lapsed {
		a.closeAlerts(ctx, "cloud-change:"+ch.ID, "Nobody answered in 30 minutes")
		_ = a.store.AddScaleActivity(ctx, ch.ResourceID, ScaleActivity{At: now, Kind: "lapsed", Actor: "argus", ChangeID: ch.ID,
			Detail: "Asked at " + ch.RequestedAt.UTC().Format("15:04") + " UTC, nobody answered in 30 minutes: " + ch.Summary})
		a.auditScale(ctx, "lapsed", ch.ResourceName, "Nobody answered "+ch.Summary+" in 30 minutes (change "+ch.ID+").")
	}
	groups, err := a.store.ScaleGroups(ctx)
	if err != nil {
		a.log.Error("scale groups: cannot list", "error", err)
		return
	}
	for _, g := range groups {
		a.tickScaleGroup(ctx, g, now)
	}
}

func (a *API) tickScaleGroup(ctx context.Context, g ScaleGroup, now time.Time) {
	m, _, err := a.cloudManagedAccount(ctx, g.AccountID)
	if err != nil {
		_ = a.store.SetScaleGroupLooked(ctx, g.ID, "Argus cannot change "+g.AccountName+": "+err.Error(), now)
		return
	}
	r := a.scaleRequest(ctx, g)
	a.ensureFloor(ctx, m, &g, now)
	a.syncFloorMembers(ctx, &g)
	a.tendMembers(ctx, m, &g, now)
	if g.State != "active" {
		return
	}
	note := func(s string) { _ = a.store.SetScaleGroupLooked(ctx, g.ID, s, now) }
	if open, err := a.store.OpenScaleChange(ctx, g.AccountID, g.ID); err != nil || open != nil {
		if open != nil {
			if open.State == "awaiting_approval" {
				note("Waiting for an answer: " + open.Summary)
			} else {
				note("Running: " + open.Summary)
			}
		}
		return
	}
	serving := g.serving()
	if g.Mode == cloud.ScaleAWS {
		note(fmt.Sprintf("AWS decides, keeping %s; %d server%s now", g.AWSTrack.Words(), len(serving), plural(len(serving))))
		return
	}
	// A question nobody answered is not asked again until the wait after
	// adding has passed, so a group busy overnight does not ask every
	// minute.
	if acts, err := a.store.ScaleActivityOf(ctx, g.ID, 1); err == nil && len(acts) == 1 && acts[0].Kind == "lapsed" &&
		now.Sub(acts[0].At) < time.Duration(g.Limits.AddWait)*time.Minute {
		note(acts[0].Detail + ". Argus asks again after " + strconv.Itoa(g.Limits.AddWait) + " minutes if it is still needed")
		return
	}
	if len(serving) < g.Limits.Min {
		// What AWS keeps is AWS's to start again; Argus only fills what
		// the Auto Scaling group does not hold, like the source server.
		if short := int(floorCopies(g)) - g.awsServing(); g.AWSGroup != "" && short > 0 {
			note(fmt.Sprintf("Below its minimum of %d: AWS is starting %d in %s", g.Limits.Min, short, g.AWSGroup))
			return
		}
		if g.LastAddAt != nil && now.Sub(*g.LastAddAt) < time.Duration(g.Limits.AddWait)*time.Minute {
			note(fmt.Sprintf("Below its minimum of %d; waiting %d minutes after the last add", g.Limits.Min, g.Limits.AddWait))
			return
		}
		a.scaleAct(ctx, r, m, g, cloud.ActionScaleAdd, fmt.Sprintf("%s has %d of its minimum %d servers", g.Name, len(serving), g.Limits.Min), now, true)
		return
	}
	var counted []string
	for _, s := range serving {
		if s.State == "member" {
			counted = append(counted, s.Hostname)
		}
	}
	samples, err := a.store.scaleSamples(ctx, counted, now.Add(-2*time.Hour))
	if err != nil {
		a.log.Error("scale groups: cannot read metrics", "group", g.Name, "error", err)
		return
	}
	v := cloud.EvaluateScale(g.Rules, samples, counted, now)
	switch {
	case v.Held != "":
		note("Doing nothing: " + v.Held)
	case v.Add:
		if why := a.cannotAdd(g, serving, now); why != "" {
			note("Busy (" + v.Why + "), but " + why)
			return
		}
		a.scaleAct(ctx, r, m, g, cloud.ActionScaleAdd, v.Why, now, false, samples)
	case v.Remove:
		if why := cannotRemove(g, serving, now); why != "" {
			note("Quiet (" + v.Why + "), but " + why)
			return
		}
		a.scaleAct(ctx, r, m, g, cloud.ActionScaleRemove, v.Why, now, false, samples)
	default:
		note(fmt.Sprintf("Normal: no rule met across %d server%s", len(counted), plural(len(counted))))
	}
}

// cannotAdd says why the group cannot add now, or "".
func (a *API) cannotAdd(g ScaleGroup, serving []ScaleMember, now time.Time) string {
	if len(serving) >= g.Limits.Max {
		return fmt.Sprintf("it is at its maximum of %d", g.Limits.Max)
	}
	if p, ok := monthly(g.Template.Size); ok && g.Limits.CostCap > 0 && p*float64(len(serving)+1) > g.Limits.CostCap {
		return "one more would go over its cap of " + money(g.Limits.CostCap) + " a month"
	}
	for _, m := range serving {
		if m.State == "joining" {
			return m.Hostname + " is still starting"
		}
	}
	if last := latest(g.LastAddAt, g.LastRemoveAt); last != nil && now.Sub(*last) < time.Duration(g.Limits.AddWait)*time.Minute {
		return fmt.Sprintf("it waits %d minutes after the last change", g.Limits.AddWait)
	}
	return ""
}

func cannotRemove(g ScaleGroup, serving []ScaleMember, now time.Time) string {
	if len(serving) <= g.Limits.Min {
		return fmt.Sprintf("it is at its minimum of %d", g.Limits.Min)
	}
	if last := latest(g.LastAddAt, g.LastRemoveAt); last != nil && now.Sub(*last) < time.Duration(g.Limits.RemoveWait)*time.Minute {
		return fmt.Sprintf("it waits %d minutes after the last change before taking one away", g.Limits.RemoveWait)
	}
	return ""
}

func latest(ts ...*time.Time) *time.Time {
	var out *time.Time
	for _, t := range ts {
		if t != nil && (out == nil || t.After(*out)) {
			out = t
		}
	}
	return out
}

// scaleAct asks for, or runs, an add or take-away. keep is an add back to
// the minimum, which the group's approval already covers.
func (a *API) scaleAct(ctx context.Context, r *http.Request, m cloudManaged, g ScaleGroup, action cloud.Action, why string,
	now time.Time, keep bool, samples ...map[string][]cloud.ScaleSample) {
	ch := CloudChange{AccountID: g.AccountID, Region: g.Region, Action: action, Level: cloud.LevelChange,
		RequestedBy: string(cloud.ScaleActor), Reason: clip(why, 480)}
	ch.ResourceID, ch.ResourceName, ch.Environment = g.ID, g.Name, g.Environment
	var err error
	if action == cloud.ActionScaleAdd {
		ch, _, err = a.planScaleAdd(ctx, g, ch, keep)
	} else {
		ch, _, err = a.planScaleRemove(ctx, g, ch)
	}
	if err != nil {
		_ = a.store.SetScaleGroupLooked(ctx, g.ID, "Wanted to "+verbOf(action)+" ("+why+"), but "+err.Error(), now)
		return
	}
	auto := keep || g.Mode == cloud.ScaleAuto
	if auto && action == cloud.ActionScaleAdd && !keep {
		if n, err := a.store.ScaleAutoAdds(ctx, g.ID, now.Add(-24*time.Hour)); err != nil || n >= cloud.ScaleAutoAddsPerDay {
			auto = false
			why += fmt.Sprintf(". Argus already added %d today by itself, so it asks", cloud.ScaleAutoAddsPerDay)
		}
	}
	plan := scalePlan{Why: why, Min: g.Limits.Min, Max: g.Limits.Max, CostCap: g.Limits.CostCap, Automatic: auto, AutoBy: g.AutoBy}
	plan.After = len(g.serving()) + 1
	if action == cloud.ActionScaleRemove {
		plan.After = len(g.serving()) - 1
	}
	if p, ok := monthly(g.Template.Size); ok {
		total := cents(p * float64(plan.After))
		plan.Monthly, plan.GroupCost = &p, &total
	}
	if len(g.Rules.Add) > 0 {
		rule := g.Rules.Add[0]
		if action == cloud.ActionScaleRemove && len(g.Rules.Remove) > 0 {
			rule = g.Rules.Remove[0]
		}
		plan.Signal, plan.Threshold = rule.Signal, rule.Value
		if len(samples) > 0 {
			plan.Series = groupSeries(samples[0], rule.Signal, now)
		}
	}
	ch.Plan, _ = json.Marshal(plan)
	ch.State = "awaiting_approval"
	if auto {
		ch.State = "applying"
	}
	ch, err = a.store.CreateCloudChange(ctx, ch)
	if err != nil {
		if !errors.Is(err, ErrCloudChangeBusy) {
			a.log.Error("scale groups: cannot record a change", "group", g.Name, "error", err)
		}
		return
	}
	kind := "rule"
	if keep {
		kind = "minimum"
	}
	_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: kind, Actor: "argus", ChangeID: ch.ID, Detail: "Rule met: " + why})
	if !auto {
		_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "asked", Actor: "argus", ChangeID: ch.ID,
			Detail: "Asked: " + ch.Summary + ". It lapses in 30 minutes"})
		_ = a.store.SetScaleGroupLooked(ctx, g.ID, "Asked at "+now.Format("15:04")+" UTC: "+ch.Summary, now)
		a.auditCloudChange(r, ch, "requested", ch.RequestedBy, ch.Summary+". Asked by Argus for scale group "+g.Name+
			" ("+why+"); waits for approval, lapses in 30 minutes.")
		title := "Add a server to " + g.Name + "?"
		if action == cloud.ActionScaleRemove {
			title = "Take a server out of " + g.Name + "?"
		}
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Severity: "warning", Audience: AudienceDeciders,
			Title: title, Detail: clip(why, 300) + ". " + ch.Summary + ". It lapses in 30 minutes.", Link: "/approvals",
			Subject: "cloud-change:" + ch.ID, Actor: string(cloud.ScaleActor)})
		return
	}
	who := "automatic, turned on by " + g.AutoBy
	if keep {
		who = "to keep the minimum approved with the group"
	}
	a.auditCloudChange(r, ch, "requested", ch.RequestedBy, ch.Summary+". By Argus for scale group "+g.Name+", "+who+" ("+why+").")
	done := a.applyCloudChange(r, m, ch)
	_ = a.store.SetScaleGroupLooked(ctx, g.ID, capitalize(verbOf(action))+": "+done.Summary+" ("+done.State+")", now)
	a.raiseAlert(ctx, ConsoleAlert{Kind: AlertCloud, Severity: "info", Audience: AudienceAdmins,
		Title: g.Name + ": " + done.Summary, Detail: "Argus did this by itself, " + who + ". " + clip(why, 300),
		Link: "/cloud/scale/" + g.ID})
}

func verbOf(a cloud.Action) string {
	if a == cloud.ActionScaleAdd {
		return "add a server"
	}
	return "take a server away"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// tendMembers moves each member along: into the target group once it runs,
// a member once healthy, failed when it is not in time, terminated once
// drained, gone when AWS no longer has it.
func (a *API) tendMembers(ctx context.Context, m cloudManaged, g *ScaleGroup, now time.Time) {
	nw, _ := m.manager.(cloud.Network)
	health := a.targetHealth(ctx, g.AccountID, g.Region, g.TargetGroup)
	actor := cloud.ScaleActor
	act := func(kind, detail string) {
		_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: kind, Actor: "argus", Detail: detail})
	}
	changed := false
	for _, mem := range g.Members {
		if !mem.Active() {
			continue
		}
		inst, err := a.store.CloudInstance(ctx, g.AccountID, g.Region, mem.InstanceID)
		missing := err != nil || inst.State == "terminated" || inst.State == "shutting-down"
		switch mem.State {
		case "joining":
			if missing {
				if err != nil && now.Sub(mem.JoinedAt) < 10*time.Minute {
					continue // not read yet
				}
				_ = a.store.SetScaleMember(ctx, g.ID, mem.InstanceID, "failed", now)
				act("failed", mem.Hostname+" ended before it joined "+g.TargetName)
				a.failedStart(ctx, g, mem.Hostname)
				changed = true
				continue
			}
			if mem.RegisteredAt == nil {
				if inst.State != "running" || nw == nil {
					continue
				}
				if _, err := nw.RegisterTarget(ctx, m.target, actor, g.Region, g.TargetGroup, mem.InstanceID, g.Port); err != nil {
					act("failed", "Could not put "+mem.Hostname+" in "+g.TargetName+": "+err.Error())
					continue
				}
				_ = a.store.RegisterScaleMember(ctx, g.ID, mem.InstanceID, now)
				act("joined", "Put "+mem.Hostname+" in "+g.TargetName+"; it serves once its health check passes")
				a.syncCloudAccountSoon(ctx, m.acct)
				continue
			}
			if health[mem.InstanceID] == "healthy" {
				_ = a.store.SetScaleMember(ctx, g.ID, mem.InstanceID, "member", now)
				_ = a.store.SetScaleFailedStarts(ctx, g.ID, 0)
				act("healthy", fmt.Sprintf("%s is healthy in %s after %s", mem.Hostname, g.TargetName, roughly(now.Sub(*mem.RegisteredAt))))
				changed = true
				continue
			}
			if now.Sub(*mem.RegisteredAt) > cloud.ScaleHealthWait {
				_ = a.store.SetScaleMember(ctx, g.ID, mem.InstanceID, "failed", now)
				if mem.InAWS {
					act("failed", fmt.Sprintf("%s did not become healthy in %s within %d minutes (it is %s). AWS replaces it by itself",
						mem.Hostname, g.TargetName, int(cloud.ScaleHealthWait.Minutes()), orWord(health[mem.InstanceID], "not checked")))
					a.failedStart(ctx, g, mem.Hostname)
					changed = true
					continue
				}
				if nw != nil {
					_, _ = nw.DeregisterTarget(ctx, m.target, actor, g.Region, g.TargetGroup, mem.InstanceID, g.Port)
				}
				act("failed", fmt.Sprintf("%s did not become healthy in %s within %d minutes (it is %s). Taken out of the target group; it still runs, for you to look at",
					mem.Hostname, g.TargetName, int(cloud.ScaleHealthWait.Minutes()), orWord(health[mem.InstanceID], "not checked")))
				a.failedStart(ctx, g, mem.Hostname)
				changed = true
			}
		case "member":
			if missing {
				_ = a.store.SetScaleMember(ctx, g.ID, mem.InstanceID, "gone", now)
				if mem.InAWS {
					act("gone", "AWS ended "+mem.Hostname+" in "+g.AWSGroup)
				} else {
					act("gone", mem.Hostname+" is gone: it was terminated outside Argus")
				}
				changed = true
			}
		case "draining":
			if missing {
				_ = a.store.SetScaleMember(ctx, g.ID, mem.InstanceID, "gone", now)
				changed = true
				continue
			}
			if mem.DrainUntil == nil || now.Before(*mem.DrainUntil) {
				continue
			}
			res, err := m.manager.Remove(ctx, m.target, actor, g.Region, mem.InstanceID, false)
			if err != nil {
				act("failed", "Could not terminate "+mem.Hostname+" after draining: "+err.Error())
				continue
			}
			_ = a.store.SetScaleMember(ctx, g.ID, mem.InstanceID, "gone", now)
			act("terminated", fmt.Sprintf("Drained %s for %d s, then terminated it", mem.Hostname, int(cloud.ScaleDrain.Seconds())))
			a.auditScale(ctx, "terminated", g.Name, fmt.Sprintf("Terminated %s (%s) of scale group %s after draining it; AWS request %s.",
				mem.Hostname, mem.InstanceID, g.Name, res.RequestID))
			if asset, ok := a.assetByHostname(ctx, mem.Hostname); ok {
				_ = a.store.ArchiveAsset(ctx, asset.ID)
			}
			a.syncCloudAccountSoon(ctx, m.acct)
			changed = true
		}
	}
	if changed {
		if fresh, err := a.store.ScaleGroup(ctx, g.ID); err == nil {
			*g = fresh
		}
	}
}

// failedStart counts a new server that did not become healthy, and pauses
// the group at the second in a row.
func (a *API) failedStart(ctx context.Context, g *ScaleGroup, host string) {
	g.FailedStarts++
	_ = a.store.SetScaleFailedStarts(ctx, g.ID, g.FailedStarts)
	if g.FailedStarts < cloud.ScaleFailedStarts || g.State != "active" {
		return
	}
	reason := fmt.Sprintf("%d new servers in a row did not become healthy, the last %s. The image or the health check may be wrong; resume once it is fixed", g.FailedStarts, host)
	g.State, g.PausedReason = "paused", reason
	_ = a.store.SetScaleGroupState(ctx, g.ID, "paused", reason)
	_ = a.store.AddScaleActivity(ctx, g.ID, ScaleActivity{Kind: "paused", Actor: "argus", Detail: "Paused: " + reason})
	a.auditScale(ctx, "paused", g.Name, "Paused scale group "+g.Name+": "+reason+".")
	a.raiseAlert(ctx, ConsoleAlert{Kind: AlertCloud, Severity: "warning", Audience: AudienceAdmins,
		Title: "Scale group " + g.Name + " stopped adding servers", Detail: reason + ".", Link: "/cloud/scale/" + g.ID})
}

func roughly(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
}

func orWord(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// scaleRequest stands in for a person's request when Argus acts by itself:
// the address new servers reach Argus at, as the group's maker saw it.
func (a *API) scaleRequest(ctx context.Context, g ScaleGroup) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://argus.invalid/", nil)
	if u, err := url.Parse(g.EnrollBase); err == nil && u.Host != "" {
		r.Host = u.Host
	}
	return r
}

// auditScale records cloud.scale.<what>.
func (a *API) auditScale(ctx context.Context, what, group, detail string) {
	if _, err := a.store.AppendAudit(ctx, AuditEvent{Action: "cloud.scale." + what, Severity: "notice",
		ActorEmail: string(cloud.ScaleActor), Target: "scale group " + group, Detail: detail}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "cloud.scale."+what)
	}
}

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/* ── On a heartbeat ──────────────────────────────────────────────────────── */

// heartbeatDisks keeps what the agent on host said about its disks, starts
// the grow its rule calls for, and answers with what the agent should do.
// It returns nil for an agent older than disk growing, and on a read that
// failed, so the agent keeps what it last heard.
func (a *API) heartbeatDisks(ctx context.Context, host string, b *DiskBeat) *DiskOrders {
	if b == nil {
		return nil
	}
	if err := a.store.recordDiskBeat(ctx, host, *b); err != nil {
		a.log.Error("heartbeat: cannot keep the disks", "hostname", host, "error", err)
	}
	_, assetHost, tags, err := a.store.assetForAgent(ctx, host)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			a.log.Error("heartbeat: cannot find the server for its disks", "hostname", host, "error", err)
		}
		return nil
	}
	if b.Run != nil {
		ended, err := a.store.recordDiskRunStatus(ctx, host, *b.Run)
		if err != nil {
			a.log.Error("heartbeat: cannot keep a disk grow's status", "hostname", host, "error", err)
		} else if ended != nil {
			a.diskRunEnded(ctx, *ended)
		}
	}
	a.expireDiskRuns(ctx, host)
	if b.Supported {
		policy, err := a.store.DiskGrowPolicy(ctx)
		if err != nil {
			a.log.Error("heartbeat: cannot read the disk grow rules", "error", err)
		} else {
			a.startAutomaticGrow(ctx, host, assetHost, policy.ruleFor(tags), time.Now())
		}
	}
	o := &DiskOrders{Scan: true}
	if id, rescan, err := a.store.diskScanAsked(ctx, host); err == nil {
		o.ScanID, o.Rescan = id, rescan
	} else {
		a.log.Error("heartbeat: cannot read a disk scan asked for", "error", err)
	}
	run, err := a.store.diskRunToSend(ctx, host)
	if err != nil {
		a.log.Error("heartbeat: cannot read the disk grow to send", "error", err)
		return nil
	}
	if run != nil {
		o.Run = &DiskGrowRequest{ID: run.ID, Key: run.Key, Path: run.Path, Bytes: run.GrowBytes}
	}
	return o
}

func (a *API) expireDiskRuns(ctx context.Context, host string) {
	stale, err := a.store.expireStaleDiskRuns(ctx, host)
	if err != nil {
		a.log.Error("cannot end quiet disk grows", "hostname", host, "error", err)
		return
	}
	for _, r := range stale {
		a.diskRunEnded(ctx, r)
	}
}

// startAutomaticGrow grows the fullest filesystem past the rule's mark that
// has room below it, at most once a day on a server, and only into room the
// server already has: it never makes a disk or a cloud volume bigger.
func (a *API) startAutomaticGrow(ctx context.Context, host, assetHost string, rule DiskGrowRule, now time.Time) {
	if rule.AutoPercent == 0 {
		return
	}
	report, _, _, err := a.store.diskReportFor(ctx, host)
	if err != nil || report == nil || !report.Supported || report.Error != "" {
		return
	}
	var pick *DiskFS
	for i := range report.Filesystems {
		f := &report.Filesystems[i]
		if f.Answer != DiskGrow || f.Path == nil || f.Room <= 0 || f.percent() < rule.AutoPercent {
			continue
		}
		if pick == nil || f.percent() > pick.percent() {
			pick = f
		}
	}
	if pick == nil {
		return
	}
	plan, err := planDiskGrow(*report, pick.Key, 0)
	if err != nil {
		return
	}
	if pending, err := a.store.hasPendingDiskRun(ctx, host); err != nil || pending {
		return
	}
	day := now.UTC().Format("2006-01-02")
	reason := fmt.Sprintf("%s was %d%% full, past the rule's %d%%", pick.Mount, pick.percent(), rule.AutoPercent)
	run, err := a.store.CreateDiskRun(ctx, newDiskRun{Hostname: host, AssetHostname: assetHost, Trigger: "automatic",
		State: RunQueued, Plan: plan, Reason: reason, RequestedBy: "rule", AutoDay: &day})
	if errors.Is(err, ErrDiskRunActive) {
		return // today's already ran, or one started in between
	}
	if err != nil {
		a.log.Error("cannot start an automatic disk grow", "hostname", host, "error", err)
		return
	}
	a.auditDisks(ctx, AuditEvent{
		Action: "disks.grow_started", Severity: "notice", Target: assetHost,
		Detail: fmt.Sprintf("Started to %s on %s because %s, as its disk grow rule (%s) says.", plan.Summary,
			assetHost, reason, diskRuleName(rule)),
	})
	a.log.Info("automatic disk grow started", "hostname", host, "run", run.ID)
}

func diskRuleName(r DiskGrowRule) string {
	if r.Tag == "" {
		return "every other server"
	}
	return "tag " + r.Tag
}

// diskRunEnded records how a grow ended, once: in the audit log with what
// it ran, and in the bell when it needs someone.
func (a *API) diskRunEnded(ctx context.Context, r DiskGrowRun) {
	action, severity := "disks.grow_finished", "notice"
	switch r.State {
	case RunFailed:
		action, severity = "disks.grow_failed", "warn"
	case RunInterrupted:
		action, severity = "disks.grow_interrupted", "warn"
	}
	who := "asked by " + r.RequestedBy
	if r.DecidedBy != nil {
		who += ", approved by " + *r.DecidedBy
	}
	if r.Trigger == "automatic" {
		who = "started by its rule because " + r.Reason
	}
	detail := fmt.Sprintf("The disk grow on %s (%s, %s) %s", r.AssetHostname, r.Summary, who,
		strings.ReplaceAll(r.State, "_", " "))
	if r.SizeAfter > 0 {
		detail += fmt.Sprintf(": %s went from %s to %s", r.Mount, gbytes(r.SizeBefore), gbytes(r.SizeAfter))
	}
	if r.Message != "" {
		detail += ". " + strings.TrimSuffix(r.Message, ".")
	}
	var cmds []string
	for _, s := range r.Steps {
		if s.Command != "" && s.State != "" {
			cmds = append(cmds, s.Command+" ("+s.State+")")
		}
	}
	if len(cmds) > 0 {
		detail += ". It ran: " + strings.Join(cmds, "; ")
	}
	detail += ". The commands' output and the partition table before are kept with the grow (" + r.ID + ")."
	a.auditDisks(ctx, AuditEvent{Action: action, Severity: severity, Target: r.AssetHostname, Detail: clip(detail, 4000)})
	if r.State == RunFailed || r.State == RunInterrupted {
		link := "/assets"
		if id, _, _, err := a.store.assetForAgent(ctx, r.Hostname); err == nil {
			link = "/assets/" + id + "?tab=disks"
		}
		title := "Growing " + r.Mount + " on " + r.AssetHostname + " failed"
		if r.State == RunInterrupted {
			title = "Growing " + r.Mount + " on " + r.AssetHostname + " was interrupted"
		}
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
			Title: title, Detail: clip(r.Message, 1000), Link: link,
		})
	}
}

func (a *API) auditDisks(ctx context.Context, e AuditEvent) {
	if _, err := a.store.AppendAudit(ctx, e); err != nil {
		a.log.Error("audit append failed", "error", err, "action", e.Action)
	}
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) registerDiskGrowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/assets/{id}/disks", a.getAssetDisks)
	mux.HandleFunc("POST /api/v1/assets/{id}/disks/scan", a.postAssetDisksScan)
	mux.HandleFunc("POST /api/v1/assets/{id}/disks/grow", a.postAssetDiskGrow)
	mux.HandleFunc("GET /api/v1/disk-runs/{id}", a.getDiskRun)
	mux.HandleFunc("POST /api/v1/disk-runs/{id}/approve", a.postDiskRunDecision(true))
	mux.HandleFunc("POST /api/v1/disk-runs/{id}/deny", a.postDiskRunDecision(false))
	mux.HandleFunc("POST /api/v1/disk-runs/{id}/cancel", a.postDiskRunCancel)
	mux.HandleFunc("GET /api/v1/settings/disk-grow", a.getDiskGrowPolicy)
	mux.HandleFunc("PUT /api/v1/settings/disk-grow", a.putDiskGrowPolicy)
}

type diskTarget struct {
	asset Asset
	agent string
	view  DisksView
}

// disksFor reads a server's Disks tab.
func (a *API) disksFor(ctx context.Context, asset Asset) (diskTarget, error) {
	t := diskTarget{asset: asset}
	agent, err := a.store.AgentHostname(ctx, asset.Hostname)
	if err != nil {
		return t, err
	}
	t.agent = agent
	a.expireDiskRuns(ctx, agent)
	v := DisksView{Hostname: asset.Hostname, Runs: []DiskGrowRun{}}
	var n int
	if err := a.store.db(ctx).QueryRow(ctx, `SELECT count(*) FROM agents WHERE lower(hostname) = lower($1)`,
		agent).Scan(&n); err != nil {
		return t, err
	}
	v.Enrolled = n > 0
	report, st, asked, err := a.store.diskReportFor(ctx, agent)
	if err != nil {
		return t, err
	}
	if st != nil {
		v.Reported, v.Supported, v.Reason, v.Scanning = true, st.Supported, st.Reason, st.Scanning
	}
	v.Report, v.ScanAsked = report, asked
	policy, err := a.store.DiskGrowPolicy(ctx)
	if err != nil {
		return t, err
	}
	v.Rule = policy.ruleFor(asset.Tags)
	if v.Runs, err = a.store.DiskRunsFor(ctx, agent, 20); err != nil {
		return t, err
	}
	for i := range v.Runs {
		v.Runs[i].AssetID = asset.ID
	}
	t.view = v
	return t, nil
}

// disksReady says whether disks can be scanned or grown on t now, answering
// the request itself when not.
func disksReady(w http.ResponseWriter, t diskTarget) bool {
	switch v := t.view; {
	case t.asset.Protocol != "ssh" && t.asset.Protocol != "rdp":
		writeErr(w, http.StatusConflict, "growing disks is for Linux and Windows servers")
	case !v.Enrolled:
		writeErr(w, http.StatusConflict, "this server has no enrolled agent; add it from Servers, Add a server")
	case !v.Reported:
		writeErr(w, http.StatusConflict, "this server's agent is too old to grow disks; "+
			"update the agent from its Settings tab first")
	case !v.Supported:
		writeErr(w, http.StatusConflict, "disks cannot be grown on this server from the console: "+v.Reason)
	default:
		return true
	}
	return false
}

func (a *API) getAssetDisks(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "disks are for administrators, owners and auditors")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.disksFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "disks", err)
		return
	}
	writeJSON(w, http.StatusOK, t.view)
}

// diskScanRequest asks for a scan; Rescan asks the disks for their size
// first, for a disk made bigger in the hypervisor or the cloud.
type diskScanRequest struct {
	Rescan bool `json:"rescan"`
}

func (a *API) postAssetDisksScan(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "scanning disks", "admin", "owner") {
		return
	}
	var in diskScanRequest
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.disksFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "disks", err)
		return
	}
	if !disksReady(w, t) {
		return
	}
	id, err := a.store.askDiskScan(r.Context(), t.agent, in.Rescan)
	if err != nil {
		a.fail(w, "disk scan", err)
		return
	}
	if in.Rescan {
		// Asking a disk for its size is harmless but touches the kernel's
		// view of the disks, so it is on the record.
		a.auditAsset(r, AuditEvent{
			Action: "disks.rescan_requested", Severity: "info", ActorEmail: sess.Email, Target: asset.Hostname,
			Detail: "Asked " + asset.Hostname + "'s disks for their size again, to see a disk made bigger outside the server.",
		})
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"scanId": id})
}

// diskGrowAsk is a grow asked for in the console.
type diskGrowAsk struct {
	// Key names the filesystem, as the server's last scan named it.
	Key string `json:"key"`
	// Bytes gives an LVM volume part of its group's room; 0 is all of it.
	Bytes int64 `json:"bytes"`
}

func (a *API) postAssetDiskGrow(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "growing disks", "admin", "owner") {
		return
	}
	var in diskGrowAsk
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Key == "" || len(in.Key) > 400 {
		writeErr(w, http.StatusBadRequest, "name the filesystem to grow")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.disksFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "disks", err)
		return
	}
	if !disksReady(w, t) {
		return
	}
	if t.view.Report == nil {
		writeErr(w, http.StatusConflict, "this server's disks have not been scanned yet; scan them first")
		return
	}
	plan, err := planDiskGrow(*t.view.Report, in.Key, in.Bytes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	state := RunQueued
	if t.view.Rule.NeedsApproval {
		state = RunAwaitingApproval
	}
	run, err := a.store.CreateDiskRun(r.Context(), newDiskRun{Hostname: t.agent, AssetHostname: asset.Hostname,
		Trigger: "manual", State: state, Plan: plan, RequestedBy: sess.Email})
	if errors.Is(err, ErrDiskRunActive) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "disk grow", err)
		return
	}
	waits := ""
	if state == RunAwaitingApproval {
		waits = "; it waits for another admin to approve it"
	}
	a.auditAsset(r, AuditEvent{
		Action: "disks.grow_requested", Severity: "notice", ActorEmail: sess.Email, Target: asset.Hostname,
		Detail: fmt.Sprintf("Asked to %s on %s%s.", plan.Summary, asset.Hostname, waits),
	})
	if state == RunAwaitingApproval {
		a.raiseAlert(r.Context(), ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: sess.Email,
			Title:   sess.Email + " asks to grow " + plan.FS.Mount + " on " + asset.Hostname,
			Detail:  "To " + plan.Summary + ". Approve or deny it on the server's Disks tab.",
			Link:    "/assets/" + asset.ID + "?tab=disks",
			Subject: diskRunSubject(run.ID),
		})
	}
	run.AssetID = asset.ID
	writeJSON(w, http.StatusAccepted, run)
}

func diskRunSubject(id string) string { return "disk-run:" + id }

func (a *API) getDiskRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "disks are for administrators, owners and auditors")
		return
	}
	run, err := a.store.DiskRunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failDiskRun(w, err)
		return
	}
	if id, _, _, err := a.store.assetForAgent(r.Context(), run.Hostname); err == nil {
		run.AssetID = id
	}
	writeJSON(w, http.StatusOK, run)
}

func (a *API) failDiskRun(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrDiskRunNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrRunState):
		writeErr(w, http.StatusConflict, strings.TrimPrefix(err.Error(), ErrRunState.Error()+": "))
	case errors.Is(err, ErrDiskRunActive):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		a.fail(w, "disk grow", err)
	}
}

func (a *API) postDiskRunDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !a.privileged(w, sess, "approving disk grows", "admin", "owner") {
			return
		}
		run, err := a.store.DecideDiskRun(r.Context(), r.PathValue("id"), sess.Email, approve)
		if err != nil {
			a.failDiskRun(w, err)
			return
		}
		action, verb := "disks.grow_approved", "Approved"
		if !approve {
			action, verb = "disks.grow_denied", "Denied"
		}
		a.auditAsset(r, AuditEvent{
			Action: action, Severity: "notice", ActorEmail: sess.Email, Target: run.AssetHostname,
			Detail: fmt.Sprintf("%s the request to %s on %s, asked by %s.", verb, run.Summary, run.AssetHostname,
				run.RequestedBy),
		})
		a.closeAlerts(r.Context(), diskRunSubject(run.ID), verb+" by "+sess.Email)
		writeJSON(w, http.StatusOK, run)
	}
}

func (a *API) postDiskRunCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "taking back disk grows", "admin", "owner") {
		return
	}
	run, err := a.store.CancelDiskRun(r.Context(), r.PathValue("id"), sess.Email)
	if err != nil {
		a.failDiskRun(w, err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "disks.grow_cancelled", Severity: "notice", ActorEmail: sess.Email, Target: run.AssetHostname,
		Detail: fmt.Sprintf("Took back the request to %s on %s before it started.", run.Summary, run.AssetHostname),
	})
	a.closeAlerts(r.Context(), diskRunSubject(run.ID), "Taken back by "+sess.Email)
	writeJSON(w, http.StatusOK, run)
}

func (a *API) getDiskGrowPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "disks are for administrators, owners and auditors")
		return
	}
	p, err := a.store.DiskGrowPolicy(r.Context())
	if err != nil {
		a.fail(w, "disk grow rules", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) putDiskGrowPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing how disks grow", "admin", "owner") {
		return
	}
	var in DiskGrowPolicy
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := a.store.DiskGrowPolicy(r.Context())
	if err != nil {
		a.fail(w, "disk grow rules", err)
		return
	}
	if err := a.store.SaveDiskGrowPolicy(r.Context(), in); err != nil {
		a.fail(w, "disk grow rules", err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "disks.policy_changed", Severity: "notice", ActorEmail: sess.Email, Target: "disk grow",
		Detail: "Changed how disks grow, from " + describeDiskPolicy(before) + " to " + describeDiskPolicy(in) + ".",
	})
	out, err := a.store.DiskGrowPolicy(r.Context())
	if err != nil {
		a.fail(w, "disk grow rules", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func describeDiskPolicy(p DiskGrowPolicy) string {
	parts := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		s := diskRuleName(r) + ": "
		if r.AutoPercent > 0 {
			s += fmt.Sprintf("grows on its own past %d%%", r.AutoPercent)
		} else {
			s += "by hand only"
		}
		if r.NeedsApproval {
			s += ", needs approval"
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

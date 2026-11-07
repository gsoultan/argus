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

// heartbeatLogs keeps what the agent on host said about logs, starts the
// cleanup its rule calls for, and answers with what the agent should do. It
// returns nil for an agent older than log cleanup, and on a read that
// failed, so the agent keeps what it last heard.
func (a *API) heartbeatLogs(ctx context.Context, host string, b *LogBeat) *LogOrders {
	if b == nil {
		return nil
	}
	prev, err := a.store.recordLogBeat(ctx, host, *b)
	if err != nil {
		a.log.Error("heartbeat: cannot keep log cleanup", "hostname", host, "error", err)
	}
	assetID, assetHost, tags, err := a.store.assetForAgent(ctx, host)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			a.log.Error("heartbeat: cannot find the server for log cleanup", "hostname", host, "error", err)
		}
		return nil
	}
	if b.Report != nil {
		a.alertFillingDisk(ctx, assetID, assetHost, prev, *b.Report)
	}
	if b.Run != nil {
		ended, err := a.store.recordLogRunStatus(ctx, host, *b.Run)
		if err != nil {
			a.log.Error("heartbeat: cannot keep a log cleanup's status", "hostname", host, "error", err)
		} else if ended != nil {
			a.logRunEnded(ctx, *ended)
		}
	}
	a.expireLogRuns(ctx, host)
	policy, err := a.store.LogCleanupPolicy(ctx)
	if err != nil {
		a.log.Error("heartbeat: cannot read the log cleanup rules", "error", err)
		return nil
	}
	rule := policy.ruleFor(tags)
	if b.Supported {
		a.startAutomaticCleanup(ctx, host, assetHost, rule, policy, time.Now())
	}
	o := &LogOrders{Scan: true, Paths: policy.Paths, Protect: policy.Protect, ActiveOverMB: rule.ActiveOverMB}
	if id, err := a.store.logScanAsked(ctx, host); err == nil {
		o.ScanID = id
	} else {
		a.log.Error("heartbeat: cannot read a log scan asked for", "error", err)
	}
	run, err := a.store.logRunToSend(ctx, host)
	if err != nil {
		a.log.Error("heartbeat: cannot read the log cleanup to send", "error", err)
		return nil
	}
	if run != nil {
		o.Run = &LogCleanRequest{ID: run.ID, Actions: run.Actions, Paths: policy.Paths, Protect: policy.Protect}
	}
	return o
}

func (a *API) expireLogRuns(ctx context.Context, host string) {
	stale, err := a.store.expireStaleLogRuns(ctx, host)
	if err != nil {
		a.log.Error("cannot end quiet log cleanups", "hostname", host, "error", err)
		return
	}
	for _, r := range stale {
		a.logRunEnded(ctx, r)
	}
}

// startAutomaticCleanup starts the cleanup rule calls for on host, at most
// once a day: when a disk passes the rule's mark, or in its daily window,
// with something to free and nothing else going or waiting there.
func (a *API) startAutomaticCleanup(ctx context.Context, host, assetHost string, rule LogCleanupRule,
	policy LogCleanupPolicy, now time.Time) {
	if rule.Mode != LogCleanDisk && rule.Mode != LogCleanDaily {
		return
	}
	report, _, _, err := a.store.logReportFor(ctx, host)
	if err != nil || report == nil || !report.Supported || report.Error != "" {
		return
	}
	var day, reason string
	switch rule.Mode {
	case LogCleanDisk:
		d, full := report.fullDisk(rule.DiskPercent)
		if !full {
			return
		}
		day = serverDay(now, report.UTCOffsetMinutes)
		reason = fmt.Sprintf("%s was %d%% full, past the rule's %d%%", d.Mount, d.percent(), rule.DiskPercent)
	case LogCleanDaily:
		var in bool
		if day, in = rule.inWindow(now, report.UTCOffsetMinutes); !in {
			return
		}
		reason = fmt.Sprintf("its daily window at %02d:%02d", rule.WindowStartMinute/60, rule.WindowStartMinute%60)
	}
	plan, err := planLogCleanup(*report, rule, automaticPick(*report, rule, now), now)
	if err != nil || plan.Bytes == 0 {
		return
	}
	if pending, err := a.store.hasPendingLogRun(ctx, host); err != nil || pending {
		return
	}
	run, err := a.store.CreateLogRun(ctx, newLogRun{Hostname: host, AssetHostname: assetHost, Trigger: "automatic",
		State: RunQueued, Plan: plan, Reason: reason, AutoDay: &day})
	if errors.Is(err, ErrLogRunActive) {
		return // today's already ran, or one started in between
	}
	if err != nil {
		a.log.Error("cannot start an automatic log cleanup", "hostname", host, "error", err)
		return
	}
	a.auditLogs(ctx, AuditEvent{
		Action: "logs.clean_started", Severity: "notice", Target: assetHost,
		Detail: fmt.Sprintf("Started cleaning logs on %s because %s, as its log cleanup rule (%s) says: %s, "+
			"about %s.", assetHost, reason, logRuleName(rule), plan.Summary, gbytes(plan.Bytes)),
	})
	a.log.Info("automatic log cleanup started", "hostname", host, "run", run.ID)
}

func logRuleName(r LogCleanupRule) string {
	if r.Tag == "" {
		return "every other server"
	}
	return "tag " + r.Tag
}

// alertFillingDisk tells admins when a disk passes 90%, and when deleted
// files still held open pass a gigabyte: once as each crosses, not on
// every scan.
func (a *API) alertFillingDisk(ctx context.Context, assetID, assetHost string, prev *LogReport, now LogReport) {
	if now.Error != "" {
		return
	}
	link := "/assets/" + assetID + "?tab=logs"
	var cleanable int64
	for _, it := range now.Items {
		cleanable += it.Bytes
	}
	for _, d := range now.Disks {
		if d.percent() < 90 {
			continue
		}
		was := 0
		if prev != nil {
			for _, p := range prev.Disks {
				if p.Mount == d.Mount {
					was = p.percent()
				}
			}
		}
		if was >= 90 {
			continue
		}
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
			Title: fmt.Sprintf("%s: %s is %d%% full", assetHost, d.Mount, d.percent()),
			Detail: fmt.Sprintf("%s free. Logs Argus can clean use %s; see the server's Logs tab.",
				gbytes(int64(d.Avail)), gbytes(cleanable)),
			Link: link,
		})
	}
	const lots = 1 << 30
	if held := now.openBytes(); held >= lots && (prev == nil || prev.openBytes() < lots) {
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertServer, Severity: "info", Audience: AudienceAdmins,
			Title:  fmt.Sprintf("%s: %s held by deleted files", assetHost, gbytes(held)),
			Detail: "Someone deleted files a program still has open, so their space is not free. Empty them or restart the program from the Logs tab.",
			Link:   link,
		})
	}
}

// logRunEnded records how a cleanup ended, once: in the audit log with
// what it removed, and in the bell when it needs someone.
func (a *API) logRunEnded(ctx context.Context, r LogCleanupRun) {
	action, severity := "logs.clean_finished", "notice"
	switch r.State {
	case RunFailed:
		action, severity = "logs.clean_failed", "warn"
	case RunInterrupted:
		action, severity = "logs.clean_interrupted", "warn"
	}
	who := "asked by " + r.RequestedBy
	if r.DecidedBy != nil {
		who += ", approved by " + *r.DecidedBy
	}
	if r.Trigger == "automatic" {
		who = "started by its rule because " + r.Reason
	}
	detail := fmt.Sprintf("The log cleanup on %s (%s, %s) %s: freed %s, removed or emptied %d file%s",
		r.AssetHostname, r.Summary, who, strings.ReplaceAll(r.State, "_", " "), gbytes(r.Freed),
		r.RemovedCount, plural(r.RemovedCount))
	if n := len(r.Skipped); n > 0 {
		detail += fmt.Sprintf(", left %d alone", n)
	}
	if r.Message != "" {
		detail += ". " + strings.TrimSuffix(r.Message, ".")
	}
	detail += ". The list of files is kept with the cleanup (" + r.ID + ")."
	a.auditLogs(ctx, AuditEvent{Action: action, Severity: severity, Target: r.AssetHostname, Detail: detail})
	if r.State == RunFailed || r.State == RunInterrupted {
		link := "/assets"
		if id, _, _, err := a.store.assetForAgent(ctx, r.Hostname); err == nil {
			link = "/assets/" + id + "?tab=logs"
		}
		title := "Cleaning logs on " + r.AssetHostname + " failed"
		if r.State == RunInterrupted {
			title = "Cleaning logs on " + r.AssetHostname + " was interrupted"
		}
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
			Title: title, Detail: clip(r.Message, 1000), Link: link,
		})
	}
}

func (a *API) auditLogs(ctx context.Context, e AuditEvent) {
	if _, err := a.store.AppendAudit(ctx, e); err != nil {
		a.log.Error("audit append failed", "error", err, "action", e.Action)
	}
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) registerLogCleanupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/assets/{id}/logs", a.getAssetLogs)
	mux.HandleFunc("POST /api/v1/assets/{id}/logs/scan", a.postAssetLogsScan)
	mux.HandleFunc("POST /api/v1/assets/{id}/logs/runs", a.postAssetLogRun)
	mux.HandleFunc("GET /api/v1/log-runs/{id}", a.getLogRun)
	mux.HandleFunc("POST /api/v1/log-runs/{id}/approve", a.postLogRunDecision(true))
	mux.HandleFunc("POST /api/v1/log-runs/{id}/deny", a.postLogRunDecision(false))
	mux.HandleFunc("POST /api/v1/log-runs/{id}/cancel", a.postLogRunCancel)
	mux.HandleFunc("GET /api/v1/settings/log-cleanup", a.getLogCleanupPolicy)
	mux.HandleFunc("PUT /api/v1/settings/log-cleanup", a.putLogCleanupPolicy)
}

type logTarget struct {
	asset Asset
	agent string
	view  LogsView
}

// logsFor reads a server's Logs tab.
func (a *API) logsFor(ctx context.Context, asset Asset) (logTarget, error) {
	t := logTarget{asset: asset}
	agent, err := a.store.AgentHostname(ctx, asset.Hostname)
	if err != nil {
		return t, err
	}
	t.agent = agent
	a.expireLogRuns(ctx, agent)
	v := LogsView{Hostname: asset.Hostname, Previews: []LogPreview{}, Runs: []LogCleanupRun{},
		Confirm: confirmName(asset.Hostname)}
	var n int
	if err := a.store.db(ctx).QueryRow(ctx, `SELECT count(*) FROM agents WHERE lower(hostname) = lower($1)`,
		agent).Scan(&n); err != nil {
		return t, err
	}
	v.Enrolled = n > 0
	report, st, asked, err := a.store.logReportFor(ctx, agent)
	if err != nil {
		return t, err
	}
	if st != nil {
		v.Reported, v.Supported, v.Reason, v.Scanning = true, st.Supported, st.Reason, st.Scanning
	}
	v.Report, v.ScanAsked = report, asked
	policy, err := a.store.LogCleanupPolicy(ctx)
	if err != nil {
		return t, err
	}
	v.Rule = policy.ruleFor(asset.Tags)
	if report != nil {
		now := time.Now()
		for _, it := range report.Items {
			p, _ := previewItem(it, v.Rule, now)
			v.Previews = append(v.Previews, p)
		}
	}
	if v.Runs, err = a.store.LogRunsFor(ctx, agent, 20); err != nil {
		return t, err
	}
	for i := range v.Runs {
		v.Runs[i].AssetID = asset.ID
	}
	t.view = v
	return t, nil
}

// logsReady says whether a cleanup can be asked of t now, answering the
// request itself when not.
func logsReady(w http.ResponseWriter, t logTarget) bool {
	switch v := t.view; {
	case t.asset.Protocol != "ssh" && t.asset.Protocol != "rdp":
		writeErr(w, http.StatusConflict, "log cleanup is for Linux and Windows servers")
	case !v.Enrolled:
		writeErr(w, http.StatusConflict, "this server has no enrolled agent; add it from Servers, Add a server")
	case !v.Reported:
		writeErr(w, http.StatusConflict, "this server's agent is too old to clean logs; "+
			"update the agent from its Settings tab first")
	case !v.Supported:
		writeErr(w, http.StatusConflict, "logs cannot be cleaned on this server from the console: "+v.Reason)
	default:
		return true
	}
	return false
}

func (a *API) getAssetLogs(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "log cleanup is for administrators, owners and auditors")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.logsFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "log cleanup", err)
		return
	}
	writeJSON(w, http.StatusOK, t.view)
}

func (a *API) postAssetLogsScan(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "scanning logs", "admin", "owner") {
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.logsFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "log cleanup", err)
		return
	}
	if !logsReady(w, t) {
		return
	}
	var id string
	err = a.store.db(r.Context()).QueryRow(r.Context(), `
		UPDATE agents SET logs_scan_id = COALESCE(logs_scan_id, gen_random_uuid()::text),
		       logs_scan_at = COALESCE(logs_scan_at, now())
		 WHERE lower(hostname) = lower($1) RETURNING logs_scan_id`, t.agent).Scan(&id)
	if err != nil {
		a.fail(w, "log scan", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"scanId": id})
}

// logRunRequest is a cleanup asked for in the console.
type logRunRequest struct {
	LogPick
	// Confirm is the server's short name, typed: files are removed for good.
	Confirm string `json:"confirm"`
}

func (a *API) postAssetLogRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "cleaning logs", "admin", "owner") {
		return
	}
	var in logRunRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(in.Items) > 100 || len(in.Open) > 50 || len(in.Restart) > 20 || len(in.EventLogs) > 4 {
		writeErr(w, http.StatusBadRequest, "pick fewer things to clean at once")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.logsFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "log cleanup", err)
		return
	}
	if !logsReady(w, t) {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(in.Confirm), t.view.Confirm) {
		writeErr(w, http.StatusBadRequest, "type "+t.view.Confirm+" to confirm: the files are removed for good")
		return
	}
	if t.view.Report == nil || t.view.Report.Error != "" && len(t.view.Report.Items) == 0 {
		writeErr(w, http.StatusConflict, "this server has not been scanned yet; scan it first")
		return
	}
	plan, err := planLogCleanup(*t.view.Report, t.view.Rule, in.LogPick, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	state := RunQueued
	if t.view.Rule.NeedsApproval {
		state = RunAwaitingApproval
	}
	run, err := a.store.CreateLogRun(r.Context(), newLogRun{Hostname: t.agent, AssetHostname: asset.Hostname,
		Trigger: "manual", State: state, Plan: plan, RequestedBy: sess.Email})
	if errors.Is(err, ErrLogRunActive) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "log cleanup", err)
		return
	}
	waits := ""
	if state == RunAwaitingApproval {
		waits = "; it waits for another admin to approve it"
	}
	a.auditAsset(r, AuditEvent{
		Action: "logs.clean_requested", Severity: "notice", ActorEmail: sess.Email, Target: asset.Hostname,
		Detail: fmt.Sprintf("Asked to clean logs on %s: %s, about %s%s.", asset.Hostname, plan.Summary,
			gbytes(plan.Bytes), waits),
	})
	if state == RunAwaitingApproval {
		a.raiseAlert(r.Context(), ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: sess.Email,
			Title:   sess.Email + " asks to clean logs on " + asset.Hostname,
			Detail:  plan.Summary + ", about " + gbytes(plan.Bytes) + ". Approve or deny it on the server's Logs tab.",
			Link:    "/assets/" + asset.ID + "?tab=logs",
			Subject: logRunSubject(run.ID),
		})
	}
	run.AssetID = asset.ID
	writeJSON(w, http.StatusAccepted, run)
}

func logRunSubject(id string) string { return "log-run:" + id }

func (a *API) getLogRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "log cleanup is for administrators, owners and auditors")
		return
	}
	run, err := a.store.LogRunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failLogRun(w, err)
		return
	}
	if id, _, _, err := a.store.assetForAgent(r.Context(), run.Hostname); err == nil {
		run.AssetID = id
	}
	writeJSON(w, http.StatusOK, run)
}

func (a *API) failLogRun(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrLogRunNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrRunState):
		writeErr(w, http.StatusConflict, strings.TrimPrefix(err.Error(), ErrRunState.Error()+": "))
	case errors.Is(err, ErrLogRunActive):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		a.fail(w, "log cleanup", err)
	}
}

func (a *API) postLogRunDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !a.privileged(w, sess, "approving log cleanups", "admin", "owner") {
			return
		}
		run, err := a.store.DecideLogRun(r.Context(), r.PathValue("id"), sess.Email, approve)
		if err != nil {
			a.failLogRun(w, err)
			return
		}
		action, verb := "logs.clean_approved", "Approved"
		if !approve {
			action, verb = "logs.clean_denied", "Denied"
		}
		a.auditAsset(r, AuditEvent{
			Action: action, Severity: "notice", ActorEmail: sess.Email, Target: run.AssetHostname,
			Detail: fmt.Sprintf("%s cleaning logs on %s (%s), asked by %s.", verb, run.AssetHostname, run.Summary,
				run.RequestedBy),
		})
		a.closeAlerts(r.Context(), logRunSubject(run.ID), verb+" by "+sess.Email)
		writeJSON(w, http.StatusOK, run)
	}
}

func (a *API) postLogRunCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "taking back log cleanups", "admin", "owner") {
		return
	}
	run, err := a.store.CancelLogRun(r.Context(), r.PathValue("id"), sess.Email)
	if err != nil {
		a.failLogRun(w, err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "logs.clean_cancelled", Severity: "notice", ActorEmail: sess.Email, Target: run.AssetHostname,
		Detail: fmt.Sprintf("Took back cleaning logs on %s (%s) before it started.", run.AssetHostname, run.Summary),
	})
	a.closeAlerts(r.Context(), logRunSubject(run.ID), "Taken back by "+sess.Email)
	writeJSON(w, http.StatusOK, run)
}

func (a *API) getLogCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "log cleanup is for administrators, owners and auditors")
		return
	}
	p, err := a.store.LogCleanupPolicy(r.Context())
	if err != nil {
		a.fail(w, "log cleanup rules", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) putLogCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing how logs are cleaned", "admin", "owner") {
		return
	}
	var in LogCleanupPolicy
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := a.store.LogCleanupPolicy(r.Context())
	if err != nil {
		a.fail(w, "log cleanup rules", err)
		return
	}
	if err := a.store.SaveLogCleanupPolicy(r.Context(), in); err != nil {
		a.fail(w, "log cleanup rules", err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "logs.policy_changed", Severity: "notice", ActorEmail: sess.Email, Target: "log cleanup",
		Detail: "Changed how logs are cleaned, from " + describeLogPolicy(before) + " to " + describeLogPolicy(in) + ".",
	})
	out, err := a.store.LogCleanupPolicy(r.Context())
	if err != nil {
		a.fail(w, "log cleanup rules", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func describeLogPolicy(p LogCleanupPolicy) string {
	parts := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		s := logRuleName(r) + ": "
		switch r.Mode {
		case LogCleanDisk:
			s += fmt.Sprintf("automatic past %d%%", r.DiskPercent)
		case LogCleanDaily:
			s += fmt.Sprintf("daily at %02d:%02d for %d min", r.WindowStartMinute/60, r.WindowStartMinute%60,
				r.WindowMinutes)
		default:
			s += "by hand only"
		}
		s += fmt.Sprintf(", rotated %dd, journal %dd/%dMB", r.RotatedKeepDays, r.JournalKeepDays, r.JournalMaxMB)
		if r.ActiveOverMB > 0 {
			s += fmt.Sprintf(", empty live logs over %dMB keeping %dMB", r.ActiveOverMB, r.ActiveKeepMB)
		}
		if r.NeedsApproval {
			s += ", needs approval"
		}
		parts = append(parts, s)
	}
	paths := "no extra paths"
	if len(p.Paths) > 0 {
		paths = "also cleaning " + strings.Join(p.Paths, ", ")
	}
	protect := "nothing extra kept"
	if len(p.Protect) > 0 {
		protect = "never cleaning " + strings.Join(p.Protect, ", ")
	}
	return "[" + strings.Join(parts, "; ") + "; " + paths + "; " + protect + "]"
}

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gsoultan/caduceus"
)

/* ── On a heartbeat ──────────────────────────────────────────────────────── */

// heartbeatPackages keeps what the agent on host said about packages,
// starts a run its rule's window calls for, and answers with what the agent
// should do. It returns nil for an agent older than package updates, which
// sent nothing and would not read an answer, and on a read that failed, so
// the agent keeps what it last heard rather than what an outage says.
func (a *API) heartbeatPackages(ctx context.Context, host string, b *PackageBeat) *PackageOrders {
	if b == nil {
		return nil
	}
	prev, err := a.store.recordPackageBeat(ctx, host, *b)
	if err != nil {
		a.log.Error("heartbeat: cannot keep package updates", "hostname", host, "error", err)
	}
	assetID, assetHost, tags, err := a.store.assetForAgent(ctx, host)
	if err != nil {
		if !errors.Is(err, caduceus.ErrNoRows) {
			a.log.Error("heartbeat: cannot find the server for package updates", "hostname", host, "error", err)
		}
		return nil
	}
	if b.Report != nil {
		a.alertNewSecurityUpdates(ctx, assetID, assetHost, prev, *b.Report)
	}
	if b.Run != nil {
		ended, err := a.store.recordPackageRunStatus(ctx, host, *b.Run)
		if err != nil {
			a.log.Error("heartbeat: cannot keep an update run's status", "hostname", host, "error", err)
		} else if ended != nil {
			a.packageRunEnded(ctx, *ended)
		}
	}
	if stale, err := a.store.expireStaleRuns(ctx, host); err != nil {
		a.log.Error("heartbeat: cannot end quiet update runs", "hostname", host, "error", err)
	} else {
		for _, r := range stale {
			a.packageRunEnded(ctx, r)
		}
	}
	policy, err := a.store.PackagePolicy(ctx)
	if err != nil {
		a.log.Error("heartbeat: cannot read the package update rules", "error", err)
		return nil
	}
	rule := policy.ruleFor(tags)
	if b.Supported {
		a.startAutomaticRun(ctx, host, assetHost, rule, policy.Holds, time.Now())
	}
	o := &PackageOrders{Check: rule.Mode != PackagesOff}
	if id, err := a.store.packageCheckAsked(ctx, host); err == nil {
		o.CheckID = id
	} else {
		a.log.Error("heartbeat: cannot read a package check asked for", "error", err)
	}
	run, err := a.store.packageRunToSend(ctx, host)
	if err != nil {
		a.log.Error("heartbeat: cannot read the update run to send", "error", err)
		return nil
	}
	if !a.attachScript(ctx, host, run) || !a.attachApp(ctx, host, run) || !a.attachFile(ctx, host, run) {
		run = nil
	}
	o.Run = run
	return o
}

// startAutomaticRun starts the run rule's window calls for on host, once
// per window: inside the window, with something the rule installs waiting,
// and nothing else going or waiting there.
func (a *API) startAutomaticRun(ctx context.Context, host, assetHost string, rule PackageRule,
	holds []string, now time.Time) {
	if rule.Mode != PackagesSecurity && rule.Mode != PackagesAll {
		return
	}
	report, _, err := a.store.packageReportFor(ctx, host)
	if err != nil || report == nil || !report.Supported {
		return
	}
	start, ok := rule.windowStart(now, report.UTCOffsetMinutes)
	if !ok || wanted(*report, rule.Mode, holds) == 0 {
		return
	}
	pending, err := a.store.hasPendingRun(ctx, a.store.db(ctx), host)
	if err != nil || pending {
		return
	}
	run, err := a.store.insertRun(ctx, a.store.db(ctx), newRun{
		Hostname: host, AssetHostname: assetHost, Mode: rule.Mode, Holds: holds,
		Reboot: rule.Reboot, Trigger: "automatic", State: RunQueued, RequestedBy: "",
		WindowStart: &start,
	})
	if errors.Is(err, ErrRunActive) {
		return // this window already ran, or a run started in between
	}
	if err != nil {
		a.log.Error("cannot start an automatic update run", "hostname", host, "error", err)
		return
	}
	what := "security updates"
	if rule.Mode == PackagesAll {
		what = "all updates"
	}
	reboot := "and not to reboot"
	if rule.Reboot == "if_needed" {
		reboot = "and to reboot if an update needs it"
	}
	a.auditPackages(ctx, AuditEvent{
		Action: "packages.run_started", Severity: "notice", Target: assetHost,
		Detail: fmt.Sprintf("Started installing %s on %s in its maintenance window, as its update rule (%s) says, %s.",
			what, assetHost, ruleName(rule), reboot),
	})
	a.log.Info("automatic update run started", "hostname", host, "run", run.ID, "mode", rule.Mode)
}

func ruleName(r PackageRule) string {
	if r.Tag == "" {
		return "every other server"
	}
	return "tag " + r.Tag
}

// alertNewSecurityUpdates tells admins when more security updates wait on
// a server than at the last look. The first report from a server is not
// news: it is how things already were.
func (a *API) alertNewSecurityUpdates(ctx context.Context, assetID, assetHost string, prev *PackageReport,
	now PackageReport) {
	if prev == nil || now.Error != "" {
		return
	}
	before, after := prev.securityCount(), now.securityCount()
	if after <= before {
		return
	}
	a.raiseAlert(ctx, ConsoleAlert{
		Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
		Title:  fmt.Sprintf("%s has %d security update%s waiting", assetHost, after, plural(after)),
		Detail: fmt.Sprintf("%d more than at the last look. Install them from the server's Updates tab.", after-before),
		Link:   "/assets/" + assetID + "?tab=updates",
	})
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// packageRunEnded records how a run ended, once: in the audit log, in the
// bell when it needs someone, and in its batch, which moves on or stops.
func (a *API) packageRunEnded(ctx context.Context, r PackageRun) {
	action, severity := "packages.run_finished", "notice"
	switch r.State {
	case RunFailed:
		action, severity = "packages.run_failed", "warn"
	case RunInterrupted:
		action, severity = "packages.run_interrupted", "warn"
	}
	who := "asked by " + r.RequestedBy
	if r.Trigger == "automatic" {
		who = "started by its update rule"
	}
	changed := "upgraded"
	switch r.Mode {
	case PackagesInstall:
		changed = "installed or changed"
	case PackagesRemove:
		changed = "removed"
	case PackagesScript:
		changed = "changed"
		a.scriptRunEnded(ctx, r)
	case AppInstall, AppUninstall, AppDisable, AppRestore, PackagesFileInstall:
		changed = "changed"
	}
	detail := fmt.Sprintf("The %s on %s (%s, %s) %s: %s %d package%s", runNoun(r), r.AssetHostname,
		describeMode(r), who, strings.ReplaceAll(r.State, "_", " "), changed, len(r.Upgraded), plural(len(r.Upgraded)))
	if r.Mode == PackagesReboot {
		detail = fmt.Sprintf("The reboot of %s (%s) %s", r.AssetHostname, who, strings.ReplaceAll(r.State, "_", " "))
	}
	if r.Mode == PackagesRelease {
		detail = fmt.Sprintf("The release upgrade of %s to %s (%s) %s", r.AssetHostname, r.Release, who,
			strings.ReplaceAll(r.State, "_", " "))
	}
	if r.Mode == PackagesPrepareRabbitMQ {
		detail = fmt.Sprintf("Preparing RabbitMQ on %s (%s) %s", r.AssetHostname, who,
			strings.ReplaceAll(r.State, "_", " "))
	}
	if isPostgresMode(r.Mode) {
		detail = fmt.Sprintf("%s on %s (%s) %s", upperFirst(runPhrase(r)), r.AssetHostname, who,
			strings.ReplaceAll(r.State, "_", " "))
	}
	if len(r.Started) > 0 {
		detail += ", started " + strings.Join(r.Started, ", ")
	}
	if len(r.Restarted) > 0 {
		detail += ", restarted " + strings.Join(r.Restarted, ", ")
	}
	if r.Rebooted {
		detail += ", rebooted"
	} else if r.RebootRequired {
		detail += ", needs a reboot"
	}
	if r.Message != "" {
		detail += ". " + strings.TrimSuffix(r.Message, ".")
	}
	a.auditPackages(ctx, AuditEvent{Action: action, Severity: severity, Target: r.AssetHostname, Detail: detail + "."})
	link := "/assets"
	if id, _, _, err := a.store.assetForAgent(ctx, r.Hostname); err == nil {
		link = "/assets/" + id + "?tab=" + runTab(r)
	}
	switch {
	case r.State == RunFailed || r.State == RunInterrupted:
		verb := map[string]string{PackagesInstall: "Installing on ", PackagesRemove: "Removing from ",
			PackagesScript: "A script on ", PackagesFileInstall: "Installing on ", PackagesReboot: "Rebooting ",
			PackagesRelease: "Upgrading the release of ", PackagesPrepareRabbitMQ: "Preparing RabbitMQ on ",
			PackagesPostgresUpgrade: "Upgrading PostgreSQL on ", PackagesPostgresDropOld: "Removing old PostgreSQL from ",
			PackagesAptSourcesFix: "Fixing apt sources on "}[r.Mode]
		if verb == "" {
			verb = "Updating "
		}
		title := verb + r.AssetHostname + " failed"
		if r.State == RunInterrupted {
			title = verb + r.AssetHostname + " was interrupted"
		}
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertServer, Severity: "warning", Audience: AudienceAdmins,
			Title: title, Detail: clip(r.Message, 1000), Link: link,
		})
	case r.RebootRequired && !r.Rebooted:
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertServer, Severity: "info", Audience: AudienceAdmins,
			Title:  r.AssetHostname + " needs a reboot",
			Detail: "Updates were installed and take effect after a reboot. Reboot it from its Updates tab.",
			Link:   link, Subject: rebootSubject(r.Hostname),
		})
	case r.Rebooted:
		a.closeAlerts(ctx, rebootSubject(r.Hostname), "Rebooted")
	}
	if r.BatchID != nil {
		if err := a.store.advanceBatch(ctx, *r.BatchID); err != nil {
			a.log.Error("cannot move an update batch along", "batch", *r.BatchID, "error", err)
		}
	}
}

func describeMode(r PackageRun) string {
	switch r.Mode {
	case PackagesAll:
		return "all updates"
	case PackagesSecurity:
		return "security updates"
	case PackagesReboot:
		return "a reboot"
	case PackagesRelease:
		return "release " + r.Release
	case PackagesPrepareRabbitMQ:
		return "RabbitMQ preparation"
	case PackagesPostgresUpgrade:
		if m := rePgUpgradeTarget.FindStringSubmatch(r.Target); m != nil {
			return "PostgreSQL " + m[1] + " " + m[2] + " to " + m[3]
		}
		return "a PostgreSQL upgrade"
	case PackagesPostgresDropOld:
		if m := rePgClusterTarget.FindStringSubmatch(r.Target); m != nil {
			return "old PostgreSQL " + m[1] + " " + m[2]
		}
		return "an old PostgreSQL cluster"
	case PackagesAptSourcesFix:
		return "apt sources fix"
	case PackagesInstall, PackagesRemove:
		return describeSoftware(r.Mode, r.Items, r.Purge)
	case PackagesScript:
		name := r.ScriptName
		if name == "" {
			name = "a script"
		}
		return r.ScriptAction + " script of " + name
	case AppInstall, AppUninstall, AppDisable, AppRestore:
		return describeApp(r)
	case PackagesFileInstall:
		name := r.SoftwareFileName
		if name == "" {
			name = "a library file"
		}
		return name
	}
	return "selected: " + strings.Join(r.Packages, ", ")
}

// runPhrase is what a run does, after a verb: "Approved installing all
// updates", "Approved removing nginx".
func runPhrase(r PackageRun) string {
	switch r.Mode {
	case PackagesInstall:
		return "installing " + describeMode(r)
	case PackagesRemove:
		return "removing " + describeMode(r)
	case PackagesScript:
		return "running the " + describeMode(r)
	case AppInstall, PackagesFileInstall:
		return "installing " + describeMode(r)
	case AppUninstall:
		return "uninstalling " + describeMode(r)
	case AppDisable:
		return "disabling " + describeMode(r)
	case AppRestore:
		return "restoring " + describeMode(r)
	case PackagesReboot:
		return "rebooting the server"
	case PackagesRelease:
		return "upgrading the server to release " + r.Release
	case PackagesPrepareRabbitMQ:
		return "preparing RabbitMQ for the next Ubuntu release"
	case PackagesPostgresUpgrade:
		return "upgrading " + describeMode(r)
	case PackagesPostgresDropOld:
		return "removing " + describeMode(r)
	case PackagesAptSourcesFix:
		return "tidying apt's sources"
	}
	return "installing " + describeMode(r)
}

// runNoun names a run in a sentence.
func runNoun(r PackageRun) string {
	switch r.Mode {
	case PackagesInstall:
		return "install"
	case PackagesRemove:
		return "removal"
	case PackagesScript:
		return "script run"
	case AppInstall:
		return "app install"
	case PackagesFileInstall:
		return "install"
	case AppUninstall:
		return "app uninstall"
	case AppDisable, AppRestore:
		return "app change"
	case PackagesReboot:
		return "reboot"
	case PackagesRelease:
		return "release upgrade"
	case PackagesPrepareRabbitMQ:
		return "RabbitMQ preparation"
	case PackagesPostgresUpgrade:
		return "PostgreSQL upgrade"
	case PackagesPostgresDropOld:
		return "old PostgreSQL removal"
	case PackagesAptSourcesFix:
		return "apt sources fix"
	}
	return "update run"
}

// runTab is the server tab a run is shown on.
func runTab(r PackageRun) string {
	if r.Mode == PackagesInstall || r.Mode == PackagesRemove || r.Mode == PackagesScript || isAppMode(r.Mode) ||
		r.Mode == PackagesFileInstall {
		return "apps"
	}
	return "updates"
}

func (a *API) auditPackages(ctx context.Context, e AuditEvent) {
	if _, err := a.store.AppendAudit(ctx, e); err != nil {
		a.log.Error("audit append failed", "error", err, "action", e.Action)
	}
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) registerPackageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/assets/{id}/packages", a.getAssetPackages)
	mux.HandleFunc("POST /api/v1/assets/{id}/packages/check", a.postAssetPackagesCheck)
	mux.HandleFunc("POST /api/v1/assets/{id}/packages/runs", a.postAssetPackageRun)
	mux.HandleFunc("GET /api/v1/package-updates", a.getPackageFleet)
	mux.HandleFunc("POST /api/v1/package-runs", a.postPackageRuns)
	mux.HandleFunc("GET /api/v1/package-runs/{id}", a.getPackageRun)
	mux.HandleFunc("POST /api/v1/package-runs/{id}/approve", a.postPackageRunDecision(true))
	mux.HandleFunc("POST /api/v1/package-runs/{id}/deny", a.postPackageRunDecision(false))
	mux.HandleFunc("POST /api/v1/package-runs/{id}/cancel", a.postPackageRunCancel)
	mux.HandleFunc("GET /api/v1/settings/package-updates", a.getPackagePolicy)
	mux.HandleFunc("PUT /api/v1/settings/package-updates", a.putPackagePolicy)
}

// packageTarget is a server as runs see it.
type packageTarget struct {
	asset Asset
	agent string
	view  PackagesView
}

// packagesFor reads a server's Updates tab.
func (a *API) packagesFor(ctx context.Context, asset Asset) (packageTarget, error) {
	t := packageTarget{asset: asset}
	agent, err := a.store.AgentHostname(ctx, asset.Hostname)
	if err != nil {
		return t, err
	}
	t.agent = agent
	if stale, err := a.store.expireStaleRuns(ctx, agent); err == nil {
		for _, r := range stale {
			a.packageRunEnded(ctx, r)
		}
	}
	v := PackagesView{Hostname: asset.Hostname, Holds: []string{}, Runs: []PackageRun{}}
	var checkAsked *string
	var heard time.Time
	err = a.store.db(ctx).QueryRow(ctx, `SELECT packages_check_id, last_seen_at FROM agents WHERE lower(hostname) = lower($1)`,
		agent).Scan(&checkAsked, &heard)
	switch {
	case errors.Is(err, caduceus.ErrNoRows):
	case err != nil:
		return t, err
	default:
		v.Enrolled = true
		v.CheckAsked = checkAsked != nil
		v.HeardAt = &heard
	}
	report, st, err := a.store.packageReportFor(ctx, agent)
	if err != nil {
		return t, err
	}
	if st != nil {
		v.Reported, v.Supported, v.Reason, v.Checking, v.Installs = true, st.Supported, st.Reason, st.Checking, st.Installs
		v.CanReboot = st.Reboot
		v.CanUpgradeRelease = st.Release
	}
	v.Report = report
	policy, err := a.store.PackagePolicy(ctx)
	if err != nil {
		return t, err
	}
	v.Rule, v.Holds = policy.ruleFor(asset.Tags), policy.Holds
	offset := 0
	if report != nil {
		offset = report.UTCOffsetMinutes
	}
	v.NextWindow = v.Rule.nextWindow(time.Now(), offset)
	if v.Runs, err = a.store.PackageRunsFor(ctx, agent, 20); err != nil {
		return t, err
	}
	for i := range v.Runs {
		v.Runs[i].AssetID = asset.ID
	}
	t.view = v
	return t, nil
}

// rebootReady refuses a reboot an agent too old to do one would fail, and
// one on a server dpkg has left half configured, which may not come back.
func rebootReady(w http.ResponseWriter, t packageTarget, in runRequest) bool {
	if in.Mode == PackagesReboot && !t.view.CanReboot {
		writeErr(w, http.StatusConflict, "this server's agent is too old to reboot from the console; "+
			"update the agent from its Settings tab first")
		return false
	}
	if in.Mode == PackagesReboot {
		if why := unfinishedRefusal(t.view.Report); why != "" {
			writeErr(w, http.StatusConflict, why)
			return false
		}
	}
	return true
}

// unfinishedRefusal is why a server must not be rebooted now, from its last
// report, or "".
func unfinishedRefusal(r *PackageReport) string {
	if r == nil || len(r.Unfinished) == 0 {
		return ""
	}
	return "do not reboot this server yet: dpkg has left " + strings.Join(r.Unfinished, ", ") + " half " +
		"installed or half configured, and it may not start again. On the server, run `sudo dpkg --configure -a` " +
		"and `sudo apt-get -f install` until both finish without errors (and `sudo apt full-upgrade` if a release " +
		"upgrade stopped part way), then press Check for updates"
}

// releaseReady refuses a release upgrade the server is not ready for, or
// that is not the one it offers: the agent would only refuse it later.
func releaseReady(w http.ResponseWriter, t packageTarget, in runRequest) bool {
	if in.Mode != PackagesRelease {
		return true
	}
	var rel *ReleaseInfo
	if t.view.Report != nil {
		rel = t.view.Report.Release
	}
	switch {
	case !t.view.CanUpgradeRelease:
		writeErr(w, http.StatusConflict, "this server's agent is too old to upgrade the operating system from "+
			"the console; update the agent from its Settings tab first")
	case rel == nil || rel.Reason != "" || rel.Next == "":
		why := "the server does not offer a newer release"
		if rel != nil && rel.Reason != "" {
			why = rel.Reason
		}
		writeErr(w, http.StatusConflict, "no release upgrade is offered for this server: "+why)
	case rel.Next != in.Release:
		writeErr(w, http.StatusConflict, fmt.Sprintf("the server offers %s, not %s; look for updates again", rel.Next,
			in.Release))
	case len(rel.Blockers) > 0:
		writeErr(w, http.StatusConflict, "the server is not ready for "+rel.NextName+": "+strings.Join(rel.Blockers, "; "))
	default:
		return true
	}
	return false
}

// rabbitMQReady refuses preparing RabbitMQ where the agent did not say it
// can: an older agent, another release, or RabbitMQ already ready.
func rabbitMQReady(w http.ResponseWriter, t packageTarget, in runRequest) bool {
	if in.Mode != PackagesPrepareRabbitMQ {
		return true
	}
	if t.view.Report == nil || t.view.Report.Release == nil || !t.view.Report.Release.PrepareRabbitMQ {
		writeErr(w, http.StatusConflict, "this server does not offer to prepare RabbitMQ: it is ready already, "+
			"is not on Ubuntu 24.04, or its agent is too old; press Check for updates and look at the release card")
		return false
	}
	return true
}

var (
	rePgUpgradeTarget = regexp.MustCompile(`^([0-9]{1,3})/([A-Za-z0-9_.-]{1,64}):([0-9]{1,3})$`)
	rePgClusterTarget = regexp.MustCompile(`^([0-9]{1,3})/([A-Za-z0-9_.-]{1,64})$`)
	reFingerprint     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// postgresReady refuses a PostgreSQL or apt sources run the agent's latest
// report does not offer as asked: an older agent, something that changed
// since the console showed it, or something standing in the way.
func postgresReady(w http.ResponseWriter, t packageTarget, in runRequest) bool {
	if !isPostgresMode(in.Mode) {
		return true
	}
	var pg *PostgresUpkeep
	var src *AptSourcesFix
	if r := t.view.Report; r != nil {
		pg, src = r.Postgres, r.AptSources
	}
	again := "; press Check for updates and look at the card again"
	switch in.Mode {
	case PackagesPostgresUpgrade:
		switch {
		case pg == nil || pg.Upgrade == nil:
			writeErr(w, http.StatusConflict, "this server does not offer to upgrade PostgreSQL"+again)
		case pg.Upgrade.expect() != in.Expect:
			writeErr(w, http.StatusConflict, fmt.Sprintf("the server offers PostgreSQL %s %s to %s, not %s%s",
				pg.Upgrade.From, pg.Upgrade.Cluster, pg.Upgrade.To, in.Expect, again))
		case len(pg.Upgrade.Blockers) > 0:
			writeErr(w, http.StatusConflict, "PostgreSQL cannot be upgraded yet: "+strings.Join(pg.Upgrade.Blockers, "; "))
		default:
			return true
		}
	case PackagesPostgresDropOld:
		if pg == nil || pg.OldCluster == nil || pg.OldCluster.expect() != in.Expect {
			writeErr(w, http.StatusConflict, "this server has no stopped PostgreSQL "+strings.Replace(in.Expect, "/", " ", 1)+
				" left by an upgrade"+again)
			return false
		}
		return true
	case PackagesAptSourcesFix:
		if src == nil || src.Fingerprint != in.Expect {
			writeErr(w, http.StatusConflict, "apt's sources on this server changed since they were shown"+again)
			return false
		}
		return true
	}
	return false
}

// packagesReady says whether a run can be asked of t now, answering the
// request itself when not.
func packagesReady(w http.ResponseWriter, t packageTarget) bool {
	switch v := t.view; {
	case t.asset.Protocol != "ssh":
		writeErr(w, http.StatusConflict, "operating system updates are for Linux servers reached over SSH")
	case !v.Enrolled:
		writeErr(w, http.StatusConflict, "this server has no enrolled agent; add it from Servers, Add a server")
	case !v.Reported:
		writeErr(w, http.StatusConflict, "this server's agent is too old to update packages; "+
			"update the agent from its Settings tab first")
	case !v.Supported:
		writeErr(w, http.StatusConflict, "this server cannot be updated from the console: "+v.Reason)
	default:
		return true
	}
	return false
}

func (a *API) getAssetPackages(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "operating system updates are for administrators, owners and auditors")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.packagesFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "package updates", err)
		return
	}
	writeJSON(w, http.StatusOK, t.view)
}

func (a *API) postAssetPackagesCheck(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "looking for updates", "admin", "owner") {
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.packagesFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "package updates", err)
		return
	}
	if !packagesReady(w, t) {
		return
	}
	if t.view.Rule.Mode == PackagesOff {
		writeErr(w, http.StatusConflict, "updates are off for this server's rule; turn them on in "+
			"Settings, Updates to look for them")
		return
	}
	var id string
	err = a.store.db(r.Context()).QueryRow(r.Context(), `
		UPDATE agents SET packages_check_id = COALESCE(packages_check_id, gen_random_uuid()::text),
		       packages_check_at = COALESCE(packages_check_at, now())
		 WHERE lower(hostname) = lower($1) RETURNING packages_check_id`, t.agent).Scan(&id)
	if err != nil {
		a.fail(w, "package check", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"checkId": id})
}

// runRequest is a run asked for in the console.
type runRequest struct {
	Mode     string   `json:"mode"`
	Packages []string `json:"packages"`
	Reboot   string   `json:"reboot"`
	// Release is the version a release upgrade goes to, as the console
	// showed it.
	Release string `json:"release"`
	// Expect is what a PostgreSQL or apt sources run is for, as the console
	// showed it: "17/main:19", "17/main", or the sources' fingerprint.
	Expect string `json:"expect"`
}

// reRelease is a VERSION_ID: 24.04, 13, 9.
var reRelease = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3})?$`)

func (in *runRequest) validate() error {
	switch in.Mode {
	case PackagesAll, PackagesSecurity:
		in.Packages = nil
	case PackagesReboot:
		// A reboot is the whole run: whatever was said about rebooting.
		in.Packages, in.Reboot, in.Release = nil, "if_needed", ""
		return nil
	case PackagesPrepareRabbitMQ:
		in.Packages, in.Reboot, in.Release, in.Expect = nil, "never", "", ""
		return nil
	case PackagesPostgresUpgrade, PackagesPostgresDropOld, PackagesAptSourcesFix:
		ok := map[string]*regexp.Regexp{PackagesPostgresUpgrade: rePgUpgradeTarget,
			PackagesPostgresDropOld: rePgClusterTarget, PackagesAptSourcesFix: reFingerprint}[in.Mode]
		if !ok.MatchString(in.Expect) {
			return errors.New("say what this run is for, as the server's Updates tab shows it")
		}
		in.Packages, in.Reboot, in.Release = nil, "never", ""
		return nil
	case PackagesRelease:
		// A new release always ends in a reboot into it.
		if !reRelease.MatchString(in.Release) {
			return errors.New("say which release to upgrade to, as the server offers it")
		}
		in.Packages, in.Reboot, in.Expect = nil, "if_needed", ""
		return nil
	case "selected":
		if len(in.Packages) == 0 {
			return errors.New("pick at least one package to install")
		}
		if len(in.Packages) > 500 {
			return errors.New("pick at most five hundred packages; install everything instead")
		}
		for _, p := range in.Packages {
			if !rePkgName.MatchString(p) {
				return fmt.Errorf("%q is not a package name", p)
			}
		}
		in.Packages = dedupe(in.Packages)
	default:
		return errors.New("say what to do: all, security, selected, reboot, release_upgrade, prepare_rabbitmq, " +
			"postgres_upgrade, postgres_drop_old or apt_sources_fix")
	}
	in.Release, in.Expect = "", ""
	switch in.Reboot {
	case "", "never":
		in.Reboot = "never"
	case "if_needed":
	default:
		return errors.New("reboot is never or if_needed")
	}
	return nil
}

func (a *API) newManualRun(ctx context.Context, t packageTarget, in runRequest, by string, dr directRuns) newRun {
	n := newRun{
		Hostname: t.agent, AssetHostname: t.asset.Hostname, Mode: in.Mode, Packages: in.Packages,
		Holds: t.view.Holds, Reboot: in.Reboot, Trigger: "manual", State: RunQueued, RequestedBy: by,
	}
	// Naming a package is choosing it: a hold keeps a package out of
	// "all" and "security", not out of a pick made by hand.
	if in.Mode == "selected" {
		n.Holds = []string{}
	}
	// A new release replaces every package: holds cannot keep one back,
	// and the console says so before anyone asks.
	if in.Mode == PackagesRelease {
		n.Holds, n.Expect = []string{}, in.Release
	}
	if in.Mode == PackagesPrepareRabbitMQ {
		n.Holds = []string{}
	}
	if isPostgresMode(in.Mode) {
		n.Holds, n.Expect = []string{}, in.Expect
	}
	if a.runWaits(ctx, t.view.Rule.NeedsApproval, KindServerRuns, t.asset.Tags, by, t.asset.Hostname, dr) {
		n.State = RunAwaitingApproval
	}
	return n
}

func (a *API) postAssetPackageRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing updates", "admin", "owner") {
		return
	}
	var in runRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.packagesFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "package updates", err)
		return
	}
	if !packagesReady(w, t) || !rebootReady(w, t, in) || !releaseReady(w, t, in) || !rabbitMQReady(w, t, in) ||
		!postgresReady(w, t, in) {
		return
	}
	dr := directRuns{}
	runs, err := a.store.CreatePackageRuns(r.Context(), []newRun{a.newManualRun(r.Context(), t, in, sess.Email, dr)})
	if errors.Is(err, ErrRunActive) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "update run", err)
		return
	}
	a.packageRunsAsked(r, sess.Email, runs, map[string]string{asset.Hostname: asset.ID})
	a.recordDirectPackageRuns(r.Context(), dr, sess.Email, runs)
	writeJSON(w, http.StatusAccepted, runs[0])
}

// postPackageRuns updates several servers, one at a time.
func (a *API) postPackageRuns(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing updates", "admin", "owner") {
		return
	}
	var in struct {
		AssetIDs []string `json:"assetIds"`
		Mode     string   `json:"mode"`
		Reboot   string   `json:"reboot"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req := runRequest{Mode: in.Mode, Reboot: in.Reboot}
	if in.Mode == "selected" {
		writeErr(w, http.StatusBadRequest, "pick packages on one server at a time; for several, install all or security updates")
		return
	}
	if isPostgresMode(in.Mode) {
		writeErr(w, http.StatusBadRequest, "fix PostgreSQL and apt sources one server at a time, from its Updates tab")
		return
	}
	if in.Mode == PackagesRelease || in.Mode == PackagesPrepareRabbitMQ {
		writeErr(w, http.StatusBadRequest, "upgrade the release of one server at a time, from its Updates tab")
		return
	}
	if err := req.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// In the order given: it is the order they are updated in.
	ids := []string{}
	seen := map[string]bool{}
	for _, id := range in.AssetIDs {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > 200 {
		writeErr(w, http.StatusBadRequest, "pick between one and two hundred servers")
		return
	}
	var (
		runs  []newRun
		links = map[string]string{}
		dr    = directRuns{}
	)
	for _, id := range ids {
		asset, err := a.store.AssetByID(r.Context(), id)
		if err != nil {
			a.failAsset(w, "asset", err)
			return
		}
		t, err := a.packagesFor(r.Context(), asset)
		if err != nil {
			a.fail(w, "package updates", err)
			return
		}
		if !t.view.Supported || !t.view.Reported || !t.view.Enrolled || asset.Protocol != "ssh" {
			reason := t.view.Reason
			if reason == "" {
				reason = "its agent cannot update packages yet"
			}
			writeErr(w, http.StatusConflict, asset.Hostname+": "+reason)
			return
		}
		if req.Mode == PackagesReboot && !t.view.CanReboot {
			writeErr(w, http.StatusConflict, asset.Hostname+": its agent is too old to reboot from the console; "+
				"update the agent from its Settings tab first")
			return
		}
		if req.Mode == PackagesReboot {
			if why := unfinishedRefusal(t.view.Report); why != "" {
				writeErr(w, http.StatusConflict, asset.Hostname+": "+why)
				return
			}
		}
		runs = append(runs, a.newManualRun(r.Context(), t, req, sess.Email, dr))
		links[asset.Hostname] = asset.ID
	}
	out, err := a.store.CreatePackageRuns(r.Context(), runs)
	if errors.Is(err, ErrRunActive) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "update runs", err)
		return
	}
	a.packageRunsAsked(r, sess.Email, out, links)
	a.recordDirectPackageRuns(r.Context(), dr, sess.Email, out)
	writeJSON(w, http.StatusAccepted, out)
}

// recordDirectPackageRuns records the runs the approval policy let through.
func (a *API) recordDirectPackageRuns(ctx context.Context, dr directRuns, by string, runs []PackageRun) {
	for _, run := range runs {
		dr.record(ctx, a, by, run.AssetHostname, "Ran "+describeMode(run)+" on "+run.AssetHostname+" (run "+run.ID+")")
	}
}

// packageRunsAsked audits runs asked for and asks for the approvals they
// wait on.
func (a *API) packageRunsAsked(r *http.Request, by string, runs []PackageRun, links map[string]string) {
	for _, run := range runs {
		reboot := "not to reboot"
		if run.Reboot == "if_needed" {
			reboot = "to reboot if an update needs it"
		}
		order := ""
		if run.BatchPosition != nil {
			order = fmt.Sprintf(", %d of %d servers updated one at a time", *run.BatchPosition+1, len(runs))
		}
		waits := ""
		if run.State == RunAwaitingApproval {
			waits = "; it waits for another admin to approve it"
		}
		detail := fmt.Sprintf("Asked to install %s on %s, %s%s%s.", describeMode(run), run.AssetHostname,
			reboot, order, waits)
		title, ask := by+" asks to update "+run.AssetHostname, "Install "+describeMode(run)
		if run.Mode == PackagesReboot {
			order = strings.Replace(order, "updated", "rebooted", 1)
			detail = fmt.Sprintf("Asked to reboot %s%s%s.", run.AssetHostname, order, waits)
			title, ask = by+" asks to reboot "+run.AssetHostname, "Reboot it"
		}
		if run.Mode == PackagesRelease {
			detail = fmt.Sprintf("Asked to upgrade %s to release %s and reboot into it%s.", run.AssetHostname,
				run.Release, waits)
			title = by + " asks to upgrade " + run.AssetHostname + " to release " + run.Release
			ask = "Upgrade the operating system to release " + run.Release + " and reboot"
		}
		if isPostgresMode(run.Mode) {
			what := map[string]string{
				PackagesPostgresUpgrade: "upgrade " + describeMode(run) + " (a backup first, the old cluster kept stopped)",
				PackagesPostgresDropOld: "drop " + describeMode(run) + " and remove its packages",
				PackagesAptSourcesFix:   "move aside the files apt ignores and point apt.postgresql.org at the server's release",
			}[run.Mode]
			detail = fmt.Sprintf("Asked to %s on %s%s.", what, run.AssetHostname, waits)
			title = by + " asks for " + runPhrase(run) + " on " + run.AssetHostname
			ask = upperFirst(what)
		}
		if run.Mode == PackagesPrepareRabbitMQ {
			detail = fmt.Sprintf("Asked to prepare RabbitMQ on %s for the next Ubuntu release (3.13 from "+
				"Canonical's PPA, its feature flags on)%s.", run.AssetHostname, waits)
			title = by + " asks to prepare RabbitMQ on " + run.AssetHostname
			ask = "Upgrade RabbitMQ to 3.13 and enable its feature flags"
		}
		a.auditAsset(r, AuditEvent{
			Action: "packages.run_requested", Severity: "notice", ActorEmail: by, Target: run.AssetHostname,
			Detail: detail,
		})
		if run.State == RunAwaitingApproval {
			a.raiseAlert(r.Context(), ConsoleAlert{
				Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: by,
				Title:   title,
				Detail:  ask + ". Approve or deny it on the server's Updates tab.",
				Link:    "/assets/" + links[run.AssetHostname] + "?tab=updates",
				Subject: packageRunSubject(run.ID),
			})
		}
	}
}

func packageRunSubject(id string) string { return "package-run:" + id }

// rebootSubject is what a server's "needs a reboot" alert is about, so a
// reboot closes it.
func rebootSubject(agent string) string { return "package-reboot:" + strings.ToLower(agent) }

func (a *API) getPackageRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "operating system updates are for administrators, owners and auditors")
		return
	}
	run, err := a.store.PackageRunByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrRunNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "update run", err)
		return
	}
	if id, _, _, err := a.store.assetForAgent(r.Context(), run.Hostname); err == nil {
		run.AssetID = id
	}
	writeJSON(w, http.StatusOK, run)
}

func (a *API) failRun(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrRunNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrRunState):
		writeErr(w, http.StatusConflict, strings.TrimPrefix(err.Error(), ErrRunState.Error()+": "))
	case errors.Is(err, ErrRunActive):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		a.fail(w, "update run", err)
	}
}

func (a *API) postPackageRunDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !a.privileged(w, sess, "approving updates", "admin", "owner") {
			return
		}
		run, err := a.store.DecidePackageRun(r.Context(), r.PathValue("id"), sess.Email, approve, false)
		if errors.Is(err, ErrOwnRequest) {
			// Theirs: approved only when nobody else could (self_approval.go).
			confirm, cerr := readConfirmSelf(r)
			if cerr != nil {
				writeErr(w, http.StatusBadRequest, cerr.Error())
				return
			}
			if !a.approveOwn(w, r, sess.Email, approve, runDeciders, false, confirm,
				strings.TrimPrefix(err.Error(), ErrRunState.Error()+": "), "update run", run.AssetHostname) {
				return
			}
			run, err = a.store.DecidePackageRun(r.Context(), r.PathValue("id"), sess.Email, approve, true)
		}
		if err != nil {
			a.failRun(w, err)
			return
		}
		action, verb := "packages.run_approved", "Approved"
		if !approve {
			action, verb = "packages.run_denied", "Denied"
		}
		a.auditAsset(r, AuditEvent{
			Action: action, Severity: "notice", ActorEmail: sess.Email, Target: run.AssetHostname,
			Detail: fmt.Sprintf("%s %s on %s, asked by %s.", verb, runPhrase(run),
				run.AssetHostname, run.RequestedBy),
		})
		a.closeAlerts(r.Context(), packageRunSubject(run.ID), verb+" by "+sess.Email)
		writeJSON(w, http.StatusOK, run)
	}
}

func (a *API) postPackageRunCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "taking back updates", "admin", "owner") {
		return
	}
	run, err := a.store.CancelPackageRun(r.Context(), r.PathValue("id"), sess.Email)
	if err != nil {
		a.failRun(w, err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "packages.run_cancelled", Severity: "notice", ActorEmail: sess.Email, Target: run.AssetHostname,
		Detail: fmt.Sprintf("Took back %s on %s before it started.", runPhrase(run), run.AssetHostname),
	})
	a.closeAlerts(r.Context(), packageRunSubject(run.ID), "Taken back by "+sess.Email)
	writeJSON(w, http.StatusOK, run)
}

func (a *API) getPackageFleet(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "operating system updates are for administrators, owners and auditors")
		return
	}
	if stale, err := a.store.expireStaleRuns(r.Context(), ""); err == nil {
		for _, run := range stale {
			a.packageRunEnded(r.Context(), run)
		}
	}
	rows, err := a.store.PackageFleet(r.Context())
	if err != nil {
		a.fail(w, "package updates", err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (a *API) getPackagePolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "operating system updates are for administrators, owners and auditors")
		return
	}
	p, err := a.store.PackagePolicy(r.Context())
	if err != nil {
		a.fail(w, "package update rules", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) putPackagePolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing how servers are updated", "admin", "owner") {
		return
	}
	var in PackagePolicy
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := a.store.PackagePolicy(r.Context())
	if err != nil {
		a.fail(w, "package update rules", err)
		return
	}
	if err := a.store.SavePackagePolicy(r.Context(), in); err != nil {
		a.fail(w, "package update rules", err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "packages.policy_changed", Severity: "notice", ActorEmail: sess.Email, Target: "package updates",
		Detail: "Changed how servers are updated, from " + describePolicy(before) + " to " + describePolicy(in) + ".",
	})
	out, err := a.store.PackagePolicy(r.Context())
	if err != nil {
		a.fail(w, "package update rules", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var weekdays = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func describePolicy(p PackagePolicy) string {
	parts := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		s := ruleName(r) + ": " + r.Mode
		if r.Mode == PackagesSecurity || r.Mode == PackagesAll {
			days := make([]string, 0, len(r.WindowDays))
			for _, d := range r.WindowDays {
				days = append(days, weekdays[d])
			}
			s += fmt.Sprintf(" %s %02d:%02d for %d min, reboot %s", strings.Join(days, "/"),
				r.WindowStartMinute/60, r.WindowStartMinute%60, r.WindowMinutes, r.Reboot)
		}
		if r.NeedsApproval {
			s += ", runs by hand need approval"
		}
		parts = append(parts, s)
	}
	holds := "no holds"
	if len(p.Holds) > 0 {
		holds = "holding " + strings.Join(p.Holds, ", ")
	}
	return "[" + strings.Join(parts, "; ") + "; " + holds + "]"
}

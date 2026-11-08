package control

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Many servers at once.

From the library, an admin picks a reviewed script, a Windows installer or an
APK and the servers (or devices) to put it on. Argus makes one run per server
in one batch, the way an update goes across a fleet: one server at a time,
and a failure stops the servers after it, so a bad script breaks one server,
not all of them.

Servers outside production go first. A production server in the batch runs
only after those before it finished, so "it must have worked outside
production first" holds inside a batch too; a batch of production servers
alone needs the script or file to have worked somewhere else already.

Servers that cannot take it (wrong platform, no agent, a change already
going) are left out and named, and the rest go ahead. Packages from a
server's own repositories are not batched: each server's preview is its own
package manager's answer, so they stay one server at a time.
*/

// maxBatch bounds one batch.
const maxBatch = 100

// BatchSkipped is a server left out of a batch, and why.
type BatchSkipped struct {
	AssetID  string `json:"assetId"`
	Hostname string `json:"hostname"`
	Reason   string `json:"reason"`
}

// BatchResult is what a batch made.
type BatchResult struct {
	Runs    []PackageRun   `json:"runs"`
	Skipped []BatchSkipped `json:"skipped"`
}

type batchTarget struct {
	t    packageTarget
	prod bool
}

// POST /api/v1/software/batches {kind, versionId?, action?, params?, fileId?, assetIds}
func (a *API) postSoftwareBatch(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing software", "admin", "owner") {
		return
	}
	var in struct {
		Kind      string            `json:"kind"`
		VersionID string            `json:"versionId"`
		Action    string            `json:"action"`
		Params    map[string]string `json:"params"`
		FileID    string            `json:"fileId"`
		AssetIDs  []string          `json:"assetIds"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case len(in.AssetIDs) == 0:
		writeErr(w, http.StatusBadRequest, "pick at least one server")
		return
	case len(in.AssetIDs) > maxBatch:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("at most %d servers at a time", maxBatch))
		return
	}
	ctx := r.Context()

	// What goes on them.
	var (
		v        ScriptVersion
		file     SoftwareFile
		what     string
		tried    bool
		plain    map[string]string
		sealed   []byte
		platform string
	)
	switch in.Kind {
	case "script":
		if in.Action == "" {
			in.Action = tunnel.ScriptInstall
		}
		if in.Action != tunnel.ScriptInstall && in.Action != tunnel.ScriptUninstall {
			writeErr(w, http.StatusBadRequest, "action is install or uninstall")
			return
		}
		var err error
		if v, err = a.store.scriptVersion(ctx, in.VersionID); err != nil {
			a.failScript(w, err)
			return
		}
		switch {
		case v.archived:
			writeErr(w, http.StatusConflict, "this script was taken out of the library")
			return
		case v.signature == "":
			writeErr(w, http.StatusConflict, fmt.Sprintf("%s v%d has not been reviewed", v.scriptName, v.Version))
			return
		case in.Action == tunnel.ScriptUninstall && v.Uninstall == "":
			writeErr(w, http.StatusConflict, fmt.Sprintf("%s v%d has no uninstall script", v.scriptName, v.Version))
			return
		}
		var secret map[string]string
		if plain, secret, err = scriptValues(v.Params, in.Params); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(secret) > 0 {
			if sealed, err = a.sealSecrets(ctx, secret); err != nil {
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
		}
		platform, tried = v.platform, v.TriedOn != nil
		what = fmt.Sprintf("the %s script of %s v%d", in.Action, v.scriptName, v.Version)
	case "file":
		var err error
		file, err = a.store.softwareFile(ctx, in.FileID)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "no such file in the library")
			return
		}
		if err != nil {
			a.fail(w, "software file", err)
			return
		}
		platform, what = file.Platform, file.Name
		if file.Platform == "windows" {
			if tried, err = a.store.fileTriedOutsideProduction(ctx, file.ID); err != nil {
				a.fail(w, "software file", err)
				return
			}
		} else {
			tried = true // devices are not tagged production
			if file.App == nil {
				writeErr(w, http.StatusBadRequest, "that library file cannot be installed")
				return
			}
			if why, ok := protectedApps[file.App.Package]; ok {
				writeErr(w, http.StatusConflict, file.App.Package+" is protected ("+why+"); it is not installed from the library")
				return
			}
		}
	default:
		writeErr(w, http.StatusBadRequest, "kind is script or file")
		return
	}

	// Who can take it.
	var targets []batchTarget
	var skipped []BatchSkipped
	seen := map[string]bool{}
	for _, id := range in.AssetIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		asset, err := a.store.AssetByID(ctx, id)
		if err != nil {
			skipped = append(skipped, BatchSkipped{AssetID: id, Reason: "no such server"})
			continue
		}
		skip := func(why string) {
			skipped = append(skipped, BatchSkipped{AssetID: id, Hostname: asset.Hostname, Reason: why})
		}
		t, err := a.packagesFor(ctx, asset)
		if err != nil {
			a.fail(w, "software batch", err)
			return
		}
		if why := batchReady(t, platform); why != "" {
			skip(why)
			continue
		}
		busy, err := a.store.hasPendingRun(ctx, a.store.db(ctx), t.agent)
		if err != nil {
			a.fail(w, "software batch", err)
			return
		}
		if busy {
			skip("already has a change waiting or running")
			continue
		}
		targets = append(targets, batchTarget{t: t, prod: isProduction(asset.Tags)})
	}
	sort.SliceStable(targets, func(i, j int) bool { return !targets[i].prod && targets[j].prod })
	if len(targets) > 0 && targets[0].prod && !tried {
		for _, x := range targets {
			skipped = append(skipped, BatchSkipped{AssetID: x.t.asset.ID, Hostname: x.t.asset.Hostname,
				Reason: "production, and " + what + " has not worked on a server outside production yet; add one to the batch"})
		}
		targets = nil
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "none of the servers picked can take it", "skipped": skipped})
		return
	}

	runs := make([]newRun, 0, len(targets))
	approval := false
	for _, x := range targets {
		n := newRun{
			Hostname: x.t.agent, AssetHostname: x.t.asset.Hostname, Packages: []string{}, Holds: []string{},
			Reboot: "never", Trigger: "manual", State: RunQueued, RequestedBy: sess.Email,
		}
		switch {
		case in.Kind == "script":
			n.Mode, n.ScriptVersionID, n.ScriptAction, n.ScriptParams, n.ScriptSecrets = PackagesScript, &v.ID, in.Action, plain, sealed
			if v.Reboot == "if_asked" {
				n.Reboot = "if_needed"
			}
		case file.Platform == "windows":
			n.Mode, n.SoftwareFileID, n.Expect = PackagesFileInstall, &file.ID, file.SHA256
			n.Items = []tunnel.SoftwareItem{fileItem(file)}
		default:
			n.Mode, n.SoftwareFileID, n.Expect = AppInstall, &file.ID, file.SHA256
			n.Items = []tunnel.SoftwareItem{{Name: file.App.Package, Version: file.App.VersionName}}
		}
		if x.t.view.Rule.NeedsApproval {
			n.State, approval = RunAwaitingApproval, true
		}
		runs = append(runs, n)
	}
	made, err := a.store.CreatePackageRuns(ctx, runs)
	if errors.Is(err, ErrRunActive) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "software batch", err)
		return
	}
	hosts := make([]string, 0, len(made))
	for i := range made {
		made[i].AssetID = targets[i].t.asset.ID
		hosts = append(hosts, made[i].AssetHostname)
	}
	detail := fmt.Sprintf("Asked for %s on %d server%s, one at a time, stopping at a failure: %s.",
		what, len(made), plural(len(made)), strings.Join(hosts, ", "))
	if file.ID != "" {
		detail = strings.TrimSuffix(detail, ".") + fmt.Sprintf(" (SHA-256 %s).", file.SHA256)
	}
	if len(skipped) > 0 {
		names := make([]string, 0, len(skipped))
		for _, s := range skipped {
			names = append(names, firstNonEmpty(s.Hostname, s.AssetID)+" ("+s.Reason+")")
		}
		detail += " Left out: " + strings.Join(names, "; ") + "."
	}
	a.auditAsset(r, AuditEvent{Action: "software.batch_requested", Severity: "notice", ActorEmail: sess.Email,
		Target: what, Detail: detail})
	if approval {
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: sess.Email,
			Title:   fmt.Sprintf("%s asks for %s on %d servers", sess.Email, what, len(made)),
			Detail:  "Approve or deny each on its server's Apps tab, or in Activity.",
			Link:    "/activity",
			Subject: packageRunSubject(made[0].ID),
		})
	}
	if skipped == nil {
		skipped = []BatchSkipped{}
	}
	writeJSON(w, http.StatusAccepted, BatchResult{Runs: made, Skipped: skipped})
}

// batchReady says why a server cannot take something for platform, "" when
// it can.
func batchReady(t packageTarget, platform string) string {
	v, as := t.view, t.asset
	switch platform {
	case "android":
		if as.Kind != "android" {
			return "an APK is for Android devices"
		}
	case tunnel.ScriptWindows:
		if !isWindowsAsset(as) {
			return "not a Windows server"
		}
	case tunnel.ScriptLinux:
		if as.Kind == "android" || as.Protocol != "ssh" {
			return "not a Linux server"
		}
	default:
		return "cannot take it"
	}
	switch {
	case !v.Enrolled:
		return "no enrolled agent"
	case !v.Reported:
		return "its agent is too old to install software"
	case platform == tunnel.ScriptWindows && !hasMode(v.Installs, PackagesScript):
		return "its agent cannot install software; update it"
	case platform == tunnel.ScriptLinux && !v.Supported:
		return "software cannot be installed on it from the console: " + v.Reason
	}
	return ""
}

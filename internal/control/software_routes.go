package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Installing and removing software on a Linux server, from its Apps tab.

	GET  /api/v1/assets/{id}/software            what is installed (?all=1: dependencies too)
	GET  /api/v1/assets/{id}/software/search?q=  what the server's repositories offer
	POST /api/v1/assets/{id}/software/preview    what an install or removal would change
	POST /api/v1/assets/{id}/software/runs       do what a preview said
	GET  /api/v1/assets/{id}/software/runs       the installs and removals, newest first
	POST /api/v1/package-runs/{id}/undo          a preview of undoing a finished run

The three questions go to the agent over the tunnel, so a person searching
gets an answer in a second rather than on the next heartbeat. The run is a
package run (package_updates.go) with mode install or remove: it travels on
the heartbeat, one per server, and waits for a second admin where the
server's update rule says so.

What a person says yes to is a preview: the exact list of packages that
would be added, changed and removed, simulated by the server's own package
manager. The run carries the preview's hash and the agent simulates again
before it starts; if the answer changed, it stops without touching
anything. A preview is good for fifteen minutes.

Admins and owners install and remove, like updates. Auditors see what is
installed and what was done.
*/

const (
	// Package run modes for software, beside all, security and selected.
	PackagesInstall = "install"
	PackagesRemove  = "remove"
	PackagesScript  = "script"
	// PackagesFileInstall installs an MSI or EXE from the library on a
	// Windows server.
	PackagesFileInstall = "file_install"

	previewTTL = 15 * time.Minute
	// A preview of an install may refresh the package lists first.
	softwareWait = 90 * time.Second
)

var rePreviewID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SoftwareAnswer is what the Installed list and the search show.
type SoftwareAnswer struct {
	// State is ok, offline, unsupported or failed.
	State     string                    `json:"state"`
	Error     string                    `json:"error,omitempty"`
	Manager   string                    `json:"manager,omitempty"`
	Installed []tunnel.InstalledPackage `json:"installed"`
	Total     int                       `json:"total,omitempty"`
	Manual    int                       `json:"manual,omitempty"`
	Found     []tunnel.FoundPackage     `json:"found"`
	Truncated bool                      `json:"truncated,omitempty"`
}

// SoftwarePreview is a preview as the console shows it.
type SoftwarePreview struct {
	ID            string                       `json:"id"`
	AssetID       string                       `json:"assetId"`
	Action        string                       `json:"action"`
	Items         []tunnel.SoftwareItem        `json:"items"`
	Purge         bool                         `json:"purge"`
	StartServices bool                         `json:"startServices"`
	Result        tunnel.SoftwarePreviewResult `json:"result"`
	Blocked       string                       `json:"blocked,omitempty"`
	NeedsApproval bool                         `json:"needsApproval"`
	UndoOf        *string                      `json:"undoOf,omitempty"`
	CreatedBy     string                       `json:"createdBy"`
	CreatedAt     time.Time                    `json:"createdAt"`
	ExpiresAt     time.Time                    `json:"expiresAt"`
	hostname      string
	assetHostname string
	hash          string
}

func (a *API) registerSoftwareRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/assets/{id}/software", a.getSoftware)
	mux.HandleFunc("GET /api/v1/assets/{id}/software/search", a.getSoftwareSearch)
	mux.HandleFunc("POST /api/v1/assets/{id}/software/preview", a.postSoftwarePreview)
	mux.HandleFunc("POST /api/v1/assets/{id}/software/runs", a.postSoftwareRun)
	mux.HandleFunc("GET /api/v1/assets/{id}/software/runs", a.getSoftwareRuns)
	mux.HandleFunc("POST /api/v1/package-runs/{id}/undo", a.postSoftwareUndo)
}

// softwareAsset reads the server a software request is about and whether
// its agent can be asked, answering the request itself when not.
func (a *API) softwareAsset(w http.ResponseWriter, r *http.Request) (packageTarget, bool) {
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return packageTarget{}, false
	}
	t, err := a.packagesFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "software", err)
		return t, false
	}
	if !softwareReady(w, t) {
		return t, false
	}
	return t, true
}

// softwareReady is packagesReady in the words of installing software.
func softwareReady(w http.ResponseWriter, t packageTarget) bool {
	switch v := t.view; {
	case len(v.Installs) > 0 && v.Enrolled:
		// A Windows server: no updates, but it installs and removes.
		return true
	case t.asset.Protocol != "ssh" && !isWindowsAsset(t.asset):
		writeErr(w, http.StatusConflict, "installing software from the console is for Linux servers reached over SSH")
	case !v.Enrolled:
		writeErr(w, http.StatusConflict, "this server has no enrolled agent; add it from Servers, Add a server")
	case !v.Reported:
		writeErr(w, http.StatusConflict, "this server's agent is too old to install software; "+
			"update the agent from its Settings tab first")
	case !v.Supported:
		writeErr(w, http.StatusConflict, "software cannot be installed on this server from the console: "+v.Reason)
	default:
		return true
	}
	return false
}

// GET /api/v1/assets/{id}/software
func (a *API) getSoftware(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "installed software is for administrators, owners and auditors")
		return
	}
	t, ok := a.softwareAsset(w, r)
	if !ok {
		return
	}
	all := r.URL.Query().Get("all")
	q := tunnel.GrantSoftware{Op: tunnel.SoftwareInstalled, All: all == "1" || all == "true"}
	writeJSON(w, http.StatusOK, a.softwareAnswer(a.askSoftware(r.Context(), sess.Email, t.agent, q)))
}

// GET /api/v1/assets/{id}/software/search?q=
func (a *API) getSoftwareSearch(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing software", "admin", "owner") {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if !tunnel.ValidSoftwareQuery(query) {
		writeErr(w, http.StatusBadRequest, "search for a package name: letters, digits, spaces and + . _ -, "+
			"at most 64 characters")
		return
	}
	t, ok := a.softwareAsset(w, r)
	if !ok {
		return
	}
	q := tunnel.GrantSoftware{Op: tunnel.SoftwareSearch, Query: query}
	writeJSON(w, http.StatusOK, a.softwareAnswer(a.askSoftware(r.Context(), sess.Email, t.agent, q)))
}

func (a *API) softwareAnswer(res tunnel.SoftwareResult, state, msg string) SoftwareAnswer {
	ans := SoftwareAnswer{State: state, Error: msg, Installed: []tunnel.InstalledPackage{},
		Found: []tunnel.FoundPackage{}}
	if state != "ok" {
		return ans
	}
	ans.Manager, ans.Total, ans.Manual, ans.Truncated = res.Manager, res.Total, res.Manual, res.Truncated
	if res.Installed != nil {
		ans.Installed = res.Installed
	}
	if len(ans.Installed) > tunnel.MaxSoftwareInstalled {
		ans.Installed, ans.Truncated = ans.Installed[:tunnel.MaxSoftwareInstalled], true
	}
	if res.Found != nil {
		ans.Found = res.Found
	}
	if len(ans.Found) > tunnel.MaxSoftwareFound {
		ans.Found, ans.Truncated = ans.Found[:tunnel.MaxSoftwareFound], true
	}
	return ans
}

// previewRequest is a preview asked for in the console.
type previewRequest struct {
	Action        string                `json:"action"`
	Items         []tunnel.SoftwareItem `json:"items"`
	Purge         bool                  `json:"purge"`
	StartServices *bool                 `json:"startServices"`
}

// POST /api/v1/assets/{id}/software/preview
func (a *API) postSoftwarePreview(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing software", "admin", "owner") {
		return
	}
	var in previewRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for i := range in.Items {
		in.Items[i].Name = strings.TrimSpace(in.Items[i].Name)
		in.Items[i].Version = strings.TrimSpace(in.Items[i].Version)
	}
	if err := tunnel.ValidateSoftwareChange(in.Action, in.Items, in.Purge); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Services start unless asked not to: installing a web server is
	// usually wanting it to serve.
	start := in.Action == PackagesInstall && (in.StartServices == nil || *in.StartServices)
	t, ok := a.softwareAsset(w, r)
	if !ok {
		return
	}
	pv, ok := a.previewSoftware(w, r, sess.Email, t, in.Action, in.Items, in.Purge, start, nil)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

// previewSoftware asks the agent and keeps its answer, answering the request
// itself when it cannot.
func (a *API) previewSoftware(w http.ResponseWriter, r *http.Request, by string, t packageTarget, action string,
	items []tunnel.SoftwareItem, purge, start bool, undoOf *string) (SoftwarePreview, bool) {
	q := tunnel.GrantSoftware{Op: tunnel.SoftwarePreview, Action: action, Items: items, Purge: purge}
	res, state, msg := a.askSoftware(r.Context(), by, t.agent, q)
	switch state {
	case "ok":
	case "offline", "unsupported":
		writeErr(w, http.StatusConflict, msg)
		return SoftwarePreview{}, false
	default:
		writeErr(w, http.StatusBadGateway, msg)
		return SoftwarePreview{}, false
	}
	if res.Preview == nil {
		writeErr(w, http.StatusBadGateway, "the agent did not say what would change")
		return SoftwarePreview{}, false
	}
	p := *res.Preview
	if p.Changes == nil {
		p.Changes = []tunnel.SoftwareChange{}
	}
	pv := SoftwarePreview{
		AssetID: t.asset.ID, Action: action, Items: items, Purge: purge, StartServices: start,
		Result: p, Blocked: p.Blocked, NeedsApproval: t.view.Rule.NeedsApproval, UndoOf: undoOf,
		CreatedBy: by, hostname: t.agent, assetHostname: t.asset.Hostname, hash: p.Hash,
	}
	if err := a.store.insertSoftwarePreview(r.Context(), &pv); err != nil {
		a.fail(w, "software preview", err)
		return SoftwarePreview{}, false
	}
	return pv, true
}

// POST /api/v1/assets/{id}/software/runs {previewId}
func (a *API) postSoftwareRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing software", "admin", "owner") {
		return
	}
	var in struct {
		PreviewID string `json:"previewId"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !rePreviewID.MatchString(in.PreviewID) {
		writeErr(w, http.StatusBadRequest, "say which preview to go ahead with")
		return
	}
	t, ok := a.softwareAsset(w, r)
	if !ok {
		return
	}
	pv, err := a.store.softwarePreview(r.Context(), in.PreviewID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeErr(w, http.StatusNotFound, "no such preview")
		return
	case err != nil:
		a.fail(w, "software preview", err)
		return
	}
	switch {
	case !strings.EqualFold(pv.hostname, t.agent):
		writeErr(w, http.StatusBadRequest, "that preview is for another server")
		return
	case pv.Blocked != "":
		writeErr(w, http.StatusConflict, pv.Blocked)
		return
	case time.Now().After(pv.ExpiresAt):
		writeErr(w, http.StatusConflict, "that preview is more than fifteen minutes old; preview it again")
		return
	}
	n := newRun{
		Hostname: t.agent, AssetHostname: t.asset.Hostname, Mode: pv.Action, Packages: []string{},
		Holds: []string{}, Reboot: "never", Trigger: "manual", State: RunQueued, RequestedBy: sess.Email,
		Items: pv.Items, Purge: pv.Purge, StartServices: pv.StartServices, Expect: pv.hash,
		PreviewID: &pv.ID, UndoOf: pv.UndoOf,
	}
	if t.view.Rule.NeedsApproval {
		n.State = RunAwaitingApproval
	}
	runs, err := a.store.CreatePackageRuns(r.Context(), []newRun{n})
	if errors.Is(err, ErrRunActive) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "software run", err)
		return
	}
	run := runs[0]
	run.AssetID = t.asset.ID
	a.softwareRunAsked(r, sess.Email, run, pv)
	writeJSON(w, http.StatusAccepted, run)
}

// softwareRunAsked audits an install or removal asked for, and asks for the
// approval it waits on.
func (a *API) softwareRunAsked(r *http.Request, by string, run PackageRun, pv SoftwarePreview) {
	adds, changes, removes := 0, 0, 0
	for _, c := range pv.Result.Changes {
		switch c.Kind {
		case tunnel.ChangeAdd:
			adds++
		case tunnel.ChangeRemove:
			removes++
		default:
			changes++
		}
	}
	extra := []string{}
	if run.Mode == PackagesInstall {
		if run.StartServices {
			extra = append(extra, "starting its services")
		} else {
			extra = append(extra, "keeping its services stopped")
		}
	}
	if run.UndoOf != nil {
		extra = append(extra, "undoing an earlier run")
	}
	if run.State == RunAwaitingApproval {
		extra = append(extra, "it waits for another admin to approve it")
	}
	tail := ""
	if len(extra) > 0 {
		tail = "; " + strings.Join(extra, "; ")
	}
	action := "software.install_requested"
	if run.Mode == PackagesRemove {
		action = "software.remove_requested"
	}
	a.auditAsset(r, AuditEvent{
		Action: action, Severity: "notice", ActorEmail: by, Target: run.AssetHostname,
		Detail: fmt.Sprintf("Asked for %s on %s: %d to add, %d to change, %d to remove%s.",
			runPhrase(run), run.AssetHostname, adds, changes, removes, tail),
	})
	if run.State == RunAwaitingApproval {
		verb := "install software on "
		if run.Mode == PackagesRemove {
			verb = "remove software from "
		}
		a.raiseAlert(r.Context(), ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: by,
			Title:   by + " asks to " + verb + run.AssetHostname,
			Detail:  strings.ToUpper(runPhrase(run)[:1]) + runPhrase(run)[1:] + ". Approve or deny it on the server's Apps tab.",
			Link:    "/assets/" + run.AssetID + "?tab=apps",
			Subject: packageRunSubject(run.ID),
		})
	}
}

// GET /api/v1/assets/{id}/software/runs
func (a *API) getSoftwareRuns(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "installed software is for administrators, owners and auditors")
		return
	}
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	t, err := a.packagesFor(r.Context(), asset)
	if err != nil {
		a.fail(w, "software", err)
		return
	}
	runs, err := a.store.softwareRunsFor(r.Context(), t.agent, 20)
	if err != nil {
		a.fail(w, "software runs", err)
		return
	}
	for i := range runs {
		runs[i].AssetID = asset.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "needsApproval": t.view.Rule.NeedsApproval})
}

// POST /api/v1/package-runs/{id}/undo previews undoing a finished install
// or removal: removing what an install added, or putting back what a
// removal took, at the versions it took. The console shows the preview and
// goes ahead with it like any other.
func (a *API) postSoftwareUndo(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "installing software", "admin", "owner") {
		return
	}
	run, err := a.store.PackageRunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failRun(w, err)
		return
	}
	action, items, err := undoOf(run)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	id, _, _, err := a.store.assetForAgent(r.Context(), run.Hostname)
	if err != nil {
		writeErr(w, http.StatusNotFound, "the server this ran on is gone")
		return
	}
	r.SetPathValue("id", id)
	t, ok := a.softwareAsset(w, r)
	if !ok {
		return
	}
	pv, ok := a.previewSoftware(w, r, sess.Email, t, action, items, false, run.StartServices, &run.ID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

// undoOf says what undoes a run: an install is undone by removing the
// packages it was asked for that were not there before (what they pulled
// in goes with them); a removal by installing what it was asked to remove,
// at the version it removed.
func undoOf(run PackageRun) (string, []tunnel.SoftwareItem, error) {
	if run.Mode != PackagesInstall && run.Mode != PackagesRemove {
		return "", nil, errors.New("only an install or a removal can be undone here")
	}
	if run.State != RunDone {
		return "", nil, errors.New("only a run that finished can be undone")
	}
	asked := map[string]bool{}
	for _, it := range run.Items {
		asked[it.Name] = true
	}
	var items []tunnel.SoftwareItem
	for _, c := range run.Upgraded {
		if !asked[c.Name] {
			continue
		}
		switch {
		case run.Mode == PackagesInstall && c.From == "" && c.To != "":
			items = append(items, tunnel.SoftwareItem{Name: c.Name})
		case run.Mode == PackagesRemove && c.From != "" && c.To == "":
			items = append(items, tunnel.SoftwareItem{Name: c.Name, Version: c.From})
		}
	}
	if len(items) == 0 {
		if run.Mode == PackagesInstall {
			return "", nil, errors.New("this install added nothing new to remove: what it asked for was already there")
		}
		return "", nil, errors.New("this removal took nothing that can be put back")
	}
	action := PackagesRemove
	if run.Mode == PackagesRemove {
		action = PackagesInstall
	}
	return action, items, tunnel.ValidateSoftwareChange(action, items, false)
}

// describeSoftware names what an install or a removal is about.
func describeSoftware(action string, items []tunnel.SoftwareItem, purge bool) string {
	names := make([]string, 0, len(items))
	for _, it := range items {
		n := it.Name
		if it.Version != "" {
			n += " " + it.Version
		}
		names = append(names, n)
	}
	s := strings.Join(names, ", ")
	if s == "" {
		s = "nothing"
	}
	if action == PackagesRemove && purge {
		s += " and its settings"
	}
	return s
}

// askSoftware puts one question to a server's agent. state is ok, offline,
// unsupported or failed, with why in msg.
func (a *API) askSoftware(ctx context.Context, actor, hostname string, q tunnel.GrantSoftware) (
	tunnel.SoftwareResult, string, string) {
	var res tunnel.SoftwareResult
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return res, "failed", "this control plane cannot reach agents: set signing_secret"
	}
	if err := q.Validate(); err != nil {
		return res, "failed", err.Error()
	}
	g := tunnel.NewGrant(actor, hostname, "software", "")
	g.Service = tunnel.SoftwareService
	g.Software = &q
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		return res, "failed", err.Error()
	}
	octx, cancel := context.WithTimeout(ctx, softwareWait)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, hostname, tunnel.Header{Service: tunnel.SoftwareService, Grant: tok})
	switch {
	case errors.Is(err, ErrNoTunnel):
		return res, "offline", "the agent on " + hostname + " is not connected"
	case tunnel.IsRefused(err) && strings.Contains(open.Error, "unknown service"):
		return res, "unsupported", "the agent on " + hostname + " is too old to install software; " +
			"update it from the server's Settings tab"
	case tunnel.IsRefused(err):
		return res, "failed", open.Error
	case err != nil:
		return res, "failed", "could not reach the agent on " + hostname + ": " + err.Error()
	}
	defer st.Close()
	go func() {
		<-octx.Done()
		_ = st.Close()
	}()
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			if octx.Err() != nil {
				return res, "failed", "the agent on " + hostname + " did not answer in time"
			}
			return res, "failed", "the agent went away before it answered"
		}
		if typ != tunnel.FrameSoftwareResult {
			continue
		}
		if err := json.Unmarshal(p, &res); err != nil {
			return res, "failed", "the agent's answer could not be read"
		}
		if !res.OK {
			return res, "failed", res.Error
		}
		return res, "ok", ""
	}
}

/* ── Store ───────────────────────────────────────────────────────────────── */

func (s *Store) insertSoftwarePreview(ctx context.Context, pv *SoftwarePreview) error {
	items, err := json.Marshal(pv.Items)
	if err != nil {
		return err
	}
	result, err := json.Marshal(pv.Result)
	if err != nil {
		return err
	}
	// Old previews are of no use to anyone: they cannot be gone ahead with.
	_, _ = s.db(ctx).Exec(ctx, `DELETE FROM software_previews WHERE created_at < now() - interval '1 day'`)
	err = s.db(ctx).QueryRow(ctx, `
		INSERT INTO software_previews (hostname, asset_hostname, action, items, purge, start_services,
		                               result, hash, blocked, undo_of, created_by)
		VALUES ($1,$2,$3,$4::jsonb,$5,$6,$7::jsonb,$8,$9,$10::uuid,$11)
		RETURNING id::text, created_at`,
		pv.hostname, pv.assetHostname, pv.Action, items, pv.Purge, pv.StartServices, result, pv.hash,
		pv.Blocked, pv.UndoOf, pv.CreatedBy).Scan(&pv.ID, &pv.CreatedAt)
	if err != nil {
		return err
	}
	pv.ExpiresAt = pv.CreatedAt.Add(previewTTL)
	return nil
}

func (s *Store) softwarePreview(ctx context.Context, id string) (SoftwarePreview, error) {
	var (
		pv            SoftwarePreview
		items, result []byte
	)
	err := s.db(ctx).QueryRow(ctx, `
		SELECT id::text, hostname, asset_hostname, action, items, purge, start_services, result, hash,
		       blocked, undo_of::text, created_by, created_at
		  FROM software_previews WHERE id = $1::uuid`, id).Scan(&pv.ID, &pv.hostname, &pv.assetHostname,
		&pv.Action, &items, &pv.Purge, &pv.StartServices, &result, &pv.hash, &pv.Blocked, &pv.UndoOf,
		&pv.CreatedBy, &pv.CreatedAt)
	if err != nil {
		return pv, err
	}
	if err := json.Unmarshal(items, &pv.Items); err != nil {
		return pv, err
	}
	if err := json.Unmarshal(result, &pv.Result); err != nil {
		return pv, err
	}
	pv.ExpiresAt = pv.CreatedAt.Add(previewTTL)
	return pv, nil
}

func (s *Store) softwareRunsFor(ctx context.Context, host string, limit int) ([]PackageRun, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+packageRunCols+` FROM package_runs
		WHERE lower(hostname) = lower($1) AND mode IN ('install', 'remove', 'script', 'file_install', 'app_install', 'app_uninstall', 'app_disable', 'app_restore')
		ORDER BY requested_at DESC LIMIT $2`, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PackageRun{}
	for rows.Next() {
		r, err := scanPackageRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

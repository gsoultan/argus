package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Install scripts in the Software library.

	GET    /api/v1/software/scripts                      the library
	POST   /api/v1/software/scripts                      a new script and its first version
	GET    /api/v1/software/scripts/{id}                 a script and every version
	POST   /api/v1/software/scripts/{id}/versions        a new version
	DELETE /api/v1/software/scripts/{id}                 take it out of the library
	GET    /api/v1/software/script-rules?platform=       what the automatic checks look for
	POST   /api/v1/software/script-check                 check text without saving it
	POST   /api/v1/software/script-versions/{id}/review  approve a version
	POST   /api/v1/assets/{id}/software/script-runs      run a reviewed version on a server

A script runs as root, so the library keeps five promises (see the design):
only text comes in; every version is checked automatically; a person other
than its author reviews it; the agent runs exactly what was reviewed, which
the control plane signs (tunnel.SignScript) and the agent verifies; and a
version runs successfully outside production before it runs on production.

Admins and owners write, review and run scripts; auditors read them.
*/

// ScriptParam is a parameter a script takes, passed as ARGUS_PARAM_<NAME>.
type ScriptParam struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Kind     string   `json:"kind"` // text, choice or secret
	Choices  []string `json:"choices,omitempty"`
	Required bool     `json:"required,omitempty"`
	Default  string   `json:"default,omitempty"`
}

// AcceptedFinding is a warning a reviewer ticked off, and why.
type AcceptedFinding struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// SoftwareScript is a library entry.
type SoftwareScript struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Platform    string    `json:"platform"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	// Latest is the newest version; Reviewed the newest reviewed one,
	// which is what runs.
	Latest   *ScriptVersion  `json:"latest,omitempty"`
	Reviewed *ScriptVersion  `json:"reviewed,omitempty"`
	Versions []ScriptVersion `json:"versions,omitempty"`
}

// ScriptVersion is one saved version.
type ScriptVersion struct {
	ID             string            `json:"id"`
	ScriptID       string            `json:"scriptId"`
	Version        int               `json:"version"`
	Install        string            `json:"install,omitempty"`
	Uninstall      string            `json:"uninstall,omitempty"`
	Check          string            `json:"check,omitempty"`
	Params         []ScriptParam     `json:"params"`
	RunAs          string            `json:"runAs"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Reboot         string            `json:"reboot"`
	InstallSHA     string            `json:"installSha"`
	UninstallSHA   string            `json:"uninstallSha,omitempty"`
	CheckSHA       string            `json:"checkSha,omitempty"`
	Findings       []ScriptFinding   `json:"findings"`
	Blocked        bool              `json:"blocked"`
	Note           string            `json:"note,omitempty"`
	Author         string            `json:"author"`
	CreatedAt      time.Time         `json:"createdAt"`
	ReviewedBy     *string           `json:"reviewedBy"`
	ReviewedAt     *time.Time        `json:"reviewedAt"`
	SelfReviewed   bool              `json:"selfReviewed"`
	Accepted       []AcceptedFinding `json:"accepted"`
	TriedOn        *string           `json:"triedOn"`
	TriedAt        *time.Time        `json:"triedAt"`
	signature      string
	scriptName     string
	platform       string
	archived       bool
}

// ScriptRunOrder is a script as the heartbeat carries it to the agent
// (agent.ScriptOrder).
type ScriptRunOrder struct {
	Action     string            `json:"action"`
	Text       string            `json:"text"`
	Check      string            `json:"check,omitempty"`
	Approval   string            `json:"approval"`
	Params     map[string]string `json:"params,omitempty"`
	Secret     []string          `json:"secret,omitempty"`
	TimeoutSec int               `json:"timeout_sec"`
}

func (a *API) registerScriptRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/software/scripts", a.getScripts)
	mux.HandleFunc("POST /api/v1/software/scripts", a.postScript)
	mux.HandleFunc("GET /api/v1/software/scripts/{id}", a.getScript)
	mux.HandleFunc("POST /api/v1/software/scripts/{id}/versions", a.postScriptVersion)
	mux.HandleFunc("DELETE /api/v1/software/scripts/{id}", a.deleteScript)
	mux.HandleFunc("GET /api/v1/software/script-rules", a.getScriptRules)
	mux.HandleFunc("POST /api/v1/software/script-check", a.postScriptCheck)
	mux.HandleFunc("POST /api/v1/software/script-versions/{id}/review", a.postScriptReview)
	mux.HandleFunc("POST /api/v1/assets/{id}/software/script-runs", a.postScriptRun)
}

/* ── Input ───────────────────────────────────────────────────────────────── */

// scriptPart is one part as the console sends it: pasted text, or a file
// read in the browser and sent as base64, which the control plane checks
// is a text script.
type scriptPart struct {
	Text       string `json:"text"`
	FileName   string `json:"fileName,omitempty"`
	FileBase64 string `json:"fileBase64,omitempty"`
}

func (p scriptPart) normalize(platform string) (string, error) {
	if p.FileBase64 != "" {
		b, err := base64.StdEncoding.DecodeString(p.FileBase64)
		if err != nil {
			return "", errors.New("the uploaded file could not be read")
		}
		if len(b) > 2*tunnel.MaxScriptBytes {
			return "", fmt.Errorf("a script can be at most %d KB", tunnel.MaxScriptBytes>>10)
		}
		return NormalizeScript(platform, p.FileName, b)
	}
	return NormalizeScript(platform, "", []byte(p.Text))
}

type versionInput struct {
	Install        scriptPart    `json:"install"`
	Uninstall      scriptPart    `json:"uninstall"`
	Check          scriptPart    `json:"check"`
	Params         []ScriptParam `json:"params"`
	RunAs          string        `json:"runAs"`
	TimeoutMinutes int           `json:"timeoutMinutes"`
	Reboot         string        `json:"reboot"`
	Note           string        `json:"note"`
}

var reScriptName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._()+-]{0,79}$`)

// build normalizes and checks a version; it is never refused for what the
// checks find (a blocked version is kept, so its author can see why).
func (in versionInput) build(ctx context.Context, platform string) (ScriptVersion, error) {
	v := ScriptVersion{RunAs: strings.TrimSpace(in.RunAs), Reboot: in.Reboot, Note: clip(strings.TrimSpace(in.Note), 500)}
	var err error
	if v.Install, err = in.Install.normalize(platform); err != nil {
		return v, fmt.Errorf("install script: %w", err)
	}
	if v.Install == "" {
		return v, errors.New("the install script is empty")
	}
	if v.Uninstall, err = in.Uninstall.normalize(platform); err != nil {
		return v, fmt.Errorf("uninstall script: %w", err)
	}
	if v.Check, err = in.Check.normalize(platform); err != nil {
		return v, fmt.Errorf("check script: %w", err)
	}
	if !tunnel.ValidRunAs(v.RunAs) || v.RunAs == "root" {
		if v.RunAs == "root" {
			v.RunAs = ""
		} else {
			return v, fmt.Errorf("%q is not an account name", v.RunAs)
		}
	}
	switch v.Reboot {
	case "":
		v.Reboot = "never"
	case "never", "if_asked":
	default:
		return v, errors.New("reboot is never or if_asked")
	}
	if in.TimeoutMinutes == 0 {
		in.TimeoutMinutes = 30
	}
	if in.TimeoutMinutes < 1 || time.Duration(in.TimeoutMinutes)*time.Minute > tunnel.MaxScriptTimeout {
		return v, fmt.Errorf("the timeout is between 1 and %d minutes", int(tunnel.MaxScriptTimeout.Minutes()))
	}
	v.TimeoutSeconds = in.TimeoutMinutes * 60
	if v.Params, err = validParams(in.Params); err != nil {
		return v, err
	}
	v.InstallSHA = tunnel.ScriptHash(v.Install)
	if v.Uninstall != "" {
		v.UninstallSHA = tunnel.ScriptHash(v.Uninstall)
	}
	if v.Check != "" {
		v.CheckSHA = tunnel.ScriptHash(v.Check)
	}
	v.Findings = []ScriptFinding{}
	for _, part := range []struct{ name, text string }{{tunnel.ScriptInstall, v.Install},
		{tunnel.ScriptUninstall, v.Uninstall}, {tunnel.ScriptCheck, v.Check}} {
		if part.text != "" {
			v.Findings = append(v.Findings, CheckScript(ctx, platform, part.name, part.text)...)
		}
	}
	for _, f := range v.Findings {
		if f.Level == LevelBlocked {
			v.Blocked = true
		}
	}
	return v, nil
}

func validParams(in []ScriptParam) ([]ScriptParam, error) {
	if len(in) > tunnel.MaxScriptParams {
		return nil, fmt.Errorf("a script takes at most %d parameters", tunnel.MaxScriptParams)
	}
	seen := map[string]bool{}
	out := []ScriptParam{}
	for _, p := range in {
		p.Name = strings.ToUpper(strings.TrimSpace(p.Name))
		p.Label = clip(strings.TrimSpace(p.Label), 80)
		if !tunnel.ValidScriptParam(p.Name) {
			return nil, fmt.Errorf("%q is not a parameter name: capitals, digits and _, starting with a letter", p.Name)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("%s is named twice", p.Name)
		}
		seen[p.Name] = true
		if p.Label == "" {
			p.Label = p.Name
		}
		switch p.Kind {
		case "text", "secret":
			p.Choices = nil
		case "choice":
			if len(p.Choices) == 0 || len(p.Choices) > 50 {
				return nil, fmt.Errorf("%s: a choice has between one and fifty options", p.Name)
			}
			for _, c := range p.Choices {
				if err := tunnel.ValidateScriptValue(p.Name, c); err != nil || c == "" {
					return nil, fmt.Errorf("%s: %q cannot be an option", p.Name, c)
				}
			}
		default:
			return nil, fmt.Errorf("%s: a parameter is text, choice or secret", p.Name)
		}
		if p.Kind == "secret" {
			p.Default = ""
		}
		if err := tunnel.ValidateScriptValue(p.Name, p.Default); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) scriptReader(w http.ResponseWriter, r *http.Request) bool {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "the software library is for administrators, owners and auditors")
		return false
	}
	return true
}

func (a *API) getScripts(w http.ResponseWriter, r *http.Request) {
	if !a.scriptReader(w, r) {
		return
	}
	list, err := a.store.scripts(r.Context())
	if err != nil {
		a.fail(w, "scripts", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) getScript(w http.ResponseWriter, r *http.Request) {
	if !a.scriptReader(w, r) {
		return
	}
	s, err := a.store.script(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failScript(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) getScriptRules(w http.ResponseWriter, r *http.Request) {
	if !a.scriptReader(w, r) {
		return
	}
	p := r.URL.Query().Get("platform")
	if p != tunnel.ScriptWindows {
		p = tunnel.ScriptLinux
	}
	writeJSON(w, http.StatusOK, ScriptRules(p))
}

// POST /api/v1/software/script-check {platform, part, text|fileBase64}
func (a *API) postScriptCheck(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "writing scripts", "admin", "owner") {
		return
	}
	var in struct {
		Platform string `json:"platform"`
		Part     string `json:"part"`
		scriptPart
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Platform != tunnel.ScriptLinux && in.Platform != tunnel.ScriptWindows {
		writeErr(w, http.StatusBadRequest, "platform is linux or windows")
		return
	}
	if in.Part != tunnel.ScriptUninstall && in.Part != tunnel.ScriptCheck {
		in.Part = tunnel.ScriptInstall
	}
	text, err := in.scriptPart.normalize(in.Platform)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	findings := []ScriptFinding{}
	if text != "" {
		findings = CheckScript(r.Context(), in.Platform, in.Part, text)
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "findings": findings})
}

// POST /api/v1/software/scripts {name, platform, description, version}
func (a *API) postScript(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "writing scripts", "admin", "owner") {
		return
	}
	var in struct {
		Name        string       `json:"name"`
		Platform    string       `json:"platform"`
		Description string       `json:"description"`
		Version     versionInput `json:"version"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !reScriptName.MatchString(in.Name) {
		writeErr(w, http.StatusBadRequest, "name the script: letters, digits, spaces and . _ ( ) + -, at most 80")
		return
	}
	if in.Platform != tunnel.ScriptLinux && in.Platform != tunnel.ScriptWindows {
		writeErr(w, http.StatusBadRequest, "platform is linux or windows")
		return
	}
	v, err := in.Version.build(r.Context(), in.Platform)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s, err := a.store.createScript(r.Context(), in.Name, in.Platform, clip(strings.TrimSpace(in.Description), 500),
		sess.Email, v)
	if err != nil {
		a.failScript(w, err)
		return
	}
	a.auditScriptSaved(r, sess.Email, s, *s.Latest)
	writeJSON(w, http.StatusCreated, s)
}

// POST /api/v1/software/scripts/{id}/versions
func (a *API) postScriptVersion(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "writing scripts", "admin", "owner") {
		return
	}
	var in versionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s, err := a.store.script(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failScript(w, err)
		return
	}
	v, err := in.build(r.Context(), s.Platform)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.addScriptVersion(r.Context(), s.ID, sess.Email, &v); err != nil {
		a.failScript(w, err)
		return
	}
	s, err = a.store.script(r.Context(), s.ID)
	if err != nil {
		a.failScript(w, err)
		return
	}
	a.auditScriptSaved(r, sess.Email, s, v)
	writeJSON(w, http.StatusCreated, s)
}

func (a *API) auditScriptSaved(r *http.Request, by string, s SoftwareScript, v ScriptVersion) {
	blocked, warnings := 0, 0
	for _, f := range v.Findings {
		switch f.Level {
		case LevelBlocked:
			blocked++
		case LevelWarning:
			warnings++
		}
	}
	a.auditAsset(r, AuditEvent{Action: "software.script_saved", Severity: "info", ActorEmail: by, Target: s.Name,
		Detail: fmt.Sprintf("Saved %s v%d (%s, install %s): %d blocking finding%s, %d warning%s. It runs nowhere until someone else reviews it.",
			s.Name, v.Version, s.Platform, v.InstallSHA[:12], blocked, plural(blocked), warnings, plural(warnings))})
}

func (a *API) deleteScript(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "removing scripts", "admin", "owner") {
		return
	}
	s, err := a.store.script(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failScript(w, err)
		return
	}
	if _, err := a.store.db(r.Context()).Exec(r.Context(),
		`UPDATE software_scripts SET archived_at = now() WHERE id = $1::uuid AND archived_at IS NULL`, s.ID); err != nil {
		a.fail(w, "script", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "software.script_removed", Severity: "notice", ActorEmail: sess.Email, Target: s.Name,
		Detail: "Took " + s.Name + " out of the software library. What it installed stays installed; past runs stay in Activity."})
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/v1/software/script-versions/{id}/review {accepted, confirmSelf}
func (a *API) postScriptReview(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "reviewing scripts", "admin", "owner") {
		return
	}
	var in struct {
		Accepted    []AcceptedFinding `json:"accepted"`
		ConfirmSelf string            `json:"confirmSelf"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := a.store.scriptVersion(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failScript(w, err)
		return
	}
	switch {
	case v.archived:
		writeErr(w, http.StatusConflict, "this script was taken out of the library")
		return
	case v.ReviewedBy != nil:
		writeErr(w, http.StatusConflict, "this version is already reviewed")
		return
	case v.Blocked:
		writeErr(w, http.StatusConflict, "this version has blocking findings; change the script and save a new version")
		return
	}
	self := strings.EqualFold(v.Author, sess.Email)
	if self {
		n, err := a.store.CountAdmins(r.Context())
		if err != nil {
			a.fail(w, "script review", err)
			return
		}
		if n > 1 {
			writeErr(w, http.StatusForbidden, "someone other than its author reviews a script; ask another admin")
			return
		}
		if in.ConfirmSelf != "review my own script" {
			writeErr(w, http.StatusConflict, `this Argus has one admin, so you may review your own script: type "review my own script" to confirm`)
			return
		}
	}
	given := map[string]string{}
	for _, acc := range in.Accepted {
		given[acc.Key] = strings.TrimSpace(acc.Reason)
	}
	accepted := []AcceptedFinding{}
	for _, f := range v.Findings {
		if f.Level != LevelWarning {
			continue
		}
		reason := given[f.Key()]
		if len(reason) < 3 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("say why %q (%s line %d) is acceptable", f.Title, f.Part, f.Line))
			return
		}
		accepted = append(accepted, AcceptedFinding{Key: f.Key(), Reason: clip(reason, 300)})
	}
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		writeErr(w, http.StatusConflict, "this control plane cannot sign scripts: set signing_secret")
		return
	}
	now := time.Now().UTC()
	appr := tunnel.ScriptApproval{VersionID: v.ID, Script: v.ScriptID, Name: v.scriptName, Version: v.Version,
		Platform: v.platform, InstallSHA: v.InstallSHA, UninstallSHA: v.UninstallSHA, CheckSHA: v.CheckSHA,
		RunAs: v.RunAs, TimeoutSec: v.TimeoutSeconds, ReviewedBy: sess.Email, ReviewedAt: now}
	for _, p := range v.Params {
		appr.Params = append(appr.Params, p.Name)
	}
	sig, err := tunnel.SignScript(a.Tunnels.grantKeyFor(r.Context()), appr)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	acc, _ := json.Marshal(accepted)
	tag, err := a.store.db(r.Context()).Exec(r.Context(), `
		UPDATE software_script_versions SET reviewed_by = $2, reviewed_at = $3, self_reviewed = $4,
		       accepted = $5::jsonb, signature = $6
		 WHERE id = $1::uuid AND reviewed_by IS NULL AND NOT blocked`, v.ID, sess.Email, now, self, acc, sig)
	if err != nil {
		a.fail(w, "script review", err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusConflict, "this version was reviewed a moment ago")
		return
	}
	how := "Reviewed"
	if self {
		how = "Self-reviewed (this Argus has one admin)"
	}
	detail := fmt.Sprintf("%s %s v%d, written by %s", how, v.scriptName, v.Version, v.Author)
	for _, acc := range accepted {
		detail += fmt.Sprintf("; accepted %s: %s", acc.Key, acc.Reason)
	}
	a.auditAsset(r, AuditEvent{Action: "software.script_reviewed", Severity: "notice", ActorEmail: sess.Email,
		Target: v.scriptName, Detail: detail + "."})
	s, err := a.store.script(r.Context(), v.ScriptID)
	if err != nil {
		a.failScript(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// POST /api/v1/assets/{id}/software/script-runs {versionId, action, params}
func (a *API) postScriptRun(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "running install scripts", "admin", "owner") {
		return
	}
	var in struct {
		VersionID string            `json:"versionId"`
		Action    string            `json:"action"`
		Params    map[string]string `json:"params"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !rePreviewID.MatchString(in.VersionID) {
		writeErr(w, http.StatusBadRequest, "say which script version to run")
		return
	}
	if in.Action == "" {
		in.Action = tunnel.ScriptInstall
	}
	if in.Action != tunnel.ScriptInstall && in.Action != tunnel.ScriptUninstall {
		writeErr(w, http.StatusBadRequest, "action is install or uninstall")
		return
	}
	t, ok := a.softwareAsset(w, r)
	if !ok {
		return
	}
	v, err := a.store.scriptVersion(r.Context(), in.VersionID)
	if err != nil {
		a.failScript(w, err)
		return
	}
	switch {
	case v.archived:
		writeErr(w, http.StatusConflict, "this script was taken out of the library")
		return
	case v.signature == "":
		writeErr(w, http.StatusConflict, fmt.Sprintf("%s v%d has not been reviewed; it runs nowhere until someone other than its author approves it", v.scriptName, v.Version))
		return
	case v.platform != tunnel.ScriptLinux:
		writeErr(w, http.StatusConflict, "this Argus runs install scripts on Linux servers; Windows scripts run once Windows servers can install software")
		return
	case in.Action == tunnel.ScriptUninstall && v.Uninstall == "":
		writeErr(w, http.StatusConflict, fmt.Sprintf("%s v%d has no uninstall script", v.scriptName, v.Version))
		return
	}
	prod := isProduction(t.asset.Tags)
	if prod && v.TriedOn == nil {
		writeErr(w, http.StatusConflict, fmt.Sprintf("%s v%d has not yet run successfully on a server outside production; "+
			"try it on a staging or test server first", v.scriptName, v.Version))
		return
	}
	plain, secret, err := scriptValues(v.Params, in.Params)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var sealed []byte
	if len(secret) > 0 {
		if sealed, err = a.sealSecrets(r.Context(), secret); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
	}
	n := newRun{
		Hostname: t.agent, AssetHostname: t.asset.Hostname, Mode: PackagesScript, Packages: []string{},
		Holds: []string{}, Reboot: "never", Trigger: "manual", State: RunQueued, RequestedBy: sess.Email,
		ScriptVersionID: &v.ID, ScriptAction: in.Action, ScriptParams: plain, ScriptSecrets: sealed,
	}
	if v.Reboot == "if_asked" {
		n.Reboot = "if_needed"
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
		a.fail(w, "script run", err)
		return
	}
	run := runs[0]
	run.AssetID = t.asset.ID
	names := make([]string, 0, len(plain)+len(secret))
	for k, val := range plain {
		names = append(names, k+"="+val)
	}
	for k := range secret {
		names = append(names, k+"=(secret)")
	}
	tail := ""
	if prod {
		tail = fmt.Sprintf("; tried first on %s", *v.TriedOn)
	}
	if run.State == RunAwaitingApproval {
		tail += "; it waits for another admin to approve it"
	}
	a.auditAsset(r, AuditEvent{Action: "software.script_requested", Severity: "notice", ActorEmail: sess.Email,
		Target: run.AssetHostname, Detail: fmt.Sprintf("Asked to run %s of %s v%d (install %s) on %s with %s%s.",
			in.Action, v.scriptName, v.Version, v.InstallSHA[:12], run.AssetHostname, strings.Join(names, ", "), tail)})
	if run.State == RunAwaitingApproval {
		a.raiseAlert(r.Context(), ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudienceAdmins, Actor: sess.Email,
			Title:   fmt.Sprintf("%s asks to run %s on %s", sess.Email, v.scriptName, run.AssetHostname),
			Detail:  fmt.Sprintf("The %s script of %s v%d. Approve or deny it on the server's Apps tab.", in.Action, v.scriptName, v.Version),
			Link:    "/assets/" + t.asset.ID + "?tab=apps",
			Subject: packageRunSubject(run.ID),
		})
	}
	writeJSON(w, http.StatusAccepted, run)
}

// isProduction says whether a server's tags make it production, where a
// script version must have run elsewhere first.
func isProduction(tags []string) bool {
	for _, t := range tags {
		switch strings.ToLower(t) {
		case "production", "prod", "prd":
			return true
		}
	}
	return false
}

// scriptValues checks a run's values against a version's parameters and
// splits them into the ones kept with the run and the secret ones.
func scriptValues(params []ScriptParam, given map[string]string) (plain, secret map[string]string, err error) {
	plain, secret = map[string]string{}, map[string]string{}
	known := map[string]bool{}
	for _, p := range params {
		known[p.Name] = true
		v, ok := given[p.Name]
		if !ok || v == "" {
			v = p.Default
		}
		if v == "" {
			if p.Required {
				return nil, nil, fmt.Errorf("%s is required", p.Label)
			}
			continue
		}
		if err := tunnel.ValidateScriptValue(p.Label, v); err != nil {
			return nil, nil, err
		}
		if p.Kind == "choice" && !slices.Contains(p.Choices, v) {
			return nil, nil, fmt.Errorf("%s is one of %s", p.Label, strings.Join(p.Choices, ", "))
		}
		if p.Kind == "secret" {
			secret[p.Name] = v
		} else {
			plain[p.Name] = v
		}
	}
	for k := range given {
		if !known[k] {
			return nil, nil, fmt.Errorf("the script takes no parameter %s", k)
		}
	}
	return plain, secret, nil
}

/* ── Secret values ───────────────────────────────────────────────────────── */

// A run's secret values wait in the database until the agent takes them,
// sealed with a key derived from signing_secret, and are wiped when the
// run ends.
func (a *API) secretsKey(ctx context.Context) ([]byte, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, errors.New("this control plane cannot keep secret values: set signing_secret")
	}
	sum := sha256.Sum256(append([]byte("argus script secrets v1\x00"), a.Tunnels.grantKeyFor(ctx).Seed()...))
	return sum[:], nil
}

func (a *API) sealSecrets(ctx context.Context, values map[string]string) ([]byte, error) {
	key, err := a.secretsKey(ctx)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	plain, _ := json.Marshal(values)
	return gcm.Seal(nonce, nonce, plain, []byte("script-secrets")), nil
}

func (a *API) openSecrets(ctx context.Context, sealed []byte) (map[string]string, error) {
	key, err := a.secretsKey(ctx)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("the run's secret values cannot be read")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("script-secrets"))
	if err != nil {
		return nil, errors.New("the run's secret values cannot be read; was signing_secret changed?")
	}
	var out map[string]string
	return out, json.Unmarshal(plain, &out)
}

/* ── On the heartbeat ────────────────────────────────────────────────────── */

// attachScript fills a script run's order with the reviewed text, its
// signed approval and its values. A run that cannot be sent is failed,
// with why, rather than left queued for ever.
func (a *API) attachScript(ctx context.Context, host string, o *PackageRunOrder) bool {
	if o == nil || o.Mode != PackagesScript {
		return true
	}
	order, err := a.scriptOrder(ctx, o.ID)
	if err == nil {
		o.Script = order
		return true
	}
	a.log.Error("heartbeat: cannot send a script run", "run", o.ID, "error", err)
	if r, ferr := a.store.failUnsentRun(ctx, host, o.ID, err.Error()); ferr == nil && r != nil {
		a.packageRunEnded(ctx, *r)
	}
	return false
}

func (a *API) scriptOrder(ctx context.Context, runID string) (*ScriptRunOrder, error) {
	var (
		versionID *string
		action    string
		params    []byte
		sealed    []byte
	)
	err := a.store.db(ctx).QueryRow(ctx, `SELECT script_version_id::text, script_action, script_params, script_secrets
		FROM package_runs WHERE id = $1::uuid`, runID).Scan(&versionID, &action, &params, &sealed)
	if err != nil {
		return nil, err
	}
	if versionID == nil {
		return nil, errors.New("the run names no script version")
	}
	v, err := a.store.scriptVersion(ctx, *versionID)
	if err != nil {
		return nil, err
	}
	if v.signature == "" {
		return nil, errors.New("the script version is not reviewed")
	}
	o := &ScriptRunOrder{Action: action, Text: v.Install, Check: v.Check, Approval: v.signature,
		Params: map[string]string{}, TimeoutSec: v.TimeoutSeconds}
	if action == tunnel.ScriptUninstall {
		o.Text = v.Uninstall
	}
	_ = json.Unmarshal(params, &o.Params)
	if len(sealed) > 0 {
		secret, err := a.openSecrets(ctx, sealed)
		if err != nil {
			return nil, err
		}
		for k, val := range secret {
			o.Params[k] = val
			o.Secret = append(o.Secret, k)
		}
	}
	return o, nil
}

// scriptRunEnded wipes a run's secret values and, for a run that worked
// outside production, records that the version has been tried.
func (a *API) scriptRunEnded(ctx context.Context, r PackageRun) {
	if _, err := a.store.db(ctx).Exec(ctx, `UPDATE package_runs SET script_secrets = NULL WHERE id = $1::uuid`, r.ID); err != nil {
		a.log.Error("cannot wipe a script run's secret values", "run", r.ID, "error", err)
	}
	if r.State != RunDone || r.ScriptVersionID == nil || r.ScriptAction != tunnel.ScriptInstall {
		return
	}
	_, _, tags, err := a.store.assetForAgent(ctx, r.Hostname)
	if err != nil || isProduction(tags) {
		return
	}
	if _, err := a.store.db(ctx).Exec(ctx, `
		UPDATE software_script_versions SET tried_on = $2, tried_at = now(), tried_run = $3::uuid
		 WHERE id = $1::uuid AND tried_on IS NULL`, *r.ScriptVersionID, r.AssetHostname, r.ID); err != nil {
		a.log.Error("cannot record a script version's first run", "version", *r.ScriptVersionID, "error", err)
	}
}

/* ── Store ───────────────────────────────────────────────────────────────── */

var errNoScript = errors.New("no such script")

func (a *API) failScript(w http.ResponseWriter, err error) {
	var pgErr interface{ SQLState() string }
	switch {
	case errors.Is(err, errNoScript), errors.Is(err, pgx.ErrNoRows):
		writeErr(w, http.StatusNotFound, "no such script")
	case errors.As(err, &pgErr) && pgErr.SQLState() == "23505":
		writeErr(w, http.StatusConflict, "a script by that name is already in the library")
	case errors.As(err, &pgErr) && pgErr.SQLState() == "22P02":
		writeErr(w, http.StatusNotFound, "no such script")
	default:
		a.fail(w, "script", err)
	}
}

const scriptVersionCols = `v.id::text, v.script_id::text, v.version, v.install, v.uninstall, v.check_text, v.params,
	v.run_as, v.timeout_seconds, v.reboot, v.install_sha, v.uninstall_sha, v.check_sha, v.findings, v.blocked, v.note,
	v.author, v.created_at, v.reviewed_by, v.reviewed_at, v.self_reviewed, v.accepted, v.tried_on, v.tried_at,
	v.signature, s.name, s.platform, s.archived_at IS NOT NULL`

func scanScriptVersion(row pgx.Row) (ScriptVersion, error) {
	var (
		v                          ScriptVersion
		params, findings, accepted []byte
	)
	err := row.Scan(&v.ID, &v.ScriptID, &v.Version, &v.Install, &v.Uninstall, &v.Check, &params, &v.RunAs,
		&v.TimeoutSeconds, &v.Reboot, &v.InstallSHA, &v.UninstallSHA, &v.CheckSHA, &findings, &v.Blocked, &v.Note,
		&v.Author, &v.CreatedAt, &v.ReviewedBy, &v.ReviewedAt, &v.SelfReviewed, &accepted, &v.TriedOn, &v.TriedAt,
		&v.signature, &v.scriptName, &v.platform, &v.archived)
	if err != nil {
		return v, err
	}
	_ = json.Unmarshal(params, &v.Params)
	_ = json.Unmarshal(findings, &v.Findings)
	_ = json.Unmarshal(accepted, &v.Accepted)
	if v.Params == nil {
		v.Params = []ScriptParam{}
	}
	if v.Findings == nil {
		v.Findings = []ScriptFinding{}
	}
	if v.Accepted == nil {
		v.Accepted = []AcceptedFinding{}
	}
	return v, nil
}

func (s *Store) scriptVersion(ctx context.Context, id string) (ScriptVersion, error) {
	if !rePreviewID.MatchString(id) {
		return ScriptVersion{}, errNoScript
	}
	return scanScriptVersion(s.db(ctx).QueryRow(ctx, `SELECT `+scriptVersionCols+`
		FROM software_script_versions v JOIN software_scripts s ON s.id = v.script_id WHERE v.id = $1::uuid`, id))
}

func (s *Store) script(ctx context.Context, id string) (SoftwareScript, error) {
	var sc SoftwareScript
	if !rePreviewID.MatchString(id) {
		return sc, errNoScript
	}
	err := s.db(ctx).QueryRow(ctx, `SELECT id::text, name, platform, description, created_by, created_at
		FROM software_scripts WHERE id = $1::uuid AND archived_at IS NULL`, id).Scan(&sc.ID, &sc.Name, &sc.Platform,
		&sc.Description, &sc.CreatedBy, &sc.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, errNoScript
	}
	if err != nil {
		return sc, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+scriptVersionCols+`
		FROM software_script_versions v JOIN software_scripts s ON s.id = v.script_id
		WHERE v.script_id = $1::uuid ORDER BY v.version DESC`, id)
	if err != nil {
		return sc, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanScriptVersion(rows)
		if err != nil {
			return sc, err
		}
		sc.Versions = append(sc.Versions, v)
	}
	if err := rows.Err(); err != nil {
		return sc, err
	}
	sc.summarize()
	return sc, nil
}

func (sc *SoftwareScript) summarize() {
	for i := range sc.Versions {
		v := sc.Versions[i]
		if sc.Latest == nil {
			sc.Latest = &v
		}
		if sc.Reviewed == nil && v.ReviewedBy != nil {
			sc.Reviewed = &v
		}
	}
}

// scripts is the library with each entry's newest and newest reviewed
// versions, without the texts of the others.
func (s *Store) scripts(ctx context.Context) ([]SoftwareScript, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id::text FROM software_scripts WHERE archived_at IS NULL ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []SoftwareScript{}
	for _, id := range ids {
		sc, err := s.script(ctx, id)
		if errors.Is(err, errNoScript) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc.Versions = nil
		out = append(out, sc)
	}
	return out, nil
}

func (s *Store) createScript(ctx context.Context, name, platform, description, by string, v ScriptVersion) (SoftwareScript, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return SoftwareScript{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO software_scripts (name, platform, description, created_by)
		VALUES ($1,$2,$3,$4) RETURNING id::text`, name, platform, description, by).Scan(&id); err != nil {
		return SoftwareScript{}, err
	}
	if err := insertScriptVersion(ctx, tx, id, by, &v); err != nil {
		return SoftwareScript{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SoftwareScript{}, err
	}
	return s.script(ctx, id)
}

func (s *Store) addScriptVersion(ctx context.Context, scriptID, by string, v *ScriptVersion) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One version number at a time per script.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM software_scripts WHERE id = $1::uuid FOR UPDATE`, scriptID); err != nil {
		return err
	}
	if err := insertScriptVersion(ctx, tx, scriptID, by, v); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertScriptVersion(ctx context.Context, tx pgx.Tx, scriptID, by string, v *ScriptVersion) error {
	params, _ := json.Marshal(v.Params)
	findings, _ := json.Marshal(v.Findings)
	return tx.QueryRow(ctx, `
		INSERT INTO software_script_versions (script_id, version, install, uninstall, check_text, params, run_as,
		       timeout_seconds, reboot, install_sha, uninstall_sha, check_sha, findings, blocked, note, author)
		VALUES ($1::uuid, (SELECT COALESCE(max(version), 0) + 1 FROM software_script_versions WHERE script_id = $1::uuid),
		        $2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10,$11,$12::jsonb,$13,$14,$15)
		RETURNING id::text, version, created_at`, scriptID, v.Install, v.Uninstall, v.Check, params, v.RunAs,
		v.TimeoutSeconds, v.Reboot, v.InstallSHA, v.UninstallSHA, v.CheckSHA, findings, v.Blocked, v.Note, by).
		Scan(&v.ID, &v.Version, &v.CreatedAt)
}

// failUnsentRun fails a queued run the control plane cannot send.
func (s *Store) failUnsentRun(ctx context.Context, host, id, msg string) (*PackageRun, error) {
	r, err := scanPackageRun(s.db(ctx).QueryRow(ctx, `
		UPDATE package_runs SET state = 'failed', step = '', message = $3, finished_at = now(), updated_at = now()
		 WHERE id::text = $2 AND lower(hostname) = lower($1) AND state IN ('queued','running')
		RETURNING `+packageRunCols, host, id, clip("Not sent to the server: "+msg, 1000)), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

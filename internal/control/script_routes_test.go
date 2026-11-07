package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Install scripts end to end on a real database: written, checked, reviewed by
someone else, signed, tried outside production, and carried on the
heartbeat exactly as reviewed with the secrets sealed until then.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run ScriptLibrary
*/

const acmeInstall = `#!/bin/bash
curl -fsSL "https://downloads.acme.example/agent-${ARGUS_PARAM_VERSION:?}.deb" -o /tmp/acme.deb
dpkg -i /tmp/acme.deb
`

func TestScriptLibraryEndToEnd(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	ctx := context.Background()
	author, _ := newAccount(t, e.store, "admin")
	reviewer, _ := newAccount(t, e.store, "admin")
	auditor, _ := newAccount(t, e.store, "auditor")
	op, _ := newAccount(t, e.store, "operator")
	e.api.UserTokens["author"], e.api.UserTokens["reviewer"] = author, reviewer
	e.api.UserTokens["auditor"], e.api.UserTokens["op"] = auditor, op
	name := unique("Acme agent")

	post := func(path, token, body string, want int) map[string]any {
		t.Helper()
		w := do(t, e.h, http.MethodPost, path, token, body)
		if w.Code != want {
			t.Fatalf("POST %s: %d %s, want %d", path, w.Code, w.Body, want)
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	scriptOf := func(m map[string]any) SoftwareScript {
		var s SoftwareScript
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &s)
		return s
	}

	// Operators cannot write scripts.
	post("/api/v1/software/scripts", "op", `{}`, http.StatusForbidden)

	// A script with a blocking finding is kept, so its author sees why, and
	// cannot be reviewed.
	bad, _ := json.Marshal(map[string]any{"name": name, "platform": "linux", "version": map[string]any{
		"install": map[string]string{"text": acmeInstall + "echo 'x ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers\n"}}})
	s := scriptOf(post("/api/v1/software/scripts", "author", string(bad), http.StatusCreated))
	t.Cleanup(func() {
		_, _ = e.store.pool.Exec(context.Background(), `UPDATE software_scripts SET archived_at = now() WHERE id = $1::uuid`, s.ID)
	})
	if s.Latest == nil || !s.Latest.Blocked || s.Latest.Version != 1 {
		t.Fatalf("v1 = %+v", s.Latest)
	}
	post("/api/v1/software/script-versions/"+s.Latest.ID+"/review", "reviewer", `{}`, http.StatusConflict)

	// v2: clean but for a download it runs unchecked (a warning), with a
	// version and a secret token.
	v2, _ := json.Marshal(map[string]any{
		"install":   map[string]string{"text": acmeInstall},
		"uninstall": map[string]string{"text": "apt-get remove -y acme-agent\n"},
		"check":     map[string]string{"text": "dpkg-query -W -f='${Version}' acme-agent\n"},
		"params": []map[string]any{{"name": "version", "label": "Version", "kind": "text", "required": true},
			{"name": "TOKEN", "label": "Token", "kind": "secret"}},
		"timeoutMinutes": 20,
	})
	s = scriptOf(post("/api/v1/software/scripts/"+s.ID+"/versions", "author", string(v2), http.StatusCreated))
	v := s.Latest
	if v.Version != 2 || v.Blocked || v.InstallSHA != tunnel.ScriptHash(acmeInstall) || v.Params[0].Name != "VERSION" {
		t.Fatalf("v2 = %+v", v)
	}
	var warning *ScriptFinding
	for i, f := range v.Findings {
		if f.Level == LevelWarning {
			warning = &v.Findings[i]
		}
	}
	if warning == nil {
		t.Fatalf("no warning for the unchecked download: %+v", v.Findings)
	}

	asset, cred := scriptServer(t, e, "staging")
	prod, prodCred := scriptServer(t, e, "production")
	run := func(a Asset, token string, want int) PackageRun {
		t.Helper()
		body := `{"versionId":"` + v.ID + `","action":"install","params":{"VERSION":"2.1.0","TOKEN":"s3cr3t-token"}}`
		w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+a.ID+"/software/script-runs", token, body)
		if w.Code != want {
			t.Fatalf("run on %s: %d %s, want %d", a.Hostname, w.Code, w.Body, want)
		}
		var r PackageRun
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		return r
	}
	// Not reviewed: runs nowhere.
	run(asset, "author", http.StatusConflict)

	// The author cannot review their own; the reviewer must say why each
	// warning is fine.
	post("/api/v1/software/script-versions/"+v.ID+"/review", "author", `{}`, http.StatusForbidden)
	post("/api/v1/software/script-versions/"+v.ID+"/review", "reviewer", `{}`, http.StatusBadRequest)
	accept, _ := json.Marshal(map[string]any{"accepted": []AcceptedFinding{{Key: warning.Key(),
		Reason: "the .deb is signed by Acme's repository key"}}})
	s = scriptOf(post("/api/v1/software/script-versions/"+v.ID+"/review", "reviewer", string(accept), http.StatusOK))
	if s.Reviewed == nil || s.Reviewed.ID != v.ID || *s.Reviewed.ReviewedBy != reviewer {
		t.Fatalf("reviewed = %+v", s.Reviewed)
	}
	if a := lastAuditOn(t, e.store, name); a.Action != "software.script_reviewed" || !strings.Contains(a.Detail, "signed by Acme") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}

	// Production first needs a successful run elsewhere.
	if w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+prod.ID+"/software/script-runs", "author",
		`{"versionId":"`+v.ID+`","params":{"VERSION":"2.1.0"}}`); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "outside production") {
		t.Fatalf("production before a try: %d %s", w.Code, w.Body)
	}
	// Bad values never become a run.
	if w := do(t, e.h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/software/script-runs", "author",
		`{"versionId":"`+v.ID+`","params":{"VERSION":"1","PATH":"/tmp"}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown parameter: %d %s", w.Code, w.Body)
	}

	r := run(asset, "author", http.StatusAccepted)
	if r.Mode != PackagesScript || r.ScriptAction != "install" || r.ScriptParams["VERSION"] != "2.1.0" || r.ScriptParams["TOKEN"] != "" {
		t.Fatalf("run = %+v", r)
	}
	if a := lastAuditOn(t, e.store, asset.Hostname); a.Action != "software.script_requested" ||
		!strings.Contains(a.Detail, "TOKEN=(secret)") || strings.Contains(a.Detail, "s3cr3t") {
		t.Errorf("audit = %s: %s", a.Action, a.Detail)
	}
	var sealed []byte
	_ = e.store.pool.QueryRow(ctx, `SELECT script_secrets FROM package_runs WHERE id = $1::uuid`, r.ID).Scan(&sealed)
	if len(sealed) == 0 || strings.Contains(string(sealed), "s3cr3t") {
		t.Fatalf("the secret is not sealed: %q", sealed)
	}

	// The heartbeat carries exactly the reviewed text, signed.
	o := scriptBeat(t, e, cred, asset.Hostname, nil)
	if o == nil || o.Run == nil || o.Run.ID != r.ID || o.Run.Script == nil {
		t.Fatalf("order = %+v", o)
	}
	so := o.Run.Script
	if so.Text != acmeInstall || so.Params["TOKEN"] != "s3cr3t-token" || len(so.Secret) != 1 || so.TimeoutSec != 1200 {
		t.Fatalf("script order = %+v", so)
	}
	got, err := tunnel.VerifyScript(e.api.Tunnels.GrantKey.Public().(ed25519.PublicKey), so.Approval)
	if err != nil || got.InstallSHA != tunnel.ScriptHash(so.Text) || got.ReviewedBy != reviewer || got.Name != name {
		t.Fatalf("approval = %+v, %v", got, err)
	}

	// It ends well: the version counts as tried, and the secret is gone.
	now := time.Now().UTC()
	scriptBeat(t, e, cred, asset.Hostname, &PackageRunStatus{ID: r.ID, State: "done", Message: "Acme agent v2 finished",
		Upgraded: []PackageChange{{Name: "acme-agent", To: "2.1.0"}}, StartedAt: &now, FinishedAt: &now,
		Script: &tunnel.ScriptReport{Packages: []tunnel.SoftwareChange{{Kind: "add", Name: "acme-agent", To: "2.1.0"}},
			PortsOpened: []int{9100}, Checked: true, Installed: true, CheckVersion: "2.1.0"}})
	sealed = nil
	_ = e.store.pool.QueryRow(ctx, `SELECT script_secrets FROM package_runs WHERE id = $1::uuid`, r.ID).Scan(&sealed)
	if sealed != nil {
		t.Error("the secret outlived the run")
	}
	w := do(t, e.h, http.MethodGet, "/api/v1/assets/"+asset.ID+"/software/runs", "auditor", "")
	var list struct {
		Runs []PackageRun `json:"runs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Runs) != 1 || list.Runs[0].ScriptReport == nil || list.Runs[0].ScriptReport.PortsOpened[0] != 9100 ||
		list.Runs[0].ScriptName != name+" v2" {
		t.Fatalf("runs: %s", w.Body)
	}
	w = do(t, e.h, http.MethodGet, "/api/v1/software/scripts/"+s.ID, "auditor", "")
	s = SoftwareScript{}
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	if s.Reviewed == nil || s.Reviewed.TriedOn == nil || *s.Reviewed.TriedOn != asset.Hostname {
		t.Fatalf("tried = %s", w.Body)
	}
	// Now production may run it.
	run(prod, "author", http.StatusAccepted)
	_ = prodCred
}

func scriptServer(t *testing.T, e *tunnelEnv, tag string) (Asset, string) {
	t.Helper()
	host := unique("script-"+tag) + ".example"
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(context.Background(), AssetInput{Hostname: host, Address: "192.0.2.40", Port: 22,
		Protocol: "ssh", Principals: []string{"ubuntu"}, Tags: []string{tag}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.store.pool.Exec(context.Background(), `DELETE FROM package_runs WHERE lower(hostname) = lower($1)`, host)
		_ = e.store.ArchiveAsset(context.Background(), asset.ID)
	})
	scriptBeat(t, e, cred, host, nil)
	return asset, cred
}

func scriptBeat(t *testing.T, e *tunnelEnv, cred, host string, st *PackageRunStatus) *PackageOrders {
	t.Helper()
	b := PackageBeat{Supported: true, Report: pkgReport(time.Now().UTC()), Run: st}
	raw, _ := json.Marshal(map[string]any{"hostname": host, "version": "1.2.0", "active_sessions": 0, "packages": b})
	w := do(t, e.h, http.MethodPost, "/api/v1/report/heartbeat", cred, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out struct {
		Packages *PackageOrders `json:"packages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.Packages
}

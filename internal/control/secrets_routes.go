package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/vault"
)

/*
Routes for secrets (secrets_servers.go).

Servers and rules are for admins and owners:

	GET    /api/v1/secrets/servers                 servers, this organization's id and key to configure them with
	POST   /api/v1/secrets/servers                 add one; it must answer, with this organization, first
	PUT    /api/v1/secrets/servers/{id}            change one
	DELETE /api/v1/secrets/servers/{id}            forget one; its secrets stay on it
	POST   /api/v1/secrets/servers/{id}/sync       contact it now
	POST   /api/v1/secrets/servers/{id}/log/resume collect its log again after a gap, from where it is now
	GET    /api/v1/secrets/rules                   who may do what
	POST   /api/v1/secrets/rules                   add a rule
	PUT    /api/v1/secrets/rules/{id}              change one
	DELETE /api/v1/secrets/rules/{id}              remove one

Secrets themselves are for anyone signed in, as far as the rules let them:
argus-control relays each request to the server that keeps the name, signed,
and naming the person, and argus-secret decides. argus-control sees names and
metadata, never a value: a value read is sealed to a key the browser made,
and a value written is sealed by the browser to the server's key.

	GET    /api/v1/secrets?prefix=&deleted=1       names the person may see, from every server
	GET    /api/v1/secrets/item?name=              one secret's metadata and versions
	GET    /api/v1/secrets/seal-key?name=          the key to seal a new value for name to
	PUT    /api/v1/secrets/item                    a new secret or version (vault.PutRequest)
	PATCH  /api/v1/secrets/item                    description, owner, tags (vault.MetaRequest)
	POST   /api/v1/secrets/read                    a value, sealed to the browser (vault.ConsoleRead)
	POST   /api/v1/secrets/label                   move a label (vault.LabelRequest)
	POST   /api/v1/secrets/delete                  schedule a deletion (vault.DeleteRequest)
	POST   /api/v1/secrets/restore                 undo one ({name})
*/

func (a *API) secretsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/secrets/servers", a.getSecretsServers)
	mux.HandleFunc("POST /api/v1/secrets/servers", a.postSecretsServer)
	mux.HandleFunc("PUT /api/v1/secrets/servers/{id}", a.putSecretsServer)
	mux.HandleFunc("DELETE /api/v1/secrets/servers/{id}", a.deleteSecretsServer)
	mux.HandleFunc("POST /api/v1/secrets/servers/{id}/sync", a.postSecretsSync)
	mux.HandleFunc("POST /api/v1/secrets/servers/{id}/log/resume", a.postSecretsLogResume)
	mux.HandleFunc("GET /api/v1/secrets/rules", a.getSecretRules)
	mux.HandleFunc("POST /api/v1/secrets/rules", a.postSecretRule)
	mux.HandleFunc("PUT /api/v1/secrets/rules/{id}", a.putSecretRule)
	mux.HandleFunc("DELETE /api/v1/secrets/rules/{id}", a.deleteSecretRule)

	mux.HandleFunc("GET /api/v1/secrets/requests", a.getSecretRequests)
	mux.HandleFunc("POST /api/v1/secrets/requests", a.postSecretRequest)
	mux.HandleFunc("POST /api/v1/secrets/requests/{id}/decide", a.postSecretRequestDecide)
	mux.HandleFunc("POST /api/v1/secrets/requests/{id}/cancel", a.postSecretRequestCancel)
	mux.HandleFunc("GET /api/v1/secrets/apps", a.getSecretApps)
	mux.HandleFunc("POST /api/v1/secrets/apps", a.postSecretApp)
	mux.HandleFunc("DELETE /api/v1/secrets/apps/{id}", a.deleteSecretApp)

	mux.HandleFunc("GET /api/v1/secrets", a.getSecrets)
	mux.HandleFunc("GET /api/v1/secrets/item", a.relaySecret(http.MethodGet, "/manage/v1/secret"))
	mux.HandleFunc("GET /api/v1/secrets/seal-key", a.getSecretsSealKey)
	mux.HandleFunc("PUT /api/v1/secrets/item", a.relaySecret(http.MethodPut, "/manage/v1/secret"))
	mux.HandleFunc("PATCH /api/v1/secrets/item", a.relaySecret(http.MethodPatch, "/manage/v1/secret"))
	mux.HandleFunc("POST /api/v1/secrets/read", a.relaySecret(http.MethodPost, "/manage/v1/read"))
	mux.HandleFunc("POST /api/v1/secrets/label", a.relaySecret(http.MethodPost, "/manage/v1/label"))
	mux.HandleFunc("POST /api/v1/secrets/delete", a.relaySecret(http.MethodPost, "/manage/v1/delete"))
	mux.HandleFunc("POST /api/v1/secrets/restore", a.relaySecret(http.MethodPost, "/manage/v1/restore"))
}

func (a *API) secretsAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "only an admin or owner can see or change secrets servers and rules")
		return "", false
	}
	return sess.Email, true
}

func (a *API) secretsErr(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidSecrets
	var se SecretsError
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.msg)
	case errors.Is(err, ErrNoSecretsServer):
		writeErr(w, http.StatusNotFound, "no such secrets server")
	case errors.Is(err, ErrNoSecretRule):
		writeErr(w, http.StatusNotFound, "no such secret rule")
	case errors.Is(err, ErrNoServerForSecret):
		writeErr(w, http.StatusNotFound, "no secrets server keeps that folder")
	case errors.As(err, &se):
		// The server's own answer: a refusal, a missing secret, a bad value.
		// Its 5xx stays a 502, so the console says the server failed, not us.
		status := se.Status
		if status >= 500 {
			status = http.StatusBadGateway
		}
		writeErr(w, status, se.Msg)
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "did not answer"):
		writeErr(w, http.StatusBadGateway, err.Error())
	default:
		a.fail(w, what, err)
	}
}

/* ── Servers ────────────────────────────────────────────────────────────── */

type secretsServersView struct {
	// Organization and ControlPublicKey are what a server's configuration
	// needs: organization and control_public_key.
	Organization     string          `json:"organization"`
	ControlPublicKey string          `json:"controlPublicKey"`
	Servers          []SecretsServer `json:"servers"`
}

func (a *API) getSecretsServers(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.secretsAdmin(w, r); !ok {
		return
	}
	key, err := a.SecretsPublicKey(r.Context())
	if err != nil {
		a.secretsErr(w, "secrets key", err)
		return
	}
	srvs, err := a.store.SecretsServers(r.Context())
	if err != nil {
		a.secretsErr(w, "list secrets servers", err)
		return
	}
	writeJSON(w, http.StatusOK, secretsServersView{Organization: tenantIDOf(r.Context()), ControlPublicKey: key, Servers: srvs})
}

// readSecretsServer reads a server from the request and checks it answers,
// signed, for this organization, before anything is saved.
func (a *API) readSecretsServer(r *http.Request) (SecretsServerInput, vault.Status, error) {
	var in SecretsServerInput
	if err := decode(r, &in); err != nil {
		return in, vault.Status{}, ErrInvalidSecrets{err.Error()}
	}
	if err := in.normalize(); err != nil {
		return in, vault.Status{}, err
	}
	st, err := a.secretsStatus(r.Context(), SecretsServer{Name: in.Name, URL: in.URL, TLSPin: in.TLSPin})
	if err != nil {
		var invalid ErrInvalidSecrets
		if errors.As(err, &invalid) {
			return in, st, err
		}
		return in, st, ErrInvalidSecrets{"argus-control could not reach it: " + err.Error()}
	}
	return in, st, nil
}

func (a *API) postSecretsServer(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	in, st, err := a.readSecretsServer(r)
	if err != nil {
		a.secretsErr(w, "add secrets server", err)
		return
	}
	srv, err := a.store.SaveSecretsServer(r.Context(), "", in, st.SealKey, st.Version, actor)
	if err != nil {
		a.secretsErr(w, "add secrets server", err)
		return
	}
	a.audit(r, "secrets.server.add", actor, "secrets server "+srv.Name,
		fmt.Sprintf("Added secrets server %s at %s, keeping %s.", srv.Name, srv.URL, strings.Join(srv.Folders, ", ")))
	a.syncSoon(r.Context(), srv.ID)
	writeJSON(w, http.StatusCreated, srv)
}

func (a *API) putSecretsServer(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	before, err := a.store.SecretsServer(r.Context(), r.PathValue("id"))
	if err != nil {
		a.secretsErr(w, "change secrets server", err)
		return
	}
	in, st, err := a.readSecretsServer(r)
	if err != nil {
		a.secretsErr(w, "change secrets server", err)
		return
	}
	srv, err := a.store.SaveSecretsServer(r.Context(), before.ID, in, st.SealKey, st.Version, actor)
	if err != nil {
		a.secretsErr(w, "change secrets server", err)
		return
	}
	a.audit(r, "secrets.server.change", actor, "secrets server "+srv.Name,
		fmt.Sprintf("Changed secrets server %s: at %s (was %s), keeping %s (was %s).", srv.Name, srv.URL, before.URL,
			strings.Join(srv.Folders, ", "), strings.Join(before.Folders, ", ")))
	a.syncSoon(r.Context(), srv.ID)
	writeJSON(w, http.StatusOK, srv)
}

func (a *API) deleteSecretsServer(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	srv, err := a.store.RemoveSecretsServer(r.Context(), r.PathValue("id"))
	if err != nil {
		a.secretsErr(w, "remove secrets server", err)
		return
	}
	a.audit(r, "secrets.server.remove", actor, "secrets server "+srv.Name,
		fmt.Sprintf("Removed secrets server %s at %s. Its secrets stay on it; argus-control no longer pushes it rules, so it refuses every read once they go stale.", srv.Name, srv.URL))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) postSecretsSync(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.secretsAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := a.store.SecretsServer(r.Context(), id); err != nil {
		a.secretsErr(w, "sync secrets server", err)
		return
	}
	// The outcome is on the server's row either way.
	_ = a.SyncSecretsServer(r.Context(), id)
	srv, err := a.store.SecretsServer(r.Context(), id)
	if err != nil {
		a.secretsErr(w, "sync secrets server", err)
		return
	}
	writeJSON(w, http.StatusOK, srv)
}

// postSecretsLogResume takes a server's log from where the server is now,
// after a gap. What is missing stays missing; this is in the audit log, so
// whoever resumed it said so.
func (a *API) postSecretsLogResume(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "say why the gap is understood")
		return
	}
	srv, err := a.store.SecretsServer(r.Context(), r.PathValue("id"))
	if err != nil {
		a.secretsErr(w, "resume secrets log", err)
		return
	}
	if srv.LogGap == "" {
		writeErr(w, http.StatusConflict, "its log has no gap")
		return
	}
	var batch vault.LogBatch
	if err := a.secretsJSON(r.Context(), srv, http.MethodGet, "/manage/v1/log?after=0&limit=1", controlActor, nil, &batch); err != nil {
		a.secretsErr(w, "resume secrets log", err)
		return
	}
	// Take only what has not been collected: the entries after the last one
	// argus-control acknowledged, which the server still keeps.
	seq, head := int64(batch.State.Acked), batch.State.Base
	if batch.State.Acked > 0 {
		var at vault.LogBatch
		if err := a.secretsJSON(r.Context(), srv, http.MethodGet, fmt.Sprintf("/manage/v1/log?after=%d&limit=1", batch.State.Acked-1), controlActor, nil, &at); err != nil {
			a.secretsErr(w, "resume secrets log", err)
			return
		}
		if len(at.Entries) == 1 && at.Entries[0].Seq == batch.State.Acked {
			head = at.Entries[0].Hash
		}
	}
	if err := a.store.resumeSecretsLog(r.Context(), srv.ID, seq, head); err != nil {
		a.secretsErr(w, "resume secrets log", err)
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{Action: "secrets.log_resumed", Severity: "warning", ActorEmail: actor,
		Target: "secrets server " + srv.Name,
		Detail: fmt.Sprintf("Resumed collecting the read log of %s from entry %d after a gap (%s). Reason: %s", srv.Name, seq+1, srv.LogGap, strings.TrimSpace(in.Reason))}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "secrets.log_resumed")
	}
	_ = a.SyncSecretsServer(r.Context(), srv.ID)
	srv, _ = a.store.SecretsServer(r.Context(), srv.ID)
	writeJSON(w, http.StatusOK, srv)
}

// syncSoon contacts a server in the background, with the request's
// organization but not its deadline.
func (a *API) syncSoon(ctx context.Context, id string) {
	ctx = context.WithoutCancel(ctx)
	a.secretsBusy.Add(1)
	go func() {
		defer a.secretsBusy.Done()
		_ = a.SyncSecretsServer(ctx, id)
	}()
}

// syncAllSoon pushes changed rules to every server.
func (a *API) syncAllSoon(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	a.secretsBusy.Add(1)
	go func() {
		defer a.secretsBusy.Done()
		srvs, err := a.store.SecretsServers(ctx)
		if err != nil {
			return
		}
		for _, s := range srvs {
			_ = a.SyncSecretsServer(ctx, s.ID)
		}
	}()
}

/* ── Rules ──────────────────────────────────────────────────────────────── */

func (a *API) getSecretRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.secretsAdmin(w, r); !ok {
		return
	}
	rules, err := a.store.SecretRules(r.Context())
	if err != nil {
		a.secretsErr(w, "list secret rules", err)
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func describeSecretRule(r SecretRule) string {
	path := r.Path
	if path == "" {
		path = "every secret"
	}
	subs := make([]string, 0, len(r.Subjects))
	for _, s := range r.Subjects {
		subs = append(subs, s.Kind+" "+s.ID)
	}
	out := fmt.Sprintf("%s may %s %s", strings.Join(subs, ", "), strings.Join(r.Can, ", "), path)
	if r.NeedsApproval {
		out += ", with an approval"
	}
	return out
}

func (a *API) saveSecretRule(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	var in SecretRule
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.normalize(); err != nil {
		a.secretsErr(w, "save secret rule", err)
		return
	}
	out, err := a.store.SaveSecretRule(r.Context(), id, in, actor)
	if err != nil {
		a.secretsErr(w, "save secret rule", err)
		return
	}
	action, status := "secrets.rule.add", http.StatusCreated
	if id != "" {
		action, status = "secrets.rule.change", http.StatusOK
	}
	a.audit(r, action, actor, "secret rule "+out.ID, describeSecretRule(out)+".")
	a.syncAllSoon(r.Context())
	writeJSON(w, status, out)
}

func (a *API) postSecretRule(w http.ResponseWriter, r *http.Request) { a.saveSecretRule(w, r, "") }

func (a *API) putSecretRule(w http.ResponseWriter, r *http.Request) {
	if !validUUID(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "no such secret rule")
		return
	}
	a.saveSecretRule(w, r, r.PathValue("id"))
}

func (a *API) deleteSecretRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.secretsAdmin(w, r)
	if !ok {
		return
	}
	rule, err := a.store.RemoveSecretRule(r.Context(), r.PathValue("id"))
	if err != nil {
		a.secretsErr(w, "remove secret rule", err)
		return
	}
	a.audit(r, "secrets.rule.remove", actor, "secret rule "+rule.ID, "Removed: "+describeSecretRule(rule)+".")
	a.syncAllSoon(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

/* ── Secrets, relayed ───────────────────────────────────────────────────── */

// secretsPerson is the signed-in person, as argus-secret is told about them.
// Approved is set by relaySecret for a read with an approved request
// (secrets_requests.go).
func (a *API) secretsPerson(r *http.Request, sess auth.Session) (vault.ManageActor, error) {
	teams, err := a.store.secretsTeamsOf(r.Context(), sess.Email)
	if err != nil {
		return vault.ManageActor{}, err
	}
	return vault.ManageActor{Kind: vault.ActorPerson, ID: strings.ToLower(sess.Email), Name: sess.Name,
		Teams: teams, Role: sess.Role, Remote: a.clientAddr(r)}, nil
}

// secretItem is a secret with the server it lives on.
type secretItem struct {
	vault.Secret
	Server   string `json:"server"`
	ServerID string `json:"serverId"`
}

// getSecrets lists, from every server that could hold names under prefix,
// what the person may see. A server that cannot answer is named, not hidden.
func (a *API) getSecrets(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if err := vault.ValidPrefix(prefix); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	actor, err := a.secretsPerson(r, sess)
	if err != nil {
		a.secretsErr(w, "list secrets", err)
		return
	}
	srvs, err := a.store.SecretsServers(r.Context())
	if err != nil {
		a.secretsErr(w, "list secrets", err)
		return
	}
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if r.URL.Query().Get("deleted") == "1" {
		q.Set("deleted", "1")
	}
	uri := "/manage/v1/secrets"
	if len(q) > 0 {
		uri += "?" + q.Encode()
	}
	type failed struct {
		Server string `json:"server"`
		Error  string `json:"error"`
	}
	out := struct {
		Secrets     []secretItem `json:"secrets"`
		Unreachable []failed     `json:"unreachable"`
	}{Secrets: []secretItem{}, Unreachable: []failed{}}
	for _, srv := range srvs {
		if prefix != "" && !containsString(srv.Folders, vault.Folder(prefix)) {
			continue
		}
		var secs []vault.Secret
		if err := a.secretsJSON(r.Context(), srv, http.MethodGet, uri, actor, nil, &secs); err != nil {
			out.Unreachable = append(out.Unreachable, failed{Server: srv.Name, Error: err.Error()})
			continue
		}
		for _, s := range secs {
			// A server is only believed about the folders it keeps.
			if containsString(srv.Folders, vault.Folder(s.Name)) {
				out.Secrets = append(out.Secrets, secretItem{Secret: s, Server: srv.Name, ServerID: srv.ID})
			}
		}
	}
	sort.Slice(out.Secrets, func(i, j int) bool { return out.Secrets[i].Name < out.Secrets[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// getSecretsSealKey is the key a new value for name is sealed to, in the
// browser, before it is sent. It comes from the server's own status, which
// argus-control read over a pinned, signed exchange.
func (a *API) getSecretsSealKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authenticate(r); !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	name := r.URL.Query().Get("name")
	if err := vault.ValidName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	srv, err := a.store.SecretsServerFor(r.Context(), name)
	if err != nil {
		a.secretsErr(w, "seal key", err)
		return
	}
	if srv.SealKey == "" {
		writeErr(w, http.StatusConflict, srv.Name+" has not been reached yet")
		return
	}
	// The browser seals with secretwire.WriteAAD(name) as the AAD.
	writeJSON(w, http.StatusOK, map[string]string{"server": srv.Name, "sealKey": srv.SealKey})
}

// relaySecret forwards one request about a single secret to the server that
// keeps it, signed for the signed-in person. The name is in the query for a
// GET and in the body otherwise; the body goes on as it came, so a sealed
// value is never opened or changed here.
func (a *API) relaySecret(method, managePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var body []byte
		name := r.URL.Query().Get("name")
		uri := managePath
		if method == http.MethodGet {
			uri += "?" + url.Values{"name": {name}}.Encode()
		} else {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				writeErr(w, http.StatusRequestEntityTooLarge, "the request is larger than argus-control relays")
				return
			}
			var named struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &named); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			name, body = named.Name, raw
		}
		if err := vault.ValidName(name); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		srv, err := a.store.SecretsServerFor(r.Context(), name)
		if err != nil {
			a.secretsErr(w, "relay secret", err)
			return
		}
		actor, err := a.secretsPerson(r, sess)
		if err != nil {
			a.secretsErr(w, "relay secret", err)
			return
		}
		if managePath == "/manage/v1/read" {
			// A read a rule holds for an approval goes through only with an
			// approved request of this person's for this secret, unexpired.
			if actor.Approved, err = a.store.secretReadApproved(r.Context(), sess.Email, name); err != nil {
				a.secretsErr(w, "relay secret", err)
				return
			}
		}
		raw, err := a.secretsCall(r.Context(), srv, method, uri, actor, body)
		if err != nil {
			a.secretsErr(w, "relay secret", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}
}

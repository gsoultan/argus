package control

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/secretwire"
	"github.com/gsoultan/argus/internal/tlsconfig"
	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/argus/internal/vault"
)

// fakeSecretServer is a real argus-secret, in process, on TLS.
type fakeSecretServer struct {
	store *vault.Store
	rules *vault.Rules
	srv   *httptest.Server
	pin   string
}

func newSecretServer(t *testing.T, org, controlKey string) *fakeSecretServer {
	t.Helper()
	root := make([]byte, vault.KeySize)
	_, _ = rand.Read(root)
	store, err := vault.Open(t.TempDir(), root, org)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key, err := vault.ParseControlKey(controlKey)
	if err != nil {
		t.Fatal(err)
	}
	rules := vault.NewRules(15 * time.Minute)
	api, err := vault.NewAPI(vault.APIOptions{Store: store, Rules: rules, ControlKey: key,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(api.Handler())
	t.Cleanup(srv.Close)
	return &fakeSecretServer{store: store, rules: rules, srv: srv, pin: tlsconfig.Pin(srv.Certificate())}
}

// TestSecretsServersEndToEnd: argus-control adds a real argus-secret, pushes
// it rules naming an enrolled agent, relays a person's write and sealed read,
// and collects the read log into its own audit log. Most of it is refusals.
func TestSecretsServersEndToEnd(t *testing.T) {
	h := newTenantHarness(t)
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	h.api.Tunnels = &TunnelHub{GrantKey: key}
	t.Cleanup(h.api.secretsBusy.Wait)
	tn, owner, pw := newTenant(t, h.store)
	ownerC := h.signIn(t, tn.Slug, owner, pw)
	ctx := WithTenant(context.Background(), tn)
	call := func(cl *http.Client, method, path string, body any) (int, string) {
		t.Helper()
		b := ""
		if body != nil {
			raw, _ := json.Marshal(body)
			b = string(raw)
		}
		code, text := h.call(t, cl, "", method, path, b)
		// Background syncs a change started finish before the test looks.
		h.api.secretsBusy.Wait()
		return code, text
	}

	opEmail := unique("op") + "@northwind.id"
	if _, err := h.store.CreateAccount(ctx, opEmail, "Op", "operator", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	opC := h.signIn(t, tn.Slug, opEmail, "correct horse battery staple")
	if code, _ := call(opC, "GET", "/api/v1/secrets/servers", nil); code != http.StatusForbidden {
		t.Errorf("an operator reading secrets servers = %d, want 403", code)
	}

	code, text := call(ownerC, "GET", "/api/v1/secrets/servers", nil)
	var view secretsServersView
	if code != http.StatusOK || json.Unmarshal([]byte(text), &view) != nil || view.ControlPublicKey == "" {
		t.Fatalf("servers = %d %s", code, text)
	}
	if view.Organization != tn.ID {
		t.Fatalf("organization = %q, want %q", view.Organization, tn.ID)
	}
	vs := newSecretServer(t, tn.ID, view.ControlPublicKey)

	// Refused before anything is saved.
	otherKey := newSecretServer(t, tn.ID, "ed25519:"+strings.Repeat("A", 43)+"=")
	wrongOrg := newSecretServer(t, "someone-else", view.ControlPublicKey)
	for name, in := range map[string]SecretsServerInput{
		"plain http":     {Name: "Vault", URL: strings.Replace(vs.srv.URL, "https", "http", 1), TLSPin: vs.pin, Folders: []string{"prod"}},
		"wrong pin":      {Name: "Vault", URL: vs.srv.URL, TLSPin: "sha256:" + strings.Repeat("0", 64), Folders: []string{"prod"}},
		"no pin":         {Name: "Vault", URL: vs.srv.URL, Folders: []string{"prod"}},
		"another key":    {Name: "Vault", URL: otherKey.srv.URL, TLSPin: otherKey.pin, Folders: []string{"prod"}},
		"another org":    {Name: "Vault", URL: wrongOrg.srv.URL, TLSPin: wrongOrg.pin, Folders: []string{"prod"}},
		"no folders":     {Name: "Vault", URL: vs.srv.URL, TLSPin: vs.pin},
		"nested folder":  {Name: "Vault", URL: vs.srv.URL, TLSPin: vs.pin, Folders: []string{"prod/db"}},
		"capital folder": {Name: "Vault", URL: vs.srv.URL, TLSPin: vs.pin, Folders: []string{"Prod"}},
	} {
		if code, text := call(ownerC, "POST", "/api/v1/secrets/servers", in); code != http.StatusBadRequest {
			t.Errorf("%s: add = %d %s, want 400", name, code, text)
		}
	}
	if code, _ := call(opC, "POST", "/api/v1/secrets/servers", SecretsServerInput{Name: "Vault", URL: vs.srv.URL, TLSPin: vs.pin, Folders: []string{"prod"}}); code != http.StatusForbidden {
		t.Errorf("an operator adding a server = %d, want 403", code)
	}

	code, text = call(ownerC, "POST", "/api/v1/secrets/servers", SecretsServerInput{Name: "Vault", URL: vs.srv.URL + "/", TLSPin: vs.pin, Folders: []string{"prod", "shared"}})
	var srv SecretsServer
	if code != http.StatusCreated || json.Unmarshal([]byte(text), &srv) != nil || srv.SealKey == "" {
		t.Fatalf("add = %d %s", code, text)
	}
	second := newSecretServer(t, tn.ID, view.ControlPublicKey)
	if code, text := call(ownerC, "POST", "/api/v1/secrets/servers", SecretsServerInput{Name: "Vault 2", URL: second.srv.URL, TLSPin: second.pin, Folders: []string{"prod"}}); code != http.StatusBadRequest || !strings.Contains(text, "already keeps") {
		t.Errorf("a second server for prod = %d %s, want 400", code, text)
	}

	// An agent and the rules that name it.
	tok, err := h.store.CreateEnrollToken(ctx, owner, "db-01", 0)
	if err != nil {
		t.Fatal(err)
	}
	enr, err := h.store.Enroll(ctx, tok.Token, "db-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []SecretRule{
		{Path: "prod/", Subjects: []vault.Subject{{Kind: vault.SubjectServer, ID: enr.CredentialID}}, Can: []string{vault.CanRead}},
		{Path: "prod/", Subjects: []vault.Subject{{Kind: vault.SubjectRole, ID: "owner"}}, Can: []string{vault.CanWrite, vault.CanRead}},
	} {
		if code, text := call(ownerC, "POST", "/api/v1/secrets/rules", r); code != http.StatusCreated {
			t.Fatalf("add rule = %d %s", code, text)
		}
	}
	for name, r := range map[string]SecretRule{
		"no subjects":   {Path: "prod/", Can: []string{vault.CanRead}},
		"unknown grant": {Path: "prod/", Subjects: []vault.Subject{{Kind: vault.SubjectRole, ID: "owner"}}, Can: []string{"everything"}},
		"unknown kind":  {Path: "prod/", Subjects: []vault.Subject{{Kind: "planet", ID: "earth"}}, Can: []string{vault.CanRead}},
		"bad path":      {Path: "../etc", Subjects: []vault.Subject{{Kind: vault.SubjectRole, ID: "owner"}}, Can: []string{vault.CanRead}},
	} {
		if code, _ := call(ownerC, "POST", "/api/v1/secrets/rules", r); code != http.StatusBadRequest {
			t.Errorf("%s: rule = %d, want 400", name, code)
		}
	}
	if err := h.api.SyncSecretsServer(ctx, srv.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if serial, _, fresh := vs.rules.Status(); serial == 0 || !fresh {
		t.Fatalf("rules on the server: serial %d fresh %v", serial, fresh)
	}
	agent, ok := vs.rules.Authenticate(enr.Credential)
	if !ok {
		t.Fatal("the enrolled agent is unknown to argus-secret")
	}
	if d, _ := vs.rules.Allow(agent, vault.CanRead, "prod/db"); !d.Allowed {
		t.Error("the agent may not read prod/db")
	}
	if d, _ := vs.rules.Allow(agent, vault.CanRead, "shared/x"); d.Allowed {
		t.Error("the agent may read shared/x, which no rule grants")
	}

	// The owner writes a value sealed in "the browser".
	code, text = call(ownerC, "GET", "/api/v1/secrets/seal-key?name=prod/db", nil)
	var sk struct{ SealKey string }
	if code != http.StatusOK || json.Unmarshal([]byte(text), &sk) != nil {
		t.Fatalf("seal key = %d %s", code, text)
	}
	pub, err := secretwire.ParseRecipient(sk.SealKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secretwire.SealTo(pub, []byte("hunter2"), secretwire.WriteAAD("prod/db"))
	if err != nil {
		t.Fatal(err)
	}
	if code, text := call(ownerC, "PUT", "/api/v1/secrets/item", vault.PutRequest{Name: "prod/db", Type: vault.TypeText, SealedValue: sealed}); code != http.StatusOK {
		t.Fatalf("write = %d %s", code, text)
	}
	if code, _ := call(ownerC, "PUT", "/api/v1/secrets/item", vault.PutRequest{Name: "nowhere/db", Type: vault.TypeText, SealedValue: sealed}); code != http.StatusNotFound {
		t.Errorf("a write to a folder no server keeps = %d, want 404", code)
	}
	if code, _ := call(opC, "PUT", "/api/v1/secrets/item", vault.PutRequest{Name: "prod/db", Type: vault.TypeText, SealedValue: sealed}); code == http.StatusOK {
		t.Error("an operator with no rule wrote a secret")
	}

	// Lists: the owner sees it, the operator does not.
	code, text = call(ownerC, "GET", "/api/v1/secrets", nil)
	if code != http.StatusOK || !strings.Contains(text, `"prod/db"`) || strings.Contains(text, "hunter2") {
		t.Errorf("owner list = %d %s", code, text)
	}
	if code, text := call(opC, "GET", "/api/v1/secrets", nil); code != http.StatusOK || strings.Contains(text, "prod/db") {
		t.Errorf("operator list = %d %s, want no prod/db", code, text)
	}

	// A read sealed to a key the browser made: argus-control relays
	// ciphertext it cannot open.
	mine, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	read := vault.ConsoleRead{ReadRequest: secretwire.ReadRequest{Name: "prod/db", Recipient: secretwire.EncodeKey(mine.PublicKey().Bytes())}}
	if code, _ := call(ownerC, "POST", "/api/v1/secrets/read", read); code != http.StatusBadRequest {
		t.Errorf("a read with no reason = %d, want 400", code)
	}
	read.Reason = "rotate the app's pool"
	code, text = call(ownerC, "POST", "/api/v1/secrets/read", read)
	var got secretwire.ReadResponse
	if code != http.StatusOK || json.Unmarshal([]byte(text), &got) != nil || strings.Contains(text, "hunter2") {
		t.Fatalf("read = %d %s", code, text)
	}
	plain, err := secretwire.OpenSealed(mine, got.Sealed, secretwire.ReadAAD(got.Name, got.Version, got.Field))
	if err != nil || string(plain) != "hunter2" {
		t.Fatalf("opened %q, %v", plain, err)
	}
	if code, _ := call(opC, "POST", "/api/v1/secrets/read", read); code != http.StatusNotFound {
		t.Errorf("an operator's read = %d, want 404 (refused looks missing)", code)
	}

	// The log reaches the audit log, in order, and the server is told.
	if err := h.api.SyncSecretsServer(ctx, srv.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	events, err := h.store.AuditEvents(ctx, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	var readOK, readRefused bool
	for _, e := range events {
		if strings.Contains(e.Detail, "hunter2") {
			t.Fatalf("a value reached the audit log: %+v", e)
		}
		if e.Action == "secret.read" && e.ActorEmail == strings.ToLower(owner) && strings.Contains(e.Detail, "rotate the app's pool") {
			readOK = true
		}
		if e.Action == "secret.read" && e.ActorEmail == strings.ToLower(opEmail) && e.Severity == "warning" {
			readRefused = true
		}
	}
	if !readOK || !readRefused {
		t.Errorf("audit: owner's read %v, operator's refusal %v", readOK, readRefused)
	}
	st, err := vs.store.LogState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Acked != st.Seq || st.Seq == 0 {
		t.Errorf("server log %+v: argus-control did not acknowledge all of it", st)
	}
	srv, _ = h.store.SecretsServer(ctx, srv.ID)
	if uint64(srv.LogSeq) != st.Seq {
		t.Errorf("collected to %d, server at %d", srv.LogSeq, st.Seq)
	}

	// A server whose log no longer follows on is a gap: collection stops,
	// it is critical in the audit log, and it takes a reason to resume.
	if _, err := h.store.db(ctx).Exec(ctx, `UPDATE secrets_servers SET log_head = 'f00d' WHERE id = $1`, srv.ID); err != nil {
		t.Fatal(err)
	}
	if err := vs.store.Note(vault.Entry{Actor: vault.Actor{Kind: vault.ActorSystem, ID: "test"}, Action: "poke", Outcome: vault.OutcomeOK}); err != nil {
		t.Fatal(err)
	}
	if err := h.api.SyncSecretsServer(ctx, srv.ID); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("sync after a changed link = %v, want a gap", err)
	}
	srv, _ = h.store.SecretsServer(ctx, srv.ID)
	if srv.LogGap == "" {
		t.Fatal("no gap recorded")
	}
	if n, _ := h.store.AuditCountFor(ctx, "secrets.log_gap", "secrets server Vault"); n != 1 {
		t.Errorf("gap events = %d, want 1", n)
	}
	_ = h.api.SyncSecretsServer(ctx, srv.ID)
	if n, _ := h.store.AuditCountFor(ctx, "secrets.log_gap", "secrets server Vault"); n != 1 {
		t.Errorf("gap events after another sync = %d, want still 1", n)
	}
	if code, _ := call(ownerC, "POST", "/api/v1/secrets/servers/"+srv.ID+"/log/resume", map[string]string{"reason": ""}); code != http.StatusBadRequest {
		t.Errorf("resume with no reason = %d, want 400", code)
	}
	if code, text := call(ownerC, "POST", "/api/v1/secrets/servers/"+srv.ID+"/log/resume", map[string]string{"reason": "test changed the link"}); code != http.StatusOK || strings.Contains(text, `"logGap"`) {
		t.Fatalf("resume = %d %s", code, text)
	}

	// The agent asks where its secrets live, with its own credential only.
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/report/secrets-servers", nil)
	req.Header.Set("Authorization", "Bearer "+enr.Credential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), vs.srv.URL) || strings.Contains(string(raw), "rulesHash") {
		t.Errorf("agent's servers = %d %s", resp.StatusCode, raw)
	}
	req, _ = http.NewRequest("GET", h.srv.URL+"/api/v1/report/secrets-servers", nil)
	req.Header.Set("Authorization", "Bearer "+testReporterToken)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("the shared reporter token was told where secrets live")
		}
	}

	// An app token: shown once, pushed as its hash, gone once revoked.
	if code, _ := call(opC, "POST", "/api/v1/secrets/apps", map[string]any{"name": "CI"}); code != http.StatusForbidden {
		t.Errorf("an operator making an app token = %d, want 403", code)
	}
	if code, _ := call(ownerC, "POST", "/api/v1/secrets/apps", map[string]any{"name": "CI", "days": 400}); code != http.StatusBadRequest {
		t.Errorf("a 400-day token = %d, want 400", code)
	}
	code, text = call(ownerC, "POST", "/api/v1/secrets/apps", map[string]any{"name": "CI", "days": 30})
	var app struct {
		ID, Token string
	}
	if code != http.StatusCreated || json.Unmarshal([]byte(text), &app) != nil || !strings.HasPrefix(app.Token, vault.AppTokenPrefix) {
		t.Fatalf("make app token = %d %s", code, text)
	}
	if code, text := call(ownerC, "GET", "/api/v1/secrets/apps", nil); code != http.StatusOK || strings.Contains(text, app.Token) {
		t.Errorf("the token came back in the list: %d %s", code, text)
	}
	if err := h.api.SyncSecretsServer(ctx, srv.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if p, ok := vs.rules.Authenticate(app.Token); !ok || p.ID != app.ID {
		t.Fatalf("argus-secret does not know the app token: %+v %v", p, ok)
	}
	if code, _ := call(ownerC, "DELETE", "/api/v1/secrets/apps/"+app.ID, nil); code != http.StatusNoContent {
		t.Errorf("revoke app = %d", code)
	}
	if code, _ := call(ownerC, "DELETE", "/api/v1/secrets/apps/"+app.ID, nil); code != http.StatusNotFound {
		t.Errorf("revoke twice = %d, want 404", code)
	}
	if err := h.api.SyncSecretsServer(ctx, srv.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, ok := vs.rules.Authenticate(app.Token); ok {
		t.Error("a revoked app token is still known to argus-secret")
	}

	// Revoking the agent takes it out of the next bundle.
	if _, err := h.store.RevokeAgentCredential(ctx, enr.CredentialID, owner); err != nil {
		t.Fatal(err)
	}
	if err := h.api.SyncSecretsServer(ctx, srv.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, ok := vs.rules.Authenticate(enr.Credential); ok {
		t.Error("a revoked agent is still known to argus-secret")
	}

	// Another organization's session cannot see this one's servers.
	tn2, owner2, pw2 := newTenant(t, h.store)
	other := h.signIn(t, tn2.Slug, owner2, pw2)
	if code, text := call(other, "GET", "/api/v1/secrets/servers", nil); code != http.StatusOK || strings.Contains(text, srv.ID) {
		t.Errorf("another organization's servers = %d %s", code, text)
	}
	if code, _ := call(other, "POST", "/api/v1/secrets/read", read); code != http.StatusNotFound {
		t.Errorf("another organization's read = %d, want 404", code)
	}

	if code, _ := call(ownerC, "DELETE", "/api/v1/secrets/servers/"+srv.ID, nil); code != http.StatusNoContent {
		t.Errorf("remove = %d", code)
	}
}

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

// TestSecretsTunnelEndToEnd: a real argus-secret with no port of its own
// enrolls with a one-time token, dials argus-control, takes its rules and
// answers an agent's read relayed through argus-control. Mostly refusals.
func TestSecretsTunnelEndToEnd(t *testing.T) {
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
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		code, text := h.call(t, ownerC, "", method, path, string(raw))
		h.api.secretsBusy.Wait()
		return code, text
	}
	// argus-secret enrolls over TLS, so argus-control is served on it here.
	ts := httptest.NewTLSServer(h.api.Handler())
	t.Cleanup(ts.Close)
	pin := tlsconfig.Pin(ts.Certificate())

	for name, in := range map[string]SecretsServerInput{
		"tunnel with an address": {Name: "Edge", Mode: "tunnel", URL: "https://edge.example.com", Folders: []string{"edge"}},
		"tunnel with a pin":      {Name: "Edge", Mode: "tunnel", TLSPin: pin, Folders: []string{"edge"}},
		"unknown mode":           {Name: "Edge", Mode: "carrier-pigeon", Folders: []string{"edge"}},
	} {
		if code, text := call("POST", "/api/v1/secrets/servers", in); code != http.StatusBadRequest {
			t.Errorf("%s: add = %d %s, want 400", name, code, text)
		}
	}
	code, text := call("POST", "/api/v1/secrets/servers", SecretsServerInput{Name: "Edge", Mode: "tunnel", Folders: []string{"edge"}})
	var added secretsEnrollView
	if code != http.StatusCreated || json.Unmarshal([]byte(text), &added) != nil ||
		!strings.HasPrefix(added.EnrollToken, vault.EnrollTokenPrefix+tn.ID+".") || added.Enrolled || added.EnrollExpiresAt == nil {
		t.Fatalf("add tunnel server = %d %s", code, text)
	}
	id := added.ID
	if code, _ := call("PUT", "/api/v1/secrets/servers/"+id, SecretsServerInput{Name: "Edge", Mode: "direct", URL: "https://x.example.com", Folders: []string{"edge"}}); code != http.StatusBadRequest {
		t.Errorf("changing a server's mode = %d, want 400", code)
	}
	if err := h.api.SyncSecretsServer(ctx, id); err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Errorf("sync before enrolling = %v", err)
	}

	// Enrolling: refused for a made-up token, another organization, or an
	// untrusted argus-control; then once, and never twice.
	dir := t.TempDir()
	enroll := func(token, org, pin string) (*vault.Enrollment, error) {
		return vault.Enroll(context.Background(), vault.EnrollOptions{DataDir: dir, ControlURL: ts.URL, Token: token,
			Pin: pin, Version: "test", Organization: org})
	}
	if _, err := enroll(vault.EnrollTokenPrefix+tn.ID+".not-a-real-token-at-all-0123456789", "", pin); err == nil {
		t.Error("a made-up token enrolled")
	}
	if _, err := enroll(added.EnrollToken, "someone-else", pin); err == nil || !strings.Contains(err.Error(), "organization") {
		t.Errorf("enrolling into another organization = %v", err)
	}
	if _, err := enroll(added.EnrollToken, "", ""); err == nil {
		t.Error("enrolled with argus-control's certificate unchecked")
	}
	enr, err := enroll(added.EnrollToken, tn.ID, pin)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if enr.Org != tn.ID || enr.ServerID != id || !strings.HasPrefix(enr.Credential, vault.ServerCredPrefix) {
		t.Fatalf("enrollment = %+v", enr)
	}
	if _, err := enroll(added.EnrollToken, tn.ID, pin); err == nil {
		t.Error("a token enrolled twice")
	}
	if kept, err := vault.LoadEnrollment(dir); err != nil || kept == nil || kept.Credential != enr.Credential {
		t.Fatalf("kept enrollment = %+v, %v", kept, err)
	}

	// The tunnel: refused with anything but the server's credential.
	for name, cred := range map[string]string{
		"no credential":      "",
		"made-up credential": vault.ServerCredPrefix + tn.ID + ".abcdefghijklmnopqrstuvwxyz0123456789ABCDEF",
		"an agent's":         agentCredentialPrefix + tn.ID + ".abcdefghijklmnopqrstuvwxyz0123456789ABCDEF",
	} {
		bad := *enr
		bad.Credential = cred
		if sess, err := vault.DialTunnel(context.Background(), &bad); err == nil {
			_ = sess.Close()
			t.Errorf("%s: tunnel accepted", name)
		}
	}

	// A real argus-secret, with no listener, on the tunnel.
	controlKey, err := vault.ParseControlKey(enr.ControlKey)
	if err != nil {
		t.Fatal(err)
	}
	root := make([]byte, vault.KeySize)
	_, _ = rand.Read(root)
	vstore, err := vault.Open(t.TempDir(), root, enr.Org)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vstore.Close() })
	rules := vault.NewRules(15 * time.Minute)
	vapi, err := vault.NewAPI(vault.APIOptions{Store: vstore, Rules: rules, ControlKey: controlKey,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	runTunnel := func(e *vault.Enrollment) (stop func()) {
		tctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			vault.RunTunnel(tctx, e, vapi.Handler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		}()
		return func() { cancel(); <-done }
	}
	stop := runTunnel(enr)
	t.Cleanup(stop)
	waitTunnel := func() {
		t.Helper()
		for i := 0; i < 200 && h.api.secretsTunnelOf(ctx, id) == nil; i++ {
			time.Sleep(25 * time.Millisecond)
		}
		if h.api.secretsTunnelOf(ctx, id) == nil {
			t.Fatal("the tunnel never came up")
		}
		h.api.secretsBusy.Wait()
	}
	waitTunnel()

	// An agent, a rule for it, and a sync over the tunnel.
	tok, err := h.store.CreateEnrollToken(ctx, owner, "edge-01", 0)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := h.store.Enroll(ctx, tok.Token, "edge-01")
	if err != nil {
		t.Fatal(err)
	}
	if code, text := call("POST", "/api/v1/secrets/rules", SecretRule{Path: "edge/",
		Subjects: []vault.Subject{{Kind: vault.SubjectServer, ID: agent.CredentialID}}, Can: []string{vault.CanRead}}); code != http.StatusCreated {
		t.Fatalf("add rule = %d %s", code, text)
	}
	if err := h.api.SyncSecretsServer(ctx, id); err != nil {
		t.Fatalf("sync over the tunnel: %v", err)
	}
	if serial, _, fresh := rules.Status(); serial == 0 || !fresh {
		t.Fatalf("rules over the tunnel: serial %d fresh %v", serial, fresh)
	}
	srv, err := h.store.SecretsServer(ctx, id)
	if err != nil || !srv.Enrolled || srv.SealKey == "" || srv.TunnelSeenAt == nil {
		t.Fatalf("server after sync = %+v, %v", srv, err)
	}
	code, text = call("GET", "/api/v1/secrets/servers", nil)
	if code != http.StatusOK || !strings.Contains(text, `"connected":true`) {
		t.Errorf("servers = %d %s, want it connected", code, text)
	}
	if _, err := vstore.Put(vault.PutInput{Name: "edge/api-key", Type: vault.TypeText, Value: []byte("s3cr3t"),
		Actor: vault.Actor{Kind: vault.ActorPerson, ID: owner}}); err != nil {
		t.Fatal(err)
	}

	// The agent learns to read through argus-control.
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/report/secrets-servers", nil)
	req.Header.Set("Authorization", "Bearer "+agent.Credential)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var list []agentSecretsServer
	_ = json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if len(list) != 1 || list[0].URL != "" || list[0].Relay != secretsRelayPrefix+id {
		t.Fatalf("agent's server list = %+v", list)
	}

	read := func(auth, path, name string) (int, []byte) {
		t.Helper()
		k, _ := ecdh.X25519().GenerateKey(rand.Reader)
		body, _ := json.Marshal(secretwire.ReadRequest{Name: name, Recipient: secretwire.EncodeKey(k.PublicKey().Bytes()), Purpose: "test"})
		req, _ := http.NewRequest("POST", h.srv.URL+path+"/v1/read", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			return res.StatusCode, raw
		}
		var out secretwire.ReadResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		plain, err := secretwire.OpenSealed(k, out.Sealed, secretwire.ReadAAD(out.Name, out.Version, ""))
		if err != nil {
			t.Fatalf("open the relayed value: %v", err)
		}
		return res.StatusCode, plain
	}
	relay := list[0].Relay
	if code, v := read("Agent "+agent.Credential, relay, "edge/api-key"); code != http.StatusOK || string(v) != "s3cr3t" {
		t.Fatalf("relayed read = %d %q", code, v)
	}
	for name, c := range map[string]struct{ auth, path string }{
		"no credential":      {"", relay},
		"bearer":             {"Bearer " + agent.Credential, relay},
		"made-up agent":      {"Agent " + agentCredentialPrefix + tn.ID + ".abcdefghijklmnopqrstuvwxyz0123456789ABCD", relay},
		"made-up app":        {"App " + vault.AppTokenPrefix + tn.ID + ".abcdefghijklmnopqrstuvwxyz0123456789ABCD", relay},
		"the server's own":   {"Agent " + enr.Credential, relay},
		"unknown server":     {"Agent " + agent.Credential, secretsRelayPrefix + "00000000-0000-0000-0000-000000000000"},
		"another org's path": {"Agent " + agent.Credential, secretsRelayPrefix + id + "/../../servers"},
	} {
		if code, _ := read(c.auth, c.path, "edge/api-key"); code == http.StatusOK {
			t.Errorf("%s: relayed read answered", name)
		}
	}
	// argus-secret still decides: no rule for the agent outside edge/.
	if code, _ := read("Agent "+agent.Credential, relay, "edge-other/x"); code == http.StatusOK {
		t.Error("a read with no rule was answered")
	}

	// The read is in argus-control's audit log once collected, by the agent,
	// from where argus-control saw it.
	if err := h.api.SyncSecretsServer(ctx, id); err != nil {
		t.Fatalf("collect: %v", err)
	}
	items, _ := vstore.LogAfter(0, 100)
	var fromAgent bool
	for _, it := range items {
		line := string(it.Line)
		if strings.Contains(line, "s3cr3t") {
			t.Fatal("a value is in the read log")
		}
		fromAgent = fromAgent || (strings.Contains(line, `"secret":"edge/api-key"`) && strings.Contains(line, "127.0.0.1"))
	}
	if !fromAgent {
		t.Error("the relayed read is not logged with the agent's address")
	}

	// Enrolling again (a rebuilt server) cuts the old connection, and the
	// old credential stops working.
	code, text = call("POST", "/api/v1/secrets/servers/"+id+"/enroll-token", nil)
	var again secretsEnrollView
	if code != http.StatusOK || json.Unmarshal([]byte(text), &again) != nil || again.EnrollToken == "" {
		t.Fatalf("new token = %d %s", code, text)
	}
	stop()
	enr2, err := vault.Enroll(context.Background(), vault.EnrollOptions{DataDir: t.TempDir(), ControlURL: ts.URL,
		Token: again.EnrollToken, Pin: pin, Version: "test"})
	if err != nil {
		t.Fatalf("enroll again: %v", err)
	}
	if sess, err := vault.DialTunnel(context.Background(), enr); err == nil {
		_ = sess.Close()
		t.Error("the replaced credential opened a tunnel")
	}
	stop2 := runTunnel(enr2)
	t.Cleanup(stop2)
	waitTunnel()
	if err := h.api.SyncSecretsServer(ctx, id); err != nil {
		t.Fatalf("sync after enrolling again: %v", err)
	}

	// A direct server is never given an enrolment token.
	vs := newSecretServer(t, tn.ID, enr.ControlKey)
	code, text = call("POST", "/api/v1/secrets/servers", SecretsServerInput{Name: "Core", URL: vs.srv.URL, TLSPin: vs.pin, Folders: []string{"core"}})
	var direct SecretsServer
	if code != http.StatusCreated || json.Unmarshal([]byte(text), &direct) != nil || strings.Contains(text, "enrollToken") {
		t.Fatalf("add direct = %d %s", code, text)
	}
	if code, _ := call("POST", "/api/v1/secrets/servers/"+direct.ID+"/enroll-token", nil); code != http.StatusBadRequest {
		t.Errorf("enrolment token for a direct server = %d, want 400", code)
	}
	if code, _ := read("Agent "+agent.Credential, secretsRelayPrefix+direct.ID, "core/x"); code != http.StatusNotFound {
		t.Errorf("relay to a direct server = %d, want 404", code)
	}
}

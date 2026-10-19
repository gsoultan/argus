package notify

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeGoogle is the token endpoint and the FCM send endpoint in one server.
type fakeGoogle struct {
	t       *testing.T
	pub     *rsa.PublicKey
	mu      sync.Mutex
	signins int
	sent    []map[string]any
	// sendStatus, when set, is returned by the send endpoint instead of 200.
	sendStatus int
}

func (g *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.URL.Path == "/token":
		_ = r.ParseForm()
		g.checkAssertion(r.Form.Get("assertion"), "http://"+r.Host+"/token")
		g.signins++
		_, _ = io.WriteString(w, `{"access_token":"ya29.access","expires_in":3600}`)
	case r.URL.Path == "/v1/projects/argus-demo/messages:send":
		if r.Header.Get("Authorization") != "Bearer ya29.access" {
			g.t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if g.sendStatus != 0 {
			w.WriteHeader(g.sendStatus)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`)
			return
		}
		var body struct {
			Message map[string]any `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.sent = append(g.sent, body.Message)
		_, _ = io.WriteString(w, `{"name":"projects/argus-demo/messages/1"}`)
	default:
		g.t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// checkAssertion verifies the JWT the way Google does: signed by the service
// account's key, for the messaging scope, addressed to the token endpoint.
func (g *fakeGoogle) checkAssertion(jwt, aud string) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		g.t.Fatalf("assertion is not a JWT: %q", jwt)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(g.pub, crypto.SHA256, sum[:], sig); err != nil {
		g.t.Fatalf("assertion signature does not verify: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	if c["iss"] != "alerts@argus-demo.iam.gserviceaccount.com" || c["scope"] != fcmScope || c["aud"] != aud {
		g.t.Errorf("claims = %v", c)
	}
}

func newFakeGoogle(t *testing.T) (*fakeGoogle, *httptest.Server, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGoogle{t: t, pub: &key.PublicKey}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)

	der, _ := x509.MarshalPKCS8PrivateKey(key)
	sa, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "argus-demo",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "alerts@argus-demo.iam.gserviceaccount.com",
		"token_uri":    srv.URL + "/token",
	})
	path := filepath.Join(t.TempDir(), "fcm.json")
	if err := os.WriteFile(path, sa, 0o600); err != nil {
		t.Fatal(err)
	}
	return g, srv, path
}

func TestFCMPushesToTheTopicAndEveryDevice(t *testing.T) {
	g, srv, creds := newFakeGoogle(t)
	f, err := NewFCM(FCMConfig{CredentialsFile: creds, Topic: "argus-sessions", Tokens: []string{"device-token-1"}}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.api = srv.URL

	subject, body := Message(SessionStarted{ID: "s-1", UserEmail: "ana@example.com", Host: "db-01", Principal: "ops", Protocol: "ssh"}, "")
	for range 2 {
		if err := f.Send(context.Background(), subject, body); err != nil {
			t.Fatal(err)
		}
	}

	if g.signins != 1 {
		t.Errorf("signed in %d times for two alerts; the access token should be reused", g.signins)
	}
	if len(g.sent) != 4 {
		t.Fatalf("sent %d messages, want 4 (topic and device, twice)", len(g.sent))
	}
	if g.sent[0]["topic"] != "argus-sessions" || g.sent[1]["token"] != "device-token-1" {
		t.Fatalf("targets = %v, %v", g.sent[0], g.sent[1])
	}
	n := g.sent[0]["notification"].(map[string]any)
	if n["body"] != "ana@example.com opened SSH on db-01 as ops" {
		t.Fatalf("notification = %v", n)
	}
}

// A device that uninstalled the app is a failure to log, and it must not stop
// the topic from being told. The device token is an address anyone holding it
// can push to, so the log names it by prefix only.
func TestFCMReportsAFailedDeviceWithoutItsWholeToken(t *testing.T) {
	g, srv, creds := newFakeGoogle(t)
	g.sendStatus = http.StatusNotFound
	f, err := NewFCM(FCMConfig{CredentialsFile: creds, Tokens: []string{"abcdefgh-SECRET-rest-of-token"}}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.api = srv.URL
	err = f.Send(context.Background(), "Argus: x", "x")
	if err == nil || !strings.Contains(err.Error(), "Requested entity was not found") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error carries the whole device token: %v", err)
	}
}

func TestFCMDropsARefusedAccessToken(t *testing.T) {
	g, srv, creds := newFakeGoogle(t)
	g.sendStatus = http.StatusUnauthorized
	f, _ := NewFCM(FCMConfig{CredentialsFile: creds, Topic: "t"}, srv.Client())
	f.api = srv.URL
	_ = f.Send(context.Background(), "", "")
	g.sendStatus = 0
	if err := f.Send(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if g.signins != 2 {
		t.Fatalf("signed in %d times; a 401 should force a fresh token", g.signins)
	}
}

func TestFCMConfigIsCheckedAtStartUp(t *testing.T) {
	_, err := NewFCM(FCMConfig{Topic: "t"}, nil)
	if !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "credentials_file") {
		t.Fatalf("err = %v", err)
	}
	_, _, creds := newFakeGoogle(t)
	_, err = NewFCM(FCMConfig{CredentialsFile: creds}, nil)
	if !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "topic or tokens") {
		t.Fatalf("err = %v", err)
	}
}

// The service account key can mint tokens for the whole Firebase project, so
// it is held to the same rule as every other private key Argus loads.
func TestFCMRefusesACredentialsFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits say nothing on Windows; secrets has its own test there")
	}
	_, _, creds := newFakeGoogle(t)
	if err := os.Chmod(creds, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFCM(FCMConfig{CredentialsFile: creds, Topic: "t"}, nil); err == nil {
		t.Fatal("a world-readable service account key was accepted")
	}
}
